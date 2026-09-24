package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type v38SQLExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

// seedV38RecoveryLedger writes a two-song recovery batch: music 1 is complete
// and pins a source document digest, music 2 is missing with its availability
// document.
func seedV38RecoveryLedger(t *testing.T, database v38SQLExecer) (batchSHA, documentSHA string) {
	t.Helper()
	h := func(character string) string { return strings.Repeat(character, 64) }
	batchSHA, documentSHA = h("a"), h("3")
	coverage := `{"total":2,"complete":1,"satisfiedNoLyrics":0,"catalogReview":0,"gameSizeEvidence":0,"ambiguous":0,"missing":1,"incomplete":0,"failed":0,"providerOutcomeRefCount":0,"selectionRefCount":0,"uniqueAcquisitionCount":0,"uniqueEvidenceCount":0}`
	availabilityJSON := `{"schemaVersion":1,"state":"missing","reasonCode":"version_conflict","fixedIdentities":[],"provenance":{}}`
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO catalog_music(music_id,title_ja) VALUES (1,'合成完整曲'),(2,'合成欠落曲')`, nil},
		{`INSERT INTO lyrics_recovery_import_batches
			(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
			 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
			 raw_byte_count,encoded_byte_count,actor,created_at)
			VALUES (?,1,2,'root-v38',?,2,?,?,?,?,?,0,0,0,0,'migration-test',1)`,
			[]any{batchSHA, h("b"), h("c"), coverage, h("d"), h("e"), h("f")}},
		{`INSERT INTO lyrics_recovery_import_items
			(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,
			 state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
			VALUES (?,1,'合成完整曲',?,1,'[]','complete',?,?,?,'',1),
			       (?,2,'合成欠落曲',?,2,'[]','missing',?,'','',?,1)`,
			[]any{batchSHA, h("1"), h("2"), h("4"), documentSHA, batchSHA, h("5"), h("6"), h("7")}},
		{`INSERT INTO song_lyrics_availability_documents
			(batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,document_sha256,result_sha256,created_at)
			VALUES (?,2,1,'missing','version_conflict','',?,?,?,1)`, []any{batchSHA, availabilityJSON, h("7"), h("6")}},
	} {
		if _, err := database.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed v38 recovery ledger: %v", err)
		}
	}
	return batchSHA, documentSHA
}

func TestMigrationV38OnlyCreatesTheTakeoverTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v37.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applySchema(); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[:37]); err != nil {
		t.Fatal(err)
	}
	seedV38RecoveryLedger(t, raw)
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
	var version int
	if err := migrated.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 38 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	afterSchema := v37SchemaObjects(t, migrated.DB)
	var added []string
	for name, definition := range afterSchema {
		before, existed := beforeSchema[name]
		if !existed {
			if !strings.HasPrefix(definition, "on lyrics_recovery_takeovers: ") {
				t.Fatalf("v38 created %s outside the takeover table: %s", name, definition)
			}
			added = append(added, name)
			continue
		}
		if before != definition {
			t.Fatalf("v38 changed %s:\nbefore %s\nafter  %s", name, before, definition)
		}
	}
	for name := range beforeSchema {
		if _, kept := afterSchema[name]; !kept {
			t.Fatalf("v38 dropped %s", name)
		}
	}
	sort.Strings(added)
	want := []string{
		"index/idx_lyrics_recovery_takeovers_item",
		"table/lyrics_recovery_takeovers",
		"trigger/lyrics_recovery_takeovers_immutable_delete",
		"trigger/lyrics_recovery_takeovers_immutable_update",
		"trigger/lyrics_recovery_takeovers_item_insert",
	}
	if strings.Join(added, ",") != strings.Join(want, ",") {
		t.Fatalf("v38 added %v want %v", added, want)
	}
	afterRows := v37TableRowDigests(t, migrated.DB)
	for table, digest := range beforeRows {
		if afterRows[table] != digest {
			t.Fatalf("v38 changed the rows of %s", table)
		}
	}
	var takeovers int
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM lyrics_recovery_takeovers`).Scan(&takeovers); err != nil || takeovers != 0 {
		t.Fatalf("v38 wrote takeover rows=%d err=%v", takeovers, err)
	}
}

func TestV38TakeoversMatchTheirItemAreImmutableAndCascadeWithTheLedger(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "v38.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	batchSHA, documentSHA := seedV38RecoveryLedger(t, database)
	document := `{"schemaVersion":3}`
	insert := func(musicID int, state string, sha any) error {
		var schemaVersion, reasonCode, documentJSON, createdAt any
		if sha != nil {
			schemaVersion, reasonCode, documentJSON, createdAt = 3, "", document, 5
		}
		_, err := database.Exec(`INSERT INTO lyrics_recovery_takeovers
			(music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
			 document_created_at,taken_over_at,taken_over_by) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			musicID, batchSHA, state, schemaVersion, reasonCode, documentJSON, sha, createdAt, 10, "document-admin")
		return err
	}
	for name, attempt := range map[string]func() error{
		"superseded digest differs from the item": func() error { return insert(1, "complete", strings.Repeat("9", 64)) },
		"complete item without its document":      func() error { return insert(1, "complete", nil) },
		"item state differs":                      func() error { return insert(1, "game_only", documentSHA) },
		"availability item with a document":       func() error { return insert(2, "missing", documentSHA) },
		"no recovery item":                        func() error { return insert(3, "missing", nil) },
	} {
		if err := attempt(); err == nil {
			t.Fatalf("%s: takeover accepted", name)
		}
	}
	if _, err := database.Exec(`INSERT INTO lyrics_recovery_takeovers
		(music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
		 document_created_at,taken_over_at,taken_over_by) VALUES (1,?,'complete',3,'',?,?,NULL,10,'x')`,
		batchSHA, document, documentSHA); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("partially NULL superseded document error=%v", err)
	}
	if err := insert(1, "complete", documentSHA); err != nil {
		t.Fatalf("complete takeover: %v", err)
	}
	if err := insert(2, "missing", nil); err != nil {
		t.Fatalf("availability-only takeover: %v", err)
	}
	if _, err := database.Exec(`UPDATE lyrics_recovery_takeovers SET taken_over_by='other' WHERE music_id=1`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("takeover update error=%v", err)
	}
	if _, err := database.Exec(`DELETE FROM lyrics_recovery_takeovers WHERE music_id=2`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("takeover delete error=%v", err)
	}
	// Content restore clears the catalog first and then the batches.
	if _, err := database.Exec(`DELETE FROM catalog_music`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM lyrics_recovery_import_batches`); err != nil {
		t.Fatalf("restore-style ledger removal: %v", err)
	}
	var remaining int
	if err := database.QueryRow(`SELECT COUNT(*) FROM lyrics_recovery_takeovers`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("takeovers after ledger removal=%d err=%v", remaining, err)
	}
}

func TestV38TakeoverLocalizationsBelongToASupersededSourceV3Document(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "v38-localizations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	batchSHA, documentSHA := seedV38RecoveryLedger(t, database)
	insert := func(musicID int, state string, schemaVersion int, localizations any) error {
		var version, reasonCode, documentJSON, sha, createdAt any
		if schemaVersion != 0 {
			version, reasonCode, documentJSON, sha, createdAt = schemaVersion, "", `{"schemaVersion":3}`, documentSHA, 5
		}
		_, err := database.Exec(`INSERT INTO lyrics_recovery_takeovers
			(music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
			 document_created_at,localizations_json,taken_over_at,taken_over_by) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			musicID, batchSHA, state, version, reasonCode, documentJSON, sha, createdAt, localizations, 10, "document-admin")
		return err
	}
	valid := `{"localizations":[{"renditionKey":"original","locale":"zh-CN"}]}`
	for name, attempt := range map[string]func() error{
		"not JSON":                          func() error { return insert(1, "complete", 3, `{"localizations":`) },
		"an array":                          func() error { return insert(1, "complete", 3, `[]`) },
		"no localizations member":           func() error { return insert(1, "complete", 3, `{}`) },
		"localizations is not an array":     func() error { return insert(1, "complete", 3, `{"localizations":{}}`) },
		"no localization rows":              func() error { return insert(1, "complete", 3, `{"localizations":[]}`) },
		"a blob":                            func() error { return insert(1, "complete", 3, []byte(valid)) },
		"a legacy v2 superseded document":   func() error { return insert(1, "complete", 2, valid) },
		"an availability-only takeover row": func() error { return insert(2, "missing", 0, valid) },
	} {
		if err := attempt(); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("%s: error=%v", name, err)
		}
	}
	if err := insert(1, "complete", 3, valid); err != nil {
		t.Fatalf("source v3 takeover with localizations: %v", err)
	}
	if err := insert(2, "missing", 0, nil); err != nil {
		t.Fatalf("availability-only takeover: %v", err)
	}
	var stored string
	if err := database.QueryRow(`SELECT localizations_json FROM lyrics_recovery_takeovers WHERE music_id=1`).Scan(&stored); err != nil || stored != valid {
		t.Fatalf("stored localizations=%q err=%v", stored, err)
	}
}
