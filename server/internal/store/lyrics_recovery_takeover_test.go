package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

// recoveryLedgerRowDigests hashes the sorted, type-tagged rows of every
// recovery ledger table and of the availability documents.
func recoveryLedgerRowDigests(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'
		AND ((name LIKE 'lyrics_recovery_%' AND name<>'lyrics_recovery_takeovers')
		 OR name='song_lyrics_availability_documents') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) != 7 {
		t.Fatalf("ledger tables=%v", tables)
	}
	digests := make(map[string]string, len(tables))
	for _, table := range tables {
		tableRows, err := s.db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := tableRows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for tableRows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := tableRows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprintf("%#v", values))
		}
		if err := tableRows.Err(); err != nil {
			t.Fatal(err)
		}
		tableRows.Close()
		sort.Strings(lines)
		digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		digests[table] = fmt.Sprintf("%d:%x", len(lines), digest)
	}
	return digests
}

func requireRecoveryLedgerUnchanged(t *testing.T, s *Store, before map[string]string) {
	t.Helper()
	after := recoveryLedgerRowDigests(t, s)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("recovery ledger rows changed\nbefore=%v\nafter=%v", before, after)
	}
}

func recoveryTakeoverRequest(musicID int) LyricsDocumentRequest {
	request := lyricsDocumentTestRequest()
	request.MusicID = musicID
	// Source documents are unique by digest across songs.
	request.Source.URL = fmt.Sprintf("https://www.sekaipedia.org/wiki/%%E5%%90%%88%%E6%%88%%90?oldid=%d", 7000+musicID)
	request.Renditions[0].PerformerIDs = []int{}
	return request
}

// recoveryTakeoverExpectedRevision is the expectedRevision a publish of
// request needs now: the revision its dry run without one reports, or nil for
// a song with nothing stored or served.
func recoveryTakeoverExpectedRevision(t *testing.T, s *Store, request LyricsDocumentRequest) *int {
	t.Helper()
	request.DryRun, request.ExpectedRevision = true, nil
	_, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin")
	if err == nil {
		return nil
	}
	var documentErr *LyricsDocumentError
	if !errors.As(err, &documentErr) || documentErr.Code != LyricsDocumentErrorRevisionRequired {
		t.Fatalf("dry run without expectedRevision: %v", err)
	}
	current, ok := documentErr.Current.(map[string]int)
	if !ok {
		t.Fatalf("revision-required error carries current=%#v", documentErr.Current)
	}
	revision := current["revision"]
	return &revision
}

// takeOverRecoverySongForTest publishes recoveryTakeoverRequest(musicID) with
// the expectedRevision the song requires.
func takeOverRecoverySongForTest(t *testing.T, s *Store, musicID int) (LyricsDocumentResult, PublicLyricsV3DetailDocument) {
	t.Helper()
	request := recoveryTakeoverRequest(musicID)
	request.ExpectedRevision = recoveryTakeoverExpectedRevision(t, s, request)
	return publishLyricsDocumentForTest(t, s, request, 0)
}

type recoveryTakeoverRow struct {
	batchSHA, state, documentJSON, documentSHA, takenOverBy string
	schemaVersion, documentCreatedAt                        int64
	superseded                                              bool
	takenOverAt                                             int64
}

func recoveryTakeoverRowsForTest(t *testing.T, s *Store) map[int]recoveryTakeoverRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT music_id,batch_sha256,item_state,document_json IS NOT NULL,
		COALESCE(schema_version,0),COALESCE(document_json,''),COALESCE(document_sha256,''),
		COALESCE(document_created_at,0),taken_over_at,taken_over_by FROM lyrics_recovery_takeovers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[int]recoveryTakeoverRow{}
	for rows.Next() {
		var musicID int
		var row recoveryTakeoverRow
		if err := rows.Scan(&musicID, &row.batchSHA, &row.state, &row.superseded, &row.schemaVersion,
			&row.documentJSON, &row.documentSHA, &row.documentCreatedAt, &row.takenOverAt, &row.takenOverBy); err != nil {
			t.Fatal(err)
		}
		result[musicID] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func catalogMusicItemForTest(t *testing.T, s *Store, musicID int) (lyricsStatus, availabilityState string) {
	t.Helper()
	response, err := s.CatalogMusic("", false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range response.Items {
		if item.MusicID == musicID {
			return item.LyricsStatus, item.LyricsAvailabilityState
		}
	}
	t.Fatalf("catalog lacks music %d", musicID)
	return "", ""
}

func requireProjectionServes(t *testing.T, s *Store, musicID int, result LyricsDocumentResult) {
	t.Helper()
	index, details, _, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	served, ok := details[musicID]
	if !ok {
		t.Fatalf("projection does not serve music %d", musicID)
	}
	servedBody, err := EncodePublicLyricsV3Detail(served)
	if err != nil {
		t.Fatal(err)
	}
	if served.Revision != result.Revision || !bytes.Equal(servedBody, result.Document) {
		t.Fatalf("served music %d revision=%d differs from the publish response\nserved=%s\nresponse=%s",
			musicID, served.Revision, servedBody, result.Document)
	}
	for _, song := range index {
		if song.MusicID == musicID {
			if song.State != PublicLyricsStateComplete || song.Revision != result.Revision {
				t.Fatalf("index entry=%+v", song)
			}
			return
		}
	}
	t.Fatalf("projection index lacks music %d", musicID)
}

func TestPublishLyricsDocumentTakesOverRecoveryLedgerSongs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		musicID    int
		state      string
		superseded bool
	}{
		{"complete item with its source document", 10, "complete", true},
		{"missing item with only an availability document", 20, "missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupRecoveryRenditionV3EditorFixture(t)
			s := fixture.store
			ledger := recoveryLedgerRowDigests(t, s)
			var recoveryDocumentJSON, recoveryDocumentSHA string
			var recoveryDocumentCreatedAt int64
			if tc.superseded {
				if err := s.db.QueryRow(`SELECT document_json,document_sha256,created_at FROM song_lyrics_source_documents
					WHERE music_id=? AND manifest_batch_sha256=?`, tc.musicID, fixture.batchSHA).Scan(
					&recoveryDocumentJSON, &recoveryDocumentSHA, &recoveryDocumentCreatedAt); err != nil {
					t.Fatal(err)
				}
			} else if _, availability := catalogMusicItemForTest(t, s, tc.musicID); availability != "missing" {
				t.Fatalf("fixture availability=%q", availability)
			}

			before := lyricsDocumentTableCounts(t, s)
			request := recoveryTakeoverRequest(tc.musicID)
			request.ExpectedRevision = recoveryTakeoverExpectedRevision(t, s, request)
			request.DryRun = true
			dryRun, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin")
			if err != nil || !dryRun.DryRun {
				t.Fatalf("dry run=%+v err=%v", dryRun, err)
			}
			if after := lyricsDocumentTableCounts(t, s); !reflect.DeepEqual(after, before) {
				t.Fatal("dry run changed the database")
			}

			request.DryRun = false
			first, _ := publishLyricsDocumentForTest(t, s, request, 0)
			if first.Revision != dryRun.Revision {
				t.Fatalf("publish revision=%d dry run revision=%d", first.Revision, dryRun.Revision)
			}
			takeovers := recoveryTakeoverRowsForTest(t, s)
			takeover, ok := takeovers[tc.musicID]
			if len(takeovers) != 1 || !ok || takeover.batchSHA != fixture.batchSHA || takeover.state != tc.state ||
				takeover.superseded != tc.superseded || takeover.takenOverBy != "document-admin" || takeover.takenOverAt <= 0 {
				t.Fatalf("takeovers=%+v", takeovers)
			}
			if tc.superseded && (takeover.documentJSON != recoveryDocumentJSON || takeover.documentSHA != recoveryDocumentSHA ||
				takeover.documentCreatedAt != recoveryDocumentCreatedAt || takeover.schemaVersion != 3) {
				t.Fatal("takeover does not keep the superseded recovery document verbatim")
			}
			requireRecoveryLedgerUnchanged(t, s, ledger)
			requireProjectionServes(t, s, tc.musicID, first)
			if status, availability := catalogMusicItemForTest(t, s, tc.musicID); status != "draft" || availability != "" {
				t.Fatalf("catalog status=%q availability=%q after takeover", status, availability)
			}

			document, err := s.GetLyricsRenditionDocument(tc.musicID)
			if err != nil || document.Revision != first.Revision {
				t.Fatalf("editor document revision=%d err=%v", document.Revision, err)
			}
			document.Renditions[0].Full.Lines[0].Chinese = "接管后在编辑器里改过的译文"
			saved, changed, err := s.SaveLyricsRenditionMutation(document, "console-editor")
			if err != nil || !changed || saved.Revision != first.Revision+1 {
				t.Fatalf("editor save revision=%d changed=%t err=%v", saved.Revision, changed, err)
			}

			request.Renditions[0].Lines[0].Chinese = "第二次整曲发布的译文"
			request.ExpectedRevision = &saved.Revision
			second, detail := publishLyricsDocumentForTest(t, s, request, 0)
			if second.Revision != saved.Revision+1 || detail.Renditions[0].Full.Lines[0].Chinese != "第二次整曲发布的译文" {
				t.Fatalf("second publish=%+v", second)
			}
			if again := recoveryTakeoverRowsForTest(t, s); !reflect.DeepEqual(again, takeovers) {
				t.Fatalf("second publish changed the takeover: before=%+v after=%+v", takeovers, again)
			}
			requireRecoveryLedgerUnchanged(t, s, ledger)
			requireProjectionServes(t, s, tc.musicID, second)
		})
	}
}

// restoreLyricsBackupIntoFreshStore restores the JSON form of content into a
// new database through the full content restore.
func restoreLyricsBackupIntoFreshStore(t *testing.T, content LyricsContentExport) (*Store, error) {
	t.Helper()
	body, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LyricsContentExport
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "takeover-destination.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	destination := New(database)
	return destination, destination.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
		EventContentExport{}, decoded, true, "operator")
}

func takenOverRecoveryFixture(t *testing.T) (*Store, map[int]LyricsDocumentResult) {
	t.Helper()
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	results := map[int]LyricsDocumentResult{}
	for _, musicID := range []int{10, 20} {
		results[musicID], _ = takeOverRecoverySongForTest(t, fixture.store, musicID)
	}
	return fixture.store, results
}

func TestRecoveryTakeoverBackupRoundTripsAndStillOpensInTheEditor(t *testing.T) {
	s, results := takenOverRecoveryFixture(t)
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.RecoveryTakeovers) != 2 || exported.RecoveryTakeovers[0].MusicID != 10 ||
		exported.RecoveryTakeovers[0].SupersededDocument == nil || exported.RecoveryTakeovers[1].SupersededDocument != nil {
		t.Fatalf("exported takeovers=%+v", exported.RecoveryTakeovers)
	}
	destination := restoreSeededContentBackup(t, exported)
	requireRecoveryLedgerUnchanged(t, destination, recoveryLedgerRowDigests(t, s))
	if restored := recoveryTakeoverRowsForTest(t, destination); !reflect.DeepEqual(restored, recoveryTakeoverRowsForTest(t, s)) {
		t.Fatalf("restored takeovers=%+v", restored)
	}
	for musicID, result := range results {
		document, err := destination.GetLyricsRenditionDocument(musicID)
		if err != nil || document.Revision != result.Revision {
			t.Fatalf("restored music %d revision=%d err=%v", musicID, document.Revision, err)
		}
		document.Renditions[0].Full.Lines[0].Chinese = "恢复后在编辑器里改过的译文"
		saved, changed, err := destination.SaveLyricsRenditionMutation(document, "console-editor")
		if err != nil || !changed || saved.Revision != result.Revision+1 {
			t.Fatalf("restored music %d save revision=%d changed=%t err=%v", musicID, saved.Revision, changed, err)
		}
		requireProjectionServes(t, destination, musicID, lyricsDocumentResultForEditorRevision(t, destination, musicID))
	}
}

// lyricsDocumentResultForEditorRevision encodes the editor document as the
// public detail the projection must serve.
func lyricsDocumentResultForEditorRevision(t *testing.T, s *Store, musicID int) LyricsDocumentResult {
	t.Helper()
	document, err := s.GetLyricsRenditionDocument(musicID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodePublicLyricsV3Detail(PublicLyricsV3DetailDocument{
		Version: 3, MusicID: musicID, Revision: document.Revision, UpdatedAt: document.UpdatedAt,
		State: PublicLyricsStateComplete, Renditions: document.Renditions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return LyricsDocumentResult{MusicID: musicID, Revision: document.Revision, Document: body}
}

func TestPreTakeoverBackupRestoresAndItsSongsCanBeTakenOver(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	exported, err := fixture.store.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["recoveryTakeovers"]; present || len(exported.RecoveryItems) != 2 {
		t.Fatalf("a backup without takeovers must look like a v37 backup; items=%d", len(exported.RecoveryItems))
	}
	destination := restoreSeededContentBackup(t, exported)
	ledger := recoveryLedgerRowDigests(t, destination)
	for _, musicID := range []int{10, 20} {
		result, _ := takeOverRecoverySongForTest(t, destination, musicID)
		requireProjectionServes(t, destination, musicID, result)
	}
	if takeovers := recoveryTakeoverRowsForTest(t, destination); len(takeovers) != 2 {
		t.Fatalf("takeovers after restoring a v37 backup=%+v", takeovers)
	}
	requireRecoveryLedgerUnchanged(t, destination, ledger)
}

func TestRecoveryTakeoverRestoreRejectsTakeoversThatDoNotMatchTheLedger(t *testing.T) {
	s, _ := takenOverRecoveryFixture(t)
	valid, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		tamper func(*LyricsContentExport)
	}{
		{"superseded digest differs from the item", func(content *LyricsContentExport) {
			content.RecoveryTakeovers[0].SupersededDocument.DocumentSHA256 = strings.Repeat("0", 64)
		}},
		{"superseded document bytes differ from their digest", func(content *LyricsContentExport) {
			document := content.RecoveryTakeovers[0].SupersededDocument
			document.DocumentJSON = strings.Replace(document.DocumentJSON, `"schemaVersion":3`, `"schemaVersion":3 `, 1)
		}},
		{"complete item loses its superseded document", func(content *LyricsContentExport) {
			content.RecoveryTakeovers[0].SupersededDocument = nil
		}},
		{"availability item gains a document", func(content *LyricsContentExport) {
			content.RecoveryTakeovers[1].SupersededDocument = content.RecoveryTakeovers[0].SupersededDocument
		}},
		{"no ledger item", func(content *LyricsContentExport) {
			content.RecoveryTakeovers[1].BatchSHA256 = strings.Repeat("1", 64)
		}},
		{"item state differs", func(content *LyricsContentExport) {
			content.RecoveryTakeovers[1].ItemState = "failed"
		}},
		{"duplicated song", func(content *LyricsContentExport) {
			content.RecoveryTakeovers = append(content.RecoveryTakeovers, content.RecoveryTakeovers[1])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := cloneLyricsContentExport(t, valid)
			tc.tamper(&tampered)
			destination, err := restoreLyricsBackupIntoFreshStore(t, tampered)
			if err == nil || !strings.Contains(err.Error(), "lyrics recovery takeover") {
				t.Fatalf("restore error=%v", err)
			}
			if counts := lyricsDocumentTableCounts(t, destination); counts["lyrics_recovery_takeovers"] != 0 ||
				counts["lyrics_recovery_import_items"] != 0 || counts["song_lyrics_source_documents"] != 0 {
				t.Fatalf("rejected restore wrote rows: %v", counts)
			}
		})
	}
	if _, err := restoreLyricsBackupIntoFreshStore(t, valid); err != nil {
		t.Fatalf("untampered restore: %v", err)
	}
}

// recoveryLedgerDocumentForTest returns music 10's recovery source document.
func recoveryLedgerDocumentForTest(t *testing.T, fixture recoveryRenditionV3EditorFixture) (int64, model.LyricsSourceDocument, int64) {
	t.Helper()
	var documentID, createdAt int64
	var documentJSON string
	if err := fixture.store.db.QueryRow(`SELECT document_id,document_json,created_at FROM song_lyrics_source_documents
		WHERE music_id=10 AND manifest_batch_sha256=?`, fixture.batchSHA).Scan(&documentID, &documentJSON, &createdAt); err != nil {
		t.Fatal(err)
	}
	document, err := model.DecodeLyricsSourceDocument([]byte(documentJSON))
	if err != nil {
		t.Fatal(err)
	}
	return documentID, document, createdAt
}

// seedRecoveryLedgerTranslations writes music 10's zh translations, peer Game
// translations and credits as revision-1 localizations, the way the offline
// recovery import stores a draft's RenditionTranslations. withEdition then
// adds a translation edition through the editor.
func seedRecoveryLedgerTranslations(t *testing.T, fixture recoveryRenditionV3EditorFixture, withEdition bool) {
	t.Helper()
	s := fixture.store
	documentID, document, createdAt := recoveryLedgerDocumentForTest(t, fixture)
	translations := make([]lyricscontract.RenditionTranslation, len(document.Renditions))
	peers := 0
	for index, rendition := range document.Renditions {
		item := lyricscontract.RenditionTranslation{
			RenditionKey: rendition.RenditionKey, TranslationCredit: "账本译者", ProofreadingCredit: "账本校对",
		}
		for line := 0; line < renditionLineCountForStore(rendition); line++ {
			item.Translations = append(item.Translations, fmt.Sprintf("账本译文%s-%d", rendition.RenditionKey, line))
		}
		if side, independent := renditionPeerTranslationSide(document, rendition.RenditionKey, "game"); independent {
			peer := lyricscontract.RenditionPeerTranslation{Side: "game", Locale: "zh-CN"}
			for line := range side.Lines {
				peer.Translations = append(peer.Translations, fmt.Sprintf("账本游戏版译文%d", line))
			}
			item.PeerTranslations = []lyricscontract.RenditionPeerTranslation{peer}
			peers++
		}
		translations[index] = item
	}
	if peers == 0 {
		t.Fatal("fixture has no independent Game side to carry a peer translation")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertLyricsRenditionLocalizationsTx(context.Background(), tx, documentID, document, translations,
		"recovery-import", createdAt); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.invalidateLocalizationProjectionCache()
	if !withEdition {
		return
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: current.Revision, Operation: "create", EditionKey: "alt", Label: "备选译本",
	}, "console-editor"); err != nil {
		t.Fatalf("create translation edition on the recovery document: %v", err)
	}
}

// documentRowsForTest keeps the rows of one document from a content export.
func documentRowsForTest(content LyricsContentExport, documentID int64) LyricsContentExport {
	var rows LyricsContentExport
	for _, row := range content.RenditionLocalizations {
		if row.DocumentID == documentID {
			rows.RenditionLocalizations = append(rows.RenditionLocalizations, row)
		}
	}
	for _, row := range content.RenditionTranslationLines {
		if row.DocumentID == documentID {
			rows.RenditionTranslationLines = append(rows.RenditionTranslationLines, row)
		}
	}
	for _, row := range content.TranslationEditionStates {
		if row.DocumentID == documentID {
			rows.TranslationEditionStates = append(rows.TranslationEditionStates, row)
		}
	}
	for _, row := range content.TranslationEditions {
		if row.DocumentID == documentID {
			rows.TranslationEditions = append(rows.TranslationEditions, row)
		}
	}
	for _, row := range content.TranslationEditionLocalizations {
		if row.DocumentID == documentID {
			rows.TranslationEditionLocalizations = append(rows.TranslationEditionLocalizations, row)
		}
	}
	for _, row := range content.TranslationEditionLines {
		if row.DocumentID == documentID {
			rows.TranslationEditionLines = append(rows.TranslationEditionLines, row)
		}
	}
	return rows
}

func takeoverLocalizationsJSONForTest(t *testing.T, s *Store, musicID int) sql.NullString {
	t.Helper()
	var body sql.NullString
	if err := s.db.QueryRow(`SELECT localizations_json FROM lyrics_recovery_takeovers WHERE music_id=?`, musicID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// ledgerLocalizationsWithEditionForTest seeds a fixture whose recovery
// document also owns a translation edition and captures its rows the way
// takeOverLyricsRecoverySongTx does, without the publish around it. Every
// setupRecoveryRenditionV3EditorFixture stores the same recovery document.
func ledgerLocalizationsWithEditionForTest(t *testing.T) (recoveryRenditionV3EditorFixture, string) {
	t.Helper()
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	seedRecoveryLedgerTranslations(t, fixture, true)
	documentID, _, _ := recoveryLedgerDocumentForTest(t, fixture)
	tx, err := fixture.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var documentJSON string
	if err := tx.QueryRow(`SELECT document_json FROM song_lyrics_source_documents WHERE document_id=?`, documentID).Scan(&documentJSON); err != nil {
		t.Fatal(err)
	}
	body, err := supersededLyricsRecoveryLocalizationsTx(context.Background(), tx, documentID, documentJSON)
	if err != nil || !body.Valid {
		t.Fatalf("capture ledger localizations with an edition: valid=%t err=%v", body.Valid, err)
	}
	return fixture, body.String
}

func TestRecoveryTakeoverKeepsTheSupersededLocalizations(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	seedRecoveryLedgerTranslations(t, fixture, false)
	documentID, _, _ := recoveryLedgerDocumentForTest(t, fixture)
	want := documentRowsForTest(mustExportLyricsContent(t, s), documentID)
	sides := map[string]int{}
	for _, line := range want.RenditionTranslationLines {
		sides[line.Side]++
	}
	if len(want.RenditionLocalizations) == 0 || sides[""] == 0 || sides["game"] == 0 {
		t.Fatalf("fixture rows before the takeover: %+v", want)
	}

	for _, musicID := range []int{10, 20} {
		takeOverRecoverySongForTest(t, s, musicID)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_rendition_localizations WHERE document_id=?`,
		documentID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("superseded document rows left=%d err=%v", remaining, err)
	}
	body := takeoverLocalizationsJSONForTest(t, s, 10)
	if !body.Valid {
		t.Fatal("takeover dropped the superseded localizations")
	}
	if availability := takeoverLocalizationsJSONForTest(t, s, 20); availability.Valid {
		t.Fatalf("availability-only takeover localizations=%q", availability.String)
	}
	kept, err := decodeLyricsRecoverySupersededLocalizations(body.String)
	if err != nil {
		t.Fatal(err)
	}
	if got := kept.contentRows(documentID); !reflect.DeepEqual(got, want) {
		t.Fatalf("kept rows differ from the exported rows before the takeover\nkept=%+v\nwant=%+v", got, want)
	}
}

func TestSupersededRecoveryLocalizationsCaptureTranslationEditions(t *testing.T) {
	fixture, body := ledgerLocalizationsWithEditionForTest(t)
	documentID, document, _ := recoveryLedgerDocumentForTest(t, fixture)
	want := documentRowsForTest(mustExportLyricsContent(t, fixture.store), documentID)
	if len(want.TranslationEditionStates) != 1 || len(want.TranslationEditions) != 2 ||
		len(want.TranslationEditionLocalizations) == 0 || len(want.TranslationEditionLines) == 0 {
		t.Fatalf("fixture edition rows: %+v", want)
	}
	kept, err := decodeLyricsRecoverySupersededLocalizations(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := kept.contentRows(documentID); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured rows differ from the exported rows\nkept=%+v\nwant=%+v", got, want)
	}
	if err := validateLyricsRecoverySupersededLocalizations(body, document); err != nil {
		t.Fatalf("captured rows do not validate: %v", err)
	}
}

func TestRecoveryTakeoverOfAnUntranslatedRecoveryDocumentKeepsNoLocalizations(t *testing.T) {
	s, _ := takenOverRecoveryFixture(t)
	for _, musicID := range []int{10, 20} {
		if body := takeoverLocalizationsJSONForTest(t, s, musicID); body.Valid {
			t.Fatalf("music %d localizations=%q", musicID, body.String)
		}
	}
}

func TestRecoveryTakeoverLocalizationsRoundTripThroughBackupsByteForByte(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	source := fixture.store
	seedRecoveryLedgerTranslations(t, fixture, false)
	// A backup taken before any takeover restores, and the restored song keeps
	// the same rows when it is taken over there.
	preTakeover := restoreSeededContentBackup(t, mustExportLyricsContent(t, source))
	for _, s := range []*Store{source, preTakeover} {
		for _, musicID := range []int{10, 20} {
			takeOverRecoverySongForTest(t, s, musicID)
		}
	}
	body := takeoverLocalizationsJSONForTest(t, source, 10)
	if !body.Valid || takeoverLocalizationsJSONForTest(t, preTakeover, 10) != body {
		t.Fatalf("localizations after a pre-takeover restore differ: %q", body.String)
	}
	exported := mustExportLyricsContent(t, source)
	if document := exported.RecoveryTakeovers[0].SupersededDocument; document == nil || document.LocalizationsJSON != body.String {
		t.Fatalf("exported takeover=%+v", exported.RecoveryTakeovers[0])
	}
	_, withEdition := ledgerLocalizationsWithEditionForTest(t)
	editionPayload := cloneLyricsContentExport(t, exported)
	editionPayload.RecoveryTakeovers[0].SupersededDocument.LocalizationsJSON = withEdition
	for name, payload := range map[string]LyricsContentExport{"translations": exported, "translation edition": editionPayload} {
		want, err := json.Marshal(payload.RecoveryTakeovers)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(mustExportLyricsContent(t, restoreSeededContentBackup(t, payload)).RecoveryTakeovers)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s: restored takeovers export differently\ngot=%s\nwant=%s", name, got, want)
		}
	}
}

func TestRecoveryTakeoverRestoreRejectsTamperedLocalizations(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	seedRecoveryLedgerTranslations(t, fixture, false)
	for _, musicID := range []int{10, 20} {
		takeOverRecoverySongForTest(t, fixture.store, musicID)
	}
	_, body := ledgerLocalizationsWithEditionForTest(t)
	valid := mustExportLyricsContent(t, fixture.store)
	valid.RecoveryTakeovers[0].SupersededDocument.LocalizationsJSON = body
	rewrite := func(mutate func(*lyricsRecoverySupersededLocalizations)) func(*LyricsContentExport) {
		return func(content *LyricsContentExport) {
			value, err := decodeLyricsRecoverySupersededLocalizations(body)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&value)
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			content.RecoveryTakeovers[0].SupersededDocument.LocalizationsJSON = string(encoded)
		}
	}
	replace := func(old, new string) func(*LyricsContentExport) {
		return func(content *LyricsContentExport) {
			if !strings.Contains(body, old) {
				t.Fatalf("localizations lack %q", old)
			}
			content.RecoveryTakeovers[0].SupersededDocument.LocalizationsJSON = strings.Replace(body, old, new, 1)
		}
	}
	for _, tc := range []struct {
		name   string
		tamper func(*LyricsContentExport)
	}{
		{"not canonical JSON", replace(`{"localizations":[`, `{"localizations": [`)},
		{"unknown member", replace(`{"localizations":[`, `{"documentId":1,"localizations":[`)},
		{"no localization rows", func(content *LyricsContentExport) {
			content.RecoveryTakeovers[0].SupersededDocument.LocalizationsJSON = `{"localizations":[]}`
		}},
		{"rows out of primary-key order", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			lines := value.TranslationLines
			lines[0], lines[1] = lines[1], lines[0]
		})},
		{"rendition outside the superseded document", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.Localizations[len(value.Localizations)-1].RenditionKey = "zz-unknown"
		})},
		{"incomplete translation lines", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.TranslationLines = value.TranslationLines[:len(value.TranslationLines)-1]
		})},
		{"incomplete peer translation lines", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.SideTranslationLines = value.SideTranslationLines[1:]
		})},
		{"inconsistent revision", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.Localizations[0].Revision++
		})},
		{"locale other than zh-CN", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.Localizations[0].Locale = "en-US"
		})},
		{"edition state differs from its mirror", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.EditionState.UpdatedBy = "someone-else"
		})},
		{"edition without its lines", rewrite(func(value *lyricsRecoverySupersededLocalizations) {
			value.EditionLines = nil
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := cloneLyricsContentExport(t, valid)
			tc.tamper(&tampered)
			destination, err := restoreLyricsBackupIntoFreshStore(t, tampered)
			if err == nil || !strings.Contains(err.Error(), "lyrics recovery takeover") {
				t.Fatalf("restore error=%v", err)
			}
			if counts := lyricsDocumentTableCounts(t, destination); counts["lyrics_recovery_takeovers"] != 0 ||
				counts["song_lyrics_source_documents"] != 0 {
				t.Fatalf("rejected restore wrote rows: %v", counts)
			}
		})
	}
	if _, err := restoreLyricsBackupIntoFreshStore(t, valid); err != nil {
		t.Fatalf("untampered restore: %v", err)
	}
}
