package lyricsprovidercoord

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

const (
	coordHelperEnv = "MOESEKAI_LYRICS_COORD_HELPER"
	coordRootEnv   = "MOESEKAI_LYRICS_COORD_ROOT"
)

func TestMissingUnprovisionedAndCorruptStateAreHold(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if owner, err := Acquire(missing); owner != nil || !errors.Is(err, ErrHold) {
		t.Fatalf("missing root owner=%v err=%v", owner, err)
	}

	root := provisionTestRootV1(t)
	if err := os.Remove(filepath.Join(root, providerRecordFilesV1[lyricsproviderpolicy.ProviderMoegirl])); err != nil {
		t.Fatal(err)
	}
	if owner, err := Acquire(root); owner != nil || !errors.Is(err, ErrHold) {
		t.Fatalf("unprovisioned root owner=%v err=%v", owner, err)
	}

	root = provisionTestRootV1(t)
	path := filepath.Join(root, providerRecordFilesV1[lyricsproviderpolicy.ProviderSekaipedia])
	if err := os.WriteFile(path, []byte(`{"schemaVersion":"corrupt"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if owner, err := Acquire(root); owner != nil || !errors.Is(err, ErrHold) {
		t.Fatalf("corrupt root owner=%v err=%v", owner, err)
	}
}

func TestRetainedOwnerRejectsLockPathReplacement(t *testing.T) {
	root := provisionTestRootV1(t)
	owner, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	lockPath := filepath.Join(root, globalLockFileV1)
	movedPath := lockPath + ".moved"
	if err := os.Rename(lockPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("replacement lock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Wrap(lyricsproviderpolicy.ProviderSekaipedia, roundTripFuncV1(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("must not run")
	})); !errors.Is(err, ErrHold) {
		t.Fatalf("lock path replacement error=%v", err)
	}
}

func TestRetainedGlobalOwnershipAcrossSubprocesses(t *testing.T) {
	if os.Getenv(coordHelperEnv) == "1" {
		runCoordLockHelper(t)
		return
	}
	root := provisionTestRootV1(t)
	command := exec.Command(os.Args[0], "-test.run=^TestRetainedGlobalOwnershipAcrossSubprocesses$")
	command.Env = append(os.Environ(), coordHelperEnv+"=1", coordRootEnv+"="+root)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !finished {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatalf("coord helper output=%q err=%v", scanner.Text(), scanner.Err())
	}
	second, err := Acquire(root)
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, ErrHold) {
		t.Fatalf("second process acquired retained global lock: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true

	reacquired, err := Acquire(root)
	if err != nil {
		t.Fatalf("reacquire after subprocess exit: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderEndpointAndMaxlagCannotBeEvaded(t *testing.T) {
	root := provisionTestRootV1(t)
	owner, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var requests atomic.Int32
	wrapped, err := owner.Wrap(lyricsproviderpolicy.ProviderSekaipedia, roundTripFuncV1(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("must not reach underlying transport")
	}))
	if err != nil {
		t.Fatal(err)
	}
	for name, requestURL := range map[string]string{
		"wrong endpoint":   "https://www.sekaipedia.org/api.php?action=query&format=json&maxlag=5",
		"missing maxlag":   "https://www.sekaipedia.org/w/api.php?action=query&format=json",
		"duplicate maxlag": "https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5&maxlag=5",
	} {
		t.Run(name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, requestURL, nil)
			if response, err := wrapped.RoundTrip(request); response != nil || !errors.Is(err, ErrHold) {
				t.Fatalf("response=%v err=%v", response, err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("unsafe provider requests reached HTTP %d times", requests.Load())
	}
}

func TestAdmissionIsDurableBeforeLocalHTTPAnd429503RetryAfterIsComplete(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter func(time.Time) (string, time.Time)
	}{
		{
			name: "429 delay-seconds", status: http.StatusTooManyRequests,
			retryAfter: func(now time.Time) (string, time.Time) { return "7200", now.Add(2 * time.Hour) },
		},
		{
			name: "503 HTTP-date", status: http.StatusServiceUnavailable,
			retryAfter: func(now time.Time) (string, time.Time) {
				want := now.Add(3 * time.Hour).Truncate(time.Second)
				return want.Format(http.TimeFormat), want
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := provisionTestRootV1(t)
			provider := lyricsproviderpolicy.ProviderSekaipedia
			observed := make(chan error, 1)
			requestTime := time.Now().UTC()
			header, expectedFloor := test.retryAfter(requestTime)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record, err := readTestRecordV1(provider, root)
				if err == nil && (record.State != stateAdmittedV1 || record.Admission == nil || record.Admission.RequestSHA256 == "") {
					err = errors.New("request reached HTTP before durable admission")
				}
				observed <- err
				w.Header().Set("Retry-After", header)
				http.Error(w, "provider overload", test.status)
			}))
			defer server.Close()

			owner, err := Acquire(root)
			if err != nil {
				t.Fatal(err)
			}
			wrapped, err := owner.Wrap(provider, localServerTransportV1(t, server))
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: wrapped}
			response, err := client.Get(canonicalTestRequestURLV1(t, provider, "action=query&format=json&maxlag=5"))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if err := <-observed; err != nil {
				t.Fatal(err)
			}
			if err := owner.ResolveProvider(provider); err != nil {
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}

			record, err := readTestRecordV1(provider, root)
			if err != nil {
				t.Fatal(err)
			}
			notBefore, err := parseCanonicalTimeV1(record.NotBefore)
			if err != nil {
				t.Fatal(err)
			}
			if record.State != stateIdleV1 || record.Admission != nil || record.FailureCount != 1 || notBefore.Before(expectedFloor) {
				t.Fatalf("completed overload state=%+v notBefore=%s floor=%s", record, notBefore, expectedFloor)
			}
		})
	}
}

func TestMaxlagRetryAfterExtendsDurableCooldownWithoutCap(t *testing.T) {
	root := provisionTestRootV1(t)
	provider := lyricsproviderpolicy.ProviderMoegirl
	const retrySeconds = 90 * 24 * 60 * 60
	before := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("maxlag") != "5" {
			http.Error(w, "missing maxlag", http.StatusBadRequest)
			return
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySeconds))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"maxlag","info":"loaded"}}`)
	}))
	defer server.Close()

	owner, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := owner.Wrap(provider, localServerTransportV1(t, server))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: wrapped}).Get(canonicalTestRequestURLV1(t, provider, "action=query&format=json&maxlag=5"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || !strings.Contains(string(body), "maxlag") {
		t.Fatalf("maxlag response body=%q err=%v", body, err)
	}
	if err := owner.ResolveProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := readTestRecordV1(provider, root)
	if err != nil {
		t.Fatal(err)
	}
	notBefore, _ := parseCanonicalTimeV1(record.NotBefore)
	if notBefore.Before(before.Add(retrySeconds*time.Second)) || record.FailureCount != 1 || record.State != stateIdleV1 {
		t.Fatalf("maxlag durable state=%+v notBefore=%s", record, notBefore)
	}
}

func TestObservedResponseRemainsHoldUntilHigherLevelResolution(t *testing.T) {
	root := provisionTestRootV1(t)
	provider := lyricsproviderpolicy.ProviderSekaipedia
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	owner, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := owner.Wrap(provider, localServerTransportV1(t, server))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: wrapped}).Get(canonicalTestRequestURLV1(t, provider, "action=query&format=json&maxlag=5"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if next, err := Acquire(root); next != nil || !errors.Is(err, ErrHold) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("observed but unresolved response did not remain HOLD: %v", err)
	}
}

func TestOnlyOneActualProviderHTTPRequestCanBeInFlight(t *testing.T) {
	root := provisionTestRootV1(t)
	provider := lyricsproviderpolicy.ProviderVocaloidFandom
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	var active atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if active.Add(1) != 1 {
			active.Add(-1)
			http.Error(w, "parallel request", http.StatusInternalServerError)
			return
		}
		defer active.Add(-1)
		close(started)
		<-release
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	owner, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	wrapped, err := owner.Wrap(provider, localServerTransportV1(t, server))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: wrapped}
	firstErr := make(chan error, 1)
	go func() {
		response, err := client.Get(canonicalTestRequestURLV1(t, provider, "action=query&format=json&maxlag=5&request=first"))
		if response != nil {
			_ = response.Body.Close()
		}
		firstErr <- err
	}()
	<-started
	second, err := client.Get(canonicalTestRequestURLV1(t, provider, "action=query&format=json&maxlag=5&request=second"))
	if second != nil {
		_ = second.Body.Close()
	}
	if !errors.Is(err, ErrHold) {
		t.Fatalf("parallel provider request error=%v", err)
	}
	if requests.Load() != 1 || active.Load() != 1 {
		t.Fatalf("actual provider requests=%d active=%d", requests.Load(), active.Load())
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	if err := owner.ResolveProvider(provider); err != nil {
		t.Fatal(err)
	}
}

func TestTransportFailureLeavesDurableAdmissionHold(t *testing.T) {
	root := provisionTestRootV1(t)
	provider := lyricsproviderpolicy.ProviderSekaipedia
	owner, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := owner.Wrap(provider, roundTripFuncV1(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection outcome unknown")
	}))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://www.sekaipedia.org/w/api.php?action=query&format=json&maxlag=5", nil)
	if response, err := wrapped.RoundTrip(request); response != nil || !errors.Is(err, ErrHold) {
		t.Fatalf("unresolved response=%v err=%v", response, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := readTestRecordV1(provider, root)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != stateAdmittedV1 || record.Admission == nil {
		t.Fatalf("unresolved admission was cleared: %+v", record)
	}
	if next, err := Acquire(root); next != nil || !errors.Is(err, ErrHold) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("unresolved admission did not HOLD reacquisition: %v", err)
	}
}

func runCoordLockHelper(t *testing.T) {
	owner, err := Acquire(os.Getenv(coordRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	fmt.Fprintln(os.Stdout, "locked")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func provisionTestRootV1(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "lyrics-provider-coord-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.WriteFile(filepath.Join(root, globalLockFileV1), []byte("retained global lock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, provider := range orderedProvidersV1() {
		record := providerRecordV1{
			SchemaVersion: stateSchemaVersionV1,
			Provider:      provider,
			Generation:    1,
			State:         stateIdleV1,
			NotBefore:     canonicalTimeV1(time.Unix(1, 0)),
			FailureCount:  0,
		}
		body, err := encodeRecordV1(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, providerRecordFilesV1[provider]), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readTestRecordV1(provider lyricsproviderpolicy.Provider, root string) (providerRecordV1, error) {
	body, err := os.ReadFile(filepath.Join(root, providerRecordFilesV1[provider]))
	if err != nil {
		return providerRecordV1{}, err
	}
	return decodeRecordV1(provider, body)
}

func canonicalTestRequestURLV1(t *testing.T, provider lyricsproviderpolicy.Provider, rawQuery string) string {
	t.Helper()
	endpoint, found := lyricsproviderpolicy.CanonicalEndpointV1(provider)
	if !found {
		t.Fatalf("provider %q has no canonical endpoint", provider)
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil || query.Encode() != rawQuery {
		t.Fatalf("test query %q is not canonical: %v", rawQuery, err)
	}
	return endpoint + "?" + rawQuery
}

func localServerTransportV1(t *testing.T, server *httptest.Server) http.RoundTripper {
	t.Helper()
	local, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return roundTripFuncV1(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		target := *request.URL
		target.Scheme = local.Scheme
		target.Host = local.Host
		clone.URL = &target
		clone.Host = ""
		response, err := server.Client().Transport.RoundTrip(clone)
		if response != nil {
			response.Request = request
		}
		return response, err
	})
}

type roundTripFuncV1 func(*http.Request) (*http.Response, error)

func (function roundTripFuncV1) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
