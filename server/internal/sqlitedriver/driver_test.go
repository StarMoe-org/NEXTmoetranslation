package sqlitedriver

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestPragmaSQLiteDriverAppliesPragmasAcrossPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=cache_size(-20000)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var foreignKeys, busyTimeout, tempStore, cacheSize int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, err = %v; want 1", foreignKeys, err)
	}
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil || busyTimeout != 10000 {
		t.Fatalf("busy_timeout = %d, err = %v; want 10000", busyTimeout, err)
	}
	if err := db.QueryRow("PRAGMA temp_store").Scan(&tempStore); err != nil || tempStore != 2 {
		t.Fatalf("temp_store = %d, err = %v; want 2 (MEMORY)", tempStore, err)
	}
	if err := db.QueryRow("PRAGMA cache_size").Scan(&cacheSize); err != nil || cacheSize != -20000 {
		t.Fatalf("cache_size = %d, err = %v; want -20000", cacheSize, err)
	}

	if _, err := db.Exec(`
		CREATE TABLE parent(id INTEGER PRIMARY KEY);
		CREATE TABLE child(id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id));
	`); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO child(id, parent_id) VALUES (1, 999)`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("expected FOREIGN KEY constraint failed, got %v", err)
	}

	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(ON)&_pragma=trusted_schema(OFF)")
	if err != nil {
		t.Fatalf("sql.Open ro: %v", err)
	}
	defer ro.Close()

	var queryOnly, trustedSchema int
	if err := ro.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("query_only = %d, err = %v; want 1", queryOnly, err)
	}
	if err := ro.QueryRow("PRAGMA trusted_schema").Scan(&trustedSchema); err != nil || trustedSchema != 0 {
		t.Fatalf("trusted_schema = %d, err = %v; want 0", trustedSchema, err)
	}

	// Verify ReadOnly: true transactions use BEGIN DEFERRED and do not contend
	// with another ReadOnly: true transaction or an active writer under _txlock=immediate.
	db.SetMaxOpenConns(4)
	wtx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin writer tx: %v", err)
	}
	defer wtx.Rollback()
	if _, err := wtx.Exec(`INSERT INTO parent(id) VALUES (1)`); err != nil {
		t.Fatalf("insert in writer tx: %v", err)
	}
	rtx1, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin readonly tx 1 while writer active: %v", err)
	}
	defer rtx1.Rollback()
	rtx2, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin readonly tx 2 while writer + ro1 active: %v", err)
	}
	defer rtx2.Rollback()
	var cnt int
	if err := rtx1.QueryRow(`SELECT COUNT(*) FROM parent`).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("rtx1 snapshot count = %d, err = %v; want 0", cnt, err)
	}
	if err := rtx2.QueryRow(`SELECT COUNT(*) FROM parent`).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("rtx2 snapshot count = %d, err = %v; want 0", cnt, err)
	}
}
