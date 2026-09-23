package lyricsrecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

type ProviderOutcomeEnterResult string

const (
	ProviderOutcomeCompleteCompositionStop ProviderOutcomeEnterResult = "complete_composition_stop"
	ProviderOutcomeCandidateFreeFallback   ProviderOutcomeEnterResult = "candidate_free_fallback"
	ProviderOutcomeIncompleteContinue      ProviderOutcomeEnterResult = "incomplete_composition_continue"
	ProviderOutcomeFailClosed              ProviderOutcomeEnterResult = "fail_closed"

	SekaipediaCanaryDiagnosticSchemaV2 = "sekaipedia-live-canary-diagnostic/v2"
	MaxSekaipediaCanaryDiagnosticBytes = 8 << 10
)

var ErrSekaipediaCanaryTerminal = errors.New("Sekaipedia live canary did not reach an exact complete composition stop")

type SekaipediaCanaryRevisionDiagnostic struct {
	AcquisitionID     lyricsacquisition.AcquisitionID `json:"acquisitionId"`
	EvidenceID        string                          `json:"evidenceId"`
	PageID            int                             `json:"pageId"`
	RevisionID        int                             `json:"revisionId"`
	RevisionTimestamp string                          `json:"revisionTimestamp"`
	SHA1              string                          `json:"sha1"`
	ContentSHA256     string                          `json:"contentSha256"`
	RawResponseSHA256 string                          `json:"rawResponseSha256"`
}

type SekaipediaCanaryDiagnostic struct {
	SchemaVersion      string                              `json:"schemaVersion"`
	RecoveryPlanID     string                              `json:"recoveryPlanId"`
	RecoveryPlanSHA256 string                              `json:"recoveryPlanSha256"`
	MusicID            int                                 `json:"musicId"`
	Provider           model.LyricsSourceProvider          `json:"provider"`
	Status             lyricsprovideroutcome.Status        `json:"status"`
	ReasonCode         lyricsprovideroutcome.ReasonCode    `json:"reasonCode"`
	Phase              lyricsprovideroutcome.Phase         `json:"phase"`
	Counts             lyricsprovideroutcome.Counts        `json:"counts"`
	EnterResult        ProviderOutcomeEnterResult          `json:"enterResult"`
	FallbackReasonCode lyricsprovideroutcome.ReasonCode    `json:"fallbackReasonCode"`
	ForensicResponses  []ForensicResponseRef               `json:"forensicResponses"`
	List               *SekaipediaCanaryRevisionDiagnostic `json:"list,omitempty"`
	Song               *SekaipediaCanaryRevisionDiagnostic `json:"song,omitempty"`
}

type AcquisitionProgress struct {
	MusicID            int
	Provider           model.LyricsSourceProvider
	AcquisitionCount   int
	Status             lyricsprovideroutcome.Status
	ReasonCode         lyricsprovideroutcome.ReasonCode
	Phase              lyricsprovideroutcome.Phase
	EnterResult        ProviderOutcomeEnterResult
	FallbackReasonCode lyricsprovideroutcome.ReasonCode
	Retryable          bool
	ForensicResponses  []ForensicResponseRef
	SekaipediaCanary   *SekaipediaCanaryDiagnostic
}

// AcquisitionSession retains only provider-wide request safety across songs.
// Every song still receives fresh transports, provider instances, and caches,
// so no candidate or fixed-authority result can cross a song boundary.
type AcquisitionSession struct {
	runtime        RuntimeConfig
	ledger         *lyricsacquisition.Ledger
	liveTransports map[model.LyricsSourceProvider]http.RoundTripper
	providerSafety map[model.LyricsSourceProvider]*lyricssource.RecoveryProviderSafety
}

func NewAcquisitionSession(
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
	liveTransports map[model.LyricsSourceProvider]http.RoundTripper,
) (*AcquisitionSession, error) {
	if ledger == nil || liveTransports == nil || runtime.PolicyVersion == "" || runtime.MaxAttempts < 1 ||
		runtime.RequestTimeout <= 0 || runtime.RetryDelay < 0 || runtime.MaxActualNetworkInFlight != 1 ||
		runtime.MediaWikiMaxlag != 5 || len(runtime.Order) == 0 {
		return nil, errors.New("lyrics recovery acquisition session input is invalid")
	}
	live := make(map[model.LyricsSourceProvider]http.RoundTripper, len(runtime.Order))
	safety := make(map[model.LyricsSourceProvider]*lyricssource.RecoveryProviderSafety, len(runtime.Order))
	seen := make(map[model.LyricsSourceProvider]struct{}, len(runtime.Order))
	for _, provider := range runtime.Order {
		if !model.IsValidLyricsSourceProvider(provider) {
			return nil, errors.New("lyrics recovery acquisition provider order is invalid")
		}
		if _, duplicate := seen[provider]; duplicate {
			return nil, errors.New("lyrics recovery acquisition provider order contains a duplicate")
		}
		if _, err := runtimeProviderConfiguration(runtime, provider); err != nil {
			return nil, err
		}
		seen[provider] = struct{}{}
		if provider == lyricssource.ProviderMoegirlPublicExact {
			if liveTransports[provider] != nil {
				return nil, errors.New("moegirl_public_exact must not receive a live or ICU transport")
			}
			continue
		}
		if liveTransports[provider] == nil {
			return nil, errors.New("lyrics recovery acquisition requires one explicit transport per network provider")
		}
		live[provider] = liveTransports[provider]
		safety[provider] = lyricssource.NewRecoveryProviderSafety()
	}
	return &AcquisitionSession{
		runtime: cloneRuntimeConfig(runtime), ledger: ledger,
		liveTransports: live, providerSafety: safety,
	}, nil
}

// AcquireSong evaluates only the non-empty provider prefix required by the
// closed policy/composition decision. Every bounded completed response is
// durably retained before status classification or parsing; semantically valid
// successes additionally enter the strict acquisition ledger. No transport
// after the stopping point is constructed or called.
func (session *AcquisitionSession) AcquireSong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
) ([]ProviderAcquisitionSet, []AcquisitionProgress, error) {
	return session.acquireSong(ctx, musicID, identity, nil)
}

// AcquireSekaipediaCanarySong runs one exact plan-bound Sekaipedia-only
// canary and returns its bounded, content-free terminal diagnostic. It never
// contacts a fallback provider. Callers may report PASS only when
// SekaipediaCanaryCompleteCompositionStop accepts the exact revision evidence.
func (session *AcquisitionSession) AcquireSekaipediaCanarySong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
) ([]ProviderAcquisitionSet, []AcquisitionProgress, SekaipediaCanaryDiagnostic, error) {
	if session == nil || session.runtime.SekaipediaCanary == nil ||
		len(session.runtime.Authorities[lyricssource.ProviderSekaipedia]) != 1 ||
		session.runtime.SekaipediaCanary.RecoveryPlanID != session.runtime.RecoveryPlanID ||
		session.runtime.SekaipediaCanary.RecoveryPlanSHA256 != session.runtime.RecoveryPlanSHA256 ||
		session.runtime.SekaipediaCanary.List != session.runtime.Authorities[lyricssource.ProviderSekaipedia][0] {
		return nil, nil, SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary runtime is not bound to the recovery plan")
	}
	target, found := session.runtime.SekaipediaCanary.song(musicID)
	if !found {
		return nil, nil, SekaipediaCanaryDiagnostic{}, errors.New("music ID is not selected by the Sekaipedia canary plan")
	}
	sets, progress, err := session.acquireSong(ctx, musicID, identity, &target)
	if err != nil {
		return nil, nil, SekaipediaCanaryDiagnostic{}, err
	}
	if len(progress) != 1 || progress[0].Provider != lyricssource.ProviderSekaipedia ||
		progress[0].SekaipediaCanary == nil {
		return nil, nil, SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary did not produce one first-provider terminal diagnostic")
	}
	return sets, progress, *progress[0].SekaipediaCanary, nil
}

func (session *AcquisitionSession) acquireSong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	canary *SekaipediaCanarySongPlan,
) ([]ProviderAcquisitionSet, []AcquisitionProgress, error) {
	if session == nil || ctx == nil || musicID <= 0 || identity.MusicID != musicID || session.ledger == nil ||
		(canary != nil && (canary.MusicID != musicID || identity.JapaneseTitle != canary.CatalogTitle)) {
		return nil, nil, errors.New("lyrics recovery acquisition input is invalid")
	}
	runtime := session.runtime
	effectiveOrder, err := runtime.ProviderOrderForMusicID(musicID)
	if err != nil {
		return nil, nil, err
	}
	if canary != nil {
		if len(effectiveOrder) == 0 || effectiveOrder[0] != lyricssource.ProviderSekaipedia ||
			(len(runtime.ProviderMusicIDs) > 0 && len(effectiveOrder) != 1) {
			return nil, nil, errors.New("Sekaipedia canary music ID is not assigned to Sekaipedia")
		}
		effectiveOrder = effectiveOrder[:1]
	}
	sets := make([]ProviderAcquisitionSet, 0, len(effectiveOrder))
	progress := make([]AcquisitionProgress, 0, len(effectiveOrder))
	decisionState := providerDecisionState{}

	for _, provider := range effectiveOrder {
		if provider == lyricssource.ProviderMoegirlPublicExact {
			terminal, providerReplay, exactErr := acquireExactPublicArtifact(
				ctx, musicID, identity, runtime, session.ledger,
			)
			if exactErr != nil {
				return nil, nil, exactErr
			}
			decision, exactErr := decisionState.advance(providerReplay)
			if exactErr != nil {
				return nil, nil, exactErr
			}
			enterResult, fallbackReason, exactErr := providerOutcomeEnterResult(providerReplay, decision)
			if exactErr != nil {
				return nil, nil, exactErr
			}
			sets = append(sets, terminal)
			progress = append(progress, AcquisitionProgress{
				MusicID: musicID, Provider: provider, AcquisitionCount: len(terminal.AcquisitionIDs),
				Status: terminal.Status, ReasonCode: terminal.ReasonCode, Phase: terminal.Phase,
				EnterResult: enterResult, FallbackReasonCode: fallbackReason, Retryable: false,
				ForensicResponses: []ForensicResponseRef{},
			})
			if decision.Continue {
				return nil, nil, errors.New("exact public artifact did not produce a complete stopping composition")
			}
			return sets, progress, nil
		}
		configured, err := runtimeProviderConfiguration(runtime, provider)
		if err != nil {
			return nil, nil, err
		}
		if canary != nil {
			if provider != lyricssource.ProviderSekaipedia {
				return nil, nil, errors.New("Sekaipedia canary attempted to configure a fallback provider")
			}
			configured, err = lyricssource.BindRecoverySekaipediaRevision(configured, lyricssource.FixedIndex{
				PageID: canary.PageID, RevisionID: canary.RevisionID,
				RevisionTimestamp: canary.RevisionTimestamp, SHA1: canary.SHA1,
				ContentSHA256: canary.ContentSHA256, Title: canary.ProviderTitle,
			})
			if err != nil {
				return nil, nil, err
			}
		} else if provider == lyricssource.ProviderSekaipedia {
			fixed, found, fixedErr := runtime.SekaipediaFixedRevision(musicID)
			if fixedErr != nil {
				return nil, nil, fixedErr
			}
			if found {
				configured, err = lyricssource.BindRecoverySekaipediaRevision(configured, fixed)
				if err != nil {
					return nil, nil, err
				}
			}
		}
		transport, err := NewAcquisitionTransport(
			provider, runtime.Authorities[provider], session.ledger, session.liveTransports[provider],
		)
		if err != nil {
			return nil, nil, err
		}
		registry, err := lyricssource.NewRecoveryRegistryWithProviderSafety(
			[]lyricssource.ProviderConfig{configured},
			map[model.LyricsSourceProvider]lyricssource.RecoveryHTTPTransport{provider: transport},
			map[model.LyricsSourceProvider]*lyricssource.RecoveryProviderSafety{provider: session.providerSafety[provider]},
		)
		if err != nil {
			return nil, nil, err
		}

		var outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate]
		maxAttempts := runtime.MaxAttempts
		if canary != nil {
			maxAttempts = 1
		}
		for attempt := 0; attempt < maxAttempts; attempt++ {
			attemptContext, cancel := context.WithTimeout(ctx, runtime.RequestTimeout)
			outcome, err = registry.SearchProviderOutcome(attemptContext, provider, identity)
			cancel()
			if err != nil {
				return nil, nil, err
			}
			if !outcome.Retryable() || attempt+1 == maxAttempts {
				break
			}
			if err := waitRecoveryRetry(ctx, runtime.RetryDelay); err != nil {
				return nil, nil, err
			}
		}

		committed := transport.Committed()
		if provider == lyricssource.ProviderSekaipedia && canary == nil {
			fixed, found, fixedErr := runtime.SekaipediaFixedRevision(musicID)
			if fixedErr != nil {
				return nil, nil, fixedErr
			}
			if found {
				if fixedErr := verifyPlanBoundSekaipediaSongAcquisition(committed, fixed); fixedErr != nil {
					return nil, nil, fixedErr
				}
			}
		}
		forensicResponses := transport.ForensicResponses()
		ids := make([]lyricsacquisition.AcquisitionID, len(committed))
		for index, acquisition := range committed {
			ids[index] = acquisition.AcquisitionID
		}
		terminal := ProviderAcquisitionSet{
			Provider: provider, AcquisitionIDs: ids, Status: outcome.Status,
			ReasonCode: outcome.Diagnostic.ReasonCode, Phase: outcome.Diagnostic.Phase,
			Counts: outcome.Diagnostic.Counts,
		}
		exact, err := replayCommittedAcquisitions(ctx, session.ledger, committed)
		if err != nil {
			return nil, nil, err
		}
		providerReplay, err := buildProviderReplay(registry, musicID, identity, outcome, exact, runtime)
		if err != nil {
			return nil, nil, err
		}
		decision, err := decisionState.advance(providerReplay)
		if err != nil {
			return nil, nil, err
		}
		var enterResult ProviderOutcomeEnterResult
		var fallbackReason lyricsprovideroutcome.ReasonCode
		if canary != nil && outcome.Status == lyricsprovideroutcome.StatusStale &&
			outcome.Diagnostic.ReasonCode == lyricsprovideroutcome.ReasonRevisionChanged {
			decision.Continue = false
			enterResult = ProviderOutcomeFailClosed
		} else {
			enterResult, fallbackReason, err = providerOutcomeEnterResult(providerReplay, decision)
			if err != nil {
				return nil, nil, err
			}
		}
		item := AcquisitionProgress{
			MusicID: musicID, Provider: provider, AcquisitionCount: len(ids),
			Status: outcome.Status, ReasonCode: outcome.Diagnostic.ReasonCode,
			Phase: outcome.Diagnostic.Phase, EnterResult: enterResult,
			FallbackReasonCode: fallbackReason, Retryable: outcome.Retryable(),
			ForensicResponses: append([]ForensicResponseRef(nil), forensicResponses...),
		}
		if canary != nil && provider == lyricssource.ProviderSekaipedia {
			diagnostic, diagnosticErr := buildSekaipediaCanaryDiagnostic(
				session.runtime.RecoveryPlanID, session.runtime.RecoveryPlanSHA256,
				identity, session.runtime.SekaipediaCanary.List, *canary,
				outcome, committed, forensicResponses, providerReplay, decision, enterResult, fallbackReason,
			)
			if diagnosticErr != nil {
				return nil, nil, diagnosticErr
			}
			item.SekaipediaCanary = &diagnostic
		}
		sets = append(sets, terminal)
		progress = append(progress, item)
		if canary != nil || !decision.Continue {
			return sets, progress, nil
		}
	}
	return sets, progress, nil
}

func verifyPlanBoundSekaipediaSongAcquisition(
	committed []lyricsacquisition.Acquisition,
	fixed lyricssource.FixedIndex,
) error {
	if fixed.RawSHA256 == "" {
		return errors.New("plan-bound Sekaipedia song revision has no raw response digest")
	}
	matches := 0
	for _, acquired := range committed {
		if acquired.Request.Provider != string(lyricssource.ProviderSekaipedia) ||
			acquired.Request.RevisionSelector != "oldid:"+strconv.Itoa(fixed.RevisionID) {
			continue
		}
		matches++
		if acquired.RawResponseSHA256 != fixed.RawSHA256 ||
			lyricssource.VerifySekaipediaRevisionContent(acquired.RawResponse, fixed) != nil {
			return errors.New("acquired Sekaipedia song bytes do not match the plan-pinned fixed response")
		}
	}
	if matches != 1 {
		return errors.New("plan-bound Sekaipedia song acquisition is missing or duplicated")
	}
	return nil
}

func providerOutcomeEnterResult(
	provider ProviderReplay,
	decision providerDecision,
) (ProviderOutcomeEnterResult, lyricsprovideroutcome.ReasonCode, error) {
	outcome := provider.Outcome
	if outcome.Validate() != nil || provider.Artifact.Provider != outcome.Provider ||
		(decision.Composition != nil && decision.Continue) {
		return "", "", errors.New("provider outcome enter result is invalid")
	}
	if outcome.Status == lyricsprovideroutcome.StatusCandidate {
		if decision.Composition != nil && !decision.Continue {
			return ProviderOutcomeCompleteCompositionStop, "", nil
		}
		if outcome.Provider == lyricssource.ProviderSekaipedia {
			return "", "", errors.New("Sekaipedia candidate did not enter a complete stopping composition")
		}
		if decision.Composition == nil && decision.Continue {
			return ProviderOutcomeIncompleteContinue, "", nil
		}
		return "", "", errors.New("candidate provider outcome has no closed enter result")
	}
	if decision.Composition != nil || provider.Fixed != nil || provider.Artifact.Candidate != nil {
		return "", "", errors.New("candidate-free provider outcome entered candidate state")
	}
	if outcome.Provider == lyricssource.ProviderSekaipedia {
		reason, allowed := SekaipediaFallbackReasonCode(outcome)
		if decision.Continue != allowed {
			return "", "", errors.New("Sekaipedia fallback decision conflicts with its closed reason policy")
		}
		if allowed {
			return ProviderOutcomeCandidateFreeFallback, reason, nil
		}
		return ProviderOutcomeFailClosed, "", nil
	}
	if decision.Continue {
		return ProviderOutcomeCandidateFreeFallback, outcome.Diagnostic.ReasonCode, nil
	}
	return ProviderOutcomeFailClosed, "", nil
}

// SekaipediaFallbackReasonCode returns only the closed candidate-free reasons
// that may advance from Sekaipedia to the next provider. Transport, malformed,
// restricted/denied, and canceled outcomes intentionally return false.
func SekaipediaFallbackReasonCode(
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
) (lyricsprovideroutcome.ReasonCode, bool) {
	if outcome.Provider != lyricssource.ProviderSekaipedia || outcome.Validate() != nil ||
		outcome.Status == lyricsprovideroutcome.StatusCandidate || len(outcome.Candidates) != 0 ||
		outcome.Diagnostic.Counts.Candidates != 0 {
		return "", false
	}
	reason := outcome.Diagnostic.ReasonCode
	switch outcome.Status {
	case lyricsprovideroutcome.StatusNoMatch:
		switch reason {
		case lyricsprovideroutcome.ReasonNoMatch, lyricsprovideroutcome.ReasonNoSearchHits,
			lyricsprovideroutcome.ReasonIdentityMismatch, lyricsprovideroutcome.ReasonMissingSongSignal,
			lyricsprovideroutcome.ReasonMissingLyrics:
			return reason, true
		}
	case lyricsprovideroutcome.StatusStale:
		if reason == lyricsprovideroutcome.ReasonRevisionChanged {
			return reason, true
		}
	case lyricsprovideroutcome.StatusAmbiguous:
		switch reason {
		case lyricsprovideroutcome.ReasonAmbiguousMatch, lyricsprovideroutcome.ReasonCandidateConflict,
			lyricsprovideroutcome.ReasonMultipleCandidates:
			return reason, true
		}
	case lyricsprovideroutcome.StatusUnsupported:
		if outcome.Diagnostic.Phase != lyricsprovideroutcome.PhaseParseLyrics {
			return "", false
		}
		switch reason {
		case lyricsprovideroutcome.ReasonUnsupportedFormat, lyricsprovideroutcome.ReasonLyricsTooLarge,
			lyricsprovideroutcome.ReasonCatalogRenditionConflict:
			return reason, true
		}
	}
	return "", false
}

func buildSekaipediaCanaryDiagnostic(
	recoveryPlanID string,
	recoveryPlanSHA256 string,
	identity lyricssource.MusicIdentity,
	list lyricssource.FixedIndex,
	target SekaipediaCanarySongPlan,
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
	committed []lyricsacquisition.Acquisition,
	forensicResponses []ForensicResponseRef,
	replay ProviderReplay,
	decision providerDecision,
	enterResult ProviderOutcomeEnterResult,
	fallbackReason lyricsprovideroutcome.ReasonCode,
) (SekaipediaCanaryDiagnostic, error) {
	if identity.MusicID != target.MusicID || identity.JapaneseTitle != target.CatalogTitle ||
		outcome.Provider != lyricssource.ProviderSekaipedia || outcome.Validate() != nil ||
		replay.Outcome.Provider != outcome.Provider || replay.Artifact.Provider != outcome.Provider || len(committed) > 2 {
		return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary terminal diagnostic input is invalid")
	}
	diagnostic := SekaipediaCanaryDiagnostic{
		SchemaVersion:  SekaipediaCanaryDiagnosticSchemaV2,
		RecoveryPlanID: recoveryPlanID, RecoveryPlanSHA256: recoveryPlanSHA256,
		MusicID: target.MusicID, Provider: outcome.Provider, Status: outcome.Status,
		ReasonCode: outcome.Diagnostic.ReasonCode, Phase: outcome.Diagnostic.Phase,
		Counts: outcome.Diagnostic.Counts, EnterResult: enterResult, FallbackReasonCode: fallbackReason,
		ForensicResponses: append([]ForensicResponseRef{}, forensicResponses...),
	}
	if len(committed) >= 1 {
		if proof, err := verifySekaipediaCanaryListAcquisition(committed[0], list); err == nil {
			diagnostic.List = &proof
		}
	}
	if len(committed) == 2 {
		if proof, err := verifySekaipediaCanarySongAcquisition(committed[1], target); err == nil {
			diagnostic.Song = &proof
		}
	}

	if enterResult == ProviderOutcomeCompleteCompositionStop {
		if outcome.Status != lyricsprovideroutcome.StatusCandidate || len(outcome.Candidates) != 1 ||
			replay.Fixed == nil || decision.Composition == nil || decision.Continue || fallbackReason != "" ||
			diagnostic.List == nil || diagnostic.Song == nil || len(diagnostic.ForensicResponses) != 2 ||
			diagnostic.ForensicResponses[0].StatusCode != http.StatusOK ||
			diagnostic.ForensicResponses[0].RawResponseSHA256 != diagnostic.List.RawResponseSHA256 ||
			diagnostic.ForensicResponses[1].StatusCode != http.StatusOK ||
			diagnostic.ForensicResponses[1].RawResponseSHA256 != diagnostic.Song.RawResponseSHA256 {
			return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary complete terminal lacks exact revision evidence")
		}
		candidate := outcome.Candidates[0]
		fixed := replay.Fixed
		if candidate.PageID != target.PageID || candidate.RevisionID != target.RevisionID ||
			candidate.Title != target.ProviderTitle || candidate.RevisionTimestamp != target.RevisionTimestamp ||
			candidate.SHA1 != target.SHA1 || candidate.RawSHA256 != target.ContentSHA256 ||
			fixed.PageID != target.PageID || fixed.RevisionID != target.RevisionID ||
			fixed.PageTitle != target.ProviderTitle || fixed.SHA1 != target.SHA1 || fixed.RawSHA256 != target.ContentSHA256 ||
			fixed.RevisionTimestamp.UTC().Format(time.RFC3339Nano) != target.RevisionTimestamp {
			return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary candidate conflicts with the plan-pinned song revision")
		}
		refs := outcome.Diagnostic.AcquisitionRefs
		if len(refs) != 2 || refs[0].EvidenceID != diagnostic.List.EvidenceID ||
			refs[0].SHA256 != diagnostic.List.RawResponseSHA256 || refs[1].EvidenceID != diagnostic.Song.EvidenceID ||
			refs[1].SHA256 != diagnostic.Song.RawResponseSHA256 {
			return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia ProviderOutcome does not enter with the exact List and song revision evidence")
		}
	}
	if err := diagnostic.Validate(); err != nil {
		return SekaipediaCanaryDiagnostic{}, err
	}
	return diagnostic, nil
}

func (diagnostic SekaipediaCanaryDiagnostic) Validate() error {
	if diagnostic.SchemaVersion != SekaipediaCanaryDiagnosticSchemaV2 ||
		!validSekaipediaCanaryPlanID(diagnostic.RecoveryPlanID) ||
		!canonicalLowerSHA256(diagnostic.RecoveryPlanSHA256) || diagnostic.MusicID <= 0 ||
		diagnostic.Provider != lyricssource.ProviderSekaipedia || diagnostic.Song != nil && diagnostic.List == nil ||
		diagnostic.ForensicResponses == nil || len(diagnostic.ForensicResponses) > 2 ||
		len(diagnostic.ForensicResponses) > diagnostic.Counts.Acquisitions {
		return errors.New("Sekaipedia canary diagnostic identity is invalid")
	}
	seenForensic := make(map[string]struct{}, len(diagnostic.ForensicResponses))
	for _, forensic := range diagnostic.ForensicResponses {
		if !canonicalLowerSHA256(forensic.ResponseID) || !canonicalLowerSHA256(forensic.RawResponseSHA256) ||
			forensic.StatusCode < 100 || forensic.StatusCode > 599 {
			return errors.New("Sekaipedia canary forensic response reference is invalid")
		}
		if _, duplicate := seenForensic[forensic.ResponseID]; duplicate {
			return errors.New("Sekaipedia canary forensic response reference is duplicated")
		}
		seenForensic[forensic.ResponseID] = struct{}{}
	}
	refs := make([]model.LyricsSourceIndexEvidenceRef, 0, 2)
	for _, proof := range []*SekaipediaCanaryRevisionDiagnostic{diagnostic.List, diagnostic.Song} {
		if proof == nil {
			continue
		}
		if err := validateSekaipediaCanaryRevisionDiagnostic(*proof); err != nil {
			return err
		}
		refs = append(refs, model.LyricsSourceIndexEvidenceRef{
			EvidenceID: proof.EvidenceID, SHA256: proof.RawResponseSHA256,
		})
	}
	if diagnostic.List != nil && diagnostic.Song != nil &&
		(diagnostic.List.AcquisitionID == diagnostic.Song.AcquisitionID || diagnostic.List.EvidenceID == diagnostic.Song.EvidenceID) {
		return errors.New("Sekaipedia canary diagnostic revision evidence aliases")
	}
	candidates := []lyricssource.Candidate{}
	if diagnostic.Status == lyricsprovideroutcome.StatusCandidate {
		candidates = append(candidates, lyricssource.Candidate{Provider: lyricssource.ProviderSekaipedia})
	}
	outcome := lyricsprovideroutcome.Outcome[lyricssource.Candidate]{
		Provider: diagnostic.Provider, Status: diagnostic.Status, Candidates: candidates,
		Diagnostic: lyricsprovideroutcome.Diagnostic{
			Provider: diagnostic.Provider, Phase: diagnostic.Phase,
			ReasonCode: diagnostic.ReasonCode, Counts: diagnostic.Counts, AcquisitionRefs: refs,
		},
	}
	if err := outcome.Validate(); err != nil {
		return errors.New("Sekaipedia canary diagnostic provider outcome is invalid")
	}
	switch diagnostic.EnterResult {
	case ProviderOutcomeCompleteCompositionStop:
		if diagnostic.Status != lyricsprovideroutcome.StatusCandidate || diagnostic.FallbackReasonCode != "" ||
			diagnostic.List == nil || diagnostic.Song == nil || len(diagnostic.ForensicResponses) != 2 ||
			diagnostic.ForensicResponses[0].StatusCode != http.StatusOK ||
			diagnostic.ForensicResponses[0].RawResponseSHA256 != diagnostic.List.RawResponseSHA256 ||
			diagnostic.ForensicResponses[1].StatusCode != http.StatusOK ||
			diagnostic.ForensicResponses[1].RawResponseSHA256 != diagnostic.Song.RawResponseSHA256 {
			return errors.New("Sekaipedia canary complete terminal is missing exact evidence")
		}
	case ProviderOutcomeCandidateFreeFallback:
		reason, allowed := SekaipediaFallbackReasonCode(outcome)
		if !allowed || diagnostic.FallbackReasonCode != reason {
			return errors.New("Sekaipedia canary fallback terminal conflicts with the closed reason policy")
		}
	case ProviderOutcomeFailClosed:
		_, allowed := SekaipediaFallbackReasonCode(outcome)
		planRevisionConflict := diagnostic.Status == lyricsprovideroutcome.StatusStale &&
			diagnostic.ReasonCode == lyricsprovideroutcome.ReasonRevisionChanged
		if allowed && !planRevisionConflict || diagnostic.FallbackReasonCode != "" {
			return errors.New("Sekaipedia canary fail-closed terminal conflicts with the closed reason policy")
		}
	default:
		return errors.New("Sekaipedia canary diagnostic enter result is invalid")
	}
	return nil
}

func SekaipediaCanaryCompleteCompositionStop(
	runtime RuntimeConfig,
	diagnostic SekaipediaCanaryDiagnostic,
) bool {
	if diagnostic.Validate() != nil || runtime.SekaipediaCanary == nil ||
		diagnostic.RecoveryPlanID != runtime.RecoveryPlanID ||
		diagnostic.RecoveryPlanSHA256 != runtime.RecoveryPlanSHA256 ||
		diagnostic.EnterResult != ProviderOutcomeCompleteCompositionStop ||
		diagnostic.FallbackReasonCode != "" || diagnostic.List == nil || diagnostic.Song == nil {
		return false
	}
	target, found := runtime.SekaipediaCanary.song(diagnostic.MusicID)
	if !found {
		return false
	}
	list := runtime.SekaipediaCanary.List
	if diagnostic.List.PageID != list.PageID || diagnostic.List.RevisionID != list.RevisionID ||
		diagnostic.List.RevisionTimestamp != list.RevisionTimestamp || diagnostic.List.SHA1 != list.SHA1 ||
		diagnostic.List.ContentSHA256 != list.ContentSHA256 ||
		diagnostic.Song.PageID != target.PageID || diagnostic.Song.RevisionID != target.RevisionID ||
		diagnostic.Song.RevisionTimestamp != target.RevisionTimestamp || diagnostic.Song.SHA1 != target.SHA1 ||
		diagnostic.Song.ContentSHA256 != target.ContentSHA256 {
		return false
	}
	wantCounts := lyricsprovideroutcome.Counts{Acquisitions: 2, Targets: 1, Evaluated: 1, Candidates: 1}
	return diagnostic.Provider == lyricssource.ProviderSekaipedia &&
		diagnostic.Status == lyricsprovideroutcome.StatusCandidate &&
		diagnostic.ReasonCode == lyricsprovideroutcome.ReasonCandidate &&
		diagnostic.Phase == lyricsprovideroutcome.PhaseFinalize && diagnostic.Counts == wantCounts
}

func MarshalSekaipediaCanaryDiagnostic(diagnostic SekaipediaCanaryDiagnostic) ([]byte, error) {
	if err := diagnostic.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(diagnostic)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > MaxSekaipediaCanaryDiagnosticBytes {
		return nil, errors.New("Sekaipedia canary diagnostic exceeds its byte boundary")
	}
	return body, nil
}

func DecodeSekaipediaCanaryDiagnostic(body []byte) (SekaipediaCanaryDiagnostic, error) {
	if len(body) == 0 || len(body) > MaxSekaipediaCanaryDiagnosticBytes {
		return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary diagnostic exceeds its byte boundary")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var diagnostic SekaipediaCanaryDiagnostic
	if err := decoder.Decode(&diagnostic); err != nil {
		return SekaipediaCanaryDiagnostic{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary diagnostic contains trailing JSON")
	}
	if err := diagnostic.Validate(); err != nil {
		return SekaipediaCanaryDiagnostic{}, err
	}
	canonical, err := json.Marshal(diagnostic)
	if err != nil || !bytes.Equal(body, canonical) {
		return SekaipediaCanaryDiagnostic{}, errors.New("Sekaipedia canary diagnostic is not canonical JSON")
	}
	return cloneSekaipediaCanaryDiagnostic(diagnostic), nil
}

func cloneSekaipediaCanaryDiagnostic(diagnostic SekaipediaCanaryDiagnostic) SekaipediaCanaryDiagnostic {
	diagnostic.ForensicResponses = append([]ForensicResponseRef{}, diagnostic.ForensicResponses...)
	if diagnostic.List != nil {
		list := *diagnostic.List
		diagnostic.List = &list
	}
	if diagnostic.Song != nil {
		song := *diagnostic.Song
		diagnostic.Song = &song
	}
	return diagnostic
}

func validateSekaipediaCanaryRevisionDiagnostic(proof SekaipediaCanaryRevisionDiagnostic) error {
	timestamp, err := time.Parse(time.RFC3339Nano, proof.RevisionTimestamp)
	if proof.AcquisitionID == "" || proof.EvidenceID == "" || proof.PageID <= 0 || proof.RevisionID <= 0 ||
		!lyricssource.HasCanonicalSHA1(proof.SHA1) || !canonicalLowerSHA256(proof.ContentSHA256) ||
		!canonicalLowerSHA256(proof.RawResponseSHA256) || err != nil ||
		timestamp.Location() != time.UTC || timestamp.UTC().Format(time.RFC3339Nano) != proof.RevisionTimestamp {
		return errors.New("Sekaipedia canary revision diagnostic is invalid")
	}
	return nil
}

func validSekaipediaCanaryPlanID(value string) bool {
	if value == "" || len(value) > 128 ||
		(value[0] < 'a' || value[0] > 'z') && (value[0] < '0' || value[0] > '9') {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '.' && character != '_' && character != '-' {
					return false
				}
			}
		}
	}
	return true
}

func canonicalLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			if value[index] < 'a' || value[index] > 'f' {
				return false
			}
		}
	}
	return true
}

func verifySekaipediaCanaryListAcquisition(
	acquired lyricsacquisition.Acquisition,
	list lyricssource.FixedIndex,
) (SekaipediaCanaryRevisionDiagnostic, error) {
	if err := validateSekaipediaCanaryListRequest(acquired, list, false); err != nil {
		return SekaipediaCanaryRevisionDiagnostic{}, err
	}
	return verifySekaipediaCanaryRevisionBytesMode(acquired, list, false)
}

func verifySekaipediaCanaryListReplayAcquisition(
	acquired lyricsacquisition.Acquisition,
	list lyricssource.FixedIndex,
) error {
	if err := validateSekaipediaCanaryListRequest(acquired, list, true); err != nil {
		return err
	}
	_, err := verifySekaipediaCanaryRevisionBytesMode(acquired, list, true)
	return err
}

func validateSekaipediaCanaryListRequest(
	acquired lyricsacquisition.Acquisition,
	list lyricssource.FixedIndex,
	allowHistoricalRevisionKind bool,
) error {
	kindMatches := acquired.Request.Kind == lyricsacquisition.RequestKindFixedIndex ||
		allowHistoricalRevisionKind && acquired.Request.Kind == lyricsacquisition.RequestKindRevision
	if !kindMatches || acquired.Request.RevisionSelector != "oldid:"+strconv.Itoa(list.RevisionID) {
		return errors.New("Sekaipedia canary List acquisition conflicts with the fixed authority")
	}
	query, err := sekaipediaCanaryRequestQuery(acquired)
	if err != nil || !exactSekaipediaCanaryQuery(query, map[string]string{
		"revids": strconv.Itoa(list.RevisionID),
	}) {
		return errors.New("Sekaipedia canary List request is not the exact fixed revision")
	}
	return nil
}

func verifySekaipediaCanarySongAcquisition(
	acquired lyricsacquisition.Acquisition,
	target SekaipediaCanarySongPlan,
) (SekaipediaCanaryRevisionDiagnostic, error) {
	if acquired.Request.Kind != lyricsacquisition.RequestKindRevision ||
		acquired.Request.RevisionSelector != "oldid:"+strconv.Itoa(target.RevisionID) {
		return SekaipediaCanaryRevisionDiagnostic{}, errors.New("Sekaipedia canary song acquisition conflicts with the plan-pinned revision")
	}
	query, err := sekaipediaCanaryRequestQuery(acquired)
	if err != nil || !exactSekaipediaCanaryQuery(query, map[string]string{
		"revids": strconv.Itoa(target.RevisionID),
	}) {
		return SekaipediaCanaryRevisionDiagnostic{}, errors.New("Sekaipedia canary song request is not the exact plan-pinned revision acquisition")
	}
	return verifySekaipediaCanaryRevisionBytesMode(acquired, lyricssource.FixedIndex{
		PageID: target.PageID, RevisionID: target.RevisionID, RevisionTimestamp: target.RevisionTimestamp,
		SHA1: target.SHA1, ContentSHA256: target.ContentSHA256,
	}, false)
}

func sekaipediaCanaryRequestQuery(acquired lyricsacquisition.Acquisition) (url.Values, error) {
	parsed, err := url.Parse(acquired.Request.CanonicalRequestIdentity)
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host != "www.sekaipedia.org" ||
		parsed.Path != "/w/api.php" || parsed.RawQuery == "" || parsed.RawQuery != parsed.Query().Encode() {
		return nil, errors.New("Sekaipedia canary acquisition request identity is not canonical maxlag=5")
	}
	return parsed.Query(), nil
}

func exactSekaipediaCanaryQuery(query url.Values, specific map[string]string) bool {
	expected := map[string]string{
		"action": "query", "cllimit": "max", "format": "json", "formatversion": "2",
		"maxlag": "5", "prop": "revisions|categories",
		"rvprop": "ids|timestamp|sha1|content", "rvslots": "main",
	}
	for key, value := range specific {
		expected[key] = value
	}
	if len(query) != len(expected) {
		return false
	}
	for key, value := range expected {
		values := query[key]
		if len(values) != 1 || values[0] != value {
			return false
		}
	}
	return true
}

func verifySekaipediaCanaryRevisionBytesMode(
	acquired lyricsacquisition.Acquisition,
	expected lyricssource.FixedIndex,
	replayOnly bool,
) (SekaipediaCanaryRevisionDiagnostic, error) {
	rawResponseSHA256 := fmt.Sprintf("%x", sha256.Sum256(acquired.RawResponse))
	evidenceEnvelopeSHA256 := fmt.Sprintf("%x", sha256.Sum256(acquired.EvidenceEnvelope))
	if acquired.ReplayOnly != replayOnly || acquired.Request.Provider != string(lyricssource.ProviderSekaipedia) ||
		acquired.RawResponseSHA256 != rawResponseSHA256 || acquired.Evidence.EvidenceID == "" ||
		acquired.Evidence.RawSHA256 != rawResponseSHA256 || !bytes.Equal(acquired.RawResponse, acquired.Evidence.Raw) ||
		acquired.EvidenceEnvelopeSHA256 != evidenceEnvelopeSHA256 || len(acquired.EvidenceEnvelope) == 0 ||
		len(acquired.ObservedRevisions) != 1 {
		return SekaipediaCanaryRevisionDiagnostic{}, errors.New("Sekaipedia canary acquisition does not preserve exact raw revision evidence")
	}
	observed := acquired.ObservedRevisions[0]
	if observed.Selector != "oldid:"+strconv.Itoa(expected.RevisionID) ||
		observed.RevisionID != int64(expected.RevisionID) || observed.Timestamp != expected.RevisionTimestamp ||
		observed.SHA1 != expected.SHA1 ||
		lyricssource.VerifySekaipediaRevisionContent(acquired.RawResponse, expected) != nil {
		return SekaipediaCanaryRevisionDiagnostic{}, errors.New("Sekaipedia canary observed revision conflicts with the plan pin")
	}
	return SekaipediaCanaryRevisionDiagnostic{
		AcquisitionID: acquired.AcquisitionID, EvidenceID: acquired.Evidence.EvidenceID,
		PageID: expected.PageID, RevisionID: expected.RevisionID, RevisionTimestamp: expected.RevisionTimestamp,
		SHA1: expected.SHA1, ContentSHA256: expected.ContentSHA256, RawResponseSHA256: rawResponseSHA256,
	}, nil
}

func replayCommittedAcquisitions(
	ctx context.Context,
	ledger *lyricsacquisition.Ledger,
	committed []lyricsacquisition.Acquisition,
) ([]lyricsacquisition.Acquisition, error) {
	exact := make([]lyricsacquisition.Acquisition, len(committed))
	for index, acquisition := range committed {
		replayed, err := ledger.ReplayByAcquisitionID(ctx, acquisition.AcquisitionID)
		if err != nil {
			return nil, err
		}
		exact[index] = replayed
	}
	return exact, nil
}

// AcquireSong is the single-song convenience entry point. Multi-song callers
// must retain one AcquisitionSession so provider delay and Retry-After state
// cannot be reset between songs.
func AcquireSong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
	liveTransports map[model.LyricsSourceProvider]http.RoundTripper,
) ([]ProviderAcquisitionSet, []AcquisitionProgress, error) {
	session, err := NewAcquisitionSession(runtime, ledger, liveTransports)
	if err != nil {
		return nil, nil, err
	}
	return session.AcquireSong(ctx, musicID, identity)
}

func AcquireSekaipediaCanarySong(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
	liveTransports map[model.LyricsSourceProvider]http.RoundTripper,
) ([]ProviderAcquisitionSet, []AcquisitionProgress, SekaipediaCanaryDiagnostic, error) {
	session, err := NewAcquisitionSession(runtime, ledger, liveTransports)
	if err != nil {
		return nil, nil, SekaipediaCanaryDiagnostic{}, err
	}
	return session.AcquireSekaipediaCanarySong(ctx, musicID, identity)
}

func waitRecoveryRetry(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
