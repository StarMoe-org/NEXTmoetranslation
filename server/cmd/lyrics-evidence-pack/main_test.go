package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func commandEvidence(t *testing.T) lyricssource.IndexEvidence {
	t.Helper()
	const pageID, revisionID = 51, 52
	raw := []byte("command exact evidence")
	rawDigest := sha256.Sum256(raw)
	rawSHA256 := hex.EncodeToString(rawDigest[:])
	contentSHA1 := sha1.Sum(raw)
	fetchedAt := "2026-08-01T00:00:00Z"
	canonical := url.URL{Scheme: "https", Host: "vocaloid.fandom.com", Path: "/wiki/Command_Evidence"}
	query := canonical.Query()
	query.Set("oldid", fmt.Sprintf("%d", revisionID))
	canonical.RawQuery = query.Encode()
	item := lyricssource.IndexEvidence{
		EvidenceID: lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			model.LyricsSourceProviderVocaloidFandom, fmt.Sprintf("fetch:vocaloid-fandom:%d", pageID), fetchedAt, rawSHA256,
		),
		SHA256: rawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: pageID, RevisionID: revisionID, MediaWikiSHA1: hex.EncodeToString(contentSHA1[:]),
		Title: "Command Evidence", CanonicalURL: canonical.String(), Categories: []string{"Lyrics"}, FetchedAt: fetchedAt,
		Raw: raw, RawSHA256: rawSHA256,
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestRunBuildsPrivateEvidencePackOffline(t *testing.T) {
	item := commandEvidence(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ledgerRoot := filepath.Join(root, "ledger")
	ledger, err := lyricsacquisition.CreateLedger(context.Background(), ledgerRoot)
	if err != nil {
		t.Fatal(err)
	}
	envelopeBody, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	envelopeDigest := sha256.Sum256(envelopeBody)
	committed, err := ledger.Commit(context.Background(), lyricsacquisition.RecordInput{
		Request: lyricsacquisition.Request{
			Provider: string(item.Provider), CanonicalRequestIdentity: item.CanonicalURL,
			Kind: lyricsacquisition.RequestKindRevision, RevisionSelector: fmt.Sprintf("oldid:%d", item.RevisionID),
		},
		FetchedAt: item.FetchedAt, RawResponse: append([]byte(nil), item.Raw...), RawResponseSHA256: item.RawSHA256,
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: item.EvidenceID, Raw: append([]byte(nil), item.Raw...), RawSHA256: item.RawSHA256,
		},
		EvidenceEnvelope: append([]byte(nil), envelopeBody...), EvidenceEnvelopeSHA256: hex.EncodeToString(envelopeDigest[:]),
		ObservedRevisions: []lyricsacquisition.ObservedRevision{},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ledger.ReplayByAcquisitionID(context.Background(), committed.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := lyricsevidencepack.EvidenceRefFromAcquisition(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	selection := lyricsevidencepack.Selection{
		SchemaVersion: lyricsevidencepack.SchemaVersionV1,
		Evidence:      []lyricscontract.EvidenceRef{ref},
	}
	selectionBody, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	selectionPath := filepath.Join(root, "selection.json")
	if err := os.WriteFile(selectionPath, selectionBody, 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(root, "pack")
	var output bytes.Buffer
	args := []string{"-ledger-root", ledgerRoot, "-selection", selectionPath, "-output-dir", outputDir}
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "with 1 items in 1 shards") {
		t.Fatalf("command output=%q", output.String())
	}
	if strings.Contains(output.String(), item.Title) || strings.Contains(output.String(), string(item.Raw)) {
		t.Fatalf("command output leaked evidence title or raw payload: %q", output.String())
	}
	resolver, err := lyricsevidencepack.OpenResolver(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.HydrateID(item.EvidenceID); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &bytes.Buffer{}); err == nil {
		t.Fatal("command overwrote an existing complete evidence pack")
	}
}

func TestRunRejectsNonPrivateSelectionMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"evidence":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-ledger-root", t.TempDir(), "-selection", path, "-output-dir", filepath.Join(t.TempDir(), "pack")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "mode-0600") {
		t.Fatalf("selection mode error=%v", err)
	}
}
