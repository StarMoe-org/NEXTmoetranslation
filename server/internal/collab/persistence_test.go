package collab

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"moesekai/server/internal/db"
	_ "moesekai/server/internal/sqlitedriver"
)

const persistenceTestSchema = `
CREATE TABLE lyrics_collab_documents (
	music_id INTEGER PRIMARY KEY, schema_version INTEGER NOT NULL DEFAULT 1,
	epoch INTEGER NOT NULL, update_v1 BLOB NOT NULL, base_revision INTEGER NOT NULL,
	authority_sha256 TEXT NOT NULL, updated_at INTEGER NOT NULL,
	checkpointed_at INTEGER NOT NULL DEFAULT 0, checkpointed_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE lyrics_collab_updates (
	music_id INTEGER NOT NULL, epoch INTEGER NOT NULL, seq INTEGER NOT NULL,
	update_v1 BLOB NOT NULL, update_sha256 TEXT NOT NULL, update_size INTEGER NOT NULL,
	created_at INTEGER NOT NULL, PRIMARY KEY (music_id,epoch,seq)
);
CREATE TABLE lyrics_collab_checkpoints (
	checkpoint_id INTEGER PRIMARY KEY AUTOINCREMENT, music_id INTEGER NOT NULL,
	epoch INTEGER NOT NULL, base_revision INTEGER NOT NULL, new_revision INTEGER NOT NULL,
	base_authority_sha256 TEXT NOT NULL, new_authority_sha256 TEXT NOT NULL,
	actor TEXT NOT NULL, changed INTEGER NOT NULL, created_at INTEGER NOT NULL
);`

func newPersistenceFixture(t *testing.T) (*sqlitePersistence, *sql.DB) {
	t.Helper()
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "collaboration.db"))
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(8)
	raw.SetMaxIdleConns(8)
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(persistenceTestSchema); err != nil {
		t.Fatal(err)
	}
	return &sqlitePersistence{db: &db.DB{DB: raw}}, raw
}

func persistenceUpdates(t *testing.T) (snapshot, first, second []byte) {
	t.Helper()
	document := crdt.New()
	text := document.GetText("probe")
	document.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "a", nil) })
	snapshot = crdt.EncodeStateAsUpdateV1(document, nil)
	state := document.StateVector()
	document.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), "b", nil) })
	first = crdt.EncodeStateAsUpdateV1(document, state)
	state = document.StateVector()
	document.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), "c", nil) })
	second = crdt.EncodeStateAsUpdateV1(document, state)
	return snapshot, first, second
}

func seedPersistenceDocument(t *testing.T, raw *sql.DB, musicID int, epoch int64, revision int, authoritySHA string, snapshot []byte) {
	t.Helper()
	if _, err := raw.Exec(`INSERT INTO lyrics_collab_documents
		(music_id,schema_version,epoch,update_v1,base_revision,authority_sha256,updated_at)
		VALUES (?,1,?,?,?,?,1)`, musicID, epoch, snapshot, revision, authoritySHA); err != nil {
		t.Fatal(err)
	}
}

func probeText(t *testing.T, update []byte) string {
	t.Helper()
	document := crdt.New()
	if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
		t.Fatal(err)
	}
	return document.GetText("probe").ToString()
}

func TestStoreUpdateRejectsRetiredEpochAtomically(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, _ := persistenceUpdates(t)
	seedPersistenceDocument(t, raw, 10, 1, 0, strings.Repeat("a", 64), snapshot)
	if _, err := raw.Exec(`UPDATE lyrics_collab_documents SET epoch=2`); err != nil {
		t.Fatal(err)
	}
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e1", first); !errors.Is(err, ErrRetiredRoom) {
		t.Fatalf("StoreUpdate error=%v want ErrRetiredRoom", err)
	}
	var after []byte
	var logRows int
	if err := raw.QueryRow(`SELECT update_v1 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_updates`).Scan(&logRows); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(snapshot) || logRows != 0 {
		t.Fatalf("retired room changed snapshot=%v logRows=%d", string(after) != string(snapshot), logRows)
	}
}

func TestStoreUpdateAppendsChecksummedOrderedLogAndLoadMergesIt(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, _ := persistenceUpdates(t)
	seedPersistenceDocument(t, raw, 10, 3, 2, strings.Repeat("a", 64), snapshot)
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e3", first); err != nil {
		t.Fatal(err)
	}
	var seq, size int
	var stored, digest []byte
	if err := raw.QueryRow(`SELECT seq,update_v1,update_sha256,update_size FROM lyrics_collab_updates
		WHERE music_id=10 AND epoch=3`).Scan(&seq, &stored, &digest, &size); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(first)
	if seq != 1 || string(stored) != string(first) || string(digest) != hex.EncodeToString(expected[:]) || size != len(first) {
		t.Fatalf("stored log seq=%d size=%d digest=%q", seq, size, digest)
	}
	loaded, err := adapter.LoadDoc("lyrics-10-e3")
	if err != nil {
		t.Fatal(err)
	}
	if got := probeText(t, loaded); got != "ab" {
		t.Fatalf("merged text=%q want ab", got)
	}
}

func TestCompactFoldsOrderedLogAndDeletesOnlyCommittedRows(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, second := persistenceUpdates(t)
	seedPersistenceDocument(t, raw, 10, 4, 2, strings.Repeat("a", 64), snapshot)
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e4", first); err != nil {
		t.Fatal(err)
	}
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e4", second); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Compact(context.Background(), "lyrics-10-e4"); err != nil {
		t.Fatal(err)
	}
	var compacted []byte
	var logRows int
	if err := raw.QueryRow(`SELECT update_v1 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&compacted); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_updates WHERE music_id=10 AND epoch=4`).Scan(&logRows); err != nil {
		t.Fatal(err)
	}
	if logRows != 0 || probeText(t, compacted) != "abc" {
		t.Fatalf("compacted text=%q logRows=%d", probeText(t, compacted), logRows)
	}
}

func TestLoadDocUsesOneSnapshotWhileCompactionCommits(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, _ := persistenceUpdates(t)
	seedPersistenceDocument(t, raw, 10, 5, 2, strings.Repeat("a", 64), snapshot)
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e5", first); err != nil {
		t.Fatal(err)
	}
	snapshotRead := make(chan struct{})
	releaseLoad := make(chan struct{})
	var once sync.Once
	adapter.afterLoadSnapshot = func() {
		once.Do(func() {
			close(snapshotRead)
			<-releaseLoad
		})
	}
	loadDone := make(chan struct {
		update []byte
		err    error
	}, 1)
	go func() {
		update, err := adapter.LoadDoc("lyrics-10-e5")
		loadDone <- struct {
			update []byte
			err    error
		}{update: update, err: err}
	}()
	select {
	case <-snapshotRead:
	case <-time.After(2 * time.Second):
		t.Fatal("LoadDoc did not reach the snapshot boundary")
	}
	compactDone := make(chan error, 1)
	go func() { compactDone <- adapter.Compact(context.Background(), "lyrics-10-e5") }()
	select {
	case err := <-compactDone:
		if err != nil {
			close(releaseLoad)
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(releaseLoad)
		t.Fatal("compaction could not commit alongside the read-only WAL snapshot")
	}
	close(releaseLoad)
	result := <-loadDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got := probeText(t, result.update); got != "ab" {
		t.Fatalf("concurrent LoadDoc text=%q want ab", got)
	}
}

func TestCheckpointSnapshotAndLedgerCommitAtomicallyWithoutDroppingUpdates(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, _ := persistenceUpdates(t)
	oldSHA := strings.Repeat("a", 64)
	newSHA := strings.Repeat("b", 64)
	seedPersistenceDocument(t, raw, 10, 6, 2, oldSHA, snapshot)
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e6", first); err != nil {
		t.Fatal(err)
	}
	baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 6}, baseRevision: 2, authoritySHA256: oldSHA}
	if err := adapter.commitCheckpoint(context.Background(), baseline, snapshot, 3, newSHA, "editor", true); err != nil {
		t.Fatal(err)
	}
	var revision, checkpointRows, logRows int
	var authority string
	var compacted []byte
	if err := raw.QueryRow(`SELECT base_revision,authority_sha256 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&revision, &authority); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT update_v1 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&compacted); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_checkpoints WHERE
		music_id=10 AND epoch=6 AND base_revision=2 AND new_revision=3 AND
		base_authority_sha256=? AND new_authority_sha256=? AND actor='editor' AND changed=1`, oldSHA, newSHA).Scan(&checkpointRows); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_updates WHERE music_id=10 AND epoch=6`).Scan(&logRows); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || authority != newSHA || checkpointRows != 1 || logRows != 0 || probeText(t, compacted) != "ab" {
		t.Fatalf("revision=%d authority=%q checkpoints=%d logs=%d text=%q", revision, authority, checkpointRows, logRows, probeText(t, compacted))
	}
}

func TestCheckpointMergesUpdateCompactedAfterEncodeBeforeCommit(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, _ := persistenceUpdates(t)
	oldSHA := strings.Repeat("a", 64)
	newSHA := strings.Repeat("b", 64)
	seedPersistenceDocument(t, raw, 10, 9, 2, oldSHA, snapshot)

	// `snapshot` is the checkpoint encoding captured before the concurrent edit.
	// The edit is then persisted and compacted before commitCheckpoint runs. The
	// checkpoint must merge the already-compacted state instead of overwriting it.
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e9", first); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Compact(context.Background(), "lyrics-10-e9"); err != nil {
		t.Fatal(err)
	}
	baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 9}, baseRevision: 2, authoritySHA256: oldSHA}
	if err := adapter.commitCheckpoint(context.Background(), baseline, snapshot, 3, newSHA, "editor", true); err != nil {
		t.Fatal(err)
	}
	loaded, err := adapter.LoadDoc("lyrics-10-e9")
	if err != nil {
		t.Fatal(err)
	}
	if got := probeText(t, loaded); got != "ab" {
		t.Fatalf("checkpoint lost compacted concurrent update: text=%q", got)
	}
	var logs int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_updates WHERE music_id=10 AND epoch=9`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 0 {
		t.Fatalf("checkpoint left compacted log rows=%d", logs)
	}
}

func TestCheckpointLedgerFailureRollsBackSnapshotAdvance(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, _, _ := persistenceUpdates(t)
	oldSHA := strings.Repeat("a", 64)
	seedPersistenceDocument(t, raw, 10, 7, 2, oldSHA, snapshot)
	if _, err := raw.Exec(`CREATE TRIGGER reject_checkpoint BEFORE INSERT ON lyrics_collab_checkpoints
		BEGIN SELECT RAISE(ABORT,'checkpoint rejected'); END`); err != nil {
		t.Fatal(err)
	}
	baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 7}, baseRevision: 2, authoritySHA256: oldSHA}
	if err := adapter.commitCheckpoint(context.Background(), baseline, snapshot, 3, strings.Repeat("b", 64), "editor", true); err == nil {
		t.Fatal("checkpoint unexpectedly committed")
	}
	var revision int
	var authority string
	if err := raw.QueryRow(`SELECT base_revision,authority_sha256 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&revision, &authority); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || authority != oldSHA {
		t.Fatalf("failed ledger insert advanced snapshot revision=%d authority=%q", revision, authority)
	}
}

func TestCheckpointTxLeavesCommitAndRollbackToCaller(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		adapter, raw := newPersistenceFixture(t)
		snapshot, _, _ := persistenceUpdates(t)
		oldSHA := strings.Repeat("a", 64)
		newSHA := strings.Repeat("b", 64)
		seedPersistenceDocument(t, raw, 10, 11, 2, oldSHA, snapshot)
		baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 11}, baseRevision: 2, authoritySHA256: oldSHA}
		tx, err := raw.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.commitCheckpointTx(context.Background(), tx, baseline, snapshot, 3, newSHA, "editor", true); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		var revision, checkpoints int
		var authority string
		if err := raw.QueryRow(`SELECT base_revision,authority_sha256 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&revision, &authority); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_checkpoints`).Scan(&checkpoints); err != nil {
			t.Fatal(err)
		}
		if revision != 2 || authority != oldSHA || checkpoints != 0 {
			t.Fatalf("revision=%d authority=%q checkpoints=%d", revision, authority, checkpoints)
		}
	})

	t.Run("commit", func(t *testing.T) {
		adapter, raw := newPersistenceFixture(t)
		snapshot, _, _ := persistenceUpdates(t)
		oldSHA := strings.Repeat("a", 64)
		newSHA := strings.Repeat("b", 64)
		seedPersistenceDocument(t, raw, 10, 12, 2, oldSHA, snapshot)
		baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 12}, baseRevision: 2, authoritySHA256: oldSHA}
		tx, err := raw.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.commitCheckpointTx(context.Background(), tx, baseline, snapshot, 3, newSHA, "editor", true); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var revision, checkpoints int
		var authority string
		if err := raw.QueryRow(`SELECT base_revision,authority_sha256 FROM lyrics_collab_documents WHERE music_id=10`).Scan(&revision, &authority); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_checkpoints`).Scan(&checkpoints); err != nil {
			t.Fatal(err)
		}
		if revision != 3 || authority != newSHA || checkpoints != 1 {
			t.Fatalf("revision=%d authority=%q checkpoints=%d", revision, authority, checkpoints)
		}
	})
}

func TestFirstCheckpointReseedBumpsEpochAndClearsRetiredLog(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, first, _ := persistenceUpdates(t)
	oldSHA := strings.Repeat("a", 64)
	seedPersistenceDocument(t, raw, 10, 8, 0, oldSHA, snapshot)
	if err := adapter.StoreUpdateContext(context.Background(), "lyrics-10-e8", first); err != nil {
		t.Fatal(err)
	}
	saved := blankLyrics(10)
	saved.Revision = 1
	saved.UpdatedAt = "2026-08-14T00:00:00Z"
	_, newSHA, _, _, err := canonicalDocument(saved)
	if err != nil {
		t.Fatal(err)
	}
	baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 8}, baseRevision: 0, authoritySHA256: oldSHA}
	oldRoom, newRoom, err := adapter.reseedCheckpoint(context.Background(), baseline, saved, "editor", true)
	if err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var revision, logs, checkpoints int
	var authority string
	if err := raw.QueryRow(`SELECT epoch,base_revision,authority_sha256 FROM lyrics_collab_documents WHERE music_id=10`).
		Scan(&epoch, &revision, &authority); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_updates WHERE music_id=10`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_checkpoints WHERE
		music_id=10 AND epoch=8 AND base_revision=0 AND new_revision=1 AND
		base_authority_sha256=? AND new_authority_sha256=? AND actor='editor' AND changed=1`, oldSHA, newSHA).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if oldRoom != "lyrics-10-e8" || newRoom != "lyrics-10-e9" || epoch != 9 || revision != 1 || authority != newSHA || logs != 0 || checkpoints != 1 {
		t.Fatalf("rooms=%q/%q epoch=%d revision=%d authority=%q logs=%d checkpoints=%d",
			oldRoom, newRoom, epoch, revision, authority, logs, checkpoints)
	}
}

func TestReseedCheckpointTxLeavesEpochCommitToCaller(t *testing.T) {
	adapter, raw := newPersistenceFixture(t)
	snapshot, _, _ := persistenceUpdates(t)
	oldSHA := strings.Repeat("a", 64)
	seedPersistenceDocument(t, raw, 10, 13, 0, oldSHA, snapshot)
	saved := blankLyrics(10)
	saved.Revision = 1
	saved.UpdatedAt = "2026-08-14T00:00:00Z"
	baseline := persistedDocument{roomIdentity: roomIdentity{musicID: 10, epoch: 13}, baseRevision: 0, authoritySHA256: oldSHA}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	oldRoom, newRoom, err := adapter.reseedCheckpointTx(context.Background(), tx, baseline, saved, "editor", true)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if oldRoom != "lyrics-10-e13" || newRoom != "lyrics-10-e14" {
		_ = tx.Rollback()
		t.Fatalf("rooms=%q/%q", oldRoom, newRoom)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var revision, checkpoints int
	if err := raw.QueryRow(`SELECT epoch,base_revision FROM lyrics_collab_documents WHERE music_id=10`).Scan(&epoch, &revision); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_collab_checkpoints`).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if epoch != 13 || revision != 0 || checkpoints != 0 {
		t.Fatalf("epoch=%d revision=%d checkpoints=%d", epoch, revision, checkpoints)
	}
}
