package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsrecoveryimport"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

func TestSaveLyricsRenditionMutationBeforeCommitSharesAtomicTransaction(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	requested.Renditions[0].Full.Lines[0].Chinese = "atomic rendition update"
	if _, err := s.db.Exec(`CREATE TABLE atomic_rendition_probe (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("reject rendition checkpoint")
	_, _, err = s.SaveLyricsRenditionMutationWithBeforeCommit(requested, "editor", func(tx *sql.Tx, saved LyricsRenditionDocument, changed bool) error {
		if !changed || saved.Revision != current.Revision+1 {
			t.Fatalf("callback saved revision=%d changed=%t", saved.Revision, changed)
		}
		if _, err := tx.Exec(`INSERT INTO atomic_rendition_probe(value) VALUES ('collab-ledger')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("save error=%v want sentinel", err)
	}
	afterRollback, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	var probes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM atomic_rendition_probe`).Scan(&probes); err != nil {
		t.Fatal(err)
	}
	if afterRollback.Revision != current.Revision || probes != 0 {
		t.Fatalf("rollback revision=%d want=%d probes=%d", afterRollback.Revision, current.Revision, probes)
	}

	saved, changed, err := s.SaveLyricsRenditionMutationWithBeforeCommit(requested, "editor", func(tx *sql.Tx, saved LyricsRenditionDocument, changed bool) error {
		_, err := tx.Exec(`INSERT INTO atomic_rendition_probe(value) VALUES (?)`, fmt.Sprintf("revision=%d changed=%t", saved.Revision, changed))
		return err
	})
	if err != nil || !changed || saved.Revision != current.Revision+1 {
		t.Fatalf("saved revision=%d changed=%t err=%v", saved.Revision, changed, err)
	}
	var value string
	if err := s.db.QueryRow(`SELECT value FROM atomic_rendition_probe`).Scan(&value); err != nil || value != "revision=2 changed=true" {
		t.Fatalf("probe=%q err=%v", value, err)
	}
}

func TestSaveLyricsRenditionMutationBeforeCommitRunsForNoOp(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE atomic_rendition_probe (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	replayed, changed, err := s.SaveLyricsRenditionMutationWithBeforeCommit(current, "editor", func(tx *sql.Tx, saved LyricsRenditionDocument, changed bool) error {
		if changed || saved.Revision != current.Revision {
			t.Fatalf("no-op callback revision=%d changed=%t", saved.Revision, changed)
		}
		_, err := tx.Exec(`INSERT INTO atomic_rendition_probe(value) VALUES ('no-op-checkpoint')`)
		return err
	})
	if err != nil || changed || replayed.Revision != current.Revision {
		t.Fatalf("replay revision=%d changed=%t err=%v", replayed.Revision, changed, err)
	}
	var probes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM atomic_rendition_probe`).Scan(&probes); err != nil || probes != 1 {
		t.Fatalf("probes=%d err=%v", probes, err)
	}
}

func TestLyricsRenditionEditorLoadSaveConflictAndImmutableFacts(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if current.MusicID != 10 || current.Status != "draft" || current.Revision != 1 ||
		current.PublishedRevision != 0 || current.UpdatedAt == "" || len(current.Renditions) != 2 {
		t.Fatalf("initial editor document=%+v", current)
	}

	requested := cloneLyricsRenditionEditorDocument(t, current)
	changedExactProjection := false
	for renditionIndex := range requested.Renditions {
		rendition := &requested.Renditions[renditionIndex]
		if rendition.Full == nil {
			continue
		}
		rendition.Full.Lines[0].Chinese = "更新后的简中"
		rendition.TranslationCredits = &PublicLyricsV3TranslationCredits{
			Translation: "新译者", Proofreading: "新校对",
		}
		if rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection || rendition.Game == nil {
			continue
		}
		fullByID := make(map[string]string, len(rendition.Full.Lines))
		for _, line := range rendition.Full.Lines {
			fullByID[line.ID] = line.Chinese
		}
		for lineIndex, lineID := range rendition.Relation.LineIDs {
			rendition.Game.Lines[lineIndex].Chinese = fullByID[lineID]
		}
		changedExactProjection = true
	}
	if !changedExactProjection {
		t.Fatal("fixture has no exact projection rendition")
	}

	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || saved.Revision != 2 || saved.Status != "draft" || saved.PublishedRevision != 0 {
		t.Fatalf("saved=%+v changed=%v", saved, changed)
	}
	for _, rendition := range saved.Renditions {
		if rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection {
			continue
		}
		fullByID := make(map[string]string, len(rendition.Full.Lines))
		for _, line := range rendition.Full.Lines {
			fullByID[line.ID] = line.Chinese
		}
		for lineIndex, lineID := range rendition.Relation.LineIDs {
			if rendition.Game.Lines[lineIndex].Chinese != fullByID[lineID] {
				t.Fatalf("projection translation[%d]=%q want=%q", lineIndex,
					rendition.Game.Lines[lineIndex].Chinese, fullByID[lineID])
			}
		}
	}

	if replay, replayChanged, err := s.SaveLyricsRenditionMutation(saved, "alice"); err != nil || replayChanged || replay.Revision != 2 {
		t.Fatalf("no-op replay=%+v changed=%v err=%v", replay, replayChanged, err)
	}

	_, _, err = s.SaveLyricsRenditionMutation(requested, "bob")
	var conflict *LyricsRenditionContractError
	if !errors.As(err, &conflict) || conflict.Code != "revision_conflict" || conflict.Current == nil || conflict.Current.Revision != 2 {
		t.Fatalf("stale save error=%#v", err)
	}

	tampered := cloneLyricsRenditionEditorDocument(t, saved)
	tampered.Renditions[0].Full.Lines[0].Japanese = "改ざんされた日本語歌詞"
	_, _, err = s.SaveLyricsRenditionMutation(tampered, "mallory")
	var contractErr *LyricsRenditionContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("immutable tamper error=%#v", err)
	}

	tamperedRuby := cloneLyricsRenditionEditorDocument(t, saved)
	tamperedRuby.Renditions[0].Full.Lines[0].Segments[0].Ruby[0].Reading = "tampered_reading"
	_, _, err = s.SaveLyricsRenditionMutation(tamperedRuby, "mallory")
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("immutable ruby tamper error=%#v", err)
	}

	var localizationCount, revisionTwoCount int
	if err := s.db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN revision=2 THEN 1 ELSE 0 END)
		FROM song_lyrics_rendition_localizations`).Scan(&localizationCount, &revisionTwoCount); err != nil {
		t.Fatal(err)
	}
	if localizationCount != len(saved.Renditions) || revisionTwoCount != localizationCount {
		t.Fatalf("localizations=%d revisionTwo=%d renditions=%d", localizationCount, revisionTwoCount, len(saved.Renditions))
	}

}

func TestLyricsRenditionEditorPersistsIndependentGameTranslationBySide(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	var renditionKey, fullBefore string
	var gameLineCount int
	for renditionIndex := range requested.Renditions {
		rendition := &requested.Renditions[renditionIndex]
		if rendition.Full == nil || rendition.Game == nil || rendition.Relation.Kind != model.LyricsSourceRenditionRelationNone {
			continue
		}
		renditionKey = rendition.Key
		fullBefore = rendition.Full.Lines[0].Chinese
		gameLineCount = len(rendition.Game.Lines)
		for lineIndex := range rendition.Game.Lines {
			rendition.Game.Lines[lineIndex].Chinese = fmt.Sprintf("independent-game-%d", lineIndex+1)
		}
		break
	}
	if renditionKey == "" {
		t.Fatal("fixture has no Full + independent Game rendition")
	}
	saved, changed, targets, err := s.SaveLyricsRenditionMutationWithTargets(requested, "game-editor")
	if err != nil || !changed || saved.Revision != current.Revision+1 {
		t.Fatalf("saved=%+v changed=%v targets=%+v err=%v", saved, changed, targets, err)
	}
	if !reflect.DeepEqual(targets, []LyricsRenditionMutationTarget{{
		RenditionKey: renditionKey, Side: "game", Locale: "zh-CN",
	}}) {
		t.Fatalf("independent Game mutation targets=%+v", targets)
	}
	var savedRendition *PublicLyricsV3Rendition
	for index := range saved.Renditions {
		if saved.Renditions[index].Key == renditionKey {
			savedRendition = &saved.Renditions[index]
			break
		}
	}
	if savedRendition == nil || savedRendition.Full.Lines[0].Chinese != fullBefore ||
		savedRendition.Game.Lines[0].Chinese != "independent-game-1" {
		t.Fatalf("saved independent Game rendition=%+v", savedRendition)
	}
	var peerCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_rendition_side_translation_lines
		WHERE rendition_key=? AND side='game' AND locale='zh-CN'`, renditionKey).Scan(&peerCount); err != nil {
		t.Fatal(err)
	}
	if peerCount != gameLineCount {
		t.Fatalf("peer translation rows=%d want=%d", peerCount, gameLineCount)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	var exportedPeer int
	for _, line := range exported.RenditionTranslationLines {
		if line.RenditionKey == renditionKey && line.Side == "game" {
			exportedPeer++
		}
	}
	if exportedPeer != gameLineCount {
		t.Fatalf("exported peer rows=%d want=%d", exportedPeer, gameLineCount)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, exported); err != nil {
		t.Fatal(err)
	}
	reloaded, err := restored.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendition := range reloaded.Renditions {
		if rendition.Key == renditionKey && rendition.Game.Lines[0].Chinese != "independent-game-1" {
			t.Fatalf("restored independent Game=%+v", rendition.Game.Lines)
		}
	}
}

func TestLyricsRenditionEditorRejectsPrimaryAndPeerTranslationsOverDocumentBoundaryWithoutMutation(t *testing.T) {
	s := setupLyricsStore(t)
	document, evidenceByIdentity := renditionV3PersistenceDocument(t)
	rendition := &document.Renditions[0]
	if rendition.Full == nil || rendition.Game == nil || rendition.Relation.Kind != model.LyricsSourceRenditionRelationNone {
		t.Fatal("fixture has no Full + independent Game rendition")
	}
	fullTemplate := rendition.Full.Lines[0]
	gameTemplate := rendition.Game.Lines[0]
	rendition.Full.Lines = make([]model.LyricsSourceFullLine, 129)
	rendition.Game.Lines = make([]model.LyricsSourceFullLine, 129)
	for index := 0; index < 129; index++ {
		rendition.Full.Lines[index] = fullTemplate
		rendition.Full.Lines[index].ID = fmt.Sprintf("full-%06d", index+1)
		rendition.Game.Lines[index] = gameTemplate
		rendition.Game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("expanded rendition document: %v", err)
	}
	translations := []lyricsstaging.RenditionTranslation{
		{RenditionKey: document.Renditions[0].RenditionKey, Translations: make([]string, 129)},
		{RenditionKey: document.Renditions[1].RenditionKey, Translations: []string{""}},
	}
	if err := insertRenditionV3PersistenceGraph(t, s, document, evidenceByIdentity, translations); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	chunk := strings.Repeat("x", maxLyricsLineTextBytes)
	for index := range requested.Renditions[0].Full.Lines {
		requested.Renditions[0].Full.Lines[index].Chinese = chunk
		requested.Renditions[0].Game.Lines[index].Chinese = chunk
	}
	var beforeRevision int
	if err := s.db.QueryRow(`SELECT revision FROM song_lyrics_rendition_localizations ORDER BY rendition_key LIMIT 1`).Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.SaveLyricsRenditionMutation(requested, "oversized-editor")
	var contractErr *LyricsRenditionContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" ||
		!strings.Contains(strings.Join(contractErr.Details, "\n"), "safe document size") {
		t.Fatalf("aggregate size error=%#v", err)
	}
	var afterRevision int
	if err := s.db.QueryRow(`SELECT revision FROM song_lyrics_rendition_localizations ORDER BY rendition_key LIMIT 1`).Scan(&afterRevision); err != nil {
		t.Fatal(err)
	}
	if afterRevision != beforeRevision {
		t.Fatalf("failed aggregate validation changed revision %d -> %d", beforeRevision, afterRevision)
	}

	var documentID int64
	if err := s.db.QueryRow(`SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, 10).Scan(&documentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_translation_lines SET text=?
		WHERE document_id=? AND rendition_key=?`, chunk, documentID, document.Renditions[0].RenditionKey); err != nil {
		t.Fatal(err)
	}
	for position := range rendition.Game.Lines {
		if _, err := s.db.Exec(`INSERT INTO song_lyrics_rendition_side_translation_lines
			(document_id,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?)`,
			documentID, document.Renditions[0].RenditionKey, "game", "zh-CN", position, chunk); err != nil {
			t.Fatal(err)
		}
	}
	historical, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("load historical oversized localization for repair: %v", err)
	}
	repair := cloneLyricsRenditionEditorDocument(t, historical)
	for index := range repair.Renditions[0].Full.Lines {
		repair.Renditions[0].Full.Lines[index].Chinese = ""
		repair.Renditions[0].Game.Lines[index].Chinese = ""
	}
	repaired, changed, err := s.SaveLyricsRenditionMutation(repair, "oversized-repair-editor")
	if err != nil || !changed || repaired.Revision != historical.Revision+1 ||
		repaired.Renditions[0].Full.Lines[0].Chinese != "" || repaired.Renditions[0].Game.Lines[0].Chinese != "" {
		t.Fatalf("repair historical aggregate saved=%+v changed=%v err=%v", repaired, changed, err)
	}
}

func TestLyricsRenditionEditorRejectsUnpublishableCreditsWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		credit string
	}{
		{name: "surrounding whitespace", credit: " 译者"},
		{name: "over public byte limit", credit: strings.Repeat("x", maxLyricsRenditionCreditBytes+1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s, _ := setupRenditionV3PersistenceStore(t)
			current, err := s.GetLyricsRenditionDocument(10)
			if err != nil {
				t.Fatal(err)
			}
			requested := cloneLyricsRenditionEditorDocument(t, current)
			requested.Renditions[0].TranslationCredits.Translation = testCase.credit
			_, changed, err := s.SaveLyricsRenditionMutation(requested, "credits-editor")
			var contractErr *LyricsRenditionContractError
			if changed || !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" ||
				!strings.Contains(strings.Join(contractErr.Details, "\n"), "2048-byte public limit") {
				t.Fatalf("changed=%v error=%#v", changed, err)
			}
			reloaded, err := s.GetLyricsRenditionDocument(10)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Revision != current.Revision ||
				reloaded.Renditions[0].TranslationCredits.Translation != current.Renditions[0].TranslationCredits.Translation {
				t.Fatalf("failed credit validation mutated document: current=%+v reloaded=%+v", current, reloaded)
			}
		})
	}
}

func TestLyricsRenditionEditorCanRepairHistoricalUnpublishableCredits(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_localizations
		SET translation_credit=? WHERE rendition_key=(SELECT rendition_key FROM song_lyrics_rendition_localizations ORDER BY rendition_key LIMIT 1)`,
		" historical translator "); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if current.Renditions[0].TranslationCredits == nil ||
		current.Renditions[0].TranslationCredits.Translation != " historical translator " {
		t.Fatalf("historical credit fixture=%+v", current.Renditions[0].TranslationCredits)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	requested.Renditions[0].TranslationCredits.Translation = "repaired translator"
	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "credits-repair-editor")
	if err != nil || !changed || saved.Revision != current.Revision+1 ||
		saved.Renditions[0].TranslationCredits.Translation != "repaired translator" {
		t.Fatalf("saved=%+v changed=%v err=%v", saved, changed, err)
	}
}

func TestLyricsRenditionEditorMutationTargetsUseActualDocumentDiff(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}

	creditsOnly := cloneLyricsRenditionEditorDocument(t, current)
	creditsOnly.Renditions[0].TranslationCredits.Translation = "credits-only"
	_, changed, targets, err := s.SaveLyricsRenditionMutationWithTargets(creditsOnly, "credits-editor")
	if err != nil || !changed || !reflect.DeepEqual(targets, []LyricsRenditionMutationTarget{{
		RenditionKey: creditsOnly.Renditions[0].Key, Side: "credits", Locale: "zh-CN",
	}}) {
		t.Fatalf("credits-only changed=%v targets=%+v err=%v", changed, targets, err)
	}

	current, err = s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	multiple := cloneLyricsRenditionEditorDocument(t, current)
	multiple.Renditions[0].Full.Lines[0].Chinese = "full-and-credits"
	multiple.Renditions[0].TranslationCredits.Translation = "full-and-credits"
	_, changed, targets, err = s.SaveLyricsRenditionMutationWithTargets(multiple, "multi-editor")
	want := []LyricsRenditionMutationTarget{
		{RenditionKey: multiple.Renditions[0].Key, Side: "full", Locale: "zh-CN"},
		{RenditionKey: multiple.Renditions[0].Key, Side: "credits", Locale: "zh-CN"},
	}
	if err != nil || !changed || !reflect.DeepEqual(targets, want) {
		t.Fatalf("multi-target changed=%v targets=%+v want=%+v err=%v", changed, targets, want, err)
	}
}

func TestLyricsRenditionEditorSaveDoesNotMoveUpdatedAtBackward(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	futureUpdatedAt := time.Now().Add(24 * time.Hour).Truncate(time.Second).Unix()
	if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_localizations SET updated_at=?`, futureUpdatedAt); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if current.UpdatedAt != formatTimestamp(futureUpdatedAt) {
		t.Fatalf("current updatedAt=%q want=%q", current.UpdatedAt, formatTimestamp(futureUpdatedAt))
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	if requested.Renditions[0].TranslationCredits == nil {
		t.Fatal("fixture has no translation credits")
	}
	requested.Renditions[0].TranslationCredits.Translation = "future-safe translator"

	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || saved.Revision != current.Revision+1 || saved.UpdatedAt != current.UpdatedAt {
		t.Fatalf("saved=%+v changed=%v current=%+v", saved, changed, current)
	}
	var backwardRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_rendition_localizations WHERE updated_at<?`, futureUpdatedAt).Scan(&backwardRows); err != nil {
		t.Fatal(err)
	}
	if backwardRows != 0 {
		t.Fatalf("localization rows moved backward=%d", backwardRows)
	}
}

func TestLyricsRenditionEditorFirstLocalizationCanStartFromIndependentGameOnly(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	if _, err := s.db.Exec(`DELETE FROM song_lyrics_rendition_localizations`); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	var renditionKey string
	for index := range requested.Renditions {
		rendition := &requested.Renditions[index]
		if rendition.Full == nil || rendition.Game == nil || rendition.Relation.Kind != model.LyricsSourceRenditionRelationNone {
			continue
		}
		renditionKey = rendition.Key
		for lineIndex := range rendition.Game.Lines {
			rendition.Game.Lines[lineIndex].Chinese = fmt.Sprintf("first-game-%d", lineIndex+1)
		}
		break
	}
	if renditionKey == "" {
		t.Fatal("fixture has no independent Game rendition")
	}
	saved, changed, targets, err := s.SaveLyricsRenditionMutationWithTargets(requested, "first-game-editor")
	if err != nil || !changed || saved.Revision != 2 || !reflect.DeepEqual(targets, []LyricsRenditionMutationTarget{{
		RenditionKey: renditionKey, Side: "game", Locale: "zh-CN",
	}}) {
		t.Fatalf("first Game save=%+v changed=%v targets=%+v err=%v", saved, changed, targets, err)
	}
	var parents, peerLines int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_rendition_localizations`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_rendition_side_translation_lines
		WHERE rendition_key=? AND side='game' AND locale='zh-CN'`, renditionKey).Scan(&peerLines); err != nil {
		t.Fatal(err)
	}
	if parents != len(saved.Renditions) || peerLines == 0 {
		t.Fatalf("first Game persistence parents=%d peerLines=%d", parents, peerLines)
	}
}

func TestLyricsRenditionEditorFirstLocalizationSaveStartsAtRevisionTwo(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	if _, err := s.db.Exec(`DELETE FROM song_lyrics_rendition_localizations`); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 1 {
		t.Fatalf("initial revision=%d", current.Revision)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	requested.Renditions[0].Full.Lines[0].Chinese = " 首次本地化\n续行 "
	requested.Renditions[1].TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: "仅署名"}
	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "alice")
	if err != nil || !changed || saved.Revision != 2 {
		t.Fatalf("saved=%+v changed=%v err=%v", saved, changed, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_rendition_localizations WHERE revision=2`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(saved.Renditions) {
		t.Fatalf("revision-two localization rows=%d want=%d", count, len(saved.Renditions))
	}

	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export partial and credit-only localization: %v", err)
	}
	if len(exported.RenditionLocalizations) != 2 || len(exported.RenditionTranslationLines) != 3 {
		t.Fatalf("exported localization shape=%+v lines=%+v", exported.RenditionLocalizations, exported.RenditionTranslationLines)
	}
	if exported.RenditionTranslationLines[0].Text != " 首次本地化\n续行 " || exported.RenditionTranslationLines[1].Text != "" ||
		exported.RenditionTranslationLines[2].Text != "" {
		t.Fatalf("exported partial/empty translations=%+v", exported.RenditionTranslationLines)
	}

	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, exported); err != nil {
		t.Fatalf("restore partial and credit-only localization: %v", err)
	}
	restoredDocument, err := restored.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if restoredDocument.Revision != saved.Revision || restoredDocument.UpdatedAt != saved.UpdatedAt ||
		restoredDocument.Renditions[0].Full.Lines[0].Chinese != " 首次本地化\n续行 " ||
		restoredDocument.Renditions[0].Full.Lines[1].Chinese != "" ||
		restoredDocument.Renditions[1].TranslationCredits == nil ||
		restoredDocument.Renditions[1].TranslationCredits.Translation != "仅署名" {
		t.Fatalf("restored partial/credit-only document=%+v", restoredDocument)
	}
}

func cloneLyricsRenditionEditorDocument(t *testing.T, input LyricsRenditionDocument) LyricsRenditionDocument {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var result LyricsRenditionDocument
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOrdinaryV3EditorReadsOnlyLegacyProvenanceGraph(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	var recoveryItems, recoveryArtifacts, recoveryContributions int
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM lyrics_recovery_import_items),
		(SELECT COUNT(*) FROM lyrics_recovery_import_artifacts),
		(SELECT COUNT(*) FROM lyrics_recovery_import_component_contributions)`).Scan(
		&recoveryItems, &recoveryArtifacts, &recoveryContributions); err != nil {
		t.Fatal(err)
	}
	if recoveryItems != 0 || recoveryArtifacts != 0 || recoveryContributions != 0 {
		t.Fatalf("ordinary v3 fixture unexpectedly owns recovery provenance items=%d artifacts=%d contributions=%d",
			recoveryItems, recoveryArtifacts, recoveryContributions)
	}
	document, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("ordinary v3 editor GET from legacy provenance: %v", err)
	}
	if document.MusicID != 10 || len(document.Renditions) != 2 {
		t.Fatalf("ordinary v3 editor document=%+v", document)
	}
}

type recoveryRenditionV3EditorFixture struct {
	store     *Store
	batchSHA  string
	document  model.LyricsSourceDocument
	artifacts []lyricsstaging.Artifact
}

func TestRecoveryImportedV3EditorReadsRecoveryGraphSavesAndSurvivesBackup(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	var legacyLyrics, legacyArtifacts, legacyEvidenceLinks, legacyEvidence, legacyContributions int
	if err := fixture.store.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics),
		(SELECT COUNT(*) FROM song_lyrics_source_artifacts),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
		(SELECT COUNT(*) FROM lyrics_source_index_evidence),
		(SELECT COUNT(*) FROM song_lyrics_component_contributions)`).Scan(
		&legacyLyrics, &legacyArtifacts, &legacyEvidenceLinks, &legacyEvidence, &legacyContributions); err != nil {
		t.Fatal(err)
	}
	if legacyLyrics != 0 || legacyArtifacts != 0 || legacyEvidenceLinks != 0 || legacyEvidence != 0 || legacyContributions != 0 {
		t.Fatalf("recovery v3 import wrote legacy editable/provenance rows lyrics=%d artifacts=%d evidenceLinks=%d evidence=%d contributions=%d",
			legacyLyrics, legacyArtifacts, legacyEvidenceLinks, legacyEvidence, legacyContributions)
	}

	current, err := fixture.store.GetLyricsDocument(10)
	if err != nil {
		t.Fatalf("recovery v3 editor GET: %v", err)
	}
	plural, ok := current.(LyricsRenditionDocument)
	if !ok || plural.MusicID != 10 || plural.Revision != 1 || len(plural.Renditions) != len(fixture.document.Renditions) {
		t.Fatalf("recovery v3 editor document=%T %+v", current, current)
	}

	requested := cloneLyricsRenditionEditorDocument(t, plural)
	requested.Renditions[0].Full.Lines[0].Chinese = "恢复后的简中"
	requested.Renditions[0].TranslationCredits = &PublicLyricsV3TranslationCredits{
		Translation: "恢复译者", Proofreading: "恢复校对",
	}
	if requested.Renditions[0].Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		fullByID := make(map[string]string, len(requested.Renditions[0].Full.Lines))
		for _, line := range requested.Renditions[0].Full.Lines {
			fullByID[line.ID] = line.Chinese
		}
		for index, lineID := range requested.Renditions[0].Relation.LineIDs {
			requested.Renditions[0].Game.Lines[index].Chinese = fullByID[lineID]
		}
	}
	saved, changed, err := fixture.store.SaveLyricsRenditionMutation(requested, "recovery-editor")
	if err != nil || !changed || saved.Revision != 2 {
		t.Fatalf("recovery v3 editor save saved=%+v changed=%v err=%v", saved, changed, err)
	}
	if saved.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" || saved.Renditions[0].TranslationCredits == nil ||
		saved.Renditions[0].TranslationCredits.Translation != "恢复译者" ||
		saved.Renditions[0].TranslationCredits.Proofreading != "恢复校对" {
		t.Fatalf("recovery v3 editor save lost localization=%+v", saved.Renditions[0])
	}

	reloaded, err := fixture.store.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("recovery v3 editor GET after save: %v", err)
	}
	if reloaded.Revision != 2 || reloaded.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" ||
		reloaded.Renditions[0].TranslationCredits == nil || reloaded.Renditions[0].TranslationCredits.Translation != "恢复译者" {
		t.Fatalf("recovery v3 editor reload=%+v", reloaded)
	}

	candidate, err := fixture.store.RecoveryPublicLyricsV3(fixture.batchSHA)
	if err != nil {
		t.Fatalf("public v3 candidate after editor save: %v", err)
	}
	detail, detailFound := candidate.Details[10]
	if !detailFound || len(detail.Renditions) == 0 || detail.Renditions[0].Full == nil ||
		detail.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" ||
		detail.Renditions[0].TranslationCredits == nil ||
		detail.Renditions[0].TranslationCredits.Translation != "恢复译者" {
		t.Fatalf("public v3 candidate lost editor localization found=%v detail=%+v", detailFound, detail)
	}

	backup, err := fixture.store.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, backup); err != nil {
		t.Fatalf("restore recovery v3 backup: %v", err)
	}
	afterBackup, err := restored.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("recovery v3 editor GET after backup: %v", err)
	}
	if afterBackup.Revision != 2 || afterBackup.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" ||
		afterBackup.Renditions[0].TranslationCredits == nil ||
		afterBackup.Renditions[0].TranslationCredits.Translation != "恢复译者" {
		t.Fatalf("recovery v3 editor after backup=%+v", afterBackup)
	}
	restoredCandidate, err := restored.RecoveryPublicLyricsV3(fixture.batchSHA)
	if err != nil {
		t.Fatalf("public v3 candidate after backup restore: %v", err)
	}
	restoredDetail, restoredFound := restoredCandidate.Details[10]
	if !restoredFound || len(restoredDetail.Renditions) == 0 || restoredDetail.Renditions[0].Full == nil ||
		restoredDetail.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" {
		t.Fatalf("public v3 candidate after backup lost localization found=%v detail=%+v", restoredFound, restoredDetail)
	}
}

func TestRecoveryImportedV3PeerOnlyCreditsReachEditorBackupAndPublicCandidate(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	translations := []lyricsstaging.RenditionTranslation{
		{
			RenditionKey: fixture.document.Renditions[0].RenditionKey,
			PeerTranslations: []lyricsstaging.RenditionPeerTranslation{{
				Side: "game", Locale: "zh-CN", Translations: []string{"仅游戏一", "仅游戏二"},
			}},
			TranslationCredit: "游戏译者", ProofreadingCredit: "游戏校对",
		},
		{RenditionKey: fixture.document.Renditions[1].RenditionKey},
	}
	ctx := context.Background()
	tx, err := fixture.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var documentID int64
	if err := tx.QueryRowContext(ctx, `SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, 10).Scan(&documentID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := insertLyricsRenditionLocalizationsTx(ctx, tx, documentID, fixture.document, translations, "recovery-import", time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC).Unix()); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	loaded, err := fixture.store.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("load recovery peer-only editor document: %v", err)
	}
	rendered := loaded.Renditions[0]
	if rendered.TranslationCredits == nil || rendered.TranslationCredits.Translation != "游戏译者" ||
		rendered.Game == nil || rendered.Game.Lines[0].Chinese != "仅游戏一" ||
		rendered.Full == nil || rendered.Full.Lines[0].Chinese != "" {
		t.Fatalf("recovery peer-only editor rendition=%+v", rendered)
	}
	candidate, err := fixture.store.RecoveryPublicLyricsV3(fixture.batchSHA)
	if err != nil {
		t.Fatalf("build recovery peer-only Public v3 candidate: %v", err)
	}
	public := candidate.Details[10].Renditions[0]
	if public.TranslationCredits == nil || public.TranslationCredits.Translation != "游戏译者" ||
		public.Game == nil || public.Game.Lines[1].Chinese != "仅游戏二" ||
		public.Full == nil || public.Full.Lines[0].Chinese != "" {
		t.Fatalf("recovery peer-only Public v3 rendition=%+v", public)
	}
	backup, err := fixture.store.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export recovery peer-only backup: %v", err)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, backup); err != nil {
		t.Fatalf("restore recovery peer-only backup: %v", err)
	}
	if _, err := restored.RecoveryPublicLyricsV3(fixture.batchSHA); err != nil {
		t.Fatalf("build restored recovery peer-only Public v3 candidate: %v", err)
	}
}

func TestRecoveryRenditionEditorFailsClosedOnOwnershipMixAndGraphDrift(t *testing.T) {
	t.Run("batch does not own document", func(t *testing.T) {
		fixture := setupRecoveryRenditionV3EditorFixture(t)
		if _, err := fixture.store.db.Exec(`DROP TRIGGER lyrics_recovery_import_items_immutable_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.db.Exec(`UPDATE lyrics_recovery_import_items SET document_sha256=? WHERE batch_sha256=? AND music_id=?`,
			strings.Repeat("f", 64), fixture.batchSHA, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.GetLyricsRenditionDocument(10); err == nil || !strings.Contains(err.Error(), "exact document") {
			t.Fatalf("document ownership drift error=%v", err)
		}
	})

	t.Run("fixed identity drift", func(t *testing.T) {
		fixture := setupRecoveryRenditionV3EditorFixture(t)
		if _, err := fixture.store.db.Exec(`DROP TRIGGER lyrics_recovery_import_artifacts_immutable_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.db.Exec(`UPDATE lyrics_recovery_import_artifacts SET fixed_identity_sha256=? WHERE batch_sha256=? AND music_id=? AND rendition_key=?`,
			strings.Repeat("f", 64), fixture.batchSHA, 10, fixture.artifacts[0].Identity.RenditionKey); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.GetLyricsRenditionDocument(10); err == nil || !strings.Contains(err.Error(), "artifact") {
			t.Fatalf("fixed identity drift error=%v", err)
		}
	})

	t.Run("component contribution drift", func(t *testing.T) {
		fixture := setupRecoveryRenditionV3EditorFixture(t)
		if _, err := fixture.store.db.Exec(`DROP TRIGGER lyrics_recovery_import_component_contributions_immutable_update`); err != nil {
			t.Fatal(err)
		}
		var component string
		if err := fixture.store.db.QueryRow(`SELECT component FROM lyrics_recovery_import_component_contributions
			WHERE batch_sha256=? AND music_id=? ORDER BY component LIMIT 1`, fixture.batchSHA, 10).Scan(&component); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.db.Exec(`UPDATE lyrics_recovery_import_component_contributions SET contribution_sha256=?
			WHERE batch_sha256=? AND music_id=? AND component=?`, strings.Repeat("f", 64), fixture.batchSHA, 10, component); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.GetLyricsRenditionDocument(10); err == nil || !strings.Contains(err.Error(), "contribution") {
			t.Fatalf("component contribution drift error=%v", err)
		}
	})

	t.Run("legacy and recovery graphs are never mixed", func(t *testing.T) {
		fixture := setupRecoveryRenditionV3EditorFixture(t)
		identity := fixture.artifacts[0].Identity
		identityJSON, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		categoriesJSON, _ := json.Marshal(identity.Categories)
		evidenceJSON, _ := json.Marshal(identity.IndexEvidenceRefs)
		identitySHA := sha256.Sum256(identityJSON)
		var documentID int64
		if err := fixture.store.db.QueryRow(`SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, 10).Scan(&documentID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.db.Exec(`INSERT INTO song_lyrics_source_artifacts
			(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
			 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
			 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, identity.Provider, identity.RenditionKey,
			identity.Origin, identity.PageID, identity.RevisionID, identity.RevisionTimestamp, identity.SHA1, identity.Title,
			identity.CanonicalURL, identity.FetchedAt, string(categoriesJSON), identity.Section, identity.CompositionRenditionKey,
			identity.VersionReason, string(evidenceJSON), string(identityJSON), hex.EncodeToString(identitySHA[:]), 1,
			strings.Repeat("1", 64), strings.Repeat("2", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.GetLyricsRenditionDocument(10); err == nil || !strings.Contains(err.Error(), "mixes recovery and legacy") {
			t.Fatalf("mixed provenance error=%v", err)
		}
	})
}

func TestLyricsRenditionEditorPerformerUnlockAndSegmentOperations(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}

	requested := cloneLyricsRenditionEditorDocument(t, current)
	// Add new performers to rendition (e.g. 歌唱者-01 一歌, 歌唱者-26 KAITO, 外部歌唱者-01 GUMI)
	newPerformers := []model.LyricsSourcePerformer{
		{PerformerID: "歌唱者-01", Name: "星乃一歌", Color: "#33AAEE"},
		{PerformerID: "歌唱者-26", Name: "KAITO", Color: "#3366CC"},
		{PerformerID: "外部歌唱者-01", Name: "GUMI", Color: "#70B85A"},
	}
	requested.Renditions[0].Performers = append(requested.Renditions[0].Performers, newPerformers...)

	// Assign performers to segments and trailing performers
	requested.Renditions[0].Full.Lines[0].TrailingPerformerIDs = []string{"歌唱者-26"}
	requested.Renditions[0].Full.Lines[0].Segments[0].PerformerIDs = []string{"歌唱者-21", "歌唱者-26"}
	requested.Renditions[0].Full.Lines[0].Segments[1].PerformerIDs = []string{"歌唱者-01", "外部歌唱者-01"}

	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "admin")
	if err != nil {
		t.Fatalf("save performer update: %v", err)
	}
	if !changed || saved.Revision != 2 {
		t.Fatalf("saved revision=%d changed=%v", saved.Revision, changed)
	}

	// Verify performers survived and persisted in read-back
	readBack, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(readBack.Renditions[0].Full.Lines[0].TrailingPerformerIDs) != 1 ||
		readBack.Renditions[0].Full.Lines[0].TrailingPerformerIDs[0] != "歌唱者-26" {
		t.Fatalf("trailing performers=%v", readBack.Renditions[0].Full.Lines[0].TrailingPerformerIDs)
	}
	if !reflect.DeepEqual(readBack.Renditions[0].Full.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21", "歌唱者-26"}) {
		t.Fatalf("segment 0 performers=%v", readBack.Renditions[0].Full.Lines[0].Segments[0].PerformerIDs)
	}
	if !reflect.DeepEqual(readBack.Renditions[0].Full.Lines[0].Segments[1].PerformerIDs, []string{"歌唱者-01", "外部歌唱者-01"}) {
		t.Fatalf("segment 1 performers=%v", readBack.Renditions[0].Full.Lines[0].Segments[1].PerformerIDs)
	}
}

func setupRecoveryRenditionV3EditorFixture(t *testing.T) recoveryRenditionV3EditorFixture {
	t.Helper()
	s := setupLyricsStore(t)
	document, evidenceByIdentity := renditionV3PersistenceDocument(t)
	artifacts := make([]lyricsstaging.Artifact, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		artifact, err := lyricsstaging.NewRecoveryArtifact(identity, evidenceByIdentity[index][0].Raw)
		if err != nil {
			t.Fatalf("build recovery v3 artifact %q: %v", identity.RenditionKey, err)
		}
		artifacts[index] = artifact
	}
	fingerprint := recoveryRenditionTestCatalogFingerprint(t, s, 10)
	draft, err := lyricsstaging.BuildRecoveryPeerDraft(10, "新曲", fingerprint, 10, []int{}, document, artifacts, nil)
	if err != nil {
		t.Fatalf("build recovery v3 draft: %v", err)
	}
	batchSHA := recoveryEditorTestSHA("batch")
	createdAt := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC).Unix()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	musicIDsSHA, err := lyricsrootmanifest.OrderedMusicIDsSHA256([]int{10, 20})
	if err != nil {
		t.Fatal(rollback(err))
	}
	refs := make([]lyricsevidencepack.EvidenceRef, 0, len(evidenceByIdentity))
	seenEvidence := make(map[string]struct{})
	var rawByteCount int64
	evidenceOrdinal := 0
	for _, parents := range evidenceByIdentity {
		for _, parent := range parents {
			if _, duplicate := seenEvidence[parent.EvidenceID]; duplicate {
				continue
			}
			seenEvidence[parent.EvidenceID] = struct{}{}
			ref := lyricsevidencepack.EvidenceRef{
				Provider: parent.Provider, AcquisitionID: recoveryEditorTestSHA(fmt.Sprintf("acquisition-%d", evidenceOrdinal)),
				EvidenceID: parent.EvidenceID, SHA256: parent.RawSHA256,
				EnvelopeSHA256: recoveryEditorTestSHA(fmt.Sprintf("envelope-%d", evidenceOrdinal)),
			}
			refs = append(refs, ref)
			evidenceOrdinal++
			rawByteCount += int64(len(parent.Raw))
		}
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].EvidenceID < refs[right].EvidenceID })
	selectionSHA, err := lyricsevidencepack.OrderedSelectionSHA256(refs)
	if err != nil {
		t.Fatal(rollback(err))
	}
	coverage := lyricsrootmanifest.Coverage{
		Total: 2, Complete: 1, Missing: 1, SelectionRefCount: len(refs),
		UniqueAcquisitionCount: len(refs), UniqueEvidenceCount: len(refs),
	}
	coverageJSON, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(rollback(err))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_batches
		(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
		 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
		 raw_byte_count,encoded_byte_count,actor,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		batchSHA, 1, lyricsrootmanifest.SchemaVersionV2, "recovery-editor-root", recoveryEditorTestSHA("root"), 2,
		musicIDsSHA, string(coverageJSON), recoveryEditorTestSHA("receipt"), recoveryEditorTestSHA("pack"), selectionSHA,
		len(refs), 1, rawByteCount, 1, "recovery-editor-test", createdAt); err != nil {
		t.Fatal(rollback(err))
	}
	for index, parents := range evidenceByIdentity {
		for _, parent := range parents {
			ref := refs[0]
			for _, candidate := range refs {
				if candidate.EvidenceID == parent.EvidenceID {
					ref = candidate
					break
				}
			}
			if err := seedRecoveryEvidenceTx(ctx, tx, ref, parent, createdAt); err != nil {
				t.Fatalf("insert recovery evidence %d: %v", index, rollback(err))
			}
		}
	}
	item := lyricsrecoveryimport.Item{
		MusicID: 10, JapaneseTitle: "新曲", CatalogFingerprint: fingerprint, TargetMusicID: 10,
		AssociationMusicIDs: []int{}, State: lyricsrootmanifest.CoverageComplete,
		ResultSHA256: recoveryEditorTestSHA("result-10"), Draft: &draft,
	}
	if err := seedRecoveryImportItemTx(ctx, tx, batchSHA, item, createdAt); err != nil {
		t.Fatal(rollback(err))
	}
	if err := seedRecoveryV3DraftItemTx(ctx, tx, batchSHA, item, createdAt); err != nil {
		t.Fatal(rollback(err))
	}
	if err := seedRecoveryProvenanceGraphTx(ctx, tx, batchSHA, item, createdAt); err != nil {
		t.Fatal(rollback(err))
	}

	missingAvailability := model.LyricsAvailabilityDocument{
		SchemaVersion:   model.LyricsAvailabilityDocumentSchemaVersion,
		State:           model.LyricsAvailabilityStateMissing,
		ReasonCode:      model.LyricsSourceVersionReasonVersionConflict,
		FixedIdentities: []model.LyricsSourceFixedIdentity{},
	}
	missingSHA, err := lyricsrecoveryimport.AvailabilityDocumentSHA256(missingAvailability)
	if err != nil {
		t.Fatal(rollback(err))
	}
	missingBody, err := json.Marshal(missingAvailability)
	if err != nil {
		t.Fatal(rollback(err))
	}
	missingItem := lyricsrecoveryimport.Item{
		MusicID: 20, JapaneseTitle: "旧曲", CatalogFingerprint: recoveryRenditionTestCatalogFingerprint(t, s, 20), TargetMusicID: 20,
		AssociationMusicIDs: []int{}, State: lyricsrootmanifest.CoverageMissing,
		ResultSHA256: recoveryEditorTestSHA("result-20"), Availability: &missingAvailability,
		AvailabilityDocumentSHA256: missingSHA,
	}
	if err := seedRecoveryImportItemTx(ctx, tx, batchSHA, missingItem, createdAt); err != nil {
		t.Fatal(rollback(err))
	}
	if err := seedRecoveryAvailabilityItemTx(ctx, tx, batchSHA, missingItem, string(missingBody), createdAt); err != nil {
		t.Fatal(rollback(err))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return recoveryRenditionV3EditorFixture{store: s, batchSHA: batchSHA, document: document, artifacts: artifacts}
}

func recoveryRenditionTestCatalogFingerprint(t *testing.T, s *Store, musicID int) string {
	t.Helper()
	var fingerprint string
	if err := s.db.QueryRow(`SELECT lyrics_catalog_fingerprint FROM catalog_music WHERE music_id=?`, musicID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func recoveryEditorTestSHA(value string) string {
	digest := sha256.Sum256([]byte("recovery-editor-v3-test:" + value))
	return hex.EncodeToString(digest[:])
}
