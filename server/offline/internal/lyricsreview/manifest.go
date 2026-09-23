package lyricsreview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"moesekai/server/internal/model"
)

const (
	SchemaVersionV1     = 1
	CanonicalEncodingV1 = "moesekai-lyrics-manual-review-decision-ordered-json-v1"
	DigestAlgorithmV1   = "sha256-moesekai-lyrics-manual-review-decision-v1"
	KindV1              = "lyrics-manual-review-decision-v1"
	MaxManifestBytes    = 8 << 20
	MaxSongs            = 10_000
	MaxScreenshots      = 16
)

var (
	canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalSHA1   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

const (
	StateComplete          = "complete"
	StateGameOnly          = "game_only"
	StateSatisfiedNoLyrics = "satisfied_no_lyrics"
	StateAmbiguous         = "ambiguous"
	StateMissing           = "missing"
	StateIncomplete        = "incomplete"
	StateFailed            = "failed"
)

type Rules struct {
	NetworkAccess            bool `json:"networkAccess"`
	LyricsTextEmitted        bool `json:"lyricsTextEmitted"`
	PhoneticRenditionEmitted bool `json:"phoneticRenditionEmitted"`
	OCRSimilarityIsAuthority bool `json:"ocrSimilarityIsAuthority"`
	RuntimeDecisionGenerated bool `json:"runtimeDecisionGenerated"`
}

type Binding struct {
	OriginalNumbersSHA256  string `json:"originalNumbersSha256"`
	ExportedWorkbookSHA256 string `json:"exportedWorkbookSha256"`
	ImagesManifestSHA256   string `json:"imagesManifestSha256"`
	OCRSimilaritySHA256    string `json:"ocrSimilaritySha256"`
	PlanID                 string `json:"planId"`
	PlanSHA256             string `json:"planSha256"`
	SourceSnapshotSHA256   string `json:"sourceSnapshotSha256"`
	RootEmbeddedSHA256     string `json:"rootEmbeddedSha256,omitempty"`
}

type VersionDeclaration struct {
	Full           bool `json:"full"`
	Game           bool `json:"game"`
	AlternateVocal bool `json:"alternateVocal"`
	AnotherVocal   bool `json:"anotherVocal"`
}

type ScreenshotEvidence struct {
	ImageIndex int    `json:"imageIndex"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
}

type RevisionBinding struct {
	Provider       model.LyricsSourceProvider `json:"provider"`
	PageID         int                        `json:"pageId"`
	RevisionID     int                        `json:"revisionId"`
	Title          string                     `json:"title"`
	SHA1           string                     `json:"sha1"`
	ContentSHA256  string                     `json:"contentSha256"`
	AcquisitionID  string                     `json:"acquisitionId"`
	EvidenceID     string                     `json:"evidenceId"`
	EvidenceSHA256 string                     `json:"evidenceSha256"`
	EnvelopeSHA256 string                     `json:"envelopeSha256"`
}

type OutcomeBinding struct {
	Provider       model.LyricsSourceProvider `json:"provider"`
	OutcomeID      string                     `json:"outcomeId,omitempty"`
	ArtifactSHA256 string                     `json:"artifactSha256,omitempty"`
}

type Decision struct {
	MusicID            int                  `json:"musicId"`
	DeclaredVersions   VersionDeclaration   `json:"declaredVersions"`
	ScreenshotEvidence []ScreenshotEvidence `json:"screenshotEvidence"`
	ExactRevision      RevisionBinding      `json:"exactRevision"`
	ProviderOutcome    OutcomeBinding       `json:"providerOutcome"`
}

type Manifest struct {
	SchemaVersion     int        `json:"schemaVersion"`
	CanonicalEncoding string     `json:"canonicalEncoding"`
	DigestAlgorithm   string     `json:"digestAlgorithm"`
	Kind              string     `json:"kind"`
	ReviewPolicy      string     `json:"reviewPolicy"`
	Rules             Rules      `json:"rules"`
	Binding           Binding    `json:"binding"`
	Songs             []Decision `json:"songs"`
	ManifestSHA256    string     `json:"manifestSha256"`
}

type RevisionObservation struct {
	Provider       model.LyricsSourceProvider
	PageID         int
	RevisionID     int
	Title          string
	SHA1           string
	ContentSHA256  string
	ResponseSHA256 string
}

type AcquisitionObservation struct {
	AcquisitionID  string
	EvidenceID     string
	SHA256         string
	EnvelopeSHA256 string
}

type CandidateObservation struct {
	PageID        int
	RevisionID    int
	SHA1          string
	ContentSHA256 string
}

type OutcomeObservation struct {
	Provider       model.LyricsSourceProvider
	OutcomeID      string
	ArtifactSHA256 string
	Acquisitions   []AcquisitionObservation
	Candidate      *CandidateObservation
}

type ResultObservation struct {
	MusicID           int
	State             string
	HasFull           bool
	HasGame           bool
	HasGameProjection bool
	AlternateCount    int
	AnotherCount      int
}

type ProjectionObservation struct {
	RevisionObservation
	HasFull           bool
	HasGame           bool
	HasGameProjection bool
	AlternateCount    int
	AnotherCount      int
}

type Resolver struct {
	manifest Manifest
	byMusic  map[int]Decision
}

func NewManifest(binding Binding, songs []Decision) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion: SchemaVersionV1, CanonicalEncoding: CanonicalEncodingV1,
		DigestAlgorithm: DigestAlgorithmV1, Kind: KindV1,
		ReviewPolicy: "manual-workbook-v11-screenshot-bound-v1",
		Rules: Rules{
			NetworkAccess: false, LyricsTextEmitted: false,
			PhoneticRenditionEmitted: false, OCRSimilarityIsAuthority: false,
			RuntimeDecisionGenerated: true,
		},
		Binding: binding, Songs: append([]Decision(nil), songs...),
	}
	if err := validateManifest(manifest, false); err != nil {
		return Manifest{}, err
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.ManifestSHA256 = digest
	if err := Validate(manifest); err != nil {
		return Manifest{}, err
	}
	return cloneManifest(manifest), nil
}

func NewResolver(manifest Manifest, planID, planSHA256, sourceSnapshotSHA256 string) (Resolver, error) {
	if err := Validate(manifest); err != nil {
		return Resolver{}, err
	}
	if planID == "" || manifest.Binding.PlanID != planID || manifest.Binding.PlanSHA256 != planSHA256 ||
		manifest.Binding.SourceSnapshotSHA256 != sourceSnapshotSHA256 {
		return Resolver{}, errors.New("lyrics review manifest does not bind the immutable recovery plan")
	}
	byMusic := make(map[int]Decision, len(manifest.Songs))
	for _, decision := range manifest.Songs {
		byMusic[decision.MusicID] = decision
	}
	return Resolver{manifest: cloneManifest(manifest), byMusic: byMusic}, nil
}

func (resolver Resolver) Manifest() Manifest {
	return cloneManifest(resolver.manifest)
}

func OpenResolver(path, planID, planSHA256, sourceSnapshotSHA256 string) (Resolver, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Resolver{}, err
	}
	if len(body) == 0 || len(body) > MaxManifestBytes {
		return Resolver{}, errors.New("lyrics review manifest is outside its byte boundary")
	}
	manifest, err := DecodeCanonical(body)
	if err != nil {
		return Resolver{}, err
	}
	return NewResolver(manifest, planID, planSHA256, sourceSnapshotSHA256)
}

func MarshalCanonical(manifest Manifest) ([]byte, error) {
	if err := Validate(manifest); err != nil {
		return nil, err
	}
	body, err := json.Marshal(manifest)
	if err != nil || len(body) == 0 || len(body) > MaxManifestBytes {
		return nil, errors.New("lyrics review manifest exceeds its byte boundary")
	}
	return body, nil
}

func DecodeCanonical(body []byte) (Manifest, error) {
	if len(body) == 0 || len(body) > MaxManifestBytes {
		return Manifest{}, errors.New("lyrics review manifest bytes are invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("lyrics review manifest contains trailing JSON")
	}
	if err := Validate(manifest); err != nil {
		return Manifest{}, err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(body, canonical) {
		return Manifest{}, errors.New("lyrics review manifest is not canonical JSON")
	}
	return cloneManifest(manifest), nil
}

func Validate(manifest Manifest) error {
	return validateManifest(manifest, true)
}

func (resolver Resolver) ValidateOutcome(musicID int, outcome OutcomeObservation) error {
	decision, found := resolver.byMusic[musicID]
	if !found {
		return nil
	}
	if outcome.Provider != decision.ExactRevision.Provider {
		return nil
	}
	if decision.ProviderOutcome.Provider != "" && decision.ProviderOutcome.Provider != outcome.Provider {
		return errors.New("lyrics review provider outcome provider drifted")
	}
	if decision.ProviderOutcome.OutcomeID != "" && decision.ProviderOutcome.OutcomeID != outcome.OutcomeID {
		return errors.New("lyrics review provider outcome ID drifted")
	}
	if decision.ProviderOutcome.ArtifactSHA256 != "" && decision.ProviderOutcome.ArtifactSHA256 != outcome.ArtifactSHA256 {
		return errors.New("lyrics review provider outcome digest drifted")
	}
	matchedRevision := false
	for _, acquisition := range outcome.Acquisitions {
		if acquisition.AcquisitionID == decision.ExactRevision.AcquisitionID &&
			acquisition.EvidenceID == decision.ExactRevision.EvidenceID &&
			acquisition.SHA256 == decision.ExactRevision.EvidenceSHA256 &&
			acquisition.EnvelopeSHA256 == decision.ExactRevision.EnvelopeSHA256 {
			matchedRevision = true
			break
		}
	}
	if !matchedRevision {
		return errors.New("lyrics review exact revision acquisition binding is missing from the provider outcome")
	}
	if outcome.Candidate != nil {
		candidate := outcome.Candidate
		revision := decision.ExactRevision
		if candidate.PageID != revision.PageID || candidate.RevisionID != revision.RevisionID ||
			candidate.SHA1 != revision.SHA1 || candidate.ContentSHA256 != revision.ContentSHA256 {
			return errors.New("lyrics review provider candidate does not bind the reviewed exact revision")
		}
	}
	return nil
}

func (resolver Resolver) ValidateResult(result ResultObservation, outcomes []OutcomeObservation) error {
	decision, found := resolver.byMusic[result.MusicID]
	if !found {
		return nil
	}
	if result.MusicID <= 0 {
		return errors.New("lyrics review result music ID is invalid")
	}
	for _, outcome := range outcomes {
		if err := resolver.ValidateOutcome(result.MusicID, outcome); err != nil {
			return err
		}
	}
	seenAuthorityOutcome := false
	for _, outcome := range outcomes {
		if outcome.Provider == decision.ExactRevision.Provider {
			seenAuthorityOutcome = true
			break
		}
	}
	if !seenAuthorityOutcome {
		return errors.New("lyrics review result omitted the reviewed authority provider outcome")
	}
	return validateDeclaredResult(decision.DeclaredVersions, result)
}

func (resolver Resolver) ValidateProjection(musicID int, projection ProjectionObservation) error {
	decision, found := resolver.byMusic[musicID]
	if !found {
		return nil
	}
	if err := resolver.validateRevisionObservation(musicID, projection.RevisionObservation); err != nil {
		return err
	}
	return validateDeclaredResult(decision.DeclaredVersions, ResultObservation{
		MusicID: musicID, State: projectionState(projection),
		HasFull: projection.HasFull, HasGame: projection.HasGame,
		HasGameProjection: projection.HasGameProjection,
		AlternateCount:    projection.AlternateCount, AnotherCount: projection.AnotherCount,
	})
}

func (resolver Resolver) validateRevisionObservation(musicID int, observation RevisionObservation) error {
	decision, found := resolver.byMusic[musicID]
	if !found {
		return nil
	}
	revision := decision.ExactRevision
	if observation.Provider != revision.Provider || observation.PageID != revision.PageID ||
		observation.RevisionID != revision.RevisionID || observation.Title != revision.Title ||
		observation.SHA1 != revision.SHA1 || observation.ContentSHA256 != revision.ContentSHA256 {
		return errors.New("lyrics review exact revision identity drifted")
	}
	if observation.ResponseSHA256 != "" && observation.ResponseSHA256 != revision.EvidenceSHA256 {
		return errors.New("lyrics review exact revision response digest drifted")
	}
	return nil
}

func validateDeclaredResult(declared VersionDeclaration, result ResultObservation) error {
	if result.MusicID <= 0 {
		return errors.New("lyrics review result music ID is invalid")
	}
	gamePresent := result.HasGame || result.HasGameProjection
	if declared.Full {
		if !result.HasFull || result.State != StateComplete {
			return errors.New("lyrics review requires a complete primary Full rendition")
		}
	} else if result.HasFull {
		return errors.New("lyrics review forbids a synthetic or unexpected primary Full rendition")
	}
	if declared.Game {
		if !gamePresent {
			return errors.New("lyrics review requires the reviewed Game rendition")
		}
		if !declared.Full && result.State != StateGameOnly {
			return errors.New("lyrics review requires a true Game-only result when Full is absent")
		}
	} else if gamePresent {
		return errors.New("lyrics review forbids an unreviewed Game rendition")
	}
	if declared.AlternateVocal {
		if result.AlternateCount == 0 {
			return errors.New("lyrics review requires the reviewed Alternate Vocal rendition")
		}
	} else if result.AlternateCount != 0 {
		return errors.New("lyrics review found an unreviewed Alternate Vocal rendition")
	}
	if declared.AnotherVocal {
		if result.AnotherCount == 0 {
			return errors.New("lyrics review requires the reviewed Another Vocal rendition")
		}
	} else if result.AnotherCount != 0 {
		return errors.New("lyrics review found an unreviewed Another Vocal rendition")
	}
	return nil
}

func projectionState(projection ProjectionObservation) string {
	switch {
	case !projection.HasFull && projection.HasGame:
		return StateGameOnly
	case projection.HasFull:
		return StateComplete
	default:
		return StateIncomplete
	}
}

func validateManifest(manifest Manifest, requireDigest bool) error {
	if manifest.SchemaVersion != SchemaVersionV1 || manifest.CanonicalEncoding != CanonicalEncodingV1 ||
		manifest.DigestAlgorithm != DigestAlgorithmV1 || manifest.Kind != KindV1 ||
		manifest.ReviewPolicy == "" || len(manifest.Songs) == 0 || len(manifest.Songs) > MaxSongs {
		return errors.New("lyrics review manifest identity is invalid")
	}
	if !manifest.Rules.RuntimeDecisionGenerated || manifest.Rules.NetworkAccess || manifest.Rules.LyricsTextEmitted ||
		manifest.Rules.PhoneticRenditionEmitted || manifest.Rules.OCRSimilarityIsAuthority {
		return errors.New("lyrics review manifest rules are unsafe")
	}
	if err := validateBinding(manifest.Binding); err != nil {
		return err
	}
	if requireDigest {
		if !canonicalSHA256.MatchString(manifest.ManifestSHA256) {
			return errors.New("lyrics review manifest digest is invalid")
		}
	} else if manifest.ManifestSHA256 != "" {
		return errors.New("new lyrics review manifest contains a premature digest")
	}
	lastMusicID := 0
	seen := make(map[int]struct{}, len(manifest.Songs))
	for _, decision := range manifest.Songs {
		if decision.MusicID <= lastMusicID || decision.MusicID <= 0 {
			return errors.New("lyrics review decisions are not strictly ordered")
		}
		if _, duplicate := seen[decision.MusicID]; duplicate {
			return errors.New("lyrics review decision music ID is duplicated")
		}
		seen[decision.MusicID] = struct{}{}
		lastMusicID = decision.MusicID
		if err := validateDecision(decision); err != nil {
			return fmt.Errorf("music %d review decision: %w", decision.MusicID, err)
		}
	}
	if requireDigest {
		digest, err := manifestDigest(manifest)
		if err != nil || digest != manifest.ManifestSHA256 {
			return errors.New("lyrics review manifest digest does not match")
		}
	}
	return nil
}

func validateBinding(binding Binding) error {
	for name, value := range map[string]string{
		"original Numbers":  binding.OriginalNumbersSHA256,
		"exported workbook": binding.ExportedWorkbookSHA256,
		"images manifest":   binding.ImagesManifestSHA256,
		"OCR similarity":    binding.OCRSimilaritySHA256,
		"plan":              binding.PlanSHA256,
		"source snapshot":   binding.SourceSnapshotSHA256,
	} {
		if !canonicalSHA256.MatchString(value) {
			return fmt.Errorf("lyrics review %s binding is invalid", name)
		}
	}
	if binding.PlanID == "" || len(binding.PlanID) > 256 {
		return errors.New("lyrics review plan binding is invalid")
	}
	if binding.RootEmbeddedSHA256 != "" && !canonicalSHA256.MatchString(binding.RootEmbeddedSHA256) {
		return errors.New("lyrics review root binding is invalid")
	}
	return nil
}

func validateDecision(decision Decision) error {
	declaredCount := 0
	if decision.DeclaredVersions.Full {
		declaredCount++
	}
	if decision.DeclaredVersions.Game {
		declaredCount++
	}
	if decision.DeclaredVersions.AlternateVocal {
		declaredCount++
	}
	if decision.DeclaredVersions.AnotherVocal {
		declaredCount++
	}
	if len(decision.ScreenshotEvidence) != declaredCount || len(decision.ScreenshotEvidence) > MaxScreenshots {
		return errors.New("review screenshot evidence does not exactly cover declared versions")
	}
	seenImages := make(map[int]struct{}, len(decision.ScreenshotEvidence))
	for _, screenshot := range decision.ScreenshotEvidence {
		if screenshot.ImageIndex <= 0 || screenshot.SizeBytes <= 0 || !canonicalSHA256.MatchString(screenshot.SHA256) {
			return errors.New("review screenshot evidence is invalid")
		}
		if _, duplicate := seenImages[screenshot.ImageIndex]; duplicate {
			return errors.New("review screenshot evidence is duplicated")
		}
		seenImages[screenshot.ImageIndex] = struct{}{}
	}
	if err := validateRevision(decision.ExactRevision); err != nil {
		return err
	}
	if decision.ProviderOutcome.Provider != "" && decision.ProviderOutcome.Provider != decision.ExactRevision.Provider {
		return errors.New("review provider outcome provider does not match exact revision")
	}
	if decision.ProviderOutcome.OutcomeID != "" && !canonicalID.MatchString(decision.ProviderOutcome.OutcomeID) {
		return errors.New("review provider outcome ID is invalid")
	}
	if decision.ProviderOutcome.ArtifactSHA256 != "" && !canonicalSHA256.MatchString(decision.ProviderOutcome.ArtifactSHA256) {
		return errors.New("review provider outcome digest is invalid")
	}
	return nil
}

func validateRevision(revision RevisionBinding) error {
	if !model.IsValidLyricsSourceProvider(revision.Provider) || revision.PageID <= 0 || revision.RevisionID <= 0 ||
		revision.Title == "" || strings.TrimSpace(revision.Title) != revision.Title ||
		!canonicalSHA1.MatchString(revision.SHA1) || !canonicalSHA256.MatchString(revision.ContentSHA256) ||
		!canonicalSHA256.MatchString(revision.AcquisitionID) || !canonicalID.MatchString(revision.EvidenceID) ||
		!canonicalSHA256.MatchString(revision.EvidenceSHA256) || !canonicalSHA256.MatchString(revision.EnvelopeSHA256) {
		return errors.New("review exact revision binding is invalid")
	}
	return nil
}

func manifestDigest(manifest Manifest) (string, error) {
	copy := manifest
	copy.ManifestSHA256 = ""
	body, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(DigestAlgorithmV1 + "\x00"))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func cloneManifest(input Manifest) Manifest {
	result := input
	result.Songs = append([]Decision(nil), input.Songs...)
	for index := range result.Songs {
		result.Songs[index].ScreenshotEvidence = append([]ScreenshotEvidence(nil), input.Songs[index].ScreenshotEvidence...)
	}
	return result
}
