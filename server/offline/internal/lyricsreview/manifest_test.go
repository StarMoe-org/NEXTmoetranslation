package lyricsreview

import (
	"bytes"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

func TestManifestResolverBindsReviewedVersionsAndExactRevision(t *testing.T) {
	const (
		planSHA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		snapshotSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		contentSHA    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		evidenceSHA   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		envelopeSHA   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		artifactSHA   = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		acquisitionID = "1111111111111111111111111111111111111111111111111111111111111111"
	)
	decision := Decision{
		MusicID: 27,
		DeclaredVersions: VersionDeclaration{
			Full: true, Game: true, AlternateVocal: true,
		},
		ScreenshotEvidence: []ScreenshotEvidence{
			{ImageIndex: 1, SHA256: strings.Repeat("2", 64), SizeBytes: 100},
			{ImageIndex: 2, SHA256: strings.Repeat("3", 64), SizeBytes: 200},
			{ImageIndex: 3, SHA256: strings.Repeat("4", 64), SizeBytes: 300},
		},
		ExactRevision: RevisionBinding{
			Provider: model.LyricsSourceProviderSekaipedia,
			PageID:   390, RevisionID: 328683, Title: "Just Be Friends",
			SHA1: strings.Repeat("5", 40), ContentSHA256: contentSHA,
			AcquisitionID:  acquisitionID,
			EvidenceID:     "revision:sekaipedia:390:328683:identity",
			EvidenceSHA256: evidenceSHA, EnvelopeSHA256: envelopeSHA,
		},
		ProviderOutcome: OutcomeBinding{
			Provider:  model.LyricsSourceProviderSekaipedia,
			OutcomeID: "outcome:sekaipedia:27:identity", ArtifactSHA256: artifactSHA,
		},
	}
	manifest, err := NewManifest(Binding{
		OriginalNumbersSHA256:  strings.Repeat("6", 64),
		ExportedWorkbookSHA256: strings.Repeat("7", 64),
		ImagesManifestSHA256:   strings.Repeat("8", 64),
		OCRSimilaritySHA256:    strings.Repeat("9", 64),
		PlanID:                 "review-test-plan", PlanSHA256: planSHA,
		SourceSnapshotSHA256: snapshotSHA,
	}, []Decision{decision})
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("romaji")) ||
		bytes.Contains(bytes.ToLower(body), []byte("romanization")) {
		t.Fatalf("review manifest leaked a forbidden phonetic rendition field: %s", body)
	}
	decoded, err := DecodeCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(decoded, "review-test-plan", planSHA, snapshotSHA)
	if err != nil {
		t.Fatal(err)
	}
	outcome := OutcomeObservation{
		Provider:  model.LyricsSourceProviderSekaipedia,
		OutcomeID: "outcome:sekaipedia:27:identity", ArtifactSHA256: artifactSHA,
		Acquisitions: []AcquisitionObservation{{
			AcquisitionID: acquisitionID,
			EvidenceID:    "revision:sekaipedia:390:328683:identity",
			SHA256:        evidenceSHA, EnvelopeSHA256: envelopeSHA,
		}},
		Candidate: &CandidateObservation{
			PageID: 390, RevisionID: 328683, SHA1: strings.Repeat("5", 40), ContentSHA256: contentSHA,
		},
	}
	result := ResultObservation{
		MusicID: 27, State: StateComplete, HasFull: true, HasGame: true, AlternateCount: 1,
	}
	if err := resolver.ValidateResult(result, []OutcomeObservation{outcome}); err != nil {
		t.Fatal(err)
	}
	outcome.Candidate.ContentSHA256 = strings.Repeat("0", 64)
	if err := resolver.ValidateResult(result, []OutcomeObservation{outcome}); err == nil {
		t.Fatal("review resolver accepted exact revision content drift")
	}
	outcome.Candidate.ContentSHA256 = contentSHA
	result.HasFull = false
	result.State = StateGameOnly
	if err := resolver.ValidateResult(result, []OutcomeObservation{outcome}); err == nil {
		t.Fatal("review resolver accepted a missing reviewed Full rendition")
	}
	result.HasFull = true
	result.State = StateComplete
	result.AlternateCount = 0
	if err := resolver.ValidateResult(result, []OutcomeObservation{outcome}); err == nil {
		t.Fatal("review resolver accepted a missing reviewed Alternate Vocal rendition")
	}
}

func TestManifestResolverAcceptsTrueReviewedGameOnly(t *testing.T) {
	decision := Decision{
		MusicID:          83,
		DeclaredVersions: VersionDeclaration{Game: true},
		ScreenshotEvidence: []ScreenshotEvidence{{
			ImageIndex: 1, SHA256: strings.Repeat("1", 64), SizeBytes: 100,
		}},
		ExactRevision: RevisionBinding{
			Provider: model.LyricsSourceProviderSekaipedia,
			PageID:   1118, RevisionID: 326661, Title: "Gimme×Gimme",
			SHA1: strings.Repeat("2", 40), ContentSHA256: strings.Repeat("3", 64),
			AcquisitionID:  strings.Repeat("4", 64),
			EvidenceID:     "revision:sekaipedia:1118:326661:identity",
			EvidenceSHA256: strings.Repeat("5", 64), EnvelopeSHA256: strings.Repeat("6", 64),
		},
		ProviderOutcome: OutcomeBinding{Provider: model.LyricsSourceProviderSekaipedia},
	}
	manifest, err := NewManifest(Binding{
		OriginalNumbersSHA256:  strings.Repeat("7", 64),
		ExportedWorkbookSHA256: strings.Repeat("8", 64),
		ImagesManifestSHA256:   strings.Repeat("9", 64),
		OCRSimilaritySHA256:    strings.Repeat("a", 64),
		PlanID:                 "game-only-plan", PlanSHA256: strings.Repeat("b", 64),
		SourceSnapshotSHA256: strings.Repeat("c", 64),
	}, []Decision{decision})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(manifest, "game-only-plan", strings.Repeat("b", 64), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	outcome := OutcomeObservation{
		Provider: model.LyricsSourceProviderSekaipedia,
		Acquisitions: []AcquisitionObservation{{
			AcquisitionID: strings.Repeat("4", 64),
			EvidenceID:    "revision:sekaipedia:1118:326661:identity",
			SHA256:        strings.Repeat("5", 64), EnvelopeSHA256: strings.Repeat("6", 64),
		}},
	}
	if err := resolver.ValidateResult(ResultObservation{
		MusicID: 83, State: StateGameOnly, HasGame: true,
	}, []OutcomeObservation{outcome}); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ValidateResult(ResultObservation{
		MusicID: 83, State: StateComplete, HasFull: true, HasGame: true,
	}, []OutcomeObservation{outcome}); err == nil {
		t.Fatal("review resolver accepted a synthetic Full for a reviewed Game-only song")
	}
}
