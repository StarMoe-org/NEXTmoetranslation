package db

import (
	"fmt"
	"path/filepath"
	"testing"

	"moesekai/server/internal/model"
)

// A catalog row that already carries a canonical identity must survive startup
// with its exact recorded bytes.
const keptMusicID = 3

// A database whose schema_migrations rows carry checksums recorded by earlier
// deployments of the song-682 migrations must still open, and startup must
// repair the structurally invalid catalog identity migration 34 wrote.
func TestOpenAcceptsHistoricalMigrationChecksumsAndRepairsCatalogIdentity(t *testing.T) {
	historical := historicalMigrationChecksums

	for version, checksums := range historical {
		for index, checksum := range checksums {
			name := fmt.Sprintf("v%d-variant%d", version, index)
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "prod.db")
				database, err := Open(path)
				if err != nil {
					t.Fatalf("initial open: %v", err)
				}
				if _, err := database.Exec(`UPDATE schema_migrations SET checksum=? WHERE version=?`,
					checksum, version); err != nil {
					database.Close()
					t.Fatal(err)
				}
				// Reproduce the deployed migration-34 catalog row: an invalid
				// fingerprint and a non-canonical identity policy version.
				if _, err := database.Exec(`INSERT OR REPLACE INTO catalog_music
					(music_id,title_ja,title_zh,title_en,jacket_url,newly_written,updated_at,
					 producer_metadata,lyricist,composer,arranger,
					 lyrics_evidence_presence_json,vocal_signals_json,
					 lyrics_catalog_fingerprint,lyrics_catalog_policy_version)
					VALUES (682,'あなたしか見えないの','眼中仅有你一人','Anata Shika Mienai no','',0,1724544000,
					 'Guiano','Guiano','Guiano','',
					 '{"title":true,"lyricist":true,"composer":true,"arranger":false,"lyricsVersion":false,"vocals":true}',
					 '[{"kind":"sekai","performers":["x"]}]','fingerprint-682','1')`); err != nil {
					database.Close()
					t.Fatal(err)
				}
				// A row whose identity is already canonical must not be rewritten.
				keptFingerprint, err := model.CatalogLyricsEvidenceFingerprint(model.CatalogLyricsEvidence{
					Title: "Kept Song", Lyricist: "Kept Lyricist",
					Vocals:   []model.CatalogVocalSignal{},
					Presence: model.CatalogEvidencePresence{Lyricist: true},
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`INSERT OR REPLACE INTO catalog_music
					(music_id,title_ja,title_zh,title_en,jacket_url,newly_written,updated_at,
					 producer_metadata,lyricist,composer,arranger,lyrics_version,
					 lyrics_evidence_presence_json,vocal_signals_json,
					 lyrics_catalog_fingerprint,lyrics_catalog_policy_version)
					VALUES (?,'Kept Song','','','',0,1724544000,
					 'Kept Lyricist','Kept Lyricist','','','unknown',
					 '{"lyricist":true,"composer":false,"arranger":false,"assetbundle":false,"versionHint":false,"lyricsVersion":false,"vocals":false}',
					 '[]',?,?)`,
					keptMusicID, keptFingerprint, model.LyricsCatalogIdentityPolicyVersion); err != nil {
					database.Close()
					t.Fatal(err)
				}
				if err := database.Checkpoint(t.Context()); err != nil {
					database.Close()
					t.Fatal(err)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}

				reopened, err := Open(path)
				if err != nil {
					t.Fatalf("reopen with deployed v%d checksum %s: %v", version, checksum[:12], err)
				}
				defer reopened.Close()

				var fingerprint, policy string
				if err := reopened.QueryRow(`SELECT lyrics_catalog_fingerprint,lyrics_catalog_policy_version
					FROM catalog_music WHERE music_id=682`).Scan(&fingerprint, &policy); err != nil {
					t.Fatal(err)
				}
				if policy != model.LyricsCatalogIdentityPolicyVersion {
					t.Fatalf("policy version was not repaired: %q", policy)
				}
				// The repaired fingerprint must equal the digest derived from the
				// row's own evidence, not any pre-baked constant.
				want, err := model.CatalogLyricsEvidenceFingerprint(model.CatalogLyricsEvidence{
					Title: "あなたしか見えないの", Lyricist: "Guiano", Composer: "Guiano",
					Vocals: []model.CatalogVocalSignal{},
					Presence: model.CatalogEvidencePresence{
						Lyricist: true, Composer: true,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if fingerprint != want {
					t.Fatalf("fingerprint=%q want=%q", fingerprint, want)
				}

				// An already-valid identity must keep its exact recorded bytes.
				var untouched string
				if err := reopened.QueryRow(`SELECT lyrics_catalog_fingerprint FROM catalog_music
					WHERE music_id=?`, keptMusicID).Scan(&untouched); err != nil {
					t.Fatal(err)
				}
				if untouched != keptFingerprint {
					t.Fatalf("valid identity was rewritten: %q want %q", untouched, keptFingerprint)
				}
			})
		}
	}
}

func TestValidateKnownMigrationPrefixAcceptsHistoricalMigrationChecksums(t *testing.T) {
	for version, checksums := range historicalMigrationChecksums {
		for index, checksum := range checksums {
			name := fmt.Sprintf("v%d-variant%d", version, index)
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "prefix.db")
				database, err := Open(path)
				if err != nil {
					t.Fatalf("initial open: %v", err)
				}
				defer database.Close()

				if _, err := database.Exec(`UPDATE schema_migrations SET checksum=? WHERE version=?`,
					checksum, version); err != nil {
					t.Fatal(err)
				}

				actualVersion, err := database.ValidateKnownMigrationPrefix(t.Context(), 27, 39)
				if err != nil {
					t.Fatalf("ValidateKnownMigrationPrefix with historical checksum v%d %s: %v", version, checksum[:12], err)
				}
				if actualVersion != len(migrations) {
					t.Fatalf("actualVersion=%d want=%d", actualVersion, len(migrations))
				}
			})
		}
	}
}
