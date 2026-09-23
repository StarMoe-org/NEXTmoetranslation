package lyricsacquisition

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const ledgerLockHelperEnvironment = "MOESEKAI_LYRICS_ACQUISITION_LOCK_HELPER"

func TestExistingUnrecognizedLedgerDoesNotGainLockNamespace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unrecognized-ledger")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{blobsDirectory, manifestsDirectory, pendingDirectory} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, metadataFileName), []byte("unrecognized metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(t.Context(), root); err == nil || !strings.Contains(err.Error(), "lock namespace") {
		t.Fatalf("OpenLedger unrecognized-root error=%v", err)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(directoryEntryNames(before), directoryEntryNames(after)) {
		t.Fatalf("unrecognized ledger changed: before=%v after=%v", directoryEntryNames(before), directoryEntryNames(after))
	}
	if _, err := os.Lstat(filepath.Join(root, ledgerLockName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenLedger created a fresh lock namespace: %v", err)
	}
}

func TestLedgerRootLockIsRetainedCrossProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestLedgerRootLockHelper$", "-test.v")
	command.Env = append(os.Environ(), ledgerLockHelperEnvironment+"="+root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("lock helper failed: %v\n%s", err, output)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(t.Context(), root)
	if err != nil {
		t.Fatalf("ledger lock was not released by Close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerRootLockHelper(t *testing.T) {
	root := os.Getenv(ledgerLockHelperEnvironment)
	if root == "" {
		t.Skip("lock helper")
	}
	ledger, err := OpenLedger(t.Context(), root)
	if err == nil {
		_ = ledger.Close()
		t.Fatal("cross-process OpenLedger acquired an already-retained ledger-root lock")
	}
	if !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("cross-process lock error=%v", err)
	}
}

func TestLedgerRootDirectoryLockSurvivesLockLeafReplacementCrossProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, ledgerLockName)
	replacement := filepath.Join(t.TempDir(), "replacement-lock")
	if err := os.WriteFile(replacement, ledgerLockBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, lockPath); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestLedgerRootLockHelper$", "-test.v")
	command.Env = append(os.Environ(), ledgerLockHelperEnvironment+"="+root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("replacement-lock helper failed: %v\n%s", err, output)
	}
	body, readErr := os.ReadFile(lockPath)
	if readErr != nil || !bytes.Equal(body, ledgerLockBody) {
		t.Fatalf("replacement lock was touched: body=%q err=%v", body, readErr)
	}
	_ = ledger.Close()
}

func TestReplayRejectsSameByteLeafInodeReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("same-byte leaf raw"), []byte("same-byte leaf evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, blobsDirectory, committed.rawResponseSHA256)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("test did not replace the leaf inode")
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "replaced after validation") {
		t.Fatalf("same-byte replacement replay error=%v", err)
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, body) {
		t.Fatalf("replacement leaf was mutated: err=%v", err)
	}
}

func TestCommitRejectsLeafThatAppearsAfterValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("post-validation raw"), []byte("post-validation evidence"))
	path := filepath.Join(root, blobsDirectory, response.rawResponseSHA256)
	if err := os.WriteFile(path, response.rawResponse, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.commit(t.Context(), response); err == nil || !strings.Contains(err.Error(), "appeared after validation") {
		t.Fatalf("post-validation leaf insertion commit error=%v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, response.rawResponse) {
		t.Fatalf("post-validation leaf was mutated: err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, pendingDirectory))
	if err != nil || len(entries) != 0 {
		t.Fatalf("commit mutated pending state before rejecting inserted leaf: entries=%v err=%v", entries, err)
	}
}

func TestReplayRejectsSameByteLockInodeReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("lock swap raw"), []byte("lock swap evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ledgerLockName)
	replacement := filepath.Join(t.TempDir(), "replacement-lock")
	if err := os.WriteFile(replacement, ledgerLockBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "lock pathname") {
		t.Fatalf("same-byte lock replacement replay error=%v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, ledgerLockBody) {
		t.Fatalf("replacement lock was mutated: err=%v", err)
	}
}

func TestMetadataSnapshotPublicationDoesNotReplaceASubstitutedSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, metadataFileName)
	originalPath := filepath.Join(root, "metadata-original.db")
	sentinelPath := filepath.Join(t.TempDir(), "sentinel")
	sentinelBody := []byte("metadata symlink target must remain untouched")
	if err := os.WriteFile(sentinelPath, sentinelBody, 0o600); err != nil {
		t.Fatal(err)
	}
	triggered := false
	opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		if boundary != metadataBoundaryBeforeSnapshotWrite {
			return nil
		}
		triggered = true
		if err := os.Rename(metadataPath, originalPath); err != nil {
			return err
		}
		return os.Symlink(sentinelPath, metadataPath)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("metadata symlink raw"), []byte("metadata symlink evidence"))
	if _, err := opened.commit(t.Context(), response); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("metadata symlink substitution commit error=%v", err)
	}
	if !triggered {
		t.Fatal("metadata snapshot publication hook was not reached")
	}
	opened.hooks.metadataBoundary = nil
	info, err := os.Lstat(metadataPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pre-publication verification overwrote or deleted the substituted symlink: info=%v err=%v", info, err)
	}
	retainedStage := filepath.Join(root, metadataSnapshotTempName)
	retainedInfo, retainedErr := os.Lstat(retainedStage)
	if retainedErr != nil || !retainedInfo.Mode().IsRegular() {
		t.Fatalf("failed publication did not retain its bounded regular stage: info=%v err=%v", retainedInfo, retainedErr)
	}
	retained, err := os.ReadFile(sentinelPath)
	if err != nil || !bytes.Equal(retained, sentinelBody) {
		t.Fatalf("metadata symlink target was mutated: body=%q err=%v", retained, err)
	}
	_ = opened.Close()
}

func TestSQLiteReconnectRejectsSameByteMetadataInodeReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("metadata reconnect raw"), []byte("metadata reconnect evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := opened.database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		connection.Close()
		t.Fatalf("discard SQLite connection error=%v", err)
	}
	_ = connection.Close()
	path := activeMetadataPathForSpool(t, opened)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement-metadata.db")
	if err := os.WriteFile(replacement, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("test did not replace the metadata inode")
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("metadata reconnect replacement replay error=%v", err)
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, body) {
		t.Fatalf("replacement metadata was mutated: err=%v", err)
	}
}

func TestSQLiteReconnectRaceDoesNotOpenOrMutateSubstitutedMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("metadata race raw"), []byte("metadata race evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	opened.database.SetMaxIdleConns(0)
	path := activeMetadataPathForSpool(t, opened)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement-metadata.db")
	if err := os.WriteFile(replacement, body, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := os.Lstat(replacement)
	if err != nil {
		t.Fatal(err)
	}
	moved := path + ".original"
	triggered := false
	testHookBeforeSQLiteRuntimeOpen = func() error {
		if triggered {
			return nil
		}
		triggered = true
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.Rename(replacement, path)
	}
	t.Cleanup(func() { testHookBeforeSQLiteRuntimeOpen = nil })
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("SQLite reconnect race error=%v", err)
	}
	if !triggered {
		t.Fatal("SQLite reconnect race hook was not reached")
	}
	replacementBody, readErr := os.ReadFile(path)
	replacementAfter, statErr := os.Lstat(path)
	if readErr != nil || statErr != nil || !bytes.Equal(replacementBody, body) || !os.SameFile(replacementBefore, replacementAfter) {
		t.Fatalf("substituted metadata was opened for mutation: read=%v stat=%v", readErr, statErr)
	}
	testHookBeforeSQLiteRuntimeOpen = nil
	_ = opened.Close()
}

func TestSQLiteRestoreRaceReadsOnlyPinnedDescriptorAndDoesNotTouchSubstitute(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("metadata restore raw"), []byte("metadata restore evidence"))
	if _, err := opened.commit(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	path := activeDiskMetadataPath(t, root)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement-metadata.db")
	if err := os.WriteFile(replacement, body, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := os.Lstat(replacement)
	if err != nil {
		t.Fatal(err)
	}
	moved := path + ".original"
	triggered := false
	testHookBeforeSQLiteRestore = func() error {
		triggered = true
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.Rename(replacement, path)
	}
	t.Cleanup(func() { testHookBeforeSQLiteRestore = nil })
	if reopened, err := openSpool(t.Context(), root); err == nil {
		_ = reopened.Close()
		t.Fatal("SQLite restore accepted a substituted metadata pathname")
	} else if !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("SQLite restore substitution error=%v", err)
	}
	if !triggered {
		t.Fatal("SQLite restore race hook was not reached")
	}
	replacementBody, readErr := os.ReadFile(path)
	replacementAfter, statErr := os.Lstat(path)
	if readErr != nil || statErr != nil || !bytes.Equal(replacementBody, body) || !os.SameFile(replacementBefore, replacementAfter) {
		t.Fatalf("substituted restore metadata was touched: read=%v stat=%v", readErr, statErr)
	}
}

func TestReplayRejectsSameInodeMetadataByteMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("metadata byte raw"), []byte("metadata byte evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	path := activeMetadataPathForSpool(t, opened)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), body...)
	mutated[len(mutated)-1] ^= 1
	if err := os.WriteFile(path, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("test unexpectedly replaced metadata inode: err=%v", err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "bytes changed") {
		t.Fatalf("same-inode metadata mutation replay error=%v", err)
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, mutated) {
		t.Fatalf("mutated metadata was repaired or changed: err=%v", err)
	}
	_ = opened.Close()
}

func TestReplayRejectsAncestorAndRootSwapsWithoutTouchingReplacement(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, "container")
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(container, "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("ancestor swap raw"), []byte("ancestor swap evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	originalContainer := filepath.Join(parent, "container-original")
	if err := os.Rename(container, originalContainer); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementRoot := filepath.Join(container, "ledger")
	if err := os.Mkdir(replacementRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(replacementRoot, "sentinel")
	if err := os.WriteFile(sentinel, []byte("replacement root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "ancestor pathname") {
		t.Fatalf("ancestor swap replay error=%v", err)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "replacement root" {
		t.Fatalf("replacement root was touched: body=%q err=%v", body, err)
	}
}

func TestReplayRejectsRootSwapWithoutTouchingReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("root swap raw"), []byte("root swap evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	originalRoot := filepath.Join(parent, "ledger-original")
	if err := os.Rename(root, originalRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("replacement root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || !strings.Contains(err.Error(), "root pathname") {
		t.Fatalf("root swap replay error=%v", err)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "replacement root" {
		t.Fatalf("replacement root was touched: body=%q err=%v", body, err)
	}
}

func TestRecoveryAcceptsOnlyExactInterruptedPublicationHardlinkPair(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("interrupted link raw"), []byte("interrupted link evidence"))
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	_, markerBody, err := buildPendingMarker(manifest, acquisitionID, key, len(manifestBody))
	if err != nil {
		t.Fatal(err)
	}
	stagedName := acquisitionID + ".marker.tmp"
	if _, err := writeStagedFile(opened.root, stagedName, markerBody); err != nil {
		t.Fatal(err)
	}
	pending := opened.root.directories[pendingDirectory].file
	if err := unix.Linkat(int(pending.Fd()), stagedName, int(pending.Fd()), acquisitionID+".json", 0); err != nil {
		t.Fatal(err)
	}
	if err := opened.root.syncDirectory(pendingDirectory); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatalf("recover exact interrupted publication pair: %v", err)
	}
	defer recovered.Close()
	entries, err := os.ReadDir(filepath.Join(root, pendingDirectory))
	want := []string{acquisitionID + ".json", stagedName}
	if err != nil || !reflect.DeepEqual(directoryEntryNames(entries), want) {
		t.Fatalf("retained exact interrupted pair=%v want=%v err=%v", directoryEntryNames(entries), want, err)
	}
}

func TestSyntheticOwnerMismatchDecisionsRunWithoutRootPrivileges(t *testing.T) {
	mismatchedOwner := uint32(os.Geteuid() + 1)
	if mismatchedOwner == 0 || int(mismatchedOwner) == os.Geteuid() {
		mismatchedOwner = uint32(os.Geteuid() + 2)
	}
	directory := trustedStat{mode: unix.S_IFDIR | 0o700, links: 1, owner: mismatchedOwner}
	regular := trustedStat{mode: unix.S_IFREG | 0o600, links: 1, owner: mismatchedOwner, size: 1}
	for name, check := range map[string]func() error{
		"ancestor": func() error { return validateAncestorStat(directory) },
		"parent":   func() error { return validatePrivateParentStat(directory) },
		"directory": func() error {
			return validatePrivateDirectoryStat(directory, "synthetic private directory")
		},
		"regular": func() error {
			return validatePrivateRegularStat(regular, "synthetic private file", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := check(); err == nil {
				t.Fatal("synthetic owner mismatch was accepted")
			}
		})
	}
	trusted := regular
	trusted.owner = uint32(os.Geteuid())
	if sameTrustedMetadataIdentity(trusted, regular) {
		t.Fatal("trusted metadata identity ignored a synthetic owner mismatch")
	}
}

func TestOpenRejectsHardlinkedAndSpecialLeaves(t *testing.T) {
	t.Run("hardlink", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ledger")
		opened := openTestSpool(t, root)
		response := testValidatedResponse(t, "ignored", "oldid:34",
			"2026-07-31T12:00:00.123456789Z", []byte("hardlink raw"), []byte("hardlink evidence"))
		committed, err := opened.commit(t.Context(), response)
		if err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		blob := filepath.Join(root, blobsDirectory, committed.rawResponseSHA256)
		alias := filepath.Join(t.TempDir(), "blob-alias")
		if err := os.Link(blob, alias); err != nil {
			t.Fatal(err)
		}
		if reopened, err := openSpool(t.Context(), root); err == nil {
			_ = reopened.Close()
			t.Fatal("Open accepted a hardlinked acquisition leaf")
		}
	})

	t.Run("special lock", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ledger")
		opened := openTestSpool(t, root)
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(root, ledgerLockName)
		if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(lock, 0o600); err != nil {
			t.Fatal(err)
		}
		if reopened, err := openSpool(t.Context(), root); err == nil {
			_ = reopened.Close()
			t.Fatal("Open accepted a special ledger-root lock leaf")
		}
		info, err := os.Lstat(lock)
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("special ledger-root lock was removed or changed: info=%v err=%v", info, err)
		}
	})

	t.Run("special", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ledger")
		opened := openTestSpool(t, root)
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(root, pendingDirectory, strings.Repeat("a", 64)+".raw.tmp")
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if reopened, err := openSpool(t.Context(), root); err == nil {
			_ = reopened.Close()
			t.Fatal("Open accepted a special pending leaf")
		}
		info, err := os.Lstat(fifo)
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("special pending leaf was removed or changed: info=%v err=%v", info, err)
		}
	})
}
