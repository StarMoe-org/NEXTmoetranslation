package lyricsrecovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricscompose"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

func TestSekaipediaCanaryClosedFallbackReasonMatrix(t *testing.T) {
	tests := []struct {
		name    string
		status  lyricsprovideroutcome.Status
		reason  lyricsprovideroutcome.ReasonCode
		phase   lyricsprovideroutcome.Phase
		allowed bool
	}{
		{name: "no match", status: lyricsprovideroutcome.StatusNoMatch, reason: lyricsprovideroutcome.ReasonNoMatch, phase: lyricsprovideroutcome.PhaseMatchIdentity, allowed: true},
		{name: "no search hits", status: lyricsprovideroutcome.StatusNoMatch, reason: lyricsprovideroutcome.ReasonNoSearchHits, phase: lyricsprovideroutcome.PhaseResolveTargets, allowed: true},
		{name: "identity mismatch", status: lyricsprovideroutcome.StatusNoMatch, reason: lyricsprovideroutcome.ReasonIdentityMismatch, phase: lyricsprovideroutcome.PhaseMatchIdentity, allowed: true},
		{name: "missing song signal", status: lyricsprovideroutcome.StatusNoMatch, reason: lyricsprovideroutcome.ReasonMissingSongSignal, phase: lyricsprovideroutcome.PhaseMatchIdentity, allowed: true},
		{name: "missing lyrics", status: lyricsprovideroutcome.StatusNoMatch, reason: lyricsprovideroutcome.ReasonMissingLyrics, phase: lyricsprovideroutcome.PhaseParseLyrics, allowed: true},
		{name: "revision changed", status: lyricsprovideroutcome.StatusStale, reason: lyricsprovideroutcome.ReasonRevisionChanged, phase: lyricsprovideroutcome.PhaseAcquireTarget, allowed: true},
		{name: "ambiguous match", status: lyricsprovideroutcome.StatusAmbiguous, reason: lyricsprovideroutcome.ReasonAmbiguousMatch, phase: lyricsprovideroutcome.PhaseResolveTargets, allowed: true},
		{name: "candidate conflict", status: lyricsprovideroutcome.StatusAmbiguous, reason: lyricsprovideroutcome.ReasonCandidateConflict, phase: lyricsprovideroutcome.PhaseFinalize, allowed: true},
		{name: "multiple candidates", status: lyricsprovideroutcome.StatusAmbiguous, reason: lyricsprovideroutcome.ReasonMultipleCandidates, phase: lyricsprovideroutcome.PhaseFinalize, allowed: true},
		{name: "unsupported format", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonUnsupportedFormat, phase: lyricsprovideroutcome.PhaseParseLyrics, allowed: true},
		{name: "lyrics too large", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonLyricsTooLarge, phase: lyricsprovideroutcome.PhaseParseLyrics, allowed: true},
		{name: "catalog rendition conflict", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonCatalogRenditionConflict, phase: lyricsprovideroutcome.PhaseParseLyrics, allowed: true},
		{name: "transport", status: lyricsprovideroutcome.StatusTransportError, reason: lyricsprovideroutcome.ReasonTransport, phase: lyricsprovideroutcome.PhaseAcquireAuthority},
		{name: "canceled", status: lyricsprovideroutcome.StatusTransportError, reason: lyricsprovideroutcome.ReasonCanceled, phase: lyricsprovideroutcome.PhaseAcquireAuthority},
		{name: "deadline", status: lyricsprovideroutcome.StatusTransportError, reason: lyricsprovideroutcome.ReasonDeadlineExceeded, phase: lyricsprovideroutcome.PhaseAcquireAuthority},
		{name: "malformed parser", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonMalformedResponse, phase: lyricsprovideroutcome.PhaseParseLyrics},
		{name: "malformed authority", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonMalformedResponse, phase: lyricsprovideroutcome.PhaseAcquireAuthority},
		{name: "restricted", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonRestrictedReprint, phase: lyricsprovideroutcome.PhaseMatchIdentity},
		{name: "unsupported outside parser", status: lyricsprovideroutcome.StatusUnsupported, reason: lyricsprovideroutcome.ReasonUnsupportedFormat, phase: lyricsprovideroutcome.PhaseFinalize},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := sekaipediaCanaryCandidateFreeOutcome(t, test.status, test.reason, test.phase)
			reason, allowed := SekaipediaFallbackReasonCode(outcome)
			if allowed != test.allowed || allowed && reason != test.reason || !allowed && reason != "" {
				t.Fatalf("closed fallback reason=%q allowed=%t wantReason=%q wantAllowed=%t", reason, allowed, test.reason, test.allowed)
			}
			enter, fallbackReason, err := providerOutcomeEnterResult(
				ProviderReplay{Outcome: outcome, Artifact: lyricsoutcomeartifact.Artifact{Provider: model.LyricsSourceProviderSekaipedia}},
				providerDecision{Continue: test.allowed},
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.allowed {
				if enter != ProviderOutcomeCandidateFreeFallback || fallbackReason != test.reason {
					t.Fatalf("allowed enter=%q fallbackReason=%q", enter, fallbackReason)
				}
			} else if enter != ProviderOutcomeFailClosed || fallbackReason != "" {
				t.Fatalf("fail-closed enter=%q fallbackReason=%q", enter, fallbackReason)
			}
		})
	}

	candidate, err := lyricsprovideroutcome.New(
		model.LyricsSourceProviderSekaipedia,
		lyricsprovideroutcome.StatusCandidate,
		[]lyricssource.Candidate{{Provider: model.LyricsSourceProviderSekaipedia}},
		lyricsprovideroutcome.Diagnostic{
			Provider: model.LyricsSourceProviderSekaipedia, Phase: lyricsprovideroutcome.PhaseFinalize,
			ReasonCode: lyricsprovideroutcome.ReasonCandidate,
			Counts:     lyricsprovideroutcome.Counts{Candidates: 1},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	enter, reason, err := providerOutcomeEnterResult(
		ProviderReplay{Outcome: candidate, Artifact: lyricsoutcomeartifact.Artifact{Provider: model.LyricsSourceProviderSekaipedia}},
		providerDecision{Composition: &lyricscompose.FixedArtifactComposition{}},
	)
	if err != nil || enter != ProviderOutcomeCompleteCompositionStop || reason != "" {
		t.Fatalf("complete candidate enter=%q reason=%q err=%v", enter, reason, err)
	}
}

func TestSekaipediaCanaryAllowedReasonPersistsBeforeFallbackProviders(t *testing.T) {
	fixture := sekaipediaCanaryRuntimeWithIdentityMismatch(t)
	providers, progress, err := AcquireSong(
		t.Context(), 2, fixture.identities[2], fixture.runtime, fixture.ledger, fixture.transports,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) < 2 || len(progress) < 2 ||
		providers[0].Provider != model.LyricsSourceProviderSekaipedia ||
		providers[0].Status != lyricsprovideroutcome.StatusNoMatch ||
		providers[0].ReasonCode != lyricsprovideroutcome.ReasonIdentityMismatch ||
		progress[0].EnterResult != ProviderOutcomeCandidateFreeFallback ||
		progress[0].FallbackReasonCode != lyricsprovideroutcome.ReasonIdentityMismatch {
		t.Fatalf("allowed fallback prefix=%+v progress=%+v", providers, progress)
	}
	if fixture.transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() == 0 ||
		fixture.transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() == 0 {
		t.Fatal("allowed candidate-free Sekaipedia reason did not continue through the deterministic fallback prefix")
	}

	set, err := NewAcquisitionSet(
		fixture.plan.PlanID, fixture.runtime.RecoveryPlanSHA256, fixture.runtime.Order,
		[]SongAcquisitionSet{{MusicID: 2, Providers: providers}},
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalAcquisitionSet(set)
	if err != nil || !bytes.Contains(body, []byte(`"reasonCode":"identity_mismatch"`)) {
		t.Fatalf("persisted acquisition set reason bytes=%s err=%v", body, err)
	}
}

func TestSekaipediaLiveCanaryPersistsCandidateFreeTerminalWithoutFallback(t *testing.T) {
	fixture := sekaipediaCanaryRuntimeWithIdentityMismatch(t)
	providers, progress, diagnostic, err := AcquireSekaipediaCanarySong(
		t.Context(), 2, fixture.identities[2], fixture.runtime, fixture.ledger, fixture.transports,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(progress) != 1 ||
		diagnostic.EnterResult != ProviderOutcomeCandidateFreeFallback ||
		diagnostic.FallbackReasonCode != lyricsprovideroutcome.ReasonIdentityMismatch ||
		SekaipediaCanaryCompleteCompositionStop(fixture.runtime, diagnostic) {
		t.Fatalf("candidate-free live canary providers=%+v progress=%+v diagnostic=%+v", providers, progress, diagnostic)
	}
	if fixture.transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		fixture.transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("candidate-free live canary contacted a fallback provider")
	}
	body, err := MarshalSekaipediaCanaryDiagnostic(diagnostic)
	if err != nil || !bytes.Contains(body, []byte(`"enterResult":"candidate_free_fallback"`)) ||
		!bytes.Contains(body, []byte(`"fallbackReasonCode":"identity_mismatch"`)) {
		t.Fatalf("candidate-free diagnostic bytes=%s err=%v", body, err)
	}
	decoded, err := DecodeSekaipediaCanaryDiagnostic(body)
	if err != nil || decoded.EnterResult != diagnostic.EnterResult || decoded.FallbackReasonCode != diagnostic.FallbackReasonCode {
		t.Fatalf("candidate-free diagnostic round trip=%+v err=%v", decoded, err)
	}
}

func TestSekaipediaCanaryTransportMalformedDeniedAndCanceledFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*testing.T, sekaipediaCanaryFixture) (context.Context, map[model.LyricsSourceProvider]http.RoundTripper)
		wantReason lyricsprovideroutcome.ReasonCode
	}{
		{
			name: "transport",
			configure: func(t *testing.T, fixture sekaipediaCanaryFixture) (context.Context, map[model.LyricsSourceProvider]http.RoundTripper) {
				fixture.transports[model.LyricsSourceProviderSekaipedia] = &fixtureRoundTripper{
					provider: model.LyricsSourceProviderSekaipedia,
					respond:  func(*http.Request) ([]byte, error) { return nil, errors.New("local fixture transport failure") },
				}
				return t.Context(), fixture.transports
			},
			wantReason: lyricsprovideroutcome.ReasonTransport,
		},
		{
			name: "malformed",
			configure: func(t *testing.T, fixture sekaipediaCanaryFixture) (context.Context, map[model.LyricsSourceProvider]http.RoundTripper) {
				fixture.transports[model.LyricsSourceProviderSekaipedia] = &fixtureRoundTripper{
					provider: model.LyricsSourceProviderSekaipedia,
					respond:  func(*http.Request) ([]byte, error) { return []byte(`{"query":{}}`), nil },
				}
				return t.Context(), fixture.transports
			},
			wantReason: lyricsprovideroutcome.ReasonMalformedResponse,
		},
		{
			name: "denied",
			configure: func(t *testing.T, fixture sekaipediaCanaryFixture) (context.Context, map[model.LyricsSourceProvider]http.RoundTripper) {
				fixture.transports[model.LyricsSourceProviderSekaipedia] = &sekaipediaCanaryStatusFixture{status: http.StatusForbidden}
				return t.Context(), fixture.transports
			},
			wantReason: lyricsprovideroutcome.ReasonMalformedResponse,
		},
		{
			name: "canceled",
			configure: func(t *testing.T, fixture sekaipediaCanaryFixture) (context.Context, map[model.LyricsSourceProvider]http.RoundTripper) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx, fixture.transports
			},
			wantReason: lyricsprovideroutcome.ReasonCanceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSekaipediaCanaryFixture(t, []int{2})
			ctx, transports := test.configure(t, fixture)
			providers, progress, err := AcquireSong(
				ctx, 2, fixture.identities[2], fixture.runtime, fixture.ledger, transports,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(providers) != 1 || len(progress) != 1 ||
				providers[0].Provider != model.LyricsSourceProviderSekaipedia ||
				providers[0].ReasonCode != test.wantReason ||
				progress[0].ReasonCode != test.wantReason || progress[0].EnterResult != ProviderOutcomeFailClosed ||
				progress[0].FallbackReasonCode != "" {
				t.Fatalf("fail-closed providers=%+v progress=%+v", providers, progress)
			}
			if transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
				transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
				t.Fatal("fail-closed Sekaipedia terminal contacted a fallback provider")
			}
		})
	}
}

func sekaipediaCanaryCandidateFreeOutcome(
	t *testing.T,
	status lyricsprovideroutcome.Status,
	reason lyricsprovideroutcome.ReasonCode,
	phase lyricsprovideroutcome.Phase,
) lyricsprovideroutcome.Outcome[lyricssource.Candidate] {
	t.Helper()
	counts := lyricsprovideroutcome.Counts{}
	switch status {
	case lyricsprovideroutcome.StatusNoMatch:
		counts.NoMatch = 1
	case lyricsprovideroutcome.StatusUnsupported:
		counts.Unsupported = 1
	case lyricsprovideroutcome.StatusStale:
		counts.Stale = 1
	case lyricsprovideroutcome.StatusTransportError:
		counts.TransportErrors = 1
	case lyricsprovideroutcome.StatusAmbiguous:
		counts.Ambiguous = 1
	}
	outcome, err := lyricsprovideroutcome.New(
		model.LyricsSourceProviderSekaipedia, status, []lyricssource.Candidate{},
		lyricsprovideroutcome.Diagnostic{
			Provider: model.LyricsSourceProviderSekaipedia, Phase: phase, ReasonCode: reason, Counts: counts,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

type sekaipediaCanaryStatusFixture struct {
	mu       sync.Mutex
	status   int
	requests int
}

func (transport *sekaipediaCanaryStatusFixture) recoveryOfflineFixture() bool { return true }

func (transport *sekaipediaCanaryStatusFixture) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.requests++
	transport.mu.Unlock()
	body := []byte(`{"error":{"code":"permissiondenied"}}`)
	return &http.Response{
		StatusCode: transport.status, Status: http.StatusText(transport.status), Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Request: request,
	}, nil
}
