package db

import (
	"database/sql"

	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLyricsSourceMigrationsAppendAfterPinnedV12(t *testing.T) {
	wantNames := map[int]string{
		13: "private_lyrics_source_artifacts",
		14: "versioned_lyrics_source_analysis_outputs",
		15: "private_lyrics_source_review_queue",
		16: "overall_lyrics_source_artifact_review_decisions",
		17: "lyrics_source_review_batch_idempotency",
		18: "structured_lyrics_source_analysis_evidence",
		19: "editable_song_lyrics_ruby",
		20: "versioned_lyrics_discovery_fixed_candidate_identity",
		21: "provider_scoped_lyrics_source_provenance",
		22: "lyrics_source_game_projection_and_ruby_contract",
		23: "sekaipedia_lyrics_source_provenance",
		24: "additive_lyrics_recovery_import_storage",
		25: "lyrics_source_document_schema_v2",
		26: "lyrics_translation_and_proofreading_credits",
		27: "lyrics_peer_renditions_and_localizations",
		29: "lyrics_translation_versions",
		30: "lyrics_translation_editions",
		31: "lyrics_yjs_collaboration",
		32: "song_682_translation_editions",
		33: "song_682_translation_qed_correction",
		34: "song_682_translation_mirror_sync",
		35: "song_lyrics_public_withdrawals",
		36: "lyrics_provider_page_targets",
		37: "lyrics_source_artifacts_projectsekai_fandom_origin",
		38: "lyrics_recovery_takeovers",
		39: "side_stories",
	}
	if latest := migrations[len(migrations)-1]; latest.version != 39 || latest.name != wantNames[39] {
		t.Fatalf("latest migration=%d/%q want=39/%q", latest.version, latest.name, wantNames[39])
	}
	for version, name := range wantNames {
		migration := migrations[version-1]
		if migration.version != version || migration.name != name {
			t.Fatalf("migration v%d=%d/%q want=%q", version, migration.version, migration.name, name)
		}
	}
	if got := migrations[11].checksum(); got != "1bcc25c5e99cdb8eabdf339a7a9b1dc91c3f820e4abacfd1db46f53f8f8150d3" {
		t.Fatalf("pinned migration v12 checksum=%s", got)
	}
	if got := migrations[17].checksum(); got != "9ef12f0d266c281cfae1b76f80a61eb6c5142fd64ea9a45d7b97e327216031ff" {
		t.Fatalf("pinned migration v18 checksum=%s", got)
	}
	if got := migrations[19].checksum(); got != "ba96cd088d14cdc9d7e34536a16d438f34d7fa232182d5e3000aa9fc0f9328dc" {
		t.Fatalf("pinned migration v20 checksum=%s", got)
	}
	if got := migrations[20].checksum(); got != "820f2be54c57bc56aeb938f498a73109f62266a562764346b557603e90ec0282" {
		t.Fatalf("pinned migration v21 checksum=%s", got)
	}
	if got := migrations[21].checksum(); got != "64edad15266a55c04f7d300d043a59a2c82e1f43f1b5f56cf5d3c7552533832d" {
		t.Fatalf("pinned migration v22 checksum=%s", got)
	}
	if got := migrations[22].checksum(); got != "65e375543d264f60af66984ab50c87a05bf593c512926813e4870a8a388bd40f" {
		t.Fatalf("pinned migration v23 checksum=%s", got)
	}
	if got := migrations[23].checksum(); got != "fefc0fba06b0de2af2ef7d7f9802d8eeb0e6bdcd911b1f16a6fb0a4e9a7a6469" {
		t.Fatalf("pinned migration v24 checksum=%s", got)
	}
}

func TestPinnedMigrationsRejectUnchecksummedPreflightCallbacks(t *testing.T) {
	migration := migrations[12]
	migration.before = func(*sql.Tx) error { return nil }
	if err := migration.validateDefinition(); err == nil || !strings.Contains(err.Error(), "unchecksummed migration callback") {
		t.Fatalf("migration definition error=%v", err)
	}
}

func TestV13MigrationRejectsUnreconcilableV12FetchBeforeCommit(t *testing.T) {
	path := legacyFixtureCopy(t, "v13-direct-preflight.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:12]); err != nil {
		t.Fatal(err)
	}
	jobID := seedV12FetchRevisionJob(t, raw, "queued", nil)
	if err := database.applyMigrations(migrations[12:13]); err == nil || !strings.Contains(err.Error(), "migration 13 preflight") ||
		!strings.Contains(err.Error(), "unreconcilable v12 fetch_revision expected_sha1") {
		t.Fatalf("v13 migration error=%v", err)
	}
	var version, expectedColumn, sourceTables, jobs int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('lyrics_discovery_jobs') WHERE name='expected_sha1'`).Scan(&expectedColumn); err != nil || expectedColumn != 0 {
		t.Fatalf("expected_sha1 columns=%d err=%v", expectedColumn, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lyrics_source_artifacts'`).Scan(&sourceTables); err != nil || sourceTables != 0 {
		t.Fatalf("source tables=%d err=%v", sourceTables, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_discovery_jobs WHERE job_id=?`, jobID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("preserved jobs=%d err=%v", jobs, err)
	}
}

func TestV13MigrationFailsClosedOnUnreconcilableV12FetchJobsAndPreservesBackup(t *testing.T) {
	for name, seed := range map[string]struct {
		state      string
		completion any
		shadowJSON string
	}{
		"nonterminal without shadow result": {state: "queued", completion: nil},
		"terminal without shadow result":    {state: "succeeded", completion: int64(10)},
		"mismatched candidate revision": {state: "succeeded", completion: int64(10),
			shadowJSON: `{"candidates":[{"pageId":12,"revisionId":35,"sha1":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			path := legacyFixtureCopy(t, "v13-unreconcilable-fetch.db")
			raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			database := &DB{DB: raw, path: path}
			if err := database.applyMigrations(migrations[:12]); err != nil {
				raw.Close()
				t.Fatal(err)
			}
			jobID := seedV12FetchRevisionJob(t, raw, seed.state, seed.completion)
			if seed.shadowJSON != "" {
				seedV12ShadowResult(t, raw, jobID, seed.shadowJSON)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unreconcilable v12 fetch_revision expected_sha1") {
				t.Fatalf("Open error=%v", err)
			}
			backupPath := path + ".pre-migration-v13.bak"
			if err := verifySQLiteBackup(backupPath); err != nil {
				t.Fatalf("v13 pre-migration backup: %v", err)
			}
			for label, databasePath := range map[string]string{"database": path, "backup": backupPath} {
				check, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
				if err != nil {
					t.Fatal(err)
				}
				var version, expectedColumn, sourceTables, jobs int
				if err := check.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 12 {
					check.Close()
					t.Fatalf("%s version=%d err=%v", label, version, err)
				}
				if err := check.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('lyrics_discovery_jobs') WHERE name='expected_sha1'`).Scan(&expectedColumn); err != nil || expectedColumn != 0 {
					check.Close()
					t.Fatalf("%s expected_sha1 columns=%d err=%v", label, expectedColumn, err)
				}
				if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lyrics_source_artifacts'`).Scan(&sourceTables); err != nil || sourceTables != 0 {
					check.Close()
					t.Fatalf("%s source tables=%d err=%v", label, sourceTables, err)
				}
				if err := check.QueryRow(`SELECT COUNT(*) FROM lyrics_discovery_jobs WHERE job_id=?`, jobID).Scan(&jobs); err != nil || jobs != 1 {
					check.Close()
					t.Fatalf("%s preserved jobs=%d err=%v", label, jobs, err)
				}
				if err := check.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if info, err := os.Stat(backupPath); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("v13 backup info=%v err=%v", info, err)
			}
		})
	}
}

func TestV13MigrationRejectsAmbiguousV12FetchCandidateSHA1s(t *testing.T) {
	path := legacyFixtureCopy(t, "v13-ambiguous-fetch.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:12]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	jobID := seedV12FetchRevisionJob(t, raw, "queued", nil)
	seedV12ShadowResult(t, raw, jobID, `{"candidates":[{"pageId":12,"revisionId":34,"sha1":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"pageId":12,"revisionId":34,"sha1":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "conflicting v12 fetch_revision expected_sha1 values") {
		t.Fatalf("Open error=%v", err)
	}
}

func TestV13MigrationReconcilesExactV12FetchCandidateSHA1(t *testing.T) {
	path := legacyFixtureCopy(t, "v13-reconciled-fetch.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:12]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	jobID := seedV12FetchRevisionJob(t, raw, "queued", nil)
	const sha1 = "0123456789abcdef0123456789abcdef01234567"
	seedV12ShadowResult(t, raw, jobID, `{"candidates":[{"pageId":12,"title":"合成試験曲","canonicalUrl":"https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34","revisionId":34,"sha1":"`+sha1+`","categories":[]}]}`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var version int
	var expectedSHA1 string
	if err := migrated.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := migrated.QueryRow(`SELECT expected_sha1 FROM lyrics_discovery_jobs WHERE job_id=?`, jobID).Scan(&expectedSHA1); err != nil || expectedSHA1 != sha1 {
		t.Fatalf("expected_sha1=%q err=%v", expectedSHA1, err)
	}
	if _, err := migrated.Exec(`UPDATE lyrics_discovery_jobs SET expected_sha1='' WHERE job_id=?`, jobID); err == nil {
		t.Fatal("v13 trigger allowed migrated fetch job SHA1 to be cleared")
	}
	if err := verifySQLiteBackup(path + ".pre-migration-v13.bak"); err != nil {
		t.Fatalf("v13 pre-migration backup: %v", err)
	}
}

func seedV12FetchRevisionJob(t *testing.T, database *sql.DB, state string, completedAt any) int64 {
	t.Helper()
	result, err := database.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, page_id, revision_id, catalog_fingerprint, policy_version,
		 attempts, max_attempts, next_attempt_at, created_at, updated_at, completed_at, version)
		VALUES (?, 'fetch_revision', ?, 10, 12, 34, ?, 'matching-v2', 0, 3, 1, 1, 1, ?, 1)`,
		strings.Repeat("c", 64), state, strings.Repeat("d", 64), completedAt)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

func seedV12ShadowResult(t *testing.T, database *sql.DB, jobID int64, resultJSON string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO lyrics_discovery_shadow_results
		(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
		VALUES (?, 10, ?, 'matching-v2', 'candidates_found', 1, ?, 2)`, jobID, strings.Repeat("d", 64), resultJSON); err != nil {
		t.Fatal(err)
	}
}

func TestV16MigrationPreservesLegacyDecisionsAndAllowsOverallGate(t *testing.T) {
	path := legacyFixtureCopy(t, "v16-overall-review-decisions.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:15]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	const fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (10,'合成試験曲')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_items
		(domain_key,kind,analysis_id,music_id,catalog_fingerprint,review_policy_version,reason_code,evidence_json,state,
		 identity_gate,source_use_gate,parse_gate,version,priority,created_at,updated_at)
		VALUES (?,'candidate_selection',NULL,10,?,'review-v1','ambiguous_candidates','{"candidates":[]}','pending',
		'not_applicable','not_applicable','not_applicable',1,0,1,1)`, strings.Repeat("b", 64), fingerprint); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_decisions
		(review_id,gate,decision,selected_candidate_json,actor,note,idempotency_key,request_sha256,expected_version,result_version,decided_at)
		VALUES (1,'candidate','excluded',NULL,'legacy-admin','none','legacy-idempotency-0001',?,1,2,2)`, strings.Repeat("c", 64)); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[15:16]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var gate, decision string
	if err := raw.QueryRow(`SELECT gate,decision FROM lyrics_source_review_decisions WHERE decision_id=1`).Scan(&gate, &decision); err != nil || gate != "candidate" || decision != "excluded" {
		raw.Close()
		t.Fatalf("legacy decision=%q/%q err=%v", gate, decision, err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_decisions
		(review_id,gate,decision,selected_candidate_json,actor,note,idempotency_key,request_sha256,expected_version,result_version,decided_at)
		VALUES (1,'overall','approved',NULL,'admin','','overall-idempotency-0001',?,2,3,3)`, strings.Repeat("d", 64)); err != nil {
		raw.Close()
		t.Fatalf("v16 rejected overall decision: %v", err)
	}
	if _, err := raw.Exec(`UPDATE lyrics_source_review_decisions SET note='changed' WHERE decision_id=1`); err == nil {
		raw.Close()
		t.Fatal("v16 lost immutable update trigger")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestV17BatchIdempotencyLedgerIsStrictAndImmutable(t *testing.T) {
	path := legacyFixtureCopy(t, "v17-batch-idempotency.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:16]); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[16:17]); err != nil {
		t.Fatal(err)
	}
	const validItems = `[{"reviewId":1,"expectedVersion":2},{"reviewId":3,"expectedVersion":4}]`
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_batch_idempotency
		(actor,idempotency_key,request_sha256,gate,decision,items_json,item_count,note,decided_at)
		VALUES ('admin','batch-idempotency-0001',?,'overall','approved',?,2,'',1)`, strings.Repeat("a", 64), validItems); err != nil {
		t.Fatalf("valid v17 ledger insert: %v", err)
	}
	for name, items := range map[string]string{
		"duplicate review": `[{"reviewId":1,"expectedVersion":2},{"reviewId":1,"expectedVersion":3}]`,
		"unsorted review":  `[{"reviewId":3,"expectedVersion":2},{"reviewId":1,"expectedVersion":3}]`,
		"unknown item key": `[{"reviewId":1,"expectedVersion":2,"extra":true}]`,
		"noninteger":       `[{"reviewId":1.5,"expectedVersion":2}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := raw.Exec(`INSERT INTO lyrics_source_review_batch_idempotency
				(actor,idempotency_key,request_sha256,gate,decision,items_json,item_count,note,decided_at)
				VALUES ('admin',?,?,'overall','approved',?,json_array_length(?),'',1)`,
				"batch-idempotency-"+strings.ReplaceAll(name, " ", "-"), strings.Repeat("b", 64), items, items); err == nil {
				t.Fatalf("v17 accepted invalid items %s", items)
			}
		})
	}
	if _, err := raw.Exec(`UPDATE lyrics_source_review_batch_idempotency SET note='changed' WHERE batch_id=1`); err == nil {
		t.Fatal("v17 ledger allowed update")
	}
	if _, err := raw.Exec(`DELETE FROM lyrics_source_review_batch_idempotency WHERE batch_id=1`); err == nil {
		t.Fatal("v17 ledger allowed delete")
	}
	var checksum string
	if err := raw.QueryRow(`SELECT checksum FROM schema_migrations WHERE version=17`).Scan(&checksum); err != nil || checksum != "665a877eef31d2882468c7ca8a29a0732f51b332996039281efc901bb23ea48a" {
		t.Fatalf("v17 checksum=%q err=%v", checksum, err)
	}
}

func TestV19EditableLyricsRubyRepairsLegacyEmptySegmentsAndEnforcesExactSpans(t *testing.T) {
	path := legacyFixtureCopy(t, "v19-editable-lyrics-ruby.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:18]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (1,'試験曲')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyrics(music_id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyric_lines(music_id,line_id,position,japanese,zh_cn,en_us) VALUES
		(1,'mixed',0,'歌詞','',''),(1,'repair',1,'修復','',''),(1,'empty',2,'','','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyric_segments(music_id,line_id,position,text,performer_ids_json) VALUES
		(1,'mixed',0,'','[]'),(1,'mixed',1,'歌詞','[]'),(1,'mixed',2,'','[]'),
		(1,'repair',4,'','[21]'),(1,'repair',7,'','[]'),(1,'empty',0,'','[]')`); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[18:19]); err != nil {
		t.Fatal(err)
	}

	rows, err := raw.Query(`SELECT line_id,position,text,performer_ids_json,ruby_json
		FROM song_lyric_segments WHERE music_id=1 ORDER BY line_id,position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type persistedSegment struct {
		lineID, text, performers, ruby string
		position                       int
	}
	var segments []persistedSegment
	for rows.Next() {
		var segment persistedSegment
		if err := rows.Scan(&segment.lineID, &segment.position, &segment.text, &segment.performers, &segment.ruby); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, segment)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].lineID != "mixed" || segments[0].position != 1 ||
		segments[0].text != "歌詞" || segments[0].ruby != `[{"text":"歌詞"}]` ||
		segments[1].lineID != "repair" || segments[1].position != 4 || segments[1].text != "修復" ||
		segments[1].performers != "[21]" || segments[1].ruby != `[{"text":"修復"}]` {
		t.Fatalf("v19 repaired segments=%+v", segments)
	}

	for name, statement := range map[string]string{
		"empty text": `INSERT INTO song_lyric_segments(music_id,line_id,position,text,performer_ids_json,ruby_json)
			VALUES (1,'mixed',3,'','[]','[]')`,
		"mismatched ruby": `INSERT INTO song_lyric_segments(music_id,line_id,position,text,performer_ids_json,ruby_json)
			VALUES (1,'mixed',3,'追記','[]','[{"text":"別"}]')`,
		"text update without ruby": `UPDATE song_lyric_segments SET text='変更' WHERE music_id=1 AND line_id='mixed' AND position=1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := raw.Exec(statement); err == nil {
				t.Fatalf("v19 trigger accepted %s", name)
			}
		})
	}
	if _, err := raw.Exec(`INSERT INTO song_lyric_segments(music_id,line_id,position,text,performer_ids_json)
		VALUES (1,'mixed',3,'旧式追記','[]')`); err != nil {
		t.Fatalf("v19 rejected the v18 insert shape: %v", err)
	}
	var legacyRuby string
	if err := raw.QueryRow(`SELECT ruby_json FROM song_lyric_segments WHERE music_id=1 AND line_id='mixed' AND position=3`).Scan(&legacyRuby); err != nil || legacyRuby != `[{"text":"旧式追記"}]` {
		t.Fatalf("v19 legacy insert normalization=%q err=%v", legacyRuby, err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyric_segments(music_id,line_id,position,text,performer_ids_json,ruby_json)
		VALUES (1,'mixed',4,'追記','[]','[{"text":"追","reading":"つい"},{"text":"記"}]')`); err != nil {
		t.Fatalf("v19 rejected exact ruby spans: %v", err)
	}
	var checksum string
	if err := raw.QueryRow(`SELECT checksum FROM schema_migrations WHERE version=19`).Scan(&checksum); err != nil || checksum != "6c2977cc4290ec56af216d1888e21ac64bbc281aa4b669e662840a5e75f3046b" {
		t.Fatalf("v19 checksum=%q err=%v", checksum, err)
	}
}

func TestV21V22BackfillLegacyFandomWithoutSyntheticProvenance(t *testing.T) {
	path := legacyFixtureCopy(t, "v21-v22-provider-backfill.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:20]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key,kind,state,music_id,catalog_fingerprint,policy_version,expected_sha1,fixed_candidate_json,
		 attempts,max_attempts,next_attempt_at,created_at,updated_at,version)
		VALUES (?,'discover','queued',10,?,'shadow-v1','','',0,3,1,1,1,1)`, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[20:22]); err != nil {
		t.Fatal(err)
	}
	var provider, status, fixedIdentity string
	if err := raw.QueryRow(`SELECT provider,provenance_status,fixed_identity_json FROM lyrics_discovery_jobs`).
		Scan(&provider, &status, &fixedIdentity); err != nil {
		t.Fatal(err)
	}
	if provider != "vocaloid_fandom" || status != "not_applicable" || fixedIdentity != "" {
		t.Fatalf("legacy job provider=%q status=%q fixedIdentity=%q", provider, status, fixedIdentity)
	}
	var renditions, documents int
	if err := raw.QueryRow(`SELECT (SELECT COUNT(*) FROM lyrics_source_renditions),
		(SELECT COUNT(*) FROM song_lyrics_source_documents)`).Scan(&renditions, &documents); err != nil {
		t.Fatal(err)
	}
	if renditions != 0 || documents != 0 {
		t.Fatalf("migration synthesized provenance renditions=%d documents=%d", renditions, documents)
	}
	if _, err := raw.Exec(`UPDATE lyrics_discovery_jobs SET provider='moegirl'`); err == nil {
		t.Fatal("provider-scoped legacy job identity was mutable")
	}
}

func TestV21RebuildPreservesLegacyArtifactsAndAdmitsProviderScopedOrigins(t *testing.T) {
	path := legacyFixtureCopy(t, "v21-provider-artifact-rebuild.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:20]); err != nil {
		t.Fatal(err)
	}
	legacyBytes := []byte("== Lyrics ==\nlegacy bytes")
	if _, err := raw.Exec(`INSERT INTO lyrics_source_artifacts
		(source_type,source_origin,page_id,revision_id,page_title,canonical_revision_url,mediawiki_sha1,categories_json,
		 raw_wikitext,raw_byte_count,raw_wikitext_sha256,artifact_sha256,first_fetched_at,first_creating_job_id,created_at)
		VALUES ('mediawiki','https://vocaloid.fandom.com',12,34,'Legacy Song',
		 'https://vocaloid.fandom.com/wiki/Legacy_Song?oldid=34',?,'[]',?,?,?, ?,1,1,1)`,
		strings.Repeat("a", 40), legacyBytes, len(legacyBytes), strings.Repeat("b", 64), strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[20:21]); err != nil {
		t.Fatal(err)
	}
	var provider, status string
	var storedBytes []byte
	if err := raw.QueryRow(`SELECT provider,provenance_status,raw_wikitext FROM lyrics_source_artifacts WHERE page_id=12`).
		Scan(&provider, &status, &storedBytes); err != nil {
		t.Fatal(err)
	}
	if provider != "vocaloid_fandom" || status != "rebuild_required" || string(storedBytes) != string(legacyBytes) {
		t.Fatalf("legacy artifact provider=%q status=%q bytes=%q", provider, status, storedBytes)
	}
	var foreignTarget string
	if err := raw.QueryRow(`SELECT "table" FROM pragma_foreign_key_list('lyrics_source_analyses') WHERE "from"='artifact_id'`).Scan(&foreignTarget); err != nil || foreignTarget != "lyrics_source_artifacts" {
		t.Fatalf("analysis foreign target=%q err=%v", foreignTarget, err)
	}
	moegirlBytes := []byte("== 歌词 ==\nprovider bytes")
	if _, err := raw.Exec(`INSERT INTO lyrics_source_artifacts
		(source_type,source_origin,page_id,revision_id,page_title,canonical_revision_url,mediawiki_sha1,categories_json,
		 raw_wikitext,raw_byte_count,raw_wikitext_sha256,artifact_sha256,first_fetched_at,first_creating_job_id,created_at,
		 provider,provenance_status)
		VALUES ('mediawiki','https://moegirl.icu',13,35,'Provider Song',
		 'https://moegirl.icu/index.php?oldid=35&title=Provider+Song',?,'[]',?,?,?, ?,1,1,1,
		 'moegirl','complete')`, strings.Repeat("d", 40), moegirlBytes, len(moegirlBytes),
		strings.Repeat("e", 64), strings.Repeat("f", 64)); err != nil {
		t.Fatalf("insert provider artifact: %v", err)
	}
}

func TestV20FixedCandidateIdentityBackfillsAndBecomesImmutable(t *testing.T) {
	path := legacyFixtureCopy(t, "v20-fixed-candidate.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:19]); err != nil {
		t.Fatal(err)
	}
	const fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	const sha1 = "0123456789abcdef0123456789abcdef01234567"
	result, err := raw.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key,kind,state,music_id,page_id,revision_id,catalog_fingerprint,policy_version,expected_sha1,
		 attempts,max_attempts,next_attempt_at,created_at,updated_at,version)
		VALUES (?,'fetch_revision','queued',10,12,34,?,'matching-v2',?,0,3,1,1,1,1)`, strings.Repeat("c", 64), fingerprint, sha1)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := result.LastInsertId()
	candidateJSON := `{"candidates":[{"pageId":12,"title":"合成試験曲","canonicalUrl":"https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34","revisionId":34,"sha1":"` + sha1 + `","categories":["Lyrics","Songs"]}]}`
	if _, err := raw.Exec(`INSERT INTO lyrics_discovery_shadow_results
		(job_id,music_id,catalog_fingerprint,policy_version,outcome,candidate_count,result_json,created_at)
		VALUES (?,10,?,'matching-v2','candidates_found',1,?,2)`, jobID, fingerprint, candidateJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_items
		(domain_key,kind,analysis_id,music_id,catalog_fingerprint,review_policy_version,reason_code,evidence_json,state,
		 identity_gate,source_use_gate,parse_gate,version,priority,created_at,updated_at)
		VALUES (?,'candidate_selection',NULL,10,?,'review-v1','ambiguous_candidates',?,'pending',
		 'not_applicable','not_applicable','not_applicable',1,0,1,1)`, strings.Repeat("e", 64), fingerprint, candidateJSON); err != nil {
		t.Fatal(err)
	}
	selectedJSON := `{"title":"合成試験曲","categories":["Lyrics","Songs"],"sha1":"` + sha1 + `","pageId":12,"canonicalUrl":"https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34","revisionId":34}`
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_decisions
		(review_id,gate,decision,selected_candidate_json,actor,note,idempotency_key,request_sha256,expected_version,result_version,decided_at)
		VALUES (1,'candidate','selected',?,'admin','','fixed-candidate-v20',?,1,2,2)`, selectedJSON, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[19:20]); err != nil {
		t.Fatal(err)
	}
	var schemaVersion, pageID, revisionID int
	var storedSHA1, title, canonicalURL, categories string
	if err := raw.QueryRow(`SELECT json_extract(fixed_candidate_json,'$.schemaVersion'),
		json_extract(fixed_candidate_json,'$.candidate.pageId'),json_extract(fixed_candidate_json,'$.candidate.revisionId'),
		json_extract(fixed_candidate_json,'$.candidate.sha1'),json_extract(fixed_candidate_json,'$.candidate.title'),
		json_extract(fixed_candidate_json,'$.candidate.canonicalUrl'),json_extract(fixed_candidate_json,'$.candidate.categories')
		FROM lyrics_discovery_jobs WHERE job_id=?`, jobID).Scan(&schemaVersion, &pageID, &revisionID, &storedSHA1, &title, &canonicalURL, &categories); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != 1 || pageID != 12 || revisionID != 34 || storedSHA1 != sha1 || title != "合成試験曲" ||
		canonicalURL != "https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34" || categories != `["Lyrics","Songs"]` {
		t.Fatalf("fixed identity schema=%d page=%d revision=%d sha=%q title=%q url=%q categories=%q",
			schemaVersion, pageID, revisionID, storedSHA1, title, canonicalURL, categories)
	}
	if _, err := raw.Exec(`UPDATE lyrics_discovery_jobs SET fixed_candidate_json=json_set(fixed_candidate_json,'$.candidate.title','改名') WHERE job_id=?`, jobID); err == nil {
		t.Fatal("v20 allowed fixed candidate identity mutation")
	}
}

func TestV20FixedCandidateIdentityFailsClosedWhenLegacyFetchCannotBeReconciled(t *testing.T) {
	path := legacyFixtureCopy(t, "v20-unreconcilable-fixed-candidate.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:19]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key,kind,state,music_id,page_id,revision_id,catalog_fingerprint,policy_version,expected_sha1,
		 attempts,max_attempts,next_attempt_at,created_at,updated_at,version)
		VALUES (?,'fetch_revision','queued',10,12,34,?,'matching-v2',?,0,3,1,1,1,1)`, strings.Repeat("c", 64),
		strings.Repeat("d", 64), strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[19:20]); err == nil || !strings.Contains(err.Error(), "unreconcilable legacy fixed candidate identity") {
		t.Fatalf("v20 migration error=%v", err)
	}
	var version, columns int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 19 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('lyrics_discovery_jobs') WHERE name='fixed_candidate_json'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("rolled-back fixed candidate columns=%d err=%v", columns, err)
	}
}

func TestV18StructuredLyricsAnalysisEvidenceIsStrictAndAdditive(t *testing.T) {
	path := legacyFixtureCopy(t, "v18-structured-lyrics-analysis.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:17]); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[17:18]); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"selected_version_json", "performers_json", "ruby_generator_version"} {
		var count int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('lyrics_source_analyses') WHERE name=?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("v18 column %s count=%d err=%v", column, count, err)
		}
	}
	var checksum string
	if err := raw.QueryRow(`SELECT checksum FROM schema_migrations WHERE version=18`).Scan(&checksum); err != nil || checksum != "9ef12f0d266c281cfae1b76f80a61eb6c5142fd64ea9a45d7b97e327216031ff" {
		t.Fatalf("v18 checksum=%q err=%v", checksum, err)
	}
}

func TestV36SeedsTheReviewedSekaipediaProviderMapsInMusicIDOrder(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "provider-targets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	rows, err := database.Query(`SELECT music_id,page_title,resolved_page_title,updated_at,updated_by
		FROM lyrics_provider_page_targets WHERE provider='sekaipedia' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type seededTarget struct {
		musicID                        int
		pageTitle, resolved, updatedBy string
		updatedAt                      int64
	}
	var targets []seededTarget
	for rows.Next() {
		var target seededTarget
		if err := rows.Scan(&target.musicID, &target.pageTitle, &target.resolved, &target.updatedAt, &target.updatedBy); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 37 {
		t.Fatalf("seeded page targets=%d want=37", len(targets))
	}
	previousMusicID := 0
	for _, target := range targets {
		if target.musicID <= previousMusicID || target.updatedBy != "migration-v36" || target.updatedAt != 1790000000 {
			t.Fatalf("seeded target=%+v previous=%d", target, previousMusicID)
		}
		previousMusicID = target.musicID
	}
	if targets[0].musicID != 50 || targets[0].pageTitle != "Blessing" || targets[0].resolved != "" {
		t.Fatalf("first seeded target=%+v", targets[0])
	}
	if targets[6].musicID != 148 || targets[6].pageTitle != "ray" || targets[6].resolved != "Ray" {
		t.Fatalf("resolved-title seeded target=%+v", targets[6])
	}
	if targets[36].musicID != 789 || targets[36].pageTitle != "Tenbin, Yubisaki de Furete" {
		t.Fatalf("last seeded target=%+v", targets[36])
	}

	aliasRows, err := database.Query(`SELECT music_id,updated_at,updated_by
		FROM lyrics_provider_contributor_aliases WHERE provider='sekaipedia' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer aliasRows.Close()
	aliases := 0
	previousMusicID = 0
	for aliasRows.Next() {
		var musicID int
		var updatedAt int64
		var updatedBy string
		if err := aliasRows.Scan(&musicID, &updatedAt, &updatedBy); err != nil {
			t.Fatal(err)
		}
		if musicID < previousMusicID || updatedBy != "migration-v36" || updatedAt != 1790000000 {
			t.Fatalf("seeded alias music=%d updatedAt=%d updatedBy=%q previous=%d", musicID, updatedAt, updatedBy, previousMusicID)
		}
		previousMusicID = musicID
		aliases++
	}
	if err := aliasRows.Err(); err != nil {
		t.Fatal(err)
	}
	if aliases != 26 {
		t.Fatalf("seeded aliases=%d want=26", aliases)
	}
	var vampire, alias string
	if err := database.QueryRow(`SELECT page_title FROM lyrics_provider_page_targets
		WHERE provider='sekaipedia' AND music_id=334`).Scan(&vampire); err != nil || vampire != "Vampire's ∞ pathoS" {
		t.Fatalf("escaped page title=%q err=%v", vampire, err)
	}
	if err := database.QueryRow(`SELECT provider_contributor FROM lyrics_provider_contributor_aliases
		WHERE provider='sekaipedia' AND music_id=334 AND catalog_contributor='やま△'`).Scan(&alias); err != nil || alias != "Yama△" {
		t.Fatalf("seeded alias=%q err=%v", alias, err)
	}
}
