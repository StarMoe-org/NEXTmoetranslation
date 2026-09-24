package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var v39SideStoryTables = []string{"side_stories", "side_story_episodes", "side_story_lines", "side_story_line_localizations"}

// isV39SideStoryObject reports whether a v37SchemaObjects definition belongs
// to a table migration v39 creates.
func isV39SideStoryObject(definition string) bool {
	for _, table := range v39SideStoryTables {
		if strings.HasPrefix(definition, "on "+table+": ") {
			return true
		}
	}
	return false
}

func TestMigrationV39UpgradesAV38DatabaseByOnlyAddingSideStoryTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v38.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applySchema(); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[:38]); err != nil {
		t.Fatal(err)
	}
	batchSHA, documentSHA := seedV38RecoveryLedger(t, raw)
	if _, err := raw.Exec(`INSERT INTO lyrics_recovery_takeovers
		(music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
		 document_created_at,taken_over_at,taken_over_by) VALUES (1,?,'complete',3,'',?,?,5,10,'migration-test')`,
		batchSHA, `{"schemaVersion":3}`, documentSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO entries(category,field,jp_key,cn_text,source,updated_at,updated_by)
		VALUES ('cards','prefix','テストキー','测试译文一','human',1,'migration-test')`); err != nil {
		t.Fatal(err)
	}
	if err := database.ensureRuntimeInvariants(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeSchema := v37SchemaObjects(t, raw)
	beforeRows := v37TableRowDigests(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var version, count int
	if err := migrated.QueryRow(`SELECT MAX(version),COUNT(*) FROM schema_migrations`).Scan(&version, &count); err != nil ||
		version != 39 || count != 39 {
		t.Fatalf("schema version=%d count=%d err=%v", version, count, err)
	}
	if _, err := os.Stat(path + ".pre-migration-v39.bak"); err != nil {
		t.Fatalf("pre-migration backup: %v", err)
	}
	afterSchema := v37SchemaObjects(t, migrated.DB)
	var added []string
	for name, definition := range afterSchema {
		before, existed := beforeSchema[name]
		if !existed {
			if !isV39SideStoryObject(definition) {
				t.Fatalf("v39 created %s outside the side-story tables: %s", name, definition)
			}
			added = append(added, name)
			continue
		}
		if before != definition {
			t.Fatalf("v39 changed %s:\nbefore %s\nafter  %s", name, before, definition)
		}
	}
	for name := range beforeSchema {
		if _, kept := afterSchema[name]; !kept {
			t.Fatalf("v39 dropped %s", name)
		}
	}
	sort.Strings(added)
	want := []string{
		"index/idx_side_story_line_localizations_locale",
		"index/sqlite_autoindex_side_stories_1",
		"index/sqlite_autoindex_side_story_episodes_1",
		"index/sqlite_autoindex_side_story_line_localizations_1",
		"index/sqlite_autoindex_side_story_lines_1",
		"table/side_stories",
		"table/side_story_episodes",
		"table/side_story_line_localizations",
		"table/side_story_lines",
	}
	if strings.Join(added, ",") != strings.Join(want, ",") {
		t.Fatalf("v39 added %v want %v", added, want)
	}
	afterRows := v37TableRowDigests(t, migrated.DB)
	for _, table := range v39SideStoryTables {
		if afterRows[table] == "" || !strings.HasPrefix(afterRows[table], "0 rows ") {
			t.Fatalf("v39 table %s after upgrade: %q", table, afterRows[table])
		}
		delete(afterRows, table)
	}
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("table count before=%d after=%d", len(beforeRows), len(afterRows))
	}
	for table, digest := range beforeRows {
		if afterRows[table] != digest {
			t.Fatalf("v39 changed the rows of %s", table)
		}
	}
	insertV39SideStoryGraph(t, migrated.DB)
}

// insertV39SideStoryGraph stores one card story with an episode, a line and
// its zh-CN row, all synthetic test data.
func insertV39SideStoryGraph(t *testing.T, database *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO side_stories(kind,story_id,title,character_id,released_at,updated_at) VALUES ('card','12','テスト題',1,1700000000000,1)`,
		`INSERT INTO side_story_episodes(kind,story_id,episode_key,scenario_id,title_jp,position,jp_asset_path,cn_state,en_state)
			VALUES ('card','12','1','test_card_01','テスト前編',1,'character/member/res001_no012/test_card_01','pending','absent')`,
		`INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,speaker,position) VALUES ('card','12','1','テスト台詞','talk','テスト話者',0)`,
		`INSERT INTO side_story_line_localizations(kind,story_id,episode_key,jp_key,locale,text,source,revision,updated_by,updated_at)
			VALUES ('card','12','1','テスト台詞','zh-CN','测试台词一','human',1,'tester',1)`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestV39SideStoryConstraintsRejectInvalidRows(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version int
	if err := database.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 39 {
		t.Fatalf("fresh schema version=%d err=%v", version, err)
	}
	insertV39SideStoryGraph(t, database.DB)
	if _, err := database.Exec(`INSERT INTO side_stories(kind,story_id,title,action_set_id) VALUES ('area','areatalk_ev_test_001','テスト区域',501)`); err != nil {
		t.Fatalf("valid area story: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO side_story_line_localizations(kind,story_id,episode_key,jp_key,locale,text,source,revision)
		VALUES ('card','12','1','テスト台詞','en-US','','llm',3)`); err != nil {
		t.Fatalf("valid empty en-US row: %v", err)
	}
	episode := `INSERT INTO side_story_episodes(kind,story_id,episode_key,scenario_id,jp_asset_path,cn_state,en_state,script_sha256)
		VALUES ('card','12',?,'test_card_02','character/member/res001_no012/test_card_02',?,?,?)`
	localization := `INSERT INTO side_story_line_localizations(kind,story_id,episode_key,jp_key,locale,text,source,revision)
		VALUES ('card','12','1','テスト台詞',?,'测试台词二',?,?)`
	for name, attempt := range map[string]func() (sql.Result, error){
		"unknown kind": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_stories(kind,story_id) VALUES ('event','13')`)
		},
		"card id with a leading zero": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_stories(kind,story_id) VALUES ('card','012')`)
		},
		"card id with ten digits": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_stories(kind,story_id) VALUES ('card','1234567890')`)
		},
		"card with an action set": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_stories(kind,story_id,action_set_id) VALUES ('card','14',3)`)
		},
		"area id with a slash": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_stories(kind,story_id,action_set_id) VALUES ('area','area/talk',3)`)
		},
		"area without an action set": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_stories(kind,story_id) VALUES ('area','areatalk_test_002')`)
		},
		"third card episode": func() (sql.Result, error) {
			return database.Exec(episode, "3", "pending", "pending", "")
		},
		"unknown CN state": func() (sql.Result, error) {
			return database.Exec(episode, "2", "done", "pending", "")
		},
		"short script digest": func() (sql.Result, error) {
			return database.Exec(episode, "2", "pending", "pending", "abc")
		},
		"episode of an unknown story": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_story_episodes(kind,story_id,episode_key,scenario_id,jp_asset_path)
				VALUES ('card','99','1','test_card_99','character/member/res/test_card_99')`)
		},
		"orphan line": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,position) VALUES ('card','12','2','テスト孤立','talk',0)`)
		},
		"title line off position -1": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,position) VALUES ('card','12','1','テスト題','title',0)`)
		},
		"unknown role": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,position) VALUES ('card','12','1','テスト他','body',2)`)
		},
		"ja-JP locale": func() (sql.Result, error) { return database.Exec(localization, "ja-JP", "human", 1) },
		"unknown source": func() (sql.Result, error) {
			return database.Exec(localization, "en-US", "official_cn", 1)
		},
		"revision 0": func() (sql.Result, error) {
			_, err := database.Exec(`DELETE FROM side_story_line_localizations WHERE locale='en-US'`)
			if err != nil {
				return nil, err
			}
			return database.Exec(localization, "en-US", "human", 0)
		},
		"orphan localization": func() (sql.Result, error) {
			return database.Exec(`INSERT INTO side_story_line_localizations(kind,story_id,episode_key,jp_key,locale,text,source,revision)
				VALUES ('card','12','1','テスト無行','zh-CN','测试台词三','human',1)`)
		},
	} {
		if _, err := attempt(); err == nil ||
			!(strings.Contains(err.Error(), "CHECK constraint failed") || strings.Contains(err.Error(), "FOREIGN KEY constraint failed")) {
			t.Errorf("%s: error=%v", name, err)
		}
	}
}

func TestV39SideStoryRowsCascadeFromStoryToLocalization(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	counts := func() string {
		var parts []string
		for _, table := range v39SideStoryTables {
			var n int
			if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, table+"="+strconv.Itoa(n))
		}
		return strings.Join(parts, " ")
	}
	insertV39SideStoryGraph(t, database.DB)
	if _, err := database.Exec(`DELETE FROM side_story_lines`); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != "side_stories=1 side_story_episodes=1 side_story_lines=0 side_story_line_localizations=0" {
		t.Fatalf("after line delete: %s", got)
	}
	if _, err := database.Exec(`DELETE FROM side_stories`); err != nil {
		t.Fatal(err)
	}
	insertV39SideStoryGraph(t, database.DB)
	if _, err := database.Exec(`DELETE FROM side_story_episodes`); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != "side_stories=1 side_story_episodes=0 side_story_lines=0 side_story_line_localizations=0" {
		t.Fatalf("after episode delete: %s", got)
	}
	if _, err := database.Exec(`DELETE FROM side_stories`); err != nil {
		t.Fatal(err)
	}
	insertV39SideStoryGraph(t, database.DB)
	if got := counts(); got != "side_stories=1 side_story_episodes=1 side_story_lines=1 side_story_line_localizations=1" {
		t.Fatalf("seeded graph: %s", got)
	}
	if _, err := database.Exec(`DELETE FROM side_stories WHERE kind='card' AND story_id='12'`); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != "side_stories=0 side_story_episodes=0 side_story_lines=0 side_story_line_localizations=0" {
		t.Fatalf("after story delete: %s", got)
	}
}
