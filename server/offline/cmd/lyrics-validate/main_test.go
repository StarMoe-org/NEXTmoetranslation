package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"
)

func validManifest(t *testing.T) lyricsstaging.Manifest {
	t.Helper()
	lines := []model.LyricsSourceExtractedLine{{Japanese: "初音歌う", Segments: []model.LyricsSourceSegment{{
		Text: "初音歌う", PerformerIDs: []string{"歌唱者-21"}, Ruby: []model.LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"},
			{Text: "歌", Reading: "うた"}, {Text: "う"}},
	}}, TrailingPerformerIDs: []string{"歌唱者-21"}}}
	draft := lyricsstaging.Draft{MusicID: 10, JapaneseTitle: "合成試験曲", CatalogFingerprint: strings.Repeat("b", 64),
		TargetMusicID: 10, AssociationMusicIDs: []int{}, Source: lyricsstaging.FixedSource{PageID: 12, RevisionID: 34,
			SHA1: strings.Repeat("a", 40), PageTitle: "合成試験曲",
			CanonicalURL: "https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34",
			Categories:   []string{"Lyrics", "Songs"}, FetchedAt: "2026-07-30T12:34:57Z",
			RawWikitextByteCount: 20, RawWikitextSHA256: strings.Repeat("c", 64)},
		SelectedVersion:      model.LyricsSourceVersion{Kind: "sekai", Label: "Project SEKAI Version"},
		Performers:           []model.LyricsSourcePerformer{{PerformerID: "歌唱者-21", Name: "初音ミク", Color: "#33CCBB"}},
		RubyGeneratorVersion: "kagome-ipadic-v1", Lines: lines, ExtractedLinesSHA256: model.LyricsSourceExtractedLinesSHA256(lines)}
	identity := model.LyricsSourceFixedIdentity{Provider: model.LyricsSourceProviderVocaloidFandom,
		Origin: model.LyricsSourceOriginVocaloidFandom, PageID: draft.Source.PageID, RevisionID: draft.Source.RevisionID,
		SHA1: draft.Source.SHA1, Title: draft.Source.PageTitle, CanonicalURL: draft.Source.CanonicalURL,
		FetchedAt: draft.Source.FetchedAt, Categories: draft.Source.Categories, Section: "page", RenditionKey: "full-sekai",
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{EvidenceID: "preflight/10/12/34", SHA256: strings.Repeat("e", 64)}}}
	component := model.LyricsSourceComponentRef{RenditionKey: identity.RenditionKey}
	draft.Document = model.LyricsSourceDocument{SchemaVersion: model.LyricsSourceDocumentSchemaVersion,
		ReasonCode: model.LyricsSourceVersionReasonUntaggedFullOnly, FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
		Provenance: model.LyricsSourceComponentProvenance{FullText: component, PerformerSegmentation: &component,
			Ruby: &component, VersionEvidence: component},
		Full: model.NewLyricsSourceFullFromLegacy(draft.SelectedVersion, draft.Performers, draft.RubyGeneratorVersion, draft.Lines)}
	documentBody, _ := json.Marshal(draft.Document)
	documentDigest := sha256.Sum256(documentBody)
	draft.DocumentSHA256 = hex.EncodeToString(documentDigest[:])
	artifact := lyricsstaging.Artifact{Identity: identity, RawWikitextByteCount: draft.Source.RawWikitextByteCount,
		RawWikitextSHA256: draft.Source.RawWikitextSHA256}
	artifactBody, _ := json.Marshal(artifact)
	artifactDigest := sha256.Sum256(artifactBody)
	artifact.ArtifactSHA256 = hex.EncodeToString(artifactDigest[:])
	draft.Artifacts = []lyricsstaging.Artifact{artifact}
	draft.DraftSHA256 = strings.Repeat("d", 64)
	// Build a validated digest through the public decoder path by first using a
	// temporary manifest generated from a matching preflight report.
	candidate := lyricsstaging.CandidateIdentity{
		Provider: identity.Provider, Origin: identity.Origin, PageID: identity.PageID, RevisionID: identity.RevisionID,
		SHA1: identity.SHA1, Title: identity.Title, CanonicalURL: identity.CanonicalURL, Categories: identity.Categories,
		Section: identity.Section, RenditionKey: identity.RenditionKey,
		VersionReason:     model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, identity.IndexEvidenceRefs...),
	}
	report := lyricsstaging.PreflightReport{SchemaVersion: 1, GeneratedAt: "1970-01-01T00:02:03Z", CatalogSchemaVersion: 18,
		CatalogCount: 1, Summary: lyricsstaging.PreflightSummary{UniqueComplete: 1}, CatalogReview: []lyricsstaging.PreflightItem{},
		GameSizeEvidence: []lyricsstaging.PreflightItem{}, UniqueComplete: []lyricsstaging.PreflightItem{{MusicID: 10,
			JapaneseTitle: "合成試験曲", CatalogFingerprint: strings.Repeat("b", 64), TargetMusicID: 10,
			AssociationMusicIDs: []int{}, Candidate: &candidate, LineCount: 1, SearchAttempts: 1, FetchAttempts: 1}},
		Ambiguous: []lyricsstaging.PreflightItem{}, Missing: []lyricsstaging.PreflightItem{}, Incomplete: []lyricsstaging.PreflightItem{}, Error: []lyricsstaging.PreflightItem{}}
	reportDigest := sha256.Sum256([]byte("preflight"))
	// NewManifest also recalculates and validates DraftSHA256, so first encode a
	// draft with the same deterministic payload through a local helper manifest.
	draftBody := []byte(`{"placeholder":true}`)
	_ = draftBody
	manifest, err := lyricsstaging.NewManifest(report, hex.EncodeToString(reportDigest[:]), []lyricsstaging.Draft{withDraftDigest(t, draft)})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func withDraftDigest(t *testing.T, draft lyricsstaging.Draft) lyricsstaging.Draft {
	t.Helper()
	// Marshal through a minimal private-shaped clone so the digest calculation
	// exactly matches the package's documented empty-digest convention.
	draft.DraftSHA256 = ""
	body, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	draft.DraftSHA256 = hex.EncodeToString(digest[:])
	return draft
}

func TestRunValidatesManifestAndReportsDigest(t *testing.T) {
	manifest := validManifest(t)
	body, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "staging.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"-input", path}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), manifest.BatchSHA256) || !strings.Contains(output.String(), "1 staged") {
		t.Fatalf("output=%q", output.String())
	}
}

func TestLoadManifestRejectsPathReplacementAfterOpen(t *testing.T) {
	manifest := validManifest(t)
	body, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "staging.json")
	replacementPath := filepath.Join(directory, "replacement.json")
	movedPath := filepath.Join(directory, "opened.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = loadManifestWithOpener(path, func(openPath string) (manifestInput, error) {
		opened, err := os.Open(openPath)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(openPath, movedPath); err != nil {
			opened.Close()
			return nil, err
		}
		if err := os.Rename(replacementPath, openPath); err != nil {
			opened.Close()
			return nil, err
		}
		return opened, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed between inspection and open") {
		t.Fatalf("path replacement error = %v", err)
	}
}

type mutatingManifestInput struct {
	file         *os.File
	mutateOffset int64
	readBytes    int64
	mutated      bool
	mutationErr  error
	mutate       func() error
}

func (file *mutatingManifestInput) Read(buffer []byte) (int, error) {
	read, err := file.file.Read(buffer)
	before := file.readBytes
	file.readBytes += int64(read)
	if !file.mutated && before <= file.mutateOffset && file.readBytes > file.mutateOffset {
		file.mutated = true
		file.mutationErr = file.mutate()
	}
	return read, err
}

func (file *mutatingManifestInput) Seek(offset int64, whence int) (int64, error) {
	return file.file.Seek(offset, whence)
}

func (file *mutatingManifestInput) Stat() (os.FileInfo, error) {
	return file.file.Stat()
}

func (file *mutatingManifestInput) Close() error {
	return file.file.Close()
}

func TestLoadManifestRejectsSameInodeEqualLengthMutationDuringRead(t *testing.T) {
	manifest := validManifest(t)
	body, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 2 || body[1] != '\n' {
		t.Fatalf("unexpected canonical manifest prefix %q", body[:2])
	}
	path := filepath.Join(t.TempDir(), "staging.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	inspectedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var mutatingInput *mutatingManifestInput
	_, err = loadManifestWithOpener(path, func(openPath string) (manifestInput, error) {
		opened, err := os.Open(openPath)
		if err != nil {
			return nil, err
		}
		mutatingInput = &mutatingManifestInput{
			file:         opened,
			mutateOffset: 1,
			mutate: func() error {
				writer, err := os.OpenFile(openPath, os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				if _, err := writer.WriteAt([]byte{' '}, 1); err != nil {
					writer.Close()
					return err
				}
				if err := writer.Sync(); err != nil {
					writer.Close()
					return err
				}
				if err := writer.Close(); err != nil {
					return err
				}
				return os.Chtimes(openPath, inspectedInfo.ModTime(), inspectedInfo.ModTime())
			},
		}
		return mutatingInput, nil
	})
	if mutatingInput == nil || !mutatingInput.mutated {
		t.Fatal("opener seam did not perform the in-place mutation")
	}
	if mutatingInput.mutationErr != nil {
		t.Fatalf("mutate opened manifest: %v", mutatingInput.mutationErr)
	}
	if err == nil || !strings.Contains(err.Error(), "contents changed while being read") {
		t.Fatalf("equal-length same-inode mutation error = %v", err)
	}

	mutatedBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutatedBody) != len(body) {
		t.Fatalf("mutation changed size from %d to %d", len(body), len(mutatedBody))
	}
	if _, err := lyricsstaging.DecodeManifest(mutatedBody); err != nil {
		t.Fatalf("mutated bytes should remain a canonically valid manifest: %v", err)
	}
	mutatedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(inspectedInfo, mutatedInfo) {
		t.Fatal("mutation unexpectedly replaced the manifest inode")
	}
}

func TestLoadManifestRejectsTamperedRubyAndUnknownFields(t *testing.T) {
	manifest := validManifest(t)
	body, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("ruby drift", func(t *testing.T) {
		tampered := bytes.Replace(body, []byte(`"text": "歌"`), []byte(`"text": "踊"`), 1)
		path := filepath.Join(t.TempDir(), "ruby.json")
		if err := os.WriteFile(path, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadManifest(path); err == nil {
			t.Fatal("validator accepted tampered ruby spans")
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		tampered := append([]byte{}, body[:len(body)-2]...)
		tampered = append(tampered, []byte(",\n  \"unknown\": true\n}\n")...)
		path := filepath.Join(t.TempDir(), "unknown.json")
		if err := os.WriteFile(path, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadManifest(path); err == nil {
			t.Fatal("validator accepted an unknown manifest field")
		}
	})
}
