package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsperformers"
)

func TestPublishLyricsDocumentRecordsNoRecoveryTakeoverWhenThePublishIsRefused(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	before := lyricsDocumentTableCounts(t, s)
	ledger := recoveryLedgerRowDigests(t, s)
	for _, musicID := range []int{10, 20} {
		stale := recoveryTakeoverRequest(musicID)
		expected := 999
		stale.ExpectedRevision = &expected
		if documentErr := lyricsDocumentErrorFor(t, s, stale, 0); documentErr.Code != LyricsDocumentErrorRevisionConflict {
			t.Fatalf("music %d stale revision error=%+v", musicID, documentErr)
		}
		invalid := recoveryTakeoverRequest(musicID)
		invalid.Renditions[0].Lines[0].Japanese = ""
		if documentErr := lyricsDocumentErrorFor(t, s, invalid, 0); documentErr.Code != LyricsDocumentErrorInvalid {
			t.Fatalf("music %d invalid document error=%+v", musicID, documentErr)
		}
	}
	if after := lyricsDocumentTableCounts(t, s); !reflect.DeepEqual(after, before) {
		t.Fatal("a refused publish of a recovery ledger song changed the database")
	}
	requireRecoveryLedgerUnchanged(t, s, ledger)
	if _, err := s.GetLyricsRenditionDocument(10); err != nil {
		t.Fatalf("recovery document is no longer readable: %v", err)
	}
}

func TestRecoveryTakeoverSupersedesTheItemOwningTheSourceDocumentOrElseTheNewestItem(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	// A later synthetic batch holds both songs as missing.
	newer := recoveryEditorTestSHA("newer-batch")
	availabilityJSON := `{"schemaVersion":1,"state":"missing","reasonCode":"version_conflict","fixedIdentities":[],"provenance":{}}`
	var createdAt int64
	if err := s.db.QueryRow(`SELECT created_at FROM lyrics_recovery_import_batches WHERE batch_sha256=?`,
		fixture.batchSHA).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO lyrics_recovery_import_batches
		(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
		 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
		 raw_byte_count,encoded_byte_count,actor,created_at) VALUES (?,1,2,'newer-root',?,2,?,?,?,?,?,0,0,0,0,'takeover-test',?)`,
		newer, recoveryEditorTestSHA("newer-root"), recoveryEditorTestSHA("newer-music"), `{"total":2,"missing":2}`,
		recoveryEditorTestSHA("newer-receipt"), recoveryEditorTestSHA("newer-pack"), recoveryEditorTestSHA("newer-selection"),
		createdAt+100); err != nil {
		t.Fatal(err)
	}
	for _, musicID := range []int{10, 20} {
		availabilitySHA := recoveryEditorTestSHA(fmt.Sprintf("newer-availability-%d", musicID))
		resultSHA := recoveryEditorTestSHA(fmt.Sprintf("newer-result-%d", musicID))
		if _, err := s.db.Exec(`INSERT INTO lyrics_recovery_import_items
			(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,
			 state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
			VALUES (?,?,'合成曲',?,?,'[]','missing',?,'','',?,?)`, newer, musicID,
			recoveryRenditionTestCatalogFingerprint(t, s, musicID), musicID, resultSHA, availabilitySHA, createdAt+100); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO song_lyrics_availability_documents
			(batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,document_sha256,result_sha256,created_at)
			VALUES (?,?,1,'missing','version_conflict','',?,?,?,?)`, newer, musicID, availabilityJSON,
			availabilitySHA, resultSHA, createdAt+100); err != nil {
			t.Fatal(err)
		}
	}
	for musicID, want := range map[int]struct{ batchSHA, state string }{
		10: {fixture.batchSHA, "complete"},
		20: {newer, "missing"},
	} {
		request := recoveryTakeoverRequest(musicID)
		request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 0)
		publishLyricsDocumentForTest(t, s, request, 0)
		if takeover := recoveryTakeoverRowsForTest(t, s)[musicID]; takeover.batchSHA != want.batchSHA || takeover.state != want.state {
			t.Fatalf("music %d takeover=%+v, want batch %s state %s", musicID, takeover, want.batchSHA, want.state)
		}
	}
}

// servedRecoveryDetailForTest is the v3 detail the recovery ledger batch
// serves for musicID.
func servedRecoveryDetailForTest(t *testing.T, s *Store, batchSHA string, musicID int) []byte {
	t.Helper()
	candidate, err := s.RecoveryPublicLyricsV3(batchSHA)
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := candidate.Details[musicID]
	if !ok {
		t.Fatalf("recovery batch does not serve music %d", musicID)
	}
	body, err := EncodePublicLyricsV3Detail(detail)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A song the recovery ledger serves becomes an editor document through its
// export: the public lyrics stay as served, and source-layer console edits,
// which the ledger document refuses, then save.
func TestTakeOverLyricsDocumentMakesAServedLedgerSongAnEditorDocument(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	seedRecoveryLedgerTranslations(t, fixture, false)
	const musicID = 10
	ctx := context.Background()
	ledgerDocument, err := s.GetLyricsRenditionDocument(musicID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ledgerDocument)
	if err != nil || !ledgerDocument.RecoveryLedgerOwned || !strings.Contains(string(encoded), `"recoveryLedgerOwned":true`) {
		t.Fatalf("ledger document owned=%t err=%v", ledgerDocument.RecoveryLedgerOwned, err)
	}
	sourceEdit := func(document LyricsRenditionDocument) (LyricsRenditionDocument, bool, error) {
		requested := cloneLyricsRenditionEditorDocument(t, document)
		line := &requested.Renditions[0].Full.Lines[1]
		line.StanzaBreakBefore = !line.StanzaBreakBefore
		return s.SaveLyricsRenditionMutation(requested, "console-editor")
	}
	var contractErr *LyricsRenditionContractError
	if _, _, err := sourceEdit(ledgerDocument); !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("ledger source edit err=%v", err)
	}

	servedBody := servedRecoveryDetailForTest(t, s, fixture.batchSHA, musicID)
	served := LyricsDocumentServed{Detail: servedBody}
	before := lyricsDocumentTableCounts(t, s)
	stale := 999
	_, err = s.TakeOverLyricsDocument(ctx, musicID, &stale, served, "document-admin")
	var documentErr *LyricsDocumentError
	if !errors.As(err, &documentErr) || documentErr.Code != LyricsDocumentErrorRevisionConflict {
		t.Fatalf("stale takeover err=%v", err)
	}
	if _, err = s.TakeOverLyricsDocument(ctx, musicID, nil, served, "document-admin"); !errors.As(err, &documentErr) ||
		documentErr.Code != LyricsDocumentErrorRevisionRequired {
		t.Fatalf("takeover without expectedRevision err=%v", err)
	}
	if _, err = s.TakeOverLyricsDocument(ctx, musicID, &stale, LyricsDocumentServed{}, "document-admin"); !errors.As(err, &documentErr) ||
		documentErr.Code != LyricsDocumentErrorNotFound {
		t.Fatalf("unserved takeover err=%v", err)
	}
	if after := lyricsDocumentTableCounts(t, s); !reflect.DeepEqual(after, before) {
		t.Fatal("a refused takeover changed the database")
	}

	export, err := s.ExportLyricsDocument(ctx, musicID, servedBody, 0)
	if err != nil {
		t.Fatal(err)
	}
	takeover, err := s.TakeOverLyricsDocument(ctx, musicID, export.Document.ExpectedRevision, served, "document-admin")
	if err != nil {
		t.Fatal(err)
	}
	if takeover.DryRun || takeover.Changes.Changed || takeover.Changes.Against != LyricsDocumentChangesAgainstServed ||
		!reflect.DeepEqual(takeover.Warnings, append([]LyricsDocumentExportWarning{}, export.Warnings...)) {
		t.Fatalf("takeover=%+v", takeover)
	}
	requireProjectionServes(t, s, musicID, takeover.LyricsDocumentResult)
	original, err := DecodePublicLyricsV3Detail(servedBody)
	if err != nil {
		t.Fatal(err)
	}
	produced, err := DecodePublicLyricsV3Detail(takeover.Document)
	if err != nil {
		t.Fatal(err)
	}
	// Every line keeps its text, ruby, segments, performers, zh and stanza
	// break; only what the export warns it cannot carry differs.
	explained := map[string]string{
		`^\.renditions\[\d+\]\.provenance`:                                      LyricsDocumentExportWarningSourceDiffers,
		`^\.renditions\[\d+\]\.(full|game)\.lines\[\d+\]\.trailingPerformerIds`: LyricsDocumentExportWarningTrailingPerformers,
	}
	codes := map[string]bool{}
	for _, warning := range takeover.Warnings {
		codes[warning.Code] = true
	}
	diffs := lyricsDocumentExportTreeDiffs("", lyricsDocumentExportComparable(t, original), lyricsDocumentExportComparable(t, produced), nil)
	registryColors := map[string]string{}
	for _, performer := range lyricsperformers.All() {
		registryColors[performer.SourceID] = performer.Color
	}
	colorPath := regexp.MustCompile(`^\.renditions\[(\d+)\]\.performers\[(\d+)\]\.color$`)
	for _, diff := range diffs {
		// The ledger fixture uses synthetic colours; PUT writes the audited
		// registry colour, which real ledger songs carry.
		matched := false
		if match := colorPath.FindStringSubmatch(diff); match != nil {
			rendition, _ := strconv.Atoi(match[1])
			index, _ := strconv.Atoi(match[2])
			performer := produced.Renditions[rendition].Performers[index]
			matched = performer.Color == registryColors[performer.PerformerID]
		}
		for pattern, code := range explained {
			matched = matched || regexp.MustCompile(pattern).MatchString(diff) && codes[code]
		}
		if !matched {
			t.Fatalf("the takeover changed the public lyrics at %s (all %v)", diff, diffs)
		}
	}
	if takeover := recoveryTakeoverRowsForTest(t, s)[musicID]; takeover.batchSHA != fixture.batchSHA {
		t.Fatalf("takeover row=%+v", takeover)
	}

	editorDocument, err := s.GetLyricsRenditionDocument(musicID)
	if err != nil || editorDocument.RecoveryLedgerOwned {
		t.Fatalf("editor document owned=%t err=%v", editorDocument.RecoveryLedgerOwned, err)
	}
	saved, changed, err := sourceEdit(editorDocument)
	if err != nil || !changed || saved.Revision != takeover.Revision+1 {
		t.Fatalf("editor source edit revision=%d changed=%t err=%v", saved.Revision, changed, err)
	}
}
