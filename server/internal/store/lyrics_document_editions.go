package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"moesekai/server/internal/model"
)

// lyricsDocumentEdition is one translation edition of a draft: its label and,
// by rendition key, its credits and the zh lines of every editable side.
type lyricsDocumentEdition struct {
	key, label string
	renditions map[string]lyricsDocumentEditionTexts
}

// lyricsDocumentEditionTexts are one rendition's credits and zh lines in one
// edition. full and game hold the lines of the Full and Game sides; the Game
// lines of an exact projection repeat the Full lines it selects.
type lyricsDocumentEditionTexts struct {
	credits    LyricsDocumentCredits
	full, game []string
}

// lyricsDocumentEditionSet is a request's translationEditions list: sent when
// the request has one, declared when it names more than the implicit main
// edition, others the non-default keys.
type lyricsDocumentEditionSet struct {
	sent, declared bool
	list           []LyricsTranslationEditionSummary
	others         map[string]bool
}

// lyricsDocumentImplicitEditions reports a list that means the one implicit
// edition main labelled 默认译本, which is stored without edition rows.
func lyricsDocumentImplicitEditions(list []LyricsTranslationEditionSummary) bool {
	return len(list) == 0 || len(list) == 1 &&
		list[0] == LyricsTranslationEditionSummary{Key: MainLyricsTranslationEditionKey, Label: MainLyricsTranslationEditionLabel}
}

func compileLyricsDocumentEditions(input []LyricsTranslationEditionSummary) (lyricsDocumentEditionSet, []LyricsDocumentIssue) {
	set := lyricsDocumentEditionSet{sent: len(input) > 0, others: map[string]bool{}}
	var issues []LyricsDocumentIssue
	issue := func(edition, message string) {
		issues = append(issues, LyricsDocumentIssue{Field: "translationEditions", Edition: edition, Message: message})
	}
	if len(input) > maxLyricsTranslationEditions {
		issue("", fmt.Sprintf("translationEditions must contain 1 to %d entries; it has %d", maxLyricsTranslationEditions, len(input)))
	}
	seen := map[string]bool{}
	for index, edition := range input {
		label := strings.TrimSpace(edition.Label)
		switch {
		case !validLyricsTranslationEditionKey(edition.Key):
			issue(edition.Key, fmt.Sprintf("edition key %q must match ^[a-z0-9][a-z0-9._-]{0,127}$", edition.Key))
		case seen[edition.Key]:
			issue(edition.Key, "edition key is repeated")
		}
		if validateLyricsTranslationEditionLabel(label) != nil {
			issue(edition.Key, fmt.Sprintf("label must be 1 to 256 bytes of UTF-8 after trimming; it has %d bytes", len(label)))
		}
		seen[edition.Key] = true
		set.list = append(set.list, LyricsTranslationEditionSummary{Key: edition.Key, Label: label})
		if index > 0 {
			set.others[edition.Key] = true
		}
	}
	if set.sent && !seen[MainLyricsTranslationEditionKey] {
		issue(MainLyricsTranslationEditionKey, "translationEditions must include the edition main")
	}
	set.declared = !lyricsDocumentImplicitEditions(set.list)
	return set, issues
}

// undeclared explains why edition cannot key field, or is empty when it is a
// declared non-default edition.
func (set lyricsDocumentEditionSet) undeclared(edition, field, defaultRule string) string {
	switch {
	case !set.sent:
		return field + " needs translationEditions, which declares the song's translation editions"
	case set.others[edition]:
		return ""
	case edition == set.list[0].Key:
		return fmt.Sprintf("edition %q is the default edition (the first in translationEditions); %s", edition, defaultRule)
	default:
		return fmt.Sprintf("edition %q is not declared in translationEditions", edition)
	}
}

func (set lyricsDocumentEditionSet) draftEditions() []lyricsDocumentEdition {
	if !set.declared {
		return nil
	}
	result := make([]lyricsDocumentEdition, len(set.list))
	for index, edition := range set.list {
		result[index] = lyricsDocumentEdition{key: edition.Key, label: edition.Label, renditions: map[string]lyricsDocumentEditionTexts{}}
	}
	return result
}

// renditionCredits validates a rendition's editionCredits and returns them
// trimmed.
func (set lyricsDocumentEditionSet) renditionCredits(key string, input map[string]LyricsDocumentCredits) (map[string]LyricsDocumentCredits, []LyricsDocumentIssue) {
	var issues []LyricsDocumentIssue
	result := map[string]LyricsDocumentCredits{}
	for _, edition := range sortedLyricsDocumentKeys(input) {
		if problem := set.undeclared(edition, "editionCredits", "its credits are translationCredits or the document credits"); problem != "" {
			issues = append(issues, LyricsDocumentIssue{Rendition: key, Field: "editionCredits", Edition: edition, Message: problem})
			continue
		}
		credits := LyricsDocumentCredits{
			Translation:  strings.TrimSpace(input[edition].Translation),
			Proofreading: strings.TrimSpace(input[edition].Proofreading),
		}
		for _, credit := range []struct{ field, value string }{
			{"editionCredits.translation", credits.Translation}, {"editionCredits.proofreading", credits.Proofreading},
		} {
			if !validLyricsDocumentCredit(credit.value) {
				issues = append(issues, LyricsDocumentIssue{Rendition: key, Field: credit.field, Edition: edition,
					Message: lyricsDocumentCreditProblem(credit.field, credit.value)})
			}
		}
		result[edition] = credits
	}
	return result, issues
}

// lineTexts validates the zhEditions of one side's lines and returns, for
// every non-default edition, one zh line per request line.
func (set lyricsDocumentEditionSet) lineTexts(key, side string, lines []LyricsDocumentLine) (map[string][]string, []LyricsDocumentIssue) {
	var issues []LyricsDocumentIssue
	result := make(map[string][]string, len(set.others))
	for edition := range set.others {
		result[edition] = make([]string, len(lines))
	}
	for index, line := range lines {
		for _, edition := range sortedLyricsDocumentKeys(line.ChineseEditions) {
			lineIndex := index
			problem := set.undeclared(edition, "zhEditions", "its text is zh")
			if problem == "" {
				problem = lyricsDocumentZhProblem(fmt.Sprintf("zhEditions[%q]", edition), line.ChineseEditions[edition])
			}
			if problem != "" {
				issues = append(issues, LyricsDocumentIssue{Rendition: key, Side: side, Line: &lineIndex, Field: "zhEditions",
					Edition: edition, Message: problem})
				continue
			}
			result[edition][index] = line.ChineseEditions[edition]
		}
	}
	return result, issues
}

func sortedLyricsDocumentKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// recordEditions keeps a rendition's non-default edition texts; the default
// edition is taken from the draft translations by mirrorDefaultEdition.
func (draft *lyricsDocumentDraft) recordEditions(key string, full, game map[string][]string, credits map[string]LyricsDocumentCredits) {
	for index := 1; index < len(draft.editions); index++ {
		edition := draft.editions[index]
		edition.renditions[key] = lyricsDocumentEditionTexts{credits: credits[edition.key], full: full[edition.key], game: game[edition.key]}
	}
}

// mirrorDefaultEdition gives the default edition a zh line, empty or not, on
// every editable side, as the edition tables and their legacy mirror store
// it, and records it as the first edition.
func (draft *lyricsDocumentDraft) mirrorDefaultEdition() {
	if len(draft.editions) == 0 {
		return
	}
	for index := range draft.translations {
		translation := &draft.translations[index]
		key := translation.RenditionKey
		texts := lyricsDocumentEditionTexts{credits: LyricsDocumentCredits{
			Translation: translation.TranslationCredit, Proofreading: translation.ProofreadingCredit,
		}}
		primary := renditionPrimaryTranslationSide(draft.document, key)
		for _, side := range lyricsDocumentEditableSides(draft.document, key) {
			source, _ := translationEditionEditableSide(draft.document, key, side)
			lines := make([]string, len(source.Lines))
			if side == primary {
				copy(lines, translation.Translations)
				translation.Translations = lines
			} else {
				if draft.sides[key] == nil {
					draft.sides[key] = map[string][]string{}
				}
				copy(lines, draft.sides[key][side])
				draft.sides[key][side] = lines
			}
			if side == "full" {
				texts.full = lines
			} else {
				texts.game = lines
			}
		}
		draft.editions[0].renditions[key] = texts
	}
}

func lyricsDocumentEditableSides(document model.LyricsSourceDocument, key string) []string {
	for _, rendition := range document.Renditions {
		if rendition.RenditionKey == key {
			return translationEditionEditableSides(rendition)
		}
	}
	return nil
}

// insertLocalizationsTx stores the zh-CN localization of a document with the
// implicit main edition.
func (draft lyricsDocumentDraft) insertLocalizationsTx(ctx context.Context, tx *sql.Tx, documentID int64, revision int, user string) error {
	for _, translation := range draft.translations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
			VALUES (?,?,'zh-CN',?,?,?,?,?)`, documentID, translation.RenditionKey, translation.TranslationCredit,
			translation.ProofreadingCredit, draft.now, user, revision); err != nil {
			return err
		}
		for position, text := range translation.Translations {
			if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_translation_lines
				(document_id,rendition_key,locale,position,text) VALUES (?,?,'zh-CN',?,?)`,
				documentID, translation.RenditionKey, position, text); err != nil {
				return err
			}
		}
		for side, texts := range draft.sides[translation.RenditionKey] {
			for position, text := range texts {
				if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_side_translation_lines
					(document_id,rendition_key,side,locale,position,text) VALUES (?,?,?,'zh-CN',?,?)`,
					documentID, translation.RenditionKey, side, position, text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// insertEditionsTx stores the draft's translation editions as the console
// edition code does: each edition with its localization of every rendition
// and the zh lines of every editable side, the state naming the default
// edition at revision, and the legacy localization rows mirroring it.
func (draft lyricsDocumentDraft) insertEditionsTx(ctx context.Context, tx *sql.Tx, documentID int64, revision int, user string) error {
	bundle := lyricsRenditionEditorBundle{musicID: draft.musicID, documentID: documentID, document: draft.document}
	for _, edition := range draft.editions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_translation_editions
			(document_id,edition_key,label,created_at,created_by) VALUES (?,?,?,?,?)`,
			documentID, edition.key, edition.label, draft.now, user); err != nil {
			return err
		}
		if err := replaceMaterializedLyricsTranslationEditionTx(tx, bundle, edition.key, edition.renditionDocument(draft.document), user, draft.now); err != nil {
			return err
		}
	}
	defaultKey := draft.editions[0].key
	if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_translation_edition_state
		(document_id,default_edition_key,revision,updated_at,updated_by) VALUES (?,?,?,?,?)`,
		documentID, defaultKey, revision, draft.now, user); err != nil {
		return err
	}
	return rewriteLegacyLyricsTranslationMirrorTx(tx, bundle, defaultKey, revision, draft.now, user)
}

// renditionDocument is the edition in the shape
// replaceMaterializedLyricsTranslationEditionTx stores: the document's
// renditions in order with credits and the zh lines of the editable sides.
func (edition lyricsDocumentEdition) renditionDocument(document model.LyricsSourceDocument) LyricsRenditionDocument {
	result := LyricsRenditionDocument{Renditions: make([]PublicLyricsV3Rendition, len(document.Renditions))}
	for index, rendition := range document.Renditions {
		texts := edition.renditions[rendition.RenditionKey]
		credits := PublicLyricsV3TranslationCredits{Translation: texts.credits.Translation, Proofreading: texts.credits.Proofreading}
		result.Renditions[index] = PublicLyricsV3Rendition{Key: rendition.RenditionKey, TranslationCredits: &credits}
		for _, side := range translationEditionEditableSides(rendition) {
			source, _ := translationEditionEditableSide(document, rendition.RenditionKey, side)
			values := texts.full
			if side == "game" {
				values = texts.game
			}
			lines := make([]PublicLyricsV3Line, len(source.Lines))
			for position := range lines {
				if position < len(values) {
					lines[position].Chinese = values[position]
				}
			}
			if side == "full" {
				result.Renditions[index].Full = &PublicLyricsV3Side{Lines: lines}
			} else {
				result.Renditions[index].Game = &PublicLyricsV3Side{Lines: lines}
			}
		}
	}
	return result
}

// lyricsDocumentStoredDetail reads the published song back inside the publish
// transaction. Its rows must project to body, the v3 detail the draft was
// validated as. It returns the detail the site serves, v4 when the song has
// several translation editions, and the stored song as a request.
func (s *Store) lyricsDocumentStoredDetail(tx *sql.Tx, musicID int, body []byte) (json.RawMessage, LyricsDocumentRequest, error) {
	exporter := &lyricsDocumentExporter{}
	detail, found, err := s.lyricsDocumentDatabaseDetail(tx, exporter, musicID)
	if err != nil {
		return nil, LyricsDocumentRequest{}, err
	}
	if !found {
		return nil, LyricsDocumentRequest{}, errors.New("the published lyrics document is missing")
	}
	stored, err := EncodePublicLyricsV3Detail(detail)
	if err != nil {
		return nil, LyricsDocumentRequest{}, err
	}
	if !bytes.Equal(stored, body) {
		return nil, LyricsDocumentRequest{}, errors.New("published lyrics document differs from its validated public detail")
	}
	document := json.RawMessage(body)
	if exporter.editions != nil && len(exporter.editions.list) > 1 {
		v4, ok, err := s.buildProjectedV4Detail(tx, musicID, detail)
		if err != nil {
			return nil, LyricsDocumentRequest{}, fmt.Errorf("build the v4 detail of the published translation editions: %w", err)
		}
		if ok {
			if document, err = EncodePublicLyricsV4Detail(v4); err != nil {
				return nil, LyricsDocumentRequest{}, err
			}
		}
	}
	return document, exporter.request(detail), nil
}

// lyricsDocumentExportEditions are the translation editions of an exported
// song, the default first; the v3-shaped detail next to them carries the
// default edition, and texts the others by edition and rendition key.
type lyricsDocumentExportEditions struct {
	list  []LyricsTranslationEditionSummary
	texts map[string]map[string]lyricsDocumentEditionTexts
}

// loadLyricsDocumentExportEditions reads every translation edition of a
// source-v3 document whose default edition selection is loaded; the implicit
// main edition gives nil.
func loadLyricsDocumentExportEditions(q queryRower, bundle lyricsRenditionEditorBundle, selection lyricsTranslationEditionSelection) (*lyricsDocumentExportEditions, error) {
	if !selection.authoritative {
		return nil, nil
	}
	result := &lyricsDocumentExportEditions{texts: map[string]map[string]lyricsDocumentEditionTexts{}}
	for _, edition := range selection.editions {
		if edition.Key == selection.defaultKey {
			result.list = append([]LyricsTranslationEditionSummary{edition}, result.list...)
			continue
		}
		result.list = append(result.list, edition)
		other, err := loadLyricsTranslationEditionSelection(q, bundle, edition.Key, true)
		if err != nil {
			return nil, err
		}
		document, err := buildLyricsTranslationEditionDocument(bundle, other)
		if err != nil {
			return nil, err
		}
		texts := make(map[string]lyricsDocumentEditionTexts, len(document.Renditions))
		for _, rendition := range document.Renditions {
			item := lyricsDocumentEditionTexts{
				full: lyricsDocumentV3SideTranslations(rendition.Full), game: lyricsDocumentV3SideTranslations(rendition.Game),
			}
			if rendition.TranslationCredits != nil {
				item.credits = LyricsDocumentCredits{Translation: rendition.TranslationCredits.Translation, Proofreading: rendition.TranslationCredits.Proofreading}
			}
			texts[rendition.Key] = item
		}
		result.texts[edition.Key] = texts
	}
	return result, nil
}

func lyricsDocumentV3SideTranslations(side *PublicLyricsV3Side) []string {
	if side == nil {
		return nil
	}
	result := make([]string, len(side.Lines))
	for index, line := range side.Lines {
		result[index] = line.Chinese
	}
	return result
}

// lyricsDocumentV4ExportEditions reads the translation editions of a served v4
// detail.
func lyricsDocumentV4ExportEditions(detail PublicLyricsV4DetailDocument) *lyricsDocumentExportEditions {
	result := &lyricsDocumentExportEditions{texts: map[string]map[string]lyricsDocumentEditionTexts{}}
	for editionIndex := range detail.TranslationEditions {
		edition := &detail.TranslationEditions[editionIndex]
		summary := LyricsTranslationEditionSummary{Key: edition.Key, Label: edition.Label}
		if edition.Key == detail.DefaultTranslationEditionKey {
			result.list = append([]LyricsTranslationEditionSummary{summary}, result.list...)
			continue
		}
		result.list = append(result.list, summary)
		texts := make(map[string]lyricsDocumentEditionTexts, len(detail.Renditions))
		for _, source := range detail.Renditions {
			translations := lyricsDocumentV4EditionRendition(edition, source.Key)
			item := lyricsDocumentEditionTexts{}
			item.full, item.game = lyricsDocumentV4Translations(source, translations)
			if translations != nil && translations.TranslationCredits != nil {
				item.credits = LyricsDocumentCredits{
					Translation: translations.TranslationCredits.Translation, Proofreading: translations.TranslationCredits.Proofreading,
				}
			}
			texts[source.Key] = item
		}
		result.texts[edition.Key] = texts
	}
	return result
}

func lyricsDocumentV4EditionRendition(edition *PublicLyricsV4TranslationEdition, key string) *PublicLyricsV4EditionRendition {
	if edition == nil {
		return nil
	}
	for index := range edition.Renditions {
		if edition.Renditions[index].RenditionKey == key {
			return &edition.Renditions[index]
		}
	}
	return nil
}

// lyricsDocumentV4Translations returns an edition's zh lines of the Full and
// Game sides of source; an exact projection's Game lines repeat the Full lines
// it selects.
func lyricsDocumentV4Translations(source PublicLyricsV4Rendition, translations *PublicLyricsV4EditionRendition) (full, game []string) {
	if translations != nil {
		if translations.Full != nil {
			full = translations.Full.Translations
		}
		if translations.Game != nil {
			game = translations.Game.Translations
		}
	}
	if source.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection && source.Full != nil {
		byID := map[string]string{}
		for index, line := range source.Full.Lines {
			if index < len(full) {
				byID[line.ID] = full[index]
			}
		}
		game = make([]string, len(source.Relation.LineIDs))
		for index, lineID := range source.Relation.LineIDs {
			game[index] = byID[lineID]
		}
	}
	return full, game
}

// apply writes the non-default editions into request, whose zh lines and
// credits are the default edition's. The implicit main edition writes nothing.
func (editions *lyricsDocumentExportEditions) apply(request *LyricsDocumentRequest) {
	if editions == nil || lyricsDocumentImplicitEditions(editions.list) {
		return
	}
	request.TranslationEditions = append([]LyricsTranslationEditionSummary(nil), editions.list...)
	for index := range request.Renditions {
		rendition := &request.Renditions[index]
		for _, edition := range editions.list[1:] {
			texts := editions.texts[edition.Key][rendition.Key]
			if texts.credits != (LyricsDocumentCredits{}) {
				if rendition.EditionCredits == nil {
					rendition.EditionCredits = map[string]LyricsDocumentCredits{}
				}
				rendition.EditionCredits[edition.Key] = texts.credits
			}
			if rendition.Game == "only" {
				applyLyricsDocumentEditionLines(rendition.GameLines, edition.Key, texts.game)
				continue
			}
			applyLyricsDocumentEditionLines(rendition.Lines, edition.Key, texts.full)
			if rendition.Game == "independent" {
				applyLyricsDocumentEditionLines(rendition.GameLines, edition.Key, texts.game)
			}
		}
	}
}

func applyLyricsDocumentEditionLines(lines []LyricsDocumentLine, edition string, texts []string) {
	for index := range lines {
		if index >= len(texts) || texts[index] == "" {
			continue
		}
		if lines[index].ChineseEditions == nil {
			lines[index].ChineseEditions = map[string]string{}
		}
		lines[index].ChineseEditions[edition] = texts[index]
	}
}

// withStoredEditionList completes a request read from a served v3 detail with
// the song's stored translation-edition list. The projection serves v3 while
// main is the only edition, so a renamed main edition's label is only in the
// database; it is taken when the stored song, read as a request, is the served
// request apart from that list.
func (s *Store) withStoredEditionList(q queryRower, musicID int, request *LyricsDocumentRequest) error {
	if len(request.TranslationEditions) > 0 {
		return nil
	}
	var materialized bool
	if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM song_lyrics_translation_edition_state AS e
 JOIN song_lyrics_source_documents AS d ON d.document_id=e.document_id WHERE d.music_id=?)`, musicID).Scan(&materialized); err != nil {
		return err
	}
	if !materialized {
		return nil
	}
	exporter := &lyricsDocumentExporter{}
	detail, found, err := s.lyricsDocumentDatabaseDetail(q, exporter, musicID)
	if err != nil || !found {
		return err
	}
	stored := exporter.request(detail)
	if len(stored.TranslationEditions) != 1 || lyricsDocumentImplicitEditions(stored.TranslationEditions) {
		return nil
	}
	list := stored.TranslationEditions
	stored.TranslationEditions = nil
	if lyricsDocumentChangesBetween(LyricsDocumentChangesAgainstServed, request, stored).Changed {
		return nil
	}
	request.TranslationEditions = list
	return nil
}
