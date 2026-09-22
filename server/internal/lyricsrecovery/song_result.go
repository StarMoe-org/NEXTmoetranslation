package lyricsrecovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/lyricscompose"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

const (
	SongResultSchemaVersionV1         = 1
	SongResultCanonicalEncodingV1     = "moesekai-lyrics-recovery-song-result-ordered-json-v1"
	SongResultDigestAlgorithmV1       = "sha256-moesekai-lyrics-recovery-song-result-v1"
	SongResultSchemaVersionV2         = 2
	SongResultCanonicalEncodingV2     = "moesekai-lyrics-recovery-song-result-ordered-json-v2"
	SongResultDigestAlgorithmV2       = "sha256-moesekai-lyrics-recovery-song-result-v2"
	SongResultSchemaVersionV3         = 3
	SongResultCanonicalEncodingV3     = "moesekai-lyrics-recovery-song-result-ordered-json-v3"
	SongResultDigestAlgorithmV3       = "sha256-moesekai-lyrics-recovery-song-result-v3"
	NoLyricsReasonCatalogInstrumental = "catalog_instrumental"
	MaxSongResultBytes                = 8 << 20
	MaxSongResultJSONDepth            = 32
)

var canonicalSongResultSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type SongResult struct {
	SchemaVersion     int                                     `json:"schemaVersion"`
	CanonicalEncoding string                                  `json:"canonicalEncoding"`
	DigestAlgorithm   string                                  `json:"digestAlgorithm"`
	MusicID           int                                     `json:"musicId"`
	State             lyricscontract.CoverageState            `json:"state"`
	ReasonCode        model.LyricsSourceVersionReasonCode     `json:"reasonCode"`
	NoLyricsReason    string                                  `json:"noLyricsReason,omitempty"`
	ProviderOutcomes  []lyricsrootmanifest.ProviderOutcomeRef `json:"providerOutcomes"`
	SelectedEvidence  []lyricscontract.EvidenceRef            `json:"selectedEvidence"`
	Components        ComponentEvidence                       `json:"components"`
	Full              *model.LyricsSourceFull                 `json:"full"`
	Game              *model.LyricsSourceFull                 `json:"game,omitempty"`
	GameProjection    *model.LyricsSourceGameProjection       `json:"gameProjection"`
	AlternateVocals   []model.LyricsSourceAlternateVocal      `json:"alternateVocals,omitempty"`
	Translations      []string                                `json:"translations,omitempty"`
	Renditions        []SongResultRendition                   `json:"renditions,omitempty"`
	ResultSHA256      string                                  `json:"resultSha256"`
}

func NewSongResult(replay ReplayResult) (SongResult, error) {
	if replay.Composition != nil && len(replay.Composition.Renditions) != 0 {
		return newSongResultV3(replay)
	}
	result := SongResult{
		SchemaVersion: SongResultSchemaVersionV2, CanonicalEncoding: SongResultCanonicalEncodingV2,
		DigestAlgorithm: SongResultDigestAlgorithmV2, MusicID: replay.MusicID,
		ProviderOutcomes: make([]lyricsrootmanifest.ProviderOutcomeRef, len(replay.Providers)),
		SelectedEvidence: append([]lyricscontract.EvidenceRef(nil), replay.Selected...),
		Components:       cloneComponentEvidence(replay.Components),
	}
	for index, provider := range replay.Providers {
		result.ProviderOutcomes[index] = lyricsrootmanifest.ProviderOutcomeRef{
			Provider: provider.Artifact.Provider, OutcomeID: provider.Artifact.OutcomeID,
			SHA256: provider.Artifact.ArtifactSHA256,
		}
	}
	sort.Slice(result.SelectedEvidence, func(left, right int) bool {
		return result.SelectedEvidence[left].EvidenceID < result.SelectedEvidence[right].EvidenceID
	})
	switch {
	case replay.Instrumental:
		if replay.Composition != nil {
			return SongResult{}, errors.New("catalog-proven instrumental conflicts with recovered lyrics composition")
		}
		result.State = lyricscontract.CoverageSatisfiedNoLyrics
		result.NoLyricsReason = NoLyricsReasonCatalogInstrumental
		result.Components = emptyComponentEvidence()
		result.SelectedEvidence = []lyricscontract.EvidenceRef{}
	case replay.Composition != nil && replay.Composition.Game != nil && len(replay.Composition.Full.Lines) == 0:
		composition := replay.Composition
		if len(composition.Full.Lines) != 0 || composition.GameProjection != nil {
			return SongResult{}, errors.New("Game-only recovery composition contains a provisional Full")
		}
		result.AlternateVocals = model.CloneLyricsSourceAlternateVocals(composition.AlternateVocals)
		hadPerformerSegmentation := songResultFullHasPerformerSegmentation(*composition.Game)
		game, err := lyricscontract.NormalizePersistedPerformerMetadata(*composition.Game)
		if err != nil {
			return SongResult{}, fmt.Errorf("lyrics recovery Game-only result: %w", lyricscontract.ErrUnsafePerformerMetadata)
		}
		if hadPerformerSegmentation && !songResultFullHasPerformerSegmentation(game) {
			omitSongResultPerformerSegmentationEvidence(&result)
		}
		persistedRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(game.RubyGeneratorVersion)
		if err != nil {
			return SongResult{}, err
		}
		game.RubyGeneratorVersion = persistedRubyVersion
		result.State = lyricscontract.CoverageGameOnly
		result.ReasonCode = composition.ReasonCode
		result.Game = &game
		translations, err := songResultTranslations(*composition, replay.Providers)
		if err != nil {
			return SongResult{}, err
		}
		result.Translations = translations
	case replay.Composition != nil:
		composition := replay.Composition
		result.AlternateVocals = model.CloneLyricsSourceAlternateVocals(composition.AlternateVocals)
		hadPerformerSegmentation := songResultFullHasPerformerSegmentation(composition.Full)
		full, err := lyricscontract.NormalizePersistedPerformerMetadata(composition.Full)
		if err != nil {
			return SongResult{}, fmt.Errorf("lyrics recovery song result: %w", lyricscontract.ErrUnsafePerformerMetadata)
		}
		if hadPerformerSegmentation && !songResultFullHasPerformerSegmentation(full) {
			omitSongResultPerformerSegmentationEvidence(&result)
		}
		persistedRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(full.RubyGeneratorVersion)
		if err != nil {
			return SongResult{}, err
		}
		full.RubyGeneratorVersion = persistedRubyVersion
		result.State = lyricscontract.CoverageComplete
		result.ReasonCode = composition.ReasonCode
		result.Full = &full
		if composition.Game != nil {
			gameHadPerformerSegmentation := songResultFullHasPerformerSegmentation(*composition.Game)
			game, gameErr := lyricscontract.NormalizePersistedPerformerMetadata(*composition.Game)
			if gameErr != nil {
				return SongResult{}, fmt.Errorf("lyrics recovery Game artifact: %w", lyricscontract.ErrUnsafePerformerMetadata)
			}
			if gameHadPerformerSegmentation && !songResultFullHasPerformerSegmentation(game) {
				omitSongResultPerformerSegmentationEvidence(&result)
			}
			gameRubyVersion, gameErr := lyricssource.RecoveryPersistedRubyGeneratorVersion(game.RubyGeneratorVersion)
			if gameErr != nil {
				return SongResult{}, gameErr
			}
			game.RubyGeneratorVersion = gameRubyVersion
			result.Game = &game
		}
		if composition.GameProjection != nil {
			projection := *composition.GameProjection
			projection.LineIDs = append([]string{}, projection.LineIDs...)
			result.GameProjection = &projection
		}
		translations, err := songResultTranslations(*composition, replay.Providers)
		if err != nil {
			return SongResult{}, err
		}
		result.Translations = translations
	default:
		result.State = closedCoverageState(replay.Providers)
		result.ReasonCode = model.LyricsSourceVersionReasonVersionConflict
		result.Components = emptyComponentEvidence()
		result.SelectedEvidence = []lyricscontract.EvidenceRef{}
	}
	result.ResultSHA256 = ""
	if err := validateSongResult(result, false); err != nil {
		return SongResult{}, err
	}
	digest, err := songResultDigest(result)
	if err != nil {
		return SongResult{}, err
	}
	result.ResultSHA256 = digest
	if err := ValidateSongResult(result); err != nil {
		return SongResult{}, err
	}
	return cloneSongResult(result), nil
}

func ValidateSongResult(result SongResult) error {
	if err := validateSongResult(result, true); err != nil {
		return err
	}
	digestInput := result
	digestInput.ResultSHA256 = ""
	digest, err := songResultDigest(digestInput)
	if err != nil {
		return err
	}
	if digest != result.ResultSHA256 {
		return fmt.Errorf("lyrics recovery song result digest does not match: got %s want %s", digest, result.ResultSHA256)
	}
	body, err := json.Marshal(result)
	if err != nil || len(body) == 0 || len(body) > MaxSongResultBytes || !utf8.Valid(body) {
		return errors.New("lyrics recovery song result exceeds its byte boundary")
	}
	return nil
}

func validateSongResult(result SongResult, requireDigest bool) error {
	if result.SchemaVersion == SongResultSchemaVersionV3 {
		return validateSongResultV3(result, requireDigest)
	}
	validEnvelope := result.SchemaVersion == SongResultSchemaVersionV1 &&
		result.CanonicalEncoding == SongResultCanonicalEncodingV1 && result.DigestAlgorithm == SongResultDigestAlgorithmV1 ||
		result.SchemaVersion == SongResultSchemaVersionV2 &&
			result.CanonicalEncoding == SongResultCanonicalEncodingV2 && result.DigestAlgorithm == SongResultDigestAlgorithmV2
	if !validEnvelope || result.MusicID <= 0 || result.ProviderOutcomes == nil || result.SelectedEvidence == nil ||
		len(result.ProviderOutcomes) > lyricsrootmanifest.MaxProviderOutcomes ||
		len(result.SelectedEvidence) > lyricsrootmanifest.MaxSelectionsPerSong {
		return errors.New("lyrics recovery song result identity is invalid")
	}
	if result.Renditions != nil {
		return errors.New("lyrics recovery song result v1/v2 contains v3 renditions")
	}
	if result.SchemaVersion == SongResultSchemaVersionV1 &&
		(result.NoLyricsReason != "" || result.Game != nil || len(result.Components.GameText) != 0 || len(result.AlternateVocals) != 0 || len(result.Components.AlternateVocals) != 0) {
		return errors.New("lyrics recovery song result v1 contains v2 fields")
	}
	if requireDigest {
		if !canonicalSongResultSHA256.MatchString(result.ResultSHA256) {
			return errors.New("lyrics recovery song result digest is invalid")
		}
	} else if result.ResultSHA256 != "" {
		return errors.New("new lyrics recovery song result contains a premature digest")
	}
	if err := validateSongOutcomeRefs(result.ProviderOutcomes); err != nil {
		return err
	}
	if err := validateSongEvidenceRefs(result.SelectedEvidence); err != nil {
		return err
	}
	if err := validateSongResultAlternateVocals(result.AlternateVocals); err != nil {
		return err
	}
	if err := validateComponentEvidence(result.Components, result.SelectedEvidence, result.State, result.SchemaVersion, result.Game != nil, len(result.AlternateVocals) != 0); err != nil {
		return err
	}
	switch result.State {
	case lyricscontract.CoverageComplete:
		if result.Full == nil || result.NoLyricsReason != "" || len(result.SelectedEvidence) == 0 ||
			len(result.ProviderOutcomes) == 0 || !model.IsValidLyricsSourceVersionReasonCode(result.ReasonCode) ||
			result.ReasonCode == model.LyricsSourceVersionReasonVersionConflict {
			return errors.New("complete lyrics recovery song result is incomplete")
		}
		if err := validateSongResultLyrics(*result.Full, result.Translations, result.Components); err != nil {
			return err
		}
		if result.Game != nil {
			if err := validateSongResultLyrics(*result.Game, result.Translations, result.Components); err != nil {
				return err
			}
		}
		if err := validateGameProjection(*result.Full, result.GameProjection); err != nil {
			return err
		}
		if result.Game != nil && result.GameProjection != nil && !songResultGameMatchesProjection(*result.Game, *result.Full, *result.GameProjection) {
			return errors.New("complete lyrics recovery Game does not match its Full projection")
		}
	case lyricscontract.CoverageGameOnly:
		if result.SchemaVersion != SongResultSchemaVersionV2 || result.Full != nil || result.Game == nil ||
			result.GameProjection != nil || result.NoLyricsReason != "" || len(result.SelectedEvidence) == 0 ||
			len(result.ProviderOutcomes) == 0 ||
			(result.ReasonCode != model.LyricsSourceVersionReasonTaggedGameOnly &&
				result.ReasonCode != model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid) {
			return errors.New("Game-only lyrics recovery song result is incomplete")
		}
		if err := validateSongResultLyrics(*result.Game, result.Translations, result.Components); err != nil {
			return err
		}
	case lyricscontract.CoverageSatisfiedNoLyrics:
		if result.SchemaVersion != SongResultSchemaVersionV2 || result.NoLyricsReason != NoLyricsReasonCatalogInstrumental ||
			result.Full != nil || result.Game != nil || result.GameProjection != nil || result.Translations != nil ||
			len(result.SelectedEvidence) != 0 || len(result.ProviderOutcomes) == 0 ||
			!componentEvidenceEmpty(result.Components) || result.ReasonCode != "" {
			return errors.New("satisfied no-lyrics recovery song result is invalid")
		}
	case lyricscontract.CoverageAmbiguous, lyricscontract.CoverageMissing,
		lyricscontract.CoverageIncomplete, lyricscontract.CoverageFailed:
		if result.NoLyricsReason != "" || result.Full != nil || result.Game != nil || result.GameProjection != nil ||
			result.Translations != nil || len(result.SelectedEvidence) != 0 || !componentEvidenceEmpty(result.Components) ||
			result.ReasonCode != model.LyricsSourceVersionReasonVersionConflict {
			return errors.New("non-complete lyrics recovery song result leaked a provisional composition")
		}
	default:
		return errors.New("lyrics recovery song result has an unsupported state")
	}
	return nil
}

func validateSongResultLyrics(
	full model.LyricsSourceFull,
	translations []string,
	components ComponentEvidence,
) error {
	if err := lyricscontract.ValidatePersistedPerformerMetadata(full); err != nil {
		return fmt.Errorf("lyrics recovery song result: %w", lyricscontract.ErrUnsafePerformerMetadata)
	}
	authoritativeVocaloidSegmentation := full.Version.Kind == "vocaloid" &&
		songResultFullHasPerformerSegmentation(full) && len(components.PerformerSegmentation) != 0
	if authoritativeVocaloidSegmentation {
		if err := model.ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation(full); err != nil {
			return err
		}
	} else if err := model.ValidateLyricsSourceFull(full); err != nil {
		return err
	}
	return validateSongResultTranslations(translations, len(full.Lines))
}

func MarshalSongResult(result SongResult) ([]byte, error) {
	if err := ValidateSongResult(result); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func DecodeSongResult(body []byte) (SongResult, error) {
	if len(body) == 0 || len(body) > MaxSongResultBytes || !utf8.Valid(body) {
		return SongResult{}, errors.New("lyrics recovery song result bytes are invalid")
	}
	if err := inspectSongResultJSON(body); err != nil {
		return SongResult{}, err
	}
	var result SongResult
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return SongResult{}, fmt.Errorf("decode lyrics recovery song result: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SongResult{}, errors.New("lyrics recovery song result contains trailing JSON")
	}
	if err := ValidateSongResult(result); err != nil {
		return SongResult{}, err
	}
	canonical, _ := json.Marshal(result)
	if !bytes.Equal(canonical, body) {
		return SongResult{}, errors.New("lyrics recovery song result is not canonical JSON")
	}
	return cloneSongResult(result), nil
}

func SongResultFileName(result SongResult) (string, error) {
	if err := ValidateSongResult(result); err != nil {
		return "", err
	}
	return fmt.Sprintf("music-%d-%s.json", result.MusicID, result.ResultSHA256), nil
}

func RootSongRef(result SongResult) (lyricsrootmanifest.SongResultRef, error) {
	if err := ValidateSongResult(result); err != nil {
		return lyricsrootmanifest.SongResultRef{}, err
	}
	return lyricsrootmanifest.SongResultRef{
		MusicID: result.MusicID, State: result.State, ResultSHA256: result.ResultSHA256,
		ProviderOutcomes: append([]lyricsrootmanifest.ProviderOutcomeRef{}, result.ProviderOutcomes...),
		SelectedEvidence: cloneEvidenceRefs(result.SelectedEvidence),
	}, nil
}

func closedCoverageState(providers []ProviderReplay) lyricscontract.CoverageState {
	state := lyricscontract.CoverageMissing
	for _, provider := range providers {
		switch provider.Outcome.Status {
		case lyricsprovideroutcome.StatusTransportError:
			return lyricscontract.CoverageFailed
		case lyricsprovideroutcome.StatusAmbiguous, lyricsprovideroutcome.StatusStale:
			state = lyricscontract.CoverageAmbiguous
		case lyricsprovideroutcome.StatusUnsupported:
			if state == lyricscontract.CoverageMissing {
				state = lyricscontract.CoverageIncomplete
			}
		}
	}
	return state
}

func validateSongOutcomeRefs(refs []lyricsrootmanifest.ProviderOutcomeRef) error {
	seenProviders := make(map[model.LyricsSourceProvider]struct{}, len(refs))
	seenOutcomes := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if !model.IsValidLyricsSourceProvider(ref.Provider) || ref.OutcomeID == "" || len(ref.OutcomeID) > 256 ||
			!canonicalSongResultSHA256.MatchString(ref.SHA256) {
			return errors.New("lyrics recovery provider outcome refs are invalid")
		}
		if _, duplicate := seenProviders[ref.Provider]; duplicate {
			return errors.New("lyrics recovery provider outcome refs duplicate one evaluated provider")
		}
		if _, duplicate := seenOutcomes[ref.OutcomeID]; duplicate {
			return errors.New("lyrics recovery provider outcome refs duplicate one exact outcome")
		}
		seenProviders[ref.Provider] = struct{}{}
		seenOutcomes[ref.OutcomeID] = struct{}{}
	}
	return nil
}

func validateSongEvidenceRefs(refs []lyricscontract.EvidenceRef) error {
	last := ""
	for _, ref := range refs {
		if !model.IsValidLyricsSourceProvider(ref.Provider) ||
			!canonicalSongResultSHA256.MatchString(ref.AcquisitionID) || ref.EvidenceID == "" ||
			!canonicalSongResultSHA256.MatchString(ref.SHA256) ||
			!canonicalSongResultSHA256.MatchString(ref.EnvelopeSHA256) ||
			last != "" && last >= ref.EvidenceID {
			return errors.New("lyrics recovery selected evidence refs are invalid")
		}
		last = ref.EvidenceID
	}
	return nil
}

func validateComponentEvidence(
	components ComponentEvidence,
	selected []lyricscontract.EvidenceRef,
	state lyricscontract.CoverageState,
	schemaVersion int,
	hasGame bool,
	hasAlternateVocals bool,
) error {
	available := make(map[lyricscontract.EvidenceRef]struct{}, len(selected))
	for _, ref := range selected {
		available[ref] = struct{}{}
	}
	used := make(map[lyricscontract.EvidenceRef]struct{}, len(selected))
	for _, refs := range requiredComponentEvidenceLists(components) {
		if refs == nil {
			return errors.New("lyrics recovery component evidence must use explicit bounded arrays")
		}
		if err := bindComponentEvidenceRefs(refs, available, used); err != nil {
			return err
		}
	}
	if schemaVersion == SongResultSchemaVersionV2 && components.GameText != nil {
		if err := bindComponentEvidenceRefs(components.GameText, available, used); err != nil {
			return err
		}
	}
	if schemaVersion == SongResultSchemaVersionV2 && components.AlternateVocals != nil {
		if err := bindComponentEvidenceRefs(components.AlternateVocals, available, used); err != nil {
			return err
		}
	}
	switch state {
	case lyricscontract.CoverageComplete:
		if len(components.FullText) == 0 || hasGame != (len(components.GameText) != 0) ||
			hasAlternateVocals != (len(components.AlternateVocals) != 0) {
			return errors.New("lyrics recovery component evidence does not bind authoritative Full/Game")
		}
	case lyricscontract.CoverageGameOnly:
		if schemaVersion != SongResultSchemaVersionV2 || len(components.FullText) != 0 || len(components.GameText) == 0 ||
			hasAlternateVocals != (len(components.AlternateVocals) != 0) {
			return errors.New("lyrics recovery component evidence does not bind authoritative Game")
		}
	default:
		if !componentEvidenceEmpty(components) {
			return errors.New("lyrics recovery component evidence exists without an authoritative lyric rendition")
		}
	}
	if len(used) != len(available) {
		return errors.New("lyrics recovery selected evidence contains an unreferenced provider artifact")
	}
	return nil
}

func bindComponentEvidenceRefs(
	refs []lyricscontract.EvidenceRef,
	available map[lyricscontract.EvidenceRef]struct{},
	used map[lyricscontract.EvidenceRef]struct{},
) error {
	if err := validateSongEvidenceRefs(refs); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, found := available[ref]; !found {
			return errors.New("lyrics recovery component evidence is outside the selected exact union")
		}
		used[ref] = struct{}{}
	}
	return nil
}

func songResultTranslations(
	composition lyricscompose.FixedArtifactComposition,
	providers []ProviderReplay,
) ([]string, error) {
	sourceKey := composition.Components.FullText
	authoritative := composition.Full
	if composition.Game != nil && len(composition.Full.Lines) == 0 {
		if sourceKey != "" || composition.Components.GameText == "" {
			return nil, errors.New("lyrics recovery Game-only translation source is invalid")
		}
		sourceKey = composition.Components.GameText
		authoritative = *composition.Game
	}
	if sourceKey == "" || len(authoritative.Lines) == 0 {
		return nil, errors.New("lyrics recovery translation source has no authoritative lyric component")
	}
	for _, provider := range providers {
		if provider.Artifact.OutcomeID != sourceKey {
			continue
		}
		if provider.Fixed == nil || len(provider.Fixed.Translations) == 0 {
			return nil, nil
		}
		if len(provider.Fixed.Translations) != len(authoritative.Lines) ||
			len(provider.Fixed.Extraction.Lines) != len(authoritative.Lines) {
			return nil, errors.New("lyrics recovery translations do not align with authoritative lyric lines")
		}
		for index, line := range authoritative.Lines {
			if provider.Fixed.Extraction.Lines[index].Japanese != line.Text {
				return nil, errors.New("lyrics recovery translation source text drifted from authoritative lyric lines")
			}
		}
		translations := append([]string(nil), provider.Fixed.Translations...)
		if err := validateSongResultTranslations(translations, len(authoritative.Lines)); err != nil {
			return nil, err
		}
		return translations, nil
	}
	return nil, errors.New("lyrics recovery translation source is outside the evaluated provider prefix")
}

func validateSongResultAlternateVocals(alternates []model.LyricsSourceAlternateVocal) error {
	if alternates == nil {
		return nil
	}
	if len(alternates) > 64 {
		return errors.New("lyrics recovery alternate vocal set is unbounded")
	}
	for index, alternate := range alternates {
		if err := model.ValidateLyricsSourceAlternateVocalPayload(alternate); err != nil {
			return fmt.Errorf("lyrics recovery alternate vocal %d: %w", index+1, err)
		}
		if alternate.Provenance.VersionEvidence.RenditionKey == "" ||
			(alternate.Full != nil && alternate.Provenance.FullText == nil) ||
			(alternate.Game != nil && alternate.Provenance.GameText == nil) ||
			(alternate.GameProjection != nil && alternate.Provenance.GameProjection == nil) {
			return fmt.Errorf("lyrics recovery alternate vocal %d has incomplete provenance", index+1)
		}
	}
	return nil
}

func validateSongResultTranslations(translations []string, lineCount int) error {
	if translations == nil {
		return nil
	}
	if len(translations) == 0 || len(translations) != lineCount {
		return errors.New("lyrics recovery translations must align one-to-one with Full lines")
	}
	totalBytes := 0
	for _, translation := range translations {
		if translation == "" || strings.TrimSpace(translation) != translation || !utf8.ValidString(translation) ||
			strings.ContainsAny(translation, "\r\n\x00") || len(translation) > 16<<10 ||
			totalBytes > 2<<20-len(translation) {
			return errors.New("lyrics recovery translation text exceeds its private boundary")
		}
		totalBytes += len(translation)
	}
	return nil
}

func validateGameProjection(full model.LyricsSourceFull, projection *model.LyricsSourceGameProjection) error {
	if projection == nil {
		return nil
	}
	positions := make(map[string]int, len(full.Lines))
	for index, line := range full.Lines {
		positions[line.ID] = index
	}
	last := -1
	seen := make(map[string]struct{}, len(projection.LineIDs))
	for _, lineID := range projection.LineIDs {
		position, found := positions[lineID]
		if !found || position <= last {
			return errors.New("lyrics recovery Game projection is not an ordered Full subset")
		}
		if _, duplicate := seen[lineID]; duplicate {
			return errors.New("lyrics recovery Game projection contains duplicate line IDs")
		}
		seen[lineID] = struct{}{}
		last = position
	}
	return nil
}

func songResultGameMatchesProjection(game, full model.LyricsSourceFull, projection model.LyricsSourceGameProjection) bool {
	if len(game.Lines) != len(projection.LineIDs) {
		return false
	}
	positions := make(map[string]int, len(full.Lines))
	for index, line := range full.Lines {
		positions[line.ID] = index
	}
	for index, lineID := range projection.LineIDs {
		position, found := positions[lineID]
		if !found || game.Lines[index].Text != full.Lines[position].Text {
			return false
		}
	}
	return true
}

func songResultFullHasPerformerSegmentation(full model.LyricsSourceFull) bool {
	if len(full.Performers) != 0 {
		return true
	}
	for _, line := range full.Lines {
		if len(line.TrailingPerformerIDs) != 0 || len(line.Segments) != 1 ||
			len(line.Segments) == 1 && line.Segments[0].Text != line.Text {
			return true
		}
		for _, segment := range line.Segments {
			if len(segment.PerformerIDs) != 0 {
				return true
			}
		}
	}
	return false
}

func omitSongResultPerformerSegmentationEvidence(result *SongResult) {
	removedOnly := make(map[lyricscontract.EvidenceRef]struct{}, len(result.Components.PerformerSegmentation))
	for _, ref := range result.Components.PerformerSegmentation {
		removedOnly[ref] = struct{}{}
	}
	result.Components.PerformerSegmentation = []lyricscontract.EvidenceRef{}
	for _, refs := range [][]lyricscontract.EvidenceRef{
		result.Components.FullText,
		result.Components.GameText,
		result.Components.GameProjection,
		result.Components.Ruby,
		result.Components.VersionEvidence,
	} {
		for _, ref := range refs {
			delete(removedOnly, ref)
		}
	}
	if len(removedOnly) == 0 {
		return
	}
	selected := make([]lyricscontract.EvidenceRef, 0, len(result.SelectedEvidence))
	for _, ref := range result.SelectedEvidence {
		if _, removed := removedOnly[ref]; !removed {
			selected = append(selected, ref)
		}
	}
	result.SelectedEvidence = selected
}

func songResultDigest(result SongResult) (string, error) {
	body, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	domain := ""
	switch result.SchemaVersion {
	case SongResultSchemaVersionV1:
		domain = "moesekai-lyrics-recovery-song-result-v1\x00"
	case SongResultSchemaVersionV2:
		domain = "moesekai-lyrics-recovery-song-result-v2\x00"
	case SongResultSchemaVersionV3:
		domain = "moesekai-lyrics-recovery-song-result-v3\x00"
	default:
		return "", errors.New("lyrics recovery song result digest schema version is invalid")
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func cloneSongResult(result SongResult) SongResult {
	result.ProviderOutcomes = append([]lyricsrootmanifest.ProviderOutcomeRef{}, result.ProviderOutcomes...)
	result.SelectedEvidence = cloneEvidenceRefs(result.SelectedEvidence)
	if result.SchemaVersion == SongResultSchemaVersionV3 {
		result.Components = ComponentEvidence{}
	} else {
		result.Components = cloneComponentEvidence(result.Components)
	}
	if result.Full != nil {
		full := cloneLyricsSourceFull(*result.Full)
		result.Full = &full
	}
	if result.Game != nil {
		game := cloneLyricsSourceFull(*result.Game)
		result.Game = &game
	}
	if result.GameProjection != nil {
		projection := *result.GameProjection
		projection.LineIDs = cloneStringsPreservingNil(projection.LineIDs)
		result.GameProjection = &projection
	}
	result.AlternateVocals = model.CloneLyricsSourceAlternateVocals(result.AlternateVocals)
	result.Translations = cloneStringsPreservingNil(result.Translations)
	result.Renditions = cloneSongResultRenditions(result.Renditions)
	return result
}

func cloneLyricsSourceFull(input model.LyricsSourceFull) model.LyricsSourceFull {
	result := input
	if input.Performers == nil {
		result.Performers = nil
	} else {
		result.Performers = append([]model.LyricsSourcePerformer{}, input.Performers...)
	}
	if input.Lines == nil {
		result.Lines = nil
		return result
	}
	result.Lines = make([]model.LyricsSourceFullLine, len(input.Lines))
	for lineIndex, line := range input.Lines {
		result.Lines[lineIndex] = line
		result.Lines[lineIndex].TrailingPerformerIDs = cloneStringsPreservingNil(line.TrailingPerformerIDs)
		if line.Segments == nil {
			result.Lines[lineIndex].Segments = nil
			continue
		}
		result.Lines[lineIndex].Segments = make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			result.Lines[lineIndex].Segments[segmentIndex] = segment
			result.Lines[lineIndex].Segments[segmentIndex].PerformerIDs = cloneStringsPreservingNil(segment.PerformerIDs)
			if segment.Ruby == nil {
				result.Lines[lineIndex].Segments[segmentIndex].Ruby = nil
			} else {
				result.Lines[lineIndex].Segments[segmentIndex].Ruby = append([]model.LyricsSourceRubySpan{}, segment.Ruby...)
				for spanIndex, span := range segment.Ruby {
					if span.ReadingEvidence != nil {
						evidence := *span.ReadingEvidence
						result.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = &evidence
					}
				}
			}
		}
	}
	return result
}

func cloneStringsPreservingNil(input []string) []string {
	if input == nil {
		return nil
	}
	return append([]string{}, input...)
}

func cloneComponentEvidence(input ComponentEvidence) ComponentEvidence {
	return ComponentEvidence{
		FullText:              cloneEvidenceRefs(input.FullText),
		GameText:              cloneOptionalEvidenceRefs(input.GameText),
		AlternateVocals:       cloneOptionalEvidenceRefs(input.AlternateVocals),
		PerformerSegmentation: cloneEvidenceRefs(input.PerformerSegmentation),
		GameProjection:        cloneEvidenceRefs(input.GameProjection),
		Ruby:                  cloneEvidenceRefs(input.Ruby),
		VersionEvidence:       cloneEvidenceRefs(input.VersionEvidence),
	}
}

func cloneOptionalEvidenceRefs(input []lyricscontract.EvidenceRef) []lyricscontract.EvidenceRef {
	if input == nil {
		return nil
	}
	return cloneEvidenceRefs(input)
}

func emptyComponentEvidence() ComponentEvidence {
	return ComponentEvidence{
		FullText:              []lyricscontract.EvidenceRef{},
		GameText:              []lyricscontract.EvidenceRef{},
		PerformerSegmentation: []lyricscontract.EvidenceRef{},
		GameProjection:        []lyricscontract.EvidenceRef{},
		Ruby:                  []lyricscontract.EvidenceRef{},
		VersionEvidence:       []lyricscontract.EvidenceRef{},
	}
}

func requiredComponentEvidenceLists(input ComponentEvidence) [][]lyricscontract.EvidenceRef {
	return [][]lyricscontract.EvidenceRef{
		input.FullText,
		input.PerformerSegmentation,
		input.GameProjection,
		input.Ruby,
		input.VersionEvidence,
	}
}

func componentEvidenceEmpty(input ComponentEvidence) bool {
	for _, refs := range append(requiredComponentEvidenceLists(input), input.GameText, input.AlternateVocals) {
		if len(refs) != 0 {
			return false
		}
	}
	return true
}

func componentEvidenceZero(input ComponentEvidence) bool {
	return input.FullText == nil && input.GameText == nil && input.AlternateVocals == nil &&
		input.PerformerSegmentation == nil && input.GameProjection == nil && input.Ruby == nil &&
		input.VersionEvidence == nil
}

func inspectSongResultJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := inspectSongJSONValue(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("lyrics recovery song result contains trailing JSON")
	}
	return nil
}

func inspectSongJSONValue(decoder *json.Decoder, depth int) error {
	if depth > MaxSongResultJSONDepth {
		return errors.New("lyrics recovery song result exceeds JSON depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || forbiddenRomanizationField(key) {
				return errors.New("lyrics recovery song result contains an invalid or romanization field")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("lyrics recovery song result contains a duplicate field")
			}
			seen[key] = struct{}{}
			if err := inspectSongJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("lyrics recovery song result object is invalid")
		}
	case '[':
		for decoder.More() {
			if err := inspectSongJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("lyrics recovery song result array is invalid")
		}
	default:
		return errors.New("lyrics recovery song result delimiter is invalid")
	}
	return nil
}

func forbiddenRomanizationField(field string) bool {
	var normalized strings.Builder
	for _, current := range strings.ToLower(field) {
		if unicode.IsLetter(current) || unicode.IsNumber(current) {
			normalized.WriteRune(current)
		}
	}
	for _, forbidden := range []string{"romaji", "romanization", "romanisation", "romanized", "romanised"} {
		if strings.Contains(normalized.String(), forbidden) {
			return true
		}
	}
	return false
}
