package lyricsrecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsprovidercoord"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

func TestSekaipediaCanaryTransportRejectsNonExactMaxlagBeforeHTTP(t *testing.T) {
	ledger := newSekaipediaCanaryTransportLedger(t)
	var actual atomic.Int32
	underlying := sekaipediaCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		actual.Add(1)
		return sekaipediaCanaryHTTPResponse(request, http.StatusOK, nil, []byte(`{}`)), nil
	})
	transport, err := NewAcquisitionTransport(model.LyricsSourceProviderSekaipedia, nil, ledger, underlying)
	if err != nil {
		t.Fatal(err)
	}

	requests := []*http.Request{
		mustSekaipediaCanaryRequest(t, http.MethodGet, "https://www.sekaipedia.org/w/api.php?action=query&format=json", nil),
		mustSekaipediaCanaryRequest(t, http.MethodGet, "https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=4", nil),
		mustSekaipediaCanaryRequest(t, http.MethodGet, "https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5&maxlag=5", nil),
		mustSekaipediaCanaryRequest(t, http.MethodPost, "https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil),
		mustSekaipediaCanaryRequest(t, http.MethodGet, "https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", bytes.NewReader([]byte("body"))),
	}
	for index, request := range requests {
		if response, err := transport.RoundTrip(request); err == nil || response != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			t.Fatalf("invalid maxlag request %d response=%v err=%v", index, response, err)
		}
	}
	if actual.Load() != 0 {
		t.Fatalf("noncanonical maxlag reached actual HTTP %d times", actual.Load())
	}

	request := mustSekaipediaCanaryRequest(t, http.MethodGet,
		"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if actual.Load() != 1 {
		t.Fatalf("exact maxlag actual HTTP count=%d", actual.Load())
	}
}

func TestSekaipediaCanaryTransportHoldsProviderSlotThroughResponseBody(t *testing.T) {
	ledger := newSekaipediaCanaryTransportLedger(t)
	var actual atomic.Int32
	underlying := sekaipediaCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		actual.Add(1)
		return sekaipediaCanaryHTTPResponse(request, http.StatusOK, nil, []byte(`{"ok":true}`)), nil
	})
	transport, err := NewAcquisitionTransport(model.LyricsSourceProviderSekaipedia, nil, ledger, underlying)
	if err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		return mustSekaipediaCanaryRequest(t, http.MethodGet,
			"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil)
	}
	first, err := transport.RoundTrip(request())
	if err != nil {
		t.Fatal(err)
	}
	if second, err := transport.RoundTrip(request()); err == nil || second != nil {
		if second != nil && second.Body != nil {
			_ = second.Body.Close()
		}
		t.Fatalf("second request entered before first response body closed: response=%v err=%v", second, err)
	}
	if actual.Load() != 1 {
		t.Fatalf("parallel body-lifetime actual HTTP count=%d", actual.Load())
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := transport.RoundTrip(request())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(third.Body)
	_ = third.Body.Close()
	if actual.Load() != 2 {
		t.Fatalf("released body-lifetime actual HTTP count=%d", actual.Load())
	}
}

func TestSekaipediaCanaryCoordinatedTransportRetainsOwnershipAndFullRetryAfter(t *testing.T) {
	coordRoot := provisionSekaipediaCanaryCoordRoot(t)
	owner, err := lyricsprovidercoord.Acquire(coordRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if second, err := lyricsprovidercoord.Acquire(coordRoot); second != nil || !errors.Is(err, lyricsprovidercoord.ErrHold) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second participating live owner was admitted: %v", err)
	}

	const retryAfterSeconds = 2 * 60 * 60
	var actual atomic.Int32
	var active atomic.Int32
	underlying := sekaipediaCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		actual.Add(1)
		if active.Add(1) != 1 {
			active.Add(-1)
			return nil, errors.New("parallel actual provider request")
		}
		defer active.Add(-1)
		header := make(http.Header)
		header.Set("Retry-After", "7200")
		return sekaipediaCanaryHTTPResponse(request, http.StatusTooManyRequests, header, []byte(`{}`)), nil
	})
	coordinated, err := owner.Wrap(lyricsproviderpolicy.ProviderSekaipedia, underlying)
	if err != nil {
		t.Fatal(err)
	}
	ledger := newSekaipediaCanaryTransportLedger(t)
	transport, err := NewAcquisitionTransport(model.LyricsSourceProviderSekaipedia, nil, ledger, coordinated)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	request := mustSekaipediaCanaryRequest(t, http.MethodGet,
		"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if actual.Load() != 1 || active.Load() != 0 {
		t.Fatalf("coordinated actual requests=%d active=%d", actual.Load(), active.Load())
	}

	record := readSekaipediaCanaryCoordRecord(t, coordRoot)
	notBefore, err := time.Parse(time.RFC3339Nano, record.NotBefore)
	if err != nil || record.State != "admitted" || record.FailureCount != 1 || record.Admission == nil ||
		notBefore.Before(before.Add(retryAfterSeconds*time.Second)) {
		t.Fatalf("full Retry-After admitted state=%+v notBefore=%s err=%v", record, notBefore, err)
	}
	if err := owner.ResolveProvider(lyricsproviderpolicy.ProviderSekaipedia); err != nil {
		t.Fatal(err)
	}
	resolved := readSekaipediaCanaryCoordRecord(t, coordRoot)
	if resolved.State != "idle" || resolved.FailureCount != 1 || resolved.Admission != nil ||
		resolved.NotBefore != record.NotBefore {
		t.Fatalf("resolved Retry-After state=%+v admitted=%+v", resolved, record)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	blocked := mustSekaipediaCanaryRequest(t, http.MethodGet,
		"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil).WithContext(ctx)
	if response, err := transport.RoundTrip(blocked); response != nil || !errors.Is(err, lyricsprovidercoord.ErrHold) {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("durable Retry-After request response=%v err=%v", response, err)
	}
	if actual.Load() != 1 {
		t.Fatalf("durable Retry-After was shortened; actual requests=%d", actual.Load())
	}
}

func TestSekaipediaCanaryOwnerRestartPreservesFutureRetryAfterAndMaxlagCooldown(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "retry-after", status: http.StatusTooManyRequests, body: []byte(`{}`)},
		{name: "maxlag", status: http.StatusOK, body: []byte(`{"error":{"code":"maxlag","info":"loaded"}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := provisionSekaipediaCanaryCoordRoot(t)
			owner, err := lyricsprovidercoord.Acquire(root)
			if err != nil {
				t.Fatal(err)
			}
			var actual atomic.Int32
			underlying := sekaipediaCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				actual.Add(1)
				header := make(http.Header)
				header.Set("Retry-After", "7200")
				return sekaipediaCanaryHTTPResponse(request, test.status, header, test.body), nil
			})
			coordinated, err := owner.Wrap(lyricsproviderpolicy.ProviderSekaipedia, underlying)
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC()
			request := mustSekaipediaCanaryRequest(t, http.MethodGet,
				"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil)
			response, err := coordinated.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(response.Body)
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if err := owner.ResolveProvider(lyricsproviderpolicy.ProviderSekaipedia); err != nil {
				t.Fatal(err)
			}
			persisted := readSekaipediaCanaryCoordRecord(t, root)
			notBefore, err := time.Parse(time.RFC3339Nano, persisted.NotBefore)
			if err != nil || persisted.State != "idle" || persisted.FailureCount != 1 ||
				notBefore.Before(before.Add(2*time.Hour)) {
				t.Fatalf("%s persisted cooldown=%+v notBefore=%s err=%v", test.name, persisted, notBefore, err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}

			restarted, err := lyricsprovidercoord.Acquire(root)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restartedTransport, err := restarted.Wrap(lyricsproviderpolicy.ProviderSekaipedia, underlying)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			blocked := mustSekaipediaCanaryRequest(t, http.MethodGet,
				"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil).WithContext(ctx)
			if response, err := restartedTransport.RoundTrip(blocked); response != nil || !errors.Is(err, lyricsprovidercoord.ErrHold) {
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				t.Fatalf("%s restarted cooldown response=%v err=%v", test.name, response, err)
			}
			if actual.Load() != 1 {
				t.Fatalf("%s owner restart shortened cooldown; actual requests=%d", test.name, actual.Load())
			}
			after := readSekaipediaCanaryCoordRecord(t, root)
			if after.NotBefore != persisted.NotBefore || after.FailureCount != persisted.FailureCount || after.State != persisted.State {
				t.Fatalf("%s owner restart changed cooldown: before=%+v after=%+v", test.name, persisted, after)
			}
		})
	}
}

func newSekaipediaCanaryTransportLedger(t *testing.T) *lyricsacquisition.Ledger {
	t.Helper()
	root := privateRecoveryTempDir(t)
	ledger, err := lyricsacquisition.CreateLedger(t.Context(), filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger
}

type sekaipediaCanaryCoordRecord struct {
	State        string `json:"state"`
	NotBefore    string `json:"notBefore"`
	FailureCount int    `json:"failureCount"`
	Admission    any    `json:"admission"`
}

func readSekaipediaCanaryCoordRecord(t *testing.T, root string) sekaipediaCanaryCoordRecord {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "sekaipedia.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record sekaipediaCanaryCoordRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func provisionSekaipediaCanaryCoordRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "lyrics-sekaipedia-canary-coord-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.WriteFile(filepath.Join(root, "global-live.lock"), []byte("retained global lock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for provider, name := range map[string]string{
		"vocaloid_fandom": "vocaloid_fandom.json",
		"moegirl":         "moegirl.json",
		"sekaipedia":      "sekaipedia.json",
	} {
		record := struct {
			SchemaVersion string `json:"schemaVersion"`
			Provider      string `json:"provider"`
			Generation    int    `json:"generation"`
			State         string `json:"state"`
			NotBefore     string `json:"notBefore"`
			FailureCount  int    `json:"failureCount"`
		}{
			SchemaVersion: "lyrics-provider-live-state/v1", Provider: provider, Generation: 1,
			State: "idle", NotBefore: time.Unix(1, 0).UTC().Format(time.RFC3339Nano),
		}
		body, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustSekaipediaCanaryRequest(t *testing.T, method, rawURL string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func sekaipediaCanaryHTTPResponse(
	request *http.Request,
	status int,
	header http.Header,
	body []byte,
) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: header,
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request,
	}
}

type sekaipediaCanaryRoundTripFunc func(*http.Request) (*http.Response, error)

func (function sekaipediaCanaryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
