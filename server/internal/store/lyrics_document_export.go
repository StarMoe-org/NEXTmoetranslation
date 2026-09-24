package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"moesekai/server/internal/lyricsperformers"
	"moesekai/server/internal/model"
)

// LyricsDocumentExport is a song converted into the request PUT
// /api/editor/v1/lyrics/document accepts: the served public detail (From
// served) or the editable database state (From database). ServedVersion and
// ServedRevision describe the served detail and are 0 when nothing is served.
// Warnings name what the request format cannot carry, so a PUT of Document
// would change it, and what a PUT would replace.
type LyricsDocumentExport struct {
	MusicID        int
	From           string
	ServedVersion  int
	ServedRevision int
	Document       LyricsDocumentRequest
	Warnings       []LyricsDocumentExportWarning
}

const (
	LyricsDocumentExportFromServed   = "served"
	LyricsDocumentExportFromDatabase = "database"
)

// LyricsDocumentExportWarning locates one loss. Line is zero-based within the
// served side and absent for rendition- or document-level warnings.
type LyricsDocumentExportWarning struct {
	Code      string `json:"code"`
	Rendition string `json:"rendition,omitempty"`
	Side      string `json:"side,omitempty"`
	Line      *int   `json:"line,omitempty"`
	Message   string `json:"message"`
}

const (
	LyricsDocumentExportWarningServedRevision     = "served_revision_differs"
	LyricsDocumentExportWarningUnpublishedDraft   = "unpublished_draft_replaced"
	LyricsDocumentExportWarningSourceMissing      = "source_missing"
	LyricsDocumentExportWarningSourceUnsupported  = "source_unsupported"
	LyricsDocumentExportWarningSourceReencoded    = "source_url_reencoded"
	LyricsDocumentExportWarningSourceDiffers      = "source_differs"
	LyricsDocumentExportWarningRenditionInferred  = "rendition_inferred"
	LyricsDocumentExportWarningGameProjection     = "game_projection_not_ordered"
	LyricsDocumentExportWarningGameDiffers        = "game_projection_differs"
	LyricsDocumentExportWarningSideLabel          = "side_label_differs"
	LyricsDocumentExportWarningTrailingPerformers = "trailing_performers_dropped"
	LyricsDocumentExportWarningUnknownPerformer   = "unknown_performer"
	LyricsDocumentExportWarningRubyMissing        = "ruby_not_served"
	LyricsDocumentExportWarningRubyUnsupported    = "ruby_not_expressible"
	LyricsDocumentExportWarningEnglish            = "english_dropped"
	LyricsDocumentExportWarningWithdrawn          = "withdrawn_republished"
	LyricsDocumentExportWarningCreditMissing      = "credit_missing"
)

// ErrLyricsDocumentExportUnsupported reports a served detail whose shape the
// exporter does not read.
var ErrLyricsDocumentExportUnsupported = errors.New("the served lyrics detail has an unsupported version")

// ExportLyricsDocument converts the detail bytes served for musicID (public
// v1, v2, v3 or v4) into a whole-song document request. bundleRevision is the
// embedded bundle revision of the song, as PUT receives it; ExpectedRevision is
// the revision PUT compares against.
func (s *Store) ExportLyricsDocument(ctx context.Context, musicID int, served []byte, bundleRevision int) (LyricsDocumentExport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return LyricsDocumentExport{}, err
	}
	exporter := &lyricsDocumentExporter{}
	detail, version, err := exporter.servedDetail(musicID, served)
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	request := exporter.request(detail)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	defer tx.Rollback()
	if err := s.withStoredEditionList(tx, musicID, &request); err != nil {
		return LyricsDocumentExport{}, err
	}
	state, err := loadLyricsDocumentCurrentState(tx, musicID, bundleRevision)
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	state.served = true
	expected := state.conflictRevision()
	request.ExpectedRevision = &expected
	replaced, err := s.lyricsDocumentDraftDiffersFromServed(tx, musicID, state, detail.Revision, request)
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	if replaced {
		exporter.warn(LyricsDocumentExportWarningUnpublishedDraft, "", "", -1, fmt.Sprintf(
			"the database holds a legacy draft at revision %d whose lyrics differ from what the site serves; a PUT of this document deletes the draft (export it with from=database to edit the draft instead)",
			state.legacyRevision))
	}
	if current := state.effectiveRevision(); current != detail.Revision {
		exporter.warn(LyricsDocumentExportWarningServedRevision, "", "", -1, fmt.Sprintf(
			"the public site serves revision %d but the database is at revision %d: either the public projection has not rebuilt yet (export again later), or the source-v3 translation saved at that revision has no translation or proofreading credit on any rendition, which the projection never serves; a PUT of this document replaces the database state",
			detail.Revision, current))
	}
	return LyricsDocumentExport{
		MusicID: musicID, From: LyricsDocumentExportFromServed, ServedVersion: version, ServedRevision: detail.Revision,
		Document: request, Warnings: exporter.warnings,
	}, nil
}

// ExportLyricsDocumentFromDatabase converts the song's editable database state
// into a whole-song document request: its source-v3 document, withdrawn or
// not, with its translation editions, or else its legacy draft. With
// nothing editable stored it returns a not_found LyricsDocumentError whose
// Current names the revision PUT compares against.
func (s *Store) ExportLyricsDocumentFromDatabase(ctx context.Context, musicID int, served LyricsDocumentServed) (LyricsDocumentExport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	defer tx.Rollback()
	state, err := loadLyricsDocumentCurrentState(tx, musicID, served.BundleRevision)
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	state.served = served.Detail != nil
	expected := state.conflictRevision()
	exporter := &lyricsDocumentExporter{}
	detail, found, err := s.lyricsDocumentDatabaseDetail(tx, exporter, musicID)
	if err != nil {
		return LyricsDocumentExport{}, err
	}
	if !found {
		return LyricsDocumentExport{}, &LyricsDocumentError{
			Code:    LyricsDocumentErrorNotFound,
			Details: []string{fmt.Sprintf("the database holds no editable lyrics for musicId %d; a PUT that creates them must send expectedRevision %d", musicID, expected)},
			Current: map[string]int{"revision": expected},
		}
	}
	request := exporter.request(detail)
	request.ExpectedRevision = &expected
	var withdrawn bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM song_lyrics_public_withdrawals WHERE music_id=?)`, musicID).Scan(&withdrawn); err != nil {
		return LyricsDocumentExport{}, err
	}
	if withdrawn {
		exporter.warn(LyricsDocumentExportWarningWithdrawn, "", "", -1,
			"the song is withdrawn from the public site; a PUT of this document publishes it again")
	}
	export := LyricsDocumentExport{MusicID: musicID, From: LyricsDocumentExportFromDatabase, Document: request, Warnings: exporter.warnings}
	if served.Detail != nil {
		var envelope struct {
			Version  int `json:"version"`
			Revision int `json:"revision"`
		}
		if err := json.Unmarshal(served.Detail, &envelope); err != nil {
			return LyricsDocumentExport{}, fmt.Errorf("decode served lyrics detail: %w", err)
		}
		export.ServedVersion, export.ServedRevision = envelope.Version, envelope.Revision
	}
	return export, nil
}

// LyricsDocumentTakeover is the publish of a served song's export: the PUT
// result plus the export warnings.
type LyricsDocumentTakeover struct {
	LyricsDocumentResult
	Warnings []LyricsDocumentExportWarning `json:"warnings"`
}

// TakeOverLyricsDocument exports the detail served for musicID as GET does and
// publishes that request as PUT does, with expectedRevision, so a song served
// from the recovery ledger, a legacy publication or the bundle becomes an
// editor document. A song the site does not serve is not_found.
func (s *Store) TakeOverLyricsDocument(ctx context.Context, musicID int, expectedRevision *int, served LyricsDocumentServed, user string) (LyricsDocumentTakeover, error) {
	if musicID <= 0 {
		return LyricsDocumentTakeover{}, &LyricsDocumentError{Code: LyricsDocumentErrorInvalid, Details: []string{"musicId must be a positive integer"}}
	}
	if served.Detail == nil {
		return LyricsDocumentTakeover{}, &LyricsDocumentError{Code: LyricsDocumentErrorNotFound,
			Details: []string{"the public site serves no lyrics detail for this musicId, so there is nothing to take over"}}
	}
	export, err := s.ExportLyricsDocument(ctx, musicID, served.Detail, served.BundleRevision)
	if err != nil {
		return LyricsDocumentTakeover{}, err
	}
	request := export.Document
	request.ExpectedRevision, request.DryRun = expectedRevision, false
	result, err := s.PublishLyricsDocumentServed(ctx, request, served, user)
	if err != nil {
		return LyricsDocumentTakeover{}, err
	}
	warnings := export.Warnings
	if warnings == nil {
		warnings = []LyricsDocumentExportWarning{}
	}
	return LyricsDocumentTakeover{LyricsDocumentResult: result, Warnings: warnings}, nil
}

// servedDetail decodes served detail bytes of any public version into the v3
// shape the request conversion reads, and returns the served version.
func (e *lyricsDocumentExporter) servedDetail(musicID int, served []byte) (PublicLyricsV3DetailDocument, int, error) {
	var envelope struct {
		Version int `json:"version"`
		MusicID int `json:"musicId"`
	}
	if err := json.Unmarshal(served, &envelope); err != nil {
		return PublicLyricsV3DetailDocument{}, 0, fmt.Errorf("decode served lyrics detail: %w", err)
	}
	if envelope.MusicID != musicID {
		return PublicLyricsV3DetailDocument{}, 0, fmt.Errorf("served lyrics detail names music %d, not %d", envelope.MusicID, musicID)
	}
	switch envelope.Version {
	case 1, 2:
		var legacy PublicLyricsDetailDocument
		if err := json.Unmarshal(served, &legacy); err != nil {
			return PublicLyricsV3DetailDocument{}, 0, fmt.Errorf("decode served lyrics detail: %w", err)
		}
		return e.fromLegacyDetail(legacy), envelope.Version, nil
	case 3:
		decoded, err := DecodePublicLyricsV3Detail(served)
		return decoded, envelope.Version, err
	case 4:
		decoded, err := DecodePublicLyricsV4Detail(served)
		if err != nil {
			return PublicLyricsV3DetailDocument{}, 0, err
		}
		return e.fromV4Detail(decoded), envelope.Version, nil
	default:
		return PublicLyricsV3DetailDocument{}, 0, fmt.Errorf("%w: %d", ErrLyricsDocumentExportUnsupported, envelope.Version)
	}
}

// lyricsDocumentDraftDiffersFromServed reports a legacy draft whose lyrics are
// not what the site serves, whatever the revision order: the site serves the
// draft only when it serves the legacy publication of the draft's revision,
// and otherwise the two are compared as requests.
func (s *Store) lyricsDocumentDraftDiffersFromServed(q queryRower, musicID int, state lyricsDocumentCurrentState, servedRevision int, served LyricsDocumentRequest) (bool, error) {
	if state.legacyRevision == 0 ||
		state.publicationRevision == state.legacyRevision && servedRevision == state.publicationRevision {
		return false, nil
	}
	exporter := &lyricsDocumentExporter{}
	draft, found, err := s.lyricsDocumentDatabaseDetail(q, exporter, musicID)
	if err != nil || !found {
		return false, err
	}
	return lyricsDocumentChangesBetween(LyricsDocumentChangesAgainstServed, &served, exporter.request(draft)).Changed, nil
}

// lyricsDocumentDatabaseDetail reads the song's editable database state in
// the v3 shape: the source-v3 document with its default translation edition,
// the others going to the exporter, or else the legacy draft. found is false
// when neither exists.
func (s *Store) lyricsDocumentDatabaseDetail(q queryRower, exporter *lyricsDocumentExporter, musicID int) (PublicLyricsV3DetailDocument, bool, error) {
	bundle, err := loadLyricsRenditionEditorBundle(q, musicID)
	if err == nil {
		selection, err := loadLyricsTranslationEditionSelection(q, bundle, "", false)
		if err != nil {
			return PublicLyricsV3DetailDocument{}, false, err
		}
		document, err := buildLyricsTranslationEditionDocument(bundle, selection)
		if err != nil {
			return PublicLyricsV3DetailDocument{}, false, err
		}
		if exporter.editions, err = loadLyricsDocumentExportEditions(q, bundle, selection); err != nil {
			return PublicLyricsV3DetailDocument{}, false, err
		}
		state := PublicLyricsStateGameOnly
		for _, rendition := range document.Renditions {
			if rendition.Full != nil {
				state = PublicLyricsStateComplete
			}
		}
		return PublicLyricsV3DetailDocument{
			Version: 3, MusicID: musicID, Revision: document.Revision, UpdatedAt: document.UpdatedAt,
			State: state, Renditions: document.Renditions,
		}, true, nil
	}
	if !errors.Is(err, ErrLyricsNotFound) {
		return PublicLyricsV3DetailDocument{}, false, err
	}
	stored, err := s.loadLyrics(q, musicID)
	if errors.Is(err, ErrLyricsNotFound) {
		return PublicLyricsV3DetailDocument{}, false, nil
	} else if err != nil {
		return PublicLyricsV3DetailDocument{}, false, err
	}
	legacy, err := s.lyricsDocumentLegacyDraftDetail(q, stored.lyrics)
	if err != nil {
		return PublicLyricsV3DetailDocument{}, false, err
	}
	return exporter.fromLegacyDetail(legacy), true, nil
}

// lyricsDocumentLegacyDraftDetail reads a legacy draft as a legacy detail: the
// v2 detail over its source document, or else the draft's own lines with
// their ruby and credits.
func (s *Store) lyricsDocumentLegacyDraftDetail(q queryRower, lyrics model.SongLyrics) (PublicLyricsDetailDocument, error) {
	bundle, err := s.loadPublicLyricsSourceBundle(q, lyrics.MusicID)
	if err != nil {
		return PublicLyricsDetailDocument{}, err
	}
	if bundle != nil {
		catalogPerformers, err := loadCatalogPerformerAliases(q)
		if err != nil {
			return PublicLyricsDetailDocument{}, err
		}
		// A draft that no longer matches its source document reads from its
		// own lines below.
		if detail, err := buildPublicLyricsV2(lyrics, bundle, catalogPerformers); err == nil {
			return detail, nil
		}
	}
	public := publicLyricsV1(lyrics)
	detail := PublicLyricsDetailDocument{
		Version: 1, MusicID: lyrics.MusicID, Revision: lyrics.Revision, UpdatedAt: lyrics.UpdatedAt,
		Attribution: public.Attribution, TranslationCredits: publicLyricsTranslationCredits(lyrics),
		Attributions: publicLyricsV1Attributions(model.PublicSongLyrics{
			SourceURL: public.SourceURL, SourcePageID: public.SourcePageID, SourceRevisionID: public.SourceRevisionID,
			LicenseName: public.LicenseName, LicenseURL: public.LicenseURL,
		}),
		Lines: make([]PublicLyricsLine, len(lyrics.Lines)),
	}
	for index, line := range lyrics.Lines {
		detail.Lines[index] = PublicLyricsLine{
			ID: fmt.Sprintf("line-%d", index+1), Order: index, Japanese: line.Japanese, Chinese: line.Chinese,
			English: line.English, StanzaBreakBefore: line.StanzaBreakBefore, Segments: line.Segments,
		}
		for _, segment := range line.Segments {
			if len(segment.Ruby) > 0 {
				// The draft carries ruby, which a v1 detail cannot.
				detail.Version = 2
			}
		}
	}
	return detail, nil
}

// MarshalJSON keeps an explicit empty performerIds list, which PUT reads as
// "no performers" while an absent list falls back to the default.
func (export LyricsDocumentExport) MarshalJSON() ([]byte, error) {
	type line struct {
		LyricsDocumentLine
		PerformerIDs *[]int `json:"performerIds,omitempty"`
	}
	type rendition struct {
		LyricsDocumentRendition
		PerformerIDs *[]int `json:"performerIds,omitempty"`
		Lines        []line `json:"lines"`
		GameLines    []line `json:"gameLines,omitempty"`
	}
	type document struct {
		LyricsDocumentRequest
		Renditions []rendition `json:"renditions"`
	}
	explicit := func(ids []int) *[]int {
		if ids == nil {
			return nil
		}
		return &ids
	}
	lines := func(input []LyricsDocumentLine) []line {
		if input == nil {
			return nil
		}
		result := make([]line, len(input))
		for index, value := range input {
			result[index] = line{LyricsDocumentLine: value, PerformerIDs: explicit(value.PerformerIDs)}
		}
		return result
	}
	body := document{LyricsDocumentRequest: export.Document, Renditions: make([]rendition, len(export.Document.Renditions))}
	for index, value := range export.Document.Renditions {
		body.Renditions[index] = rendition{
			LyricsDocumentRendition: value, PerformerIDs: explicit(value.PerformerIDs),
			Lines: lines(value.Lines), GameLines: lines(value.GameLines),
		}
	}
	warnings := export.Warnings
	if warnings == nil {
		warnings = []LyricsDocumentExportWarning{}
	}
	return json.Marshal(struct {
		MusicID        int                           `json:"musicId"`
		From           string                        `json:"from"`
		ServedVersion  int                           `json:"servedVersion"`
		ServedRevision int                           `json:"servedRevision"`
		Document       document                      `json:"document"`
		Warnings       []LyricsDocumentExportWarning `json:"warnings"`
	}{export.MusicID, export.From, export.ServedVersion, export.ServedRevision, body, warnings})
}

type lyricsDocumentExporter struct {
	warnings []LyricsDocumentExportWarning
	// editions are the translation editions of the converted detail, nil for
	// the implicit main edition.
	editions *lyricsDocumentExportEditions
}

func (e *lyricsDocumentExporter) warn(code, rendition, side string, line int, message string) {
	warning := LyricsDocumentExportWarning{Code: code, Rendition: rendition, Side: side, Message: message}
	if line >= 0 {
		lineIndex := line
		warning.Line = &lineIndex
	}
	e.warnings = append(e.warnings, warning)
}

// fromLegacyDetail lifts a v1 or v2 detail, which has no renditions, into one
// v3 rendition so a single converter builds the request.
func (e *lyricsDocumentExporter) fromLegacyDetail(legacy PublicLyricsDetailDocument) PublicLyricsV3DetailDocument {
	// Virtual singers are catalog characters 21-26; any other performer, or
	// none at all, reads as the SEKAI rendition.
	virtualSingers, others := 0, 0
	for _, line := range legacy.Lines {
		for _, segment := range line.Segments {
			for _, id := range segment.PerformerIDs {
				if id >= 21 && id <= 26 {
					virtualSingers++
				} else {
					others++
				}
			}
		}
	}
	kind := model.LyricsSourceRenditionSekai
	if virtualSingers > 0 && others == 0 {
		kind = model.LyricsSourceRenditionVocaloid
	}
	key := string(kind)
	label := lyricsDocumentDefaultLabel(kind)
	e.warn(LyricsDocumentExportWarningRenditionInferred, key, "", -1, fmt.Sprintf(
		"the served v%d detail names no rendition; it is exported as %q labelled %q", legacy.Version, key, label))
	performers := map[string]model.LyricsSourcePerformer{}
	lines := make([]PublicLyricsV3Line, len(legacy.Lines))
	for index, source := range legacy.Lines {
		line := PublicLyricsV3Line{
			ID: source.ID, Order: index, Japanese: source.Japanese, Chinese: source.Chinese, English: source.English,
			StanzaBreakBefore: source.StanzaBreakBefore, Segments: make([]PublicLyricsV3Segment, len(source.Segments)),
			TrailingPerformerIDs: e.legacyPerformerIDs(key, index, source.TrailingPerformerIDs, performers),
		}
		for segmentIndex, segment := range source.Segments {
			ruby := make([]PublicLyricsV3RubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				ruby[rubyIndex] = PublicLyricsV3RubySpan{Text: span.Text, Reading: span.Reading}
			}
			if len(ruby) == 0 {
				ruby = []PublicLyricsV3RubySpan{{Text: segment.Text}}
			}
			line.Segments[segmentIndex] = PublicLyricsV3Segment{
				Text: segment.Text, PerformerIDs: e.legacyPerformerIDs(key, index, segment.PerformerIDs, performers), Ruby: ruby,
			}
		}
		lines[index] = line
	}
	if legacy.Version == 1 {
		for index, line := range lines {
			if publicV3TextContainsHan(line.Japanese) {
				e.warn(LyricsDocumentExportWarningRubyMissing, key, "full", index,
					"the served v1 detail carries no ruby; PUT requires a {kanji|reading} for every kanji of this line")
			}
		}
	}
	rendition := PublicLyricsV3Rendition{
		Key: key, Kind: kind, Label: label, Relation: PublicLyricsV3Relation{Kind: model.LyricsSourceRenditionRelationNone},
		SourceTabPaths: []model.LyricsSourceTabPath{{label}},
	}
	side := &PublicLyricsV3Side{Version: model.LyricsSourceVersion{Kind: string(kind), Label: label}, Lines: lines}
	if legacy.State == PublicLyricsStateGameOnly {
		rendition.Game = side
		rendition.AvailableVersions = []string{"game"}
	} else {
		rendition.Full = side
		rendition.AvailableVersions = []string{"full"}
		if legacy.GameProjection != nil {
			rendition.AvailableVersions = append(rendition.AvailableVersions, "game")
			rendition.Relation = PublicLyricsV3Relation{
				Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: key,
				LineIDs: append([]string(nil), legacy.GameProjection.LineIDs...),
			}
			rendition.Game = e.legacyProjectedSide(key, side, legacy.GameProjection.LineIDs)
		}
	}
	for _, performer := range performers {
		rendition.Performers = append(rendition.Performers, performer)
	}
	sort.Slice(rendition.Performers, func(i, j int) bool {
		return rendition.Performers[i].PerformerID < rendition.Performers[j].PerformerID
	})
	textComponent := "full_text"
	if rendition.Full == nil {
		textComponent = "game_text"
	}
	for index, attribution := range legacy.Attributions {
		// Legacy attributions name no component; only the first is known to
		// attribute the text.
		component := "renditions/" + key + "/" + textComponent
		if index > 0 {
			component = fmt.Sprintf("renditions/%s/attributions[%d]", key, index)
		}
		rendition.Provenance = append(rendition.Provenance, PublicLyricsV3ComponentAttribution{
			Component: component, Provider: attribution.Provider, Title: attribution.Title,
			RevisionID: attribution.RevisionID, RevisionURL: attribution.RevisionURL,
			LicenseName: attribution.LicenseName, LicenseURL: attribution.LicenseURL,
		})
	}
	switch {
	case legacy.TranslationCredits != nil:
		rendition.TranslationCredits = &PublicLyricsV3TranslationCredits{
			Translation: legacy.TranslationCredits.Translation, Proofreading: legacy.TranslationCredits.Proofreading,
		}
	case strings.TrimSpace(legacy.Attribution) != "":
		rendition.TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: strings.TrimSpace(legacy.Attribution)}
	}
	state := legacy.State
	if state == "" {
		state = PublicLyricsStateComplete
	}
	return PublicLyricsV3DetailDocument{
		Version: legacy.Version, MusicID: legacy.MusicID, Revision: legacy.Revision, UpdatedAt: legacy.UpdatedAt,
		State: state, Renditions: []PublicLyricsV3Rendition{rendition},
	}
}

func (e *lyricsDocumentExporter) legacyPerformerIDs(key string, line int, ids []int, roster map[string]model.LyricsSourcePerformer) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		performer, ok := lyricsDocumentPerformer(id)
		if !ok {
			e.warn(LyricsDocumentExportWarningUnknownPerformer, key, "full", line,
				fmt.Sprintf("performer ID %d is not a known character or audited singer and is dropped", id))
			continue
		}
		roster[performer.PerformerID] = performer
		result = append(result, performer.PerformerID)
	}
	return result
}

func (e *lyricsDocumentExporter) legacyProjectedSide(key string, full *PublicLyricsV3Side, lineIDs []string) *PublicLyricsV3Side {
	byID := make(map[string]PublicLyricsV3Line, len(full.Lines))
	for _, line := range full.Lines {
		byID[line.ID] = line
	}
	game := &PublicLyricsV3Side{Version: full.Version, Lines: make([]PublicLyricsV3Line, 0, len(lineIDs))}
	for _, lineID := range lineIDs {
		line, ok := byID[lineID]
		if !ok {
			e.warn(LyricsDocumentExportWarningGameProjection, key, "game", -1,
				fmt.Sprintf("the Game projection names unknown line %q", lineID))
			continue
		}
		line.Order = len(game.Lines)
		game.Lines = append(game.Lines, line)
	}
	return game
}

// fromV4Detail applies the default translation edition to the source
// renditions and keeps the other editions for the request.
func (e *lyricsDocumentExporter) fromV4Detail(detail PublicLyricsV4DetailDocument) PublicLyricsV3DetailDocument {
	var edition *PublicLyricsV4TranslationEdition
	for index := range detail.TranslationEditions {
		if detail.TranslationEditions[index].Key == detail.DefaultTranslationEditionKey {
			edition = &detail.TranslationEditions[index]
		}
	}
	e.editions = lyricsDocumentV4ExportEditions(detail)
	result := PublicLyricsV3DetailDocument{
		Version: detail.Version, MusicID: detail.MusicID, Revision: detail.Revision, UpdatedAt: detail.UpdatedAt,
		State: detail.State, Renditions: make([]PublicLyricsV3Rendition, len(detail.Renditions)),
	}
	for index, source := range detail.Renditions {
		translations := lyricsDocumentV4EditionRendition(edition, source.Key)
		rendition := PublicLyricsV3Rendition{
			Key: source.Key, Kind: source.Kind, Label: source.Label, AvailableVersions: source.AvailableVersions,
			Performers: source.Performers, Relation: source.Relation, SourceTabPaths: source.SourceTabPaths,
			Provenance: source.Provenance,
		}
		if translations != nil {
			rendition.TranslationCredits = translations.TranslationCredits
		}
		fullTranslations, gameTranslations := lyricsDocumentV4Translations(source, translations)
		rendition.Full = lyricsDocumentExportV4Side(source.Full, fullTranslations)
		rendition.Game = lyricsDocumentExportV4Side(source.Game, gameTranslations)
		result.Renditions[index] = rendition
	}
	return result
}

func lyricsDocumentExportV4Side(source *PublicLyricsV4Side, translations []string) *PublicLyricsV3Side {
	if source == nil {
		return nil
	}
	side := &PublicLyricsV3Side{Version: source.Version, Lines: make([]PublicLyricsV3Line, len(source.Lines))}
	for index, line := range source.Lines {
		side.Lines[index] = PublicLyricsV3Line{
			ID: line.ID, Order: line.Order, Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments: line.Segments, TrailingPerformerIDs: line.TrailingPerformerIDs,
		}
		if index < len(translations) {
			side.Lines[index].Chinese = translations[index]
		}
	}
	return side
}

// request converts a v3-shaped detail into the whole-song request.
func (e *lyricsDocumentExporter) request(detail PublicLyricsV3DetailDocument) LyricsDocumentRequest {
	request := LyricsDocumentRequest{MusicID: detail.MusicID, Renditions: []LyricsDocumentRendition{}}
	source := e.source(detail)
	request.Source = LyricsDocumentSource{URL: source.RevisionURL, Title: source.Title}
	translated := make([]bool, len(detail.Renditions))
	for index, rendition := range detail.Renditions {
		var input LyricsDocumentRendition
		input, translated[index] = e.rendition(rendition)
		request.Renditions = append(request.Renditions, input)
	}
	// The document credits are those of the first translated rendition, else
	// of the first credited one.
	var credits *PublicLyricsV3TranslationCredits
	for _, translatedOnly := range []bool{true, false} {
		for index, rendition := range detail.Renditions {
			if credits == nil && rendition.TranslationCredits != nil && (translated[index] || !translatedOnly) {
				credits = rendition.TranslationCredits
			}
		}
	}
	if credits != nil {
		request.TranslationCredit, request.ProofreadingCredit = credits.Translation, credits.Proofreading
	}
	// PUT gives the document credits to every rendition with a zh line and to
	// no other; a rendition served otherwise names its own.
	for index, rendition := range detail.Renditions {
		var expected *PublicLyricsV3TranslationCredits
		if translated[index] && credits != nil {
			expected = credits
		}
		if !sameLyricsDocumentExportCredits(expected, rendition.TranslationCredits) {
			own := &LyricsDocumentCredits{}
			if rendition.TranslationCredits != nil {
				own.Translation, own.Proofreading = rendition.TranslationCredits.Translation, rendition.TranslationCredits.Proofreading
			}
			request.Renditions[index].TranslationCredits = own
		}
	}
	credited := false
	for _, rendition := range request.Renditions {
		credited = credited || lyricsDocumentEffectiveCredits(request, rendition) != LyricsDocumentCredits{}
	}
	if !credited {
		e.warn(LyricsDocumentExportWarningCreditMissing, "", "", -1,
			"no rendition names a translation or proofreading credit; PUT requires one on at least one rendition, since the public site serves no song without one")
	}
	e.editions.apply(&request)
	return request
}

// source picks the Full text attribution of the first rendition (Game text
// for a Game-only rendition) and warns about every other attributed revision.
func (e *lyricsDocumentExporter) source(detail PublicLyricsV3DetailDocument) PublicLyricsV3ComponentAttribution {
	var chosen *PublicLyricsV3ComponentAttribution
	for _, suffix := range []string{"/full_text", "/game_text", ""} {
		for renditionIndex := range detail.Renditions {
			for index := range detail.Renditions[renditionIndex].Provenance {
				attribution := &detail.Renditions[renditionIndex].Provenance[index]
				if chosen == nil && strings.HasSuffix(attribution.Component, suffix) {
					chosen = attribution
				}
			}
		}
	}
	if chosen == nil {
		e.warn(LyricsDocumentExportWarningSourceMissing, "", "", -1,
			"the served detail attributes no source revision; PUT requires source.url")
		return PublicLyricsV3ComponentAttribution{}
	}
	result := *chosen
	if parsed, err := parseLyricsDocumentSourceURL(result.RevisionURL, result.Title); err != nil {
		e.warn(LyricsDocumentExportWarningSourceUnsupported, "", "", -1, fmt.Sprintf(
			"the %s source %s cannot be used by PUT: %v", result.Provider, result.RevisionURL, err))
	} else if parsed.provider != result.Provider || parsed.canonicalURL != result.RevisionURL {
		e.warn(LyricsDocumentExportWarningSourceReencoded, "", "", -1, fmt.Sprintf(
			"PUT attributes the %s source %s as %s %s", result.Provider, result.RevisionURL, parsed.provider, parsed.canonicalURL))
	}
	for _, rendition := range detail.Renditions {
		differing := map[string][]string{}
		var revisions []string
		for _, attribution := range rendition.Provenance {
			if attribution.Provider == result.Provider && attribution.RevisionID == result.RevisionID &&
				attribution.RevisionURL == result.RevisionURL && attribution.Title == result.Title {
				continue
			}
			revision := fmt.Sprintf("%s %s", attribution.Provider, attribution.RevisionURL)
			if differing[revision] == nil {
				revisions = append(revisions, revision)
			}
			differing[revision] = append(differing[revision], strings.TrimPrefix(attribution.Component, "renditions/"+rendition.Key+"/"))
		}
		for _, revision := range revisions {
			e.warn(LyricsDocumentExportWarningSourceDiffers, rendition.Key, "", -1, fmt.Sprintf(
				"%s attributed to %s cannot be carried; PUT attributes the whole song to %s",
				strings.Join(differing[revision], ", "), revision, result.RevisionURL))
		}
	}
	return result
}

// rendition converts one rendition and reports whether PUT will see a zh line
// in it.
func (e *lyricsDocumentExporter) rendition(rendition PublicLyricsV3Rendition) (LyricsDocumentRendition, bool) {
	input := LyricsDocumentRendition{Key: rendition.Key, Kind: string(rendition.Kind), Label: rendition.Label}
	full, game := rendition.Full, rendition.Game
	if full != nil && game != nil && game.Version.Label != full.Version.Label {
		e.warn(LyricsDocumentExportWarningSideLabel, rendition.Key, "game", -1, fmt.Sprintf(
			"the Game label %q cannot be carried; PUT labels both sides %q, and the main site shows only the rendition label", game.Version.Label, full.Version.Label))
	}
	var fullLines, gameLines []lyricsDocumentExportLine
	switch {
	case full == nil && game != nil:
		gameLines = e.lines(rendition.Key, "game", game.Lines)
		input.Game = "only"
	case game == nil:
		fullLines = e.lines(rendition.Key, "full", full.Lines)
		input.Game = "none"
	case rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection:
		fullLines = e.lines(rendition.Key, "full", full.Lines)
		input.Game = e.projection(rendition, fullLines)
		if input.Game == "independent" {
			gameLines = e.lines(rendition.Key, "game", game.Lines)
		}
	default:
		fullLines = e.lines(rendition.Key, "full", full.Lines)
		gameLines = e.lines(rendition.Key, "game", game.Lines)
		input.Game = "independent"
	}
	defaults := lyricsDocumentExportDefaultPerformers(fullLines)
	if full == nil {
		defaults = lyricsDocumentExportDefaultPerformers(gameLines)
	}
	input.PerformerIDs = defaults
	translated := false
	convert := func(lines []lyricsDocumentExportLine) []LyricsDocumentLine {
		if lines == nil {
			return nil
		}
		result := make([]LyricsDocumentLine, len(lines))
		for index, line := range lines {
			result[index] = line.line
			if line.line.Segments == nil && !sameLyricsDocumentExportIDs(line.performers, defaults) {
				result[index].PerformerIDs = line.performers
			}
			translated = translated || line.line.Chinese != ""
		}
		return result
	}
	input.Lines = convert(fullLines)
	if input.Lines == nil {
		input.Lines = []LyricsDocumentLine{}
	}
	input.GameLines = convert(gameLines)
	return input, translated
}

// projection maps an exact projection onto same or cut when it selects Full
// lines in order without repeats and every Game line repeats its Full line,
// and onto independent otherwise.
func (e *lyricsDocumentExporter) projection(rendition PublicLyricsV3Rendition, fullLines []lyricsDocumentExportLine) string {
	positions := make(map[string]int, len(rendition.Full.Lines))
	for index, line := range rendition.Full.Lines {
		positions[line.ID] = index
	}
	selected := make([]int, 0, len(rendition.Relation.LineIDs))
	ordered := rendition.Relation.FullRenditionKey == rendition.Key && len(rendition.Relation.LineIDs) == len(rendition.Game.Lines)
	for _, lineID := range rendition.Relation.LineIDs {
		position, ok := positions[lineID]
		if !ok || len(selected) > 0 && position <= selected[len(selected)-1] {
			ordered = false
			break
		}
		selected = append(selected, position)
	}
	if !ordered || len(selected) == 0 {
		e.warn(LyricsDocumentExportWarningGameProjection, rendition.Key, "game", -1,
			"the Game projection does not select Full lines in order; it is exported as independent gameLines")
		return "independent"
	}
	for index, position := range selected {
		if difference := lyricsDocumentExportProjectionDifference(rendition.Full.Lines[position], rendition.Game.Lines[index]); difference != "" {
			e.warn(LyricsDocumentExportWarningGameDiffers, rendition.Key, "game", index, fmt.Sprintf(
				"the Game line differs from the Full line it projects in %s; same and cut repeat the Full line, so the Game side is exported as independent gameLines and PUT publishes it without the exact projection",
				difference))
			return "independent"
		}
	}
	if len(selected) == len(rendition.Full.Lines) {
		return "same"
	}
	for _, position := range selected {
		fullLines[position].line.InGame = true
	}
	return "cut"
}

// lyricsDocumentExportProjectionDifference names the first field in which a
// projected Game line differs from its Full line.
func lyricsDocumentExportProjectionDifference(full, game PublicLyricsV3Line) string {
	switch {
	case full.Japanese != game.Japanese:
		return "ja"
	case full.Chinese != game.Chinese:
		return "zh"
	case full.English != game.English:
		return "en"
	case full.StanzaBreakBefore != game.StanzaBreakBefore:
		return "stanzaBreakBefore"
	case !reflect.DeepEqual(lyricsDocumentExportSegments(full.Segments), lyricsDocumentExportSegments(game.Segments)):
		return "segments"
	case strings.Join(full.TrailingPerformerIDs, "\x00") != strings.Join(game.TrailingPerformerIDs, "\x00"):
		return "trailingPerformerIds"
	}
	return ""
}

func lyricsDocumentExportSegments(segments []PublicLyricsV3Segment) []PublicLyricsV3Segment {
	result := make([]PublicLyricsV3Segment, len(segments))
	for index, segment := range segments {
		result[index] = PublicLyricsV3Segment{
			Text: segment.Text, PerformerIDs: append([]string{}, segment.PerformerIDs...),
			Ruby: append([]PublicLyricsV3RubySpan{}, segment.Ruby...),
		}
	}
	return result
}

// lyricsDocumentExportLine is one converted line; performers is the single
// performer list of an unsegmented line and nil for a segmented one.
type lyricsDocumentExportLine struct {
	line       LyricsDocumentLine
	performers []int
}

// lines converts served lines, joining adjacent segments sung by the same
// single performer or by nobody, which render as one run; the main site draws
// every segment with several performers as its own gradient span, so those
// keep their boundaries. A line left with several segments carries them as
// request segments.
func (e *lyricsDocumentExporter) lines(key, side string, lines []PublicLyricsV3Line) []lyricsDocumentExportLine {
	result := make([]lyricsDocumentExportLine, len(lines))
	for index, line := range lines {
		var segments []LyricsDocumentSegment
		for _, served := range line.Segments {
			ids := e.performerIDs(key, side, index, served.PerformerIDs)
			var markup strings.Builder
			for _, span := range served.Ruby {
				if span.Reading == "" {
					markup.WriteString(escapeLyricsDocumentMarkup(span.Text))
					continue
				}
				if !publicV3ReadingBaseSpan(span.Text) || !lyricsDocumentKanaReading(span.Reading) {
					e.warn(LyricsDocumentExportWarningRubyUnsupported, key, side, index, fmt.Sprintf(
						"ruby %q over %q is not a kanji base with a kana reading, which the {kanji|reading} markup requires", span.Reading, span.Text))
				}
				markup.WriteString("{" + span.Text + "|" + span.Reading + "}")
			}
			if last := len(segments) - 1; last >= 0 && len(ids) <= 1 && sameLyricsDocumentExportIDs(segments[last].PerformerIDs, ids) {
				segments[last].Japanese += markup.String()
				continue
			}
			segments = append(segments, LyricsDocumentSegment{Japanese: markup.String(), PerformerIDs: ids})
		}
		if len(line.TrailingPerformerIDs) > 0 {
			e.warn(LyricsDocumentExportWarningTrailingPerformers, key, side, index,
				"trailing performers cannot be carried")
		}
		if line.English != "" {
			e.warn(LyricsDocumentExportWarningEnglish, key, side, index, "PUT does not store en lines")
		}
		converted := LyricsDocumentLine{Chinese: line.Chinese, StanzaBreakBefore: line.StanzaBreakBefore}
		var performers []int
		switch len(segments) {
		case 0:
			performers = []int{}
		case 1:
			converted.Japanese, performers = segments[0].Japanese, segments[0].PerformerIDs
		default:
			for _, segment := range segments {
				converted.Japanese += segment.Japanese
			}
			converted.Segments = segments
		}
		result[index] = lyricsDocumentExportLine{line: converted, performers: performers}
	}
	return result
}

// performerIDs keeps the served order, which PUT stores as given.
func (e *lyricsDocumentExporter) performerIDs(key, side string, line int, ids []string) []int {
	result := []int{}
	for _, id := range ids {
		numeric, ok := lyricsDocumentExportPerformerID(id)
		if !ok {
			e.warn(LyricsDocumentExportWarningUnknownPerformer, key, side, line,
				fmt.Sprintf("performer %q has no numeric catalog or audited singer ID and is dropped", id))
			continue
		}
		result = append(result, numeric)
	}
	return result
}

// lyricsDocumentExportPerformerID inverts lyricsDocumentPerformer.
func lyricsDocumentExportPerformerID(performerID string) (int, bool) {
	if digits, ok := strings.CutPrefix(performerID, "歌唱者-"); ok {
		id, err := strconv.Atoi(digits)
		if err == nil {
			if performer, known := lyricsDocumentPerformer(id); known && performer.PerformerID == performerID {
				return id, true
			}
		}
	}
	for _, external := range lyricsperformers.All() {
		if external.SourceID == performerID {
			return external.NumericID, true
		}
	}
	return 0, false
}

// lyricsDocumentExportDefaultPerformers is the most common performer list of
// the unsegmented lines; the earliest wins a tie.
func lyricsDocumentExportDefaultPerformers(lines []lyricsDocumentExportLine) []int {
	counts := map[string]int{}
	var best []int
	bestCount := 0
	for _, line := range lines {
		if line.performers == nil {
			continue
		}
		key := fmt.Sprint(line.performers)
		counts[key]++
		if counts[key] > bestCount {
			best, bestCount = line.performers, counts[key]
		}
	}
	if best == nil {
		return []int{}
	}
	return append([]int{}, best...)
}

func sameLyricsDocumentExportIDs(left, right []int) bool {
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

func sameLyricsDocumentExportCredits(left, right *PublicLyricsV3TranslationCredits) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
