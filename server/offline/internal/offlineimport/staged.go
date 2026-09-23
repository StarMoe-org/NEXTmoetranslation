package offlineimport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/db"
	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
	"moesekai/server/offline/internal/lyricsstaging"
)

var (
	ErrLyricsStagedManifestConflict        = errors.New("staged lyrics manifest conflicts with an existing lyrics document")
	ErrLyricsStagedManifestDrift           = errors.New("staged lyrics manifest no longer matches the local catalog or source identity")
	ErrLyricsStagedManifestRebuildRequired = errors.New("staged lyrics source provenance must be rebuilt")
)

type StagedLyricsImportItem struct {
	MusicID        int
	Lyrics         model.SongLyrics
	SourceDocument *model.LyricsSourceDocument
	Changed        bool
}

type stagedImportCatalogItem struct {
	musicID            int
	japaneseTitle      string
	producerMetadata   string
	evidence           model.CatalogLyricsEvidence
	catalogFingerprint string
	vocals             []model.CatalogVocalSignal
}

// StagedLyricsImportCommitHook runs as the final operation inside the staged
// import transaction. If it returns nil, Commit is the next database call.
type StagedLyricsImportCommitHook func(*sql.Tx, []StagedLyricsImportItem) error

// ImportStagedLyricsManifestWithCommitHook is the receipt-aware variant used by
// the offline importer. commitAttempted is true exactly when the hook completed
// and SQLite Commit was invoked, including an ambiguous Commit error.
func ImportStagedLyricsManifestWithCommitHook(
	ctx context.Context,
	lyricsStore *store.Store,
	manifest lyricsstaging.Manifest,
	actor string,
	beforeCommit StagedLyricsImportCommitHook,
) ([]StagedLyricsImportItem, bool, error) {
	return importStagedLyricsManifest(ctx, lyricsStore, manifest, nil, actor, beforeCommit)
}

// ImportStagedLyricsManifestWithEvidenceReceiptAndCommitHook combines the
// strict concrete-evidence handoff with the durable pre-commit receipt
// boundary. Evidence parents and artifact links are inserted or verified in
// the same transaction before the hook runs; Commit is the next database call.
func ImportStagedLyricsManifestWithEvidenceReceiptAndCommitHook(
	ctx context.Context,
	lyricsStore *store.Store,
	manifest lyricsstaging.Manifest,
	receipt lyricsstaging.PrivateEvidenceReceipt,
	actor string,
	beforeCommit StagedLyricsImportCommitHook,
) ([]StagedLyricsImportItem, bool, error) {
	return importStagedLyricsManifest(ctx, lyricsStore, manifest, &receipt, actor, beforeCommit)
}

func importStagedLyricsManifest(
	ctx context.Context,
	lyricsStore *store.Store,
	manifest lyricsstaging.Manifest,
	receipt *lyricsstaging.PrivateEvidenceReceipt,
	actor string,
	beforeCommit StagedLyricsImportCommitHook,
) ([]StagedLyricsImportItem, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("staged lyrics import requires context")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > store.MaxLyricsImportActorBytes || !utf8.ValidString(actor) {
		return nil, false, store.ErrLyricsSourceInvalidRequest
	}
	if err := lyricsstaging.ValidateManifest(manifest); err != nil {
		return nil, false, err
	}
	if receipt != nil {
		candidates, err := stagedManifestEvidenceCandidates(manifest)
		if err != nil {
			return nil, false, err
		}
		if err := lyricsstaging.ValidatePrivateEvidenceReceiptForCandidates(*receipt, candidates); err != nil {
			return nil, false, fmt.Errorf("%w: staged evidence receipt: %v", ErrLyricsStagedManifestRebuildRequired, err)
		}
	}

	session, err := lyricsStore.BeginOfflineLyricsImport(ctx, stagedManifestMusicIDs(manifest))
	if err != nil {
		return nil, false, err
	}
	defer session.Close()
	tx := session.Tx()
	if err := validateStagedImportRuntimeSchema(ctx, tx); err != nil {
		return nil, false, err
	}
	if err := validatePeerTranslationRuntimeSchema(ctx, tx, stagedManifestHasPeerTranslations(manifest), "staged lyrics import"); err != nil {
		return nil, false, err
	}
	if receipt != nil {
		if err := store.InsertOrVerifyLyricsIndexEvidenceCollectionTx(ctx, tx, receipt.IndexEvidence, time.Now().UTC()); err != nil {
			return nil, false, err
		}
	}
	catalog, err := loadStagedImportCatalog(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if len(catalog) != manifest.Preflight.CatalogCount {
		return nil, false, fmt.Errorf("%w: manifest catalog count %d does not match current catalog count %d",
			ErrLyricsStagedManifestDrift, manifest.Preflight.CatalogCount, len(catalog))
	}
	catalogByMusicID := make(map[int]stagedImportCatalogItem, len(catalog))
	grouping := make([]model.CatalogLyricsGroupingRecord, 0, len(catalog))
	for _, item := range catalog {
		catalogByMusicID[item.musicID] = item
		grouping = append(grouping, model.CatalogLyricsGroupingRecord{
			MusicID: item.musicID, Fingerprint: item.catalogFingerprint, Evidence: item.evidence,
		})
	}
	targets := model.ClassifyCatalogLyricsTargets(grouping)
	targetByMusicID := make(map[int]model.CatalogLyricsTarget, len(targets))
	for _, target := range targets {
		targetByMusicID[target.MusicID] = target
	}
	if len(targetByMusicID) != len(catalogByMusicID) {
		return nil, false, fmt.Errorf("%w: current catalog classification is incomplete", ErrLyricsStagedManifestDrift)
	}

	performerAliases, err := store.LoadCatalogPerformerAliases(tx)
	if err != nil {
		return nil, false, err
	}
	requested := make([]model.SongLyrics, len(manifest.Items))
	results := make([]StagedLyricsImportItem, len(manifest.Items))
	for index, staged := range manifest.Items {
		catalogItem, exists := catalogByMusicID[staged.MusicID]
		if !exists || catalogItem.japaneseTitle != staged.JapaneseTitle ||
			catalogItem.catalogFingerprint != staged.CatalogFingerprint {
			return nil, false, fmt.Errorf("%w: music %d catalog identity changed", ErrLyricsStagedManifestDrift, staged.MusicID)
		}
		target, exists := targetByMusicID[staged.MusicID]
		if !exists || target.Disposition != model.LyricsCatalogTargetFullTarget ||
			target.TargetMusicID != staged.TargetMusicID || target.CatalogFingerprint != staged.CatalogFingerprint ||
			!sameStagedAssociationIDs(target.AssociationMusicIDs, staged.AssociationMusicIDs) {
			return nil, false, fmt.Errorf("%w: music %d catalog target or associations changed", ErrLyricsStagedManifestDrift, staged.MusicID)
		}
		if staged.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			if err := store.ValidatePersistedLyricsSourceDocument(staged.Document); err != nil {
				return nil, false, fmt.Errorf("%w: music %d source v3: %v", ErrLyricsStagedManifestRebuildRequired, staged.MusicID, err)
			}
			// Source-v3 documents are always owned by the plural rendition
			// editor. A legacy SongLyrics row would create a second mutable
			// translation store, so an existing one is a rebuild-required
			// conflict rather than something to reconcile implicitly.
			_, loadErr := store.LoadLyricsTx(tx, staged.MusicID)
			if loadErr == nil {
				return nil, false, fmt.Errorf("%w: music %d has a legacy editable row for source v3", ErrLyricsStagedManifestRebuildRequired, staged.MusicID)
			}
			if !errors.Is(loadErr, store.ErrLyricsNotFound) {
				return nil, false, loadErr
			}
			exists, matched, provenanceErr := stagedLyricsSourceDocumentMatches(ctx, tx, staged, receipt != nil)
			if provenanceErr != nil {
				return nil, false, provenanceErr
			}
			if exists && !matched {
				return nil, false, fmt.Errorf("%w: music %d immutable source-v3 graph or localization drifted", ErrLyricsStagedManifestRebuildRequired, staged.MusicID)
			}
			if matched {
				results[index] = StagedLyricsImportItem{MusicID: staged.MusicID, SourceDocument: store.CloneLyricsSourceDocument(staged.Document)}
			} else {
				results[index] = StagedLyricsImportItem{MusicID: staged.MusicID, Changed: true, SourceDocument: store.CloneLyricsSourceDocument(staged.Document)}
			}
			continue
		}
		draft, err := stagedManifestLyricsDraft(staged, catalogItem.vocals, performerAliases)
		if err != nil {
			return nil, false, err
		}
		if err := store.ValidateImportedLyrics(draft, performerAliases); err != nil {
			return nil, false, err
		}
		requested[index] = draft

		current, loadErr := store.LoadLyricsTx(tx, staged.MusicID)
		switch {
		case loadErr == nil:
			if !store.SameLyricsContent(draft, current) {
				return nil, false, fmt.Errorf("%w: music %d", ErrLyricsStagedManifestConflict, staged.MusicID)
			}
			_, matched, provenanceErr := stagedLyricsSourceDocumentMatches(ctx, tx, staged, receipt != nil)
			if provenanceErr != nil {
				return nil, false, provenanceErr
			}
			if !matched {
				return nil, false, fmt.Errorf("%w: music %d has editable bytes without the required immutable source document", ErrLyricsStagedManifestRebuildRequired, staged.MusicID)
			}
			results[index] = StagedLyricsImportItem{MusicID: staged.MusicID, Lyrics: current}
		case errors.Is(loadErr, store.ErrLyricsNotFound):
			results[index] = StagedLyricsImportItem{MusicID: staged.MusicID, Changed: true}
		default:
			return nil, false, loadErr
		}
	}

	now := time.Now().Unix()
	changed := false
	for index, result := range results {
		if !result.Changed {
			continue
		}
		var saved model.SongLyrics
		var err error
		if manifest.Items[index].Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			saved, err = insertStagedV3Draft(ctx, tx, manifest.Items[index], actor, manifest.BatchSHA256, now, receipt != nil)
		} else {
			saved, err = insertStagedLyricsDraft(ctx, tx, requested[index], manifest.Items[index], actor, manifest.BatchSHA256, now, receipt != nil)
		}
		if err != nil {
			return nil, false, err
		}
		results[index].Lyrics = saved
		changed = true
	}
	if beforeCommit != nil {
		if err := beforeCommit(tx, results); err != nil {
			return nil, false, err
		}
	}
	if err := session.Commit(); err != nil {
		return nil, true, err
	}
	if changed {
		session.NotifyChange()
	}
	return results, true, nil
}

func stagedManifestHasPeerTranslations(manifest lyricsstaging.Manifest) bool {
	for _, draft := range manifest.Items {
		for _, rendition := range draft.RenditionTranslations {
			if len(rendition.PeerTranslations) != 0 {
				return true
			}
		}
	}
	return false
}

func validateStagedImportRuntimeSchema(ctx context.Context, tx *sql.Tx) error {
	var minimumVersion, maximumVersion, count int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(version),0),COALESCE(MAX(version),0),COUNT(*) FROM schema_migrations`).
		Scan(&minimumVersion, &maximumVersion, &count); err != nil {
		return fmt.Errorf("read staged-import runtime schema: %w", err)
	}
	if minimumVersion != 1 || count != maximumVersion || maximumVersion < lyricsstaging.MaximumCatalogRuntimeSchema ||
		maximumVersion > lyricsImportMaximumCompatibleRuntimeSchema {
		return fmt.Errorf("staged lyrics import requires a contiguous schema-v%d through schema-v%d runtime",
			lyricsstaging.MaximumCatalogRuntimeSchema, lyricsImportMaximumCompatibleRuntimeSchema)
	}
	if maximumVersion >= db.LyricsPeerTranslationSchemaVersion {
		if err := db.ValidateLyricsPeerTranslationSchema(ctx, tx, true, "staged lyrics import"); err != nil {
			return err
		}
	}
	if maximumVersion >= db.LyricsTranslationEditionSchemaVersion {
		if err := db.ValidateLyricsTranslationEditionSchema(ctx, tx, true, "staged lyrics import"); err != nil {
			return err
		}
	}
	return nil
}

func loadStagedImportCatalog(ctx context.Context, tx *sql.Tx) ([]stagedImportCatalogItem, error) {
	rows, err := tx.QueryContext(ctx, `SELECT music_id,title_ja,producer_metadata,lyricist,composer,arranger,
		assetbundle_name,version_hint,lyrics_version,lyrics_evidence_presence_json,vocal_signals_json,
		lyrics_catalog_fingerprint,lyrics_catalog_policy_version FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return nil, fmt.Errorf("query staged-import catalog contract: %w", err)
	}
	defer rows.Close()
	items := []stagedImportCatalogItem{}
	lastMusicID := 0
	for rows.Next() {
		var item stagedImportCatalogItem
		var presenceJSON, vocalsJSON, policyVersion string
		if err := rows.Scan(&item.musicID, &item.japaneseTitle, &item.producerMetadata,
			&item.evidence.Lyricist, &item.evidence.Composer, &item.evidence.Arranger,
			&item.evidence.Assetbundle, &item.evidence.VersionHint, &item.evidence.LyricsVersion,
			&presenceJSON, &vocalsJSON, &item.catalogFingerprint, &policyVersion); err != nil {
			return nil, fmt.Errorf("scan staged-import catalog contract: %w", err)
		}
		if item.musicID <= lastMusicID || strings.TrimSpace(item.japaneseTitle) == "" ||
			policyVersion != model.LyricsCatalogIdentityPolicyVersion {
			return nil, fmt.Errorf("%w: catalog row %d violates the pinned identity policy", ErrLyricsStagedManifestDrift, item.musicID)
		}
		lastMusicID = item.musicID
		item.evidence.PolicyVersion = policyVersion
		item.evidence.Title = item.japaneseTitle
		if err := decodeStagedCatalogJSON([]byte(presenceJSON), &item.evidence.Presence); err != nil {
			return nil, fmt.Errorf("%w: catalog music %d evidence presence: %v", ErrLyricsStagedManifestDrift, item.musicID, err)
		}
		if err := decodeStagedCatalogJSON([]byte(vocalsJSON), &item.evidence.Vocals); err != nil {
			return nil, fmt.Errorf("%w: catalog music %d vocal signals: %v", ErrLyricsStagedManifestDrift, item.musicID, err)
		}
		computed, err := model.CatalogLyricsEvidenceFingerprint(item.evidence)
		if err != nil {
			return nil, fmt.Errorf("recompute catalog music %d fingerprint: %w", item.musicID, err)
		}
		if item.catalogFingerprint != computed {
			return nil, fmt.Errorf("%w: catalog music %d stored fingerprint does not match its evidence", ErrLyricsStagedManifestDrift, item.musicID)
		}
		item.vocals = append([]model.CatalogVocalSignal(nil), item.evidence.Vocals...)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func decodeStagedCatalogJSON(body []byte, target any) error {
	if len(body) == 0 || target == nil {
		return errors.New("JSON body and target are required")
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value")
	}
	return nil
}

func stagedManifestLyricsDraft(staged lyricsstaging.Draft, vocals []model.CatalogVocalSignal,
	catalogPerformers store.CatalogPerformerAliases,
) (model.SongLyrics, error) {
	fullIdentity, err := stagedFullTextIdentity(staged)
	if err != nil {
		return model.SongLyrics{}, err
	}
	authoritativePerformerSegmentation := staged.Document.PrivateReview != nil &&
		staged.Document.PrivateReview.PerformerSegmentationEvidence ==
			model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured
	segmentationPolicy := lyricssource.PerformerSegmentationPolicyFromCatalogVocals(vocals)
	kind := staged.Document.Full.Version.Kind
	if kind != "original" && kind != "sekai" && kind != "vocaloid" {
		return model.SongLyrics{}, fmt.Errorf("%w: music %d has invalid Full rendition kind", ErrLyricsStagedManifestDrift, staged.MusicID)
	}
	if segmentationPolicy == lyricssource.PerformerSegmentationDisabled && kind == "sekai" {
		return model.SongLyrics{}, fmt.Errorf("%w: music %d catalog-disabled document selected a SEKAI Full", ErrLyricsStagedManifestDrift, staged.MusicID)
	}
	if segmentationPolicy != lyricssource.PerformerSegmentationDisabled &&
		segmentationPolicy != lyricssource.PerformerSegmentationSekaiEligible {
		return model.SongLyrics{}, fmt.Errorf("%w: music %d has invalid catalog performer policy", ErrLyricsStagedManifestDrift, staged.MusicID)
	}
	if staged.Document.Provenance.PerformerSegmentation != nil && kind != "sekai" && !authoritativePerformerSegmentation {
		return model.SongLyrics{}, fmt.Errorf("%w: music %d non-SEKAI document lacks authoritative structured performer evidence", ErrLyricsStagedManifestDrift, staged.MusicID)
	}
	sourceLines := staged.Document.Full.LegacyExtractedLines()
	unassignedFull := staged.Document.Provenance.PerformerSegmentation == nil
	if unassignedFull && len(staged.Document.Full.Performers) != 0 {
		return model.SongLyrics{}, fmt.Errorf("%w: music %d unsegmented Full declares performers", ErrLyricsStagedManifestRebuildRequired, staged.MusicID)
	}
	aliases, declaredUnmapped := catalogPerformers.Resolve(staged.Document.Full.Performers)
	lines := make([]model.LyricLine, len(sourceLines))
	for lineIndex, sourceLine := range sourceLines {
		lineFallback := []int{}
		if unassignedFull {
			if len(sourceLine.Segments) != 1 || sourceLine.Segments[0].Text != sourceLine.Japanese ||
				len(sourceLine.Segments[0].PerformerIDs) != 0 || len(sourceLine.TrailingPerformerIDs) != 0 {
				return model.SongLyrics{}, fmt.Errorf("%w: music %d line %d violates unassigned Full segmentation", ErrLyricsStagedManifestRebuildRequired, staged.MusicID, lineIndex+1)
			}
		} else {
			lineFallback, err = store.MapDeclaredLyricsSourcePerformerIDs(
				sourceLine.TrailingPerformerIDs,
				aliases,
				declaredUnmapped,
			)
			if err != nil {
				return model.SongLyrics{}, err
			}
		}
		segments := make([]model.LyricSegment, len(sourceLine.Segments))
		var japanese strings.Builder
		for segmentIndex, sourceSegment := range sourceLine.Segments {
			performerIDs := []int{}
			if !unassignedFull {
				performerIDs, err = store.MapDeclaredLyricsSourcePerformerIDs(
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
			}
			// Some catalog songs expose only an outside_character vocalist.
			// Numeric lyrics performer IDs deliberately share the closed
			// game-character catalog namespace, so fabricating a colliding ID
			// would be unsafe. Preserve an empty editable draft assignment when
			// neither a mapped source label nor a catalog fallback exists;
			// ordinary publication validation still requires a performer.
			if strings.TrimSpace(sourceSegment.Text) == "" {
				return model.SongLyrics{}, fmt.Errorf("%w: music %d line %d segment %d has empty text",
					ErrLyricsStagedManifestDrift, staged.MusicID, lineIndex+1, segmentIndex+1)
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
		if japanese.String() != sourceLine.Japanese {
			return model.SongLyrics{}, fmt.Errorf("%w: music %d line %d source segments changed",
				ErrLyricsStagedManifestDrift, staged.MusicID, lineIndex+1)
		}
		chinese := ""
		if staged.Translations != nil {
			chinese = staged.Translations[lineIndex]
		}
		lines[lineIndex] = model.LyricLine{
			ID:    staged.Document.Full.Lines[lineIndex].ID,
			Order: lineIndex, Japanese: sourceLine.Japanese, Chinese: chinese, English: "",
			StanzaBreakBefore: sourceLine.StanzaBreakBefore, Segments: segments,
		}
	}
	return model.SongLyrics{
		MusicID: staged.MusicID, Status: "draft", Revision: 0,
		Attribution: "", SourceNote: "", LicenseNote: "", SourceURL: fullIdentity.CanonicalURL,
		SourcePageID: fullIdentity.PageID, SourceRevisionID: fullIdentity.RevisionID,
		SourceSHA1: fullIdentity.SHA1, SourceFetchedAt: fullIdentity.FetchedAt, Lines: lines,
	}, nil
}

func insertStagedLyricsDraft(ctx context.Context, tx *sql.Tx, lyrics model.SongLyrics, staged lyricsstaging.Draft, actor, batchSHA256 string, now int64, linkEvidence bool) (model.SongLyrics, error) {
	sourceHash := store.LyricsSourceHash(lyrics.Lines)
	if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics
		(music_id,revision,updated_at,updated_by,attribution,source_note,source_url,license_note,source_hash,
		 source_page_id,source_revision_id,source_sha1,source_fetched_at,source_fetched_at_rfc3339)
		VALUES (?,1,?,?,?,?,?,?,?,?,?,?,?,?)`, lyrics.MusicID, now, actor, lyrics.Attribution, lyrics.SourceNote,
		lyrics.SourceURL, lyrics.LicenseNote, sourceHash, lyrics.SourcePageID, lyrics.SourceRevisionID,
		lyrics.SourceSHA1, mustParseStagedTimestamp(lyrics.SourceFetchedAt), lyrics.SourceFetchedAt); err != nil {
		return model.SongLyrics{}, err
	}
	for _, line := range lyrics.Lines {
		stanzaBreak := 0
		if line.StanzaBreakBefore {
			stanzaBreak = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_lines
			(music_id,line_id,position,japanese,zh_cn,en_us,stanza_break_before) VALUES (?,?,?,?,?,?,?)`,
			lyrics.MusicID, line.ID, line.Order, line.Japanese, line.Chinese, line.English, stanzaBreak); err != nil {
			return model.SongLyrics{}, err
		}
		for position, segment := range line.Segments {
			performersJSON, err := json.Marshal(segment.PerformerIDs)
			if err != nil {
				return model.SongLyrics{}, err
			}
			rubyJSON, err := json.Marshal(segment.Ruby)
			if err != nil {
				return model.SongLyrics{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_segments
				(music_id,line_id,position,text,performer_ids_json,ruby_json) VALUES (?,?,?,?,?,?)`,
				lyrics.MusicID, line.ID, position, segment.Text, string(performersJSON), string(rubyJSON)); err != nil {
				return model.SongLyrics{}, err
			}
		}
	}
	if err := insertStagedLyricsSourceDocument(ctx, tx, staged, batchSHA256, now, linkEvidence); err != nil {
		return model.SongLyrics{}, err
	}
	var documentID int64
	if err := tx.QueryRowContext(ctx, `SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, staged.MusicID).Scan(&documentID); err != nil {
		return model.SongLyrics{}, err
	}
	if err := store.InsertLyricsRenditionLocalizationsTx(ctx, tx, documentID, staged.Document, staged.RenditionTranslations, actor, now); err != nil {
		return model.SongLyrics{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.import_stage',?)`,
		now, actor, fmt.Sprintf("musicId=%d revision=1 batchSha256=%s", lyrics.MusicID, batchSHA256)); err != nil {
		return model.SongLyrics{}, err
	}
	lyrics.Status = "draft"
	lyrics.Revision = 1
	lyrics.UpdatedAt = store.FormatLyricsTimestamp(now)
	return lyrics, nil
}

func insertStagedV3Draft(ctx context.Context, tx *sql.Tx, staged lyricsstaging.Draft, actor, batchSHA256 string, now int64, linkEvidence bool) (model.SongLyrics, error) {
	if err := ensureRecoveryItemOwnsNoEditableLyrics(ctx, tx, staged.MusicID); err != nil {
		return model.SongLyrics{}, err
	}
	if err := insertStagedLyricsSourceDocument(ctx, tx, staged, batchSHA256, now, linkEvidence); err != nil {
		return model.SongLyrics{}, err
	}
	var documentID int64
	if err := tx.QueryRowContext(ctx, `SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, staged.MusicID).Scan(&documentID); err != nil {
		return model.SongLyrics{}, err
	}
	if err := store.InsertLyricsRenditionLocalizationsTx(ctx, tx, documentID, staged.Document, staged.RenditionTranslations, actor, now); err != nil {
		return model.SongLyrics{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.import_stage',?)`,
		now, actor, fmt.Sprintf("musicId=%d revision=1 batchSha256=%s schemaVersion=3", staged.MusicID, batchSHA256)); err != nil {
		return model.SongLyrics{}, err
	}
	return model.SongLyrics{}, nil
}

func stagedManifestEvidenceCandidates(manifest lyricsstaging.Manifest) ([]lyricsstaging.CandidateIdentity, error) {
	candidates, err := lyricsstaging.EvidenceCandidatesFromValidatedManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLyricsStagedManifestRebuildRequired, err)
	}
	return candidates, nil
}

func stagedFullTextIdentity(staged lyricsstaging.Draft) (model.LyricsSourceFixedIdentity, error) {
	renditionKey := staged.Document.Provenance.FullText.RenditionKey
	for _, identity := range staged.Document.FixedIdentities {
		if identity.RenditionKey == renditionKey {
			return identity, nil
		}
	}
	return model.LyricsSourceFixedIdentity{}, fmt.Errorf("%w: music %d full-text fixed identity is missing", ErrLyricsStagedManifestRebuildRequired, staged.MusicID)
}

func insertStagedLyricsSourceDocument(ctx context.Context, tx *sql.Tx, staged lyricsstaging.Draft, batchSHA256 string, now int64, linkEvidence bool) error {
	if err := store.ValidatePersistedLyricsSourceDocument(staged.Document); err != nil {
		return fmt.Errorf("%w: music %d: %v", ErrLyricsStagedManifestRebuildRequired, staged.MusicID, err)
	}
	documentJSON, err := json.Marshal(staged.Document)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?)`, staged.MusicID, staged.Document.SchemaVersion, staged.Document.ReasonCode,
		string(documentJSON), staged.DocumentSHA256, batchSHA256, now)
	if err != nil {
		return err
	}
	documentID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	for _, artifact := range staged.Artifacts {
		identityJSON, err := json.Marshal(artifact.Identity)
		if err != nil {
			return err
		}
		identityDigest := sha256.Sum256(identityJSON)
		categoriesJSON, err := json.Marshal(artifact.Identity.Categories)
		if err != nil {
			return err
		}
		indexJSON, err := json.Marshal(artifact.Identity.IndexEvidenceRefs)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_artifacts
			(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
			 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
			 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, artifact.Identity.Provider,
			artifact.Identity.RenditionKey, artifact.Identity.Origin, artifact.Identity.PageID, artifact.Identity.RevisionID,
			artifact.Identity.RevisionTimestamp, artifact.Identity.SHA1, artifact.Identity.Title, artifact.Identity.CanonicalURL,
			artifact.Identity.FetchedAt, string(categoriesJSON), artifact.Identity.Section,
			artifact.Identity.CompositionRenditionKey, artifact.Identity.VersionReason,
			string(indexJSON), string(identityJSON), hex.EncodeToString(identityDigest[:]), artifact.RawWikitextByteCount,
			artifact.RawWikitextSHA256, artifact.ArtifactSHA256); err != nil {
			return err
		}
		if linkEvidence {
			for position, reference := range artifact.Identity.IndexEvidenceRefs {
				if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_artifact_index_evidence
					(document_id,rendition_key,position,provider,evidence_id,sha256) VALUES (?,?,?,?,?,?)`,
					documentID, artifact.Identity.RenditionKey, position, artifact.Identity.Provider,
					reference.EvidenceID, reference.SHA256); err != nil {
					return err
				}
			}
		}
	}
	for component, renditionKey := range store.LyricsSourceComponentRefs(staged.Document) {
		contributionDigest := sha256.Sum256([]byte(staged.DocumentSHA256 + "\x00" + component + "\x00" + renditionKey))
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_component_contributions
			(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, documentID, component,
			renditionKey, hex.EncodeToString(contributionDigest[:])); err != nil {
			return err
		}
	}
	return nil
}

func stagedLyricsSourceDocumentMatches(ctx context.Context, tx *sql.Tx, staged lyricsstaging.Draft, requireEvidenceLinks bool) (bool, bool, error) {
	var documentID int64
	var storedSHA string
	if err := tx.QueryRowContext(ctx, `SELECT document_id,document_sha256 FROM song_lyrics_source_documents WHERE music_id=?`, staged.MusicID).
		Scan(&documentID, &storedSHA); err == sql.ErrNoRows {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	if storedSHA != staged.DocumentSHA256 {
		return true, false, nil
	}
	var artifactCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics_source_artifacts WHERE document_id=?`, documentID).Scan(&artifactCount); err != nil {
		return true, false, err
	}
	if artifactCount != len(staged.Artifacts) {
		return true, false, nil
	}
	for _, artifact := range staged.Artifacts {
		var storedArtifactSHA, storedRawSHA string
		var storedRawCount int
		if err := tx.QueryRowContext(ctx, `SELECT artifact_sha256,raw_wikitext_sha256,raw_byte_count
			FROM song_lyrics_source_artifacts WHERE document_id=? AND rendition_key=?`, documentID,
			artifact.Identity.RenditionKey).Scan(&storedArtifactSHA, &storedRawSHA, &storedRawCount); err == sql.ErrNoRows {
			return true, false, nil
		} else if err != nil {
			return true, false, err
		}
		if storedArtifactSHA != artifact.ArtifactSHA256 || storedRawSHA != artifact.RawWikitextSHA256 ||
			storedRawCount != artifact.RawWikitextByteCount {
			return true, false, nil
		}
		if requireEvidenceLinks {
			var linkCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence
				WHERE document_id=? AND rendition_key=?`, documentID, artifact.Identity.RenditionKey).Scan(&linkCount); err != nil {
				return true, false, err
			}
			if linkCount != len(artifact.Identity.IndexEvidenceRefs) {
				return true, false, nil
			}
			for position, reference := range artifact.Identity.IndexEvidenceRefs {
				var provider model.LyricsSourceProvider
				var evidenceID, digest string
				if err := tx.QueryRowContext(ctx, `SELECT provider,evidence_id,sha256
					FROM song_lyrics_source_artifact_index_evidence
					WHERE document_id=? AND rendition_key=? AND position=?`, documentID,
					artifact.Identity.RenditionKey, position).Scan(&provider, &evidenceID, &digest); err != nil {
					return true, false, err
				}
				if provider != artifact.Identity.Provider || evidenceID != reference.EvidenceID || digest != reference.SHA256 {
					return true, false, nil
				}
			}
		}
	}
	refs := store.LyricsSourceComponentRefs(staged.Document)
	var contributionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics_component_contributions WHERE document_id=?`, documentID).
		Scan(&contributionCount); err != nil {
		return true, false, err
	}
	if contributionCount != len(refs) {
		return true, false, nil
	}
	for component, renditionKey := range refs {
		digest := sha256.Sum256([]byte(staged.DocumentSHA256 + "\x00" + component + "\x00" + renditionKey))
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT contribution_sha256 FROM song_lyrics_component_contributions
			WHERE document_id=? AND component=? AND rendition_key=?`, documentID, component, renditionKey).Scan(&stored); err == sql.ErrNoRows {
			return true, false, nil
		} else if err != nil {
			return true, false, err
		}
		if stored != hex.EncodeToString(digest[:]) {
			return true, false, nil
		}
	}
	if staged.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		storedTranslations, err := store.ExportLyricsRenditionLocalizationsTx(ctx, tx, documentID, staged.Document)
		if err != nil {
			return true, false, err
		}
		storedDigest, err := store.LyricsRenditionTranslationsDigest(storedTranslations)
		if err != nil {
			return true, false, err
		}
		expectedDigest, err := store.LyricsRenditionTranslationsDigest(staged.RenditionTranslations)
		if err != nil || storedDigest != expectedDigest {
			return true, false, err
		}
	}
	return true, true, nil
}

func mustParseStagedTimestamp(value string) int64 {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed.Unix()
}

func sameStagedAssociationIDs(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func stagedManifestMusicIDs(manifest lyricsstaging.Manifest) []int {
	musicIDs := make([]int, 0, len(manifest.Items))
	for _, item := range manifest.Items {
		musicIDs = append(musicIDs, item.MusicID)
	}
	return musicIDs
}
