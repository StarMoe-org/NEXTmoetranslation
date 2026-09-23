package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
	"moesekai/server/offline/internal/lyricsrecovery"
)

func TestAuthorizedLiveCanaryCommandPublishesExactDiagnosticBeforeOverallPass(t *testing.T) {
	arguments, _ := bindLiveCanaryCommandListReplay(t, commandTestArguments(t))
	arguments = append(arguments,
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
	)
	ownership := newLiveCanaryCommandOwnership(t, liveCanaryCommandExact)
	withLiveCanaryCommandOwnership(t, ownership)

	var output bytes.Buffer
	if err := run(context.Background(), arguments, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "RESULT mode=live-canary") ||
		!strings.Contains(text, "enterResult=complete_composition_stop") ||
		!strings.Contains(text, "fallbackReasonCode= exactRevisionEvidence=true") ||
		strings.Count(text, "PASS mode=live-canary") != 1 {
		t.Fatalf("live-canary success output=%q", text)
	}
	if ownership.sekaipediaRequests.Load() != 1 || ownership.listRequests.Load() != 0 ||
		ownership.songRequests.Load() != 1 || ownership.fallbackRequests.Load() != 0 ||
		ownership.resolutions.Load() != 1 {
		t.Fatalf("live-canary requests=%d list=%d song=%d fallback=%d resolutions=%d",
			ownership.sekaipediaRequests.Load(), ownership.listRequests.Load(), ownership.songRequests.Load(),
			ownership.fallbackRequests.Load(), ownership.resolutions.Load())
	}

	diagnosticPath := liveCanaryCommandDiagnosticPath(t, arguments, 2)
	diagnostic, err := lyricsrecovery.OpenSekaipediaCanaryDiagnostic(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	runtime := liveCanaryCommandRuntime(t, arguments)
	if diagnostic.EnterResult != lyricsrecovery.ProviderOutcomeCompleteCompositionStop ||
		diagnostic.FallbackReasonCode != "" ||
		!lyricsrecovery.SekaipediaCanaryCompleteCompositionStop(runtime, diagnostic) {
		t.Fatalf("published exact live-canary diagnostic=%+v", diagnostic)
	}
	if info, err := os.Lstat(diagnosticPath); err != nil || info.Mode().Perm() != 0o600 || info.Mode().Type() != 0 {
		t.Fatalf("published diagnostic info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(commandFlagValue(t, arguments, "-acquisition-set")); err != nil {
		t.Fatalf("successful live canary did not publish its exact acquisition set: %v", err)
	}
}

func TestAuthorizedLiveCanaryCommandReplaysExactPlanBoundListIDFromValidatedCopyWithoutSecondListRequest(t *testing.T) {
	arguments := commandTestArguments(t)
	sourceParent, err := os.MkdirTemp(recoveryCommandTestRoot, "live-canary-list-replay-source-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceParent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceParent) })
	sourceRoot := filepath.Join(sourceParent, "ledger")
	sourceLedger, err := lyricsacquisition.CreateLedger(t.Context(), sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := sourceLedger.Commit(t.Context(), exactReplayFixtureRecord(
		t, time.Date(2026, 8, 2, 15, 58, 47, 64_046_000, time.UTC),
	))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := sourceLedger.Commit(t.Context(), exactReplayFixtureRecord(
		t, time.Date(2026, 8, 2, 16, 58, 47, 64_046_000, time.UTC),
	))
	if err != nil {
		t.Fatal(err)
	}
	if selected.AcquisitionID == latest.AcquisitionID {
		t.Fatal("exact replay fixture did not create distinct acquisition IDs")
	}
	if err := sourceLedger.Close(); err != nil {
		t.Fatal(err)
	}
	arguments = rewriteLiveCanaryCommandPlan(t, arguments, func(plan *lyricsextractionplan.RecoveryPlan) {
		plan.SekaipediaCanary.List.AcquisitionID = string(selected.AcquisitionID)
	})
	sourceBefore := snapshotLiveCanaryReplaySource(t, sourceRoot)
	arguments = append(arguments,
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
		"-sekaipedia-list-replay-ledger", sourceRoot,
		"-sekaipedia-list-replay-acquisition-id", string(selected.AcquisitionID),
	)
	ownership := newLiveCanaryCommandOwnership(t, liveCanaryCommandExact)
	withLiveCanaryCommandOwnership(t, ownership)

	var output bytes.Buffer
	if err := run(context.Background(), arguments, &output); err != nil {
		t.Fatal(err)
	}
	if ownership.sekaipediaRequests.Load() != 1 || ownership.listRequests.Load() != 0 ||
		ownership.songRequests.Load() != 1 || ownership.fallbackRequests.Load() != 0 {
		t.Fatalf("exact List replay made unexpected live requests: sekaipedia=%d list=%d song=%d fallback=%d",
			ownership.sekaipediaRequests.Load(), ownership.listRequests.Load(), ownership.songRequests.Load(),
			ownership.fallbackRequests.Load())
	}
	diagnostic, err := lyricsrecovery.OpenSekaipediaCanaryDiagnostic(liveCanaryCommandDiagnosticPath(t, arguments, 2))
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.List == nil {
		t.Fatal("live canary omitted the replayed List diagnostic")
	}
	outputLedger, err := lyricsacquisition.OpenLedger(t.Context(), commandFlagValue(t, arguments, "-ledger"))
	if err != nil {
		t.Fatal(err)
	}
	retained, replayErr := outputLedger.ReplayByAcquisitionID(t.Context(), diagnostic.List.AcquisitionID)
	closeErr := outputLedger.Close()
	if replayErr != nil || closeErr != nil {
		t.Fatal(errors.Join(replayErr, closeErr))
	}
	if retained.FetchedAt != selected.FetchedAt || retained.FetchedAt == latest.FetchedAt ||
		retained.RawResponseSHA256 != selected.RawResponseSHA256 || !bytes.Equal(retained.RawResponse, selected.RawResponse) {
		t.Fatalf("live canary did not consume the exact non-latest List acquisition metadata: selected=%s latest=%s retained=%+v",
			selected.AcquisitionID, latest.AcquisitionID, retained)
	}
	if _, err := os.Lstat(exactReplayRuntimeCopyPath(commandFlagValue(t, arguments, "-ledger"))); err != nil {
		t.Fatalf("live canary did not retain its private runtime ledger copy: %v", err)
	}
	if sourceAfter := snapshotLiveCanaryReplaySource(t, sourceRoot); !reflect.DeepEqual(sourceBefore, sourceAfter) {
		t.Fatalf("live canary mutated the protected replay source: before=%v after=%v", sourceBefore, sourceAfter)
	}
}

func bindLiveCanaryCommandListReplay(
	t *testing.T,
	arguments []string,
) ([]string, lyricsacquisition.Acquisition) {
	t.Helper()
	sourceParent, err := os.MkdirTemp(recoveryCommandTestRoot, "live-canary-required-list-replay-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceParent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceParent) })
	sourceRoot := filepath.Join(sourceParent, "ledger")
	ledger, err := lyricsacquisition.CreateLedger(t.Context(), sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ledger.Commit(t.Context(), exactReplayFixtureRecord(
		t, time.Date(2026, 8, 2, 15, 58, 47, 64_046_000, time.UTC),
	))
	if err != nil {
		_ = ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	arguments = rewriteLiveCanaryCommandPlan(t, arguments, func(plan *lyricsextractionplan.RecoveryPlan) {
		plan.SekaipediaCanary.List.AcquisitionID = string(selected.AcquisitionID)
	})
	return append(arguments, "-sekaipedia-list-replay-ledger", sourceRoot), selected
}

func snapshotLiveCanaryReplaySource(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			result[relative] = fmt.Sprintf("directory:%04o", info.Mode().Perm())
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		result[relative] = fmt.Sprintf("file:%04o:%d:%s", info.Mode().Perm(), len(body), hex.EncodeToString(digest[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAuthorizedLiveCanaryCommandCandidateFreeTerminalNeverPrintsOverallPass(t *testing.T) {
	arguments := commandTestArguments(t)
	arguments = rewriteLiveCanaryCommandPlan(t, arguments, func(plan *lyricsextractionplan.RecoveryPlan) {
		plan.Providers.Configurations[0].ContributorAliases = []lyricsextractionplan.RecoveryContributorAlias{}
	})
	arguments, _ = bindLiveCanaryCommandListReplay(t, arguments)
	arguments = append(arguments,
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
	)
	ownership := newLiveCanaryCommandOwnership(t, liveCanaryCommandExact)
	withLiveCanaryCommandOwnership(t, ownership)

	var output bytes.Buffer
	err := run(context.Background(), arguments, &output)
	if !errors.Is(err, lyricsrecovery.ErrSekaipediaCanaryTerminal) {
		t.Fatalf("candidate-free live canary error=%v output=%q", err, output.String())
	}
	text := output.String()
	if strings.Contains(text, "PASS mode=live-canary") ||
		!strings.Contains(text, "FAIL mode=live-canary") ||
		!strings.Contains(text, "enterResult=candidate_free_fallback") ||
		!strings.Contains(text, "fallbackReasonCode=identity_mismatch") {
		t.Fatalf("candidate-free live-canary output=%q", text)
	}
	if ownership.sekaipediaRequests.Load() != 1 || ownership.listRequests.Load() != 0 ||
		ownership.songRequests.Load() != 1 || ownership.fallbackRequests.Load() != 0 ||
		ownership.resolutions.Load() != 1 {
		t.Fatalf("candidate-free requests=%d list=%d song=%d fallback=%d resolutions=%d",
			ownership.sekaipediaRequests.Load(), ownership.listRequests.Load(), ownership.songRequests.Load(),
			ownership.fallbackRequests.Load(), ownership.resolutions.Load())
	}
	diagnostic, err := lyricsrecovery.OpenSekaipediaCanaryDiagnostic(liveCanaryCommandDiagnosticPath(t, arguments, 2))
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.EnterResult != lyricsrecovery.ProviderOutcomeCandidateFreeFallback ||
		string(diagnostic.FallbackReasonCode) != "identity_mismatch" ||
		lyricsrecovery.SekaipediaCanaryCompleteCompositionStop(liveCanaryCommandRuntime(t, arguments), diagnostic) {
		t.Fatalf("candidate-free published diagnostic=%+v", diagnostic)
	}
	if _, err := os.Lstat(commandFlagValue(t, arguments, "-acquisition-set")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed live canary published an overall acquisition set: %v", err)
	}
}

func TestAuthorizedLiveCanaryCommandFailClosedTerminalNeverPrintsOverallPass(t *testing.T) {
	arguments, _ := bindLiveCanaryCommandListReplay(t, commandTestArguments(t))
	arguments = append(arguments,
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
	)
	ownership := newLiveCanaryCommandOwnership(t, liveCanaryCommandTransportFailure)
	withLiveCanaryCommandOwnership(t, ownership)

	var output bytes.Buffer
	err := run(context.Background(), arguments, &output)
	if !errors.Is(err, lyricsrecovery.ErrSekaipediaCanaryTerminal) {
		t.Fatalf("fail-closed live canary error=%v output=%q", err, output.String())
	}
	text := output.String()
	if strings.Contains(text, "PASS mode=live-canary") ||
		!strings.Contains(text, "FAIL mode=live-canary") ||
		!strings.Contains(text, "enterResult=fail_closed") ||
		!strings.Contains(text, "fallbackReasonCode= exactRevisionEvidence=false") {
		t.Fatalf("fail-closed live-canary output=%q", text)
	}
	if ownership.sekaipediaRequests.Load() != 1 || ownership.listRequests.Load() != 0 ||
		ownership.songRequests.Load() != 1 || ownership.fallbackRequests.Load() != 0 {
		t.Fatalf("fail-closed requests=%d list=%d song=%d fallback=%d",
			ownership.sekaipediaRequests.Load(), ownership.listRequests.Load(), ownership.songRequests.Load(),
			ownership.fallbackRequests.Load())
	}
	diagnostic, err := lyricsrecovery.OpenSekaipediaCanaryDiagnostic(liveCanaryCommandDiagnosticPath(t, arguments, 2))
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.EnterResult != lyricsrecovery.ProviderOutcomeFailClosed || diagnostic.FallbackReasonCode != "" ||
		lyricsrecovery.SekaipediaCanaryCompleteCompositionStop(liveCanaryCommandRuntime(t, arguments), diagnostic) {
		t.Fatalf("fail-closed published diagnostic=%+v", diagnostic)
	}
}

func TestAuthorizedLiveCanaryCommandRejectsCompatibilityIDMismatchBeforeOwnership(t *testing.T) {
	arguments, selected := bindLiveCanaryCommandListReplay(t, commandTestArguments(t))
	mismatch := strings.Repeat("0", 64)
	if mismatch == string(selected.AcquisitionID) {
		mismatch = strings.Repeat("1", 64)
	}
	arguments = append(arguments,
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
		"-sekaipedia-list-replay-acquisition-id", mismatch,
	)
	var ownershipCalls atomic.Int32
	previous := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
		ownershipCalls.Add(1)
		return nil, errors.New("compatibility mismatch reached live ownership")
	}
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previous })

	if err := run(context.Background(), arguments, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "does not exactly match the immutable plan") {
		t.Fatalf("compatibility mismatch error=%v", err)
	}
	if ownershipCalls.Load() != 0 {
		t.Fatalf("compatibility mismatch touched live ownership %d times", ownershipCalls.Load())
	}
	for _, flagName := range []string{"-ledger", "-acquisition-set", "-provider-outcomes"} {
		if _, err := os.Lstat(commandFlagValue(t, arguments, flagName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("compatibility mismatch touched %s: %v", flagName, err)
		}
	}
}

func TestAuthorizedLiveCanaryCommandRequiresExactPlanBindingBeforeOwnership(t *testing.T) {
	arguments, _ := bindLiveCanaryCommandListReplay(t, commandTestArguments(t))
	arguments = rewriteLiveCanaryCommandPlan(t, arguments, func(plan *lyricsextractionplan.RecoveryPlan) {
		plan.SekaipediaCanary = nil
	})
	arguments = append(arguments,
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
	)
	var ownershipCalls atomic.Int32
	previous := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
		ownershipCalls.Add(1)
		return nil, errors.New("exact plan rejection reached live ownership")
	}
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previous })

	if err := run(context.Background(), arguments, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "exact Sekaipedia List replay identity") {
		t.Fatalf("missing exact plan binding error=%v", err)
	}
	if ownershipCalls.Load() != 0 {
		t.Fatalf("missing exact plan binding touched live ownership %d times", ownershipCalls.Load())
	}
	for _, flagName := range []string{"-ledger", "-acquisition-set", "-provider-outcomes"} {
		if _, err := os.Lstat(commandFlagValue(t, arguments, flagName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing exact plan binding touched %s: %v", flagName, err)
		}
	}
}

type liveCanaryCommandMode int

const (
	liveCanaryCommandExact liveCanaryCommandMode = iota
	liveCanaryCommandTransportFailure
)

type liveCanaryCommandOwnership struct {
	mode               liveCanaryCommandMode
	listBody           []byte
	songBody           []byte
	sekaipediaRequests atomic.Int32
	listRequests       atomic.Int32
	songRequests       atomic.Int32
	fallbackRequests   atomic.Int32
	resolutions        atomic.Int32
	closed             atomic.Int32
}

func newLiveCanaryCommandOwnership(t *testing.T, mode liveCanaryCommandMode) *liveCanaryCommandOwnership {
	t.Helper()
	return &liveCanaryCommandOwnership{
		mode:     mode,
		listBody: liveCanaryCommandFixture(t, "sekaipedia-list-335193.json"),
		songBody: liveCanaryCommandFixture(t, "sekaipedia-roki-330574.json"),
	}
}

func (ownership *liveCanaryCommandOwnership) Wrap(
	provider lyricsproviderpolicy.Provider,
	_ http.RoundTripper,
) (http.RoundTripper, error) {
	if provider != lyricsproviderpolicy.ProviderSekaipedia {
		return liveCanaryCommandRoundTripFunc(func(*http.Request) (*http.Response, error) {
			ownership.fallbackRequests.Add(1)
			return nil, errors.New("live canary must not contact a fallback provider")
		}), nil
	}
	return liveCanaryCommandRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		ownership.sekaipediaRequests.Add(1)
		query := request.URL.Query()
		revisionID := query.Get("revids")
		if !exactLiveCanaryRevisionQuery(query, revisionID) {
			return nil, errors.New("unexpected non-exact Sekaipedia canary request")
		}
		var body []byte
		switch revisionID {
		case "335193":
			ownership.listRequests.Add(1)
			body = ownership.listBody
		case "330574":
			ownership.songRequests.Add(1)
			body = ownership.songBody
		default:
			return nil, errors.New("unexpected Sekaipedia canary revision")
		}
		if ownership.mode == liveCanaryCommandTransportFailure {
			return nil, errors.New("reviewed local transport failure")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request,
		}, nil
	}), nil
}

func (ownership *liveCanaryCommandOwnership) ResolveProvider(provider lyricsproviderpolicy.Provider) error {
	if provider != lyricsproviderpolicy.ProviderSekaipedia {
		return errors.New("live canary resolved a fallback provider")
	}
	ownership.resolutions.Add(1)
	return nil
}

func (ownership *liveCanaryCommandOwnership) Close() error {
	ownership.closed.Add(1)
	return nil
}

func withLiveCanaryCommandOwnership(t *testing.T, ownership *liveCanaryCommandOwnership) {
	t.Helper()
	previous := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) { return ownership, nil }
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previous })
}

type liveCanaryCommandRoundTripFunc func(*http.Request) (*http.Response, error)

func (function liveCanaryCommandRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func exactLiveCanaryRevisionQuery(actual url.Values, revisionID string) bool {
	if revisionID == "" {
		return false
	}
	expected := url.Values{
		"action": {"query"}, "cllimit": {"max"}, "format": {"json"}, "formatversion": {"2"},
		"maxlag": {"5"}, "prop": {"revisions|categories"}, "revids": {revisionID},
		"rvprop": {"ids|timestamp|sha1|content"}, "rvslots": {"main"},
	}
	return reflect.DeepEqual(actual, expected)
}

func liveCanaryCommandFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "lyricssource", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func rewriteLiveCanaryCommandPlan(
	t *testing.T,
	arguments []string,
	mutate func(*lyricsextractionplan.RecoveryPlan),
) []string {
	t.Helper()
	path := commandFlagValue(t, arguments, "-plan")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lyricsextractionplan.DecodeRecoveryCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&plan)
	body, err = lyricsextractionplan.MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := lyricsextractionplan.RecoveryCanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	result := append([]string(nil), arguments...)
	for index := 0; index+1 < len(result); index++ {
		if result[index] == "-expected-plan-sha256" {
			result[index+1] = digest
			return result
		}
	}
	t.Fatal("command arguments omit expected plan SHA-256")
	return nil
}

func liveCanaryCommandRuntime(t *testing.T, arguments []string) lyricsrecovery.RuntimeConfig {
	t.Helper()
	body, err := os.ReadFile(commandFlagValue(t, arguments, "-plan"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lyricsextractionplan.DecodeRecoveryCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := lyricsrecovery.RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = lyricsrecovery.WithSekaipediaCanaryPlan(runtime, plan)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func liveCanaryCommandDiagnosticPath(t *testing.T, arguments []string, musicID int) string {
	t.Helper()
	name, err := lyricsrecovery.SekaipediaCanaryDiagnosticFileName(musicID)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(commandFlagValue(t, arguments, "-provider-outcomes"), name)
}
