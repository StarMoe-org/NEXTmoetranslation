package lyricsacquisition

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	metadataCrashHelperEnvironment = "MOESEKAI_LYRICS_ACQUISITION_CRASH_HELPER"
	metadataCrashBoundaryEnv       = "MOESEKAI_LYRICS_ACQUISITION_CRASH_BOUNDARY"
	metadataCrashIterationEnv      = "MOESEKAI_LYRICS_ACQUISITION_CRASH_ITERATION"
	metadataSequentialCrashCycles  = 80
	metadataCrashHelperExitCode    = 86
	metadataCrashHelperParallelism = 1
)

var metadataCrashBoundaries = []metadataPersistenceBoundary{
	metadataBoundaryAfterRuntimeSerialization,
	metadataBoundaryBeforeRecoverySlotNamespaceSync,
	metadataBoundaryAfterRecoverySlotNamespaceSync,
	metadataBoundaryBeforeSnapshotWrite,
	metadataBoundaryAfterSnapshotWrite,
	metadataBoundaryBeforeSnapshotFileSync,
	metadataBoundaryAfterSnapshotFileSync,
	metadataBoundaryBeforeSelectorWrite,
	metadataBoundaryAfterSelectorWrite,
	metadataBoundaryBeforeSelectorFileSync,
	metadataBoundaryAfterSelectorFileSync,
	metadataBoundaryBeforeSelectorNamespaceSync,
	metadataBoundaryAfterSelectorNamespaceSync,
	metadataBoundaryBeforeConnectorRebind,
	metadataBoundaryAfterConnectorRebind,
	metadataBoundaryBeforeStandbyVerification,
	metadataBoundaryAfterStandbyVerification,
}

// Run the durability matrix with one non-race, single-P helper. Serial helper
// execution is the verified SQLite-safe mode; every boundary, cycle, and
// assertion remains in the dedicated crash-durability gate.
var metadataCrashHelperSlots = make(chan struct{}, metadataCrashHelperParallelism)

func requireCrashDurabilityGate(t *testing.T) {
	t.Helper()
	if raceDetectorEnabled {
		t.Skip("abrupt-exit durability is covered by the complete non-race gate; the race gate covers non-crashing concurrent commit, recovery, and replay paths")
	}
}

func buildMetadataCrashHelperExecutable(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "lyricsacquisition-crash-helper.test")
	// The race-enabled parent retains every orchestration and recovery assertion.
	// The child intentionally calls os.Exit at the injected durability boundary,
	// so a race runtime there cannot complete its report and only multiplies the
	// process startup cost and ThreadSanitizer address-space pressure.
	command := exec.Command("go", "test", "-c", "-race=false", "-o", executable, ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build non-race crash helper: %v\n%s", err, output)
	}
	return executable
}

func metadataCrashHelperCommandEnvironment(root string, boundary metadataPersistenceBoundary, iteration int) []string {
	environment := append(os.Environ(),
		"GOMAXPROCS=1",
		metadataCrashHelperEnvironment+"="+root,
		metadataCrashBoundaryEnv+"="+string(boundary),
	)
	if iteration >= 0 {
		environment = append(environment, fmt.Sprintf("%s=%d", metadataCrashIterationEnv, iteration))
	}
	return environment
}

// Namespace creation fsyncs are bootstrap boundaries. They are exercised on a
// fresh bounded ledger for every cycle because a correctly reused namespace
// cannot recreate them after bootstrap.
var metadataBootstrapCrashBoundaries = []metadataPersistenceBoundary{
	metadataBoundaryBeforeRecoverySlotNamespaceSync,
	metadataBoundaryAfterRecoverySlotNamespaceSync,
	metadataBoundaryBeforeSelectorNamespaceSync,
	metadataBoundaryAfterSelectorNamespaceSync,
}

// Every other before/after pair is reached on each steady-state reuse
// transition and therefore must survive far more cycles than the 32 historical
// recovery-slot name bound on one continuously reopened ledger.
var metadataSteadyStateCrashBoundaries = []metadataPersistenceBoundary{
	metadataBoundaryAfterRuntimeSerialization,
	metadataBoundaryBeforeSnapshotWrite,
	metadataBoundaryAfterSnapshotWrite,
	metadataBoundaryBeforeSnapshotFileSync,
	metadataBoundaryAfterSnapshotFileSync,
	metadataBoundaryBeforeSelectorWrite,
	metadataBoundaryAfterSelectorWrite,
	metadataBoundaryBeforeSelectorFileSync,
	metadataBoundaryAfterSelectorFileSync,
	metadataBoundaryBeforeConnectorRebind,
	metadataBoundaryAfterConnectorRebind,
	metadataBoundaryBeforeStandbyVerification,
	metadataBoundaryAfterStandbyVerification,
}

func resetMetadataTestHooks() {
	testHookAtomicPublicationPreflight = nil
	testHookBeforeSQLiteRuntimeOpen = nil
	testHookBeforeAtomicNoReplacePublish = nil
	testHookBeforeOwnedFileRetire = nil
}

func TestDescriptorPublicationPreflightRunsBeforeLedgerRootCreation(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	injected := errors.New("injected descriptor publication preflight failure")
	testHookAtomicPublicationPreflight = func(*os.File) error { return injected }
	ledger, err := CreateLedger(t.Context(), root)
	if ledger != nil || !errors.Is(err, injected) {
		t.Fatalf("preflight failure ledger=%v err=%v", ledger, err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight failure touched the ledger root: %v", err)
	}
}

func TestDescriptorPublicationProbeNameCannotAliasLedgerRootOrAncestor(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), atomicPublicationProbeDirectoryName)
		ledger, err := CreateLedger(t.Context(), root)
		if ledger != nil || err == nil {
			t.Fatalf("reserved descriptor publication probe root ledger=%v err=%v", ledger, err)
		}
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reserved descriptor publication probe root was touched: %v", err)
		}
	})

	t.Run("ancestor", func(t *testing.T) {
		probe := filepath.Join(t.TempDir(), atomicPublicationProbeDirectoryName)
		if err := os.Mkdir(probe, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(probe, "ledger")
		ledger, err := CreateLedger(t.Context(), root)
		if ledger != nil || err == nil {
			t.Fatalf("reserved descriptor publication probe ancestor ledger=%v err=%v", ledger, err)
		}
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ledger beneath reserved descriptor publication probe was touched: %v", err)
		}
		entries, err := os.ReadDir(probe)
		if err != nil || len(entries) != 0 {
			t.Fatalf("reserved descriptor publication probe ancestor was touched: entries=%v err=%v", entries, err)
		}
	})
}

func TestRuntimeDescriptorPublicationFeaturePreflight(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := requireAtomicNamespacePublication(); err != nil {
		t.Fatal(err)
	}
	if err := preflightAtomicNamespacePublication(directory); err != nil {
		t.Fatalf("real descriptor publication feature preflight failed: %v", err)
	}
	if err := preflightAtomicNamespacePublication(directory); err != nil {
		t.Fatalf("retained descriptor publication capability verification failed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(directory.Name(), atomicPublicationProbeDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || len(entries) > 2 {
		t.Fatalf("descriptor publication capability probe is not bounded: entries=%d", len(entries))
	}
}

func TestDescriptorPublicationPreflightConcurrentCreationConvergesToOneBoundedVerifiedProbe(t *testing.T) {
	root := t.TempDir()
	const callers = 16
	results := make(chan error, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			directory, err := os.Open(root)
			if err == nil {
				err = preflightAtomicNamespacePublication(directory)
				err = errors.Join(err, directory.Close())
			}
			results <- err
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("concurrent descriptor publication preflight produced no verified capability probe")
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightAtomicNamespacePublication(directory); err != nil {
		_ = directory.Close()
		t.Fatalf("concurrent descriptor publication preflight did not converge: %v", err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, atomicPublicationProbeDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || len(entries) > 2 {
		t.Fatalf("concurrent descriptor publication probe is not bounded: entries=%d", len(entries))
	}
}

func TestDescriptorPublicationPreflightRejectsUnverifiedProbeWithoutTouchingIt(t *testing.T) {
	root := t.TempDir()
	probePath := filepath.Join(root, atomicPublicationProbeDirectoryName)
	if err := os.Mkdir(probePath, 0o700); err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(probePath, "unverified-replacement")
	body := []byte("unverified descriptor publication probe\n")
	if err := os.WriteFile(unknownPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(unknownPath)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	preflightErr := preflightAtomicNamespacePublication(directory)
	closeErr := directory.Close()
	if preflightErr == nil {
		t.Fatal("descriptor publication preflight trusted an unverified retained namespace")
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	afterBody, readErr := os.ReadFile(unknownPath)
	after, statErr := os.Lstat(unknownPath)
	if readErr != nil || statErr != nil || !bytes.Equal(afterBody, body) || !os.SameFile(before, after) {
		t.Fatalf("descriptor publication preflight touched an unverified replacement: body=%q read=%v stat=%v", afterBody, readErr, statErr)
	}
	entries, err := os.ReadDir(probePath)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(unknownPath) {
		t.Fatalf("descriptor publication preflight changed an unverified namespace: entries=%v err=%v", entries, err)
	}
}

func TestMetadataSnapshotPositivePartialWriteAtCompletesExactly(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	calls := 0
	opened.hooks.metadataSnapshotWriteAt = func(file *os.File, body []byte, offset int64) (int, error) {
		calls++
		maximum := 17
		if len(body) < maximum {
			maximum = len(body)
		}
		return file.WriteAt(body[:maximum], offset)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("positive partial write raw"), []byte("positive partial write evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatalf("positive partial WriteAt commit failed: %v", err)
	}
	if calls < 2 {
		t.Fatalf("positive partial WriteAt was not retried: calls=%d", calls)
	}
	state := readActiveDiskMetadataState(t, root, committed.acquisitionID)
	if state.requests != 1 || state.acquisitions != 1 || state.counters.requestCount != 1 || state.counters.acquisitionCount != 1 {
		t.Fatalf("positive partial WriteAt did not publish exact metadata: %+v", state)
	}
}

func TestMetadataSnapshotShortErrorAndInvalidWriteCountsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		hook func(error) func(*os.File, []byte, int64) (int, error)
	}{
		{
			name: "positive short with error",
			hook: func(injected error) func(*os.File, []byte, int64) (int, error) {
				called := false
				return func(file *os.File, body []byte, offset int64) (int, error) {
					if called {
						return 0, injected
					}
					called = true
					maximum := 19
					if len(body) < maximum {
						maximum = len(body)
					}
					written, err := file.WriteAt(body[:maximum], offset)
					return written, errors.Join(err, injected)
				}
			},
		},
		{
			name: "negative count",
			hook: func(error) func(*os.File, []byte, int64) (int, error) {
				return func(*os.File, []byte, int64) (int, error) { return -1, nil }
			},
		},
		{
			name: "oversized count",
			hook: func(error) func(*os.File, []byte, int64) (int, error) {
				return func(_ *os.File, body []byte, _ int64) (int, error) { return len(body) + 1, nil }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer resetMetadataTestHooks()
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(root, metadataFileName)
			before, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected metadata WriteAt failure")
			opened.hooks.metadataSnapshotWriteAt = test.hook(injected)
			response := testValidatedResponse(t, "ignored", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte("write fault raw "+test.name), []byte("write fault evidence "+test.name))
			_, _, acquisitionID, _, buildErr := buildManifest(response)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			if committed, commitErr := opened.commit(t.Context(), response); commitErr == nil || committed.acquisitionID != "" {
				t.Fatalf("invalid WriteAt returned success: committed=%+v err=%v", committed, commitErr)
			}
			after, err := os.ReadFile(metadataPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("WriteAt fault changed active metadata: err=%v", err)
			}
			stage, err := os.Lstat(filepath.Join(root, metadataSnapshotTempName))
			if err != nil || !stage.Mode().IsRegular() {
				t.Fatalf("WriteAt fault did not retain one bounded stage: info=%v err=%v", stage, err)
			}
			resetMetadataTestHooks()
			_ = opened.Close()
			reopened, err := openExistingSpool(t.Context(), root)
			if err != nil {
				t.Fatalf("reopen after WriteAt fault: %v", err)
			}
			if _, err := reopened.replayByAcquisitionID(t.Context(), acquisitionID); err != nil {
				_ = reopened.Close()
				t.Fatalf("manifest reconciliation after WriteAt fault: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataPersistenceBoundaryMapIsExactUniqueAndReadOnly(t *testing.T) {
	expected := []struct {
		boundary  metadataPersistenceBoundary
		operation metadataPersistenceOperation
		phase     metadataBoundaryPhase
	}{
		{metadataBoundaryAfterRuntimeSerialization, metadataOperationRuntimeSerialization, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeRecoverySlotNamespaceSync, metadataOperationRecoverySlotNamespaceSync, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterRecoverySlotNamespaceSync, metadataOperationRecoverySlotNamespaceSync, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeSnapshotWrite, metadataOperationSnapshotWrite, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterSnapshotWrite, metadataOperationSnapshotWrite, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeSnapshotFileSync, metadataOperationSnapshotFileSync, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterSnapshotFileSync, metadataOperationSnapshotFileSync, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeSelectorWrite, metadataOperationSelectorWrite, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterSelectorWrite, metadataOperationSelectorWrite, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeSelectorFileSync, metadataOperationSelectorFileSync, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterSelectorFileSync, metadataOperationSelectorFileSync, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeSelectorNamespaceSync, metadataOperationSelectorNamespaceSync, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterSelectorNamespaceSync, metadataOperationSelectorNamespaceSync, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeConnectorRebind, metadataOperationConnectorRebind, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterConnectorRebind, metadataOperationConnectorRebind, metadataBoundaryPhaseAfter},
		{metadataBoundaryBeforeStandbyVerification, metadataOperationStandbyVerification, metadataBoundaryPhaseBefore},
		{metadataBoundaryAfterStandbyVerification, metadataOperationStandbyVerification, metadataBoundaryPhaseAfter},
	}
	seenBoundaries := make(map[metadataPersistenceBoundary]bool, len(expected))
	seenOperationPhases := make(map[string]bool, len(expected))
	for _, want := range expected {
		if seenBoundaries[want.boundary] {
			t.Fatalf("duplicate metadata boundary %q", want.boundary)
		}
		seenBoundaries[want.boundary] = true
		contract, valid := metadataBoundaryContractFor(want.boundary)
		if !valid || contract.operation != want.operation || contract.phase != want.phase {
			t.Fatalf("boundary %q contract=%+v valid=%v, want %s/%s", want.boundary, contract, valid, want.operation, want.phase)
		}
		key := string(contract.operation) + "\x00" + string(contract.phase)
		if seenOperationPhases[key] {
			t.Fatalf("duplicate or misleading operation/phase mapping %s/%s", contract.operation, contract.phase)
		}
		seenOperationPhases[key] = true
	}
	for _, misleading := range []metadataPersistenceBoundary{
		"after_serialization",
		"after_file_fsync",
		"after_recovery_slot_namespace_sync",
		"before_metadata_snapshot_publish",
		"metadata_post_rebind_standby",
	} {
		if contract, valid := metadataBoundaryContractFor(misleading); valid {
			t.Fatalf("misleading boundary %q was accepted as %+v", misleading, contract)
		}
	}
	called := false
	probe := &spool{hooks: spoolHooks{metadataBoundary: func(metadataPersistenceBoundary) error {
		called = true
		return nil
	}}}
	if err := probe.reachMetadataBoundary("after_file_fsync"); err == nil || called {
		t.Fatalf("unknown boundary reached hook: called=%v err=%v", called, err)
	}
}

func TestMetadataPersistenceBoundariesFollowExactOperationOrderOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	var reached []metadataPersistenceBoundary
	opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		reached = append(reached, boundary)
		return nil
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("boundary order raw"), []byte("boundary order evidence"))
	if _, err := opened.commit(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	opened.hooks.metadataBoundary = nil
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if len(reached) != len(metadataCrashBoundaries) {
		t.Fatalf("metadata boundary count=%d want=%d: %v", len(reached), len(metadataCrashBoundaries), reached)
	}
	for index, want := range metadataCrashBoundaries {
		if reached[index] != want {
			t.Fatalf("metadata boundary %d=%q want %q; all=%v", index, reached[index], want, reached)
		}
	}
}

func TestMetadataPersistenceFaultBoundariesNeverReturnSuccess(t *testing.T) {
	tests := []struct {
		boundary       metadataPersistenceBoundary
		wantActiveRows int64
	}{
		{boundary: metadataBoundaryAfterRuntimeSerialization},
		{boundary: metadataBoundaryBeforeRecoverySlotNamespaceSync},
		{boundary: metadataBoundaryAfterRecoverySlotNamespaceSync},
		{boundary: metadataBoundaryBeforeSnapshotWrite},
		{boundary: metadataBoundaryAfterSnapshotWrite},
		{boundary: metadataBoundaryBeforeSnapshotFileSync},
		{boundary: metadataBoundaryAfterSnapshotFileSync},
		{boundary: metadataBoundaryBeforeSelectorWrite},
		{boundary: metadataBoundaryAfterSelectorWrite, wantActiveRows: 1},
		{boundary: metadataBoundaryBeforeSelectorFileSync, wantActiveRows: 1},
		{boundary: metadataBoundaryAfterSelectorFileSync, wantActiveRows: 1},
		{boundary: metadataBoundaryBeforeSelectorNamespaceSync, wantActiveRows: 1},
		{boundary: metadataBoundaryAfterSelectorNamespaceSync, wantActiveRows: 1},
		{boundary: metadataBoundaryBeforeConnectorRebind, wantActiveRows: 1},
		{boundary: metadataBoundaryAfterConnectorRebind, wantActiveRows: 1},
		{boundary: metadataBoundaryBeforeStandbyVerification, wantActiveRows: 1},
		{boundary: metadataBoundaryAfterStandbyVerification, wantActiveRows: 1},
	}
	for _, test := range tests {
		t.Run(string(test.boundary), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected " + string(test.boundary))
			var reached []metadataPersistenceBoundary
			opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
				reached = append(reached, boundary)
				if boundary == test.boundary {
					return injected
				}
				return nil
			}
			response := testValidatedResponse(t, "ignored", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte("fault boundary raw "+string(test.boundary)), []byte("fault boundary evidence "+string(test.boundary)))
			_, _, acquisitionID, _, err := buildManifest(response)
			if err != nil {
				t.Fatal(err)
			}
			committed, commitErr := opened.commit(t.Context(), response)
			if !errors.Is(commitErr, injected) || committed.acquisitionID != "" {
				t.Fatalf("fault boundary returned result: committed=%+v err=%v", committed, commitErr)
			}
			if len(reached) == 0 || reached[len(reached)-1] != test.boundary {
				t.Fatalf("fault at %s continued to later boundaries: reached=%v", test.boundary, reached)
			}
			state := readActiveDiskMetadataState(t, root, "")
			if state.requests != test.wantActiveRows || state.acquisitions != test.wantActiveRows ||
				state.counters.requestCount != test.wantActiveRows || state.counters.acquisitionCount != test.wantActiveRows {
				t.Fatalf("fault boundary active snapshot=%+v want rows=%d", state, test.wantActiveRows)
			}
			opened.hooks.metadataBoundary = nil
			_ = opened.Close()
			reopened, err := openExistingSpool(t.Context(), root)
			if err != nil {
				t.Fatalf("reopen after %s: %v", test.boundary, err)
			}
			if _, err := reopened.replayByAcquisitionID(t.Context(), acquisitionID); err != nil {
				_ = reopened.Close()
				t.Fatalf("manifest reconciliation after %s: %v", test.boundary, err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataCrashBoundariesLeaveCompleteOldOrNewSnapshot(t *testing.T) {
	requireCrashDurabilityGate(t)
	helperExecutable := buildMetadataCrashHelperExecutable(t)
	for _, boundary := range metadataCrashBoundaries {
		t.Run(string(boundary), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			first := testValidatedResponse(t, "ignored", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte("crash old raw"), []byte("crash old evidence"))
			oldSnapshot, firstID := appendExpectedMetadataSnapshot(t, expectedMetadataSnapshot{}, first)
			committed, err := opened.commit(t.Context(), first)
			if err != nil || committed.acquisitionID != firstID {
				t.Fatalf("commit old snapshot acquisition=%q want=%q err=%v", committed.acquisitionID, firstID, err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			second := testValidatedResponse(t, "ignored", "oldid:35",
				"2026-07-31T12:00:01.123456789Z", []byte("crash new raw"), []byte("crash new evidence"))
			newSnapshot, secondID := appendExpectedMetadataSnapshot(t, oldSnapshot, second)
			command := exec.Command(helperExecutable, "-test.run=^TestMetadataCrashBoundaryHelper$", "-test.v")
			command.Env = metadataCrashHelperCommandEnvironment(root, boundary, -1)
			output, commandErr := command.CombinedOutput()
			exit, ok := commandErr.(*exec.ExitError)
			if commandErr == nil || !ok || exit.ExitCode() != metadataCrashHelperExitCode {
				t.Fatalf("crash helper exit=%v at %s\n%s", commandErr, boundary, output)
			}
			crashSnapshot := readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root))
			if !diskMetadataSnapshotMatches(crashSnapshot, oldSnapshot) && !diskMetadataSnapshotMatches(crashSnapshot, newSnapshot) {
				t.Fatalf("abrupt exit at %s left neither the complete old nor complete new snapshot: rows=%d/%d counters=%+v IDs=%v\n%s",
					boundary, crashSnapshot.state.requests, crashSnapshot.state.acquisitions, crashSnapshot.state.counters,
					metadataAcquisitionIDs(crashSnapshot.acquisitions), output)
			}
			recovered, err := openExistingSpool(t.Context(), root)
			if err != nil {
				t.Fatalf("reopen after abrupt exit at %s: %v\n%s", boundary, err, output)
			}
			assertExactDiskMetadataSnapshot(t, "recovered active", readDiskMetadataSnapshot(t, activeMetadataPathForSpool(t, recovered)), newSnapshot)
			if recovered.metadataStandbyFile == nil {
				_ = recovered.Close()
				t.Fatalf("recovery after abrupt exit at %s retained no complete old standby", boundary)
			}
			assertExactDiskMetadataSnapshot(t, "recovered standby",
				readDiskMetadataSnapshot(t, filepath.Join(root, recovered.metadataStandbyPath)), oldSnapshot)
			for _, acquisitionID := range []string{firstID, secondID} {
				if _, err := recovered.replayByAcquisitionID(t.Context(), acquisitionID); err != nil {
					_ = recovered.Close()
					t.Fatalf("exact-ID replay %s after abrupt exit at %s: %v", acquisitionID, boundary, err)
				}
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
			assertExactDiskMetadataSnapshot(t, "durable recovered active",
				readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root)), newSnapshot)
			reopened, err := openExistingSpool(t.Context(), root)
			if err != nil {
				t.Fatalf("second reopen after abrupt exit at %s: %v", boundary, err)
			}
			if _, err := reopened.replayByAcquisitionID(t.Context(), secondID); err != nil {
				_ = reopened.Close()
				t.Fatalf("second exact-ID replay after abrupt exit at %s: %v", boundary, err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			slots, selectors := countMetadataStateMachineEntries(t, root)
			if slots != 3 || selectors != 2 {
				t.Fatalf("crash recovery state machine slots=%d selectors=%d, want 3/2", slots, selectors)
			}
		})
	}
}

func TestMetadataCrashBoundaryHelper(t *testing.T) {
	root := os.Getenv(metadataCrashHelperEnvironment)
	if root == "" {
		t.Skip("metadata crash helper")
	}
	boundary := metadataPersistenceBoundary(os.Getenv(metadataCrashBoundaryEnv))
	response := testValidatedResponse(t, "ignored", "oldid:35",
		"2026-07-31T12:00:01.123456789Z", []byte("crash new raw"), []byte("crash new evidence"))
	if rawIteration := os.Getenv(metadataCrashIterationEnv); rawIteration != "" {
		iteration, err := strconv.Atoi(rawIteration)
		if err != nil || iteration < 0 {
			t.Fatalf("invalid sequential crash iteration: %q", rawIteration)
		}
		response = reusableSlotCrashResponse(t, iteration)
	}
	opened, err := openSpoolWithHooks(t.Context(), root, spoolHooks{
		metadataBoundary: func(reached metadataPersistenceBoundary) error {
			if reached == boundary {
				os.Exit(metadataCrashHelperExitCode)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.commit(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("crash boundary %s was not reached", boundary)
}

func TestBootstrapMetadataBoundariesSurviveEightySequentialCrashReopenLedgers(t *testing.T) {
	requireCrashDurabilityGate(t)
	if metadataSequentialCrashCycles <= maxMetadataRecoverySlots*2 {
		t.Fatalf("sequential crash cycle count=%d is not far beyond the historical slot bound %d",
			metadataSequentialCrashCycles, maxMetadataRecoverySlots)
	}
	helperExecutable := buildMetadataCrashHelperExecutable(t)
	for _, boundary := range metadataBootstrapCrashBoundaries {
		t.Run(string(boundary), func(t *testing.T) {
			t.Parallel()
			metadataCrashHelperSlots <- struct{}{}
			defer func() { <-metadataCrashHelperSlots }()
			parent := t.TempDir()
			for iteration := 0; iteration < metadataSequentialCrashCycles; iteration++ {
				root := filepath.Join(parent, fmt.Sprintf("ledger-%03d", iteration))
				opened, err := openSpool(t.Context(), root)
				if err != nil {
					t.Fatalf("boundary %s iteration %d create ledger: %v", boundary, iteration, err)
				}
				initial := testValidatedResponse(t, "ignored", "oldid:999",
					"2026-07-31T12:00:00.123456789Z", []byte("bootstrap crash initial raw"), []byte("bootstrap crash initial evidence"))
				oldSnapshot, initialID := appendExpectedMetadataSnapshot(t, expectedMetadataSnapshot{}, initial)
				committed, err := opened.commit(t.Context(), initial)
				if err != nil || committed.acquisitionID != initialID {
					_ = opened.Close()
					t.Fatalf("boundary %s iteration %d initial acquisition=%q want=%q err=%v",
						boundary, iteration, committed.acquisitionID, initialID, err)
				}
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
				response := reusableSlotCrashResponse(t, iteration)
				newSnapshot, acquisitionID := appendExpectedMetadataSnapshot(t, oldSnapshot, response)
				command := exec.Command(helperExecutable, "-test.run=^TestMetadataCrashBoundaryHelper$", "-test.v")
				command.Env = metadataCrashHelperCommandEnvironment(root, boundary, iteration)
				output, commandErr := command.CombinedOutput()
				exit, ok := commandErr.(*exec.ExitError)
				if commandErr == nil || !ok || exit.ExitCode() != metadataCrashHelperExitCode {
					t.Fatalf("boundary %s iteration %d crash helper exit=%v\n%s", boundary, iteration, commandErr, output)
				}
				crashSnapshot := readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root))
				if !diskMetadataSnapshotMatches(crashSnapshot, oldSnapshot) && !diskMetadataSnapshotMatches(crashSnapshot, newSnapshot) {
					t.Fatalf("boundary %s iteration %d selected neither complete old nor complete new metadata: rows=%d/%d counters=%+v IDs=%v",
						boundary, iteration, crashSnapshot.state.requests, crashSnapshot.state.acquisitions,
						crashSnapshot.state.counters, metadataAcquisitionIDs(crashSnapshot.acquisitions))
				}
				recovered, err := openExistingSpool(t.Context(), root)
				if err != nil {
					t.Fatalf("boundary %s iteration %d bootstrap recovery failed: %v\n%s", boundary, iteration, err, output)
				}
				assertExactDiskMetadataSnapshot(t, "bootstrap recovered active",
					readDiskMetadataSnapshot(t, activeMetadataPathForSpool(t, recovered)), newSnapshot)
				if recovered.metadataStandbyFile == nil {
					_ = recovered.Close()
					t.Fatalf("boundary %s iteration %d retained no complete old standby", boundary, iteration)
				}
				assertExactDiskMetadataSnapshot(t, "bootstrap recovered standby",
					readDiskMetadataSnapshot(t, filepath.Join(root, recovered.metadataStandbyPath)), oldSnapshot)
				for _, exactID := range []string{initialID, acquisitionID} {
					if _, err := recovered.replayByAcquisitionID(t.Context(), exactID); err != nil {
						_ = recovered.Close()
						t.Fatalf("boundary %s iteration %d exact replay %s failed: %v", boundary, iteration, exactID, err)
					}
				}
				if err := recovered.Close(); err != nil {
					t.Fatal(err)
				}
				assertExactDiskMetadataSnapshot(t, "bootstrap durable active",
					readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root)), newSnapshot)
				slots, selectors := countMetadataStateMachineEntries(t, root)
				if slots != 3 || selectors != 2 {
					t.Fatalf("boundary %s iteration %d metadata namespace slots=%d selectors=%d, want 3/2",
						boundary, iteration, slots, selectors)
				}
			}
		})
	}
}

func TestReusableMetadataSlotsSurviveEightySequentialCrashesAtEverySteadyStateBoundary(t *testing.T) {
	requireCrashDurabilityGate(t)
	if metadataSequentialCrashCycles <= maxMetadataRecoverySlots*2 {
		t.Fatalf("sequential crash cycle count=%d is not far beyond the historical slot bound %d",
			metadataSequentialCrashCycles, maxMetadataRecoverySlots)
	}
	helperExecutable := buildMetadataCrashHelperExecutable(t)
	for _, boundary := range metadataSteadyStateCrashBoundaries {
		t.Run(string(boundary), func(t *testing.T) {
			t.Parallel()
			metadataCrashHelperSlots <- struct{}{}
			defer func() { <-metadataCrashHelperSlots }()
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			initial := testValidatedResponse(t, "ignored", "oldid:999",
				"2026-07-31T12:00:00.123456789Z", []byte("reusable crash initial raw"), []byte("reusable crash initial evidence"))
			expected, initialID := appendExpectedMetadataSnapshot(t, expectedMetadataSnapshot{}, initial)
			committed, err := opened.commit(t.Context(), initial)
			if err != nil || committed.acquisitionID != initialID {
				t.Fatalf("commit initial reusable snapshot acquisition=%q want=%q err=%v", committed.acquisitionID, initialID, err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}

			for iteration := 0; iteration < metadataSequentialCrashCycles; iteration++ {
				response := reusableSlotCrashResponse(t, iteration)
				nextExpected, acquisitionID := appendExpectedMetadataSnapshot(t, expected, response)
				command := exec.Command(helperExecutable, "-test.run=^TestMetadataCrashBoundaryHelper$", "-test.v")
				command.Env = metadataCrashHelperCommandEnvironment(root, boundary, iteration)
				output, commandErr := command.CombinedOutput()
				exit, ok := commandErr.(*exec.ExitError)
				if commandErr == nil || !ok || exit.ExitCode() != metadataCrashHelperExitCode {
					t.Fatalf("boundary %s iteration %d crash helper exit=%v\n%s", boundary, iteration, commandErr, output)
				}
				crashSnapshot := readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root))
				if !diskMetadataSnapshotMatches(crashSnapshot, expected) && !diskMetadataSnapshotMatches(crashSnapshot, nextExpected) {
					t.Fatalf("boundary %s iteration %d selected neither complete old nor complete new metadata: rows=%d/%d counters=%+v IDs=%v",
						boundary, iteration, crashSnapshot.state.requests, crashSnapshot.state.acquisitions,
						crashSnapshot.state.counters, metadataAcquisitionIDs(crashSnapshot.acquisitions))
				}
				recovered, err := openExistingSpool(t.Context(), root)
				if err != nil {
					t.Fatalf("boundary %s iteration %d reusable-slot recovery failed: %v\n%s", boundary, iteration, err, output)
				}
				assertExactDiskMetadataSnapshot(t, "sequential recovered active",
					readDiskMetadataSnapshot(t, activeMetadataPathForSpool(t, recovered)), nextExpected)
				if recovered.metadataStandbyFile == nil {
					_ = recovered.Close()
					t.Fatalf("boundary %s iteration %d retained no complete old standby", boundary, iteration)
				}
				assertExactDiskMetadataSnapshot(t, "sequential recovered standby",
					readDiskMetadataSnapshot(t, filepath.Join(root, recovered.metadataStandbyPath)), expected)
				for _, exactID := range []string{initialID, acquisitionID} {
					if _, err := recovered.replayByAcquisitionID(t.Context(), exactID); err != nil {
						_ = recovered.Close()
						t.Fatalf("boundary %s iteration %d exact replay %s failed: %v", boundary, iteration, exactID, err)
					}
				}
				if err := recovered.Close(); err != nil {
					t.Fatal(err)
				}
				assertExactDiskMetadataSnapshot(t, "sequential durable active",
					readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root)), nextExpected)
				slots, selectors := countMetadataStateMachineEntries(t, root)
				wantSelectors := iteration + 2
				if wantSelectors > maxMetadataSelectorSlots {
					wantSelectors = maxMetadataSelectorSlots
				}
				if slots != 3 || selectors != wantSelectors {
					t.Fatalf("boundary %s iteration %d metadata namespace slots=%d selectors=%d, want 3/%d",
						boundary, iteration, slots, selectors, wantSelectors)
				}
				expected = nextExpected
			}
			final := readDiskMetadataSnapshot(t, activeDiskMetadataPath(t, root))
			assertExactDiskMetadataSnapshot(t, "sequential final active", final, expected)
			if final.state.acquisitions != metadataSequentialCrashCycles+1 || final.state.requests != metadataSequentialCrashCycles+1 {
				t.Fatalf("boundary %s final state rows=%d/%d, want %d",
					boundary, final.state.requests, final.state.acquisitions, metadataSequentialCrashCycles+1)
			}
			slots, selectors := countMetadataStateMachineEntries(t, root)
			if slots != 3 || selectors != maxMetadataSelectorSlots {
				t.Fatalf("boundary %s final namespace slots=%d selectors=%d, want 3/%d",
					boundary, slots, selectors, maxMetadataSelectorSlots)
			}
		})
	}
}

func TestFullHistoricalMetadataSlotNamespaceReusesVerifiedInactiveDescriptor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	type retainedSlot struct {
		body []byte
		info os.FileInfo
	}
	retained := make(map[string]retainedSlot, maxMetadataRecoverySlots+1)
	slotNames := []string{metadataSnapshotTempName}
	for index := 0; index < maxMetadataRecoverySlots; index++ {
		slotNames = append(slotNames, metadataRecoverySlotName(index))
	}
	for index, name := range slotNames {
		body := []byte(fmt.Sprintf("retained incomplete historical metadata slot %02d", index))
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		retained[name] = retainedSlot{body: body, info: info}
	}
	metadataPath := filepath.Join(root, metadataFileName)
	metadataBody, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	metadataInfo, err := os.Lstat(metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := openExistingSpool(t.Context(), root)
	if err != nil {
		t.Fatalf("open full historical metadata slot namespace: %v", err)
	}
	if reopened.metadataStandbyFile != nil {
		_ = reopened.Close()
		t.Fatal("incomplete historical slots unexpectedly became the rollback snapshot")
	}
	reusedName := reopened.metadataWritePath
	reusedBefore, found := retained[reusedName]
	if !found || reopened.metadataWriteFile == nil {
		_ = reopened.Close()
		t.Fatalf("open did not pin a verified inactive historical slot: %q", reusedName)
	}
	response := testValidatedResponse(t, "ignored", "oldid:2000",
		"2026-07-31T12:03:20.123456789Z", []byte("full slot reuse raw"), []byte("full slot reuse evidence"))
	committed, err := reopened.commit(t.Context(), response)
	if err != nil {
		_ = reopened.Close()
		t.Fatalf("reuse full historical metadata slot namespace: %v", err)
	}
	if committed.acquisitionID == "" {
		_ = reopened.Close()
		t.Fatal("verified inactive slot reuse returned an empty acquisition ID")
	}

	reusedPath := filepath.Join(root, reusedName)
	reusedBody, err := os.ReadFile(reusedPath)
	reusedAfter, statErr := os.Lstat(reusedPath)
	if err != nil || statErr != nil || !os.SameFile(reusedBefore.info, reusedAfter) || bytes.Equal(reusedBefore.body, reusedBody) {
		_ = reopened.Close()
		t.Fatalf("inactive slot reuse did not stay on its verified descriptor: read=%v stat=%v", err, statErr)
	}
	for name, before := range retained {
		if name == reusedName {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(root, name))
		after, statErr := os.Lstat(filepath.Join(root, name))
		if readErr != nil || statErr != nil || !bytes.Equal(body, before.body) || !os.SameFile(before.info, after) {
			_ = reopened.Close()
			t.Fatalf("slot reuse touched inactive slot %s: read=%v stat=%v", name, readErr, statErr)
		}
	}
	rollbackBody, err := os.ReadFile(metadataPath)
	rollbackInfo, statErr := os.Lstat(metadataPath)
	if err != nil || statErr != nil || !bytes.Equal(rollbackBody, metadataBody) || !os.SameFile(metadataInfo, rollbackInfo) {
		_ = reopened.Close()
		t.Fatalf("slot reuse changed the complete rollback metadata.db snapshot: read=%v stat=%v", err, statErr)
	}
	rollback := readDiskMetadataState(t, metadataPath, "")
	active := readActiveDiskMetadataState(t, root, committed.acquisitionID)
	if rollback.acquisitions != 0 || rollback.requests != 0 || rollback.counters != (metadataCounters{}) ||
		active.acquisitions != 1 || active.requests != 1 || active.counters.acquisitionCount != 1 || active.counters.requestCount != 1 {
		_ = reopened.Close()
		t.Fatalf("slot reuse did not preserve complete active and rollback snapshots: active=%+v rollback=%+v", active, rollback)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := openExistingSpool(t.Context(), root)
	if err != nil {
		t.Fatalf("reopen after full-slot reuse: %v", err)
	}
	if _, err := again.replayByAcquisitionID(t.Context(), committed.acquisitionID); err != nil {
		_ = again.Close()
		t.Fatalf("exact replay after full-slot reuse: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	slots, selectors := countMetadataStateMachineEntries(t, root)
	if slots != maxMetadataRecoverySlots+2 || selectors != 1 {
		t.Fatalf("full historical namespace changed size: slots=%d selectors=%d", slots, selectors)
	}
}

func TestMetadataSlotAppearingAfterValidationIsNeverTouched(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	path := filepath.Join(root, metadataSnapshotTempName)
	body := []byte("unverified metadata slot that appeared after validation")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:2001",
		"2026-07-31T12:03:21.123456789Z", []byte("unverified slot raw"), []byte("unverified slot evidence"))
	if committed, err := opened.commit(t.Context(), response); err == nil || committed.acquisitionID != "" || !strings.Contains(err.Error(), "appeared after validation") {
		t.Fatalf("unverified slot commit result=%+v err=%v", committed, err)
	}
	afterBody, readErr := os.ReadFile(path)
	after, statErr := os.Lstat(path)
	if readErr != nil || statErr != nil || !bytes.Equal(afterBody, body) || !os.SameFile(before, after) {
		t.Fatalf("unverified metadata slot was touched: body=%q read=%v stat=%v", afterBody, readErr, statErr)
	}
}

func TestPostRebindStandbyVerificationChecksTheRealPathTransition(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	standbyPath := filepath.Join(root, metadataFileName)
	standbyBody, err := os.ReadFile(standbyPath)
	if err != nil {
		t.Fatal(err)
	}
	standbyInfo, err := os.Lstat(standbyPath)
	if err != nil {
		t.Fatal(err)
	}
	savedStandby := filepath.Join(t.TempDir(), "verified-standby.db")
	replacementBody := []byte("post-rebind standby replacement must remain untouched")
	var replacementInfo os.FileInfo
	afterBoundaryReached := false
	opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		switch boundary {
		case metadataBoundaryBeforeStandbyVerification:
			if opened.metadataStandbyFile == nil {
				return errors.New("post-rebind standby descriptor is missing")
			}
			if err := os.Rename(standbyPath, savedStandby); err != nil {
				return err
			}
			if err := os.WriteFile(standbyPath, replacementBody, 0o600); err != nil {
				return err
			}
			var err error
			replacementInfo, err = os.Lstat(standbyPath)
			return err
		case metadataBoundaryAfterStandbyVerification:
			afterBoundaryReached = true
		}
		return nil
	}
	response := testValidatedResponse(t, "ignored", "oldid:2002",
		"2026-07-31T12:03:22.123456789Z", []byte("standby transition raw"), []byte("standby transition evidence"))
	if committed, err := opened.commit(t.Context(), response); err == nil || committed.acquisitionID != "" {
		t.Fatalf("standby transition replacement returned success: committed=%+v err=%v", committed, err)
	}
	if afterBoundaryReached {
		t.Fatal("post-rebind standby verification reported completion after detecting a replacement")
	}
	replacement, readErr := os.ReadFile(standbyPath)
	replacementAfter, statErr := os.Lstat(standbyPath)
	if readErr != nil || statErr != nil || replacementInfo == nil || !bytes.Equal(replacement, replacementBody) || !os.SameFile(replacementInfo, replacementAfter) {
		t.Fatalf("post-rebind standby verification touched the replacement: read=%v stat=%v", readErr, statErr)
	}
	savedBody, savedReadErr := os.ReadFile(savedStandby)
	savedInfo, savedStatErr := os.Lstat(savedStandby)
	if savedReadErr != nil || savedStatErr != nil || !bytes.Equal(savedBody, standbyBody) || !os.SameFile(standbyInfo, savedInfo) {
		t.Fatalf("post-rebind standby verification changed the verified rollback snapshot: read=%v stat=%v", savedReadErr, savedStatErr)
	}
	resetMetadataTestHooks()
	_ = opened.Close()
}

func reusableSlotCrashResponse(t *testing.T, iteration int) validatedProviderResponse {
	t.Helper()
	observed := time.Date(2026, time.July, 31, 12, 1+iteration/60, iteration%60, 123456789, time.UTC)
	return testValidatedResponse(t, "ignored", fmt.Sprintf("oldid:%d", 1000+iteration),
		observed.Format(time.RFC3339Nano), []byte(fmt.Sprintf("reusable crash raw %d", iteration)),
		[]byte(fmt.Sprintf("reusable crash evidence %d", iteration)))
}

func TestPostPublicationIdentityMismatchPreservesReplacementTarget(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	var selectedPath string
	publishedElsewhere := filepath.Join(t.TempDir(), "published-new.db")
	replacementBody := []byte("post-publication replacement must survive")
	var replacementInfo os.FileInfo
	opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		if boundary != metadataBoundaryAfterSelectorFileSync {
			return nil
		}
		selectedPath = activeDiskMetadataPath(t, root)
		if err := os.Rename(selectedPath, publishedElsewhere); err != nil {
			return err
		}
		if err := os.WriteFile(selectedPath, replacementBody, 0o600); err != nil {
			return err
		}
		var err error
		replacementInfo, err = os.Lstat(selectedPath)
		return err
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("post publication raw"), []byte("post publication evidence"))
	if committed, err := opened.commit(t.Context(), response); err == nil || committed.acquisitionID != "" {
		t.Fatalf("post-publication mismatch returned success: committed=%+v err=%v", committed, err)
	}
	body, readErr := os.ReadFile(selectedPath)
	after, statErr := os.Lstat(selectedPath)
	if readErr != nil || statErr != nil || replacementInfo == nil || !bytes.Equal(body, replacementBody) || !os.SameFile(replacementInfo, after) {
		t.Fatalf("post-publication mismatch overwrote or deleted replacement: body=%q read=%v stat=%v", body, readErr, statErr)
	}
	if _, err := os.Lstat(filepath.Join(root, metadataSnapshotTempName)); err != nil {
		t.Fatalf("post-publication mismatch lost the prior active snapshot slot: %v", err)
	}
	if _, err := os.Lstat(publishedElsewhere); err != nil {
		t.Fatalf("post-publication mismatch lost the complete new snapshot: %v", err)
	}
	resetMetadataTestHooks()
	_ = opened.Close()
}

func TestMetadataPublicationRejectsDestinationSubstitutionWithoutDeletingIt(t *testing.T) {
	tests := []struct {
		name    string
		install func(*testing.T, string) (os.FileInfo, func() error)
	}{
		{
			name: "symlink",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				sentinel := filepath.Join(t.TempDir(), "sentinel")
				if err := os.WriteFile(sentinel, []byte("symlink sentinel"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, path); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error {
					body, err := os.ReadFile(sentinel)
					if err != nil || string(body) != "symlink sentinel" {
						return fmt.Errorf("symlink target changed: %q: %w", body, err)
					}
					return nil
				}
			},
		},
		{
			name: "hardlink",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				sentinel := filepath.Join(t.TempDir(), "hardlink-sentinel")
				body := []byte("hardlink sentinel")
				if err := os.WriteFile(sentinel, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(sentinel, path); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error {
					retained, err := os.ReadFile(sentinel)
					if err != nil || !bytes.Equal(retained, body) {
						return fmt.Errorf("hardlink target changed: %q: %w", retained, err)
					}
					return nil
				}
			},
		},
		{
			name: "special fifo",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error { return nil }
			},
		},
		{
			name: "mode",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				if err := os.WriteFile(path, []byte("mode replacement"), 0o640); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error { return nil }
			},
		},
		{
			name: "size",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error { return nil }
			},
		},
		{
			name: "same owner mode size different inode",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				original := path + ".saved-for-copy"
				body, err := os.ReadFile(original)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error { return nil }
			},
		},
	}
	if os.Geteuid() == 0 {
		tests = append(tests, struct {
			name    string
			install func(*testing.T, string) (os.FileInfo, func() error)
		}{
			name: "owner",
			install: func(t *testing.T, path string) (os.FileInfo, func() error) {
				t.Helper()
				if err := os.WriteFile(path, []byte("owner replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chown(path, 1, -1); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info, func() error { return nil }
			},
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer resetMetadataTestHooks()
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(root, metadataFileName)
			savedOriginal := metadataPath + ".saved-for-copy"
			var replacement os.FileInfo
			var verify func() error
			opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
				if boundary != metadataBoundaryBeforeSnapshotWrite {
					return nil
				}
				if err := os.Rename(metadataPath, savedOriginal); err != nil {
					return err
				}
				replacement, verify = test.install(t, metadataPath)
				return nil
			}
			response := testValidatedResponse(t, "ignored", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte("destination substitution raw "+test.name), []byte("destination substitution evidence "+test.name))
			if committed, err := opened.commit(t.Context(), response); err == nil || committed.acquisitionID != "" {
				t.Fatalf("destination substitution returned success: committed=%+v err=%v", committed, err)
			}
			after, err := os.Lstat(metadataPath)
			if err != nil || replacement == nil || !os.SameFile(replacement, after) {
				t.Fatalf("destination substitution was overwritten or deleted: replacement=%v after=%v err=%v", replacement, after, err)
			}
			if verify != nil {
				if err := verify(); err != nil {
					t.Fatal(err)
				}
			}
			stage, err := os.Lstat(filepath.Join(root, metadataSnapshotTempName))
			if err != nil || !stage.Mode().IsRegular() {
				t.Fatalf("destination substitution lost the complete staged snapshot: info=%v err=%v", stage, err)
			}
			resetMetadataTestHooks()
			_ = opened.Close()
		})
	}
}

func TestInactiveMetadataSlotReuseRaceIsReadOnlyAndFailsClosed(t *testing.T) {
	type replacementFixture struct {
		info   os.FileInfo
		verify func() error
	}
	tests := []struct {
		name    string
		install func(*testing.T, string, os.FileInfo) replacementFixture
	}{
		{
			name: "symlink",
			install: func(t *testing.T, path string, _ os.FileInfo) replacementFixture {
				t.Helper()
				sentinel := filepath.Join(t.TempDir(), "symlink-sentinel")
				body := []byte("inactive-slot symlink sentinel")
				if err := os.WriteFile(sentinel, body, 0o600); err != nil {
					t.Fatal(err)
				}
				sentinelInfo, err := os.Lstat(sentinel)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, path); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return replacementFixture{info: info, verify: func() error {
					retained, readErr := os.ReadFile(sentinel)
					after, statErr := os.Lstat(sentinel)
					if readErr != nil || statErr != nil || !bytes.Equal(retained, body) || !os.SameFile(sentinelInfo, after) {
						return fmt.Errorf("symlink target changed: body=%q read=%v stat=%v", retained, readErr, statErr)
					}
					return nil
				}}
			},
		},
		{
			name: "hardlink",
			install: func(t *testing.T, path string, _ os.FileInfo) replacementFixture {
				t.Helper()
				sentinel := filepath.Join(t.TempDir(), "hardlink-sentinel")
				body := []byte("inactive-slot hardlink sentinel")
				if err := os.WriteFile(sentinel, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(sentinel, path); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return replacementFixture{info: info, verify: func() error {
					retained, readErr := os.ReadFile(sentinel)
					linked, statErr := os.Lstat(sentinel)
					if readErr != nil || statErr != nil || !bytes.Equal(retained, body) || !os.SameFile(info, linked) {
						return fmt.Errorf("hardlink target changed: body=%q read=%v stat=%v", retained, readErr, statErr)
					}
					return nil
				}}
			},
		},
		{
			name: "special fifo",
			install: func(t *testing.T, path string, _ os.FileInfo) replacementFixture {
				t.Helper()
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return replacementFixture{info: info, verify: func() error {
					after, statErr := os.Lstat(path)
					if statErr != nil || after.Mode()&os.ModeNamedPipe == 0 {
						return fmt.Errorf("FIFO replacement changed: info=%v stat=%v", after, statErr)
					}
					return nil
				}}
			},
		},
		{
			name: "same owner mode size different inode",
			install: func(t *testing.T, path string, original os.FileInfo) replacementFixture {
				t.Helper()
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(original, info) || original.Size() != info.Size() || original.Mode() != info.Mode() {
					t.Fatalf("replacement did not preserve metadata while changing inode: original=%v replacement=%v", original, info)
				}
				return replacementFixture{info: info, verify: func() error {
					body, readErr := os.ReadFile(path)
					if readErr != nil || len(body) != 0 {
						return fmt.Errorf("different-inode replacement changed: body=%q read=%v", body, readErr)
					}
					return nil
				}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer resetMetadataTestHooks()
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			var writePath, saved string
			var original os.FileInfo
			var originalBody []byte
			var replacement replacementFixture
			fired := false
			opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
				if boundary != metadataBoundaryBeforeSnapshotWrite || fired {
					return nil
				}
				fired = true
				if opened.metadataWritePath == "" {
					return errors.New("inactive metadata slot was not allocated")
				}
				writePath = filepath.Join(root, opened.metadataWritePath)
				var err error
				originalBody, err = os.ReadFile(writePath)
				if err != nil {
					return err
				}
				original, err = os.Lstat(writePath)
				if err != nil {
					return err
				}
				saved = filepath.Join(t.TempDir(), "verified-inactive-slot")
				if err := os.Rename(writePath, saved); err != nil {
					return err
				}
				replacement = test.install(t, writePath, original)
				return nil
			}
			response := testValidatedResponse(t, "ignored", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte("inactive slot race raw "+test.name), []byte("inactive slot race evidence "+test.name))
			_, _, acquisitionID, _, err := buildManifest(response)
			if err != nil {
				t.Fatal(err)
			}
			committed, commitErr := opened.commit(t.Context(), response)
			if commitErr == nil || committed.acquisitionID != "" || opened.failure == nil || !errors.Is(opened.failure, commitErr) {
				t.Fatalf("inactive-slot race did not fail closed: committed=%+v commit=%v failure=%v", committed, commitErr, opened.failure)
			}
			if replayed, replayErr := opened.replayByAcquisitionID(t.Context(), acquisitionID); replayErr == nil || replayed.acquisitionID != "" || !errors.Is(replayErr, commitErr) {
				t.Fatalf("inactive-slot race remained available for replay: replayed=%+v err=%v", replayed, replayErr)
			}
			if retried, retryErr := opened.commit(t.Context(), response); retryErr == nil || retried.acquisitionID != "" || !errors.Is(retryErr, commitErr) {
				t.Fatalf("inactive-slot race remained available for commit: committed=%+v err=%v", retried, retryErr)
			}
			after, statErr := os.Lstat(writePath)
			if statErr != nil || replacement.info == nil || !os.SameFile(replacement.info, after) {
				t.Fatalf("inactive-slot replacement was moved or deleted: replacement=%v after=%v stat=%v", replacement.info, after, statErr)
			}
			if replacement.verify != nil {
				if err := replacement.verify(); err != nil {
					t.Fatal(err)
				}
			}
			retainedBody, readErr := os.ReadFile(saved)
			retained, retainedErr := os.Lstat(saved)
			if readErr != nil || retainedErr != nil || original == nil || !bytes.Equal(retainedBody, originalBody) || !os.SameFile(original, retained) {
				t.Fatalf("verified inactive descriptor was written after its pathname raced: body=%q read=%v stat=%v", retainedBody, readErr, retainedErr)
			}
			state := readActiveDiskMetadataState(t, root, "")
			if state.acquisitions != 0 || state.requests != 0 || state.counters != (metadataCounters{}) {
				t.Fatalf("failed inactive-slot reuse selected a new logical state: %+v", state)
			}
			resetMetadataTestHooks()
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataSelectorReusableSlotUsesPinnedDescriptorRepeatedly(t *testing.T) {
	for iteration := 0; iteration < 8; iteration++ {
		t.Run(fmt.Sprintf("iteration_%02d", iteration), func(t *testing.T) {
			defer resetMetadataTestHooks()
			root := filepath.Join(t.TempDir(), "ledger")
			opened, err := openSpool(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			selectorPath := filepath.Join(root, metadataStateDirectory, metadataSelectorSlotName(0))
			saved := filepath.Join(t.TempDir(), "verified-selector-slot")
			replacementBody := []byte(fmt.Sprintf("selector replacement %d", iteration))
			var replacement os.FileInfo
			fired := false
			opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
				if boundary != metadataBoundaryBeforeSelectorWrite || fired {
					return nil
				}
				fired = true
				if err := os.Rename(selectorPath, saved); err != nil {
					return err
				}
				if err := os.WriteFile(selectorPath, replacementBody, 0o600); err != nil {
					return err
				}
				var err error
				replacement, err = os.Lstat(selectorPath)
				return err
			}
			response := testValidatedResponse(t, "ignored", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte(fmt.Sprintf("selector race raw %d", iteration)), []byte(fmt.Sprintf("selector race evidence %d", iteration)))
			_, _, acquisitionID, _, err := buildManifest(response)
			if err != nil {
				t.Fatal(err)
			}
			if committed, err := opened.commit(t.Context(), response); err == nil || committed.acquisitionID != "" {
				t.Fatalf("selector slot replacement returned success: committed=%+v err=%v", committed, err)
			}
			body, readErr := os.ReadFile(selectorPath)
			after, statErr := os.Lstat(selectorPath)
			if readErr != nil || statErr != nil || replacement == nil || !bytes.Equal(body, replacementBody) || !os.SameFile(replacement, after) {
				t.Fatalf("selector slot replacement moved or changed: body=%q read=%v stat=%v", body, readErr, statErr)
			}
			resetMetadataTestHooks()
			_ = opened.Close()
			reopened, err := openExistingSpool(t.Context(), root)
			if err != nil {
				t.Fatalf("reopen after selector-slot race: %v", err)
			}
			if _, err := reopened.replayByAcquisitionID(t.Context(), acquisitionID); err != nil {
				_ = reopened.Close()
				t.Fatalf("selector-slot race recovery lost exact logical state: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAtomicNoReplacePublicationPreservesRacedSourceAndDestination(t *testing.T) {
	t.Run("destination", func(t *testing.T) {
		defer resetMetadataTestHooks()
		root := filepath.Join(t.TempDir(), "ledger")
		opened, err := openSpool(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		response := testValidatedResponse(t, "ignored", "oldid:34",
			"2026-07-31T12:00:00.123456789Z", []byte("destination race raw"), []byte("destination race evidence"))
		manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
		if err != nil {
			t.Fatal(err)
		}
		_, markerBody, err := buildPendingMarker(manifest, acquisitionID, key, len(manifestBody))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, pendingDirectory, acquisitionID+".json")
		replacementBody := []byte("destination race replacement")
		var replacement os.FileInfo
		testHookBeforeAtomicNoReplacePublish = func() error {
			testHookBeforeAtomicNoReplacePublish = nil
			if err := os.WriteFile(target, replacementBody, 0o600); err != nil {
				return err
			}
			var err error
			replacement, err = os.Lstat(target)
			return err
		}
		if _, err := opened.publishMarker(acquisitionID, markerBody); err == nil {
			t.Fatal("no-replace publication accepted a raced destination")
		}
		body, readErr := os.ReadFile(target)
		after, statErr := os.Lstat(target)
		if readErr != nil || statErr != nil || replacement == nil || !bytes.Equal(body, replacementBody) || !os.SameFile(replacement, after) {
			t.Fatalf("raced destination changed: body=%q read=%v stat=%v", body, readErr, statErr)
		}
		staged := filepath.Join(root, pendingDirectory, acquisitionID+".marker.tmp")
		if _, err := os.Lstat(staged); err != nil {
			t.Fatalf("raced destination caused staged inode deletion: %v", err)
		}
		_ = opened.Close()
	})

	t.Run("source", func(t *testing.T) {
		defer resetMetadataTestHooks()
		root := filepath.Join(t.TempDir(), "ledger")
		opened, err := openSpool(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		response := testValidatedResponse(t, "ignored", "oldid:34",
			"2026-07-31T12:00:00.123456789Z", []byte("source race raw"), []byte("source race evidence"))
		manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
		if err != nil {
			t.Fatal(err)
		}
		_, markerBody, err := buildPendingMarker(manifest, acquisitionID, key, len(manifestBody))
		if err != nil {
			t.Fatal(err)
		}
		staged := filepath.Join(root, pendingDirectory, acquisitionID+".marker.tmp")
		saved := filepath.Join(t.TempDir(), "saved-original-stage")
		target := filepath.Join(root, pendingDirectory, acquisitionID+".json")
		replacementBody := []byte("source race replacement")
		var replacement os.FileInfo
		testHookBeforeAtomicNoReplacePublish = func() error {
			testHookBeforeAtomicNoReplacePublish = nil
			if err := os.Rename(staged, saved); err != nil {
				return err
			}
			if err := os.WriteFile(staged, replacementBody, 0o600); err != nil {
				return err
			}
			var err error
			replacement, err = os.Lstat(staged)
			return err
		}
		if _, err := opened.publishMarker(acquisitionID, markerBody); err == nil {
			t.Fatal("no-replace publication accepted a raced source")
		}
		publishedBody, publishedErr := os.ReadFile(target)
		if publishedErr != nil || !bytes.Equal(publishedBody, markerBody) {
			t.Fatalf("descriptor publication did not use the verified source: body=%q err=%v", publishedBody, publishedErr)
		}
		replacementAfterBody, readErr := os.ReadFile(staged)
		replacementAfter, statErr := os.Lstat(staged)
		if readErr != nil || statErr != nil || replacement == nil || !bytes.Equal(replacementAfterBody, replacementBody) || !os.SameFile(replacement, replacementAfter) {
			t.Fatalf("raced source replacement moved or changed: body=%q read=%v stat=%v", replacementAfterBody, readErr, statErr)
		}
		if original, err := os.ReadFile(saved); err != nil || !bytes.Equal(original, markerBody) {
			t.Fatalf("original staged inode was not preserved by the test race: body=%q err=%v", original, err)
		}
		_ = opened.Close()
	})
}

func TestOwnedFileRetirementPreservesReplacementIntroducedAtAtomicBoundary(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("retire race raw"), []byte("retire race evidence"))
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	marker, markerBody, err := buildPendingMarker(manifest, acquisitionID, key, len(manifestBody))
	if err != nil {
		t.Fatal(err)
	}
	markerInfo, err := opened.publishMarker(acquisitionID, markerBody)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, pendingDirectory, acquisitionID+".json")
	saved := filepath.Join(t.TempDir(), "saved-owned-marker")
	replacementBody := append([]byte(nil), markerBody...)
	replacementBody[len(replacementBody)-1] ^= 1
	var replacement os.FileInfo
	testHookBeforeOwnedFileRetire = func() error {
		testHookBeforeOwnedFileRetire = nil
		if err := os.Rename(path, saved); err != nil {
			return err
		}
		if err := os.WriteFile(path, replacementBody, 0o600); err != nil {
			return err
		}
		var err error
		replacement, err = os.Lstat(path)
		return err
	}
	if err := opened.removeMarker(acquisitionID, markerBody, markerInfo); err == nil {
		t.Fatal("owned-file retirement accepted a replacement introduced at the atomic boundary")
	}
	body, readErr := os.ReadFile(path)
	after, statErr := os.Lstat(path)
	if readErr != nil || statErr != nil || replacement == nil || !bytes.Equal(body, replacementBody) || !os.SameFile(replacement, after) {
		t.Fatalf("bounded retention moved or changed the replacement: body=%q read=%v stat=%v", body, readErr, statErr)
	}
	if _, err := os.Lstat(filepath.Join(root, quarantineDirectory, acquisitionID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bounded retention created an unexpected quarantine pathname: %v", err)
	}
	if original, err := os.ReadFile(saved); err != nil || !bytes.Equal(original, markerBody) {
		t.Fatalf("original owned marker was lost by the test race: body=%q err=%v", original, err)
	}
	if marker.AcquisitionID != acquisitionID {
		t.Fatal("marker fixture changed identity")
	}
	_ = opened.Close()
}

func TestConnectorRebindHoldsOneCoherentLockBoundary(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	opened.database.SetMaxIdleConns(0)
	rebindEntered := make(chan struct{})
	allowRebind := make(chan struct{})
	var once sync.Once
	opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		if boundary == metadataBoundaryBeforeConnectorRebind {
			once.Do(func() { close(rebindEntered) })
			<-allowRebind
		}
		return nil
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("connector lock raw"), []byte("connector lock evidence"))
	commitResult := make(chan error, 1)
	go func() {
		_, err := opened.commit(t.Context(), response)
		commitResult <- err
	}()
	<-rebindEntered
	runtimeOpenEntered := make(chan struct{})
	allowRuntimeOpen := make(chan struct{})
	var runtimeOnce sync.Once
	testHookBeforeSQLiteRuntimeOpen = func() error {
		runtimeOnce.Do(func() { close(runtimeOpenEntered) })
		<-allowRuntimeOpen
		return nil
	}
	connectionResult := make(chan error, 1)
	go func() {
		connection, err := opened.database.Conn(t.Context())
		if connection != nil {
			err = errors.Join(err, connection.Close())
		}
		connectionResult <- err
	}()
	select {
	case <-runtimeOpenEntered:
		t.Fatal("SQLite Connect crossed the connector rebind write lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(allowRebind)
	select {
	case <-runtimeOpenEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("SQLite Connect did not resume after connector rebind")
	}
	close(allowRuntimeOpen)
	if err := <-commitResult; err != nil {
		t.Fatalf("commit across connector rebind failed: %v", err)
	}
	if err := <-connectionResult; err != nil {
		t.Fatalf("fresh connection after connector rebind failed: %v", err)
	}
	opened.metadataConnector.mu.RLock()
	connectorFile := opened.metadataConnector.metadataFile
	opened.metadataConnector.mu.RUnlock()
	if connectorFile != opened.metadataFile {
		t.Fatal("connector and spool retained different metadata descriptors")
	}
}

func TestConcurrentCommitAndReplayAreActuallySerialized(t *testing.T) {
	defer resetMetadataTestHooks()
	root := filepath.Join(t.TempDir(), "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	firstInput := exportedTestRecord(testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("serialized first raw"), []byte("serialized first evidence")))
	first, err := ledger.Commit(t.Context(), firstInput)
	if err != nil {
		t.Fatal(err)
	}
	publishEntered := make(chan struct{})
	allowPublish := make(chan struct{})
	var once sync.Once
	ledger.spool.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		if boundary != metadataBoundaryBeforeSnapshotWrite {
			return nil
		}
		once.Do(func() { close(publishEntered) })
		<-allowPublish
		return nil
	}
	secondInput := exportedTestRecord(testValidatedResponse(t, "ignored", "oldid:35",
		"2026-07-31T12:00:01.123456789Z", []byte("serialized second raw"), []byte("serialized second evidence")))
	commitResult := make(chan error, 1)
	go func() {
		_, err := ledger.Commit(t.Context(), secondInput)
		commitResult <- err
	}()
	<-publishEntered
	replayResult := make(chan error, 1)
	go func() {
		_, err := ledger.ReplayByAcquisitionID(t.Context(), first.AcquisitionID)
		replayResult <- err
	}()
	select {
	case err := <-replayResult:
		t.Fatalf("replay crossed an active commit serialization boundary: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowPublish)
	if err := <-commitResult; err != nil {
		t.Fatalf("serialized commit failed: %v", err)
	}
	if err := <-replayResult; err != nil {
		t.Fatalf("serialized replay failed after commit: %v", err)
	}
}

func TestMetadataSnapshotRotationIsBoundedAndLeavesImmutableArtifactsUntouched(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	firstResponse := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("immutable artifact raw"), []byte("immutable artifact evidence"))
	first, err := opened.commit(t.Context(), firstResponse)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, blobsDirectory, first.rawResponseSHA256),
		filepath.Join(root, blobsDirectory, first.evidence.rawSHA256),
		filepath.Join(root, blobsDirectory, first.envelope.sha256),
		filepath.Join(root, manifestsDirectory, first.acquisitionID+".json"),
	}
	type artifact struct {
		info os.FileInfo
		body []byte
	}
	before := make([]artifact, len(paths))
	for index, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[index] = artifact{info: info, body: body}
	}
	for index := 35; index <= 38; index++ {
		response := testValidatedResponse(t, "ignored", fmt.Sprintf("oldid:%d", index),
			fmt.Sprintf("2026-07-31T12:00:%02d.123456789Z", index-34),
			[]byte(fmt.Sprintf("rotation raw %d", index)), []byte(fmt.Sprintf("rotation evidence %d", index)))
		if _, err := opened.commit(t.Context(), response); err != nil {
			t.Fatal(err)
		}
	}
	for index, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil || !bytes.Equal(body, before[index].body) || !os.SameFile(info, before[index].info) {
			t.Fatalf("metadata rotation rewrote immutable artifact %s: err=%v", path, err)
		}
	}
	slots, selectors := countMetadataStateMachineEntries(t, root)
	if slots != 3 || selectors != maxMetadataSelectorSlots {
		t.Fatalf("metadata rotation state machine slots=%d selectors=%d, want 3/%d", slots, selectors, maxMetadataSelectorSlots)
	}
	active, err := os.Lstat(filepath.Join(root, metadataFileName))
	retained, retainedErr := os.Lstat(filepath.Join(root, metadataSnapshotTempName))
	if err != nil || retainedErr != nil || os.SameFile(active, retained) {
		t.Fatalf("metadata rotation slots are invalid: active=%v retained=%v err=%v/%v", active, retained, err, retainedErr)
	}
}

func countMetadataStateMachineEntries(t *testing.T, root string) (int, int) {
	t.Helper()
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	slots := 0
	for _, entry := range rootEntries {
		name := entry.Name()
		if metadataSlotNameAllowed(name) {
			slots++
			continue
		}
		if strings.HasPrefix(name, ".metadata.db") {
			t.Fatalf("metadata slot namespace contains unrecognized entry %q", name)
		}
	}
	stateEntries, err := os.ReadDir(filepath.Join(root, metadataStateDirectory))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stateEntries {
		if !metadataSelectorSlotNamePattern.MatchString(entry.Name()) {
			t.Fatalf("fresh bounded selector namespace contains append-only or unrecognized entry %q", entry.Name())
		}
	}
	return slots, len(stateEntries)
}

func TestUnsupportedSidecarFailsBeforeLegacyQuarantineInitialization(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, quarantineDirectory)); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(root, metadataFileName+"-wal")
	body := []byte("unsupported legacy sidecar")
	if err := os.WriteFile(sidecar, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if reopened, err := openExistingSpool(t.Context(), root); err == nil {
		_ = reopened.Close()
		t.Fatal("open accepted an unsupported sidecar on a pre-quarantine ledger")
	}
	if _, err := os.Lstat(filepath.Join(root, quarantineDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported sidecar rejection initialized quarantine: %v", err)
	}
	afterBody, readErr := os.ReadFile(sidecar)
	after, statErr := os.Lstat(sidecar)
	if readErr != nil || statErr != nil || !bytes.Equal(afterBody, body) || !os.SameFile(before, after) {
		t.Fatalf("unsupported sidecar was touched: body=%q read=%v stat=%v", afterBody, readErr, statErr)
	}
}

type forbiddenASTReference struct {
	importPath string
	function   string
	position   token.Position
}

func firstForbiddenPathnamePrimitiveReference(fileSet *token.FileSet, parsed *ast.File) (*forbiddenASTReference, error) {
	imports := make(map[string]string)
	for _, imported := range parsed.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, err
		}
		alias := filepath.Base(importPath)
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		if importPath == "os/exec" || importPath == "plugin" || importPath == "C" ||
			alias == "." && forbiddenPathnamePrimitive(importPath, "") {
			return &forbiddenASTReference{importPath: importPath, position: fileSet.Position(imported.Pos())}, nil
		}
		if alias != "_" && alias != "." {
			imports[alias] = importPath
		}
	}
	var found *forbiddenASTReference
	ast.Inspect(parsed, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		var importPath, function string
		switch reference := node.(type) {
		case *ast.SelectorExpr:
			qualifier, ok := reference.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath = imports[qualifier.Name]
			function = reference.Sel.Name
		case *ast.Ident:
			if _, importedQualifier := imports[reference.Name]; importedQualifier {
				return true
			}
			function = reference.Name
		default:
			return true
		}
		if forbiddenPathnamePrimitive(importPath, function) {
			found = &forbiddenASTReference{importPath: importPath, function: function, position: fileSet.Position(node.Pos())}
			return false
		}
		return true
	})
	return found, nil
}

func TestNoPathnameCheckThenRenameOrUnlinkDeletionPrimitiveRemains(t *testing.T) {
	entries, err := os.ReadDir(packageSourceDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(packageSourceDirectory(t), name)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := firstForbiddenPathnamePrimitiveReference(fileSet, parsed)
		if err != nil {
			t.Fatalf("inspect imports in %s: %v", name, err)
		}
		if reference != nil {
			t.Fatalf("%s:%d still references forbidden pathname move/deletion primitive %s.%s",
				name, reference.position.Line, reference.importPath, reference.function)
		}
	}
}

var guardSeedSyscallPrimitiveFunctions = []string{
	"Rename", "Renameat", "Renameat2", "RenameatxNp", "RenamexNp",
	"Unlink", "Unlinkat", "Rmdir", "Remove", "RemoveAll",
	"Exec", "ForkExec", "StartProcess",
	"Syscall", "Syscall6", "Syscall9", "Syscall12", "Syscall15", "Syscall18", "SyscallN", "SyscallNoError",
	"AllThreadsSyscall", "AllThreadsSyscall6",
	"RawSyscall", "RawSyscall6", "RawSyscallNoError",
}

var guardSeedRawSyscallBridgeFunctions = []string{
	"Syscall", "Syscall6", "Syscall9", "Syscall12", "Syscall15", "Syscall18", "SyscallN", "SyscallNoError",
	"AllThreadsSyscall", "AllThreadsSyscall6",
	"RawSyscall", "RawSyscall6", "RawSyscallNoError",
}

func forbiddenPathnamePrimitive(importPath, function string) bool {
	forbidden := map[string]map[string]bool{
		"os": {
			"Rename": true, "Remove": true, "RemoveAll": true, "StartProcess": true,
		},
		"syscall": {
			"Rename": true, "Renameat": true, "Renameat2": true, "RenameatxNp": true, "RenamexNp": true,
			"Unlink": true, "Unlinkat": true, "Rmdir": true, "Remove": true, "RemoveAll": true,
			"Exec": true, "ForkExec": true, "StartProcess": true,
			"Syscall": true, "Syscall6": true, "Syscall9": true, "Syscall12": true, "Syscall15": true,
			"Syscall18": true, "SyscallN": true, "SyscallNoError": true,
			"AllThreadsSyscall": true, "AllThreadsSyscall6": true,
			"RawSyscall": true, "RawSyscall6": true, "RawSyscallNoError": true,
		},
		"golang.org/x/sys/unix": {
			"Rename": true, "Renameat": true, "Renameat2": true, "RenameatxNp": true, "RenamexNp": true,
			"Unlink": true, "Unlinkat": true, "Rmdir": true, "Remove": true, "RemoveAll": true,
			"Exec": true, "ForkExec": true, "StartProcess": true,
			"Syscall": true, "Syscall6": true, "Syscall9": true, "Syscall12": true, "Syscall15": true,
			"Syscall18": true, "SyscallN": true, "SyscallNoError": true,
			"AllThreadsSyscall": true, "AllThreadsSyscall6": true,
			"RawSyscall": true, "RawSyscall6": true, "RawSyscallNoError": true,
		},
		"os/exec": {
			"Command": true, "CommandContext": true,
		},
	}
	if functions, found := forbidden[importPath]; found {
		return function == "" || functions[function]
	}
	switch strings.ToLower(function) {
	case "rename", "renameat", "renameat2", "renameatxnp", "renamexnp",
		"unlink", "unlinkat", "rmdir", "remove", "removeall",
		"command", "commandcontext", "exec", "forkexec", "startprocess",
		"syscall", "syscall6", "syscall9", "syscall12", "syscall15", "syscall18", "syscalln", "syscallnoerror",
		"allthreadssyscall", "allthreadssyscall6",
		"rawsyscall", "rawsyscall6", "rawsyscallnoerror",
		"atomicswapat", "atomicpublishnoreplaceat":
		return true
	default:
		return false
	}
}

func TestForbiddenPathnamePrimitiveGuardCoversAliasesReferencesAndSubprocessBypasses(t *testing.T) {
	contracts := []struct {
		importPath string
		functions  []string
	}{
		{importPath: "os", functions: []string{"Rename", "Remove", "RemoveAll", "StartProcess"}},
		{importPath: "syscall", functions: guardSeedSyscallPrimitiveFunctions},
		{importPath: "golang.org/x/sys/unix", functions: guardSeedSyscallPrimitiveFunctions},
		{importPath: "os/exec", functions: []string{"Command", "CommandContext"}},
	}
	for _, contract := range contracts {
		for _, function := range contract.functions {
			if !forbiddenPathnamePrimitive(contract.importPath, function) {
				t.Fatalf("guard omitted forbidden reference %s.%s", contract.importPath, function)
			}
		}
	}
	for _, function := range append(append([]string(nil), guardSeedSyscallPrimitiveFunctions...),
		"Command", "CommandContext", "atomicSwapAt", "atomicPublishNoReplaceAt") {
		for _, variant := range []string{function, strings.ToLower(function[:1]) + function[1:]} {
			if !forbiddenPathnamePrimitive("example.invalid/guard-seed", variant) {
				t.Fatalf("guard omitted generic forbidden reference %s", variant)
			}
		}
	}
	for _, allowed := range []struct {
		importPath string
		function   string
	}{
		{importPath: "os", function: "OpenFile"},
		{importPath: "syscall", function: "Stat_t"},
		{importPath: "golang.org/x/sys/unix", function: "Linkat"},
		{importPath: "golang.org/x/sys/unix", function: "Fclonefileat"},
		{importPath: "database/sql", function: "ExecContext"},
		{importPath: "", function: "retireOwnedStagedFileAt"},
	} {
		if forbiddenPathnamePrimitive(allowed.importPath, allowed.function) {
			t.Fatalf("guard rejected reviewed reference %s.%s", allowed.importPath, allowed.function)
		}
	}
}

func TestForbiddenPathnamePrimitiveASTGuardRejectsSeededSyscallVariants(t *testing.T) {
	type seed struct {
		name       string
		source     string
		importPath string
		function   string
	}
	var seeds []seed
	appendQualifiedReferences := func(importPath, alias string, functions []string) {
		t.Helper()
		for _, function := range functions {
			seeds = append(seeds, seed{
				name:       "qualified " + importPath + " " + function,
				source:     fmt.Sprintf("package seeded\nimport %s %q\nvar _ = %s.%s\n", alias, importPath, alias, function),
				importPath: importPath,
				function:   function,
			})
		}
	}
	appendQualifiedReferences("os", "seedos", []string{"Rename", "Remove", "RemoveAll", "StartProcess"})
	appendQualifiedReferences("syscall", "sc", guardSeedSyscallPrimitiveFunctions)
	appendQualifiedReferences("golang.org/x/sys/unix", "ux", guardSeedSyscallPrimitiveFunctions)

	// These raw-operation sources are parsed from memory only. The seeded calls
	// are never type-checked or executed, so every raw syscall bypass remains a
	// read-only guard input even when a constant is platform-specific.
	rawOperations := []string{
		"SYS_RENAME", "SYS_RENAMEAT", "SYS_RENAMEAT2", "SYS_RENAMEATX_NP", "SYS_RENAMEX_NP",
		"SYS_UNLINK", "SYS_UNLINKAT", "SYS_RMDIR", "SYS_REMOVE",
		"SYS_EXECVE", "SYS_EXECVEAT", "SYS_FORK", "SYS_VFORK", "SYS_CLONE", "SYS_CLONE3", "SYS_POSIX_SPAWN",
	}
	for _, imported := range []struct {
		path  string
		alias string
	}{
		{path: "syscall", alias: "sc"},
		{path: "golang.org/x/sys/unix", alias: "ux"},
	} {
		for _, bridge := range guardSeedRawSyscallBridgeFunctions {
			for _, operation := range rawOperations {
				seeds = append(seeds, seed{
					name: "read-only raw " + imported.path + " " + bridge + " " + operation,
					source: fmt.Sprintf("package seeded\nimport %s %q\nfunc readOnlySeed() { _, _, _ = %s.%s(uintptr(%s.%s), 0, 0, 0) }\n",
						imported.alias, imported.path, imported.alias, bridge, imported.alias, operation),
					importPath: imported.path,
					function:   bridge,
				})
			}
		}
	}

	for _, imported := range []struct {
		path      string
		functions []string
	}{
		{path: "os", functions: []string{"Rename", "Remove", "RemoveAll", "StartProcess"}},
		{path: "syscall", functions: guardSeedSyscallPrimitiveFunctions},
		{path: "golang.org/x/sys/unix", functions: guardSeedSyscallPrimitiveFunctions},
	} {
		for _, function := range imported.functions {
			seeds = append(seeds, seed{
				name:       "dot import " + imported.path + " " + function,
				source:     fmt.Sprintf("package seeded\nimport . %q\nvar _ = %s\n", imported.path, function),
				importPath: imported.path,
			})
		}
	}

	genericFunctions := append(append([]string(nil), guardSeedSyscallPrimitiveFunctions...),
		"Command", "CommandContext", "atomicSwapAt", "atomicPublishNoReplaceAt")
	seenGeneric := make(map[string]bool, len(genericFunctions)*2)
	for _, function := range genericFunctions {
		for _, variant := range []string{function, strings.ToLower(function[:1]) + function[1:]} {
			if seenGeneric[variant] {
				continue
			}
			seenGeneric[variant] = true
			seeds = append(seeds,
				seed{
					name:       "generic package wrapper " + variant,
					source:     fmt.Sprintf("package seeded\nimport helper %q\nvar _ = helper.%s\n", "example.invalid/guard-seed", variant),
					importPath: "example.invalid/guard-seed",
					function:   variant,
				},
				seed{
					name:     "local wrapper " + variant,
					source:   fmt.Sprintf("package seeded\nvar _ = %s\n", variant),
					function: variant,
				},
			)
		}
	}

	for _, reference := range []string{
		"runner.Command", "runner.CommandContext", "(*runner.Cmd).Start", "(*runner.Cmd).Run",
		"(*runner.Cmd).Output", "(*runner.Cmd).CombinedOutput",
	} {
		seeds = append(seeds, seed{
			name:       "os exec subprocess " + reference,
			source:     fmt.Sprintf("package seeded\nimport runner %q\nvar _ = %s\n", "os/exec", reference),
			importPath: "os/exec",
		})
	}
	for _, imported := range []struct {
		name        string
		declaration string
		path        string
	}{
		{name: "os exec default", declaration: `"os/exec"`, path: "os/exec"},
		{name: "os exec dot", declaration: `. "os/exec"`, path: "os/exec"},
		{name: "os exec blank", declaration: `_ "os/exec"`, path: "os/exec"},
		{name: "plugin default", declaration: `"plugin"`, path: "plugin"},
		{name: "plugin alias", declaration: `loader "plugin"`, path: "plugin"},
		{name: "plugin dot", declaration: `. "plugin"`, path: "plugin"},
		{name: "plugin blank", declaration: `_ "plugin"`, path: "plugin"},
		{name: "cgo default", declaration: `"C"`, path: "C"},
		{name: "cgo alias", declaration: `bridge "C"`, path: "C"},
		{name: "cgo dot", declaration: `. "C"`, path: "C"},
		{name: "cgo blank", declaration: `_ "C"`, path: "C"},
	} {
		seeds = append(seeds, seed{
			name:       imported.name,
			source:     "package seeded\nimport " + imported.declaration + "\n",
			importPath: imported.path,
		})
	}

	for _, seed := range seeds {
		t.Run(seed.name, func(t *testing.T) {
			fileSet := token.NewFileSet()
			parsed, err := parser.ParseFile(fileSet, "seeded.go", seed.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			reference, err := firstForbiddenPathnamePrimitiveReference(fileSet, parsed)
			if err != nil {
				t.Fatal(err)
			}
			if reference == nil || reference.importPath != seed.importPath || reference.function != seed.function {
				t.Fatalf("seeded forbidden reference=%+v, want %s.%s", reference, seed.importPath, seed.function)
			}
		})
	}

	for name, source := range map[string]string{
		"descriptor-pinned Linkat":       "package seeded\nimport unix \"golang.org/x/sys/unix\"\nvar _ = unix.Linkat\n",
		"descriptor-pinned Fclonefileat": "package seeded\nimport unix \"golang.org/x/sys/unix\"\nvar _ = unix.Fclonefileat\n",
		"read-only os open":              "package seeded\nimport \"os\"\nvar _ = os.OpenFile\n",
		"SQLite ExecContext":             "package seeded\nimport \"database/sql\"\nvar _ = (*sql.DB).ExecContext\n",
	} {
		t.Run("allowed "+name, func(t *testing.T) {
			fileSet := token.NewFileSet()
			allowed, err := parser.ParseFile(fileSet, "allowed.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if reference, err := firstForbiddenPathnamePrimitiveReference(fileSet, allowed); err != nil || reference != nil {
				t.Fatalf("AST guard rejected %s seed: reference=%+v err=%v", name, reference, err)
			}
		})
	}
}

func packageSourceDirectory(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return working
}
