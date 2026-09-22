package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"moesekai/server/internal/lyricsperformers"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

var (
	ErrLyricsSourceReviewNotApproved = errors.New("lyrics source review is not approved for import")
	ErrLyricsSourcePerformerMapping  = errors.New("lyrics source performer mapping is incomplete")
)

const (
	approvedLyricsSourceAttribution = "Japanese lyrics from Vocaloid Wiki; fixed revision reviewed by the MoeSeka team"
	approvedLyricsSourceNote        = "Imported from an approved immutable lyrics source analysis"
	approvedLyricsSourceLicenseNote = "Source-use review approved for this fixed revision; translations are not included"
)

type approvedLyricsSourceImport struct {
	reviewID             int64
	analysisID           int64
	musicID              int
	catalogFingerprint   string
	matchingPolicy       string
	restrictionPolicy    string
	extractorVersion     string
	matchOutcome         string
	restrictionOutcome   string
	extractionOutcome    string
	selectedVersion      model.LyricsSourceVersion
	performers           []model.LyricsSourcePerformer
	rubyGeneratorVersion string
	extractedLines       []model.LyricsSourceExtractedLine
	extractedLineCount   int
	extractedLinesSHA256 string
	pageID               int
	revisionID           int
	canonicalURL         string
	mediaWikiSHA1        string
	firstFetchedAtMillis int64
	vocals               []model.CatalogVocalSignal
	associations         []model.LyricsSourceAssociation
}

type approvedLyricsSourceImportQuery interface {
	catalogMusicIdentityQuery
	catalogLyricsTargetsQuery
	queryRower
}

// ImportApprovedLyricsSource creates the immutable first lyrics draft from a
// fixed source analysis only after the artifact review and current catalog
// identity have been revalidated in the same transaction as the save. It does
// not publish the draft.
func (s *Store) ImportApprovedLyricsSource(ctx context.Context, reviewID int64, actor string) (model.SongLyrics, bool, error) {
	if ctx == nil {
		return model.SongLyrics{}, false, errors.New("approved lyrics source import requires context")
	}
	normalizedActor := strings.TrimSpace(actor)
	if reviewID <= 0 || normalizedActor == "" || len(actor) > maxLyricsReviewActorBytes {
		return model.SongLyrics{}, false, ErrLyricsSourceInvalidRequest
	}

	initial, err := loadApprovedLyricsSourceImport(ctx, s.db, reviewID)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	return s.importApprovedLyricsSourceSnapshot(ctx, reviewID, normalizedActor, initial)
}

func (s *Store) importApprovedLyricsSourceSnapshot(ctx context.Context, reviewID int64, actor string, initial approvedLyricsSourceImport) (model.SongLyrics, bool, error) {
	unlock := s.lockLyrics(initial.musicID)
	defer unlock()
	if current, loadErr := s.loadLyrics(s.db, initial.musicID); loadErr == nil {
		draft, err := approvedLyricsSourceDraft(s.db, initial)
		if err != nil {
			return model.SongLyrics{}, false, err
		}
		if sameLyricsContent(draft, current.lyrics) {
			return current.lyrics, false, nil
		}
		return model.SongLyrics{}, false, ErrLyricsAlreadySaved
	} else if !errors.Is(loadErr, ErrLyricsNotFound) {
		return model.SongLyrics{}, false, loadErr
	}

	return s.saveLyricsMutationLocked(model.SongLyrics{MusicID: initial.musicID}, actor, lyricsSaveVerifiedImport, nil, nil,
		func(tx *sql.Tx, input *model.SongLyrics) error {
			current, err := loadApprovedLyricsSourceImport(ctx, tx, reviewID)
			if err != nil {
				return err
			}
			if !sameApprovedLyricsSourceImport(initial, current) {
				return ErrLyricsSourceArtifactConflict
			}
			draft, err := approvedLyricsSourceDraft(tx, current)
			if err != nil {
				return err
			}
			*input = draft
			return nil
		})
}

func loadApprovedLyricsSourceImport(ctx context.Context, query approvedLyricsSourceImportQuery, reviewID int64) (approvedLyricsSourceImport, error) {
	var result approvedLyricsSourceImport
	var selectedVersionJSON, performersJSON, extractedLinesJSON, vocalsJSON string
	var state, identityGate, sourceUseGate, parseGate, reviewPolicy, catalogPolicy string
	err := query.QueryRowContext(ctx, `SELECT r.review_id,r.analysis_id,r.music_id,r.catalog_fingerprint,r.review_policy_version,
		r.state,r.identity_gate,r.source_use_gate,r.parse_gate,
		a.matching_policy_version,a.restriction_policy_version,a.extractor_version,a.match_outcome,a.restriction_outcome,
		a.extraction_outcome,a.selected_version_json,a.performers_json,a.ruby_generator_version,a.extracted_lines_json,
		a.extracted_line_count,a.extracted_lines_sha256,
		s.page_id,s.revision_id,s.canonical_revision_url,s.mediawiki_sha1,s.first_fetched_at,
		c.vocal_signals_json,c.lyrics_catalog_policy_version
		FROM lyrics_source_review_items r
		JOIN lyrics_source_analyses a ON a.analysis_id=r.analysis_id
		JOIN lyrics_source_artifacts s ON s.artifact_id=a.artifact_id
		JOIN catalog_music c ON c.music_id=r.music_id
		WHERE r.review_id=? AND r.kind='artifact_review'`, reviewID).Scan(
		&result.reviewID, &result.analysisID, &result.musicID, &result.catalogFingerprint, &reviewPolicy,
		&state, &identityGate, &sourceUseGate, &parseGate,
		&result.matchingPolicy, &result.restrictionPolicy, &result.extractorVersion, &result.matchOutcome,
		&result.restrictionOutcome, &result.extractionOutcome, &selectedVersionJSON, &performersJSON,
		&result.rubyGeneratorVersion, &extractedLinesJSON, &result.extractedLineCount, &result.extractedLinesSHA256,
		&result.pageID, &result.revisionID, &result.canonicalURL, &result.mediaWikiSHA1, &result.firstFetchedAtMillis,
		&vocalsJSON, &catalogPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return approvedLyricsSourceImport{}, ErrLyricsSourceReviewNotFound
	}
	if err != nil {
		return approvedLyricsSourceImport{}, err
	}
	if state != LyricsSourceReviewStateApproved || identityGate != LyricsSourceReviewDecisionApproved ||
		sourceUseGate != LyricsSourceReviewDecisionApproved || parseGate != LyricsSourceReviewDecisionApproved ||
		reviewPolicy != model.LyricsReviewPolicyVersion || catalogPolicy != model.LyricsCatalogIdentityPolicyVersion ||
		result.matchingPolicy != model.LyricsMatchingPolicyVersion || result.restrictionPolicy != model.LyricsRestrictionPolicyVersion ||
		result.extractorVersion != model.LyricsExtractorVersion || result.matchOutcome != "matched" ||
		result.restrictionOutcome != "clear" || result.extractionOutcome != "extracted" {
		return approvedLyricsSourceImport{}, ErrLyricsSourceReviewNotApproved
	}
	identity, err := loadCatalogMusicIdentityContext(ctx, query, result.musicID)
	if err != nil {
		return approvedLyricsSourceImport{}, err
	}
	if identity.CatalogFingerprint != result.catalogFingerprint {
		return approvedLyricsSourceImport{}, ErrLyricsSourceReviewNotApproved
	}
	result.associations, err = approvedLyricsSourceClassification(ctx, query, result.analysisID, result.musicID, result.catalogFingerprint)
	if err != nil {
		return approvedLyricsSourceImport{}, err
	}
	if json.Unmarshal([]byte(selectedVersionJSON), &result.selectedVersion) != nil ||
		json.Unmarshal([]byte(performersJSON), &result.performers) != nil ||
		json.Unmarshal([]byte(extractedLinesJSON), &result.extractedLines) != nil ||
		json.Unmarshal([]byte(vocalsJSON), &result.vocals) != nil {
		return approvedLyricsSourceImport{}, ErrLyricsSourceArtifactConflict
	}
	if result.extractedLineCount != len(result.extractedLines) ||
		model.LyricsSourceExtractedLinesSHA256(result.extractedLines) != result.extractedLinesSHA256 {
		return approvedLyricsSourceImport{}, ErrLyricsSourceArtifactConflict
	}
	canonicalPerformers, canonicalRubyVersion, canonicalLines, err := canonicalizeLyricsSourceAnalysisMetadata(
		result.selectedVersion, result.performers, result.rubyGeneratorVersion, result.extractedLines,
	)
	if err != nil {
		return approvedLyricsSourceImport{}, ErrLyricsSourceArtifactConflict
	}
	result.performers = canonicalPerformers
	result.rubyGeneratorVersion = canonicalRubyVersion
	result.extractedLines = canonicalLines
	result.extractedLinesSHA256 = model.LyricsSourceExtractedLinesSHA256(canonicalLines)
	result.extractedLineCount = len(canonicalLines)
	if result.selectedVersion.Kind == "" || strings.TrimSpace(result.selectedVersion.Label) == "" ||
		strings.TrimSpace(result.rubyGeneratorVersion) == "" || result.extractedLineCount != len(result.extractedLines) ||
		result.extractedLineCount == 0 || model.LyricsSourceExtractedLinesSHA256(result.extractedLines) != result.extractedLinesSHA256 ||
		result.pageID <= 0 || result.revisionID <= 0 || !hasCanonicalLyricsSourceSHA1(result.mediaWikiSHA1) ||
		result.firstFetchedAtMillis <= 0 || ValidateLyricsSourceRevisionURL(result.canonicalURL, result.revisionID) != nil {
		return approvedLyricsSourceImport{}, ErrLyricsSourceArtifactConflict
	}
	return result, nil
}

func approvedLyricsSourceClassification(ctx context.Context, query approvedLyricsSourceImportQuery, analysisID int64, musicID int, catalogFingerprint string) ([]model.LyricsSourceAssociation, error) {
	rows, err := query.QueryContext(ctx, `SELECT music_id,catalog_fingerprint,kind FROM lyrics_source_associations
		WHERE analysis_id=? ORDER BY music_id,kind`, analysisID)
	if err != nil {
		return nil, err
	}
	stored := []model.LyricsSourceAssociation{}
	for rows.Next() {
		var association model.LyricsSourceAssociation
		if err := rows.Scan(&association.MusicID, &association.CatalogFingerprint, &association.Kind); err != nil {
			rows.Close()
			return nil, err
		}
		stored = append(stored, association)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, ErrLyricsSourceArtifactConflict
	}

	targets, err := catalogLyricsTargetsContext(ctx, query)
	if err != nil {
		return nil, err
	}
	currentTarget, ok := lyricsSourceTargetForMusic(targets, musicID)
	if !ok || currentTarget.Disposition != model.LyricsCatalogTargetFullTarget ||
		currentTarget.TargetMusicID != musicID || currentTarget.CatalogFingerprint != catalogFingerprint {
		return nil, ErrLyricsSourceReviewNotApproved
	}
	current := make([]model.LyricsSourceAssociation, 0, len(currentTarget.AssociationMusicIDs)+1)
	for _, target := range targets {
		if target.TargetMusicID != musicID {
			continue
		}
		if target.Disposition != model.LyricsCatalogTargetFullTarget && target.Disposition != model.LyricsCatalogTargetGameSizeEvidence {
			return nil, ErrLyricsSourceReviewNotApproved
		}
		current = append(current, model.LyricsSourceAssociation{
			MusicID: target.MusicID, CatalogFingerprint: target.CatalogFingerprint, Kind: target.Disposition,
		})
	}
	sort.Slice(current, func(i, j int) bool {
		if current[i].MusicID != current[j].MusicID {
			return current[i].MusicID < current[j].MusicID
		}
		return current[i].Kind < current[j].Kind
	})
	if !reflect.DeepEqual(stored, current) {
		return nil, ErrLyricsSourceReviewNotApproved
	}
	return current, nil
}

func performerRenditionMatchesCatalogPolicy(policy lyricssource.PerformerSegmentationPolicy, kind string) bool {
	switch policy {
	case lyricssource.PerformerSegmentationDisabled:
		return kind == "original" || kind == "vocaloid"
	case lyricssource.PerformerSegmentationSekaiEligible:
		return kind == "original" || kind == "sekai" || kind == "vocaloid"
	default:
		return false
	}
}

func approvedLyricsSourceDraft(query queryRower, source approvedLyricsSourceImport) (model.SongLyrics, error) {
	catalogPerformers, err := loadCatalogPerformerAliases(query)
	if err != nil {
		return model.SongLyrics{}, err
	}
	policy := lyricssource.PerformerSegmentationPolicyFromCatalogVocals(source.vocals)
	if !performerRenditionMatchesCatalogPolicy(policy, source.selectedVersion.Kind) {
		return model.SongLyrics{}, ErrLyricsSourceArtifactConflict
	}
	aliases, declaredUnmapped := resolveLyricsSourcePerformerAliases(catalogPerformers, source.performers)

	lines := make([]model.LyricLine, len(source.extractedLines))
	for lineIndex, sourceLine := range source.extractedLines {
		lineFallback, err := MapDeclaredLyricsSourcePerformerIDs(
			sourceLine.TrailingPerformerIDs,
			aliases,
			declaredUnmapped,
		)
		if err != nil {
			return model.SongLyrics{}, err
		}
		segments := make([]model.LyricSegment, len(sourceLine.Segments))
		var japanese strings.Builder
		for segmentIndex, sourceSegment := range sourceLine.Segments {
			performerIDs, err := MapDeclaredLyricsSourcePerformerIDs(
				sourceSegment.PerformerIDs,
				aliases,
				declaredUnmapped,
			)
			if err != nil {
				return model.SongLyrics{}, err
			}
			if len(performerIDs) == 0 {
				performerIDs = lineFallback
			}
			if strings.TrimSpace(sourceSegment.Text) == "" {
				return model.SongLyrics{}, ErrLyricsSourceArtifactConflict
			}
			ruby := make([]model.LyricRubySpan, len(sourceSegment.Ruby))
			for rubyIndex, span := range sourceSegment.Ruby {
				ruby[rubyIndex] = model.LyricRubySpan{Text: span.Text, Reading: span.Reading}
			}
			segments[segmentIndex] = model.LyricSegment{
				Text: sourceSegment.Text, PerformerIDs: append([]int{}, performerIDs...), Ruby: ruby,
			}
			japanese.WriteString(sourceSegment.Text)
		}
		if japanese.String() != sourceLine.Japanese || strings.TrimSpace(sourceLine.Japanese) == "" {
			return model.SongLyrics{}, ErrLyricsSourceArtifactConflict
		}
		lines[lineIndex] = model.LyricLine{
			ID:    fmt.Sprintf("wiki-%d-%d-%d", source.pageID, source.revisionID, lineIndex+1),
			Order: lineIndex, Japanese: sourceLine.Japanese, Chinese: "", English: "",
			StanzaBreakBefore: sourceLine.StanzaBreakBefore, Segments: segments,
		}
	}
	return model.SongLyrics{
		MusicID: source.musicID, Revision: 0, Status: "draft",
		Attribution: approvedLyricsSourceAttribution, SourceNote: approvedLyricsSourceNote,
		SourceURL: source.canonicalURL, LicenseNote: approvedLyricsSourceLicenseNote,
		SourcePageID: source.pageID, SourceRevisionID: source.revisionID, SourceSHA1: source.mediaWikiSHA1,
		SourceFetchedAt: time.UnixMilli(source.firstFetchedAtMillis).UTC().Format(time.RFC3339Nano), Lines: lines,
	}, nil
}

type catalogPerformerAliases struct {
	aliases  map[string]int
	validIDs map[int]bool
}

func loadCatalogPerformerAliases(query queryRower) (catalogPerformerAliases, error) {
	rows, err := query.Query(`SELECT performer_id,name_ja,name_zh,name_en FROM catalog_performers ORDER BY performer_id`)
	if err != nil {
		return catalogPerformerAliases{}, err
	}
	defer rows.Close()
	result := newCatalogPerformerAliases()
	for rows.Next() {
		var performerID int
		var names [3]string
		if err := rows.Scan(&performerID, &names[0], &names[1], &names[2]); err != nil {
			return catalogPerformerAliases{}, err
		}
		addCatalogPerformerAliases(&result, performerID, names[:]...)
	}
	if err := rows.Err(); err != nil {
		return catalogPerformerAliases{}, err
	}
	addCanonicalLyricsSourcePerformerAliases(&result)
	if err := addAuditedExternalLyricsPerformerAliases(&result); err != nil {
		return catalogPerformerAliases{}, err
	}
	return result, nil
}

func newCatalogPerformerAliases() catalogPerformerAliases {
	return catalogPerformerAliases{aliases: map[string]int{}, validIDs: map[int]bool{}}
}

func addCatalogPerformerAliases(result *catalogPerformerAliases, performerID int, names ...string) {
	if result == nil || performerID <= 0 {
		return
	}
	result.validIDs[performerID] = true
	for _, name := range names {
		if alias := normalizeLyricsSourcePerformerAlias(name); alias != "" {
			result.aliases[alias] = performerID
		}
	}
}

func addCanonicalLyricsSourcePerformerAliases(result *catalogPerformerAliases) {
	if result == nil {
		return
	}
	for alias, performerID := range canonicalLyricsSourcePerformerIDs {
		if result.validIDs[performerID] {
			result.aliases[alias] = performerID
		}
	}
}

// addAuditedExternalLyricsPerformerAliases extends only the lyrics performer
// namespace. These IDs are not game characters and therefore are deliberately
// absent from catalog_performers; a database row colliding with the reserved
// range fails closed instead of silently changing its meaning.
func addAuditedExternalLyricsPerformerAliases(result *catalogPerformerAliases) error {
	if result == nil {
		return errors.New("lyrics performer alias catalog is nil")
	}
	if err := addAuditedExternalLyricsPerformerIDs(result.validIDs); err != nil {
		return err
	}
	for _, performer := range lyricsperformers.All() {
		names := make([]string, 0, len(performer.Aliases)+2)
		names = append(names, performer.SourceID, performer.Name)
		names = append(names, performer.Aliases...)
		for _, name := range names {
			if alias := normalizeLyricsSourcePerformerAlias(name); alias != "" {
				result.aliases[alias] = performer.NumericID
			}
		}
	}
	return nil
}

func addAuditedExternalLyricsPerformerIDs(validIDs map[int]bool) error {
	if validIDs == nil {
		return errors.New("lyrics performer ID catalog is nil")
	}
	for _, performer := range lyricsperformers.All() {
		if validIDs[performer.NumericID] {
			return errors.New("catalog performer ID collides with a reserved lyrics-only performer")
		}
		validIDs[performer.NumericID] = true
	}
	return nil
}

func resolveLyricsSourcePerformerAliases(
	catalogPerformers catalogPerformerAliases,
	performers []model.LyricsSourcePerformer,
) (map[string]int, map[string]bool) {
	aliases := make(map[string]int, len(catalogPerformers.aliases)+len(performers)*2)
	for alias, performerID := range catalogPerformers.aliases {
		aliases[alias] = performerID
	}
	declaredUnmapped := make(map[string]bool, len(performers)*2)
	for _, performer := range performers {
		candidates := []string{
			normalizeLyricsSourcePerformerAlias(performer.PerformerID),
			normalizeLyricsSourcePerformerAlias(performer.Name),
		}
		mapped := 0
		for _, candidate := range candidates {
			if performerID := aliases[candidate]; performerID > 0 {
				mapped = performerID
				break
			}
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if mapped > 0 {
				aliases[candidate] = mapped
			} else {
				declaredUnmapped[candidate] = true
			}
		}
	}
	return aliases, declaredUnmapped
}

var canonicalLyricsSourcePerformerIDs = map[string]int{
	"ichika": 1, "saki": 2, "honami": 3, "shiho": 4,
	"minori": 5, "haruka": 6, "airi": 7, "shizuku": 8,
	"kohane": 9, "an": 10, "akito": 11, "toya": 12,
	"tsukasa": 13, "emu": 14, "nene": 15, "rui": 16,
	"kanade": 17, "mafuyu": 18, "ena": 19, "mizuki": 20,
	"miku": 21, "hatsunemiku": 21, "rin": 22, "kagaminerin": 22,
	"len": 23, "kagaminelen": 23, "luka": 24, "megurineluka": 24,
	"meiko": 25, "kaito": 26,
}

func normalizeLyricsSourcePerformerAlias(value string) string {
	value = strings.ToLower(norm.NFKC.String(strings.TrimSpace(value)))
	var builder strings.Builder
	for _, current := range value {
		if unicode.IsLetter(current) || unicode.IsNumber(current) {
			builder.WriteRune(current)
		}
	}
	return builder.String()
}

func mapLyricsSourcePerformerIDs(sourceIDs []string, aliases map[string]int) ([]int, error) {
	seen := map[int]bool{}
	result := make([]int, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		normalized := normalizeLyricsSourcePerformerAlias(sourceID)
		if normalized == "" || normalized == "chorus" || normalized == "ensemble" || normalized == "all" || normalized == "everyone" {
			continue
		}
		performerID := aliases[normalized]
		if performerID <= 0 {
			return nil, ErrLyricsSourcePerformerMapping
		}
		if !seen[performerID] {
			seen[performerID] = true
			result = append(result, performerID)
		}
	}
	sort.Ints(result)
	return result, nil
}

func selectedCatalogVocalPerformers(versionKind string, vocals []model.CatalogVocalSignal, validIDs map[int]bool) []int {
	preferred := func(vocalType string) bool {
		switch versionKind {
		case "sekai":
			return vocalType == "sekai"
		case "vocaloid":
			return vocalType == "virtual_singer" || vocalType == "original_song"
		default:
			return vocalType == "original_song" || vocalType == "virtual_singer"
		}
	}
	collect := func(requirePreferred bool) []int {
		seen := map[int]bool{}
		result := []int{}
		for _, vocal := range vocals {
			if vocal.CharacterType != "game_character" || !validIDs[vocal.CharacterID] || requirePreferred && !preferred(vocal.VocalType) {
				continue
			}
			if !seen[vocal.CharacterID] {
				seen[vocal.CharacterID] = true
				result = append(result, vocal.CharacterID)
			}
		}
		sort.Ints(result)
		return result
	}
	if selected := collect(true); len(selected) > 0 {
		return selected
	}
	return collect(false)
}

func sameApprovedLyricsSourceImport(left, right approvedLyricsSourceImport) bool {
	return reflect.DeepEqual(left, right)
}
