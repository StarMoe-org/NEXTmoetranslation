package lyricsacquisition

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"moesekai/server/internal/lyricssource"

	_ "modernc.org/sqlite"
)

const (
	metadataRestartHelperEnvironment   = "MOESEKAI_LYRICS_ACQUISITION_METADATA_RESTART_HELPER"
	metadataRestartAcquisitionIDEnvVar = "MOESEKAI_LYRICS_ACQUISITION_METADATA_RESTART_ID"
)

func testValidatedResponse(t *testing.T, _ string, selector, fetchedAt string, raw, evidence []byte) validatedProviderResponse {
	t.Helper()
	revisionID, err := strconv.Atoi(strings.TrimPrefix(selector, "oldid:"))
	if err != nil || revisionID <= 0 {
		t.Fatalf("invalid test selector %q", selector)
	}
	title := "Ledger Fixture " + strconv.Itoa(revisionID)
	canonicalURL := "https://vocaloid.fandom.com/wiki/Ledger_Fixture_" + strconv.Itoa(revisionID) + "?oldid=" + strconv.Itoa(revisionID)
	rawSHA1 := sha1.Sum(evidence)
	rawSHA256 := sha256Hex(evidence)
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		lyricssource.ProviderVocaloidFandom, "fetch:vocaloid-fandom:12", fetchedAt, rawSHA256,
	)
	envelope := lyricssource.IndexEvidence{
		EvidenceID: evidenceID, SHA256: rawSHA256,
		Kind: lyricssource.IndexEvidenceKindMediaWikiRevision, Provider: lyricssource.ProviderVocaloidFandom,
		Origin: lyricssource.OriginVocaloidFandom, PageID: 12, RevisionID: revisionID,
		MediaWikiSHA1: hex.EncodeToString(rawSHA1[:]), Title: title, CanonicalURL: canonicalURL,
		Categories: []string{}, FetchedAt: fetchedAt, Raw: append([]byte(nil), evidence...), RawSHA256: rawSHA256,
	}
	envelopeBody, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(envelope); err != nil {
		t.Fatalf("invalid test evidence envelope: %v", err)
	}
	return validatedProviderResponse{
		request: acquisitionRequest{
			provider:                 "vocaloid_fandom",
			canonicalRequestIdentity: canonicalURL,
			kind:                     acquisitionRequestRevision,
			revisionSelector:         selector,
		},
		fetchedAt:         fetchedAt,
		rawResponse:       append([]byte(nil), raw...),
		rawResponseSHA256: sha256Hex(raw),
		evidence: evidenceProjection{
			evidenceID: evidenceID, raw: append([]byte(nil), evidence...), rawSHA256: rawSHA256,
		},
		envelope: evidenceEnvelope{raw: envelopeBody, sha256: sha256Hex(envelopeBody)},
		observedRevisions: []observedRevision{
			{selector: selector, revisionID: int64(revisionID), timestamp: "2026-07-31T11:59:59.123456789Z", sha1: strings.Repeat("b", 40)},
			{selector: "older:33", revisionID: 33, timestamp: "2026-07-30T11:59:59Z", sha1: strings.Repeat("c", 40)},
		},
	}
}

func openTestSpool(t *testing.T, root string) *spool {
	t.Helper()
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("close spool: %v", err)
		}
	})
	return opened
}

type diskMetadataState struct {
	requests     int64
	acquisitions int64
	counters     metadataCounters
	acquisition  acquisitionMetadata
}

type diskMetadataSnapshot struct {
	state        diskMetadataState
	acquisitions []acquisitionMetadata
}

type expectedMetadataSnapshot struct {
	counters     metadataCounters
	acquisitions []acquisitionMetadata
}

func appendExpectedMetadataSnapshot(
	t *testing.T,
	current expectedMetadataSnapshot,
	response validatedProviderResponse,
) (expectedMetadataSnapshot, string) {
	t.Helper()
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range current.acquisitions {
		if existing.acquisitionID == acquisitionID || existing.requestKey == key {
			t.Fatalf("sequential crash fixture reused acquisition or request identity %s", acquisitionID)
		}
	}
	next := expectedMetadataSnapshot{
		counters:     current.counters,
		acquisitions: append([]acquisitionMetadata(nil), current.acquisitions...),
	}
	next.acquisitions = append(next.acquisitions, metadataFromManifest(manifest, acquisitionID, key, len(manifestBody)))
	sort.Slice(next.acquisitions, func(left, right int) bool {
		return next.acquisitions[left].acquisitionID < next.acquisitions[right].acquisitionID
	})
	next.counters.requestCount++
	next.counters.acquisitionCount++
	next.counters.rawBytes += int64(len(response.rawResponse))
	next.counters.evidenceBytes += int64(len(response.evidence.raw))
	next.counters.envelopeBytes += int64(len(response.envelope.raw))
	next.counters.manifestBytes += int64(len(manifestBody))
	return next, acquisitionID
}

func metadataAcquisitionIDs(rows []acquisitionMetadata) []string {
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = row.acquisitionID
	}
	return ids
}

func diskMetadataSnapshotMatches(actual diskMetadataSnapshot, expected expectedMetadataSnapshot) bool {
	return actual.state.requests == expected.counters.requestCount &&
		actual.state.acquisitions == expected.counters.acquisitionCount &&
		actual.state.counters == expected.counters &&
		reflect.DeepEqual(actual.acquisitions, expected.acquisitions)
}

func assertExactDiskMetadataSnapshot(
	t *testing.T,
	label string,
	actual diskMetadataSnapshot,
	expected expectedMetadataSnapshot,
) {
	t.Helper()
	if diskMetadataSnapshotMatches(actual, expected) {
		return
	}
	t.Fatalf("%s metadata snapshot rows=%d/%d counters=%+v IDs=%v, want rows=%d counters=%+v IDs=%v",
		label, actual.state.requests, actual.state.acquisitions, actual.state.counters,
		metadataAcquisitionIDs(actual.acquisitions), len(expected.acquisitions), expected.counters,
		metadataAcquisitionIDs(expected.acquisitions))
}

func activeMetadataPathForSpool(t *testing.T, opened *spool) string {
	t.Helper()
	if opened == nil || opened.root == nil || opened.metadataBinding == nil {
		t.Fatal("open spool metadata binding is required")
	}
	_, _, name := opened.metadataBinding.snapshot()
	return filepath.Join(opened.root.path, name)
}

func activeDiskMetadataPath(t *testing.T, root string) string {
	t.Helper()
	stateDirectory := filepath.Join(root, metadataStateDirectory)
	entries, err := os.ReadDir(stateDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(root, metadataFileName)
	}
	if err != nil {
		t.Fatal(err)
	}
	var latest *metadataSelector
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".stage") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(stateDirectory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		selector, err := decodeMetadataSelector(body, entry.Name())
		if err != nil {
			if metadataSelectorSlotNamePattern.MatchString(entry.Name()) {
				continue
			}
			t.Fatal(err)
		}
		if latest == nil || selector.Sequence > latest.Sequence {
			copy := selector
			latest = &copy
		}
	}
	if latest == nil {
		return filepath.Join(root, metadataFileName)
	}
	return filepath.Join(root, latest.Slot)
}

func readActiveDiskMetadataState(t *testing.T, root, acquisitionID string) diskMetadataState {
	t.Helper()
	return readDiskMetadataState(t, activeDiskMetadataPath(t, root), acquisitionID)
}

func readDiskMetadataState(t *testing.T, path, acquisitionID string) diskMetadataState {
	t.Helper()
	snapshot := readDiskMetadataSnapshot(t, path)
	if acquisitionID == "" {
		return snapshot.state
	}
	for _, metadata := range snapshot.acquisitions {
		if metadata.acquisitionID == acquisitionID {
			snapshot.state.acquisition = metadata
			return snapshot.state
		}
	}
	t.Fatalf("read exact on-disk acquisition metadata row %s: %v", acquisitionID, sql.ErrNoRows)
	return diskMetadataState{}
}

func readDiskMetadataSnapshot(t *testing.T, path string) diskMetadataSnapshot {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var snapshot diskMetadataSnapshot
	if err := database.QueryRow(`SELECT (SELECT COUNT(*) FROM requests),(SELECT COUNT(*) FROM acquisitions)`).
		Scan(&snapshot.state.requests, &snapshot.state.acquisitions); err != nil {
		t.Fatalf("read on-disk acquisition metadata row counts: %v", err)
	}
	if err := database.QueryRow(`SELECT request_count,acquisition_count,raw_bytes,evidence_bytes,envelope_bytes,manifest_bytes FROM spool_counters WHERE singleton=1`).
		Scan(&snapshot.state.counters.requestCount, &snapshot.state.counters.acquisitionCount, &snapshot.state.counters.rawBytes,
			&snapshot.state.counters.evidenceBytes, &snapshot.state.counters.envelopeBytes, &snapshot.state.counters.manifestBytes); err != nil {
		t.Fatalf("read on-disk acquisition metadata counters: %v", err)
	}
	rows, err := database.Query(`SELECT ` + acquisitionMetadataColumns + ` FROM acquisitions ORDER BY acquisition_id`)
	if err != nil {
		t.Fatalf("read exact on-disk acquisition metadata rows: %v", err)
	}
	for rows.Next() {
		metadata, scanErr := scanAcquisitionMetadata(rows)
		if scanErr != nil {
			_ = rows.Close()
			t.Fatalf("scan exact on-disk acquisition metadata row: %v", scanErr)
		}
		snapshot.acquisitions = append(snapshot.acquisitions, metadata)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close exact on-disk acquisition metadata rows: %v", err)
	}
	var integrity string
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("on-disk acquisition metadata integrity=%q err=%v", integrity, err)
	}
	foreignRows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("read on-disk acquisition metadata foreign keys: %v", err)
	}
	if foreignRows.Next() {
		_ = foreignRows.Close()
		t.Fatal("on-disk acquisition metadata foreign-key check failed")
	}
	if err := foreignRows.Close(); err != nil {
		t.Fatalf("close on-disk acquisition metadata foreign-key check: %v", err)
	}
	return snapshot
}

func TestManifestDecoderRejectsDuplicateUnknownAndTrailingFields(t *testing.T) {
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte(`{"query":{"page":34}}`), []byte(`{"evidence":34}`))
	_, body, _, _, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string][]byte{
		"duplicate": bytes.Replace(body, []byte(`{"schemaVersion":2,`), []byte(`{"schemaVersion":2,"schemaVersion":2,`), 1),
		"unknown":   bytes.Replace(body, []byte(`{"schemaVersion":2,`), []byte(`{"schemaVersion":2,"unknown":true,`), 1),
		"trailing":  append(append([]byte(nil), body...), []byte(` {}`)...),
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeManifest(mutated); err == nil {
				t.Fatalf("manifest decoder accepted %s field/data mutation", name)
			}
		})
	}
}

func TestSpoolFailsClosedOnBlobTamper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("provider-response"), []byte("evidence-projection"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(root, blobsDirectory, committed.rawResponseSHA256)
	if err := os.WriteFile(blobPath, []byte("provider-responze"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tampered blob replay error=%v, want explicit corruption", err)
	}
}

func TestSpoolFailsClosedOnCanonicalEvidenceEnvelopeTamper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("envelope raw"), []byte("envelope evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, blobsDirectory, committed.envelope.sha256)
	body := append([]byte(nil), committed.envelope.raw...)
	body[len(body)/2] ^= 1
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tampered canonical envelope replay error=%v, want explicit corruption", err)
	}
}

func TestOfflineReplaySelectsOnlyTheExplicitAcquisitionID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	firstResponse := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("same raw"), []byte("same evidence"))
	secondResponse := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456791Z", []byte("same raw"), []byte("same evidence"))
	first, err := opened.commit(t.Context(), firstResponse)
	if err != nil {
		t.Fatal(err)
	}
	second, err := opened.commit(t.Context(), secondResponse)
	if err != nil {
		t.Fatal(err)
	}
	if first.acquisitionID == second.acquisitionID {
		t.Fatal("independent reacquisitions received the same acquisition ID")
	}
	firstReplay, err := opened.replayByAcquisitionID(t.Context(), first.acquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	secondReplay, err := opened.replayByAcquisitionID(t.Context(), second.acquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	if firstReplay.fetchedAt != firstResponse.fetchedAt || secondReplay.fetchedAt != secondResponse.fetchedAt ||
		firstReplay.acquisitionID != first.acquisitionID || secondReplay.acquisitionID != second.acquisitionID {
		t.Fatalf("explicit replays selected the wrong acquisition identities")
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), strings.Repeat("f", 64)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing exact acquisition replay error=%v", err)
	}
}

func TestProcessRestartReplaysExactAcquisitionIDFromDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("restart raw"), []byte("restart evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessRestartMetadataReplayHelper$", "-test.v")
	command.Env = append(os.Environ(),
		metadataRestartHelperEnvironment+"="+root,
		metadataRestartAcquisitionIDEnvVar+"="+committed.acquisitionID,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("metadata restart helper failed: %v\n%s", err, output)
	}
}

func TestProcessRestartMetadataReplayHelper(t *testing.T) {
	root := os.Getenv(metadataRestartHelperEnvironment)
	if root == "" {
		t.Skip("metadata restart helper")
	}
	acquisitionID := os.Getenv(metadataRestartAcquisitionIDEnvVar)
	state := readActiveDiskMetadataState(t, root, acquisitionID)
	if state.requests != 1 || state.acquisitions != 1 || state.counters.requestCount != 1 || state.counters.acquisitionCount != 1 {
		t.Fatalf("process restart observed non-durable metadata: %+v", state)
	}
	opened, err := openExistingSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	replayed, err := opened.replayByAcquisitionID(t.Context(), acquisitionID)
	if err != nil || !replayed.replayOnly || replayed.acquisitionID != acquisitionID {
		t.Fatalf("process restart exact-ID replay=%+v err=%v", replayed, err)
	}
}

func TestFailedMetadataPersistenceDoesNotReportCommitSuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	persistenceFailure := errors.New("simulated metadata snapshot persistence failure")
	opened.hooks.metadataBoundary = func(boundary metadataPersistenceBoundary) error {
		if boundary == metadataBoundaryBeforeSnapshotWrite {
			return persistenceFailure
		}
		return nil
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("failed persistence raw"), []byte("failed persistence evidence"))
	_, _, acquisitionID, _, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := opened.commit(t.Context(), response)
	if !errors.Is(err, persistenceFailure) {
		t.Fatalf("commit persistence error=%v", err)
	}
	if committed.acquisitionID != "" {
		t.Fatalf("failed metadata persistence returned a successful acquisition: %+v", committed)
	}
	state := readActiveDiskMetadataState(t, root, "")
	if state.requests != 0 || state.acquisitions != 0 || state.counters != (metadataCounters{}) {
		t.Fatalf("failed metadata persistence changed the published disk snapshot: %+v", state)
	}
	retainedStage, err := os.Lstat(filepath.Join(root, metadataSnapshotTempName))
	if err != nil || retainedStage.Mode().Perm() != 0o600 || !retainedStage.Mode().IsRegular() {
		t.Fatalf("failed metadata persistence did not retain one bounded reusable snapshot stage: info=%v err=%v", retainedStage, err)
	}
	pending, err := os.ReadDir(filepath.Join(root, pendingDirectory))
	wantPending := []string{
		acquisitionID + ".envelope.tmp", acquisitionID + ".evidence.tmp", acquisitionID + ".json",
		acquisitionID + ".manifest.tmp", acquisitionID + ".marker.tmp", acquisitionID + ".raw.tmp",
	}
	if err != nil || !reflect.DeepEqual(directoryEntryNames(pending), wantPending) {
		t.Fatalf("failed metadata persistence did not retain exact bounded recovery sources: entries=%v err=%v", directoryEntryNames(pending), err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), acquisitionID); !errors.Is(err, persistenceFailure) {
		t.Fatalf("failed metadata persistence remained operational for replay: %v", err)
	}
	opened.hooks.metadataBoundary = nil
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openExistingSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.replayByAcquisitionID(t.Context(), acquisitionID); err != nil {
		t.Fatalf("recovery after failed persistence: %v", err)
	}
}

func TestMetadataSnapshotZeroByteWriteFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, metadataFileName)
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	opened.hooks.metadataSnapshotWriteAt = func(*os.File, []byte, int64) (int, error) { return 0, nil }
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("short write raw"), []byte("short write evidence"))
	if _, err := opened.commit(t.Context(), response); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-byte metadata snapshot write error=%v", err)
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("zero-byte metadata snapshot write changed published metadata: err=%v", err)
	}
	stageInfo, err := os.Lstat(filepath.Join(root, metadataSnapshotTempName))
	if err != nil || !stageInfo.Mode().IsRegular() || stageInfo.Size() != 0 {
		t.Fatalf("zero-byte metadata snapshot write did not retain its bounded zero-byte stage: info=%v err=%v", stageInfo, err)
	}
	opened.hooks.metadataSnapshotWriteAt = nil
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRetainsAndReusesOnlyARecognizedMetadataSnapshotStage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	stagePath := filepath.Join(root, metadataSnapshotTempName)
	partial := []byte("partial uncommitted snapshot")
	if err := os.WriteFile(stagePath, partial, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := openExistingSpool(t.Context(), root)
	if err != nil {
		t.Fatalf("open with retained metadata snapshot stage: %v", err)
	}
	state := readActiveDiskMetadataState(t, root, "")
	if state.requests != 0 || state.acquisitions != 0 || state.counters != (metadataCounters{}) {
		t.Fatalf("retained stage changed the last published metadata snapshot: %+v", state)
	}
	afterBody, err := os.ReadFile(stagePath)
	after, statErr := os.Lstat(stagePath)
	if err != nil || statErr != nil || !bytes.Equal(afterBody, partial) || !os.SameFile(before, after) {
		t.Fatalf("open deleted or replaced the retained stage: body=%q read=%v stat=%v", afterBody, err, statErr)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("reused stage raw"), []byte("reused stage evidence"))
	if _, err := reopened.commit(t.Context(), response); err != nil {
		t.Fatalf("reuse retained metadata snapshot stage: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	active, err := os.Lstat(filepath.Join(root, metadataFileName))
	retained, retainedErr := os.Lstat(stagePath)
	if err != nil || retainedErr != nil || os.SameFile(active, retained) {
		t.Fatalf("metadata publication did not retain exactly two distinct snapshot slots: active=%v retained=%v err=%v/%v", active, retained, err, retainedErr)
	}
}

func TestSameRawBytesCanRepresentDifferentAcquisitions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	raw := []byte("shared provider bytes")
	evidence := []byte("shared evidence bytes")
	firstResponse := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", raw, evidence)
	secondResponse := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456791Z", raw, evidence)
	first, err := opened.commit(t.Context(), firstResponse)
	if err != nil {
		t.Fatal(err)
	}
	second, err := opened.commit(t.Context(), secondResponse)
	if err != nil {
		t.Fatal(err)
	}
	if first.acquisitionID == second.acquisitionID || first.rawResponseSHA256 != second.rawResponseSHA256 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	var acquisitionCount int
	if err := opened.database.QueryRow(`SELECT COUNT(*) FROM acquisitions`).Scan(&acquisitionCount); err != nil || acquisitionCount != 2 {
		t.Fatalf("acquisition count=%d err=%v", acquisitionCount, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, blobsDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("content-addressed blob count=%d, want shared raw/projection plus two canonical envelopes", len(entries))
	}
	quarantine, err := os.ReadDir(filepath.Join(root, quarantineDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantine) != 0 {
		t.Fatalf("descriptor publication unexpectedly moved retained sources into quarantine: entries=%v", directoryEntryNames(quarantine))
	}
}

func TestOfflineReplayPreservesIdentityAndOnlySetsOperationalFlag(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("raw replay"), []byte("evidence replay"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	var changesBefore int64
	var fetchedAtBefore, evidenceIDBefore string
	if err := opened.database.QueryRow(`SELECT total_changes()`).Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}
	if err := opened.database.QueryRow(`SELECT fetched_at,evidence_id FROM acquisitions WHERE acquisition_id=?`, committed.acquisitionID).
		Scan(&fetchedAtBefore, &evidenceIDBefore); err != nil {
		t.Fatal(err)
	}
	replayed, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID)
	if err != nil {
		t.Fatalf("exact replay error=%v", err)
	}
	var changesAfter int64
	var fetchedAtAfter, evidenceIDAfter string
	if err := opened.database.QueryRow(`SELECT total_changes()`).Scan(&changesAfter); err != nil {
		t.Fatal(err)
	}
	if err := opened.database.QueryRow(`SELECT fetched_at,evidence_id FROM acquisitions WHERE acquisition_id=?`, committed.acquisitionID).
		Scan(&fetchedAtAfter, &evidenceIDAfter); err != nil {
		t.Fatal(err)
	}
	if !replayed.replayOnly || committed.replayOnly || !sameAcquisitionIdentity(committed, replayed) ||
		replayed.fetchedAt != response.fetchedAt || replayed.evidence.evidenceID != response.evidence.evidenceID ||
		!reflect.DeepEqual(replayed.observedRevisions, response.observedRevisions) || changesAfter != changesBefore ||
		fetchedAtAfter != fetchedAtBefore || evidenceIDAfter != evidenceIDBefore {
		t.Fatalf("committed=%+v replayed=%+v changes=%d/%d fetchedAt=%q/%q evidenceID=%q/%q",
			committed, replayed, changesBefore, changesAfter, fetchedAtBefore, fetchedAtAfter, evidenceIDBefore, evidenceIDAfter)
	}
}

func TestSpoolRejectsModeAndSymlinkSubstitution(t *testing.T) {
	newCommitted := func(t *testing.T) (string, *spool, validatedProviderResponse, acquiredProviderResponse) {
		t.Helper()
		root := filepath.Join(t.TempDir(), "spool")
		opened := openTestSpool(t, root)
		response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
			"2026-07-31T12:00:00.123456789Z", []byte("mode raw"), []byte("mode evidence"))
		committed, err := opened.commit(t.Context(), response)
		if err != nil {
			t.Fatal(err)
		}
		return root, opened, response, committed
	}

	t.Run("mode", func(t *testing.T) {
		root, opened, _, committed := newCommitted(t)
		if err := os.Chmod(filepath.Join(root, blobsDirectory, committed.rawResponseSHA256), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil {
			t.Fatal("replay accepted a non-0600 blob")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root, opened, _, committed := newCommitted(t)
		manifestPath := filepath.Join(root, manifestsDirectory, committed.acquisitionID+".json")
		if err := os.Remove(manifestPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", blobsDirectory, committed.rawResponseSHA256), manifestPath); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil {
			t.Fatal("replay accepted a symlink manifest")
		}
	})

	t.Run("root symlink", func(t *testing.T) {
		parent := t.TempDir()
		realRoot := filepath.Join(parent, "real")
		if err := os.Mkdir(realRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		linkRoot := filepath.Join(parent, "link")
		if err := os.Symlink(realRoot, linkRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := openSpool(t.Context(), linkRoot); err == nil {
			t.Fatal("open accepted a symlink spool root")
		}
	})
}

func TestCommitIsIdempotentAndDoesNotPreallocateCapacity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("idempotent raw"), []byte("idempotent evidence"))
	first, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	artifactPaths := map[string]string{
		"raw":      filepath.Join(root, blobsDirectory, first.rawResponseSHA256),
		"evidence": filepath.Join(root, blobsDirectory, first.evidence.rawSHA256),
		"envelope": filepath.Join(root, blobsDirectory, first.envelope.sha256),
		"manifest": filepath.Join(root, manifestsDirectory, first.acquisitionID+".json"),
	}
	beforeArtifacts := make(map[string]os.FileInfo, len(artifactPaths))
	for role, path := range artifactPaths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		beforeArtifacts[role] = info
	}
	second, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	if first.acquisitionID != second.acquisitionID {
		t.Fatalf("idempotent commit first=%+v second=%+v", first, second)
	}
	for role, path := range artifactPaths {
		after, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(beforeArtifacts[role], after) {
			t.Fatalf("idempotent commit replaced the %s artifact inode", role)
		}
	}
	var acquisitions, requests int
	if err := opened.database.QueryRow(`SELECT (SELECT COUNT(*) FROM acquisitions),(SELECT COUNT(*) FROM requests)`).Scan(&acquisitions, &requests); err != nil {
		t.Fatal(err)
	}
	if acquisitions != 1 || requests != 1 {
		t.Fatalf("idempotent metadata acquisitions=%d requests=%d", acquisitions, requests)
	}
	metadataInfo, err := os.Lstat(activeMetadataPathForSpool(t, opened))
	if err != nil {
		t.Fatal(err)
	}
	if metadataInfo.Size() >= 1<<20 {
		t.Fatalf("metadata index appears preallocated: %d bytes", metadataInfo.Size())
	}
	pending, err := os.ReadDir(filepath.Join(root, pendingDirectory))
	if err != nil || len(pending) != 6 {
		t.Fatalf("bounded retained publication sources=%d err=%v", len(pending), err)
	}
	for _, entry := range pending {
		if !strings.HasPrefix(entry.Name(), first.acquisitionID) {
			t.Fatalf("idempotent commit added an unrelated retained source %q", entry.Name())
		}
	}
}

func TestCommittedMetadataSnapshotIsImmediatelyDurableOnDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	metadataPath := filepath.Join(root, metadataFileName)
	beforeBody, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("durable metadata raw"), []byte("durable metadata evidence"))
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	if committed.acquisitionID != acquisitionID {
		t.Fatalf("committed acquisition ID=%q want %q", committed.acquisitionID, acquisitionID)
	}
	activePath := activeMetadataPathForSpool(t, opened)
	afterBody, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if activePath == metadataPath || bytes.Equal(beforeBody, afterBody) || os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("metadata commit did not select a complete descriptor-pinned standby snapshot")
	}
	state := readDiskMetadataState(t, activePath, acquisitionID)
	wantMetadata := metadataFromManifest(manifest, acquisitionID, key, len(manifestBody))
	wantCounters := metadataCounters{
		requestCount: 1, acquisitionCount: 1,
		rawBytes: int64(len(response.rawResponse)), evidenceBytes: int64(len(response.evidence.raw)),
		envelopeBytes: int64(len(response.envelope.raw)), manifestBytes: int64(len(manifestBody)),
	}
	if state.requests != 1 || state.acquisitions != 1 || state.counters != wantCounters || !reflect.DeepEqual(state.acquisition, wantMetadata) {
		t.Fatalf("on-disk metadata state=%+v want counters=%+v acquisition=%+v", state, wantCounters, wantMetadata)
	}
	retainedBody, err := os.ReadFile(metadataPath)
	retainedInfo, retainedErr := os.Lstat(metadataPath)
	if err != nil || retainedErr != nil || !bytes.Equal(retainedBody, beforeBody) || !os.SameFile(beforeInfo, retainedInfo) {
		t.Fatalf("committed metadata publication did not preserve the exact prior snapshot in its bounded slot: read=%v stat=%v", err, retainedErr)
	}
}

func TestLogicalEnvelopeCapacityExceeds64MiBWithoutLargeArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	const logicalRowCount = 17
	logicalEnvelopeBytes := int64(logicalRowCount * maxEvidenceEnvelopeBytes)
	if logicalEnvelopeBytes <= 64<<20 || logicalEnvelopeBytes >= maxAggregateEnvelope {
		t.Fatalf("test logical envelope bytes=%d do not exercise the intended capacity range", logicalEnvelopeBytes)
	}

	tx, err := opened.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	requestKey := strings.Repeat("f", 64)
	if _, err := tx.Exec(`INSERT INTO requests(request_key,provider,canonical_request_identity,request_kind,revision_selector) VALUES (?,?,?,?,?)`,
		requestKey, "capacity_fixture", "https://example.invalid/logical-envelope-capacity", "revision", "oldid:1"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < logicalRowCount; index++ {
		acquisitionID := fmt.Sprintf("%064x", index+1)
		if _, err := tx.Exec(`INSERT INTO acquisitions(`+acquisitionMetadataColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			acquisitionID, requestKey, "capacity_fixture", "https://example.invalid/logical-envelope-capacity",
			"revision", "oldid:1", "2026-07-31T12:00:00Z", strings.Repeat("a", 64), 1,
			strings.Repeat("b", 64), 1, fmt.Sprintf("capacity-evidence-%d", index), strings.Repeat("c", 64),
			maxEvidenceEnvelopeBytes, acquisitionID, 1, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`UPDATE spool_counters SET request_count=?,acquisition_count=?,raw_bytes=?,evidence_bytes=?,envelope_bytes=?,manifest_bytes=? WHERE singleton=1`,
		1, logicalRowCount, logicalRowCount, logicalRowCount, logicalEnvelopeBytes, logicalRowCount); err != nil {
		t.Fatalf("schema rejected a logical envelope total above 64 MiB: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := opened.validateMetadataCounters(t.Context()); err != nil {
		t.Fatalf("exact logical counters above 64 MiB were rejected: %v", err)
	}

	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("capacity raw"), []byte("capacity evidence"))
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.ensureMetadataCapacity(t.Context(), manifest, acquisitionID, key, len(manifestBody)); err != nil {
		t.Fatalf("application capacity rejected a logical envelope total above 64 MiB: %v", err)
	}
	for _, directory := range []string{pendingDirectory, blobsDirectory, manifestsDirectory} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("logical capacity fixture allocated %d content artifacts in %s", len(entries), directory)
		}
	}
	var physicalBytes int64
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		physicalBytes += info.Size()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if physicalBytes >= 1<<20 {
		t.Fatalf("logical capacity fixture unexpectedly allocated %d physical bytes", physicalBytes)
	}
	if _, err := opened.database.Exec(`UPDATE spool_counters SET envelope_bytes=? WHERE singleton=1`, maxAggregateEnvelope+1); err == nil {
		t.Fatal("SQLite schema accepted a logical envelope total beyond 32 GiB")
	}
}

func TestEnvelopeCapacityIsEnforcedBeforeAnyPublication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	if _, err := opened.database.Exec(`UPDATE spool_counters SET envelope_bytes=? WHERE singleton=1`, maxAggregateEnvelope); err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("capacity raw"), []byte("capacity evidence"))
	if _, err := opened.commit(t.Context(), response); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("envelope capacity error=%v", err)
	}
	for _, directory := range []string{pendingDirectory, blobsDirectory, manifestsDirectory} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("capacity rejection published %d entries in %s", len(entries), directory)
		}
	}
}

func TestHistoricalZeroRowMetadataReconcilesDurableManifestsWithoutPendingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("historical reconcile raw"), []byte("historical reconcile evidence"))
	manifest, manifestBody, acquisitionID, _, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.publishBlob(acquisitionID, "raw", response.rawResponse, response.rawResponseSHA256, maxRawResponseBytes); err != nil {
		t.Fatal(err)
	}
	if err := opened.publishBlob(acquisitionID, "evidence", response.evidence.raw, response.evidence.rawSHA256, maxEvidenceProjectionBytes); err != nil {
		t.Fatal(err)
	}
	if err := opened.publishBlob(acquisitionID, "envelope", response.envelope.raw, response.envelope.sha256, maxEvidenceEnvelopeBytes); err != nil {
		t.Fatal(err)
	}
	if err := opened.publishManifest(acquisitionID, manifestBody); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(root, pendingDirectory)
	pending, err := os.ReadDir(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range pending {
		if err := os.Remove(filepath.Join(pendingPath, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := os.ReadDir(pendingPath); err != nil || len(entries) != 0 {
		t.Fatalf("historical fixture pending state=%v err=%v", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, metadataStateDirectory)); err != nil || len(entries) != 0 {
		t.Fatalf("historical fixture unexpectedly has metadata selectors=%v err=%v", entries, err)
	}
	before := readDiskMetadataState(t, filepath.Join(root, metadataFileName), "")
	if before.requests != 0 || before.acquisitions != 0 || before.counters != (metadataCounters{}) {
		t.Fatalf("historical zero-row metadata fixture is not empty: %+v", before)
	}
	reconciled, err := openExistingSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciled.Close()
	if _, err := reconciled.replayByAcquisitionID(t.Context(), acquisitionID); err != nil {
		t.Fatalf("historical zero-row manifest reconciliation failed: %v", err)
	}
	after := readActiveDiskMetadataState(t, root, acquisitionID)
	want := metadataFromManifest(manifest, acquisitionID, mustRequestKey(t, manifest), len(manifestBody))
	if after.acquisitions != 1 || after.requests != 1 || !reflect.DeepEqual(after.acquisition, want) {
		t.Fatalf("historical manifest reconciliation state=%+v want=%+v", after, want)
	}
}

func mustRequestKey(t *testing.T, manifest acquisitionManifest) string {
	t.Helper()
	key, err := requestKey(manifest.response(nil, nil, nil).request)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestManifestOnlyRecoveryPersistsMetadataAfterReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	crash := errors.New("simulated crash after manifest publication")
	opened, err := openSpoolWithHooks(t.Context(), root, spoolHooks{afterStage: func(stage string) error {
		if stage == commitStageManifest {
			return crash
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("recovery raw"), []byte("recovery evidence"))
	_, _, acquisitionID, _, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.commit(t.Context(), response); !errors.Is(err, crash) {
		t.Fatalf("commit error=%v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, metadataFileName)
	beforeRecovery := readDiskMetadataState(t, metadataPath, "")
	if beforeRecovery.requests != 0 || beforeRecovery.acquisitions != 0 || beforeRecovery.counters != (metadataCounters{}) {
		t.Fatalf("manifest-only crash unexpectedly persisted metadata before recovery: %+v", beforeRecovery)
	}
	recovered, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := recovered.replayByAcquisitionID(t.Context(), acquisitionID)
	if err != nil || !replayed.replayOnly || replayed.fetchedAt != response.fetchedAt {
		t.Fatalf("recovered replay identity mismatch: err=%v", err)
	}
	afterRecovery := readActiveDiskMetadataState(t, root, acquisitionID)
	if afterRecovery.requests != 1 || afterRecovery.acquisitions != 1 || afterRecovery.counters.requestCount != 1 || afterRecovery.counters.acquisitionCount != 1 {
		t.Fatalf("manifest reconciliation was not persisted immediately: %+v", afterRecovery)
	}
	pending, err := os.ReadDir(filepath.Join(root, pendingDirectory))
	if err != nil || len(pending) != 6 {
		t.Fatalf("retained manifest-recovery sources=%d err=%v", len(pending), err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed, err = reopened.replayByAcquisitionID(t.Context(), acquisitionID)
	if err != nil || !replayed.replayOnly || replayed.fetchedAt != response.fetchedAt {
		t.Fatalf("second reopen lost manifest-reconciled metadata: replay=%+v err=%v", replayed, err)
	}
}

func TestCrashOrderInvariants(t *testing.T) {
	stages := []struct {
		stage      string
		wantReplay bool
	}{
		{stage: commitStageMarker, wantReplay: false},
		{stage: commitStageRaw, wantReplay: false},
		{stage: commitStageEvidence, wantReplay: false},
		{stage: commitStageEnvelope, wantReplay: false},
		{stage: commitStageManifest, wantReplay: true},
		{stage: commitStageMetadata, wantReplay: true},
	}
	for _, test := range stages {
		t.Run(test.stage, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "spool")
			crash := fmt.Errorf("simulated crash at %s", test.stage)
			opened, err := openSpoolWithHooks(t.Context(), root, spoolHooks{afterStage: func(stage string) error {
				if stage == test.stage {
					return crash
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
				"2026-07-31T12:00:00.123456789Z", []byte("crash raw "+test.stage), []byte("crash evidence "+test.stage))
			_, _, acquisitionID, _, err := buildManifest(response)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := opened.commit(t.Context(), response); !errors.Is(err, crash) {
				t.Fatalf("commit error=%v", err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			recovered := openTestSpool(t, root)
			_, replayErr := recovered.replayByAcquisitionID(t.Context(), acquisitionID)
			if test.wantReplay {
				if replayErr != nil {
					t.Fatalf("durable stage exact replay error=%v", replayErr)
				}
			} else if !errors.Is(replayErr, sql.ErrNoRows) {
				t.Fatalf("incomplete stage exact replay error=%v", replayErr)
			}
			pending, err := os.ReadDir(filepath.Join(root, pendingDirectory))
			if err != nil || len(pending) == 0 || len(pending) > 6 {
				t.Fatalf("bounded retained crash sources=%d err=%v", len(pending), err)
			}
			for _, entry := range pending {
				if !strings.HasPrefix(entry.Name(), acquisitionID) {
					t.Fatalf("crash recovery retained unrelated source %q", entry.Name())
				}
			}
		})
	}
}

func TestPendingRecoveryRetainsUnexpectedOwnedStageWithoutMovingItsInode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	crash := errors.New("simulated crash after marker")
	opened, err := openSpoolWithHooks(t.Context(), root, spoolHooks{afterStage: func(stage string) error {
		if stage == commitStageMarker {
			return crash
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	shared := []byte("shared raw and evidence")
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", shared, shared)
	_, _, acquisitionID, _, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.commit(t.Context(), response); !errors.Is(err, crash) {
		t.Fatalf("commit error=%v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	unexpectedPath := filepath.Join(root, pendingDirectory, acquisitionID+".evidence.tmp")
	unexpectedBody := []byte("not owned by pending recovery")
	if err := os.WriteFile(unexpectedPath, unexpectedBody, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(unexpectedPath)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := openSpool(t.Context(), root)
	if err != nil {
		t.Fatalf("recovery could not retain a recognized unexpected stage: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	afterBody, err := os.ReadFile(unexpectedPath)
	after, statErr := os.Lstat(unexpectedPath)
	if err != nil || statErr != nil || !bytes.Equal(afterBody, unexpectedBody) || sha256Hex(afterBody) != sha256Hex(unexpectedBody) || !os.SameFile(before, after) {
		t.Fatalf("unexpected staging inode moved or changed during bounded retention: body=%q read=%v stat=%v", afterBody, err, statErr)
	}
	if _, err := os.Lstat(filepath.Join(root, quarantineDirectory, filepath.Base(unexpectedPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bounded retention created an unexpected quarantine pathname: %v", err)
	}
}

func TestMarkerRemovalRefusesReplacementInode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("marker raw"), []byte("marker evidence"))
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		t.Fatal(err)
	}
	marker, markerBody, err := buildPendingMarker(manifest, acquisitionID, key, len(manifestBody))
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := opened.publishMarker(acquisitionID, markerBody)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, pendingDirectory, acquisitionID+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	marker.RequestKey = strings.Repeat("d", 64)
	replacementBody, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacementBody, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.removeMarker(acquisitionID, markerBody, originalInfo); err == nil {
		t.Fatal("marker removal deleted a replacement pathname")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(path)
	if err != nil || !bytes.Equal(body, replacementBody) || sha256Hex(body) != sha256Hex(replacementBody) || !os.SameFile(replacementInfo, afterInfo) {
		t.Fatalf("replacement marker changed or was deleted: body=%q err=%v", body, err)
	}
}

func TestCommitCanProceedWhileBoundedIncompleteSourcesRemainRetained(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	crash := errors.New("simulated interrupted commit")
	opened, err := openSpoolWithHooks(t.Context(), root, spoolHooks{afterStage: func(stage string) error {
		if stage == commitStageRaw {
			return crash
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	first := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("first raw"), []byte("first evidence"))
	_, _, firstID, _, err := buildManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.commit(t.Context(), first); !errors.Is(err, crash) {
		t.Fatalf("first commit error=%v", err)
	}
	opened.hooks = spoolHooks{}
	second := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=35", "oldid:35",
		"2026-07-31T12:00:01.123456789Z", []byte("second raw"), []byte("second evidence"))
	committed, err := opened.commit(t.Context(), second)
	if err != nil {
		t.Fatalf("second commit could not proceed with bounded retained sources: %v", err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), firstID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("incomplete first acquisition became replayable: %v", err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err != nil {
		t.Fatalf("complete second acquisition is not replayable: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, pendingDirectory))
	if err != nil || len(entries) == 0 || len(entries) > maxRetainedPendingEntries {
		t.Fatalf("bounded retained entries=%v err=%v", entries, err)
	}
}

func TestSpoolRejectsPinnedDirectorySubstitution(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "https://www.sekaipedia.org/w/api.php?action=query&revids=34", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("directory raw"), []byte("directory evidence"))
	committed, err := opened.commit(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, blobsDirectory)
	renamed := filepath.Join(root, blobsDirectory+"-original")
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(renamed, original); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.replayByAcquisitionID(t.Context(), committed.acquisitionID); err == nil {
		t.Fatal("replay accepted substitution of a pinned spool directory")
	}
}

func TestOpenRejectsUnexpectedSQLiteSchemaObject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", "file:"+activeDiskMetadataPath(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE injected(value TEXT) STRICT`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openSpool(context.Background(), root); err == nil {
		t.Fatal("open accepted an unexpected SQLite schema object")
	}
}

func TestOpenRejectsAnOrphanRequestEvenWhenItsCounterMatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("request graph raw"), []byte("request graph evidence"))
	if _, err := opened.commit(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	second := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456791Z", []byte("request graph raw two"), []byte("request graph evidence two"))
	if _, err := opened.commit(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := acquisitionRequest{
		provider: "vocaloid_fandom", canonicalRequestIdentity: "https://vocaloid.fandom.com/api.php?action=query&generator=search&gsrsearch=orphan",
		kind: acquisitionRequestSearch,
	}
	key, err := requestKey(orphan)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", "file:"+activeDiskMetadataPath(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO requests(request_key,provider,canonical_request_identity,request_kind,revision_selector) VALUES (?,?,?,?,?)`,
		key, orphan.provider, orphan.canonicalRequestIdentity, orphan.kind, orphan.revisionSelector); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE spool_counters SET request_count=request_count+1,acquisition_count=acquisition_count+1 WHERE singleton=1`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openSpool(context.Background(), root); err == nil {
		t.Fatal("open accepted an unreferenced request row with a matching counter")
	}
}

func TestOpenRejectsAnEnvelopeCounterThatDoesNotBindExactManifestRows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	opened := openTestSpool(t, root)
	response := testValidatedResponse(t, "ignored", "oldid:34",
		"2026-07-31T12:00:00.123456789Z", []byte("envelope counter raw"), []byte("envelope counter evidence"))
	if _, err := opened.commit(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", "file:"+activeDiskMetadataPath(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE spool_counters SET envelope_bytes=envelope_bytes+1 WHERE singleton=1`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openSpool(context.Background(), root); err == nil {
		t.Fatal("open accepted an envelope byte counter that drifted from exact acquisition rows")
	}
}
