package lyricsoutcomeartifact

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/model"
)

func TestArtifactCanonicalIdentityIsDeterministicAndContentFree(t *testing.T) {
	first := testArtifact(t)
	second := testArtifact(t)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical provider outcomes produced different artifacts")
	}
	body, err := MarshalCanonical(first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonical(body)
	if err != nil || !reflect.DeepEqual(decoded, first) {
		t.Fatalf("artifact round trip err=%v", err)
	}
	lower := strings.ToLower(string(body))
	for _, forbidden := range []string{"romaji", "romanization", "translation", "canonicalurl", "source text", "rawresponse", "path"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("content-free artifact contains forbidden token %q", forbidden)
		}
	}
}

func TestArtifactDecoderRejectsHostileJSONAndTamper(t *testing.T) {
	artifact := testArtifact(t)
	body, err := MarshalCanonical(artifact)
	if err != nil {
		t.Fatal(err)
	}
	depth := []byte(`{"x":` + strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1) + `}`)
	tampered := bytes.Replace(body, []byte(`"musicId":2`), []byte(`"musicId":3`), 1)
	for name, mutated := range map[string][]byte{
		"duplicate":       bytes.Replace(body, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1),
		"unknown":         bytes.Replace(body, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"message":"private"`), 1),
		"trailing":        append(append([]byte(nil), body...), []byte(`{}`)...),
		"invalid UTF-8":   append([]byte{0xff}, body...),
		"excessive depth": depth,
		"oversized":       bytes.Repeat([]byte{' '}, MaxArtifactBytes+1),
		"digest tamper":   tampered,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonical(mutated); err == nil {
				t.Fatal("hostile provider outcome artifact was accepted")
			}
		})
	}
}

func TestArtifactAcceptsClosedOriginalRendition(t *testing.T) {
	artifact := testArtifact(t)
	artifact.Candidate.RenditionKey = "full-original"
	artifact.Candidate.VersionReason = model.LyricsSourceVersionReasonUntaggedFullOnly
	artifact.ArtifactSHA256 = ""
	canonical, err := New(
		artifact.MusicID, artifact.Provider, artifact.Status, artifact.ReasonCode, artifact.Phase,
		artifact.Counts, artifact.ParserVersion, artifact.PolicyVersion, artifact.Candidate, artifact.Acquisitions,
	)
	if err != nil || canonical.Candidate == nil || canonical.Candidate.RenditionKey != "full-original" {
		t.Fatalf("closed original rendition artifact=%+v err=%v", canonical, err)
	}
}

func TestArtifactAcceptsClosedVocaloidGameOnlyRendition(t *testing.T) {
	artifact := testArtifact(t)
	artifact.Candidate.RenditionKey = "game-vocaloid"
	artifact.Candidate.VersionReason = model.LyricsSourceVersionReasonTaggedGameOnly
	artifact.ArtifactSHA256 = ""
	canonical, err := New(
		artifact.MusicID, artifact.Provider, artifact.Status, artifact.ReasonCode, artifact.Phase,
		artifact.Counts, artifact.ParserVersion, artifact.PolicyVersion, artifact.Candidate, artifact.Acquisitions,
	)
	if err != nil || canonical.Candidate == nil || canonical.Candidate.RenditionKey != "game-vocaloid" {
		t.Fatalf("closed Vocaloid Game-only rendition artifact=%+v err=%v", canonical, err)
	}
}

func TestArtifactRejectsOpenVersionAndRenditionLeakChannels(t *testing.T) {
	for name, mutate := range map[string]func(*Artifact){
		"parser":    func(artifact *Artifact) { artifact.ParserVersion = "private title or error" },
		"policy":    func(artifact *Artifact) { artifact.PolicyVersion = "file:///private/path" },
		"rendition": func(artifact *Artifact) { artifact.Candidate.RenditionKey = "raw-lyric-fragment" },
	} {
		t.Run(name, func(t *testing.T) {
			artifact := testArtifact(t)
			mutate(&artifact)
			if err := Validate(artifact); err == nil {
				t.Fatal("open compact identity field accepted arbitrary content")
			}
		})
	}
}

func TestArtifactPrivatePublicationFailsClosedAndRecoversCrashPair(t *testing.T) {
	parent := privateTempDir(t)
	directory := filepath.Join(parent, "outcomes")
	if err := CreatePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	artifact := testArtifact(t)
	path, err := PublishCreateExclusive(directory, artifact)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("published artifact mode=%v", info.Mode())
	}
	if _, err := PublishCreateExclusive(directory, artifact); !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("duplicate publication error=%v", err)
	}
	opened, err := Open(path)
	if err != nil || opened.ArtifactSHA256 != artifact.ArtifactSHA256 {
		t.Fatalf("opened artifact=%+v err=%v", opened, err)
	}

	crashDirectory := filepath.Join(parent, "crash")
	if err := CreatePrivateDirectory(crashDirectory); err != nil {
		t.Fatal(err)
	}
	name, _ := FileName(artifact)
	body, _ := MarshalCanonical(artifact)
	tempPath := filepath.Join(crashDirectory, "."+name+".tmp")
	finalPath := filepath.Join(crashDirectory, name)
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(tempPath, finalPath); err != nil {
		t.Fatal(err)
	}
	recovered, err := PublishCreateExclusive(crashDirectory, artifact)
	if err != nil || recovered != finalPath {
		t.Fatalf("crash-pair recovery path=%q err=%v", recovered, err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash-pair stage remains: %v", err)
	}
}

func TestArtifactOpenRejectsTamperSymlinkAndWrongMode(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, string){
		"tamper": func(t *testing.T, path string) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body[len(body)/2] ^= 1
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"wrong mode": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			parent := privateTempDir(t)
			directory := filepath.Join(parent, "outcomes")
			if err := CreatePrivateDirectory(directory); err != nil {
				t.Fatal(err)
			}
			path, err := PublishCreateExclusive(directory, testArtifact(t))
			if err != nil {
				t.Fatal(err)
			}
			mutate(t, path)
			if _, err := Open(path); err == nil {
				t.Fatal("invalid provider outcome artifact file was accepted")
			}
		})
	}

	parent := privateTempDir(t)
	target := filepath.Join(parent, "target.json")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("provider outcome Open accepted a symlink")
	}
}

func testArtifact(t *testing.T) Artifact {
	t.Helper()
	sha := strings.Repeat("a", 64)
	artifact, err := New(
		2, model.LyricsSourceProviderSekaipedia, lyricsprovideroutcome.StatusCandidate,
		lyricsprovideroutcome.ReasonCandidate, lyricsprovideroutcome.PhaseFinalize,
		lyricsprovideroutcome.Counts{Acquisitions: 2, Targets: 1, Evaluated: 1, Candidates: 1},
		"sekaipedia-list-song-parser-v1", "lyrics-provider-policy/v1",
		&CandidateIdentity{
			PageID: 398, RevisionID: 330574, SHA1: strings.Repeat("b", 40), RawSHA256: sha,
			RenditionKey: "full-sekai", VersionReason: model.LyricsSourceVersionReasonTaggedFullAndGame, LineCount: 60,
		},
		[]AcquisitionRef{
			{AcquisitionID: strings.Repeat("1", 64), EvidenceID: "authority:sekaipedia:song-index:335193:" + sha, SHA256: sha, EnvelopeSHA256: strings.Repeat("2", 64)},
			{AcquisitionID: strings.Repeat("3", 64), EvidenceID: "revision:sekaipedia:398:330574:" + sha, SHA256: sha, EnvelopeSHA256: strings.Repeat("4", 64)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
