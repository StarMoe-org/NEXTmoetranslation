package offlineimport

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/db"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsrecoveryimport"
)

func TestLyricsImportRuntimeSchemasAllowReviewedV27ThroughV36Contiguously(t *testing.T) {
	validators := map[string]func(context.Context, *sql.Tx) error{
		"recovery": validateRecoveryImportRuntimeSchema,
		"staged":   validateStagedImportRuntimeSchema,
	}
	cases := []struct {
		name      string
		mutate    func(*testing.T, *sql.Tx)
		wantError bool
	}{
		{name: "current v36"},
		{name: "v35 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version=36`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v34 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v33 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v32 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (33,34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v31 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (32,33,34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v30 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (31,32,33,34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v29 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (30,31,32,33,34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v28 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (29,30,31,32,33,34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "v27 input runtime", mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (28,29,30,31,32,33,34,35,36)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "gap before v36", wantError: true, mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version=35`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unreviewed v37", wantError: true, mutate: func(t *testing.T, tx *sql.Tx) {
			if _, err := tx.Exec(`INSERT INTO schema_migrations(version,name,checksum,applied_at)
				VALUES (37,'future_migration',?,1)`, strings.Repeat("f", 64)); err != nil {
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
					if err == nil || !strings.Contains(err.Error(), "contiguous schema-v27 through schema-v36 runtime") {
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
