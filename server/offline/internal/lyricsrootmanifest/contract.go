// Package lyricsrootmanifest defines the closed compact lyrics root-manifest v1 boundary.
package lyricsrootmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsevidencepack"
)

const (
	SchemaVersionV1     = 1
	CanonicalEncodingV1 = "moesekai-lyrics-root-manifest-ordered-json-v1"
	DigestAlgorithmV1   = "sha256-moesekai-lyrics-root-manifest-v1"
	CanonicalEncodingV2 = "moesekai-lyrics-root-manifest-ordered-json-v2"
	DigestAlgorithmV2   = "sha256-moesekai-lyrics-root-manifest-v2"

	MaxManifestBytes        = 16 << 20
	MaxAssemblyRequestBytes = 16 << 20
	MaxJSONDepth            = 16
	MaxProviderOutcomes     = 16
	MaxSelectionsPerSong    = 64
	MaxIdentityBytes        = 128
	MaxOutcomeIDBytes       = 256
)

var (
	canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	canonicalRefID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

	ErrAlreadyPublished = errors.New("lyrics root manifest is already published")
)

type ScopeKind string

const (
	ScopeFinal   ScopeKind = "final"
	ScopePartial ScopeKind = "partial"
	ScopeRetry   ScopeKind = "retry"
)

// CatalogBinding contains only immutable catalog identity and hash material.
type CatalogBinding struct {
	SchemaVersion         int    `json:"schemaVersion"`
	RuntimeSchemaVersion  int    `json:"runtimeSchemaVersion"`
	RecordCount           int    `json:"recordCount"`
	IdentityPolicyVersion string `json:"identityPolicyVersion"`
	SourceSHA256          string `json:"sourceSha256"`
	IdentitySHA256        string `json:"identitySha256"`
	MusicIDsSHA256        string `json:"musicIdsSha256"`
}

// PlanBinding binds the immutable extraction plan without retaining its path.
type PlanBinding struct {
	PlanID string `json:"planId"`
	SHA256 string `json:"sha256"`
}

// ScopeBinding makes final, partial, and retry roots unambiguous. Partial and
// retry scopes must identify the exact root they supersede.
type ScopeBinding struct {
	Kind                 ScopeKind `json:"kind"`
	ScopeID              string    `json:"scopeId"`
	SupersedesRootID     string    `json:"supersedesRootId"`
	SupersedesRootSHA256 string    `json:"supersedesRootSha256"`
}

// ProviderOutcomeRef points to a compact provider outcome artifact.
type ProviderOutcomeRef struct {
	Provider  model.LyricsSourceProvider `json:"provider"`
	OutcomeID string                     `json:"outcomeId"`
	SHA256    string                     `json:"sha256"`
}

// SelectedEvidenceRef is the exact pack reference selected for one song.
type SelectedEvidenceRef = lyricscontract.EvidenceRef

// SongResultRef is the ordered compact per-song output binding.
type SongResultRef struct {
	MusicID          int                          `json:"musicId"`
	State            lyricscontract.CoverageState `json:"state"`
	ResultSHA256     string                       `json:"resultSha256"`
	ProviderOutcomes []ProviderOutcomeRef         `json:"providerOutcomes"`
	SelectedEvidence []SelectedEvidenceRef        `json:"selectedEvidence"`
}

// PackShardBinding retains the required ordered shard digest sequence and counters.
type PackShardBinding struct {
	Ordinal          int    `json:"ordinal"`
	SHA256           string `json:"sha256"`
	EncodedByteCount int    `json:"encodedByteCount"`
	RawByteCount     int    `json:"rawByteCount"`
	ItemCount        int    `json:"itemCount"`
}

// EvidencePackBinding compactly binds the exact selected union and every ordered shard.
type EvidencePackBinding struct {
	PackSHA256       string             `json:"packSha256"`
	SelectionSHA256  string             `json:"selectionSha256"`
	ItemCount        int                `json:"itemCount"`
	ShardCount       int                `json:"shardCount"`
	RawByteCount     int64              `json:"rawByteCount"`
	EncodedByteCount int64              `json:"encodedByteCount"`
	Shards           []PackShardBinding `json:"shards"`
}

// AssemblyRequest is the closed root input before derived pack, coverage, and root digests.
type AssemblyRequest struct {
	RootID  string          `json:"rootId"`
	Scope   ScopeBinding    `json:"scope"`
	Catalog CatalogBinding  `json:"catalog"`
	Plan    PlanBinding     `json:"plan"`
	Songs   []SongResultRef `json:"songs"`
}

// Manifest is the strict compact output. It intentionally has no title,
// lyrics, raw payload, translation, romanization, private error, timestamp, or
// path field.
type Manifest struct {
	SchemaVersion     int                     `json:"schemaVersion"`
	CanonicalEncoding string                  `json:"canonicalEncoding"`
	DigestAlgorithm   string                  `json:"digestAlgorithm"`
	RootID            string                  `json:"rootId"`
	Scope             ScopeBinding            `json:"scope"`
	Catalog           CatalogBinding          `json:"catalog"`
	Plan              PlanBinding             `json:"plan"`
	Songs             []SongResultRef         `json:"songs"`
	EvidencePack      EvidencePackBinding     `json:"evidencePack"`
	Coverage          lyricscontract.Coverage `json:"coverage"`
	RootSHA256        string                  `json:"rootSha256"`
}

type requestDerived struct {
	coverage lyricscontract.Coverage
	selected []lyricscontract.EvidenceRef
}

// Assemble derives a final root, all counters, and pack bindings and proves the
// global selected union. Partial and retry roots require AssembleAgainstParent.
func Assemble(request AssemblyRequest, resolver *lyricsevidencepack.Resolver) (Manifest, error) {
	if request.Scope.Kind == ScopePartial || request.Scope.Kind == ScopeRetry {
		return Manifest{}, errors.New("partial or retry lyrics root assembly requires a validated direct parent root")
	}
	return assemble(request, resolver)
}

// AssembleAgainstParent derives a partial or retry root after self-contained
// validation of its exact direct parent, supersession binding, catalog identity,
// and true subset. The caller must retain separate parent-aware proof when that
// direct parent is itself partial or retry.
func AssembleAgainstParent(
	request AssemblyRequest,
	resolver *lyricsevidencepack.Resolver,
	parent Manifest,
) (Manifest, error) {
	if request.Scope.Kind == ScopeFinal {
		return Manifest{}, errors.New("final lyrics root assembly must not specify a parent root")
	}
	if err := Validate(parent); err != nil {
		return Manifest{}, fmt.Errorf("validate parent lyrics root: %w", err)
	}
	if _, err := validateRequest(request, lyricscontract.SchemaVersionV2); err != nil {
		return Manifest{}, err
	}
	if err := validateParentBinding(request, parent); err != nil {
		return Manifest{}, err
	}
	return assemble(request, resolver)
}

func assemble(request AssemblyRequest, resolver *lyricsevidencepack.Resolver) (Manifest, error) {
	if resolver == nil {
		return Manifest{}, errors.New("validated evidence pack resolver is required")
	}
	derived, err := validateRequest(request, lyricscontract.SchemaVersionV2)
	if err != nil {
		return Manifest{}, err
	}
	if err := resolver.ValidateSelected(derived.selected); err != nil {
		return Manifest{}, err
	}
	packManifest := resolver.Manifest()
	manifest := Manifest{
		SchemaVersion: lyricscontract.SchemaVersionV2, CanonicalEncoding: CanonicalEncodingV2, DigestAlgorithm: DigestAlgorithmV2,
		RootID: request.RootID, Scope: request.Scope, Catalog: request.Catalog, Plan: request.Plan,
		Songs: cloneSongs(request.Songs), EvidencePack: bindingFromPack(packManifest), Coverage: derived.coverage,
	}
	manifest.RootSHA256, err = rootDigest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if err := Validate(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate verifies the self-contained compact root contract. Partial and retry
// roots additionally require ValidateAgainstParent for their relational proof.
func Validate(manifest Manifest) error {
	validEnvelope := manifest.SchemaVersion == SchemaVersionV1 &&
		manifest.CanonicalEncoding == CanonicalEncodingV1 && manifest.DigestAlgorithm == DigestAlgorithmV1 ||
		manifest.SchemaVersion == lyricscontract.SchemaVersionV2 &&
			manifest.CanonicalEncoding == CanonicalEncodingV2 && manifest.DigestAlgorithm == DigestAlgorithmV2
	if !validEnvelope || !canonicalSHA256.MatchString(manifest.RootSHA256) {
		return errors.New("lyrics root manifest envelope is invalid")
	}
	request := AssemblyRequest{RootID: manifest.RootID, Scope: manifest.Scope, Catalog: manifest.Catalog, Plan: manifest.Plan, Songs: manifest.Songs}
	derived, err := validateRequest(request, manifest.SchemaVersion)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(derived.coverage, manifest.Coverage) {
		return errors.New("lyrics root coverage counters do not match ordered song results")
	}
	if err := validatePackBinding(manifest.EvidencePack, derived); err != nil {
		return err
	}
	digest, err := rootDigest(manifest)
	if err != nil || digest != manifest.RootSHA256 {
		return errors.New("lyrics root manifest digest does not match")
	}
	body, err := json.Marshal(manifest)
	if err != nil || len(body) == 0 || len(body) > MaxManifestBytes || !utf8.Valid(body) {
		return errors.New("lyrics root manifest exceeds its encoded boundary")
	}
	return nil
}

// ValidateAgainstParent verifies a partial or retry root's direct supersession,
// compatible catalog identity, and true nonempty strict subset of parent songs.
// A partial or retry parent still requires its own separately retained proof.
func ValidateAgainstParent(manifest Manifest, parent Manifest) error {
	if err := Validate(parent); err != nil {
		return fmt.Errorf("validate parent lyrics root: %w", err)
	}
	if err := Validate(manifest); err != nil {
		return err
	}
	request := AssemblyRequest{
		RootID: manifest.RootID, Scope: manifest.Scope, Catalog: manifest.Catalog,
		Plan: manifest.Plan, Songs: manifest.Songs,
	}
	return validateParentBinding(request, parent)
}

// ValidateAgainstPack rebinds a decoded root to the exact validated pack. It
// does not prove a partial or retry root's relationship to its parent.
func ValidateAgainstPack(manifest Manifest, resolver *lyricsevidencepack.Resolver) error {
	if err := Validate(manifest); err != nil {
		return err
	}
	if resolver == nil {
		return errors.New("validated evidence pack resolver is required")
	}
	derived, _ := validateRequest(
		AssemblyRequest{RootID: manifest.RootID, Scope: manifest.Scope, Catalog: manifest.Catalog, Plan: manifest.Plan, Songs: manifest.Songs},
		manifest.SchemaVersion,
	)
	if err := resolver.ValidateSelected(derived.selected); err != nil {
		return err
	}
	if !reflect.DeepEqual(manifest.EvidencePack, bindingFromPack(resolver.Manifest())) {
		return errors.New("lyrics root evidence pack binding does not match the exact pack")
	}
	return nil
}

// MarshalCanonical emits the sole compact root encoding after self-contained
// validation. Partial and retry callers must separately retain parent proof.
func MarshalCanonical(manifest Manifest) ([]byte, error) {
	if err := Validate(manifest); err != nil {
		return nil, err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > MaxManifestBytes || !utf8.Valid(body) {
		return nil, errors.New("lyrics root manifest exceeds its encoded boundary")
	}
	return body, nil
}

func validateParentBinding(request AssemblyRequest, parent Manifest) error {
	if request.Scope.Kind != ScopePartial && request.Scope.Kind != ScopeRetry {
		return errors.New("only partial or retry lyrics roots may bind a parent root")
	}
	if request.Scope.SupersedesRootID != parent.RootID || request.Scope.SupersedesRootSHA256 != parent.RootSHA256 {
		return errors.New("partial or retry lyrics root supersession does not match the direct parent root")
	}
	if request.Scope.ScopeID != parent.Scope.ScopeID || !reflect.DeepEqual(request.Catalog, parent.Catalog) {
		return errors.New("partial or retry lyrics root catalog identity is incompatible with the parent root")
	}
	if len(request.Songs) == 0 || len(request.Songs) >= len(parent.Songs) {
		return errors.New("partial or retry lyrics root music IDs are not a nonempty strict subset of the parent root")
	}
	parentIndex := 0
	for _, childSong := range request.Songs {
		for parentIndex < len(parent.Songs) && parent.Songs[parentIndex].MusicID < childSong.MusicID {
			parentIndex++
		}
		if parentIndex == len(parent.Songs) || parent.Songs[parentIndex].MusicID != childSong.MusicID {
			return errors.New("partial or retry lyrics root music IDs are not a nonempty strict subset of the parent root")
		}
		parentIndex++
	}
	return nil
}

func validateRequest(request AssemblyRequest, schemaVersion int) (requestDerived, error) {
	var derived requestDerived
	if schemaVersion != SchemaVersionV1 && schemaVersion != lyricscontract.SchemaVersionV2 {
		return derived, errors.New("lyrics root request schema version is invalid")
	}
	if !validIdentity(request.RootID) || !validIdentity(request.Scope.ScopeID) || request.Catalog.SchemaVersion <= 0 ||
		request.Catalog.RuntimeSchemaVersion < request.Catalog.SchemaVersion || request.Catalog.RecordCount <= 0 ||
		request.Catalog.RecordCount > lyricscontract.MaxCatalogRecordCount ||
		!validIdentity(request.Catalog.IdentityPolicyVersion) || !canonicalSHA256.MatchString(request.Catalog.SourceSHA256) ||
		!canonicalSHA256.MatchString(request.Catalog.IdentitySHA256) ||
		!canonicalSHA256.MatchString(request.Catalog.MusicIDsSHA256) || !validIdentity(request.Plan.PlanID) ||
		!canonicalSHA256.MatchString(request.Plan.SHA256) || request.Songs == nil ||
		len(request.Songs) > request.Catalog.RecordCount {
		return derived, errors.New("lyrics root request identity bindings are invalid")
	}
	switch request.Scope.Kind {
	case ScopeFinal:
		if request.Scope.SupersedesRootID != "" || request.Scope.SupersedesRootSHA256 != "" ||
			len(request.Songs) != request.Catalog.RecordCount {
			return derived, errors.New("final lyrics root must be unsuperseded and exactly match the positive bounded catalog record count")
		}
	case ScopePartial, ScopeRetry:
		if !validIdentity(request.Scope.SupersedesRootID) || !canonicalSHA256.MatchString(request.Scope.SupersedesRootSHA256) ||
			request.Scope.SupersedesRootID == request.RootID || len(request.Songs) == 0 {
			return derived, errors.New("partial or retry lyrics root requires an explicit supersession and nonempty bounded scope")
		}
	default:
		return derived, errors.New("lyrics root scope kind is invalid")
	}
	coverage := lyricscontract.Coverage{Total: len(request.Songs)}
	musicIDs := make([]int, len(request.Songs))
	acquisitions := make(map[string]SelectedEvidenceRef)
	evidence := make(map[string]SelectedEvidenceRef)
	lastMusicID := 0
	for songIndex, song := range request.Songs {
		if song.MusicID <= 0 || songIndex > 0 && song.MusicID <= lastMusicID || !canonicalSHA256.MatchString(song.ResultSHA256) ||
			song.ProviderOutcomes == nil || len(song.ProviderOutcomes) > MaxProviderOutcomes || song.SelectedEvidence == nil ||
			len(song.SelectedEvidence) > MaxSelectionsPerSong {
			return derived, fmt.Errorf("lyrics root song result %d is invalid", songIndex)
		}
		lastMusicID = song.MusicID
		musicIDs[songIndex] = song.MusicID
		switch song.State {
		case lyricscontract.CoverageComplete:
			coverage.Complete++
			if len(song.ProviderOutcomes) == 0 || len(song.SelectedEvidence) == 0 {
				return derived, errors.New("complete song result requires provider outcomes and selected evidence")
			}
		case lyricscontract.CoverageGameOnly:
			if schemaVersion != lyricscontract.SchemaVersionV2 || len(song.ProviderOutcomes) == 0 || len(song.SelectedEvidence) == 0 {
				return derived, errors.New("Game-only song result requires root v2 provider outcomes and selected evidence")
			}
			coverage.GameOnly++
		case lyricscontract.CoverageSatisfiedNoLyrics:
			if schemaVersion != lyricscontract.SchemaVersionV2 || len(song.ProviderOutcomes) == 0 || len(song.SelectedEvidence) != 0 {
				return derived, errors.New("satisfied no-lyrics song result requires root v2 provider outcomes and no selected lyrics evidence")
			}
			coverage.SatisfiedNoLyrics++
		case lyricscontract.CoverageCatalogReview:
			coverage.CatalogReview++
		case lyricscontract.CoverageGameSizeEvidence:
			coverage.GameSizeEvidence++
		case lyricscontract.CoverageAmbiguous:
			coverage.Ambiguous++
		case lyricscontract.CoverageMissing:
			coverage.Missing++
		case lyricscontract.CoverageIncomplete:
			coverage.Incomplete++
		case lyricscontract.CoverageFailed:
			coverage.Failed++
		default:
			return derived, errors.New("song result has an invalid coverage state")
		}
		if err := validateProviderOutcomes(song.ProviderOutcomes); err != nil {
			return derived, err
		}
		if err := validateSelections(song.SelectedEvidence); err != nil {
			return derived, err
		}
		coverage.ProviderOutcomeRefCount += len(song.ProviderOutcomes)
		coverage.SelectionRefCount += len(song.SelectedEvidence)
		for _, selection := range song.SelectedEvidence {
			if existing, found := acquisitions[selection.AcquisitionID]; found && existing != selection {
				return derived, errors.New("one AcquisitionID resolves to conflicting selected evidence")
			}
			acquisitions[selection.AcquisitionID] = selection
			if existing, found := evidence[selection.EvidenceID]; found && existing != selection {
				return derived, errors.New("one EvidenceID resolves to conflicting selected acquisition/evidence")
			}
			evidence[selection.EvidenceID] = selection
		}
	}
	if request.Scope.Kind == ScopeFinal {
		musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
		if err != nil || musicIDsSHA256 != request.Catalog.MusicIDsSHA256 {
			return derived, errors.New("final lyrics root ordered music IDs do not match the catalog binding")
		}
	}
	coverage.UniqueAcquisitionCount = len(acquisitions)
	coverage.UniqueEvidenceCount = len(evidence)
	derived.coverage = coverage
	derived.selected = make([]lyricscontract.EvidenceRef, 0, len(evidence))
	for _, selection := range evidence {
		derived.selected = append(derived.selected, selection)
	}
	sort.Slice(derived.selected, func(left, right int) bool {
		return derived.selected[left].EvidenceID < derived.selected[right].EvidenceID
	})
	return derived, nil
}

func validateProviderOutcomes(outcomes []ProviderOutcomeRef) error {
	seenProviders := make(map[model.LyricsSourceProvider]struct{}, len(outcomes))
	seenOutcomes := make(map[string]struct{}, len(outcomes))
	for _, outcome := range outcomes {
		if !model.IsValidLyricsSourceProvider(outcome.Provider) || !validRefIdentity(outcome.OutcomeID, MaxOutcomeIDBytes) ||
			!canonicalSHA256.MatchString(outcome.SHA256) {
			return errors.New("provider outcome reference is invalid")
		}
		if _, duplicate := seenProviders[outcome.Provider]; duplicate {
			return errors.New("provider outcome reference duplicates one evaluated provider")
		}
		if _, duplicate := seenOutcomes[outcome.OutcomeID]; duplicate {
			return errors.New("provider outcome reference is duplicated")
		}
		seenProviders[outcome.Provider] = struct{}{}
		seenOutcomes[outcome.OutcomeID] = struct{}{}
	}
	return nil
}

func validateSelections(selections []SelectedEvidenceRef) error {
	last := ""
	for _, selection := range selections {
		if !model.IsValidLyricsSourceProvider(selection.Provider) || !canonicalSHA256.MatchString(selection.AcquisitionID) ||
			!validRefIdentity(selection.EvidenceID, 256) || !canonicalSHA256.MatchString(selection.SHA256) ||
			!canonicalSHA256.MatchString(selection.EnvelopeSHA256) {
			return errors.New("selected acquisition/evidence reference is invalid")
		}
		key := selection.EvidenceID + "\x00" + string(selection.Provider) + "\x00" + selection.AcquisitionID
		if last != "" && last >= key {
			return errors.New("selected acquisition/evidence references are not uniquely ordered")
		}
		last = key
	}
	return nil
}

func validatePackBinding(binding EvidencePackBinding, derived requestDerived) error {
	if !canonicalSHA256.MatchString(binding.PackSHA256) || !canonicalSHA256.MatchString(binding.SelectionSHA256) ||
		binding.ItemCount != derived.coverage.UniqueEvidenceCount || binding.ShardCount != len(binding.Shards) ||
		binding.Shards == nil || binding.RawByteCount < 0 || binding.RawByteCount > lyricsevidencepack.MaxPackRawBytes ||
		binding.EncodedByteCount < 0 || binding.EncodedByteCount > lyricsevidencepack.MaxPackEncodedBytes {
		return errors.New("lyrics root evidence pack binding is invalid")
	}
	selectionDigest, err := lyricscontract.OrderedSelectionSHA256(derived.selected)
	if err != nil || selectionDigest != binding.SelectionSHA256 {
		return errors.New("lyrics root selected evidence union does not match its pack binding")
	}
	items := 0
	var rawBytes, encodedBytes int64
	for index, shard := range binding.Shards {
		if shard.Ordinal != index || !canonicalSHA256.MatchString(shard.SHA256) || shard.ItemCount <= 0 ||
			shard.RawByteCount <= 0 || shard.RawByteCount > lyricsevidencepack.MaxShardRawBytes ||
			shard.EncodedByteCount <= 0 || shard.EncodedByteCount > lyricsevidencepack.MaxShardEncodedBytes {
			return errors.New("lyrics root ordered evidence shard binding is invalid")
		}
		items += shard.ItemCount
		rawBytes += int64(shard.RawByteCount)
		encodedBytes += int64(shard.EncodedByteCount)
	}
	if items != binding.ItemCount || rawBytes != binding.RawByteCount || encodedBytes != binding.EncodedByteCount ||
		binding.ShardCount == 0 && binding.ItemCount != 0 || binding.ShardCount != 0 && binding.ItemCount == 0 {
		return errors.New("lyrics root evidence pack counters do not match ordered shards")
	}
	return nil
}

func bindingFromPack(manifest lyricsevidencepack.Manifest) EvidencePackBinding {
	binding := EvidencePackBinding{
		PackSHA256: manifest.PackSHA256, SelectionSHA256: manifest.SelectionSHA256,
		ItemCount: manifest.Totals.ItemCount, ShardCount: manifest.Totals.ShardCount,
		RawByteCount: manifest.Totals.RawByteCount, EncodedByteCount: manifest.Totals.EncodedByteCount,
		Shards: make([]PackShardBinding, len(manifest.Shards)),
	}
	for index, shard := range manifest.Shards {
		binding.Shards[index] = PackShardBinding{Ordinal: shard.Ordinal, SHA256: shard.SHA256,
			EncodedByteCount: shard.EncodedByteCount, RawByteCount: shard.RawByteCount, ItemCount: shard.ItemCount}
	}
	return binding
}

func rootDigest(manifest Manifest) (string, error) {
	manifest.RootSHA256 = ""
	body, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	domain := ""
	switch manifest.SchemaVersion {
	case SchemaVersionV1:
		domain = "moesekai-lyrics-root-manifest-v1\x00"
	case lyricscontract.SchemaVersionV2:
		domain = "moesekai-lyrics-root-manifest-v2\x00"
	default:
		return "", errors.New("lyrics root manifest digest schema version is invalid")
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validIdentity(value string) bool {
	return len(value) <= MaxIdentityBytes && canonicalID.MatchString(value)
}

func validRefIdentity(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && canonicalRefID.MatchString(value) && strings.TrimSpace(value) == value
}

func cloneSongs(input []SongResultRef) []SongResultRef {
	result := append([]SongResultRef{}, input...)
	for index := range result {
		result[index].ProviderOutcomes = append([]ProviderOutcomeRef{}, input[index].ProviderOutcomes...)
		result[index].SelectedEvidence = append([]SelectedEvidenceRef{}, input[index].SelectedEvidence...)
	}
	return result
}
