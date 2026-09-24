package offlineimport

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/db"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
	"moesekai/server/offline/internal/lyricsrecoveryimport"
)

func TestLyricsImportRuntimeSchemasAllowReviewedV27ThroughV38Contiguously(t *testing.T) {
	validators := map[string]func(context.Context, *sql.Tx) error{
		"recovery": validateRecoveryImportRuntimeSchema,
		"staged":   validateStagedImportRuntimeSchema,
	}
	deleteVersionsFrom := func(first int) func(*testing.T, *sql.Tx) {
		return func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version>=?`, first); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name      string
		mutate    func(*testing.T, *sql.Tx)
		wantError bool
	}{
		{name: "current v38"},
		{name: "v37 input runtime", mutate: deleteVersionsFrom(38)},
		{name: "v36 input runtime", mutate: deleteVersionsFrom(37)},
		{name: "v35 input runtime", mutate: deleteVersionsFrom(36)},
		{name: "v34 input runtime", mutate: deleteVersionsFrom(35)},
		{name: "v33 input runtime", mutate: deleteVersionsFrom(34)},
		{name: "v32 input runtime", mutate: deleteVersionsFrom(33)},
		{name: "v31 input runtime", mutate: deleteVersionsFrom(32)},
		{name: "v30 input runtime", mutate: deleteVersionsFrom(31)},
		{name: "v29 input runtime", mutate: deleteVersionsFrom(30)},
		{name: "v28 input runtime", mutate: deleteVersionsFrom(29)},
		{name: "v27 input runtime", mutate: deleteVersionsFrom(28)},
		{name: "gap before v38", wantError: true, mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version=37`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unreviewed v39", wantError: true, mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`INSERT INTO schema_migrations(version,name,checksum,applied_at)
				VALUES (39,'future_migration',?,1)`, strings.Repeat("f", 64)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for validatorName, validate := range validators {
		for _, test := range cases {
			t.Run(validatorName+"/"+test.name, func(t *testing.T) {
				database := openImportTestDatabase(t)
				tx, err := database.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if test.mutate != nil {
					test.mutate(t, tx)
				}
				err = validate(context.Background(), tx)
				if test.wantError {
					if err == nil || !strings.Contains(err.Error(), "contiguous schema-v27 through schema-v38 runtime") {
						t.Fatalf("runtime schema gate error=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("runtime schema gate: %v", err)
				}
			})
		}
	}
}

func TestRecoveryEditableReplayRevisionUsesStoredRevision(t *testing.T) {
	requested := model.SongLyrics{MusicID: 42, Revision: 0, Lines: []model.LyricLine{{ID: "line-1", Japanese: "同じ"}}}
	current := requested
	current.Lines = append([]model.LyricLine(nil), requested.Lines...)
	current.Revision = 7
	if got, err := recoveryEditableReplayRevision(42, requested, current); err != nil || got != 7 {
		t.Fatalf("replay revision=%d err=%v want=7", got, err)
	}
	current.Lines[0].Japanese = "変更"
	if _, err := recoveryEditableReplayRevision(42, requested, current); err == nil {
		t.Fatal("editable lyrics drift was accepted during replay")
	}
}

func TestRecoveryCategoriesJSONAlwaysPersistsAnArray(t *testing.T) {
	for name, test := range map[string]struct {
		categories []string
		want       string
	}{
		"nil":   {categories: nil, want: "[]"},
		"empty": {categories: []string{}, want: "[]"},
		"values": {
			categories: []string{"Songs", "Project SEKAI"},
			want:       `["Songs","Project SEKAI"]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := recoveryCategoriesJSON(test.categories)
			if err != nil || got != test.want {
				t.Fatalf("categories JSON=%q want=%q err=%v", got, test.want, err)
			}
		})
	}
}

func TestPeerTranslationRuntimeSchemaRequiresExactV29OnlyWhenPresent(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, *sql.Tx)
		wantError string
	}{
		{name: "exact schema-v29"},
		{name: "schema-v28", wantError: "requires schema-v29", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version=29`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong ledger name", wantError: "ledger is invalid", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`UPDATE schema_migrations SET name='wrong' WHERE version=29`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong ledger checksum", wantError: "ledger is invalid", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`UPDATE schema_migrations SET checksum=? WHERE version=29`, strings.Repeat("f", 64)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing peer table", wantError: "table is invalid", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DROP TABLE song_lyrics_rendition_side_translation_lines`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing lookup index", wantError: "index is invalid", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DROP INDEX idx_song_lyrics_rendition_side_translation_lines_lookup`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openImportTestDatabase(t)
			tx, err := database.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if test.mutate != nil {
				test.mutate(t, tx)
			}
			if err := validatePeerTranslationRuntimeSchema(context.Background(), tx, false, "test import"); err != nil {
				t.Fatalf("no-peer schema gate: %v", err)
			}
			err = validatePeerTranslationRuntimeSchema(context.Background(), tx, true, "test import")
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("exact peer schema: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("peer schema error=%v want substring %q", err, test.wantError)
			}
		})
	}
}

func TestRecoveryImportCatalogTargetMatchesStateClosedSet(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	fullTarget := model.CatalogLyricsTarget{
		MusicID: 42, CatalogFingerprint: fingerprint,
		Disposition: model.LyricsCatalogTargetFullTarget, TargetMusicID: 42,
		AssociationMusicIDs: []int{},
	}
	for _, state := range []lyricscontract.CoverageState{
		lyricscontract.CoverageComplete,
		lyricscontract.CoverageGameOnly,
		lyricscontract.CoverageAmbiguous,
		lyricscontract.CoverageMissing,
		lyricscontract.CoverageIncomplete,
		lyricscontract.CoverageFailed,
	} {
		item := lyricsrecoveryimport.Item{
			MusicID: 42, CatalogFingerprint: fingerprint, TargetMusicID: 42,
			AssociationMusicIDs: []int{}, State: state,
		}
		if !recoveryImportCatalogTargetMatches(item, fullTarget) {
			t.Fatalf("state %q rejected its exact Full catalog target", state)
		}
	}

	instrumentalReview := model.CatalogLyricsTarget{
		MusicID: 42, CatalogFingerprint: fingerprint,
		Disposition: model.LyricsCatalogTargetReview, ReasonCode: "instrumental_no_vocals",
		AssociationMusicIDs: []int{},
	}
	noLyrics := lyricsrecoveryimport.Item{
		MusicID: 42, CatalogFingerprint: fingerprint, TargetMusicID: 42,
		AssociationMusicIDs: []int{}, State: lyricscontract.CoverageSatisfiedNoLyrics,
	}
	if !recoveryImportCatalogTargetMatches(noLyrics, instrumentalReview) {
		t.Fatal("reviewed catalog instrumental was rejected for satisfied no-lyrics")
	}
}

func TestRecoveryImportCatalogTargetMatchesRejectsCrossStateOrIdentityDrift(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	complete := lyricsrecoveryimport.Item{
		MusicID: 42, CatalogFingerprint: fingerprint, TargetMusicID: 42,
		AssociationMusicIDs: []int{}, State: lyricscontract.CoverageComplete,
	}
	noLyrics := complete
	noLyrics.State = lyricscontract.CoverageSatisfiedNoLyrics
	fullTarget := model.CatalogLyricsTarget{
		MusicID: 42, CatalogFingerprint: fingerprint,
		Disposition: model.LyricsCatalogTargetFullTarget, TargetMusicID: 42,
		AssociationMusicIDs: []int{},
	}
	instrumentalReview := model.CatalogLyricsTarget{
		MusicID: 42, CatalogFingerprint: fingerprint,
		Disposition: model.LyricsCatalogTargetReview, ReasonCode: "instrumental_no_vocals",
		AssociationMusicIDs: []int{},
	}

	tests := map[string]struct {
		item   lyricsrecoveryimport.Item
		target model.CatalogLyricsTarget
	}{
		"complete cannot consume instrumental review": {item: complete, target: instrumentalReview},
		"no-lyrics cannot consume Full target":        {item: noLyrics, target: fullTarget},
		"no-lyrics reason is closed": {item: noLyrics, target: func() model.CatalogLyricsTarget {
			target := instrumentalReview
			target.ReasonCode = "medley_composite_source"
			return target
		}()},
		"catalog fingerprint drift": {item: complete, target: func() model.CatalogLyricsTarget {
			target := fullTarget
			target.CatalogFingerprint = strings.Repeat("b", 64)
			return target
		}()},
		"Full target drift": {item: complete, target: func() model.CatalogLyricsTarget {
			target := fullTarget
			target.TargetMusicID = 43
			return target
		}()},
		"Full association drift": {item: complete, target: func() model.CatalogLyricsTarget {
			target := fullTarget
			target.AssociationMusicIDs = []int{43}
			return target
		}()},
		"review target must remain unelected": {item: noLyrics, target: func() model.CatalogLyricsTarget {
			target := instrumentalReview
			target.TargetMusicID = 42
			return target
		}()},
		"unknown state": {item: func() lyricsrecoveryimport.Item {
			item := complete
			item.State = lyricscontract.CoverageState("future")
			return item
		}(), target: fullTarget},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if recoveryImportCatalogTargetMatches(test.item, test.target) {
				t.Fatal("unsafe recovery catalog target was accepted")
			}
		})
	}
}

func openImportTestDatabase(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "offline-import.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestNewRecoveryBatchRefusesSongsTakenOverByADocumentPublish(t *testing.T) {
	database := openImportTestDatabase(t)
	h := func(character string) string { return strings.Repeat(character, 64) }
	batchSHA := h("a")
	coverage := `{"total":2,"complete":0,"satisfiedNoLyrics":0,"catalogReview":0,"gameSizeEvidence":0,"ambiguous":0,"missing":2,"incomplete":0,"failed":0,"providerOutcomeRefCount":0,"selectionRefCount":0,"uniqueAcquisitionCount":0,"uniqueEvidenceCount":0}`
	availabilityJSON := `{"schemaVersion":1,"state":"missing","reasonCode":"version_conflict","fixedIdentities":[],"provenance":{}}`
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO catalog_music(music_id,title_ja) VALUES (1,'合成接管曲'),(2,'合成未接管曲')`, nil},
		{`INSERT INTO lyrics_recovery_import_batches
			(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
			 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
			 raw_byte_count,encoded_byte_count,actor,created_at)
			VALUES (?,1,2,'root-takeover',?,2,?,?,?,?,?,0,0,0,0,'import-test',1)`,
			[]any{batchSHA, h("b"), h("c"), coverage, h("d"), h("e"), h("f")}},
		{`INSERT INTO lyrics_recovery_import_items
			(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,
			 state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
			VALUES (?,1,'合成接管曲',?,1,'[]','missing',?,'','',?,1),(?,2,'合成未接管曲',?,2,'[]','missing',?,'','',?,1)`,
			[]any{batchSHA, h("1"), h("2"), h("3"), batchSHA, h("4"), h("5"), h("6")}},
		{`INSERT INTO song_lyrics_availability_documents
			(batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,document_sha256,result_sha256,created_at)
			VALUES (?,1,1,'missing','version_conflict','',?,?,?,1),(?,2,1,'missing','version_conflict','',?,?,?,1)`,
			[]any{batchSHA, availabilityJSON, h("3"), h("2"), batchSHA, availabilityJSON, h("6"), h("5")}},
		{`INSERT INTO lyrics_recovery_takeovers(music_id,batch_sha256,item_state,taken_over_at,taken_over_by)
			VALUES (1,?,'missing',2,'document-admin')`, []any{batchSHA}},
	} {
		if _, err := database.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed taken-over recovery ledger: %v", err)
		}
	}
	manifest := func(musicIDs ...int) lyricsrecoveryimport.Manifest {
		var result lyricsrecoveryimport.Manifest
		for _, musicID := range musicIDs {
			result.Items = append(result.Items, lyricsrecoveryimport.Item{MusicID: musicID, State: lyricscontract.CoverageMissing})
		}
		return result
	}
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = refuseRecoveryItemsForTakenOverSongs(context.Background(), tx, manifest(2, 1))
	if !errors.Is(err, store.ErrLyricsRecoveryImportConflict) || !strings.Contains(err.Error(), "music 1 was taken over") {
		t.Fatalf("taken-over song error=%v", err)
	}
	if err := refuseRecoveryItemsForTakenOverSongs(context.Background(), tx, manifest(2)); err != nil {
		t.Fatalf("song without a takeover: %v", err)
	}
	// A v37 runtime has no takeover table and so no takeovers.
	if _, err := tx.Exec(`DROP TABLE lyrics_recovery_takeovers`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version=38`); err != nil {
		t.Fatal(err)
	}
	if err := refuseRecoveryItemsForTakenOverSongs(context.Background(), tx, manifest(1, 2)); err != nil {
		t.Fatalf("v37 runtime: %v", err)
	}
}

func TestNewRecoveryBatchRefusesSongsWithEditorOwnedSourceDocuments(t *testing.T) {
	database := openImportTestDatabase(t)
	h := func(character string) string { return strings.Repeat(character, 64) }
	recoveryBatchSHA, seedSHA, stagedBatchSHA := h("a"), h("b"), h("c")
	coverage := `{"total":5,"complete":0,"satisfiedNoLyrics":0,"catalogReview":0,"gameSizeEvidence":0,"ambiguous":0,"missing":5,"incomplete":0,"failed":0,"providerOutcomeRefCount":0,"selectionRefCount":0,"uniqueAcquisitionCount":0,"uniqueEvidenceCount":0}`
	insertSourceDocument := `INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,3,'','{}',?,?,1)`
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO catalog_music(music_id,title_ja) VALUES
			(1,'合成编辑器曲'),(2,'合成种子曲'),(3,'合成恢复曲'),(4,'合成暂存曲'),(5,'合成空曲')`, nil},
		{`INSERT INTO lyrics_recovery_import_batches
			(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
			 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
			 raw_byte_count,encoded_byte_count,actor,created_at)
			VALUES (?,1,2,'root-editor-owned',?,5,?,?,?,?,?,0,0,0,0,'import-test',1)`,
			[]any{recoveryBatchSHA, h("d"), h("e"), coverage, h("f"), h("1"), h("2")}},
		{`INSERT INTO embedded_lyrics_editor_seed_batches
			(seed_sha256,archive_sha256,release_id,schema_version,source_batch_sha256,root_sha256,
			 catalog_policy_version,catalog_count,music_ids_sha256,catalog_fingerprints_sha256,created_at)
			VALUES (?,?,'synthetic-seed',1,?,?,'synthetic-policy',5,?,?,1)`,
			[]any{seedSHA, h("3"), recoveryBatchSHA, h("d"), h("e"), h("4")}},
		{insertSourceDocument, []any{1, h("5"), lyricsEditorManifestBatchSHA256}},
		{insertSourceDocument, []any{2, h("6"), seedSHA}},
		{insertSourceDocument, []any{3, h("7"), recoveryBatchSHA}},
		{insertSourceDocument, []any{4, h("8"), stagedBatchSHA}},
	} {
		if _, err := database.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed source documents: %v", err)
		}
	}
	manifest := func(musicIDs ...int) lyricsrecoveryimport.Manifest {
		var result lyricsrecoveryimport.Manifest
		for _, musicID := range musicIDs {
			result.Items = append(result.Items, lyricsrecoveryimport.Item{MusicID: musicID, State: lyricscontract.CoverageMissing})
		}
		return result
	}
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = refuseRecoveryItemsForEditorOwnedSongs(context.Background(), tx, manifest(5, 1))
	if !errors.Is(err, store.ErrLyricsRecoveryImportConflict) ||
		!strings.Contains(err.Error(), "music 1 already has a source document published by the lyrics editor") {
		t.Fatalf("editor-published song error=%v", err)
	}
	err = refuseRecoveryItemsForEditorOwnedSongs(context.Background(), tx, manifest(5, 2))
	if !errors.Is(err, store.ErrLyricsRecoveryImportConflict) ||
		!strings.Contains(err.Error(), "music 2 already has a source document from embedded lyrics editor seed "+seedSHA) {
		t.Fatalf("seeded song error=%v", err)
	}
	// Recovery-batch and staged-import documents are left to the per-item checks.
	if err := refuseRecoveryItemsForEditorOwnedSongs(context.Background(), tx, manifest(3, 4, 5)); err != nil {
		t.Fatalf("recovery, staged and document-free songs: %v", err)
	}
	// A v27 runtime has no seed tables.
	for _, statement := range []string{
		`DROP TABLE embedded_lyrics_editor_seed_items`,
		`DROP TABLE embedded_lyrics_editor_seed_batches`,
		`DELETE FROM schema_migrations WHERE version>=28`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := refuseRecoveryItemsForEditorOwnedSongs(context.Background(), tx, manifest(2, 3, 4, 5)); err != nil {
		t.Fatalf("v27 runtime: %v", err)
	}
	if err := refuseRecoveryItemsForEditorOwnedSongs(context.Background(), tx, manifest(1)); !errors.Is(err, store.ErrLyricsRecoveryImportConflict) {
		t.Fatalf("v27 runtime editor-published song error=%v", err)
	}
}
