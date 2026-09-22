package lyricsrecovery

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscompose"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsoutcomeartifact"
	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

type ProviderReplay struct {
	Outcome      lyricsprovideroutcome.Outcome[lyricssource.Candidate]
	Artifact     lyricsoutcomeartifact.Artifact
	Fixed        *lyricssource.FixedRevision
	EvidenceRefs []lyricscontract.EvidenceRef
}

type ReplayResult struct {
	MusicID             int
	Instrumental        bool
	Providers           []ProviderReplay
	Composition         *lyricscompose.FixedArtifactComposition
	Selected            []lyricscontract.EvidenceRef
	Components          ComponentEvidence
	RenditionComponents []RenditionComponentEvidence
}

type ComponentEvidence struct {
	FullText              []lyricscontract.EvidenceRef `json:"fullText"`
	GameText              []lyricscontract.EvidenceRef `json:"gameText,omitempty"`
	AlternateVocals       []lyricscontract.EvidenceRef `json:"alternateVocals,omitempty"`
	PerformerSegmentation []lyricscontract.EvidenceRef `json:"performerSegmentation"`
	GameProjection        []lyricscontract.EvidenceRef `json:"gameProjection"`
	Ruby                  []lyricscontract.EvidenceRef `json:"ruby"`
	VersionEvidence       []lyricscontract.EvidenceRef `json:"versionEvidence"`
}

type RenditionComponentEvidenceRef struct {
	Component model.LyricsSourceRenditionComponentKind `json:"component"`
	OutcomeID string                                   `json:"outcomeId"`
	Evidence  []lyricscontract.EvidenceRef             `json:"evidence"`
}

type RenditionComponentEvidence struct {
	RenditionKey string                          `json:"renditionKey"`
	Components   []RenditionComponentEvidenceRef `json:"components"`
}

func ReplaySong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	planPolicyVersion string,
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
	orderedProviders []ProviderAcquisitionSet,
) (ReplayResult, error) {
	return replaySong(ctx, musicID, identity, planPolicyVersion, runtime, ledger, orderedProviders, true)
}

// replaySong can re-evaluate parser outcomes from an exact ordered acquisition
// set without trusting its historical terminal. Production replay always sets
// requireClosedTerminal; the relaxed mode is reserved for audited offline
// rebind diagnostics that publish nothing and still consume every pinned ID.
func replaySong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	planPolicyVersion string,
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
	orderedProviders []ProviderAcquisitionSet,
	requireClosedTerminal bool,
) (ReplayResult, error) {
	if ctx == nil || musicID <= 0 || identity.MusicID != musicID || ledger == nil ||
		planPolicyVersion == "" || planPolicyVersion != runtime.PolicyVersion {
		return ReplayResult{}, errors.New("exact song replay input is invalid")
	}
	effectiveOrder, err := runtime.ProviderOrderForMusicID(musicID)
	if err != nil {
		return ReplayResult{}, err
	}
	if err := validateOrderedProviderPrefix(effectiveOrder, orderedProviders); err != nil {
		return ReplayResult{}, err
	}

	result := ReplayResult{
		MusicID: musicID, Instrumental: identity.Instrumental,
		Providers: make([]ProviderReplay, 0, len(orderedProviders)),
	}
	decisionState := providerDecisionState{}
	for index, terminal := range orderedProviders {
		provider := terminal.Provider
		if provider == lyricssource.ProviderMoegirlPublicExact {
			providerReplay, exactErr := replayExactPublicArtifact(
				ctx, musicID, identity, runtime, ledger, terminal,
			)
			if exactErr != nil {
				return ReplayResult{}, exactErr
			}
			result.Providers = append(result.Providers, providerReplay)
			decision, exactErr := decisionState.advance(providerReplay)
			if exactErr != nil {
				return ReplayResult{}, exactErr
			}
			if decision.Continue {
				return ReplayResult{}, errors.New("exact public replay did not produce a complete stopping composition")
			}
			if index+1 != len(orderedProviders) {
				return ReplayResult{}, errors.New("exact public replay declares a provider after its stopping point")
			}
			if decision.Composition == nil {
				return result, nil
			}
			return bindReplayComposition(result, *decision.Composition)
		}
		configured, err := runtimeProviderConfiguration(runtime, provider)
		if err != nil {
			return ReplayResult{}, err
		}
		var fixedSekaipedia *lyricssource.FixedIndex
		if provider == lyricssource.ProviderSekaipedia {
			fixed, found, fixedErr := runtime.SekaipediaFixedRevision(musicID)
			if fixedErr != nil {
				return ReplayResult{}, fixedErr
			}
			if found {
				configured, err = lyricssource.BindRecoverySekaipediaRevision(configured, fixed)
				if err != nil {
					return ReplayResult{}, err
				}
				fixedSekaipedia = &fixed
			}
		}
		replay, err := NewReplayTransport(ctx, provider, runtime.Authorities[provider], ledger, terminal)
		if err != nil {
			return ReplayResult{}, err
		}
		registry, err := lyricssource.NewRecoveryRegistry(
			[]lyricssource.ProviderConfig{configured},
			map[model.LyricsSourceProvider]lyricssource.RecoveryHTTPTransport{provider: replay},
		)
		if err != nil {
			return ReplayResult{}, err
		}
		outcome, err := registry.SearchProviderOutcome(ctx, provider, identity)
		if err != nil {
			return ReplayResult{}, err
		}
		if requireClosedTerminal {
			err = replay.AssertOutcomeConsumed(outcome)
		} else {
			err = replay.AssertAcquisitionsConsumed()
		}
		if err != nil {
			return ReplayResult{}, fmt.Errorf("provider %q exact replay: %w", provider, err)
		}
		acquisitions := replay.Acquisitions()
		if fixedSekaipedia != nil {
			if err := verifyPlanBoundSekaipediaSongAcquisition(acquisitions, *fixedSekaipedia); err != nil {
				return ReplayResult{}, err
			}
		}
		providerReplay, err := buildProviderReplay(registry, musicID, identity, outcome, acquisitions, runtime)
		if err != nil {
			return ReplayResult{}, err
		}
		result.Providers = append(result.Providers, providerReplay)
		decision, err := decisionState.advance(providerReplay)
		if err != nil {
			return ReplayResult{}, err
		}
		if !decision.Continue {
			if index+1 != len(orderedProviders) {
				return ReplayResult{}, errors.New("exact replay declares a provider after the closed stopping point")
			}
			if decision.Composition != nil {
				return bindReplayComposition(result, *decision.Composition)
			}
			return result, nil
		}
		if index+1 == len(orderedProviders) {
			if len(orderedProviders) != len(effectiveOrder) {
				return ReplayResult{}, errors.New("exact replay provider prefix stops before a required fallback evaluation")
			}
			return result, nil
		}
	}
	return ReplayResult{}, errors.New("exact replay evaluated no provider")
}

func applyOfflinePolicyAndComposition(result ReplayResult) (ReplayResult, error) {
	if len(result.Providers) == 0 {
		return ReplayResult{}, errors.New("recovery policy received an empty provider prefix")
	}
	decisionState := providerDecisionState{}
	for index, provider := range result.Providers {
		decision, err := decisionState.advance(provider)
		if err != nil {
			return ReplayResult{}, err
		}
		if !decision.Continue {
			if index+1 != len(result.Providers) {
				return ReplayResult{}, errors.New("recovery policy received an evaluated provider after its stopping point")
			}
			if decision.Composition != nil {
				return bindReplayComposition(result, *decision.Composition)
			}
			return result, nil
		}
	}
	return result, nil
}

func bindReplayComposition(
	result ReplayResult,
	composition lyricscompose.FixedArtifactComposition,
) (ReplayResult, error) {
	providersBySource := make(map[string]ProviderReplay, len(result.Providers))
	for _, provider := range result.Providers {
		if _, duplicate := providersBySource[provider.Artifact.OutcomeID]; duplicate {
			return ReplayResult{}, errors.New("recovery composition source key is duplicated")
		}
		providersBySource[provider.Artifact.OutcomeID] = provider
	}
	selectedByEvidence := make(map[string]lyricscontract.EvidenceRef)
	for _, sourceKey := range composition.SelectedSourceKeys {
		provider, found := providersBySource[sourceKey]
		if !found || provider.Outcome.Status != lyricsprovideroutcome.StatusCandidate || provider.Fixed == nil {
			return ReplayResult{}, errors.New("recovery composition selected an unavailable provider artifact")
		}
		for _, ref := range provider.EvidenceRefs {
			if existing, duplicate := selectedByEvidence[ref.EvidenceID]; duplicate && existing != ref {
				return ReplayResult{}, errors.New("recovery composition selected conflicting exact evidence")
			}
			selectedByEvidence[ref.EvidenceID] = ref
		}
	}
	selected := make([]lyricscontract.EvidenceRef, 0, len(selectedByEvidence))
	for _, ref := range selectedByEvidence {
		selected = append(selected, ref)
	}
	sort.Slice(selected, func(left, right int) bool { return selected[left].EvidenceID < selected[right].EvidenceID })
	result.Composition = &composition
	result.Selected = selected
	if len(composition.Renditions) != 0 {
		if composition.ReasonCode != "" || len(composition.Full.Lines) != 0 || composition.Game != nil ||
			composition.GameProjection != nil || len(composition.AlternateVocals) != 0 || composition.PrivateReview != nil ||
			composition.Components != (lyricscompose.FixedArtifactComponents{}) || !componentEvidenceZero(result.Components) ||
			result.RenditionComponents != nil {
			return ReplayResult{}, errors.New("recovery peer rendition composition contains legacy or pre-bound components")
		}
		renditionComponents, err := renditionComponentEvidenceForComposition(composition, result.Providers)
		if err != nil {
			return ReplayResult{}, err
		}
		result.RenditionComponents = renditionComponents
		result.Components = ComponentEvidence{}
	} else {
		result.Components = componentEvidenceForComposition(composition, result.Providers)
		result.RenditionComponents = nil
	}
	return result, nil
}

// AllowsFallback is the pure closed recovery-v2 authority policy. Retryable
// transport failures and malformed parser/contract/finalization terminals never
// become deterministic absence.
func AllowsFallback(
	provider model.LyricsSourceProvider,
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
) bool {
	if outcome.Status == lyricsprovideroutcome.StatusCandidate || len(outcome.Candidates) != 0 || outcome.Validate() != nil {
		return false
	}
	switch provider {
	case lyricssource.ProviderSekaipedia:
		switch outcome.Status {
		case lyricsprovideroutcome.StatusNoMatch, lyricsprovideroutcome.StatusStale,
			lyricsprovideroutcome.StatusAmbiguous:
			return true
		case lyricsprovideroutcome.StatusUnsupported:
			if outcome.Diagnostic.Phase != lyricsprovideroutcome.PhaseParseLyrics {
				return false
			}
			switch outcome.Diagnostic.ReasonCode {
			case lyricsprovideroutcome.ReasonUnsupportedFormat, lyricsprovideroutcome.ReasonLyricsTooLarge,
				lyricsprovideroutcome.ReasonCatalogRenditionConflict:
				return true
			}
		}
	case lyricssource.ProviderMoegirl:
		if outcome.Diagnostic.Counts.Candidates != 0 {
			return false
		}
		if outcome.Status == lyricsprovideroutcome.StatusNoMatch {
			return true
		}
		if outcome.Status == lyricsprovideroutcome.StatusUnsupported &&
			outcome.Diagnostic.Phase == lyricsprovideroutcome.PhaseParseLyrics {
			switch outcome.Diagnostic.ReasonCode {
			case lyricsprovideroutcome.ReasonUnsupportedFormat, lyricsprovideroutcome.ReasonLyricsTooLarge,
				lyricsprovideroutcome.ReasonCatalogRenditionConflict:
				return true
			}
		}
	}
	return false
}

func replayReferences(
	acquired []lyricsacquisition.Acquisition,
) ([]lyricscontract.EvidenceRef, []lyricsoutcomeartifact.AcquisitionRef, error) {
	evidenceRefs := make([]lyricscontract.EvidenceRef, len(acquired))
	artifactRefs := make([]lyricsoutcomeartifact.AcquisitionRef, len(acquired))
	for index, item := range acquired {
		ref, err := lyricsevidencepack.EvidenceRefFromAcquisition(item)
		if err != nil {
			return nil, nil, err
		}
		evidenceRefs[index] = ref
		artifactRefs[index] = lyricsoutcomeartifact.AcquisitionRef{
			AcquisitionID: ref.AcquisitionID, EvidenceID: ref.EvidenceID,
			SHA256: ref.SHA256, EnvelopeSHA256: ref.EnvelopeSHA256,
		}
	}
	sort.Slice(evidenceRefs, func(left, right int) bool {
		return evidenceRefs[left].EvidenceID < evidenceRefs[right].EvidenceID
	})
	sort.Slice(artifactRefs, func(left, right int) bool {
		return artifactRefs[left].EvidenceID < artifactRefs[right].EvidenceID
	})
	return evidenceRefs, artifactRefs, nil
}

func outcomeReferencesResolved(
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
	refs []lyricscontract.EvidenceRef,
) error {
	available := make(map[string]string, len(refs))
	for _, ref := range refs {
		if previous, duplicate := available[ref.EvidenceID]; duplicate && previous != ref.SHA256 {
			return errors.New("replayed evidence ID resolves to conflicting SHA-256 values")
		}
		available[ref.EvidenceID] = ref.SHA256
	}
	for _, required := range outcome.Diagnostic.AcquisitionRefs {
		if available[required.EvidenceID] != required.SHA256 {
			return errors.New("provider outcome references an unconsumed or conflicting acquisition")
		}
	}
	return nil
}

func componentEvidenceForComposition(
	composition lyricscompose.FixedArtifactComposition,
	providers []ProviderReplay,
) ComponentEvidence {
	bySource := make(map[string][]lyricscontract.EvidenceRef, len(providers))
	for _, provider := range providers {
		bySource[provider.Artifact.OutcomeID] = append([]lyricscontract.EvidenceRef(nil), provider.EvidenceRefs...)
	}
	return ComponentEvidence{
		FullText:              cloneEvidenceRefs(bySource[composition.Components.FullText]),
		GameText:              cloneEvidenceRefs(bySource[composition.Components.GameText]),
		AlternateVocals:       cloneEvidenceRefs(bySource[composition.Components.AlternateVocals]),
		PerformerSegmentation: cloneEvidenceRefs(bySource[composition.Components.PerformerSegmentation]),
		GameProjection:        cloneEvidenceRefs(bySource[composition.Components.GameProjection]),
		Ruby:                  cloneEvidenceRefs(bySource[composition.Components.Ruby]),
		VersionEvidence:       cloneEvidenceRefs(bySource[composition.Components.VersionEvidence]),
	}
}

func renditionComponentEvidenceForComposition(
	composition lyricscompose.FixedArtifactComposition,
	providers []ProviderReplay,
) ([]RenditionComponentEvidence, error) {
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(composition.Renditions)
	if err != nil {
		return nil, fmt.Errorf("recovery peer rendition components: %w", err)
	}
	bySource := make(map[string]ProviderReplay, len(providers))
	for _, provider := range providers {
		if provider.Artifact.OutcomeID == "" {
			continue
		}
		bySource[provider.Artifact.OutcomeID] = provider
	}
	result := make([]RenditionComponentEvidence, 0, len(composition.Renditions))
	byRendition := make(map[string]int, len(composition.Renditions))
	for _, binding := range bindings {
		provider, found := bySource[binding.FixedIdentityKey]
		if !found || provider.Outcome.Status != lyricsprovideroutcome.StatusCandidate || provider.Fixed == nil ||
			len(provider.EvidenceRefs) == 0 {
			return nil, fmt.Errorf("recovery rendition component %q selected an unavailable exact artifact", binding.ComponentKey)
		}
		resultIndex, found := byRendition[binding.RenditionKey]
		if !found {
			resultIndex = len(result)
			byRendition[binding.RenditionKey] = resultIndex
			result = append(result, RenditionComponentEvidence{RenditionKey: binding.RenditionKey})
		}
		result[resultIndex].Components = append(result[resultIndex].Components, RenditionComponentEvidenceRef{
			Component: binding.Component, OutcomeID: provider.Artifact.OutcomeID,
			Evidence: cloneEvidenceRefs(provider.EvidenceRefs),
		})
	}
	return result, nil
}

func cloneRenditionComponentEvidence(input []RenditionComponentEvidence) []RenditionComponentEvidence {
	if input == nil {
		return nil
	}
	result := make([]RenditionComponentEvidence, len(input))
	for index, rendition := range input {
		result[index] = RenditionComponentEvidence{
			RenditionKey: rendition.RenditionKey,
			Components:   make([]RenditionComponentEvidenceRef, len(rendition.Components)),
		}
		for componentIndex, component := range rendition.Components {
			result[index].Components[componentIndex] = component
			result[index].Components[componentIndex].Evidence = cloneEvidenceRefs(component.Evidence)
		}
	}
	return result
}

func cloneEvidenceRefs(input []lyricscontract.EvidenceRef) []lyricscontract.EvidenceRef {
	return append([]lyricscontract.EvidenceRef{}, input...)
}
