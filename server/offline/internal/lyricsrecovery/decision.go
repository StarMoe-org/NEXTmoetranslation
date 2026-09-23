package lyricsrecovery

import (
	"errors"
	"fmt"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricscompose"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

type providerDecision struct {
	Continue    bool
	Composition *lyricscompose.FixedArtifactComposition
}

type providerDecisionState struct {
	inputs []lyricscompose.FixedArtifactInput
}

// advance is the one closed recovery-v2 provider stopping model used by live
// acquisition and exact offline replay. Candidate finalization has already
// happened from captured evidence only. The only candidate state that may
// advance is an explicitly incomplete Full/version composition.
func (state *providerDecisionState) advance(provider ProviderReplay) (providerDecision, error) {
	outcome := provider.Outcome
	if err := outcome.Validate(); err != nil || provider.Artifact.Provider != outcome.Provider {
		return providerDecision{}, errors.New("recovery provider decision input is invalid")
	}
	if outcome.Status != lyricsprovideroutcome.StatusCandidate {
		if provider.Fixed != nil || provider.Artifact.Candidate != nil {
			return providerDecision{}, errors.New("non-candidate recovery terminal contains candidate state")
		}
		return providerDecision{Continue: AllowsFallback(outcome.Provider, outcome)}, nil
	}
	if provider.Fixed == nil || provider.Artifact.Candidate == nil || provider.Artifact.OutcomeID == "" {
		return providerDecision{}, errors.New("candidate recovery terminal has no exact fixed artifact")
	}
	state.inputs = append(state.inputs, lyricscompose.FixedArtifactInput{
		SourceKey:           provider.Artifact.OutcomeID,
		LogicalRenditionKey: provider.Artifact.Candidate.RenditionKey,
		Fixed:               *provider.Fixed,
	})
	composition, err := lyricscompose.ComposeFixedArtifacts(state.inputs)
	if err == nil {
		return providerDecision{Composition: &composition}, nil
	}
	if errors.Is(err, lyricscompose.ErrComponentsIncomplete) {
		return providerDecision{Continue: true}, nil
	}
	return providerDecision{}, fmt.Errorf("recovery candidate composition failed closed: %w", err)
}

func buildProviderReplay(
	registry *lyricssource.Registry,
	musicID int,
	identity lyricssource.MusicIdentity,
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
	acquired []lyricsacquisition.Acquisition,
	runtime RuntimeConfig,
) (ProviderReplay, error) {
	if registry == nil || musicID <= 0 || identity.MusicID != musicID || outcome.Validate() != nil || runtime.PolicyVersion == "" {
		return ProviderReplay{}, errors.New("recovery provider replay input is invalid")
	}
	parserVersion := runtime.Parsers[outcome.Provider]
	if parserVersion == "" {
		return ProviderReplay{}, errors.New("recovery provider parser version is missing")
	}
	evidenceRefs, artifactRefs, err := replayReferences(acquired)
	if err != nil {
		return ProviderReplay{}, err
	}
	if err := outcomeReferencesResolved(outcome, evidenceRefs); err != nil {
		return ProviderReplay{}, err
	}

	var fixed *lyricssource.FixedRevision
	var compactCandidate *lyricsoutcomeartifact.CandidateIdentity
	if outcome.Status == lyricsprovideroutcome.StatusCandidate {
		if len(outcome.Candidates) != 1 {
			return ProviderReplay{}, errors.New("candidate recovery outcome is not singular")
		}
		finalized, finalizeErr := registry.FinalizeRecoveryCandidate(identity, outcome.Candidates[0])
		if finalizeErr != nil {
			return ProviderReplay{}, fmt.Errorf("finalize provider %q exact candidate: %w", outcome.Provider, finalizeErr)
		}
		fixed = &finalized
		candidate := outcome.Candidates[0]
		compactCandidate = &lyricsoutcomeartifact.CandidateIdentity{
			PageID: candidate.PageID, RevisionID: candidate.RevisionID, SHA1: candidate.SHA1,
			RawSHA256: candidate.RawSHA256, RenditionKey: candidate.RenditionKey,
			VersionReason: candidate.VersionReason, LineCount: len(finalized.Extraction.Lines),
		}
	}

	artifact, err := lyricsoutcomeartifact.New(
		musicID, outcome.Provider, outcome.Status, outcome.Diagnostic.ReasonCode, outcome.Diagnostic.Phase,
		outcome.Diagnostic.Counts, parserVersion, runtime.PolicyVersion, compactCandidate, artifactRefs,
	)
	if err != nil {
		return ProviderReplay{}, fmt.Errorf("build provider %q compact outcome artifact: %w", outcome.Provider, err)
	}
	return ProviderReplay{Outcome: outcome, Artifact: artifact, Fixed: fixed, EvidenceRefs: evidenceRefs}, nil
}

func runtimeProviderConfiguration(
	runtime RuntimeConfig,
	provider model.LyricsSourceProvider,
) (lyricssource.ProviderConfig, error) {
	var configured lyricssource.ProviderConfig
	matches := 0
	for _, candidate := range runtime.Providers {
		if candidate.Provider == provider {
			configured = candidate
			matches++
		}
	}
	if matches != 1 || runtime.Parsers[provider] == "" {
		return lyricssource.ProviderConfig{}, errors.New("recovery runtime provider configuration is not exact")
	}
	return configured, nil
}

func validateOrderedProviderPrefix(
	order []model.LyricsSourceProvider,
	providers []ProviderAcquisitionSet,
) error {
	if len(order) == 0 || len(providers) == 0 || len(providers) > len(order) {
		return errors.New("recovery provider evaluation must be a non-empty plan prefix")
	}
	seen := make(map[model.LyricsSourceProvider]struct{}, len(order))
	for index, provider := range order {
		if !model.IsValidLyricsSourceProvider(provider) {
			return errors.New("recovery provider order is invalid")
		}
		if _, duplicate := seen[provider]; duplicate {
			return errors.New("recovery provider order contains a duplicate")
		}
		seen[provider] = struct{}{}
		if index < len(providers) && (providers[index].Provider != provider || providers[index].AcquisitionIDs == nil) {
			return errors.New("recovery provider evaluation is gapped, reordered, or not a plan prefix")
		}
	}
	return nil
}

// validateOrderedProviderSelection is the plan-independent acquisition-set
// shape check. It permits a scoped song to start at its assigned provider while
// retaining global order. Exact prefix authorization is enforced separately
// against the immutable per-song provider scope.
func validateOrderedProviderSelection(
	order []model.LyricsSourceProvider,
	providers []ProviderAcquisitionSet,
) error {
	if len(order) == 0 || len(providers) == 0 || len(providers) > len(order) {
		return errors.New("recovery provider evaluation must be a non-empty ordered selection")
	}
	positions := make(map[model.LyricsSourceProvider]int, len(order))
	for index, provider := range order {
		if !model.IsValidLyricsSourceProvider(provider) {
			return errors.New("recovery provider order is invalid")
		}
		if _, duplicate := positions[provider]; duplicate {
			return errors.New("recovery provider order contains a duplicate")
		}
		positions[provider] = index
	}
	lastPosition := -1
	for _, provider := range providers {
		position, found := positions[provider.Provider]
		if !found || position <= lastPosition || provider.AcquisitionIDs == nil {
			return errors.New("recovery provider evaluation is reordered, duplicated, or outside the plan order")
		}
		lastPosition = position
	}
	return nil
}
