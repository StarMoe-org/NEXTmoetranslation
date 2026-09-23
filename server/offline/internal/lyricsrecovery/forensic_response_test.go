package lyricsrecovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestForensicResponseStoreRestartReplayIdempotenceAndTamper(t *testing.T) {
	root := privateRecoveryTempDir(t)
	path := filepath.Join(root, "ledger") + ForensicResponseStoreSuffix
	store, err := openOrCreateForensicResponseStore(path)
	if err != nil {
		t.Fatal(err)
	}
	response := lyricssource.RecoveryHTTPResponse{
		Action:              "page",
		CanonicalRequestURL: exactSekaipediaSongCanaryURL(330574),
		FetchedAt:           time.Date(2026, 8, 3, 8, 21, 34, 123_456_000, time.UTC),
		StatusCode:          http.StatusServiceUnavailable,
		Status:              "503 Service Unavailable",
		Header: http.Header{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Retry-After":   {"17"},
			"X-Exact-Value": {"first", "second"},
		},
		Raw: []byte{0x7b, 0x22, 0x65, 0x72, 0x72, 0x6f, 0x72},
	}
	first, err := store.Commit(t.Context(), model.LyricsSourceProviderSekaipedia, response)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Commit(t.Context(), model.LyricsSourceProviderSekaipedia, response)
	if err != nil || second != first {
		t.Fatalf("idempotent forensic response commit changed identity: first=%+v second=%+v err=%v", first, second, err)
	}

	reopened, err := OpenForensicResponseStore(path)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := reopened.Replay(t.Context(), first.ResponseID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Provider != model.LyricsSourceProviderSekaipedia || replayed.Action != response.Action ||
		replayed.CanonicalRequestURL != response.CanonicalRequestURL ||
		replayed.FetchedAt != response.FetchedAt.Format(time.RFC3339Nano) ||
		replayed.StatusCode != response.StatusCode || replayed.Status != response.Status ||
		!reflect.DeepEqual(replayed.Header, response.Header) || !bytes.Equal(replayed.Raw, response.Raw) {
		t.Fatal("restarted forensic response replay did not preserve the exact completed response")
	}
	if entries := forensicResponseRegularEntries(t, filepath.Join(path, forensicResponseBlobDirectory)); entries != 1 {
		t.Fatalf("idempotent forensic raw publication created %d blobs", entries)
	}
	if entries := forensicResponseRegularEntries(t, filepath.Join(path, forensicResponseRecordDirectory)); entries != 1 {
		t.Fatalf("idempotent forensic manifest publication created %d records", entries)
	}

	blobPath := filepath.Join(path, forensicResponseBlobDirectory, first.RawResponseSHA256+".json")
	body, err := os.ReadFile(blobPath)
	if err != nil || len(body) < 2 {
		t.Fatalf("read forensic raw artifact for tamper test: bytes=%d err=%v", len(body), err)
	}
	body[len(body)-2] ^= 1
	if err := os.WriteFile(blobPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, firstErr := reopened.Replay(t.Context(), first.ResponseID)
	_, secondErr := reopened.Replay(t.Context(), first.ResponseID)
	if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() {
		t.Fatalf("forensic tamper replay was not deterministically closed: first=%v second=%v", firstErr, secondErr)
	}
}

func TestSekaipediaCanaryRetainsMalformedAndNonSuccessSongResponsesBeforeClosedOutcome(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		status     string
		raw        []byte
		header     http.Header
		wantStatus lyricsprovideroutcome.Status
		wantReason lyricsprovideroutcome.ReasonCode
	}{
		{
			name: "malformed JSON", statusCode: http.StatusOK, status: "200 OK",
			raw: []byte{0x7b}, header: http.Header{"Content-Type": {"application/json"}},
			wantStatus: lyricsprovideroutcome.StatusUnsupported, wantReason: lyricsprovideroutcome.ReasonMalformedResponse,
		},
		{
			name: "non success", statusCode: http.StatusServiceUnavailable, status: "503 Service Unavailable",
			raw:        []byte{0x7b, 0x22, 0x65, 0x72, 0x72, 0x6f, 0x72, 0x22, 0x3a, 0x7b, 0x7d, 0x7d},
			header:     http.Header{"Content-Type": {"application/json"}, "Retry-After": {"19"}, "X-Forensic": {"one", "two"}},
			wantStatus: lyricsprovideroutcome.StatusTransportError, wantReason: lyricsprovideroutcome.ReasonTransport,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSekaipediaCanaryFixture(t, []int{2})
			list := fixture.runtime.SekaipediaCanary.List
			listRaw := sekaipediaEnvelopePageInfoDrift(t, mustFixture(t, "sekaipedia-list-335193.json"))
			sourceLedger, source := commitHistoricalSekaipediaListAcquisition(
				t, listRaw, exactSekaipediaListCanaryURL(list.RevisionID),
			)
			sourceReplay := replayCanaryAcquisition(t, t.Context(), sourceLedger, source.AcquisitionID)
			target := fixture.songs[2]
			fetchedAt := time.Date(2026, 8, 3, 8, 27, 19, 987_654_000, time.UTC)
			live := &forensicCanarySongTransport{
				canonicalRequestURL: exactSekaipediaSongCanaryURL(target.RevisionID),
				fetchedAt:           fetchedAt, statusCode: test.statusCode, status: test.status,
				header: test.header.Clone(), raw: append([]byte(nil), test.raw...),
			}
			replay, err := NewPlanBoundSekaipediaListReplayTransport(sourceReplay, list, live)
			if err != nil {
				t.Fatal(err)
			}
			fixture.transports[model.LyricsSourceProviderSekaipedia] = replay
			session, err := NewAcquisitionSession(fixture.runtime, fixture.ledger, fixture.transports)
			if err != nil {
				t.Fatal(err)
			}
			sets, progress, diagnostic, err := session.AcquireSekaipediaCanarySong(
				t.Context(), 2, fixture.identities[2],
			)
			if err != nil {
				t.Fatal(err)
			}
			requestCount, requestURL := live.snapshot()
			if !replay.Consumed() || requestCount != 1 || requestURL != live.canonicalRequestURL ||
				len(sets) != 1 || len(progress) != 1 || len(sets[0].AcquisitionIDs) != 1 ||
				sets[0].Status != test.wantStatus || sets[0].ReasonCode != test.wantReason ||
				progress[0].EnterResult != ProviderOutcomeFailClosed || progress[0].FallbackReasonCode != "" ||
				diagnostic.EnterResult != ProviderOutcomeFailClosed || diagnostic.Song != nil ||
				len(diagnostic.ForensicResponses) != 2 {
				t.Fatalf("closed canary response boundary mismatch: requests=%d providers=%+v progress=%+v diagnostic=%+v", requestCount, sets, progress, diagnostic)
			}
			if fixture.transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
				fixture.transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
				t.Fatal("closed Sekaipedia canary response contacted a fallback provider")
			}

			ledgerRoot, err := fixture.ledger.RootPath()
			if err != nil {
				t.Fatal(err)
			}
			storePath, err := ForensicResponseStorePath(ledgerRoot)
			if err != nil {
				t.Fatal(err)
			}
			store, err := OpenForensicResponseStore(storePath)
			if err != nil {
				t.Fatal(err)
			}
			retained, err := store.Replay(t.Context(), diagnostic.ForensicResponses[1].ResponseID)
			if err != nil {
				t.Fatal(err)
			}
			if retained.Provider != model.LyricsSourceProviderSekaipedia || retained.Action != "page" ||
				retained.CanonicalRequestURL != live.canonicalRequestURL || retained.FetchedAt != fetchedAt.Format(time.RFC3339Nano) ||
				retained.StatusCode != test.statusCode || retained.Status != test.status ||
				!reflect.DeepEqual(retained.Header, test.header) || !bytes.Equal(retained.Raw, test.raw) {
				t.Fatal("closed Sekaipedia canary did not retain byte-exact private forensic response evidence")
			}
			body, err := MarshalSekaipediaCanaryDiagnostic(diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			lower := bytes.ToLower(body)
			if (len(test.raw) >= 8 && bytes.Contains(body, test.raw)) || bytes.Contains(body, []byte(`"raw"`)) ||
				bytes.Contains(lower, []byte("romaji")) || bytes.Contains(lower, []byte("romanization")) ||
				bytes.Contains(lower, []byte("romanized")) {
				t.Fatal("closed Sekaipedia canary diagnostic exposed private response bytes or a forbidden field")
			}
		})
	}
}

func TestSekaipediaCanaryWrongReturnedRevisionFailsClosedWithoutFallback(t *testing.T) {
	fixture := newSekaipediaCanaryFixture(t, []int{2})
	list := fixture.runtime.SekaipediaCanary.List
	listRaw := sekaipediaEnvelopePageInfoDrift(t, mustFixture(t, "sekaipedia-list-335193.json"))
	sourceLedger, source := commitHistoricalSekaipediaListAcquisition(
		t, listRaw, exactSekaipediaListCanaryURL(list.RevisionID),
	)
	sourceReplay := replayCanaryAcquisition(t, t.Context(), sourceLedger, source.AcquisitionID)
	target := fixture.songs[2]
	wrong := sekaipediaRevisionIDDrift(t, mustFixture(t, "sekaipedia-roki-330574.json"))
	live := &forensicCanarySongTransport{
		canonicalRequestURL: exactSekaipediaSongCanaryURL(target.RevisionID),
		fetchedAt:           time.Date(2026, 8, 3, 8, 31, 44, 123_000_000, time.UTC),
		statusCode:          http.StatusOK,
		status:              "200 OK",
		header:              http.Header{"Content-Type": {"application/json"}},
		raw:                 wrong,
	}
	replay, err := NewPlanBoundSekaipediaListReplayTransport(sourceReplay, list, live)
	if err != nil {
		t.Fatal(err)
	}
	fixture.transports[model.LyricsSourceProviderSekaipedia] = replay
	session, err := NewAcquisitionSession(fixture.runtime, fixture.ledger, fixture.transports)
	if err != nil {
		t.Fatal(err)
	}
	sets, progress, diagnostic, err := session.AcquireSekaipediaCanarySong(t.Context(), 2, fixture.identities[2])
	if err != nil {
		t.Fatal(err)
	}
	requestCount, requestURL := live.snapshot()
	if requestCount != 1 || requestURL != live.canonicalRequestURL || len(sets) != 1 || len(progress) != 1 ||
		sets[0].Status == lyricsprovideroutcome.StatusCandidate || len(sets[0].AcquisitionIDs) != 1 ||
		progress[0].EnterResult != ProviderOutcomeFailClosed || progress[0].FallbackReasonCode != "" ||
		diagnostic.EnterResult != ProviderOutcomeFailClosed || diagnostic.Song != nil ||
		len(diagnostic.ForensicResponses) != 2 {
		t.Fatalf("wrong returned revision did not fail closed: requests=%d providers=%+v progress=%+v diagnostic=%+v", requestCount, sets, progress, diagnostic)
	}
	if fixture.transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		fixture.transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("wrong returned Sekaipedia revision contacted a fallback provider")
	}
}

type forensicCanarySongTransport struct {
	mu                  sync.Mutex
	canonicalRequestURL string
	fetchedAt           time.Time
	statusCode          int
	status              string
	header              http.Header
	raw                 []byte
	requests            int
	requestURL          string
}

func (transport *forensicCanarySongTransport) recoveryOfflineFixture() bool { return true }

func (transport *forensicCanarySongTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.requests++
	if request != nil && request.URL != nil {
		transport.requestURL = request.URL.String()
	}
	transport.mu.Unlock()
	if request == nil || request.Method != http.MethodGet || request.Body != nil || request.URL == nil ||
		request.URL.String() != transport.canonicalRequestURL {
		return nil, errors.New("forensic canary received a non-exact song revision request")
	}
	return &http.Response{
		StatusCode: transport.statusCode,
		Status:     transport.status,
		Header:     transport.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(transport.raw)),
		Request:    request,
	}, nil
}

func (transport *forensicCanarySongTransport) RecoveryAcquisitionFetchedAt(
	request *http.Request,
	response *http.Response,
) (time.Time, bool, error) {
	if request == nil || response == nil || response.Request != request || transport.fetchedAt.IsZero() ||
		transport.fetchedAt.Location() != time.UTC {
		return time.Time{}, false, errors.New("forensic canary fetchedAt binding is invalid")
	}
	return transport.fetchedAt, true, nil
}

func (transport *forensicCanarySongTransport) snapshot() (int, string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.requests, transport.requestURL
}

func forensicResponseRegularEntries(t *testing.T, directory string) int {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			count++
		}
	}
	return count
}

func sekaipediaRevisionIDDrift(t *testing.T, body []byte) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	query, queryOK := envelope["query"].(map[string]any)
	pages, pagesOK := query["pages"].([]any)
	if !queryOK || !pagesOK || len(pages) != 1 {
		t.Fatal("exact revision mutation fixture has no single page")
	}
	page, pageOK := pages[0].(map[string]any)
	revisions, revisionsOK := page["revisions"].([]any)
	if !pageOK || !revisionsOK || len(revisions) != 1 {
		t.Fatal("exact revision mutation fixture has no single revision")
	}
	revision, revisionOK := revisions[0].(map[string]any)
	revisionID, revisionIDOK := revision["revid"].(float64)
	if !revisionOK || !revisionIDOK || revisionID <= 0 {
		t.Fatal("exact revision mutation fixture has no revision identity")
	}
	revision["revid"] = revisionID + 1
	result, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
