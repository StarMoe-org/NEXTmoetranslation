//go:build !darwin && !linux

package lyricsacquisition

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformPublishesCompleteFailClosedNamespaceContract(t *testing.T) {
	for label, name := range map[string]string{
		"probe directory": atomicPublicationProbeDirectoryName,
		"probe source":    atomicPublicationProbeSourceName,
		"probe target":    atomicPublicationProbeTargetName,
	} {
		if err := validateLeafName(name); err != nil {
			t.Fatalf("unsupported %s name is not a valid namespace leaf: %v", label, err)
		}
	}
	if atomicPublicationProbeDirectoryName == atomicPublicationProbeSourceName ||
		atomicPublicationProbeDirectoryName == atomicPublicationProbeTargetName ||
		atomicPublicationProbeSourceName == atomicPublicationProbeTargetName {
		t.Fatal("unsupported descriptor publication probe namespace aliases its own entries")
	}
	if len(atomicPublicationProbeBody) == 0 {
		t.Fatal("unsupported descriptor publication probe body is empty")
	}

	root := t.TempDir()
	sourcePath := filepath.Join(root, "publication-source")
	sourceBody := []byte("unsupported publication source must remain unchanged\n")
	if err := os.WriteFile(sourcePath, sourceBody, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.Lstat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	operations := []struct {
		name string
		call func() error
	}{
		{name: "require", call: requireAtomicNamespacePublication},
		{name: "preflight", call: func() error { return preflightAtomicNamespacePublication(directory) }},
		{name: "verify retained probe", call: func() error {
			return verifyAtomicNamespacePublicationProbe(directory, directory, trustedStat{})
		}},
		{name: "publish", call: func() error {
			return atomicPublishDescriptorNoReplaceAt(source, directory, atomicPublicationProbeTargetName)
		}},
	}
	for _, operation := range operations {
		if err := operation.call(); !errors.Is(err, errAtomicNamespacePublicationUnsupported) {
			t.Fatalf("unsupported %s did not return the fail-closed publication error: %v", operation.name, err)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(sourcePath) {
		t.Fatalf("unsupported publication contract mutated its namespace: %v", entries)
	}
	sourceAfterBody, readErr := os.ReadFile(sourcePath)
	sourceAfter, statErr := os.Lstat(sourcePath)
	if readErr != nil || statErr != nil || string(sourceAfterBody) != string(sourceBody) || !os.SameFile(sourceBefore, sourceAfter) {
		t.Fatalf("unsupported publication contract touched its source: body=%q read=%v stat=%v", sourceAfterBody, readErr, statErr)
	}
}

func TestUnsupportedPlatformFailsBeforeLedgerFilesystemInitialization(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if ledger != nil || !errors.Is(err, errAtomicNamespacePublicationUnsupported) {
		t.Fatalf("unsupported platform did not fail closed: ledger=%v err=%v", ledger, err)
	}
	if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsupported platform touched the ledger target before failing: %v", statErr)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("unsupported platform touched the ledger parent before failing: entries=%v err=%v", entries, readErr)
	}
}

func TestUnsupportedPlatformFailsBeforeExistingLedgerMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, "existing-marker")
	markerBody := []byte("existing unsupported ledger marker\n")
	if err := os.WriteFile(markerPath, markerBody, 0o600); err != nil {
		t.Fatal(err)
	}
	markerBefore, err := os.Lstat(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	ledger, err := OpenLedger(t.Context(), root)
	if ledger != nil || !errors.Is(err, errAtomicNamespacePublicationUnsupported) {
		t.Fatalf("unsupported platform did not fail closed before opening an existing ledger: ledger=%v err=%v", ledger, err)
	}
	entries, readErr := os.ReadDir(root)
	markerAfterBody, markerReadErr := os.ReadFile(markerPath)
	markerAfter, statErr := os.Lstat(markerPath)
	if readErr != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(markerPath) {
		t.Fatalf("unsupported platform mutated the existing ledger namespace: entries=%v err=%v", entries, readErr)
	}
	if markerReadErr != nil || statErr != nil || string(markerAfterBody) != string(markerBody) || !os.SameFile(markerBefore, markerAfter) {
		t.Fatalf("unsupported platform touched the existing ledger marker: body=%q read=%v stat=%v", markerAfterBody, markerReadErr, statErr)
	}
}
