package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

const (
	MainLyricsTranslationEditionKey   = "main"
	MainLyricsTranslationEditionLabel = "默认译本"
	maxLyricsTranslationEditions      = 16
)

var translationEditionKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

var ErrLyricsTranslationEditionNotFound = errors.New("lyrics translation edition not found")

type LyricsTranslationEditionSummary struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type LyricsTranslationEditionMutation struct {
	MusicID          int    `json:"musicId"`
	Revision         int    `json:"revision"`
	Operation        string `json:"operation"`
	SourceEditionKey string `json:"sourceEditionKey,omitempty"`
	EditionKey       string `json:"editionKey,omitempty"`
	Label            string `json:"label,omitempty"`
}

type lyricsTranslationEditionSelection struct {
	authoritative bool
	key           string
	defaultKey    string
	editions      []LyricsTranslationEditionSummary
	localization  lyricsRenditionLocalizationState
}

func cloneLyricsTranslationEditionSummaries(input []LyricsTranslationEditionSummary) []LyricsTranslationEditionSummary {
	if input == nil {
		return nil
	}
	return append([]LyricsTranslationEditionSummary(nil), input...)
}

func validLyricsTranslationEditionKey(value string) bool {
	return translationEditionKeyPattern.MatchString(value)
}

func validateLyricsTranslationEditionLabel(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"translation edition label must be trim-stable UTF-8 between 1 and 256 bytes"}}
	}
	return nil
}

func loadLyricsTranslationEditionSelection(q queryRower, bundle lyricsRenditionEditorBundle, requestedKey string, explicit bool) (lyricsTranslationEditionSelection, error) {
	if explicit && !validLyricsTranslationEditionKey(requestedKey) {
		return lyricsTranslationEditionSelection{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"translationEditionKey is invalid"}}
	}
	var defaultKey string
	var revision int
	var updatedAt int64
	err := q.QueryRow(`SELECT default_edition_key,revision,updated_at
		FROM song_lyrics_translation_edition_state WHERE document_id=?`, bundle.documentID).Scan(&defaultKey, &revision, &updatedAt)
	if err == sql.ErrNoRows {
		key := MainLyricsTranslationEditionKey
		if explicit {
			key = requestedKey
		}
		if key != MainLyricsTranslationEditionKey {
			return lyricsTranslationEditionSelection{}, ErrLyricsTranslationEditionNotFound
		}
		return lyricsTranslationEditionSelection{
			key: MainLyricsTranslationEditionKey, defaultKey: MainLyricsTranslationEditionKey,
			editions:     []LyricsTranslationEditionSummary{{Key: MainLyricsTranslationEditionKey, Label: MainLyricsTranslationEditionLabel}},
			localization: bundle.localization,
		}, nil
	}
	if err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	if !validLyricsTranslationEditionKey(defaultKey) || revision <= 0 || updatedAt <= 0 {
		return lyricsTranslationEditionSelection{}, errors.New("source v3 translation edition state is invalid")
	}
	rows, err := q.Query(`SELECT edition_key,label FROM song_lyrics_translation_editions
		WHERE document_id=? ORDER BY edition_key`, bundle.documentID)
	if err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	var editions []LyricsTranslationEditionSummary
	for rows.Next() {
		var item LyricsTranslationEditionSummary
		if err := rows.Scan(&item.Key, &item.Label); err != nil {
			rows.Close()
			return lyricsTranslationEditionSelection{}, err
		}
		if !validLyricsTranslationEditionKey(item.Key) || validateLyricsTranslationEditionLabel(item.Label) != nil {
			rows.Close()
			return lyricsTranslationEditionSelection{}, errors.New("source v3 translation edition metadata is invalid")
		}
		editions = append(editions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return lyricsTranslationEditionSelection{}, err
	}
	if err := rows.Close(); err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	if len(editions) == 0 || len(editions) > maxLyricsTranslationEditions {
		return lyricsTranslationEditionSelection{}, errors.New("source v3 translation edition set is invalid")
	}
	key := defaultKey
	if explicit {
		key = requestedKey
	}
	foundDefault, foundRequested := false, false
	for _, edition := range editions {
		foundDefault = foundDefault || edition.Key == defaultKey
		foundRequested = foundRequested || edition.Key == key
	}
	if !foundDefault {
		return lyricsTranslationEditionSelection{}, errors.New("source v3 default translation edition is missing")
	}
	if !foundRequested {
		return lyricsTranslationEditionSelection{}, ErrLyricsTranslationEditionNotFound
	}
	localization, err := loadMaterializedLyricsTranslationEdition(q, bundle.documentID, key, bundle.document, revision, updatedAt)
	if err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	defaultLocalization := localization
	if key != defaultKey {
		defaultLocalization, err = loadMaterializedLyricsTranslationEdition(q, bundle.documentID, defaultKey, bundle.document, revision, updatedAt)
		if err != nil {
			return lyricsTranslationEditionSelection{}, err
		}
	}
	legacyMirror, err := loadLyricsRenditionLocalizationState(q, bundle.documentID, bundle.document)
	if err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	if !lyricsTranslationEditionMirrorMatches(legacyMirror, defaultLocalization) {
		return lyricsTranslationEditionSelection{}, errors.New("source v3 default translation edition mirror is stale")
	}
	return lyricsTranslationEditionSelection{
		authoritative: true, key: key, defaultKey: defaultKey,
		editions: editions, localization: localization,
	}, nil
}

func lyricsTranslationEditionMirrorMatches(legacy, authoritative lyricsRenditionLocalizationState) bool {
	return legacy.HasRows && authoritative.HasRows && legacy.Revision == authoritative.Revision && legacy.UpdatedAt == authoritative.UpdatedAt &&
		reflect.DeepEqual(legacy.Translations, authoritative.Translations) &&
		reflect.DeepEqual(legacy.SideTranslations, authoritative.SideTranslations)
}

func loadMaterializedLyricsTranslationEdition(q queryRower, documentID int64, editionKey string, document model.LyricsSourceDocument, revision int, updatedAt int64) (lyricsRenditionLocalizationState, error) {
	rows, err := q.Query(`SELECT rendition_key,locale,translation_credit,proofreading_credit
		FROM song_lyrics_translation_edition_localizations
		WHERE document_id=? AND edition_key=? ORDER BY rendition_key,locale`, documentID, editionKey)
	if err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	parents := make(map[string]lyricscontract.RenditionTranslation, len(document.Renditions))
	for rows.Next() {
		var item lyricscontract.RenditionTranslation
		var locale string
		if err := rows.Scan(&item.RenditionKey, &locale, &item.TranslationCredit, &item.ProofreadingCredit); err != nil {
			rows.Close()
			return lyricsRenditionLocalizationState{}, err
		}
		if locale != "zh-CN" || item.RenditionKey == "" || item.TranslationCredit != strings.TrimSpace(item.TranslationCredit) ||
			item.ProofreadingCredit != strings.TrimSpace(item.ProofreadingCredit) || len(item.TranslationCredit) > 2048 ||
			len(item.ProofreadingCredit) > 2048 || !utf8.ValidString(item.TranslationCredit) || !utf8.ValidString(item.ProofreadingCredit) {
			rows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 translation edition localization is invalid")
		}
		if _, duplicate := parents[item.RenditionKey]; duplicate {
			rows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 translation edition localization is duplicated")
		}
		parents[item.RenditionKey] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return lyricsRenditionLocalizationState{}, err
	}
	if err := rows.Close(); err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	if len(parents) != len(document.Renditions) {
		return lyricsRenditionLocalizationState{}, errors.New("source v3 translation edition localizations do not cover every rendition")
	}
	lineRows, err := q.Query(`SELECT rendition_key,side,locale,position,text
		FROM song_lyrics_translation_edition_lines
		WHERE document_id=? AND edition_key=? ORDER BY rendition_key,side,locale,position`, documentID, editionKey)
	if err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	lines := map[string]map[string][]string{}
	for lineRows.Next() {
		var renditionKey, side, locale, text string
		var position int
		if err := lineRows.Scan(&renditionKey, &side, &locale, &position, &text); err != nil {
			lineRows.Close()
			return lyricsRenditionLocalizationState{}, err
		}
		source, editable := translationEditionEditableSide(document, renditionKey, side)
		if !editable || locale != "zh-CN" || position < 0 || !utf8.ValidString(text) || len(text) > maxLyricsLineTextBytes {
			lineRows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 translation edition line is invalid")
		}
		if lines[renditionKey] == nil {
			lines[renditionKey] = map[string][]string{}
		}
		if position != len(lines[renditionKey][side]) || len(lines[renditionKey][side]) >= len(source.Lines) {
			lineRows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 translation edition lines are incomplete")
		}
		lines[renditionKey][side] = append(lines[renditionKey][side], text)
	}
	if err := lineRows.Err(); err != nil {
		lineRows.Close()
		return lyricsRenditionLocalizationState{}, err
	}
	if err := lineRows.Close(); err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	state := lyricsRenditionLocalizationState{
		HasRows: true, Revision: revision, UpdatedAt: updatedAt,
		Translations:     make([]lyricscontract.RenditionTranslation, 0, len(document.Renditions)),
		SideTranslations: map[string]map[string][]string{},
	}
	for _, rendition := range document.Renditions {
		item, found := parents[rendition.RenditionKey]
		if !found {
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 translation edition localization %q is missing", rendition.RenditionKey)
		}
		for _, side := range translationEditionEditableSides(rendition) {
			source, _ := translationEditionEditableSide(document, rendition.RenditionKey, side)
			values := lines[rendition.RenditionKey][side]
			if len(values) != len(source.Lines) {
				return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 translation edition %q/%s is incomplete", rendition.RenditionKey, side)
			}
			if side == renditionPrimaryTranslationSide(document, rendition.RenditionKey) {
				item.Translations = append([]string(nil), values...)
			} else {
				if state.SideTranslations[rendition.RenditionKey] == nil {
					state.SideTranslations[rendition.RenditionKey] = map[string][]string{}
				}
				state.SideTranslations[rendition.RenditionKey][side] = append([]string(nil), values...)
			}
		}
		state.Translations = append(state.Translations, item)
	}
	return state, nil
}

func translationEditionEditableSides(rendition model.LyricsSourceRendition) []string {
	var result []string
	if rendition.Full != nil {
		result = append(result, "full")
	}
	if rendition.Game != nil && rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection {
		result = append(result, "game")
	}
	return result
}

func translationEditionEditableSide(document model.LyricsSourceDocument, renditionKey, side string) (*model.LyricsSourceFull, bool) {
	for index := range document.Renditions {
		rendition := &document.Renditions[index]
		if rendition.RenditionKey != renditionKey {
			continue
		}
		for _, editable := range translationEditionEditableSides(*rendition) {
			if editable == side {
				return renditionTranslationSide(document, renditionKey, side)
			}
		}
	}
	return nil, false
}

func buildLyricsTranslationEditionDocument(bundle lyricsRenditionEditorBundle, selection lyricsTranslationEditionSelection) (LyricsRenditionDocument, error) {
	result, err := buildLyricsRenditionEditorDocument(bundle, selection.localization)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	result.TranslationEditionKey = selection.key
	result.DefaultTranslationEditionKey = selection.defaultKey
	result.TranslationEditions = cloneLyricsTranslationEditionSummaries(selection.editions)
	return result, nil
}

func normalizeLyricsTranslationEditionEnvelope(input *LyricsRenditionDocument, current LyricsRenditionDocument) error {
	if input.TranslationEditionKey == "" {
		input.TranslationEditionKey = current.TranslationEditionKey
	}
	if input.DefaultTranslationEditionKey == "" {
		input.DefaultTranslationEditionKey = current.DefaultTranslationEditionKey
	}
	if input.TranslationEditions == nil {
		input.TranslationEditions = cloneLyricsTranslationEditionSummaries(current.TranslationEditions)
	}
	if input.TranslationEditionKey != current.TranslationEditionKey ||
		input.DefaultTranslationEditionKey != current.DefaultTranslationEditionKey ||
		!reflect.DeepEqual(input.TranslationEditions, current.TranslationEditions) {
		return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"translation edition selector and metadata are immutable in lyrics save requests"}}
	}
	return nil
}

func replaceMaterializedLyricsTranslationEditionTx(tx *sql.Tx, bundle lyricsRenditionEditorBundle, editionKey string, document LyricsRenditionDocument, user string, now int64) error {
	if _, err := tx.Exec(`DELETE FROM song_lyrics_translation_edition_lines WHERE document_id=? AND edition_key=?`, bundle.documentID, editionKey); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyrics_translation_edition_localizations WHERE document_id=? AND edition_key=?`, bundle.documentID, editionKey); err != nil {
		return err
	}
	if len(document.Renditions) != len(bundle.document.Renditions) {
		return errors.New("translation edition rendition set changed")
	}
	for index, sourceRendition := range bundle.document.Renditions {
		rendition := document.Renditions[index]
		if rendition.Key != sourceRendition.RenditionKey {
			return errors.New("translation edition rendition order changed")
		}
		translationCredit, proofreadingCredit := "", ""
		if rendition.TranslationCredits != nil {
			translationCredit = rendition.TranslationCredits.Translation
			proofreadingCredit = rendition.TranslationCredits.Proofreading
		}
		if _, err := tx.Exec(`INSERT INTO song_lyrics_translation_edition_localizations
			(document_id,edition_key,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by)
			VALUES (?,?,?,?,?,?,?,?)`, bundle.documentID, editionKey, rendition.Key, "zh-CN",
			translationCredit, proofreadingCredit, now, user); err != nil {
			return err
		}
		for _, side := range translationEditionEditableSides(sourceRendition) {
			var lines []PublicLyricsV3Line
			if side == "full" && rendition.Full != nil {
				lines = rendition.Full.Lines
			} else if side == "game" && rendition.Game != nil {
				lines = rendition.Game.Lines
			} else {
				return fmt.Errorf("translation edition rendition %q/%s is missing", rendition.Key, side)
			}
			source, _ := translationEditionEditableSide(bundle.document, rendition.Key, side)
			if len(lines) != len(source.Lines) {
				return fmt.Errorf("translation edition rendition %q/%s line count changed", rendition.Key, side)
			}
			for position, line := range lines {
				if _, err := tx.Exec(`INSERT INTO song_lyrics_translation_edition_lines
					(document_id,edition_key,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?,?)`,
					bundle.documentID, editionKey, rendition.Key, side, "zh-CN", position, line.Chinese); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func rewriteLegacyLyricsTranslationMirrorTx(tx *sql.Tx, bundle lyricsRenditionEditorBundle, editionKey string, revision int, now int64, user string) error {
	state, err := loadMaterializedLyricsTranslationEdition(tx, bundle.documentID, editionKey, bundle.document, revision, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyrics_rendition_side_translation_lines WHERE document_id=?`, bundle.documentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyrics_rendition_translation_lines WHERE document_id=?`, bundle.documentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyrics_rendition_localizations WHERE document_id=?`, bundle.documentID); err != nil {
		return err
	}
	primary := make(map[string]lyricscontract.RenditionTranslation, len(state.Translations))
	for _, item := range state.Translations {
		primary[item.RenditionKey] = item
	}
	for _, rendition := range bundle.document.Renditions {
		item := primary[rendition.RenditionKey]
		if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
			VALUES (?,?,?,?,?,?,?,?)`, bundle.documentID, rendition.RenditionKey, "zh-CN",
			item.TranslationCredit, item.ProofreadingCredit, now, user, revision); err != nil {
			return err
		}
		for position, text := range item.Translations {
			if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_translation_lines
				(document_id,rendition_key,locale,position,text) VALUES (?,?,?,?,?)`, bundle.documentID,
				rendition.RenditionKey, "zh-CN", position, text); err != nil {
				return err
			}
		}
		for side, values := range state.SideTranslations[rendition.RenditionKey] {
			for position, text := range values {
				if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_side_translation_lines
					(document_id,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?)`, bundle.documentID,
					rendition.RenditionKey, side, "zh-CN", position, text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func clearLyricsTranslationEditionDocument(document LyricsRenditionDocument) LyricsRenditionDocument {
	for renditionIndex := range document.Renditions {
		document.Renditions[renditionIndex].TranslationCredits = nil
		for _, side := range []*PublicLyricsV3Side{document.Renditions[renditionIndex].Full, document.Renditions[renditionIndex].Game} {
			if side == nil {
				continue
			}
			for lineIndex := range side.Lines {
				side.Lines[lineIndex].Chinese = ""
			}
		}
	}
	return document
}

func cloneLyricsRenditionDocument(document LyricsRenditionDocument) (LyricsRenditionDocument, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	var result LyricsRenditionDocument
	if err := json.Unmarshal(body, &result); err != nil {
		return LyricsRenditionDocument{}, err
	}
	return result, nil
}

func materializeMainLyricsTranslationEditionTx(tx *sql.Tx, bundle lyricsRenditionEditorBundle, user string) (lyricsTranslationEditionSelection, error) {
	selection := lyricsTranslationEditionSelection{
		key: MainLyricsTranslationEditionKey, defaultKey: MainLyricsTranslationEditionKey,
		editions:     []LyricsTranslationEditionSummary{{Key: MainLyricsTranslationEditionKey, Label: MainLyricsTranslationEditionLabel}},
		localization: bundle.localization,
	}
	current, err := buildLyricsTranslationEditionDocument(bundle, selection)
	if err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	now := lyricsRenditionEditorUpdatedAt(bundle, bundle.localization)
	if now <= 0 {
		now = time.Now().Unix()
	}
	if _, err := tx.Exec(`INSERT INTO song_lyrics_translation_editions
		(document_id,edition_key,label,created_at,created_by) VALUES (?,?,?,?,?)`, bundle.documentID,
		MainLyricsTranslationEditionKey, MainLyricsTranslationEditionLabel, now, user); err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	if err := replaceMaterializedLyricsTranslationEditionTx(tx, bundle, MainLyricsTranslationEditionKey, current, user, now); err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	if _, err := tx.Exec(`INSERT INTO song_lyrics_translation_edition_state
		(document_id,default_edition_key,revision,updated_at,updated_by) VALUES (?,?,?,?,?)`, bundle.documentID,
		MainLyricsTranslationEditionKey, current.Revision, now, user); err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	if err := rewriteLegacyLyricsTranslationMirrorTx(tx, bundle, MainLyricsTranslationEditionKey, current.Revision, now, user); err != nil {
		return lyricsTranslationEditionSelection{}, err
	}
	selection.authoritative = true
	selection.localization, err = loadMaterializedLyricsTranslationEdition(tx, bundle.documentID, MainLyricsTranslationEditionKey, bundle.document, current.Revision, now)
	return selection, err
}

func (s *Store) MutateLyricsTranslationEdition(input LyricsTranslationEditionMutation, user string) (LyricsRenditionDocument, error) {
	unlock := s.lockLyrics(input.MusicID)
	defer unlock()
	if input.MusicID <= 0 {
		return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"musicId must be positive"}}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	defer tx.Rollback()
	bundle, err := loadLyricsRenditionEditorBundle(tx, input.MusicID)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	selection, err := loadLyricsTranslationEditionSelection(tx, bundle, "", false)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	current, err := buildLyricsTranslationEditionDocument(bundle, selection)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	if input.Revision != current.Revision {
		copy := current
		return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "revision_conflict", Current: &copy}
	}
	if !selection.authoritative {
		selection, err = materializeMainLyricsTranslationEditionTx(tx, bundle, user)
		if err != nil {
			return LyricsRenditionDocument{}, err
		}
	}
	now := time.Now().Unix()
	if now < selection.localization.UpdatedAt {
		now = selection.localization.UpdatedAt
	}
	nextRevision := current.Revision + 1
	affectedKey := input.EditionKey
	switch input.Operation {
	case "create":
		if !validLyricsTranslationEditionKey(input.EditionKey) || input.EditionKey == MainLyricsTranslationEditionKey || input.SourceEditionKey != "" {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"create requires a new non-main editionKey"}}
		}
		if err := validateLyricsTranslationEditionLabel(input.Label); err != nil {
			return LyricsRenditionDocument{}, err
		}
		if len(selection.editions) >= maxLyricsTranslationEditions {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_limit", Details: []string{"a song may have at most 16 translation editions"}}
		}
		blank, err := cloneLyricsRenditionDocument(current)
		if err != nil {
			return LyricsRenditionDocument{}, err
		}
		blank = clearLyricsTranslationEditionDocument(blank)
		if _, err := tx.Exec(`INSERT INTO song_lyrics_translation_editions
			(document_id,edition_key,label,created_at,created_by) VALUES (?,?,?,?,?)`, bundle.documentID,
			input.EditionKey, input.Label, now, user); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_exists"}
			}
			return LyricsRenditionDocument{}, err
		}
		if err := replaceMaterializedLyricsTranslationEditionTx(tx, bundle, input.EditionKey, blank, user, now); err != nil {
			return LyricsRenditionDocument{}, err
		}
	case "clone":
		if !validLyricsTranslationEditionKey(input.SourceEditionKey) || !validLyricsTranslationEditionKey(input.EditionKey) ||
			input.EditionKey == MainLyricsTranslationEditionKey || input.SourceEditionKey == input.EditionKey {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"clone requires distinct valid sourceEditionKey and non-main editionKey"}}
		}
		if err := validateLyricsTranslationEditionLabel(input.Label); err != nil {
			return LyricsRenditionDocument{}, err
		}
		if len(selection.editions) >= maxLyricsTranslationEditions {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_limit", Details: []string{"a song may have at most 16 translation editions"}}
		}
		sourceSelection, err := loadLyricsTranslationEditionSelection(tx, bundle, input.SourceEditionKey, true)
		if errors.Is(err, ErrLyricsTranslationEditionNotFound) {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_not_found"}
		}
		if err != nil {
			return LyricsRenditionDocument{}, err
		}
		source, err := buildLyricsTranslationEditionDocument(bundle, sourceSelection)
		if err != nil {
			return LyricsRenditionDocument{}, err
		}
		if _, err := tx.Exec(`INSERT INTO song_lyrics_translation_editions
			(document_id,edition_key,label,created_at,created_by) VALUES (?,?,?,?,?)`, bundle.documentID,
			input.EditionKey, input.Label, now, user); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_exists"}
			}
			return LyricsRenditionDocument{}, err
		}
		if err := replaceMaterializedLyricsTranslationEditionTx(tx, bundle, input.EditionKey, source, user, now); err != nil {
			return LyricsRenditionDocument{}, err
		}
	case "rename":
		if !validLyricsTranslationEditionKey(input.EditionKey) || input.SourceEditionKey != "" {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"rename requires editionKey"}}
		}
		if err := validateLyricsTranslationEditionLabel(input.Label); err != nil {
			return LyricsRenditionDocument{}, err
		}
		result, err := tx.Exec(`UPDATE song_lyrics_translation_editions SET label=? WHERE document_id=? AND edition_key=?`,
			input.Label, bundle.documentID, input.EditionKey)
		if err != nil {
			return LyricsRenditionDocument{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_not_found"}
		}
	case "set-default":
		if !validLyricsTranslationEditionKey(input.EditionKey) || input.SourceEditionKey != "" || input.Label != "" {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"set-default requires only editionKey"}}
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM song_lyrics_translation_editions WHERE document_id=? AND edition_key=?`,
			bundle.documentID, input.EditionKey).Scan(&exists); err != nil {
			return LyricsRenditionDocument{}, err
		}
		if exists != 1 {
			return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_not_found"}
		}
		selection.defaultKey = input.EditionKey
	default:
		return LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"operation must be create, clone, rename, or set-default"}}
	}
	if _, err := tx.Exec(`UPDATE song_lyrics_translation_edition_state
		SET default_edition_key=?,revision=?,updated_at=?,updated_by=? WHERE document_id=?`,
		selection.defaultKey, nextRevision, now, user, bundle.documentID); err != nil {
		return LyricsRenditionDocument{}, err
	}
	if err := rewriteLegacyLyricsTranslationMirrorTx(tx, bundle, selection.defaultKey, nextRevision, now, user); err != nil {
		return LyricsRenditionDocument{}, err
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.translation-edition.mutate',?)`,
		now, user, fmt.Sprintf("musicId=%d revision=%d operation=%s editionKey=%s sourceEditionKey=%s", input.MusicID,
			nextRevision, input.Operation, input.EditionKey, input.SourceEditionKey)); err != nil {
		return LyricsRenditionDocument{}, err
	}
	resultSelection, err := loadLyricsTranslationEditionSelection(tx, bundle, affectedKey, true)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	result, err := buildLyricsTranslationEditionDocument(bundle, resultSelection)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	if err := tx.Commit(); err != nil {
		return LyricsRenditionDocument{}, err
	}
	s.NotifyChange()
	return result, nil
}

func sortedLyricsTranslationEditionKeys(editions []LyricsTranslationEditionSummary) []string {
	keys := make([]string, len(editions))
	for index, edition := range editions {
		keys[index] = edition.Key
	}
	sort.Strings(keys)
	return keys
}
