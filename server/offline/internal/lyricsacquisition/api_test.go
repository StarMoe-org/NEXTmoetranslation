package lyricsacquisition

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func exportedTestRecord(response validatedProviderResponse) RecordInput {
	observed := make([]ObservedRevision, len(response.observedRevisions))
	for index, revision := range response.observedRevisions {
		observed[index] = ObservedRevision{
			Selector: revision.selector, RevisionID: revision.revisionID,
			Timestamp: revision.timestamp, SHA1: revision.sha1,
		}
	}
	return RecordInput{
		Request: Request{
			Provider: response.request.provider, CanonicalRequestIdentity: response.request.canonicalRequestIdentity,
			Kind: RequestKind(response.request.kind), RevisionSelector: response.request.revisionSelector,
		},
		FetchedAt:   response.fetchedAt,
		RawResponse: append([]byte(nil), response.rawResponse...), RawResponseSHA256: response.rawResponseSHA256,
		Evidence: EvidenceProjection{
			EvidenceID: response.evidence.evidenceID, Raw: append([]byte(nil), response.evidence.raw...),
			RawSHA256: response.evidence.rawSHA256,
		},
		EvidenceEnvelope: append([]byte(nil), response.envelope.raw...), EvidenceEnvelopeSHA256: response.envelope.sha256,
		ObservedRevisions: observed,
	}
}

func TestOpenLedgerRequiresAnExistingRootWithoutCreatingIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-ledger")
	if _, err := OpenLedger(t.Context(), root); err == nil {
		t.Fatal("OpenLedger created a missing root")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenLedger changed missing root: %v", err)
	}
	created, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateLedgerRejectsAnIndirectSymlinkAncestorWithoutCreatingItsTarget(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	nested := filepath.Join(realParent, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(linkedParent, "nested", "ledger")
	if _, err := CreateLedger(t.Context(), root); err == nil {
		t.Fatal("CreateLedger accepted a symlink in the destination ancestor chain")
	}
	if _, err := os.Lstat(filepath.Join(nested, "ledger")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected ledger creation changed the symlink target: %v", err)
	}
}

func TestOpenLedgerRejectsIncompleteExistingRootWithoutInitializingIt(t *testing.T) {
	for _, test := range []struct {
		name              string
		createDirectories bool
	}{
		{name: "empty root"},
		{name: "missing metadata", createDirectories: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "incomplete-ledger")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.createDirectories {
				for _, name := range []string{blobsDirectory, manifestsDirectory, pendingDirectory} {
					if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenLedger(t.Context(), root); err == nil {
				t.Fatal("OpenLedger initialized an incomplete existing root")
			}
			after, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(directoryEntryNames(before), directoryEntryNames(after)) {
				t.Fatalf("OpenLedger changed incomplete root entries: before=%v after=%v", directoryEntryNames(before), directoryEntryNames(after))
			}
			for _, name := range []string{metadataFileName, ledgerLockName} {
				if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("OpenLedger created %s: %v", name, err)
				}
			}
		})
	}
}

func directoryEntryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func TestLedgerAllowsOnlyOneOpenHandlePerProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if second, err := OpenLedger(t.Context(), root); err == nil {
		_ = second.Close()
		t.Fatal("same-process OpenLedger acquired an already-owned ledger")
	} else if !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("same-process ledger ownership error=%v", err)
	}
}

func TestExportedLedgerAPIReplaysOnlyExplicitAcquisitionID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	firstInput := exportedTestRecord(testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("api raw"), []byte("api evidence")))
	secondInput := exportedTestRecord(testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456791Z", []byte("api raw"), []byte("api evidence")))
	first, err := ledger.Commit(t.Context(), firstInput)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Commit(t.Context(), secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if first.AcquisitionID == second.AcquisitionID {
		t.Fatal("independent acquisitions have the same ID")
	}
	firstReplay, err := ledger.ReplayByAcquisitionID(t.Context(), first.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	secondReplay, err := ledger.ReplayByAcquisitionID(t.Context(), second.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	if !firstReplay.ReplayOnly || !secondReplay.ReplayOnly || firstReplay.FetchedAt != firstInput.FetchedAt ||
		secondReplay.FetchedAt != secondInput.FetchedAt || firstReplay.AcquisitionID != first.AcquisitionID ||
		secondReplay.AcquisitionID != second.AcquisitionID {
		t.Fatal("exact-ID API selected a latest or different acquisition")
	}
	if _, err := ledger.ReplayByAcquisitionID(t.Context(), AcquisitionID("not-an-id")); err == nil {
		t.Fatal("exact-ID API accepted an invalid acquisition ID")
	}
}

func TestExportedLedgerAPIIsIdempotentAndDefensivelyClones(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	ledger, err := CreateLedger(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	input := exportedTestRecord(testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("clone raw"), []byte("clone evidence")))
	first, err := ledger.Commit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Commit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.AcquisitionID != second.AcquisitionID || !reflect.DeepEqual(first, second) {
		t.Fatal("identical exported commits are not idempotent")
	}
	first.RawResponse[0] ^= 0xff
	first.Evidence.Raw[0] ^= 0xff
	first.EvidenceEnvelope[0] ^= 0xff
	replayed, err := ledger.ReplayByAcquisitionID(t.Context(), second.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayed.RawResponse, input.RawResponse) || !bytes.Equal(replayed.Evidence.Raw, input.Evidence.Raw) ||
		!bytes.Equal(replayed.EvidenceEnvelope, input.EvidenceEnvelope) {
		t.Fatal("caller mutation changed retained acquisition bytes")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateLedger(t.Context(), root); err == nil {
		t.Fatal("CreateLedger reopened an existing root")
	}
	if _, err := ledger.ReplayByAcquisitionID(t.Context(), second.AcquisitionID); !errors.Is(err, errSpoolClosed) {
		t.Fatalf("closed ledger replay error=%v", err)
	}
}
