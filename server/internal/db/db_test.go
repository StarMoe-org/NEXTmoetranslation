package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRunsIntegrityCheckBeforeReturning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a corrupt SQLite database")
	}
}

func TestOpenSecuresDatabaseAndParentDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-data")
	path := filepath.Join(root, "production.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	assertMode := func(target string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode=%#o want %#o", target, got, want)
		}
	}
	assertMode(root, 0o700)
	assertMode(path, 0o600)

	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	assertMode(root, 0o700)
	assertMode(path, 0o600)
}

// TestReadsNotBlockedByLongWrite is the regression guard for the production
// incident where SetMaxOpenConns(1) serialized every query behind the daily
// cn-sync write transaction, turning sync windows into multi-minute stalls.
//
// It opens a write transaction, holds it open, and asserts that a concurrent
// read still completes quickly. With a single-connection pool this read would
// block until the writer commits (or time out); with WAL + a real pool it
// returns immediately.
func TestReadsNotBlockedByLongWrite(t *testing.T) {
	d := openTemp(t)

	if _, err := d.Exec(`INSERT INTO entries (category, field, jp_key, cn_text) VALUES ('cards','prefix','jp','cn')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Begin a write transaction and keep it open (simulating a long cn-sync).
	tx, err := d.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE entries SET cn_text='changed' WHERE jp_key='jp'`); err != nil {
		t.Fatalf("tx write: %v", err)
	}

	// A concurrent reader must not be blocked by the open writer.
	done := make(chan error, 1)
	go func() {
		var n int
		done <- d.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent read failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent read blocked by open write transaction — connection pool is starved (SetMaxOpenConns too low?)")
	}
}

// TestConcurrentWritersQueueNotDeadlock verifies that with _txlock=immediate two
// writers serialize via busy_timeout rather than deadlocking on lock upgrade
// (the SQLITE_BUSY trap that appears once the pool allows more than one conn).
func TestConcurrentWritersQueueNotDeadlock(t *testing.T) {
	d := openTemp(t)

	write := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tx, err := d.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO entries (category, field, jp_key, cn_text) VALUES ('cards','prefix',?, 'x')`,
			time.Now().UnixNano()); err != nil {
			tx.Rollback()
			return err
		}
		time.Sleep(50 * time.Millisecond) // hold the write lock briefly
		return tx.Commit()
	}

	errs := make(chan error, 2)
	go func() { errs <- write() }()
	go func() { errs <- write() }()

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("concurrent writer failed (deadlock or busy timeout?): %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent writers did not complete — possible deadlock")
		}
	}
}

func TestRuntimeLyricsOwnershipTriggersRejectFutureMixedWrites(t *testing.T) {
	insertSource := func(t *testing.T, database *DB, musicID, schemaVersion int, reasonCode, digestSeed string) error {
		t.Helper()
		_, err := database.Exec(`INSERT INTO song_lyrics_source_documents
			(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
			VALUES (?,?,?,?,?,?,1)`, musicID, schemaVersion, reasonCode, "{}", strings.Repeat(digestSeed, 64), strings.Repeat("f", 64))
		return err
	}

	t.Run("source v3 after legacy", func(t *testing.T) {
		database := openTemp(t)
		if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (701,'legacy first')`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (701)`); err != nil {
			t.Fatal(err)
		}
		if err := insertSource(t, database, 701, 3, "", "a"); err == nil || !strings.Contains(err.Error(), "source v3 cannot coexist") {
			t.Fatalf("source-v3 insert after legacy error=%v", err)
		}
	})

	t.Run("legacy after source v3", func(t *testing.T) {
		database := openTemp(t)
		if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (702,'source first')`); err != nil {
			t.Fatal(err)
		}
		if err := insertSource(t, database, 702, 3, "", "b"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (702)`); err == nil || !strings.Contains(err.Error(), "legacy editable lyrics cannot coexist") {
			t.Fatalf("legacy insert after source-v3 error=%v", err)
		}
	})

	t.Run("legacy update while source v3 exists", func(t *testing.T) {
		database := openTemp(t)
		if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (703,'historical mixed')`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (703)`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`DROP TRIGGER song_lyrics_source_v3_reject_legacy_insert`); err != nil {
			t.Fatal(err)
		}
		if err := insertSource(t, database, 703, 3, "", "c"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE song_lyrics SET revision=1 WHERE music_id=703`); err == nil || !strings.Contains(err.Error(), "legacy editable lyrics cannot coexist") {
			t.Fatalf("legacy update with source-v3 error=%v", err)
		}
	})

	t.Run("legacy source v2 remains compatible", func(t *testing.T) {
		database := openTemp(t)
		if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (704,'legacy v2')`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (704)`); err != nil {
			t.Fatal(err)
		}
		if err := insertSource(t, database, 704, 2, "untagged_full_only", "d"); err != nil {
			t.Fatalf("legacy source-v2 ownership was rejected: %v", err)
		}
	})

	t.Run("native source v3 remains immutable without legacy or localization rows", func(t *testing.T) {
		database := openTemp(t)
		if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (705,'native source v3')`); err != nil {
			t.Fatal(err)
		}
		if err := insertSource(t, database, 705, 3, "", "e"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`DELETE FROM song_lyrics_source_documents WHERE music_id=705`); err == nil || !strings.Contains(err.Error(), "source v3 documents are immutable") {
			t.Fatalf("native source-v3 delete error=%v", err)
		}
	})
}

func TestOpenRejectsHistoricalMixedLyricsStorageOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (700,'混在テスト')`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (700)`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`DROP TRIGGER song_lyrics_source_v3_reject_legacy_insert`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (700,3,'','{}',?, ?,1)`, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
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

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "mixed lyrics storage ownership for music 700") {
		t.Fatalf("historical mixed lyrics storage was accepted: %v", err)
	}
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
