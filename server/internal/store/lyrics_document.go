package store

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsperformers"
	"moesekai/server/internal/model"
)

// LyricsDocumentRequest is the admin whole-song publish payload. Ruby is
// written inline in each ja line as {kanji|reading}; {{ and }} write a literal
// brace. zh is plain text. TranslationEditions lists the song's zh-CN
// translation editions, the default first: zh and the credits are the default
// edition's, zhEditions and editionCredits carry the others. Omitted, the song
// has the one implicit edition main labelled 默认译本.
type LyricsDocumentRequest struct {
	MusicID             int                               `json:"musicId"`
	ExpectedRevision    *int                              `json:"expectedRevision,omitempty"`
	Source              LyricsDocumentSource              `json:"source"`
	TranslationCredit   string                            `json:"translationCredit,omitempty"`
	ProofreadingCredit  string                            `json:"proofreadingCredit,omitempty"`
	TranslationEditions []LyricsTranslationEditionSummary `json:"translationEditions,omitempty"`
	Renditions          []LyricsDocumentRendition         `json:"renditions"`
	DryRun              bool                              `json:"dryRun,omitempty"`
}

type LyricsDocumentSource struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// LyricsDocumentRendition.Game is none (Full only), same (Game identical to
// Full), cut (Game is the Full lines marked inGame), independent (Game is
// GameLines) or only (a Game-only rendition: GameLines and no Lines). A nil
// PerformerIDs falls back to the catalog vocal performers. TranslationCredits
// replaces the document credits for this rendition, translated or not; an
// empty object publishes the rendition without credits. EditionCredits are
// the credits of this rendition in non-default editions.
type LyricsDocumentRendition struct {
	Key                string                           `json:"key"`
	Kind               string                           `json:"kind,omitempty"`
	Label              string                           `json:"label,omitempty"`
	PerformerIDs       []int                            `json:"performerIds,omitempty"`
	Game               string                           `json:"game,omitempty"`
	Lines              []LyricsDocumentLine             `json:"lines"`
	GameLines          []LyricsDocumentLine             `json:"gameLines,omitempty"`
	TranslationCredits *LyricsDocumentCredits           `json:"translationCredits,omitempty"`
	EditionCredits     map[string]LyricsDocumentCredits `json:"editionCredits,omitempty"`
}

type LyricsDocumentCredits struct {
	Translation  string `json:"translation,omitempty"`
	Proofreading string `json:"proofreading,omitempty"`
}

// LyricsDocumentLine.PerformerIDs overrides the rendition default when set;
// an explicit empty list marks a line without performers. Performers keep the
// order given, which the main site uses for colour gradients and avatars.
// Segments split the line into runs sung by different performers; their ja
// markups concatenate to the line's ja. ChineseEditions holds the line's zh in
// non-default editions; a missing edition has an empty line.
type LyricsDocumentLine struct {
	Japanese          string                  `json:"ja"`
	Chinese           string                  `json:"zh,omitempty"`
	ChineseEditions   map[string]string       `json:"zhEditions,omitempty"`
	English           string                  `json:"en,omitempty"`
	PerformerIDs      []int                   `json:"performerIds,omitempty"`
	Segments          []LyricsDocumentSegment `json:"segments,omitempty"`
	StanzaBreakBefore bool                    `json:"stanzaBreakBefore,omitempty"`
	InGame            bool                    `json:"inGame,omitempty"`
}

// LyricsDocumentSegment is one run of a line's ja markup. A nil PerformerIDs
// falls back to the line's performers.
type LyricsDocumentSegment struct {
	Japanese     string `json:"ja"`
	PerformerIDs []int  `json:"performerIds"`
}

type LyricsDocumentResult struct {
	DryRun     bool                  `json:"dryRun"`
	MusicID    int                   `json:"musicId"`
	Revision   int                   `json:"revision"`
	PublicPath string                `json:"publicPath"`
	Document   json.RawMessage       `json:"document"`
	Changes    LyricsDocumentChanges `json:"changes"`
}

// LyricsDocumentServed is what the public site serves for the song when a
// document request arrives: the embedded bundle revision (0 when absent) and
// the served detail bytes (nil when the caller has none or nothing is served).
type LyricsDocumentServed struct {
	BundleRevision int
	Detail         []byte
}

// LyricsDocumentIssue locates one validation failure. Line is zero-based and
// absent for rendition- or document-level issues; markup messages name the
// zero-based character index in the line's ja and the offending text. Edition
// names the translation edition key of a translationEditions, zhEditions or
// editionCredits issue.
type LyricsDocumentIssue struct {
	Rendition string `json:"rendition"`
	Side      string `json:"side,omitempty"`
	Line      *int   `json:"line,omitempty"`
	Field     string `json:"field,omitempty"`
	Edition   string `json:"edition,omitempty"`
	Message   string `json:"message"`
}

type LyricsDocumentError struct {
	Code    string
	Details []string
	Issues  []LyricsDocumentIssue
	Current any
}

func (e *LyricsDocumentError) Error() string { return e.Code }

const (
	LyricsDocumentErrorInvalid          = "invalid_lyrics_document"
	LyricsDocumentErrorSourceRevision   = "source_revision_required"
	LyricsDocumentErrorRevisionConflict = "revision_conflict"
	LyricsDocumentErrorRevisionRequired = "expected_revision_required"
	LyricsDocumentErrorNotFound         = "not_found"

	lyricsDocumentRubyGeneratorVersion = "moe-lyrics-document-ruby-markup-v1"
	// Editor-published documents belong to no recovery or seed manifest; the
	// migration v32 song-682 document uses the same all-zero batch identity.
	lyricsDocumentManifestBatchSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
)

func LyricsDocumentPublicPath(musicID int) string {
	return fmt.Sprintf("/files/translation/lyrics/music_%d.json", musicID)
}

// lyricsDocumentDraft is a request compiled into the source-v3 document and
// the zh-CN localization rows that the public projection will read.
type lyricsDocumentDraft struct {
	musicID      int
	document     model.LyricsSourceDocument
	bindings     []model.LyricsSourceRenditionComponentBinding
	identities   map[string]model.LyricsSourceFixedIdentity
	translations []lyricscontract.RenditionTranslation
	sides        map[string]map[string][]string
	// editions is nil for the implicit main edition, else every edition, the
	// default first, as the edition tables store it.
	editions []lyricsDocumentEdition
	now      int64
}

type lyricsDocumentCurrentState struct {
	legacyRevision       int
	publicationRevision  int
	localizationRevision int
	editionRevision      int
	bundleRevision       int
	sourceDocuments      int
	recoveryItems        int
	recoveryTakenOver    bool
	served               bool
}

// effectiveRevision is the revision the public site currently serves for the
// song: the legacy publication, the source-v3 localization projection, or the
// embedded bundle, whichever is newest.
func (state lyricsDocumentCurrentState) effectiveRevision() int {
	result := max(state.publicationRevision, state.bundleRevision)
	if state.localizationRevision > 1 {
		result = max(result, state.localizationRevision)
	}
	return result
}

// occupied reports lyrics stored or served that a publish replaces: a legacy
// draft or publication, a source document, a recovery ledger item, a bundle
// entry or a served detail.
func (state lyricsDocumentCurrentState) occupied() bool {
	return state.legacyRevision > 0 || state.publicationRevision > 0 || state.sourceDocuments > 0 ||
		state.recoveryItems > 0 || state.bundleRevision > 0 || state.served
}

// conflictRevision is what expectedRevision must equal: the served revision,
// or a newer legacy draft, which a publish replaces, so a legacy save between
// an export and its PUT is a conflict. It is at least 1 for an occupied song,
// so 0 names only a song with nothing stored or served.
func (state lyricsDocumentCurrentState) conflictRevision() int {
	result := max(state.effectiveRevision(), state.legacyRevision)
	if state.occupied() {
		result = max(result, 1)
	}
	return result
}

// nextRevision stays above every stored and bundled revision, and above 1
// because the localization projection treats revision 1 as the unedited
// recovery baseline.
func (state lyricsDocumentCurrentState) nextRevision() int {
	return max(state.legacyRevision, state.publicationRevision, state.localizationRevision,
		state.editionRevision, state.bundleRevision, 1) + 1
}

// PublishLyricsDocument publishes request with no served detail at hand, so
// its changes compare with the database state.
func (s *Store) PublishLyricsDocument(ctx context.Context, request LyricsDocumentRequest, bundleRevision int, user string) (LyricsDocumentResult, error) {
	return s.PublishLyricsDocumentServed(ctx, request, LyricsDocumentServed{BundleRevision: bundleRevision}, user)
}

// PublishLyricsDocumentServed replaces the song's lyrics with one source-v3
// document and its zh-CN localization, which the public projection then
// serves as the v3 detail (v4 with several translation editions). A dry run
// runs every write of the publish and rolls them back, so it fails wherever
// the publish would. expectedRevision is required once the song has anything
// stored or served. The result's Document is the detail the site will serve.
// Changes compare the stored result with served.Detail, else with the
// database state. The first publish of a song held by the recovery import
// ledger records a takeover of its ledger item; the ledger rows stay
// unchanged.
func (s *Store) PublishLyricsDocumentServed(ctx context.Context, request LyricsDocumentRequest, served LyricsDocumentServed, user string) (LyricsDocumentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.MusicID <= 0 {
		return LyricsDocumentResult{}, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{"musicId must be a positive integer"}}
	}
	source, err := parseLyricsDocumentSourceURL(request.Source.URL, request.Source.Title)
	if err != nil {
		return LyricsDocumentResult{}, &LyricsDocumentError{Code: LyricsDocumentErrorSourceRevision, Details: []string{err.Error()}}
	}
	catalog, err := loadCatalogMusicIdentityContext(ctx, s.db, request.MusicID)
	if errors.Is(err, sql.ErrNoRows) {
		return LyricsDocumentResult{}, &LyricsDocumentError{Code: LyricsDocumentErrorNotFound, Details: []string{"musicId is not in the catalog"}}
	} else if err != nil {
		return LyricsDocumentResult{}, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	draft, err := compileLyricsDocument(request, source, catalog.Vocals, now)
	if err != nil {
		return LyricsDocumentResult{}, err
	}

	unlock := s.lockLyrics(request.MusicID)
	defer unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LyricsDocumentResult{}, err
	}
	defer tx.Rollback()
	state, err := loadLyricsDocumentCurrentState(tx, request.MusicID, served.BundleRevision)
	if err != nil {
		return LyricsDocumentResult{}, err
	}
	state.served = served.Detail != nil
	if current := state.conflictRevision(); request.ExpectedRevision == nil && state.occupied() {
		return LyricsDocumentResult{}, &LyricsDocumentError{
			Code: LyricsDocumentErrorRevisionRequired,
			Details: []string{fmt.Sprintf(
				"expectedRevision is required because the song already has lyrics stored or served; send %d, the revision GET reports, to replace them", current)},
			Current: map[string]int{"revision": current},
		}
	}
	if current := state.conflictRevision(); request.ExpectedRevision != nil && *request.ExpectedRevision != current {
		return LyricsDocumentResult{}, &LyricsDocumentError{
			Code:    LyricsDocumentErrorRevisionConflict,
			Details: []string{fmt.Sprintf("expectedRevision %d does not match the current revision %d", *request.ExpectedRevision, current)},
			Current: map[string]int{"revision": current},
		}
	}
	revision := state.nextRevision()
	body, err := draft.publicDetail(revision)
	if err != nil {
		return LyricsDocumentResult{}, err
	}
	baseline, against, err := s.lyricsDocumentChangeBaseline(tx, request.MusicID, served.Detail)
	if err != nil {
		return LyricsDocumentResult{}, fmt.Errorf("read the lyrics the document replaces: %w", err)
	}
	if state.recoveryItems > 0 && !state.recoveryTakenOver {
		if err := takeOverLyricsRecoverySongTx(ctx, tx, request.MusicID, draft.now, user); err != nil {
			return LyricsDocumentResult{}, fmt.Errorf("record lyrics recovery takeover: %w", err)
		}
	}
	if err := draft.replaceTx(ctx, tx, revision, user); err != nil {
		return LyricsDocumentResult{}, err
	}
	document, stored, err := s.lyricsDocumentStoredDetail(tx, request.MusicID, body)
	if err != nil {
		return LyricsDocumentResult{}, fmt.Errorf("reload published lyrics document: %w", err)
	}
	result := LyricsDocumentResult{
		DryRun: request.DryRun, MusicID: request.MusicID, Revision: revision,
		PublicPath: LyricsDocumentPublicPath(request.MusicID), Document: document,
		Changes: lyricsDocumentChangesBetween(against, baseline, stored),
	}
	if request.DryRun {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return LyricsDocumentResult{}, err
	}
	s.invalidateLocalizationProjectionCache()
	s.NotifyChange()
	return result, nil
}

func loadLyricsDocumentCurrentState(q queryRower, musicID, bundleRevision int) (lyricsDocumentCurrentState, error) {
	state := lyricsDocumentCurrentState{bundleRevision: max(bundleRevision, 0)}
	err := q.QueryRow(`SELECT
		COALESCE((SELECT revision FROM song_lyrics WHERE music_id=?),0),
		COALESCE((SELECT revision FROM song_lyrics_publications WHERE music_id=?),0),
		COALESCE((SELECT MAX(l.revision) FROM song_lyrics_rendition_localizations AS l
		 JOIN song_lyrics_source_documents AS d ON d.document_id=l.document_id WHERE d.music_id=?),0),
		COALESCE((SELECT e.revision FROM song_lyrics_translation_edition_state AS e
		 JOIN song_lyrics_source_documents AS d ON d.document_id=e.document_id WHERE d.music_id=?),0),
		(SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=?),
		(SELECT COUNT(*) FROM lyrics_recovery_import_items WHERE music_id=?),
		EXISTS(SELECT 1 FROM lyrics_recovery_takeovers WHERE music_id=?)`,
		musicID, musicID, musicID, musicID, musicID, musicID, musicID).Scan(&state.legacyRevision, &state.publicationRevision,
		&state.localizationRevision, &state.editionRevision, &state.sourceDocuments, &state.recoveryItems, &state.recoveryTakenOver)
	return state, err
}

// takeOverLyricsRecoverySongTx records the recovery ledger item a whole-song
// publish supersedes, before replaceTx deletes the item's source document: the
// item owning the current source document, whose row is copied verbatim with
// the localization and translation-edition rows it owns, or else the song's
// newest item.
func takeOverLyricsRecoverySongTx(ctx context.Context, tx *sql.Tx, musicID int, now int64, user string) error {
	var batchSHA, state string
	var documentID int64
	var schemaVersion, createdAt sql.NullInt64
	var reasonCode, documentJSON, documentSHA, localizationsJSON sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT item.batch_sha256,item.state,source.document_id,source.schema_version,
		source.reason_code,source.document_json,source.document_sha256,source.created_at
		FROM song_lyrics_source_documents AS source
		JOIN lyrics_recovery_import_items AS item
		 ON item.batch_sha256=source.manifest_batch_sha256 AND item.music_id=source.music_id
		WHERE source.music_id=?`, musicID).Scan(&batchSHA, &state, &documentID, &schemaVersion, &reasonCode,
		&documentJSON, &documentSHA, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT batch_sha256,state FROM lyrics_recovery_import_items
			WHERE music_id=? ORDER BY created_at DESC,batch_sha256 DESC LIMIT 1`, musicID).Scan(&batchSHA, &state)
	} else if err == nil {
		localizationsJSON, err = supersededLyricsRecoveryLocalizationsTx(ctx, tx, documentID, documentJSON.String)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_takeovers
		(music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
		 document_created_at,localizations_json,taken_over_at,taken_over_by) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		musicID, batchSHA, state, schemaVersion, reasonCode, documentJSON, documentSHA, createdAt,
		localizationsJSON, now, user)
	return err
}

func compileLyricsDocument(request LyricsDocumentRequest, source lyricsDocumentSourceRevision, vocals []model.CatalogVocalSignal, now time.Time) (lyricsDocumentDraft, error) {
	var issues []LyricsDocumentIssue
	addIssue := func(rendition, side string, line int, field, message string) {
		issue := LyricsDocumentIssue{Rendition: rendition, Side: side, Field: field, Message: message}
		if line >= 0 {
			lineIndex := line
			issue.Line = &lineIndex
		}
		issues = append(issues, issue)
	}
	translationCredit := strings.TrimSpace(request.TranslationCredit)
	proofreadingCredit := strings.TrimSpace(request.ProofreadingCredit)
	for _, credit := range []struct{ field, value string }{
		{"translationCredit", translationCredit}, {"proofreadingCredit", proofreadingCredit},
	} {
		if !validLyricsDocumentCredit(credit.value) {
			addIssue("", "", -1, credit.field, lyricsDocumentCreditProblem(credit.field, credit.value))
		}
	}
	editionSet, editionIssues := compileLyricsDocumentEditions(request.TranslationEditions)
	issues = append(issues, editionIssues...)
	if len(request.Renditions) == 0 || len(request.Renditions) > maxLyricsSourceRenditionsForDocument {
		addIssue("", "", -1, "renditions", fmt.Sprintf("renditions must contain 1 to %d entries; it has %d",
			maxLyricsSourceRenditionsForDocument, len(request.Renditions)))
	}
	fetchedAt := now.Format(time.RFC3339)
	draft := lyricsDocumentDraft{
		musicID: request.MusicID, identities: map[string]model.LyricsSourceFixedIdentity{},
		sides: map[string]map[string][]string{}, editions: editionSet.draftEditions(), now: now.Unix(),
	}
	document := model.LyricsSourceDocument{SchemaVersion: model.LyricsSourceDocumentSchemaVersionV3}
	seenKeys := map[string]bool{}
	hasTranslation, credited := false, false
	var uncredited []string
	// A rendition-level problem skips the rendition but its lines are still
	// checked, so one response lists every issue.
	for _, input := range request.Renditions {
		key := input.Key
		valid := true
		renditionIssue := func(side, field, message string) {
			addIssue(key, side, -1, field, message)
			valid = false
		}
		if !publicV3RenditionKeyPattern.MatchString(key) || len("fixed-"+key) > 128 {
			renditionIssue("", "key", fmt.Sprintf("key %q must match ^[a-z0-9][a-z0-9._-]*$ and be at most 122 bytes", key))
		} else if seenKeys[key] {
			renditionIssue("", "key", "rendition key is repeated")
		}
		seenKeys[key] = true
		kind := model.LyricsSourceRenditionKind(input.Kind)
		if kind == "" {
			kind = model.LyricsSourceRenditionKind(key)
		}
		if !model.IsValidLyricsSourceRenditionKind(kind) {
			renditionIssue("", "kind", fmt.Sprintf("kind %q must be original, sekai, vocaloid or alternate", kind))
		}
		label := strings.TrimSpace(input.Label)
		if label == "" {
			label = lyricsDocumentDefaultLabel(kind)
		}
		if label == "" && model.IsValidLyricsSourceRenditionKind(kind) {
			renditionIssue("", "label", "an alternate rendition needs a label that names its Alternate or Another Vocal version")
		}
		game := input.Game
		if game == "" {
			game = "none"
		}
		knownGame := game == "none" || game == "same" || game == "cut" || game == "independent" || game == "only"
		if !knownGame {
			renditionIssue("", "game", fmt.Sprintf("game %q must be none, same, cut, independent or only", game))
		}
		ownGame := game == "independent" || game == "only"
		switch {
		case game == "only" && len(input.Lines) != 0:
			renditionIssue("full", "lines", "a Game-only rendition has no lines; its lines go in gameLines")
		case game != "only" && len(input.Lines) == 0:
			renditionIssue("full", "lines", "lines must not be empty")
		case game == "independent" && len(input.GameLines) == 0:
			renditionIssue("game", "gameLines", "an independent Game needs gameLines")
		case game == "only" && len(input.GameLines) == 0:
			renditionIssue("game", "gameLines", "a Game-only rendition needs gameLines")
		case knownGame && !ownGame && len(input.GameLines) != 0:
			renditionIssue("game", "gameLines", "gameLines are only valid when game is independent or only")
		}
		var renditionCredits *LyricsDocumentCredits
		if input.TranslationCredits != nil {
			renditionCredits = &LyricsDocumentCredits{
				Translation:  strings.TrimSpace(input.TranslationCredits.Translation),
				Proofreading: strings.TrimSpace(input.TranslationCredits.Proofreading),
			}
			for _, credit := range []struct{ field, value string }{
				{"translationCredits.translation", renditionCredits.Translation},
				{"translationCredits.proofreading", renditionCredits.Proofreading},
			} {
				if !validLyricsDocumentCredit(credit.value) {
					addIssue(key, "", -1, credit.field, lyricsDocumentCreditProblem(credit.field, credit.value))
				}
			}
			credited = credited || renditionCredits.Translation != "" || renditionCredits.Proofreading != ""
		} else if lyricsDocumentRenditionTranslated(input) {
			uncredited = append(uncredited, key)
			credited = credited || translationCredit != "" || proofreadingCredit != ""
		}
		editionCredits, creditIssues := editionSet.renditionCredits(key, input.EditionCredits)
		fullEditions, fullEditionIssues := editionSet.lineTexts(key, "full", input.Lines)
		gameEditions, gameEditionIssues := editionSet.lineTexts(key, "game", input.GameLines)
		issues = append(append(append(issues, creditIssues...), fullEditionIssues...), gameEditionIssues...)
		defaults := input.PerformerIDs
		if defaults == nil {
			defaults = lyricsDocumentCatalogPerformers(kind, vocals)
		}
		identityKey := "fixed-" + key
		reason := model.LyricsSourceVersionReasonUntaggedFullOnly
		switch game {
		case "same":
			reason = model.LyricsSourceVersionReasonUntaggedUncutIdentity
		case "cut", "independent":
			reason = model.LyricsSourceVersionReasonTaggedFullAndGame
		case "only":
			reason = model.LyricsSourceVersionReasonTaggedGameOnly
		}
		version := model.LyricsSourceVersion{Kind: string(kind), Label: label}
		var full *model.LyricsSourceFull
		var fullTranslations []string
		if len(input.Lines) > 0 {
			var fullIssues []LyricsDocumentIssue
			full, fullTranslations, fullIssues = buildLyricsDocumentSide(key, model.LyricsSourceRenditionSideFull, version, identityKey, input.Lines, defaults, game == "cut" || !knownGame)
			issues = append(issues, fullIssues...)
		}
		rendition := model.LyricsSourceRendition{
			RenditionKey: key, SourceKind: kind, SourceTabPaths: []model.LyricsSourceTabPath{{label}},
			ReasonCode: reason, Full: full,
			Relation: model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
		}
		var gameTranslations []string
		if len(input.GameLines) > 0 && (ownGame || !knownGame) {
			var gameIssues []LyricsDocumentIssue
			rendition.Game, gameTranslations, gameIssues = buildLyricsDocumentSide(key, model.LyricsSourceRenditionSideGame, version, identityKey, input.GameLines, defaults, false)
			issues = append(issues, gameIssues...)
		}
		if !valid || game != "only" && full == nil || ownGame && rendition.Game == nil {
			continue
		}
		if game == "same" || game == "cut" {
			lineIDs := []string{}
			for index, line := range input.Lines {
				if game == "same" || line.InGame {
					lineIDs = append(lineIDs, full.Lines[index].ID)
				}
			}
			if len(lineIDs) == 0 {
				addIssue(key, "full", -1, "inGame", "a cut Game needs at least one line with inGame: true")
				continue
			}
			rendition.Relation = model.LyricsSourceRenditionRelation{
				Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: key, LineIDs: lineIDs,
			}
		}
		reference := model.LyricsSourceComponentRef{RenditionKey: identityKey}
		ref := func(present bool) *model.LyricsSourceComponentRef {
			if !present {
				return nil
			}
			copy := reference
			return &copy
		}
		rendition.Provenance = model.LyricsSourceRenditionProvenance{RelationEvidence: reference, VersionEvidence: reference}
		if full != nil {
			rendition.Provenance.FullText = ref(true)
			rendition.Provenance.FullPerformerSegmentation = ref(model.LyricsSourceFullHasPerformerSegmentation(*full))
			rendition.Provenance.FullRuby = ref(lyricsSourceFullHasRuby(*full))
		}
		if rendition.Game != nil {
			rendition.Provenance.GameText = ref(true)
			rendition.Provenance.GamePerformerSegmentation = ref(model.LyricsSourceFullHasPerformerSegmentation(*rendition.Game))
			rendition.Provenance.GameRuby = ref(lyricsSourceFullHasRuby(*rendition.Game))
		}
		rendition.SourcePerformerIDs = lyricsDocumentRosterIDs(full, rendition.Game)
		rendition.FullPerformerEvidence = lyricsDocumentPerformerEvidence(full, rendition.SourcePerformerIDs)
		rendition.GamePerformerEvidence = lyricsDocumentPerformerEvidence(rendition.Game, rendition.SourcePerformerIDs)
		if err := model.ValidateLyricsSourceRenditionPayload(rendition); err != nil {
			addIssue(key, "", -1, "", err.Error())
			continue
		}
		identity := model.LyricsSourceFixedIdentity{
			Provider: source.provider, Origin: source.origin,
			// The revision URL carries no page ID; the revision ID is unique within
			// the wiki and stands in for it.
			PageID: source.revisionID, RevisionID: source.revisionID,
			SHA1: lyricsDocumentSHA1(source.canonicalURL), Title: source.title, CanonicalURL: source.canonicalURL,
			FetchedAt: fetchedAt, Categories: []string{}, Section: label, RenditionKey: identityKey,
			CompositionRenditionKey: key, VersionReason: reason,
			IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
				EvidenceID: fmt.Sprintf("revision:%s:%d", source.provider, source.revisionID),
				SHA256:     lyricsDocumentSHA256(source.canonicalURL),
			}},
		}
		if source.provider == model.LyricsSourceProviderSekaipedia {
			identity.RevisionTimestamp = fetchedAt
		}
		if err := model.ValidateLyricsSourceFixedIdentity(identity); err != nil {
			return lyricsDocumentDraft{}, &LyricsDocumentError{Code: LyricsDocumentErrorSourceRevision, Details: []string{err.Error()}}
		}
		document.Renditions = append(document.Renditions, rendition)
		document.FixedIdentities = append(document.FixedIdentities, identity)
		draft.identities[identityKey] = identity
		translation := lyricscontract.RenditionTranslation{RenditionKey: key}
		renditionTranslated := false
		if lyricsDocumentAnyText(fullTranslations) {
			translation.Translations = fullTranslations
			renditionTranslated = true
		}
		if lyricsDocumentAnyText(gameTranslations) {
			// A Game-only rendition keeps its translation in the primary lines.
			if full == nil {
				translation.Translations = gameTranslations
			} else {
				draft.sides[key] = map[string][]string{"game": gameTranslations}
			}
			renditionTranslated = true
		}
		if renditionTranslated {
			translation.TranslationCredit, translation.ProofreadingCredit = translationCredit, proofreadingCredit
			hasTranslation = true
		}
		if renditionCredits != nil {
			translation.TranslationCredit, translation.ProofreadingCredit = renditionCredits.Translation, renditionCredits.Proofreading
			// Stored credits need translation rows, which the editor also keeps
			// empty for an untranslated rendition.
			if !renditionTranslated && (translation.TranslationCredit != "" || translation.ProofreadingCredit != "") {
				translation.Translations = fullTranslations
				if full == nil {
					translation.Translations = gameTranslations
				}
			}
		}
		draft.translations = append(draft.translations, translation)
		draft.recordEditions(key, fullEditions, gameEditions, editionCredits)
	}
	if len(uncredited) > 0 && translationCredit == "" && proofreadingCredit == "" {
		addIssue("", "", -1, "translationCredit", fmt.Sprintf(
			"translationCredit or proofreadingCredit is required: rendition(s) %s have zh lines and no translationCredits of their own",
			strings.Join(uncredited, ", ")))
	} else if !credited && len(request.Renditions) > 0 {
		addIssue("", "", -1, "translationCredit",
			"at least one rendition needs a translation or proofreading credit to publish; the public site serves no song without one")
	}
	if len(issues) == 0 && !hasTranslation {
		addIssue("", "", -1, "zh", "at least one line needs a zh translation to publish")
	}
	if len(issues) > maxLyricsDocumentIssues {
		omitted := len(issues) - maxLyricsDocumentIssues
		issues = append(issues[:maxLyricsDocumentIssues:maxLyricsDocumentIssues], LyricsDocumentIssue{
			Field: "issues", Message: fmt.Sprintf("%d more issues are not listed; fix the listed ones and send the document again", omitted),
		})
	}
	if len(issues) > 0 {
		return lyricsDocumentDraft{}, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{"the lyrics document failed validation"}, Issues: issues}
	}
	sort.Slice(document.Renditions, func(i, j int) bool { return document.Renditions[i].RenditionKey < document.Renditions[j].RenditionKey })
	sort.Slice(document.FixedIdentities, func(i, j int) bool {
		return document.FixedIdentities[i].RenditionKey < document.FixedIdentities[j].RenditionKey
	})
	sort.Slice(draft.translations, func(i, j int) bool { return draft.translations[i].RenditionKey < draft.translations[j].RenditionKey })
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		return lyricsDocumentDraft{}, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{err.Error()}}
	}
	if err := validateStoreV3DocumentGraph(document); err != nil {
		return lyricsDocumentDraft{}, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{err.Error()}}
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return lyricsDocumentDraft{}, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{err.Error()}}
	}
	draft.document, draft.bindings = document, bindings
	draft.mirrorDefaultEdition()
	return draft, nil
}

const (
	maxLyricsSourceRenditionsForDocument = 16
	maxLyricsDocumentIssues              = 200
	// MaxLyricsDocumentJapaneseLineBytes is the source model's line text limit;
	// zh lines live in the localization rows and allow maxLyricsLineTextBytes.
	MaxLyricsDocumentJapaneseLineBytes = 8 << 10
	maxLyricsDocumentCreditBytes       = 2048
)

func validLyricsDocumentCredit(value string) bool {
	return len(value) <= maxLyricsDocumentCreditBytes && !strings.ContainsAny(value, "\r\n\x00")
}

func lyricsDocumentCreditProblem(field, value string) string {
	if len(value) > maxLyricsDocumentCreditBytes {
		return fmt.Sprintf("%s must be one line of at most %d bytes; it has %d bytes", field, maxLyricsDocumentCreditBytes, len(value))
	}
	return fmt.Sprintf("%s must be one line of at most %d bytes; it contains a line break or NUL at character %d",
		field, maxLyricsDocumentCreditBytes, lyricsDocumentLineBreakAt(value))
}

// lyricsDocumentZhProblem describes why value, the zh text named name, cannot
// be stored as one translation line, or is empty when it can.
func lyricsDocumentZhProblem(name, value string) string {
	rule := fmt.Sprintf("%s must be one line of at most %d bytes", name, maxLyricsLineTextBytes)
	switch {
	case len(value) > maxLyricsLineTextBytes:
		return fmt.Sprintf("%s; it has %d bytes", rule, len(value))
	case !utf8.ValidString(value):
		return rule + "; it is not valid UTF-8"
	case strings.ContainsAny(value, "\r\n\x00"):
		return fmt.Sprintf("%s; it contains a line break or NUL at character %d", rule, lyricsDocumentLineBreakAt(value))
	}
	return ""
}

// lyricsDocumentLineBreakAt is the character index of the first CR, LF or NUL.
func lyricsDocumentLineBreakAt(value string) int {
	index := 0
	for _, current := range value {
		if current == '\r' || current == '\n' || current == 0 {
			return index
		}
		index++
	}
	return -1
}

func buildLyricsDocumentSide(key string, side model.LyricsSourceRenditionSide, version model.LyricsSourceVersion, identityKey string,
	lines []LyricsDocumentLine, defaults []int, allowInGame bool,
) (*model.LyricsSourceFull, []string, []LyricsDocumentIssue) {
	var issues []LyricsDocumentIssue
	addIssue := func(line int, field, message string) {
		lineIndex := line
		issues = append(issues, LyricsDocumentIssue{Rendition: key, Side: string(side), Line: &lineIndex, Field: field, Message: message})
	}
	if len(lines) > maxLyricsLines {
		lineCount := len(lines)
		return nil, nil, []LyricsDocumentIssue{{Rendition: key, Side: string(side), Field: "lines",
			Message: fmt.Sprintf("%d lines exceed the limit of %d", lineCount, maxLyricsLines)}}
	}
	prefix := "full"
	if side == model.LyricsSourceRenditionSideGame {
		prefix = "game"
	}
	full := &model.LyricsSourceFull{Version: version, Performers: []model.LyricsSourcePerformer{}, Lines: make([]model.LyricsSourceFullLine, len(lines))}
	translations := make([]string, len(lines))
	performers := map[string]model.LyricsSourcePerformer{}
	// resolve keeps the given order; only the rendition roster is a sorted set.
	resolve := func(line int, field, context string, ids []int) []model.LyricsSourcePerformer {
		result := []model.LyricsSourcePerformer{}
		seen := map[string]bool{}
		for _, id := range ids {
			performer, ok := lyricsDocumentPerformer(id)
			if !ok {
				addIssue(line, field, fmt.Sprintf("%sperformer ID %d is not a known character or audited singer", context, id))
				continue
			}
			if !seen[performer.PerformerID] {
				seen[performer.PerformerID] = true
				result = append(result, performer)
			}
		}
		return result
	}
	for index, input := range lines {
		text, spans, problems := parseLyricsDocumentRuby(input.Japanese)
		for _, problem := range problems {
			addIssue(index, "ja", problem)
		}
		jaRule := fmt.Sprintf("ja must be one non-empty line of at most %d bytes", MaxLyricsDocumentJapaneseLineBytes)
		switch {
		case strings.TrimSpace(text) == "" && len(problems) == 0:
			addIssue(index, "ja", jaRule+"; it is empty")
		case len(text) > MaxLyricsDocumentJapaneseLineBytes:
			addIssue(index, "ja", fmt.Sprintf("%s; it has %d bytes", jaRule, len(text)))
		case strings.ContainsAny(input.Japanese, "\r\n\x00"):
			addIssue(index, "ja", fmt.Sprintf("%s; it contains a line break or NUL at character %d", jaRule, lyricsDocumentLineBreakAt(input.Japanese)))
		}
		if input.English != "" {
			addIssue(index, "en", "English lines are not stored for source-v3 documents; remove en")
		}
		if problem := lyricsDocumentZhProblem("zh", input.Chinese); problem != "" {
			addIssue(index, "zh", problem)
		}
		if input.InGame && !allowInGame {
			addIssue(index, "inGame", "inGame is only valid on Full lines when game is cut")
		}
		ids := input.PerformerIDs
		if ids == nil {
			ids = defaults
		}
		linePerformers := resolve(index, "performerIds", "", ids)
		parsed := []lyricsDocumentParsedSegment{{text: text, spans: spans}}
		segmentPerformers := [][]model.LyricsSourcePerformer{linePerformers}
		if input.Segments != nil {
			var segmentProblems []string
			parsed, segmentProblems = parseLyricsDocumentSegments(input.Japanese, input.Segments, len(problems) == 0)
			for _, problem := range segmentProblems {
				addIssue(index, "segments", problem)
			}
			segmentPerformers = make([][]model.LyricsSourcePerformer, len(input.Segments))
			for segmentIndex, segment := range input.Segments {
				segmentPerformers[segmentIndex] = linePerformers
				if segment.PerformerIDs != nil {
					segmentPerformers[segmentIndex] = resolve(index, "segments", fmt.Sprintf("segment %d: ", segmentIndex), segment.PerformerIDs)
				}
			}
		}
		if len(issues) > 0 {
			continue
		}
		segments := make([]model.LyricsSourceSegment, len(parsed))
		for segmentIndex, segment := range parsed {
			performerIDs := make([]string, len(segmentPerformers[segmentIndex]))
			for performerIndex, performer := range segmentPerformers[segmentIndex] {
				performers[performer.PerformerID] = performer
				performerIDs[performerIndex] = performer.PerformerID
			}
			for spanIndex := range segment.spans {
				if segment.spans[spanIndex].Reading != "" {
					segment.spans[spanIndex].ReadingEvidence = &model.LyricsSourceReadingEvidence{
						Kind: model.LyricsSourceReadingEvidenceExplicitSourceKana, FixedIdentityKey: identityKey,
						RenditionKey: key, Side: side, SourceRowOrdinal: index + 1, SourceSegmentOrdinal: segmentIndex + 1,
					}
					full.RubyGeneratorVersion = lyricsDocumentRubyGeneratorVersion
				}
			}
			segments[segmentIndex] = model.LyricsSourceSegment{Text: segment.text, PerformerIDs: performerIDs, Ruby: segment.spans}
		}
		full.Lines[index] = model.LyricsSourceFullLine{
			ID: fmt.Sprintf("%s-%06d", prefix, index+1), Text: text, StanzaBreakBefore: input.StanzaBreakBefore,
			Segments: segments, TrailingPerformerIDs: []string{},
		}
		translations[index] = input.Chinese
	}
	if len(issues) > 0 {
		return nil, nil, issues
	}
	for _, performer := range performers {
		full.Performers = append(full.Performers, performer)
	}
	sort.Slice(full.Performers, func(i, j int) bool { return full.Performers[i].PerformerID < full.Performers[j].PerformerID })
	return full, translations, nil
}

func lyricsDocumentDefaultLabel(kind model.LyricsSourceRenditionKind) string {
	switch kind {
	case model.LyricsSourceRenditionSekai:
		return "SEKAI Version"
	case model.LyricsSourceRenditionVocaloid:
		return "VIRTUAL SINGER Version"
	case model.LyricsSourceRenditionOriginal:
		return "Original Version"
	default:
		return ""
	}
}

// lyricsDocumentCatalogPerformers returns the game characters of the catalog
// vocals that match the rendition kind. Alternate renditions have no catalog
// default and must name their performers.
func lyricsDocumentCatalogPerformers(kind model.LyricsSourceRenditionKind, vocals []model.CatalogVocalSignal) []int {
	vocalType := ""
	switch kind {
	case model.LyricsSourceRenditionSekai:
		vocalType = "sekai"
	case model.LyricsSourceRenditionVocaloid:
		vocalType = "virtual_singer"
	case model.LyricsSourceRenditionOriginal:
		vocalType = "original_song"
	default:
		return []int{}
	}
	seen := map[int]bool{}
	result := []int{}
	for _, vocal := range vocals {
		if vocal.VocalType != vocalType || vocal.CharacterType != "game_character" || seen[vocal.CharacterID] {
			continue
		}
		if _, ok := lyricsDocumentPerformer(vocal.CharacterID); ok {
			seen[vocal.CharacterID] = true
			result = append(result, vocal.CharacterID)
		}
	}
	sort.Ints(result)
	return result
}

// lyricsDocumentPerformer maps a catalog character ID (1-26) or an audited
// external singer ID to its persisted performer identity.
func lyricsDocumentPerformer(id int) (model.LyricsSourcePerformer, bool) {
	if id >= 1 && id <= 26 {
		key := fmt.Sprintf("歌唱者-%02d", id)
		persisted, known, err := lyricscontract.NormalizeAuditedPerformerValues(key, key)
		if err != nil || !known || persisted.ID != key {
			return model.LyricsSourcePerformer{}, false
		}
		return model.LyricsSourcePerformer{PerformerID: persisted.ID, Name: persisted.Name}, true
	}
	for _, external := range lyricsperformers.All() {
		if external.NumericID == id {
			return model.LyricsSourcePerformer{PerformerID: external.SourceID, Name: external.Name, Color: external.Color}, true
		}
	}
	return model.LyricsSourcePerformer{}, false
}

func lyricsDocumentRosterIDs(full, game *model.LyricsSourceFull) []string {
	seen := map[string]bool{}
	var result []string
	for _, side := range []*model.LyricsSourceFull{full, game} {
		if side == nil {
			continue
		}
		for _, performer := range side.Performers {
			if !seen[performer.PerformerID] {
				seen[performer.PerformerID] = true
				result = append(result, performer.PerformerID)
			}
		}
	}
	sort.Strings(result)
	return result
}

func lyricsDocumentPerformerEvidence(side *model.LyricsSourceFull, roster []string) model.LyricsSourcePerformerEvidenceState {
	switch {
	case side == nil || !model.LyricsSourceFullHasPerformerSegmentation(*side):
		return model.LyricsSourcePerformerEvidenceNone
	case model.LyricsSourceFullHasCompletePerformerEvidence(*side, roster):
		return model.LyricsSourcePerformerEvidenceSourceComplete
	default:
		return model.LyricsSourcePerformerEvidenceSourcePartial
	}
}

func lyricsSourceFullHasRuby(full model.LyricsSourceFull) bool {
	for _, line := range full.Lines {
		for _, segment := range line.Segments {
			for _, span := range segment.Ruby {
				if span.Reading != "" {
					return true
				}
			}
		}
	}
	return false
}

func lyricsDocumentAnyText(values []string) bool {
	for _, value := range values {
		if value != "" {
			return true
		}
	}
	return false
}

func lyricsDocumentSHA1(value string) string {
	digest := sha1.Sum([]byte(value))
	return hex.EncodeToString(digest[:])
}

func lyricsDocumentSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (draft lyricsDocumentDraft) contributions() map[string]string {
	result := make(map[string]string, len(draft.bindings))
	for _, binding := range draft.bindings {
		result[binding.ComponentKey] = binding.FixedIdentityKey
	}
	return result
}

func (draft lyricsDocumentDraft) localization(revision int) lyricsRenditionLocalizationState {
	return lyricsRenditionLocalizationState{
		HasRows: true, Revision: revision, UpdatedAt: draft.now,
		Translations: draft.translations, SideTranslations: draft.sides,
	}
}

// publicDetail builds the v3 detail exactly as the localization projection
// will serve it once the draft is stored at revision.
func (draft lyricsDocumentDraft) publicDetail(revision int) (json.RawMessage, error) {
	localization := draft.localization(revision)
	bundle := lyricsRenditionEditorBundle{
		musicID: draft.musicID, document: draft.document, bindings: draft.bindings,
		identities: draft.identities, contributions: draft.contributions(), localization: localization,
	}
	editorDocument, err := buildLyricsRenditionEditorDocument(bundle, localization)
	if err != nil {
		return nil, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{err.Error()}}
	}
	body, err := encodeLyricsDocumentProjection(draft.musicID, editorDocument)
	if err != nil {
		return nil, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{err.Error()}}
	}
	return body, nil
}

// encodeLyricsDocumentProjection mirrors PublishedLyricsLocalizationProjection
// for one song.
func encodeLyricsDocumentProjection(musicID int, document LyricsRenditionDocument) ([]byte, error) {
	if document.Revision <= 1 || !lyricsRenditionHasLocalizationCredit(document) {
		return nil, errors.New("the localization projection serves only documents with revision > 1 and a translation credit")
	}
	state := PublicLyricsStateGameOnly
	for _, rendition := range document.Renditions {
		if rendition.Full != nil {
			state = PublicLyricsStateComplete
			break
		}
	}
	return EncodePublicLyricsV3Detail(PublicLyricsV3DetailDocument{
		Version: 3, MusicID: musicID, Revision: document.Revision, UpdatedAt: document.UpdatedAt,
		State: state, Renditions: document.Renditions,
	})
}

// replaceTx swaps every lyrics row of the song for the draft in the caller's
// transaction: the previous source document with its artifacts,
// contributions, localizations and editions, the legacy SongLyrics rows with
// their publication, and the withdrawal marker. Live collaboration rooms are
// fenced by the epoch bump; the caller reseeds them after commit.
func (draft lyricsDocumentDraft) replaceTx(ctx context.Context, tx *sql.Tx, revision int, user string) error {
	musicID := draft.musicID
	if _, err := tx.ExecContext(ctx, `UPDATE lyrics_collab_documents SET epoch=epoch+1, updated_at=? WHERE music_id=?`,
		draft.now, musicID); err != nil {
		return err
	}
	if err := suspendLyricsSourceDocumentDeleteGuardsTx(ctx, tx); err != nil {
		return err
	}
	for _, statement := range []string{
		// The edition state keys the editions ON DELETE RESTRICT and does not
		// cascade from its document, so it goes first.
		`DELETE FROM song_lyrics_translation_edition_state WHERE document_id IN
		 (SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?)`,
		`DELETE FROM song_lyrics_source_documents WHERE music_id=?`,
		`DELETE FROM song_lyrics WHERE music_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, musicID); err != nil {
			return err
		}
	}
	if err := restoreLyricsSourceDocumentDeleteGuardsTx(ctx, tx); err != nil {
		return err
	}
	if err := rerecordEmbeddedLyricsSeedOwnershipTx(ctx, tx, musicID); err != nil {
		return err
	}
	documentJSON, err := json.Marshal(draft.document)
	if err != nil {
		return err
	}
	documentSHA := lyricsDocumentSHA256(string(documentJSON))
	inserted, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?)`, musicID, model.LyricsSourceDocumentSchemaVersionV3, "", string(documentJSON),
		documentSHA, lyricsDocumentManifestBatchSHA256, draft.now)
	if err != nil {
		return err
	}
	documentID, err := inserted.LastInsertId()
	if err != nil {
		return err
	}
	for _, identity := range draft.document.FixedIdentities {
		identityJSON, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		evidenceJSON, err := json.Marshal(identity.IndexEvidenceRefs)
		if err != nil {
			return err
		}
		// No raw wikitext is retained for editor-published documents; the byte
		// count and digests follow the migration v32 synthetic artifact.
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_artifacts
			(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,
			 page_title,canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,
			 version_reason,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,
			 raw_wikitext_sha256,artifact_sha256)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, identity.Provider, identity.RenditionKey,
			identity.Origin, identity.PageID, identity.RevisionID, identity.RevisionTimestamp, identity.SHA1,
			identity.Title, identity.CanonicalURL, identity.FetchedAt, "[]", identity.Section,
			identity.CompositionRenditionKey, identity.VersionReason, string(evidenceJSON), string(identityJSON),
			lyricsDocumentSHA256(string(identityJSON)), 1, lyricsDocumentManifestBatchSHA256,
			lyricsDocumentManifestBatchSHA256); err != nil {
			return err
		}
	}
	for component, identityKey := range draft.contributions() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_component_contributions
			(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, documentID, component,
			identityKey, lyricsDocumentSHA256(documentSHA+"\x00"+component+"\x00"+identityKey)); err != nil {
			return err
		}
	}
	insertLocalizations := draft.insertLocalizationsTx
	if draft.editions != nil {
		insertLocalizations = draft.insertEditionsTx
	}
	if err := insertLocalizations(ctx, tx, documentID, revision, user); err != nil {
		return err
	}
	if _, err := clearPublicLyricsWithdrawalTx(tx, musicID); err != nil {
		return err
	}
	renditionKeys := make([]string, len(draft.document.Renditions))
	for index, rendition := range draft.document.Renditions {
		renditionKeys[index] = rendition.RenditionKey
	}
	source := draft.document.FixedIdentities[0].CanonicalURL
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.document.publish',?)`,
		draft.now, user, fmt.Sprintf("musicId=%d revision=%d renditions=%s source=%s",
			musicID, revision, strings.Join(renditionKeys, ","), source))
	return err
}

// rerecordEmbeddedLyricsSeedOwnershipTx marks the song's embedded seed ledger
// items as preserved_existing. Startup replay re-verifies inserted items
// against the seed's own document, which the replacement no longer matches;
// preserved items only require that the database still owns the song.
func rerecordEmbeddedLyricsSeedOwnershipTx(ctx context.Context, tx *sql.Tx, musicID int) error {
	var inserted int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedded_lyrics_editor_seed_items
		WHERE music_id=? AND apply_status='inserted'`, musicID).Scan(&inserted); err != nil {
		return err
	}
	if inserted == 0 {
		return nil
	}
	if err := suspendEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx, tx); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TEMP TABLE lyrics_document_seed_items AS SELECT * FROM embedded_lyrics_editor_seed_items
		 WHERE music_id=? AND apply_status='inserted'`,
		`DELETE FROM embedded_lyrics_editor_seed_items WHERE music_id=? AND apply_status='inserted'`,
	} {
		if _, err := tx.ExecContext(ctx, statement, musicID); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`UPDATE temp.lyrics_document_seed_items SET apply_status='preserved_existing'`,
		`INSERT INTO embedded_lyrics_editor_seed_items SELECT * FROM temp.lyrics_document_seed_items`,
		`DROP TABLE temp.lyrics_document_seed_items`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return restoreEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx, tx)
}
