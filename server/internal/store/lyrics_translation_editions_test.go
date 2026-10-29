package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

func TestLyricsTranslationEditionsLazyMaterializationCASSaveAndDefaultMirror(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	initial, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if initial.TranslationEditionKey != MainLyricsTranslationEditionKey ||
		initial.DefaultTranslationEditionKey != MainLyricsTranslationEditionKey ||
		!reflect.DeepEqual(initial.TranslationEditions, []LyricsTranslationEditionSummary{{Key: "main", Label: "默认译本"}}) {
		t.Fatalf("virtual main selector=%+v", initial)
	}
	var stateRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_translation_edition_state WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=10)`).Scan(&stateRows); err != nil || stateRows != 0 {
		t.Fatalf("GET eagerly materialized v30 state=%d err=%v", stateRows, err)
	}

	created, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: initial.Revision, Operation: "create", EditionKey: "alternate", Label: "另一译本",
	}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if created.TranslationEditionKey != "alternate" || created.DefaultTranslationEditionKey != "main" ||
		created.Revision != initial.Revision+1 || len(created.TranslationEditions) != 2 {
		t.Fatalf("created edition=%+v", created)
	}
	assertLyricsEditionDocumentTranslations(t, created, "")
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_translation_edition_state WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=10)`).Scan(&stateRows); err != nil || stateRows != 1 {
		t.Fatalf("metadata mutation state rows=%d err=%v", stateRows, err)
	}

	mainAfterCreate, err := s.GetLyricsRenditionDocumentEdition(10, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if mainAfterCreate.Renditions[0].Full.Lines[0].Chinese != initial.Renditions[0].Full.Lines[0].Chinese {
		t.Fatal("materializing main changed the legacy translation")
	}

	alternateSave := cloneLyricsRenditionEditorDocument(t, created)
	alternateSave.Renditions[0].Full.Lines[0].Chinese = "alternate-full"
	alternateSave.Renditions[0].TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: "Alternate Translator"}
	savedAlternate, changed, err := s.SaveLyricsRenditionMutation(alternateSave, "bob")
	if err != nil || !changed || savedAlternate.TranslationEditionKey != "alternate" || savedAlternate.Revision != created.Revision+1 {
		t.Fatalf("save alternate=%+v changed=%v err=%v", savedAlternate, changed, err)
	}
	mainAfterAlternateSave, err := s.GetLyricsRenditionDocumentEdition(10, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if mainAfterAlternateSave.Renditions[0].Full.Lines[0].Chinese == "alternate-full" {
		t.Fatal("saving a non-default edition overwrote main")
	}
	if _, _, err := s.SaveLyricsRenditionMutation(alternateSave, "stale"); err == nil {
		t.Fatal("stale edition save was accepted")
	} else {
		var conflict *LyricsRenditionContractError
		if !errors.As(err, &conflict) || conflict.Code != "revision_conflict" || conflict.Current == nil || conflict.Current.TranslationEditionKey != "alternate" {
			t.Fatalf("stale edition conflict=%#v", err)
		}
	}

	cloned, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: savedAlternate.Revision, Operation: "clone", SourceEditionKey: "alternate",
		EditionKey: "alternate-copy", Label: "复制译本",
	}, "carol")
	if err != nil || cloned.Renditions[0].Full.Lines[0].Chinese != "alternate-full" {
		t.Fatalf("clone edition=%+v err=%v", cloned, err)
	}
	renamed, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: cloned.Revision, Operation: "rename", EditionKey: "alternate-copy", Label: "重命名译本",
	}, "carol")
	if err != nil || renamed.TranslationEditions[0].Key != "alternate" {
		t.Fatalf("rename edition=%+v err=%v", renamed, err)
	}
	setDefault, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: renamed.Revision, Operation: "set-default", EditionKey: "alternate",
	}, "dave")
	if err != nil || setDefault.DefaultTranslationEditionKey != "alternate" || setDefault.TranslationEditionKey != "alternate" {
		t.Fatalf("set default=%+v err=%v", setDefault, err)
	}
	var mirrored string
	if err := s.db.QueryRow(`SELECT text FROM song_lyrics_rendition_translation_lines
		WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=10) AND rendition_key=? AND position=0`, setDefault.Renditions[0].Key).Scan(&mirrored); err != nil {
		t.Fatal(err)
	}
	if mirrored != "alternate-full" {
		t.Fatalf("legacy default mirror=%q", mirrored)
	}

	legacyClientSave := cloneLyricsRenditionEditorDocument(t, setDefault)
	legacyClientSave.TranslationEditionKey = ""
	legacyClientSave.DefaultTranslationEditionKey = ""
	legacyClientSave.TranslationEditions = nil
	legacyClientSave.Renditions[0].Full.Lines[0].Chinese = "legacy-client-default-save"
	legacySaved, changed, err := s.SaveLyricsRenditionMutation(legacyClientSave, "legacy-client")
	if err != nil || !changed || legacySaved.TranslationEditionKey != "alternate" || len(legacySaved.TranslationEditions) != 3 {
		t.Fatalf("legacy client default save=%+v changed=%v err=%v", legacySaved, changed, err)
	}
	mainFinal, err := s.GetLyricsRenditionDocumentEdition(10, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if mainFinal.Renditions[0].Full.Lines[0].Chinese == "legacy-client-default-save" {
		t.Fatal("legacy client default save destroyed main")
	}

	for _, rendition := range documentSourceRenditionsFromEditor(t, s, 10) {
		if rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection {
			continue
		}
		var gameRows int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_translation_edition_lines
			WHERE rendition_key=? AND side='game'`, rendition.RenditionKey).Scan(&gameRows); err != nil {
			t.Fatal(err)
		}
		if gameRows != 0 {
			t.Fatalf("exact projection persisted %d Game edition lines", gameRows)
		}
	}
}

func TestLyricsTranslationEditionBackupRoundTripAndLegacyRestoreClearsV30(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	legacyBackup, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal(legacyBackup)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacyJSON, []byte("translationEdition")) {
		t.Fatalf("virtual main changed legacy backup JSON: %s", legacyJSON)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: current.Revision, Operation: "create", EditionKey: "alternate", Label: "另一译本",
	}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	request := cloneLyricsRenditionEditorDocument(t, created)
	request.Renditions[0].Full.Lines[0].Chinese = "backup-alternate"
	saved, _, err := s.SaveLyricsRenditionMutation(request, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: saved.Revision, Operation: "set-default", EditionKey: "alternate",
	}, "alice"); err != nil {
		t.Fatal(err)
	}
	editionBackup, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if len(editionBackup.TranslationEditionStates) != 1 || len(editionBackup.TranslationEditions) != 2 ||
		len(editionBackup.TranslationEditionLocalizations) == 0 || len(editionBackup.TranslationEditionLines) == 0 {
		t.Fatalf("edition backup shape=%+v", editionBackup)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, editionBackup); err != nil {
		t.Fatalf("restore edition backup: %v", err)
	}
	reloaded, err := restored.GetLyricsRenditionDocumentEdition(10, "alternate", true)
	if err != nil || reloaded.DefaultTranslationEditionKey != "alternate" ||
		reloaded.Renditions[0].Full.Lines[0].Chinese != "backup-alternate" {
		t.Fatalf("restored alternate=%+v err=%v", reloaded, err)
	}

	invalid := cloneLyricsContentExport(t, editionBackup)
	invalid.TranslationEditionLines = invalid.TranslationEditionLines[:len(invalid.TranslationEditionLines)-1]
	if err := setupLyricsStore(t).ImportTranslationContent(nil, EventContentExport{}, invalid); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("incomplete edition backup error=%v", err)
	}

	if err := s.ImportTranslationContent(nil, EventContentExport{}, legacyBackup); err != nil {
		t.Fatalf("restore legacy backup over v30 content: %v", err)
	}
	legacyRestored, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if legacyRestored.TranslationEditionKey != "main" || len(legacyRestored.TranslationEditions) != 1 {
		t.Fatalf("legacy restore retained v30 editions=%+v", legacyRestored.TranslationEditions)
	}
	var states int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_translation_edition_state WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=10)`).Scan(&states); err != nil || states != 0 {
		t.Fatalf("legacy restore v30 state rows=%d err=%v", states, err)
	}
}

func assertLyricsEditionDocumentTranslations(t *testing.T, document LyricsRenditionDocument, want string) {
	t.Helper()
	for _, rendition := range document.Renditions {
		for _, side := range []*PublicLyricsV3Side{rendition.Full, rendition.Game} {
			if side == nil {
				continue
			}
			for _, line := range side.Lines {
				if line.Chinese != want {
					t.Fatalf("edition translation=%q want=%q", line.Chinese, want)
				}
			}
		}
	}
}

func documentSourceRenditionsFromEditor(t *testing.T, s *Store, musicID int) []model.LyricsSourceRendition {
	t.Helper()
	var body string
	if err := s.db.QueryRow(`SELECT document_json FROM song_lyrics_source_documents WHERE music_id=? AND schema_version=3`, musicID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	document, err := model.DecodeLyricsSourceDocument([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return document.Renditions
}
