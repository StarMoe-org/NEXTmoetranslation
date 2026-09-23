package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestExactAcquisitionReplaySourceUsesFullyValidatedPrivateRuntimeCopy(t *testing.T) {
	const content = "reviewed revision content\n"
	const pageID = 901
	const revisionID = 1901
	revisionTimestamp := time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC)
	contentSHA1 := sha1.Sum([]byte(content))
	contentSHA256 := sha256.Sum256([]byte(content))
	raw, err := json.Marshal(map[string]any{
		"batchcomplete": true,
		"query": map[string]any{"pages": []any{map[string]any{
			"pageid": pageID, "ns": 0, "title": "Reviewed index", "categories": []any{},
			"revisions": []any{map[string]any{
				"revid": revisionID, "timestamp": revisionTimestamp.Format(time.RFC3339Nano),
				"sha1": hex.EncodeToString(contentSHA1[:]),
				"slots": map[string]any{"main": map[string]any{
					"contentmodel": "wikitext", "contentformat": "text/x-wiki", "content": content,
				}},
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{
		"action": {"query"}, "cllimit": {"max"}, "format": {"json"}, "formatversion": {"2"},
		"maxlag": {"5"}, "prop": {"revisions|categories"}, "revids": {strconv.Itoa(revisionID)},
		"rvprop": {"ids|timestamp|sha1|content"}, "rvslots": {"main"},
	}
	capture, err := lyricssource.CaptureRecoveryHTTPResponse(
		model.LyricsSourceProviderSekaipedia, nil,
		lyricssource.RecoveryHTTPResponse{
			Action: "page", CanonicalRequestURL: "https://www.sekaipedia.org/w/api.php?" + query.Encode(),
			FetchedAt: revisionTimestamp.Add(time.Hour), Raw: raw,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(recoveryCommandTestRoot, "exact-replay-source-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ledgerRoot := filepath.Join(root, "ledger")
	ledger, err := lyricsacquisition.CreateLedger(t.Context(), ledgerRoot)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := ledger.Commit(t.Context(), exactReplayRecordInput(capture))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := readExactAcquisitionReplaySource(t.Context(), ledgerRoot, filepath.Join(root, "runtime-copy"), committed.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ReplayOnly || loaded.AcquisitionID != committed.AcquisitionID ||
		loaded.Request != committed.Request || loaded.FetchedAt != committed.FetchedAt ||
		loaded.RawResponseSHA256 != committed.RawResponseSHA256 ||
		loaded.Evidence.EvidenceID != committed.Evidence.EvidenceID ||
		loaded.Evidence.RawSHA256 != committed.Evidence.RawSHA256 ||
		loaded.EvidenceEnvelopeSHA256 != committed.EvidenceEnvelopeSHA256 ||
		!bytes.Equal(loaded.RawResponse, committed.RawResponse) ||
		!bytes.Equal(loaded.Evidence.Raw, committed.Evidence.Raw) ||
		!bytes.Equal(loaded.EvidenceEnvelope, committed.EvidenceEnvelope) {
		t.Fatalf("read-only exact replay source changed identity: committedId=%s loadedId=%s committedRawSha256=%s loadedRawSha256=%s",
			committed.AcquisitionID, loaded.AcquisitionID, committed.RawResponseSHA256, loaded.RawResponseSHA256)
	}
	if err := lyricssource.VerifySekaipediaRevisionContent(loaded.RawResponse, lyricssource.FixedIndex{
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: revisionTimestamp.Format(time.RFC3339Nano),
		SHA1: hex.EncodeToString(contentSHA1[:]), ContentSHA256: hex.EncodeToString(contentSHA256[:]),
	}); err != nil {
		t.Fatal(err)
	}

	blobPath := filepath.Join(ledgerRoot, "blobs", committed.RawResponseSHA256)
	if err := os.WriteFile(blobPath, append(append([]byte(nil), committed.RawResponse...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readExactAcquisitionReplaySource(t.Context(), ledgerRoot, filepath.Join(root, "corrupt-runtime-copy"), committed.AcquisitionID); err == nil {
		t.Fatal("exact replay source accepted changed blob bytes")
	}
}

func TestExactAcquisitionReplaySourceRejectsSelectorSQLiteManifestInconsistency(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, lyricsacquisition.Acquisition)
	}{
		{
			name: "latest selector conflicts with SQLite",
			mutate: func(t *testing.T, ledgerRoot string, _ lyricsacquisition.Acquisition) {
				selectorPath, _ := latestExactReplaySelector(t, ledgerRoot)
				body, err := os.ReadFile(selectorPath)
				if err != nil {
					t.Fatal(err)
				}
				var selector struct {
					SchemaVersion    int    `json:"schemaVersion"`
					Sequence         uint64 `json:"sequence"`
					Slot             string `json:"slot"`
					SHA256           string `json:"sha256"`
					ByteCount        int64  `json:"byteCount"`
					AcquisitionCount int64  `json:"acquisitionCount"`
				}
				if err := json.Unmarshal(body, &selector); err != nil {
					t.Fatal(err)
				}
				selector.SHA256 = strings.Repeat("0", 64)
				body, err = json.Marshal(selector)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(selectorPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "selector-bound SQLite conflicts with its digest",
			mutate: func(t *testing.T, ledgerRoot string, _ lyricsacquisition.Acquisition) {
				_, slot := latestExactReplaySelector(t, ledgerRoot)
				path := filepath.Join(ledgerRoot, slot)
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				body[len(body)-1] ^= 1
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest conflicts with selector-backed SQLite",
			mutate: func(t *testing.T, ledgerRoot string, acquisition lyricsacquisition.Acquisition) {
				path := filepath.Join(ledgerRoot, "manifests", string(acquisition.AcquisitionID)+".json")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := os.MkdirTemp(recoveryCommandTestRoot, "exact-replay-inconsistency-")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			ledgerRoot := filepath.Join(root, "ledger")
			ledger, err := lyricsacquisition.CreateLedger(t.Context(), ledgerRoot)
			if err != nil {
				t.Fatal(err)
			}
			committed, err := ledger.Commit(t.Context(), exactReplayFixtureRecord(t, time.Date(2026, 8, 2, 15, 58, 47, 64_046_000, time.UTC)))
			if err != nil {
				t.Fatal(err)
			}
			if err := ledger.Close(); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, ledgerRoot, committed)
			if _, err := readExactAcquisitionReplaySource(
				t.Context(), ledgerRoot, filepath.Join(root, "runtime-copy"), committed.AcquisitionID,
			); err == nil {
				t.Fatal("fully validated exact replay accepted selector/SQLite/manifest inconsistency")
			}
		})
	}
}

func latestExactReplaySelector(t *testing.T, ledgerRoot string) (string, string) {
	t.Helper()
	stateRoot := filepath.Join(ledgerRoot, ".metadata-state")
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	var (
		latestPath     string
		latestSlot     string
		latestSequence uint64
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(stateRoot, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var selector struct {
			Sequence uint64 `json:"sequence"`
			Slot     string `json:"slot"`
		}
		if err := json.Unmarshal(body, &selector); err != nil || selector.Sequence == 0 || selector.Slot == "" {
			t.Fatalf("decode exact replay fixture selector %q: sequence=%d slot=%q err=%v", entry.Name(), selector.Sequence, selector.Slot, err)
		}
		if selector.Sequence > latestSequence {
			latestPath = path
			latestSlot = selector.Slot
			latestSequence = selector.Sequence
		}
	}
	if latestPath == "" {
		t.Fatal("exact replay fixture has no selector")
	}
	return latestPath, latestSlot
}

func exactReplayFixtureRecord(t *testing.T, fetchedAt time.Time) lyricsacquisition.RecordInput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "lyricssource", "testdata", "sekaipedia-list-335193.json"))
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{
		"action": {"query"}, "cllimit": {"max"}, "format": {"json"}, "formatversion": {"2"},
		"maxlag": {"5"}, "prop": {"revisions|categories"}, "revids": {"335193"},
		"rvprop": {"ids|timestamp|sha1|content"}, "rvslots": {"main"},
	}
	capture, err := lyricssource.CaptureRecoveryHTTPResponse(
		model.LyricsSourceProviderSekaipedia, nil,
		lyricssource.RecoveryHTTPResponse{
			Action: "page", CanonicalRequestURL: "https://www.sekaipedia.org/w/api.php?" + query.Encode(),
			FetchedAt: fetchedAt, Raw: raw,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return exactReplayRecordInput(capture)
}

func TestExactAcquisitionReplaySourceCanInspectHistoricalLedgerReadOnly(t *testing.T) {
	ledgerRoot := os.Getenv("MOESEKAI_EXACT_REPLAY_TEST_LEDGER")
	acquisitionID := os.Getenv("MOESEKAI_EXACT_REPLAY_TEST_ACQUISITION_ID")
	if ledgerRoot == "" && acquisitionID == "" {
		t.Skip("historical exact replay source not requested")
	}
	if ledgerRoot == "" || acquisitionID == "" {
		t.Fatal("historical exact replay source requires both environment pins")
	}
	sourceBefore := snapshotLiveCanaryReplaySource(t, ledgerRoot)
	runtimeParent, err := os.MkdirTemp(recoveryCommandTestRoot, "historical-exact-replay-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeParent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeParent) })
	loaded, err := readExactAcquisitionReplaySource(
		t.Context(), ledgerRoot, filepath.Join(runtimeParent, "ledger-copy"),
		lyricsacquisition.AcquisitionID(acquisitionID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ReplayOnly || string(loaded.AcquisitionID) != acquisitionID ||
		!bytes.Equal(loaded.RawResponse, loaded.Evidence.Raw) {
		t.Fatalf("historical exact replay source mismatch: acquisitionId=%s rawSha256=%s evidenceSha256=%s",
			loaded.AcquisitionID, loaded.RawResponseSHA256, loaded.Evidence.RawSHA256)
	}
	if sourceAfter := snapshotLiveCanaryReplaySource(t, ledgerRoot); !reflect.DeepEqual(sourceBefore, sourceAfter) {
		t.Fatalf("historical exact replay source changed: before=%v after=%v", sourceBefore, sourceAfter)
	}
}

func exactReplayRecordInput(capture lyricssource.RecoveryCapture) lyricsacquisition.RecordInput {
	observed := make([]lyricsacquisition.ObservedRevision, len(capture.ObservedRevisions))
	for index, revision := range capture.ObservedRevisions {
		observed[index] = lyricsacquisition.ObservedRevision{
			Selector: revision.Selector, RevisionID: revision.RevisionID,
			Timestamp: revision.Timestamp, SHA1: revision.SHA1,
		}
	}
	return lyricsacquisition.RecordInput{
		Request: lyricsacquisition.Request{
			Provider: string(capture.Provider), CanonicalRequestIdentity: capture.CanonicalRequestURL,
			Kind: lyricsacquisition.RequestKind(capture.RequestKind), RevisionSelector: capture.RevisionSelector,
		},
		FetchedAt: capture.FetchedAt, RawResponse: append([]byte(nil), capture.RawResponse...),
		RawResponseSHA256: capture.RawResponseSHA256,
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: capture.Evidence.EvidenceID, Raw: append([]byte(nil), capture.Evidence.Raw...),
			RawSHA256: capture.Evidence.RawSHA256,
		},
		EvidenceEnvelope:       append([]byte(nil), capture.EvidenceEnvelope...),
		EvidenceEnvelopeSHA256: capture.EvidenceEnvelopeSHA256,
		ObservedRevisions:      observed,
	}
}
