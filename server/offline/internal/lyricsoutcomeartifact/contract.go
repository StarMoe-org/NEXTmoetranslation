// Package lyricsoutcomeartifact defines the strict content-free persisted
// provider-outcome boundary used by recovery-v2.
package lyricsoutcomeartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf8"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

const (
	SchemaVersionV1     = 1
	CanonicalEncodingV1 = "moesekai-lyrics-provider-outcome-artifact-ordered-json-v1"
	DigestAlgorithmV1   = "sha256-moesekai-lyrics-provider-outcome-artifact-v1"
	MaxArtifactBytes    = 256 << 10
	MaxJSONDepth        = 16
)

var (
	canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalSHA1   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

type AcquisitionRef struct {
	AcquisitionID  string `json:"acquisitionId"`
	EvidenceID     string `json:"evidenceId"`
	SHA256         string `json:"sha256"`
	EnvelopeSHA256 string `json:"envelopeSha256"`
}

type CandidateIdentity struct {
	PageID        int                                 `json:"pageId"`
	RevisionID    int                                 `json:"revisionId"`
	SHA1          string                              `json:"sha1"`
	RawSHA256     string                              `json:"rawSha256"`
	RenditionKey  string                              `json:"renditionKey"`
	VersionReason model.LyricsSourceVersionReasonCode `json:"versionReason"`
	LineCount     int                                 `json:"lineCount"`
}

type Artifact struct {
	SchemaVersion     int                              `json:"schemaVersion"`
	CanonicalEncoding string                           `json:"canonicalEncoding"`
	DigestAlgorithm   string                           `json:"digestAlgorithm"`
	OutcomeID         string                           `json:"outcomeId"`
	MusicID           int                              `json:"musicId"`
	Provider          model.LyricsSourceProvider       `json:"provider"`
	Status            lyricsprovideroutcome.Status     `json:"status"`
	ReasonCode        lyricsprovideroutcome.ReasonCode `json:"reasonCode"`
	Phase             lyricsprovideroutcome.Phase      `json:"phase"`
	Counts            lyricsprovideroutcome.Counts     `json:"counts"`
	ParserVersion     string                           `json:"parserVersion"`
	PolicyVersion     string                           `json:"policyVersion"`
	Candidate         *CandidateIdentity               `json:"candidate"`
	Acquisitions      []AcquisitionRef                 `json:"acquisitions"`
	ArtifactSHA256    string                           `json:"artifactSha256"`
}

func New(
	musicID int,
	provider model.LyricsSourceProvider,
	status lyricsprovideroutcome.Status,
	reason lyricsprovideroutcome.ReasonCode,
	phase lyricsprovideroutcome.Phase,
	counts lyricsprovideroutcome.Counts,
	parserVersion string,
	policyVersion string,
	candidate *CandidateIdentity,
	acquisitions []AcquisitionRef,
) (Artifact, error) {
	artifact := Artifact{
		SchemaVersion: SchemaVersionV1, CanonicalEncoding: CanonicalEncodingV1,
		DigestAlgorithm: DigestAlgorithmV1, MusicID: musicID, Provider: provider,
		Status: status, ReasonCode: reason, Phase: phase, Counts: counts,
		ParserVersion: parserVersion, PolicyVersion: policyVersion,
		Candidate: cloneCandidate(candidate),
	}
	if acquisitions == nil {
		artifact.Acquisitions = nil
	} else {
		artifact.Acquisitions = append([]AcquisitionRef{}, acquisitions...)
	}
	if err := validatePayload(artifact, false); err != nil {
		return Artifact{}, err
	}
	identityDigest, err := digestArtifact("moesekai-lyrics-provider-outcome-id-v1\x00", artifact)
	if err != nil {
		return Artifact{}, err
	}
	artifact.OutcomeID = fmt.Sprintf("outcome:%s:%d:%s", provider, musicID, identityDigest)
	artifact.ArtifactSHA256, err = digestArtifact("moesekai-lyrics-provider-outcome-artifact-v1\x00", artifact)
	if err != nil {
		return Artifact{}, err
	}
	if err := Validate(artifact); err != nil {
		return Artifact{}, err
	}
	return cloneArtifact(artifact), nil
}

func Validate(artifact Artifact) error {
	if err := validatePayload(artifact, true); err != nil {
		return err
	}
	expectedIDArtifact := artifact
	expectedIDArtifact.OutcomeID = ""
	expectedIDArtifact.ArtifactSHA256 = ""
	identityDigest, err := digestArtifact("moesekai-lyrics-provider-outcome-id-v1\x00", expectedIDArtifact)
	if err != nil {
		return err
	}
	expectedID := fmt.Sprintf("outcome:%s:%d:%s", artifact.Provider, artifact.MusicID, identityDigest)
	if artifact.OutcomeID != expectedID {
		return errors.New("provider outcome ID does not match canonical payload")
	}
	digestArtifactInput := artifact
	digestArtifactInput.ArtifactSHA256 = ""
	digest, err := digestArtifact("moesekai-lyrics-provider-outcome-artifact-v1\x00", digestArtifactInput)
	if err != nil || digest != artifact.ArtifactSHA256 {
		return errors.New("provider outcome artifact digest does not match")
	}
	body, err := json.Marshal(artifact)
	if err != nil || len(body) == 0 || len(body) > MaxArtifactBytes || !utf8.Valid(body) {
		return errors.New("provider outcome artifact exceeds its canonical byte boundary")
	}
	return nil
}

func validatePayload(artifact Artifact, requireDerived bool) error {
	parserVersion, policyVersion, registered := registeredRecoveryVersions(artifact.Provider)
	if artifact.SchemaVersion != SchemaVersionV1 || artifact.CanonicalEncoding != CanonicalEncodingV1 ||
		artifact.DigestAlgorithm != DigestAlgorithmV1 || artifact.MusicID <= 0 ||
		!model.IsValidLyricsSourceProvider(artifact.Provider) || !registered ||
		artifact.ParserVersion != parserVersion || artifact.PolicyVersion != policyVersion {
		return errors.New("provider outcome artifact identity is invalid")
	}
	if requireDerived {
		if !canonicalID.MatchString(artifact.OutcomeID) || !canonicalSHA256.MatchString(artifact.ArtifactSHA256) {
			return errors.New("provider outcome artifact derived identity is invalid")
		}
	} else if artifact.OutcomeID != "" || artifact.ArtifactSHA256 != "" {
		return errors.New("new provider outcome artifact contains premature derived identity")
	}

	refs, err := canonicalAcquisitions(artifact.Acquisitions)
	if err != nil || !equalAcquisitions(refs, artifact.Acquisitions) {
		return errors.New("provider outcome artifact acquisition references are invalid")
	}
	candidateValues := []CandidateIdentity(nil)
	indexRefs := make([]model.LyricsSourceIndexEvidenceRef, len(refs))
	for index, ref := range refs {
		indexRefs[index] = model.LyricsSourceIndexEvidenceRef{EvidenceID: ref.EvidenceID, SHA256: ref.SHA256}
	}
	if artifact.Candidate != nil {
		if err := validateCandidate(*artifact.Candidate); err != nil {
			return err
		}
		candidateValues = []CandidateIdentity{*artifact.Candidate}
	}
	outcome, err := lyricsprovideroutcome.New(
		artifact.Provider, artifact.Status, candidateValues,
		lyricsprovideroutcome.Diagnostic{
			Provider: artifact.Provider, Phase: artifact.Phase, ReasonCode: artifact.ReasonCode,
			Counts: artifact.Counts, AcquisitionRefs: indexRefs,
		},
	)
	if err != nil {
		return fmt.Errorf("validate closed provider outcome: %w", err)
	}
	if outcome.Status != artifact.Status {
		return errors.New("provider outcome artifact status drifted")
	}
	return nil
}

func validateCandidate(candidate CandidateIdentity) error {
	if candidate.PageID <= 0 || candidate.RevisionID <= 0 || !canonicalSHA1.MatchString(candidate.SHA1) ||
		(candidate.RawSHA256 != "" && !canonicalSHA256.MatchString(candidate.RawSHA256)) ||
		!validRecoveryRenditionKey(candidate.RenditionKey) ||
		!model.IsValidLyricsSourceCandidateVersionReasonCode(candidate.VersionReason) ||
		candidate.LineCount <= 0 || candidate.LineCount > 1000 {
		return errors.New("provider outcome compact candidate identity is invalid")
	}
	return nil
}

func canonicalAcquisitions(input []AcquisitionRef) ([]AcquisitionRef, error) {
	if input == nil || len(input) > lyricsprovideroutcome.MaxAcquisitionRefs {
		return nil, errors.New("provider outcome acquisitions must be an explicit bounded array")
	}
	refs := append([]AcquisitionRef(nil), input...)
	sort.Slice(refs, func(left, right int) bool {
		if refs[left].EvidenceID != refs[right].EvidenceID {
			return refs[left].EvidenceID < refs[right].EvidenceID
		}
		return refs[left].AcquisitionID < refs[right].AcquisitionID
	})
	seenAcquisitions := make(map[string]AcquisitionRef, len(refs))
	for index, ref := range refs {
		if !canonicalSHA256.MatchString(ref.AcquisitionID) || !canonicalID.MatchString(ref.EvidenceID) ||
			!canonicalSHA256.MatchString(ref.SHA256) || !canonicalSHA256.MatchString(ref.EnvelopeSHA256) {
			return nil, errors.New("provider outcome exact acquisition reference is invalid")
		}
		if index > 0 && refs[index-1].EvidenceID >= ref.EvidenceID {
			return nil, errors.New("provider outcome evidence references are duplicated or conflicting")
		}
		if previous, found := seenAcquisitions[ref.AcquisitionID]; found && previous != ref {
			return nil, errors.New("provider outcome acquisition ID resolves to conflicting evidence")
		}
		seenAcquisitions[ref.AcquisitionID] = ref
	}
	return refs, nil
}

func registeredRecoveryVersions(provider model.LyricsSourceProvider) (string, string, bool) {
	parserVersion, ok := lyricsextractionplan.RegisteredRecoveryParserVersion(lyricsextractionplan.Provider(provider))
	if !ok {
		return "", "", false
	}
	return parserVersion, lyricsextractionplan.RecoveryProviderPolicyVersionV1, true
}

func validRecoveryRenditionKey(value string) bool {
	switch value {
	case "full-original", "full-sekai", "full-vocaloid", "game-sekai", "game-vocaloid":
		return true
	default:
		return false
	}
}

func digestArtifact(domain string, artifact Artifact) (string, error) {
	body, err := json.Marshal(artifact)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func MarshalCanonical(artifact Artifact) ([]byte, error) {
	if err := Validate(artifact); err != nil {
		return nil, err
	}
	return json.Marshal(artifact)
}

func FileName(artifact Artifact) (string, error) {
	if err := Validate(artifact); err != nil {
		return "", err
	}
	return "music-" + strconv.Itoa(artifact.MusicID) + "-" + string(artifact.Provider) + "-" + artifact.ArtifactSHA256 + ".json", nil
}

func cloneCandidate(candidate *CandidateIdentity) *CandidateIdentity {
	if candidate == nil {
		return nil
	}
	cloned := *candidate
	return &cloned
}

func cloneArtifact(artifact Artifact) Artifact {
	artifact.Candidate = cloneCandidate(artifact.Candidate)
	if artifact.Acquisitions == nil {
		artifact.Acquisitions = nil
	} else {
		artifact.Acquisitions = append([]AcquisitionRef{}, artifact.Acquisitions...)
	}
	return artifact
}

func equalAcquisitions(left, right []AcquisitionRef) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
