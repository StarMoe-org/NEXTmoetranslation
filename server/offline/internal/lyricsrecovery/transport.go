package lyricsrecovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

type recoveryOfflineFixtureTransport interface {
	http.RoundTripper
	recoveryOfflineFixture() bool
}

type acquisitionFetchedAtOverride interface {
	RecoveryAcquisitionFetchedAt(*http.Request, *http.Response) (time.Time, bool, error)
}

func SekaipediaCanaryDiagnosticFileName(musicID int) (string, error) {
	if musicID <= 0 {
		return "", errors.New("Sekaipedia canary diagnostic music ID is invalid")
	}
	return fmt.Sprintf("sekaipedia-live-canary-%010d.json", musicID), nil
}

func PublishSekaipediaCanaryDiagnostic(
	directory string,
	diagnostic SekaipediaCanaryDiagnostic,
) (string, error) {
	body, err := MarshalSekaipediaCanaryDiagnostic(diagnostic)
	if err != nil {
		return "", err
	}
	name, err := SekaipediaCanaryDiagnosticFileName(diagnostic.MusicID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	if err := publishPrivateFile(path, body, func(candidate []byte) error {
		_, err := DecodeSekaipediaCanaryDiagnostic(candidate)
		return err
	}); err != nil {
		return "", err
	}
	return path, nil
}

func OpenSekaipediaCanaryDiagnostic(path string) (SekaipediaCanaryDiagnostic, error) {
	body, err := readPrivateFile(path, MaxSekaipediaCanaryDiagnosticBytes, 1)
	if err != nil {
		return SekaipediaCanaryDiagnostic{}, err
	}
	return DecodeSekaipediaCanaryDiagnostic(body)
}

type AcquisitionTransport struct {
	provider    model.LyricsSourceProvider
	authorities []lyricssource.FixedIndex
	ledger      *lyricsacquisition.Ledger
	forensics   *ForensicResponseStore
	underlying  http.RoundTripper

	mu           sync.Mutex
	inFlight     bool
	fetchedAt    map[*http.Response]time.Time
	retained     map[string]int
	forensicRefs []ForensicResponseRef
	committed    []lyricsacquisition.Acquisition
}

func NewAcquisitionTransport(
	provider model.LyricsSourceProvider,
	authorities []lyricssource.FixedIndex,
	ledger *lyricsacquisition.Ledger,
	underlying http.RoundTripper,
) (*AcquisitionTransport, error) {
	if !model.IsValidLyricsSourceProvider(provider) || ledger == nil || underlying == nil {
		return nil, errors.New("acquisition transport requires provider, ledger, and explicit live transport")
	}
	ledgerRoot, err := ledger.RootPath()
	if err != nil {
		return nil, err
	}
	forensicRoot, err := ForensicResponseStorePath(ledgerRoot)
	if err != nil {
		return nil, err
	}
	forensics, err := openOrCreateForensicResponseStore(forensicRoot)
	if err != nil {
		return nil, err
	}
	return &AcquisitionTransport{
		provider: provider, authorities: append([]lyricssource.FixedIndex(nil), authorities...),
		ledger: ledger, forensics: forensics, underlying: underlying,
		fetchedAt: make(map[*http.Response]time.Time), retained: make(map[string]int),
	}, nil
}

func (transport *AcquisitionTransport) RecoveryOffline() bool {
	fixture, ok := transport.underlying.(recoveryOfflineFixtureTransport)
	return ok && fixture.recoveryOfflineFixture()
}

func (transport *AcquisitionTransport) RecoveryRequestOffline(request *http.Request) bool {
	marker, ok := transport.underlying.(interface {
		RecoveryRequestOffline(*http.Request) bool
	})
	return ok && marker.RecoveryRequestOffline(request)
}

func (transport *AcquisitionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("acquisition transport request is invalid")
	}
	maxlag := request.URL.Query()["maxlag"]
	if request.Method != http.MethodGet || request.Body != nil || len(maxlag) != 1 || maxlag[0] != "5" {
		return nil, errors.New("acquisition transport requires one bodyless GET with exact maxlag=5")
	}
	transport.mu.Lock()
	if transport.inFlight {
		transport.mu.Unlock()
		return nil, errors.New("more than one actual provider request is in flight")
	}
	transport.inFlight = true
	transport.mu.Unlock()

	response, err := transport.underlying.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		transport.releaseInFlight()
		return nil, err
	}
	if response == nil || response.Body == nil {
		transport.releaseInFlight()
		return nil, errors.New("acquisition transport received an incomplete provider response")
	}
	fetchedAt := time.Now().UTC()
	if override, ok := transport.underlying.(acquisitionFetchedAtOverride); ok {
		exact, useExact, overrideErr := override.RecoveryAcquisitionFetchedAt(request, response)
		if overrideErr != nil || useExact && (exact.IsZero() || exact.Location() != time.UTC) {
			_ = response.Body.Close()
			transport.releaseInFlight()
			if overrideErr != nil {
				return nil, overrideErr
			}
			return nil, errors.New("acquisition fetchedAt override is invalid")
		}
		if useExact {
			fetchedAt = exact
		}
	}
	transport.mu.Lock()
	transport.fetchedAt[response] = fetchedAt
	transport.mu.Unlock()
	response.Body = &acquisitionResponseBody{
		ReadCloser: response.Body,
		release:    transport.releaseInFlight,
	}
	return response, nil
}

func (transport *AcquisitionTransport) releaseInFlight() {
	transport.mu.Lock()
	transport.inFlight = false
	transport.mu.Unlock()
}

type acquisitionResponseBody struct {
	io.ReadCloser
	releaseOnce sync.Once
	release     func()
}

func (body *acquisitionResponseBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if err != nil {
		body.releaseOnce.Do(body.release)
	}
	return count, err
}

func (body *acquisitionResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.releaseOnce.Do(body.release)
	return err
}

func (transport *AcquisitionTransport) RecoveryFetchedAt(
	_ *http.Request,
	response *http.Response,
) (time.Time, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	fetchedAt, found := transport.fetchedAt[response]
	if !found {
		return time.Time{}, errors.New("acquisition response has no exact fetchedAt")
	}
	return fetchedAt, nil
}

func (transport *AcquisitionTransport) RecoveryRetainResponse(
	ctx context.Context,
	_ *http.Request,
	httpResponse *http.Response,
	response lyricssource.RecoveryHTTPResponse,
) error {
	if transport == nil || transport.forensics == nil || httpResponse == nil {
		return errors.New("acquisition forensic response boundary is invalid")
	}
	transport.mu.Lock()
	fetchedAt, found := transport.fetchedAt[httpResponse]
	transport.mu.Unlock()
	if !found || !fetchedAt.Equal(response.FetchedAt) {
		return errors.New("acquisition forensic response has no exact fetchedAt binding")
	}
	ref, err := transport.forensics.Commit(ctx, transport.provider, response)
	if err != nil {
		return err
	}
	transport.mu.Lock()
	delete(transport.fetchedAt, httpResponse)
	transport.retained[ref.ResponseID]++
	transport.forensicRefs = append(transport.forensicRefs, ref)
	transport.mu.Unlock()
	return nil
}

func (transport *AcquisitionTransport) RecoveryCommitResponse(
	ctx context.Context,
	response lyricssource.RecoveryHTTPResponse,
) error {
	if response.StatusCode != http.StatusOK {
		return errors.New("semantic acquisition rejects a non-success response")
	}
	ref, err := forensicResponseReference(transport.provider, response)
	if err != nil {
		return err
	}
	transport.mu.Lock()
	if transport.retained[ref.ResponseID] < 1 {
		transport.mu.Unlock()
		return errors.New("semantic acquisition response was not durably retained first")
	}
	transport.retained[ref.ResponseID]--
	transport.mu.Unlock()
	capture, err := lyricssource.CaptureRecoveryHTTPResponse(transport.provider, transport.authorities, response)
	if err != nil {
		return err
	}
	committed, err := transport.ledger.Commit(ctx, recordInputFromCapture(capture))
	if err != nil {
		return err
	}
	transport.mu.Lock()
	transport.committed = append(transport.committed, committed)
	transport.mu.Unlock()
	return nil
}

func (transport *AcquisitionTransport) ForensicResponses() []ForensicResponseRef {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]ForensicResponseRef(nil), transport.forensicRefs...)
}

func (transport *AcquisitionTransport) Committed() []lyricsacquisition.Acquisition {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	result := make([]lyricsacquisition.Acquisition, len(transport.committed))
	for index, acquired := range transport.committed {
		result[index] = cloneAcquisition(acquired)
	}
	return result
}

// PlanBoundSekaipediaListReplayTransport serves only the exact bytes from one
// independently validated, plan-pinned List acquisition before delegating song
// requests to the coordinated live transport. It cannot select by request key
// or recency. Canary mode retains the source fetchedAt; repeatable acquisition
// mode timestamps each local observation without claiming another HTTP fetch.
type PlanBoundSekaipediaListReplayTransport struct {
	live             http.RoundTripper
	canonicalRequest string
	sourceFetchedAt  time.Time
	raw              []byte
	replayEveryMatch bool

	mu                  sync.Mutex
	replayCount         int
	lastReplayFetchedAt time.Time
	pendingResponse     *http.Response
	pendingFetchedAt    time.Time
}

func NewPlanBoundSekaipediaListReplayTransport(
	acquired lyricsacquisition.Acquisition,
	expected lyricssource.FixedIndex,
	live http.RoundTripper,
) (*PlanBoundSekaipediaListReplayTransport, error) {
	if live == nil || !canonicalLowerSHA256(string(acquired.AcquisitionID)) ||
		verifySekaipediaCanaryListReplayAcquisition(acquired, expected) != nil {
		return nil, errors.New("source acquisition is not exactly bound to the Sekaipedia List canary plan")
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, acquired.FetchedAt)
	if err != nil || fetchedAt.Location() != time.UTC ||
		fetchedAt.UTC().Format(time.RFC3339Nano) != acquired.FetchedAt {
		return nil, errors.New("source Sekaipedia List acquisition fetchedAt is invalid")
	}
	return &PlanBoundSekaipediaListReplayTransport{
		live: live, canonicalRequest: acquired.Request.CanonicalRequestIdentity,
		sourceFetchedAt: fetchedAt, raw: append([]byte(nil), acquired.RawResponse...),
	}, nil
}

func NewRepeatablePlanBoundSekaipediaListReplayTransport(
	acquired lyricsacquisition.Acquisition,
	expected lyricssource.FixedIndex,
	live http.RoundTripper,
) (*PlanBoundSekaipediaListReplayTransport, error) {
	transport, err := NewPlanBoundSekaipediaListReplayTransport(acquired, expected, live)
	if err != nil {
		return nil, err
	}
	transport.replayEveryMatch = true
	return transport, nil
}

func (transport *PlanBoundSekaipediaListReplayTransport) RecoveryRequestOffline(request *http.Request) bool {
	if transport == nil || request == nil || request.URL == nil || request.Method != http.MethodGet || request.Body != nil {
		return false
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return request.URL.String() == transport.canonicalRequest &&
		transport.pendingResponse == nil && (transport.replayEveryMatch || transport.replayCount == 0)
}

func (transport *PlanBoundSekaipediaListReplayTransport) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	if transport == nil || request == nil || request.URL == nil {
		return nil, errors.New("Sekaipedia List replay request is invalid")
	}
	transport.mu.Lock()
	if transport.pendingResponse != nil {
		transport.mu.Unlock()
		return nil, errors.New("Sekaipedia List replay response has not crossed the fetchedAt boundary")
	}
	exactReplay := request.Method == http.MethodGet && request.Body == nil &&
		request.URL.String() == transport.canonicalRequest
	if exactReplay {
		if transport.replayCount > 0 && !transport.replayEveryMatch {
			transport.mu.Unlock()
			return nil, errors.New("Sekaipedia List canary replay was requested more than once")
		}
		fetchedAt := transport.sourceFetchedAt
		if transport.replayEveryMatch {
			// A regular acquisition locally observes the exact immutable source
			// bytes once per song. Give every observation a truthful unique UTC
			// time so the song-local acquisition records remain distinct without
			// pretending that another provider request occurred.
			fetchedAt = time.Now().UTC()
			if !fetchedAt.After(transport.lastReplayFetchedAt) {
				fetchedAt = transport.lastReplayFetchedAt.Add(time.Nanosecond)
			}
			transport.lastReplayFetchedAt = fetchedAt
		}
		transport.replayCount++
		response := &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(transport.raw)),
			ContentLength: int64(len(transport.raw)), Request: request,
		}
		transport.pendingResponse = response
		transport.pendingFetchedAt = fetchedAt
		transport.mu.Unlock()
		return response, nil
	}
	if transport.replayCount == 0 {
		transport.mu.Unlock()
		return nil, errors.New("first Sekaipedia request does not match the exact List replay acquisition")
	}
	live := transport.live
	transport.mu.Unlock()
	return live.RoundTrip(request)
}

func (transport *PlanBoundSekaipediaListReplayTransport) RecoveryAcquisitionFetchedAt(
	request *http.Request,
	response *http.Response,
) (time.Time, bool, error) {
	if transport == nil || response == nil {
		return time.Time{}, false, errors.New("Sekaipedia List replay fetchedAt request is invalid")
	}
	transport.mu.Lock()
	if response == transport.pendingResponse {
		fetchedAt := transport.pendingFetchedAt
		transport.pendingResponse = nil
		transport.pendingFetchedAt = time.Time{}
		transport.mu.Unlock()
		if fetchedAt.IsZero() || fetchedAt.Location() != time.UTC {
			return time.Time{}, false, errors.New("Sekaipedia List replay fetchedAt is invalid")
		}
		return fetchedAt, true, nil
	}
	live := transport.live
	transport.mu.Unlock()
	if override, ok := live.(acquisitionFetchedAtOverride); ok {
		return override.RecoveryAcquisitionFetchedAt(request, response)
	}
	return time.Time{}, false, nil
}

func (transport *PlanBoundSekaipediaListReplayTransport) Consumed() bool {
	if transport == nil {
		return false
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.replayCount > 0 && transport.pendingResponse == nil && transport.pendingFetchedAt.IsZero()
}

func (transport *PlanBoundSekaipediaListReplayTransport) ReplayCount() int {
	if transport == nil {
		return 0
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.replayCount
}

type ReplayTransport struct {
	provider    model.LyricsSourceProvider
	authorities []lyricssource.FixedIndex
	acquired    []lyricsacquisition.Acquisition
	terminal    ProviderAcquisitionSet

	mu               sync.Mutex
	next             int
	pending          *replayPending
	terminalObserved bool
	failure          error
}

type replayPending struct {
	response    *http.Response
	acquisition lyricsacquisition.Acquisition
	fetchedAt   time.Time
	retained    bool
}

func NewReplayTransport(
	ctx context.Context,
	provider model.LyricsSourceProvider,
	authorities []lyricssource.FixedIndex,
	ledger *lyricsacquisition.Ledger,
	terminal ProviderAcquisitionSet,
) (*ReplayTransport, error) {
	orderedIDs := terminal.AcquisitionIDs
	if ctx == nil || !model.IsValidLyricsSourceProvider(provider) || ledger == nil || orderedIDs == nil ||
		terminal.Provider != provider || validateProviderTerminal(terminal) != nil {
		return nil, errors.New("replay transport requires provider, ledger, and an explicit closed ordered acquisition set")
	}
	seenIDs := make(map[lyricsacquisition.AcquisitionID]struct{}, len(orderedIDs))
	seenRequests := make(map[string]lyricsacquisition.AcquisitionID, len(orderedIDs))
	acquired := make([]lyricsacquisition.Acquisition, len(orderedIDs))
	for index, acquisitionID := range orderedIDs {
		if _, duplicate := seenIDs[acquisitionID]; duplicate {
			return nil, errors.New("ordered replay acquisition IDs contain a duplicate")
		}
		seenIDs[acquisitionID] = struct{}{}
		item, err := ledger.ReplayByAcquisitionID(ctx, acquisitionID)
		if err != nil {
			return nil, fmt.Errorf("replay exact acquisition %d: %w", index, err)
		}
		if !item.ReplayOnly || item.Request.Provider != string(provider) {
			return nil, errors.New("ordered replay acquisition is cross-provider or not replay-only")
		}
		if previous, duplicate := seenRequests[item.Request.CanonicalRequestIdentity]; duplicate && previous != acquisitionID {
			return nil, errors.New("ordered replay contains conflicting acquisitions for one exact request")
		}
		seenRequests[item.Request.CanonicalRequestIdentity] = acquisitionID
		acquired[index] = cloneAcquisition(item)
	}
	terminal.AcquisitionIDs = append([]lyricsacquisition.AcquisitionID(nil), terminal.AcquisitionIDs...)
	return &ReplayTransport{
		provider: provider, authorities: append([]lyricssource.FixedIndex(nil), authorities...),
		acquired: acquired, terminal: terminal,
	}, nil
}

func (transport *ReplayTransport) RecoveryOffline() bool {
	return true
}

func (transport *ReplayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("offline replay request is invalid")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.pending != nil {
		return nil, transport.failLocked("offline replay has an uncommitted pending response")
	}
	if transport.next >= len(transport.acquired) {
		if transport.terminal.Status != lyricsprovideroutcome.StatusTransportError || transport.terminalObserved {
			return nil, transport.failLocked("offline replay required a missing acquisition")
		}
		transport.terminalObserved = true
		return nil, replayTerminalError(transport.terminal.ReasonCode)
	}
	acquired := transport.acquired[transport.next]
	if request.Method != http.MethodGet || request.URL.String() != acquired.Request.CanonicalRequestIdentity {
		return nil, transport.failLocked("offline replay request conflicts with the explicit acquisition order")
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, acquired.FetchedAt)
	if err != nil || fetchedAt.Location() != time.UTC {
		return nil, errors.New("offline replay acquisition fetchedAt is invalid")
	}
	response := &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(acquired.RawResponse)),
		ContentLength: int64(len(acquired.RawResponse)), Request: request,
	}
	transport.pending = &replayPending{response: response, acquisition: acquired, fetchedAt: fetchedAt}
	return response, nil
}

func (transport *ReplayTransport) RecoveryFetchedAt(
	_ *http.Request,
	response *http.Response,
) (time.Time, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.pending == nil || transport.pending.response != response {
		return time.Time{}, errors.New("offline replay response is not the pending exact acquisition")
	}
	return transport.pending.fetchedAt, nil
}

func (transport *ReplayTransport) RecoveryRetainResponse(
	_ context.Context,
	_ *http.Request,
	httpResponse *http.Response,
	response lyricssource.RecoveryHTTPResponse,
) error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.pending == nil || transport.pending.response != httpResponse || response.StatusCode != http.StatusOK ||
		response.CanonicalRequestURL != transport.pending.acquisition.Request.CanonicalRequestIdentity ||
		!response.FetchedAt.Equal(transport.pending.fetchedAt) ||
		response.Raw == nil || !bytes.Equal(response.Raw, transport.pending.acquisition.RawResponse) ||
		digestHex(response.Raw) != transport.pending.acquisition.RawResponseSHA256 {
		return transport.failLocked("offline replay response is not the exact durable acquisition")
	}
	transport.pending.retained = true
	return nil
}

func (transport *ReplayTransport) RecoveryCommitResponse(
	_ context.Context,
	response lyricssource.RecoveryHTTPResponse,
) error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.pending == nil || !transport.pending.retained {
		return errors.New("offline replay has no retained pending exact acquisition")
	}
	capture, err := lyricssource.CaptureRecoveryHTTPResponse(transport.provider, transport.authorities, response)
	if err != nil {
		transport.failure = err
		return err
	}
	if err := captureMatchesAcquisition(capture, transport.pending.acquisition); err != nil {
		transport.failure = err
		return err
	}
	transport.pending = nil
	transport.next++
	return nil
}

func (transport *ReplayTransport) Acquisitions() []lyricsacquisition.Acquisition {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	result := make([]lyricsacquisition.Acquisition, len(transport.acquired))
	for index, acquired := range transport.acquired {
		result[index] = cloneAcquisition(acquired)
	}
	return result
}

// AssertAcquisitionsConsumed verifies that the parser consumed the caller-pinned
// exact acquisition sequence without consulting its historical parser terminal.
// It is used only while deriving a new terminal from immutable ledger evidence.
func (transport *ReplayTransport) AssertAcquisitionsConsumed() error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.assertAcquisitionsConsumedLocked()
}

func (transport *ReplayTransport) AssertOutcomeConsumed(
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
) error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if err := transport.assertAcquisitionsConsumedLocked(); err != nil {
		return err
	}
	terminal := transport.terminal
	if outcome.Provider != terminal.Provider || outcome.Status != terminal.Status ||
		outcome.Diagnostic.ReasonCode != terminal.ReasonCode || outcome.Diagnostic.Phase != terminal.Phase ||
		outcome.Diagnostic.Counts != terminal.Counts {
		return errors.New("offline replay provider outcome conflicts with the closed acquisition terminal")
	}
	wantTerminalFailure := terminal.Status == lyricsprovideroutcome.StatusTransportError
	if transport.terminalObserved != wantTerminalFailure {
		return errors.New("offline replay terminal failure observation is incomplete or unexpected")
	}
	return nil
}

func (transport *ReplayTransport) assertAcquisitionsConsumedLocked() error {
	if transport.failure != nil {
		return transport.failure
	}
	if transport.pending != nil || transport.next != len(transport.acquired) {
		return errors.New("offline replay acquisition set was not consumed exactly")
	}
	return nil
}

func replayTerminalError(reason lyricsprovideroutcome.ReasonCode) error {
	switch reason {
	case lyricsprovideroutcome.ReasonCanceled:
		return context.Canceled
	case lyricsprovideroutcome.ReasonDeadlineExceeded:
		return context.DeadlineExceeded
	default:
		return errors.New("offline replay exact terminal transport failure")
	}
}

func (transport *ReplayTransport) failLocked(message string) error {
	if transport.failure == nil {
		transport.failure = errors.New(message)
	}
	return transport.failure
}

func recordInputFromCapture(capture lyricssource.RecoveryCapture) lyricsacquisition.RecordInput {
	observed := make([]lyricsacquisition.ObservedRevision, len(capture.ObservedRevisions))
	for index, revision := range capture.ObservedRevisions {
		observed[index] = lyricsacquisition.ObservedRevision{
			Selector: revision.Selector, RevisionID: revision.RevisionID,
			Timestamp: revision.Timestamp, SHA1: revision.SHA1,
		}
	}
	return lyricsacquisition.RecordInput{
		Request: lyricsacquisition.Request{
			Provider: string(capture.Provider), CanonicalRequestIdentity: capture.CanonicalRequestURL,
			Kind: lyricsacquisition.RequestKind(capture.RequestKind), RevisionSelector: capture.RevisionSelector,
		},
		FetchedAt: capture.FetchedAt, RawResponse: append([]byte(nil), capture.RawResponse...),
		RawResponseSHA256: capture.RawResponseSHA256,
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: capture.Evidence.EvidenceID, Raw: append([]byte(nil), capture.Evidence.Raw...),
			RawSHA256: capture.Evidence.RawSHA256,
		},
		EvidenceEnvelope:       append([]byte(nil), capture.EvidenceEnvelope...),
		EvidenceEnvelopeSHA256: capture.EvidenceEnvelopeSHA256,
		ObservedRevisions:      observed,
	}
}

func captureMatchesAcquisition(capture lyricssource.RecoveryCapture, acquired lyricsacquisition.Acquisition) error {
	expected := recordInputFromCapture(capture)
	if acquired.Request != expected.Request || acquired.FetchedAt != expected.FetchedAt ||
		acquired.RawResponseSHA256 != expected.RawResponseSHA256 ||
		!bytes.Equal(acquired.RawResponse, expected.RawResponse) ||
		acquired.Evidence.EvidenceID != expected.Evidence.EvidenceID ||
		acquired.Evidence.RawSHA256 != expected.Evidence.RawSHA256 ||
		!bytes.Equal(acquired.Evidence.Raw, expected.Evidence.Raw) ||
		acquired.EvidenceEnvelopeSHA256 != expected.EvidenceEnvelopeSHA256 ||
		!bytes.Equal(acquired.EvidenceEnvelope, expected.EvidenceEnvelope) ||
		len(acquired.ObservedRevisions) != len(expected.ObservedRevisions) {
		return errors.New("offline replay acquisition conflicts with recaptured exact response")
	}
	for index := range acquired.ObservedRevisions {
		if acquired.ObservedRevisions[index] != expected.ObservedRevisions[index] {
			return errors.New("offline replay observed revisions conflict with the exact response")
		}
	}
	return nil
}

func cloneAcquisition(acquired lyricsacquisition.Acquisition) lyricsacquisition.Acquisition {
	acquired.RawResponse = append([]byte(nil), acquired.RawResponse...)
	acquired.Evidence.Raw = append([]byte(nil), acquired.Evidence.Raw...)
	acquired.EvidenceEnvelope = append([]byte(nil), acquired.EvidenceEnvelope...)
	acquired.ObservedRevisions = append([]lyricsacquisition.ObservedRevision(nil), acquired.ObservedRevisions...)
	return acquired
}
