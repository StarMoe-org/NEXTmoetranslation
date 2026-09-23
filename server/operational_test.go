package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lifecycle"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/workspaceverify"
)

const (
	verifyWorkspaceHelperEnv        = "MOESEKAI_VERIFY_WORKSPACE_HELPER"
	verifyWorkspaceFailureHelperEnv = "MOESEKAI_VERIFY_WORKSPACE_FAILURE_HELPER"
	verifyRuntimeFailureHelperEnv   = "MOESEKAI_VERIFY_RUNTIME_FAILURE_HELPER"
	verifyWebDirFailureHelperEnv    = "MOESEKAI_VERIFY_WEB_DIR_FAILURE_HELPER"
	verifyDBPathFailureHelperEnv    = "MOESEKAI_VERIFY_DB_PATH_FAILURE_HELPER"
	verifyDataDirFailureHelperEnv   = "MOESEKAI_VERIFY_DATA_DIR_FAILURE_HELPER"
	verifyTimezoneFailureHelperEnv  = "MOESEKAI_VERIFY_TIMEZONE_FAILURE_HELPER"
	blockedProbeHelperEnv           = "MOESEKAI_BLOCKED_PROBE_HELPER"
)

type operationalProjection struct{ status filesvc.ProjectionStatus }

func (p *operationalProjection) Status() filesvc.ProjectionStatus { return p.status }

type operationalSearch struct{ status searchindex.Status }

func (s *operationalSearch) Status() searchindex.Status { return s.status }

type changingProjection struct{ calls int }

func (p *changingProjection) Status() filesvc.ProjectionStatus {
	p.calls++
	if p.calls == 1 {
		return filesvc.ProjectionStatus{Generation: 1}
	}
	return filesvc.ProjectionStatus{Generation: 1, Pending: true, LastError: "projection_generation_failed"}
}

func TestOperationalDetailsRequireAdminAndServerBoundsBodyRead(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, operationalDeps{database: database, auth: authService})

	public := httptest.NewRecorder()
	mux.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/healthz/details", nil))
	if public.Code != http.StatusUnauthorized {
		t.Fatalf("public health details status = %d", public.Code)
	}
	if public.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public health details cache = %q", public.Header().Get("Cache-Control"))
	}
	health := httptest.NewRecorder()
	mux.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != `{"status":"ok"}` {
		t.Fatalf("legacy health response status=%d body=%q", health.Code, health.Body.String())
	}
	if health.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health cache = %q", health.Header().Get("Cache-Control"))
	}

	server := newHTTPServer(":0", mux)
	if server.ReadTimeout != 30*time.Second || server.ReadHeaderTimeout != 10*time.Second || server.MaxHeaderBytes != 1<<20 {
		t.Fatalf("HTTP bounds read=%s header=%s maxHeader=%d", server.ReadTimeout, server.ReadHeaderTimeout, server.MaxHeaderBytes)
	}
}

func TestReadinessRequiresInitialProjectionButLivenessDoesNot(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	projection := &operationalProjection{}
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, operationalDeps{database: database, auth: authService, projection: projection})

	ready := httptest.NewRecorder()
	mux.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || ready.Body.String() != `{"status":"not_ready"}` || ready.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("initial readiness status=%d cache=%q body=%q", ready.Code, ready.Header().Get("Cache-Control"), ready.Body.String())
	}
	health := httptest.NewRecorder()
	mux.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("liveness before projection = %d", health.Code)
	}

	projection.status = filesvc.ProjectionStatus{Generation: 1, LastSuccessAt: time.Now().UTC().Format(time.RFC3339)}
	ready = httptest.NewRecorder()
	mux.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK || ready.Body.String() != `{"status":"ready"}` {
		t.Fatalf("published readiness status=%d body=%q", ready.Code, ready.Body.String())
	}

	projection.status = filesvc.ProjectionStatus{
		Generation: 1, Pending: true, LastSuccessAt: time.Now().UTC().Format(time.RFC3339),
	}
	ready = httptest.NewRecorder()
	mux.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || ready.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("pending latest projection readiness status=%d body=%q", ready.Code, ready.Body.String())
	}

	projection.status = filesvc.ProjectionStatus{
		Generation: 1, Pending: true, LastError: "projection_generation_failed",
		LastSuccessAt: time.Now().UTC().Format(time.RFC3339),
	}
	ready = httptest.NewRecorder()
	mux.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || ready.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("failed latest projection readiness status=%d body=%q", ready.Code, ready.Body.String())
	}

	projection.status = filesvc.ProjectionStatus{Generation: 2, LastSuccessAt: time.Now().UTC().Format(time.RFC3339)}
	ready = httptest.NewRecorder()
	mux.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK || ready.Body.String() != `{"status":"ready"}` {
		t.Fatalf("recovered projection readiness status=%d body=%q", ready.Code, ready.Body.String())
	}
}

func TestReadinessRechecksVolatileStateImmediatelyBeforeSuccess(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "readiness-recheck.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	projection := &changingProjection{}
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, operationalDeps{database: database, auth: authService, projection: projection})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != `{"status":"not_ready"}` || projection.calls != 2 {
		t.Fatalf("readiness recheck status=%d body=%q calls=%d", recorder.Code, recorder.Body.String(), projection.calls)
	}
}

func TestServerRejectsIncompleteSeedPublicationMarker(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	if err := rejectIncompleteSeed(databasePath); err != nil {
		t.Fatalf("marker-free database rejected: %v", err)
	}
	if err := os.WriteFile(databasePath+".seed-incomplete", []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectIncompleteSeed(databasePath); err == nil || !strings.Contains(err.Error(), "incomplete seed publication marker") {
		t.Fatalf("incomplete marker error = %v", err)
	}
}

func TestProductionReadinessRequiresSearchOrValidatedCache(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "search-readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	projection := &operationalProjection{status: filesvc.ProjectionStatus{Generation: 1}}
	search := &operationalSearch{}
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, operationalDeps{database: database, auth: authService, projection: projection, search: search})

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness without search = %d", recorder.Code)
	}
	search.status = searchindex.Status{Ready: true, Degraded: true, Generation: 1, Source: "cache"}
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"status":"ready"}` {
		t.Fatalf("readiness with cached search status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestShutdownConfigurationUsesOneStrictTotalBudget(t *testing.T) {
	t.Setenv("SHUTDOWN_BUDGET_MS", "80")
	t.Setenv("SHUTDOWN_DRAIN_MS", "15")
	config, err := shutdownConfigFromEnv()
	if err != nil || config.Budget != 80*time.Millisecond || config.Drain != 15*time.Millisecond {
		t.Fatalf("shutdown config=%+v err=%v", config, err)
	}
	t.Setenv("SHUTDOWN_DRAIN_MS", "80")
	if _, err := shutdownConfigFromEnv(); err == nil {
		t.Fatal("shutdown accepted a drain equal to the total budget")
	}
}

func TestLyricsDiscoveryConfigurationIsFailClosedAndStrict(t *testing.T) {
	for _, key := range []string{
		"LYRICS_DISCOVERY_SCAN_MS", "LYRICS_DISCOVERY_LEASE_MS", "LYRICS_DISCOVERY_JOB_TIMEOUT_MS",
		"LYRICS_DISCOVERY_IDLE_MS", "LYRICS_DISCOVERY_RETRY_MIN_MS", "LYRICS_DISCOVERY_RETRY_MAX_MS",
		"LYRICS_DISCOVERY_CONCURRENCY",
	} {
		t.Setenv(key, "")
	}
	options, err := lyricsDiscoveryOptionsFromEnv()
	if err != nil || options.ScanInterval <= 0 || options.JobTimeout <= 0 || options.JobTimeout >= options.LeaseDuration ||
		options.RetryMax < options.RetryMin || options.Concurrency != 4 {
		t.Fatalf("default lyrics discovery options=%+v err=%v", options, err)
	}
	t.Setenv("LYRICS_DISCOVERY_LEASE_MS", "10000")
	t.Setenv("LYRICS_DISCOVERY_JOB_TIMEOUT_MS", "10000")
	if _, err := lyricsDiscoveryOptionsFromEnv(); err == nil {
		t.Fatal("lyrics discovery accepted a job timeout equal to its lease")
	}
	t.Setenv("LYRICS_DISCOVERY_LEASE_MS", "20000")
	t.Setenv("LYRICS_DISCOVERY_JOB_TIMEOUT_MS", "1000")
	t.Setenv("LYRICS_DISCOVERY_RETRY_MIN_MS", "5000")
	t.Setenv("LYRICS_DISCOVERY_RETRY_MAX_MS", "1000")
	if _, err := lyricsDiscoveryOptionsFromEnv(); err == nil {
		t.Fatal("lyrics discovery accepted retry maximum below minimum")
	}
	t.Setenv("LYRICS_DISCOVERY_RETRY_MAX_MS", "5000")
	for _, invalid := range []string{"0", "17", "+4", "04", "four"} {
		t.Setenv("LYRICS_DISCOVERY_CONCURRENCY", invalid)
		if _, err := lyricsDiscoveryOptionsFromEnv(); err == nil {
			t.Fatalf("lyrics discovery accepted invalid concurrency %q", invalid)
		}
	}
}

func TestLyricsFetchRevisionConfigurationIsFailClosedAndStrict(t *testing.T) {
	for _, key := range []string{"LYRICS_FETCH_REVISION_LEASE_MS", "LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS",
		"LYRICS_FETCH_REVISION_IDLE_MS", "LYRICS_FETCH_REVISION_RETRY_MIN_MS", "LYRICS_FETCH_REVISION_RETRY_MAX_MS",
		"LYRICS_FETCH_REVISION_CONCURRENCY"} {
		t.Setenv(key, "")
	}
	options, err := lyricsFetchRevisionOptionsFromEnv()
	if err != nil || options.JobTimeout <= 0 || options.JobTimeout >= options.LeaseDuration || options.RetryMax < options.RetryMin ||
		options.Concurrency != 4 {
		t.Fatalf("default lyrics fetch options=%+v err=%v", options, err)
	}
	t.Setenv("LYRICS_FETCH_REVISION_LEASE_MS", "10000")
	t.Setenv("LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS", "10000")
	if _, err := lyricsFetchRevisionOptionsFromEnv(); err == nil {
		t.Fatal("lyrics fetch accepted a job timeout equal to its lease")
	}
	t.Setenv("LYRICS_FETCH_REVISION_LEASE_MS", "20000")
	t.Setenv("LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS", "1000")
	t.Setenv("LYRICS_FETCH_REVISION_RETRY_MIN_MS", "5000")
	t.Setenv("LYRICS_FETCH_REVISION_RETRY_MAX_MS", "1000")
	if _, err := lyricsFetchRevisionOptionsFromEnv(); err == nil {
		t.Fatal("lyrics fetch accepted retry maximum below minimum")
	}
	t.Setenv("LYRICS_FETCH_REVISION_RETRY_MAX_MS", "5000")
	for _, invalid := range []string{"0", "17", "+4", "04", "four"} {
		t.Setenv("LYRICS_FETCH_REVISION_CONCURRENCY", invalid)
		if _, err := lyricsFetchRevisionOptionsFromEnv(); err == nil {
			t.Fatalf("lyrics fetch accepted invalid concurrency %q", invalid)
		}
	}
}

func TestDrainingAdmitsOnlyExactHealthAndReadinessProbes(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "draining.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	projection := &operationalProjection{status: filesvc.ProjectionStatus{Generation: 1}}
	state := &lifecycle.State{}
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, operationalDeps{database: database, auth: authService, draining: state, projection: projection})
	entered := 0
	for _, path := range []string{"/read", "/api/work", "/api/lyrics/source/search", "/files/data/search-index.json"} {
		mux.HandleFunc(path, func(http.ResponseWriter, *http.Request) { entered++ })
	}
	handler := lifecycleMiddleware(state, mux)

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness before drain = %d", ready.Code)
	}
	state.Drain()
	ready = httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || ready.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("readiness while draining status=%d body=%q", ready.Code, ready.Body.String())
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("liveness while draining = %d", health.Code)
	}

	requests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/read"},
		{http.MethodGet, "/api/work"},
		{http.MethodGet, "/api/lyrics/source/search"},
		{http.MethodGet, "/files/data/search-index.json"},
		{http.MethodGet, "/healthz/details"},
		{http.MethodPost, "/healthz"},
		{http.MethodOptions, "/api/work"},
	}
	for _, request := range requests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"status":"draining"}` {
			t.Fatalf("draining %s %s status=%d body=%q", request.method, request.path, response.Code, response.Body.String())
		}
	}
	if entered != 0 {
		t.Fatalf("%d API/search/static handlers entered after drain", entered)
	}
}

func TestDrainingResponseRetainsCORSLoggingAndMetrics(t *testing.T) {
	httpRequestTotal.Store(0)
	httpClientErrors.Store(0)
	httpServerErrors.Store(0)
	state := &lifecycle.State{}
	entered := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/work", func(http.ResponseWriter, *http.Request) { entered = true })
	handler := loggingMiddleware(corsMiddleware(lifecycleMiddleware(state, preflightMiddleware(mux)), "https://console.example"))
	state.Drain()

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/work", nil)
		request.Header.Set("Origin", "https://console.example")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != `{"status":"draining"}` {
			t.Fatalf("draining %s status=%d body=%q", method, recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get("Access-Control-Allow-Origin") != "https://console.example" ||
			recorder.Header().Get("Vary") != "Origin" || recorder.Header().Get("X-Request-ID") == "" ||
			!strings.Contains(recorder.Header().Get("Access-Control-Allow-Headers"), "X-Moe-Loaded-Producer-State") ||
			!strings.Contains(recorder.Header().Get("Access-Control-Allow-Headers"), "X-SSE-Presence") {
			t.Fatalf("draining %s headers = %#v", method, recorder.Header())
		}
	}
	if entered {
		t.Fatal("draining request entered application handler")
	}
	if httpRequestTotal.Load() != 2 || httpServerErrors.Load() != 2 || httpClientErrors.Load() != 0 {
		t.Fatalf("drain counters total=%d client=%d server=%d", httpRequestTotal.Load(), httpClientErrors.Load(), httpServerErrors.Load())
	}
}

func TestDrainingPublicFileResponsesKeepPermissiveCORS(t *testing.T) {
	state := &lifecycle.State{}
	mux := http.NewServeMux()
	mux.HandleFunc("/files/", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/translation/", func(http.ResponseWriter, *http.Request) {})
	handler := corsMiddleware(lifecycleMiddleware(state, preflightMiddleware(mux)), "https://console.example")
	state.Drain()

	for _, path := range []string{"/files/translation/cards.json", "/translation/cards.json"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Origin", "https://pjsk.moe")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want %d", path, recorder.Code, http.StatusServiceUnavailable)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("%s draining CORS = %q, want %q", path, got, "*")
		}
	}
}

func TestTokenTTLRejectsMalformedAndExcessiveValues(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "1.5", "abc", "0168", "+168", "721"} {
		if _, err := parseTTL(value); err == nil {
			t.Fatalf("parseTTL(%q) unexpectedly succeeded", value)
		}
	}
	for _, test := range []struct {
		value string
		want  time.Duration
	}{{"1", time.Hour}, {"168", 7 * 24 * time.Hour}, {"720", 30 * 24 * time.Hour}} {
		got, err := parseTTL(test.value)
		if err != nil || got != test.want {
			t.Fatalf("parseTTL(%q)=%s err=%v want %s", test.value, got, err, test.want)
		}
	}
}

func TestSeedAdminUsesFirstSuccessfullyCreatedAccount(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "seed-admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	t.Setenv("TRANSLATOR_ACCOUNTS", ":bad,editor:strong-password-123")
	t.Setenv("ADMIN_PASSWORD", "")
	if err := seedAdminFromEnv(authService); err != nil {
		t.Fatal(err)
	}
	user, err := authService.GetUser("editor")
	if err != nil || user.Role != auth.RoleAdmin {
		t.Fatalf("first valid account = %+v err=%v", user, err)
	}
}

func TestPublishedAdminPasswordTemplateIsRejected(t *testing.T) {
	if err := validateBootstrapAdminPassword(publishedAdminPasswordTemplate); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("published administrator password template error=%v", err)
	}
	if err := validateBootstrapAdminPassword("unique-bootstrap-password"); err != nil {
		t.Fatalf("unique administrator password rejected: %v", err)
	}
}

func TestStandaloneProductionProfileCannotBeDisabledByEnvironment(t *testing.T) {
	previous := runtimeProfile
	t.Cleanup(func() { runtimeProfile = previous })
	runtimeProfile = runtimeProfileNextProduction
	for _, value := range []string{"", "false", "TRUE", " true", "true "} {
		if _, err := resolveProductionMode(value); err == nil || !strings.Contains(err.Error(), "remain exactly") {
			t.Fatalf("standalone production profile accepted MOESEKAI_PRODUCTION=%q: %v", value, err)
		}
	}
	if production, err := resolveProductionMode("true"); err != nil || !production {
		t.Fatalf("standalone production profile true=%v err=%v", production, err)
	}
	for _, test := range []struct {
		value      string
		configured bool
	}{
		{value: "", configured: false},
		{value: "", configured: true},
		{value: "utc", configured: true},
		{value: "UTC ", configured: true},
		{value: "Pacific/Honolulu", configured: true},
	} {
		if err := validateRuntimeTimezone(true, test.value, test.configured); err == nil || !strings.Contains(err.Error(), "remain exactly") {
			t.Fatalf("standalone production profile accepted TZ=%q configured=%v: %v", test.value, test.configured, err)
		}
	}
	if err := validateRuntimeTimezone(true, "UTC", true); err != nil {
		t.Fatalf("standalone production rejected TZ=UTC: %v", err)
	}
	if err := validateRuntimeTimezone(false, "Pacific/Honolulu", true); err != nil {
		t.Fatalf("development rejected arbitrary TZ: %v", err)
	}
	for _, test := range []struct {
		value      string
		configured bool
	}{
		{value: "", configured: false},
		{value: "", configured: true},
		{value: "/data", configured: true},
		{value: "/app/web/", configured: true},
		{value: " /app/web", configured: true},
	} {
		if _, err := resolveWebDir(true, test.value, test.configured); err == nil || !strings.Contains(err.Error(), "remain exactly") {
			t.Fatalf("standalone production profile accepted WEB_DIR=%q configured=%v: %v", test.value, test.configured, err)
		}
	}
	if webDir, err := resolveWebDir(true, "/app/web", true); err != nil || webDir != "/app/web" {
		t.Fatalf("standalone production WEB_DIR=%q err=%v", webDir, err)
	}
	if webDir, err := resolveWebDir(false, "", true); err != nil || webDir != "./web" {
		t.Fatalf("development WEB_DIR fallback=%q err=%v", webDir, err)
	}
	for _, test := range []struct {
		value      string
		configured bool
	}{
		{value: "", configured: false},
		{value: "", configured: true},
		{value: "/data/other.db", configured: true},
		{value: "/data/moesekai.db?mode=rwc", configured: true},
		{value: "/data/%6doesekai.db", configured: true},
	} {
		if _, err := resolveDBPath(true, test.value, test.configured); err == nil || !strings.Contains(err.Error(), "remain exactly") {
			t.Fatalf("standalone production profile accepted DB_PATH=%q configured=%v: %v", test.value, test.configured, err)
		}
	}
	if dbPath, err := resolveDBPath(true, "/data/moesekai.db", true); err != nil || dbPath != "/data/moesekai.db" {
		t.Fatalf("standalone production DB_PATH=%q err=%v", dbPath, err)
	}
	if dbPath, err := resolveDBPath(false, "", true); err != nil || dbPath != "./data/moesekai.db" {
		t.Fatalf("development DB_PATH fallback=%q err=%v", dbPath, err)
	}
	for _, test := range []struct {
		value      string
		configured bool
	}{
		{value: "", configured: false},
		{value: "", configured: true},
		{value: "/data/", configured: true},
		{value: " /data", configured: true},
		{value: "/tmp/data", configured: true},
	} {
		if _, err := resolveDataDir(true, test.value, test.configured); err == nil || !strings.Contains(err.Error(), "remain exactly") {
			t.Fatalf("standalone production profile accepted DATA_DIR=%q configured=%v: %v", test.value, test.configured, err)
		}
	}
	if dataDir, err := resolveDataDir(true, "/data", true); err != nil || dataDir != "/data" {
		t.Fatalf("standalone production DATA_DIR=%q err=%v", dataDir, err)
	}
	if dataDir, err := resolveDataDir(false, "", true); err != nil || dataDir != "./data" {
		t.Fatalf("development DATA_DIR fallback=%q err=%v", dataDir, err)
	}
	if dataDir, err := resolveDataDir(false, filepath.Join(".", "custom-data"), true); err != nil || dataDir != filepath.Join(".", "custom-data") {
		t.Fatalf("development DATA_DIR override=%q err=%v", dataDir, err)
	}
}

func TestProductionStartupRequiresMasterKeyAndInitializedAdmin(t *testing.T) {
	if production, err := parseProductionMode("true"); err != nil || !production {
		t.Fatalf("production parse=%v err=%v", production, err)
	}
	if _, err := parseProductionMode("enabled"); err == nil {
		t.Fatal("invalid production mode was accepted")
	}
	if err := validateProductionMasterKey(true, strings.Repeat("x", 31)); err == nil {
		t.Fatal("production accepted a 31-byte master key")
	}
	if err := validateProductionMasterKey(true, "  "+strings.Repeat("x", 31)+"  "); err == nil {
		t.Fatal("production counted surrounding whitespace as secret material")
	}
	if err := validateProductionMasterKey(true, strings.Repeat("x", 32)); err != nil {
		t.Fatalf("production rejected a 32-byte master key: %v", err)
	}
	if err := validateProductionMasterKey(true, "replace-with-at-least-32-random-bytes"); err == nil {
		t.Fatal("production accepted the published master-key template")
	}
	if err := validateProductionMasterKey(false, "short"); err != nil {
		t.Fatalf("development rejected a short master key: %v", err)
	}
	if err := validateConsoleOrigin(true, "*"); err != nil {
		t.Fatalf("production rejected wildcard console CORS: %v", err)
	}
	if err := validateConsoleOrigin(true, ""); err == nil {
		t.Fatal("production accepted an empty console origin")
	}
	if err := validateConsoleOrigin(true, "https://console.example"); err != nil {
		t.Fatalf("production rejected explicit console origin: %v", err)
	}
	for _, origin := range []string{"http://localhost:8080", "http://127.0.0.1", "http://[::1]:8080"} {
		if err := validateConsoleOrigin(true, origin); err != nil {
			t.Fatalf("production rejected loopback HTTP console origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{"http://console.example", "http://192.0.2.10:8080"} {
		if err := validateConsoleOrigin(true, origin); err == nil {
			t.Fatalf("production accepted non-loopback HTTP console origin %q", origin)
		}
	}
	for _, origin := range []string{"https://user@example.com", "https://console.example/", "https://console.example/path", "https://console.example?token=x", "file:///tmp/console"} {
		if err := validateConsoleOrigin(false, origin); err == nil {
			t.Fatalf("invalid console origin %q was accepted", origin)
		}
	}
	if err := validateConsoleOrigin(false, "*"); err != nil {
		t.Fatalf("development wildcard console origin rejected: %v", err)
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "production-validation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService := auth.New(database, "operations-secret-at-least-32-bytes", time.Hour)
	if err := validateProductionAdmin(true, authService); err == nil {
		t.Fatal("production accepted an uninitialized administrator")
	}
	if err := validateProductionAdmin(false, authService); err != nil {
		t.Fatalf("development required an administrator: %v", err)
	}
	if _, err := authService.CreateUser("admin", "strong-password-123", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := validateProductionAdmin(true, authService); err != nil {
		t.Fatalf("initialized production admin rejected: %v", err)
	}
}

func TestOperationalDurationEnvironmentValidation(t *testing.T) {
	for _, value := range []string{"abc", "0", "-1", "100000000000000000000"} {
		t.Setenv("TEST_DURATION_MS", value)
		if _, err := durationEnvMs("TEST_DURATION_MS", time.Second, time.Millisecond, time.Minute); err == nil {
			t.Fatalf("invalid duration %q was accepted", value)
		}
	}
	t.Setenv("TEST_DURATION_MS", "5000")
	if got, err := durationEnvMs("TEST_DURATION_MS", time.Second, time.Millisecond, time.Minute); err != nil || got != 5*time.Second {
		t.Fatalf("duration=%s err=%v", got, err)
	}
	if err := os.Unsetenv("TEST_DURATION_MS"); err != nil {
		t.Fatal(err)
	}
	if got, err := durationEnvMs("TEST_DURATION_MS", 7*time.Second, time.Millisecond, time.Minute); err != nil || got != 7*time.Second {
		t.Fatalf("default duration=%s err=%v", got, err)
	}
}

func TestSeedConfigFromEnvRejectsInvalidGroupWithoutPartialWrites(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "seed-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	configuration, err := config.New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRANSLATE_SCHEDULER_ENABLED", "true")
	t.Setenv("LYRICS_DISCOVERY_ENABLED", "ture")
	t.Setenv("UPSTREAM_REPO", "owner/repo")
	if err := seedConfigFromEnv(configuration); err == nil {
		t.Fatal("invalid environment seed unexpectedly succeeded")
	}
	if got := configuration.Get(config.KeyUpstreamRepo); got != "" {
		t.Fatalf("invalid environment seed partially changed config: %q", got)
	}
}

func TestSeedConfigFromEnvDefaultsSchedulerOffAndPreservesExplicitTrue(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		expected string
	}{
		{name: "unset", value: "", expected: "false"},
		{name: "explicit true", value: "true", expected: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, err := db.Open(filepath.Join(t.TempDir(), "scheduler-seed.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			configuration, err := config.New(database, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TRANSLATE_SCHEDULER_ENABLED", test.value)
			t.Setenv("LYRICS_DISCOVERY_ENABLED", "false")
			t.Setenv("LYRICS_FETCH_REVISION_ENABLED", "false")
			if err := seedConfigFromEnv(configuration); err != nil {
				t.Fatal(err)
			}
			if got := configuration.Get(config.KeySchedulerOn); got != test.expected {
				t.Fatalf("seeded scheduler = %q, want %q", got, test.expected)
			}
		})
	}
}

func TestVerifyWorkspaceCLIExitsBeforeDatabaseAndBackgroundStartup(t *testing.T) {
	if os.Getenv(verifyWorkspaceHelperEnv) == workspaceverify.ModeExternal {
		os.Args = []string{"moesekai-server", "--verify-workspace"}
		main()
		return
	}
	workspace, err := filepath.Abs(filepath.Join("internal", "workspaceverify", "testdata", "valid"))
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(workspace, "web-workspace-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifestBytes)
	command := exec.Command(os.Args[0], "-test.run=^TestVerifyWorkspaceCLIExitsBeforeDatabaseAndBackgroundStartup$")
	command.Env = []string{
		verifyWorkspaceHelperEnv + "=" + workspaceverify.ModeExternal,
		"MOESEKAI_PRODUCTION=false",
		"WORKSPACE_MODE=" + workspaceverify.ModeExternal,
		"WORKSPACE_WEB_DIR=" + workspace,
		fmt.Sprintf("WORKSPACE_MANIFEST_SHA256=%x", digest),
		"DB_PATH=" + filepath.Join(t.TempDir(), "must-not-exist", "database.db"),
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("verify-only process failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "workspace verified") {
		t.Fatalf("verify-only output = %q", output)
	}
}

func TestVerifyWorkspaceCLIDisabledExitsBeforeDatabaseAndBackgroundStartup(t *testing.T) {
	if os.Getenv(verifyWorkspaceHelperEnv) == workspaceverify.ModeDisabled {
		os.Args = []string{"moesekai-server", "--verify-workspace"}
		main()
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestVerifyWorkspaceCLIDisabledExitsBeforeDatabaseAndBackgroundStartup$")
	command.Env = []string{
		verifyWorkspaceHelperEnv + "=" + workspaceverify.ModeDisabled,
		"MOESEKAI_PRODUCTION=true",
		"TZ=UTC",
		"WORKSPACE_MODE=" + workspaceverify.ModeDisabled,
		"WEB_DIR=/app/web",
		"DB_PATH=/data/moesekai.db",
		"DATA_DIR=/data",
		"SHUTDOWN_BUDGET_MS=not-a-number",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("disabled verify-only process failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "workspace verified: disabled") {
		t.Fatalf("disabled verify-only output = %q", output)
	}
}

func TestRuntimeRejectsExternalBeforeDatabaseAndBackgroundStartup(t *testing.T) {
	if helper := os.Getenv(verifyRuntimeFailureHelperEnv); helper != "" {
		os.Args = []string{"moesekai-server"}
		if helper == "cli" {
			os.Args = append(os.Args, "--verify-runtime")
		}
		main()
		return
	}
	for _, helper := range []string{"cli", "server"} {
		t.Run(helper, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "must-not-exist", "database.db")
			command := exec.Command(os.Args[0], "-test.run=^TestRuntimeRejectsExternalBeforeDatabaseAndBackgroundStartup$")
			command.Env = []string{
				verifyRuntimeFailureHelperEnv + "=" + helper,
				"MOESEKAI_PRODUCTION=false",
				"WORKSPACE_MODE=external",
				"DB_PATH=" + databasePath,
			}
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "external workspace is available only to verifier tooling") {
				t.Fatalf("runtime external verification err=%v output=%q", err, output)
			}
			if _, err := os.Stat(filepath.Dir(databasePath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime external verification touched database parent: %v", err)
			}
		})
	}
}

func TestProductionWebDirOverrideFailsBeforeDatabaseAndBackgroundStartup(t *testing.T) {
	if os.Getenv(verifyWebDirFailureHelperEnv) == "1" {
		os.Args = []string{"moesekai-server", "--verify-runtime"}
		main()
		return
	}
	databasePath := filepath.Join(t.TempDir(), "must-not-exist", "database.db")
	command := exec.Command(os.Args[0], "-test.run=^TestProductionWebDirOverrideFailsBeforeDatabaseAndBackgroundStartup$")
	command.Env = []string{
		verifyWebDirFailureHelperEnv + "=1",
		"MOESEKAI_PRODUCTION=true",
		"TZ=UTC",
		"WORKSPACE_MODE=disabled",
		"WEB_DIR=/data",
		"DB_PATH=" + databasePath,
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `standalone production binary requires WEB_DIR to remain exactly "/app/web"`) {
		t.Fatalf("production WEB_DIR override err=%v output=%q", err, output)
	}
	if _, err := os.Stat(filepath.Dir(databasePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production WEB_DIR override touched database parent: %v", err)
	}
}

func TestProductionDBPathOverrideFailsBeforePersistentDataAndBackgroundStartup(t *testing.T) {
	if os.Getenv(verifyDBPathFailureHelperEnv) == "1" {
		os.Args = []string{"moesekai-server", "--verify-runtime"}
		main()
		return
	}
	dataDir := filepath.Join(t.TempDir(), "must-not-exist")
	command := exec.Command(os.Args[0], "-test.run=^TestProductionDBPathOverrideFailsBeforePersistentDataAndBackgroundStartup$")
	command.Env = []string{
		verifyDBPathFailureHelperEnv + "=1",
		"MOESEKAI_PRODUCTION=true",
		"TZ=UTC",
		"WORKSPACE_MODE=disabled",
		"WEB_DIR=/app/web",
		"DB_PATH=" + filepath.Join(dataDir, "moesekai.db"),
		"DATA_DIR=/data",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `standalone production binary requires DB_PATH to remain exactly "/data/moesekai.db"`) {
		t.Fatalf("production DB_PATH override err=%v output=%q", err, output)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production DB_PATH override touched data directory: %v", err)
	}
}

func TestProductionDataDirOverrideFailsBeforePersistentDataAndBackgroundStartup(t *testing.T) {
	if os.Getenv(verifyDataDirFailureHelperEnv) == "1" {
		os.Args = []string{"moesekai-server", "--verify-runtime"}
		main()
		return
	}
	dataDir := filepath.Join(t.TempDir(), "must-not-exist")
	command := exec.Command(os.Args[0], "-test.run=^TestProductionDataDirOverrideFailsBeforePersistentDataAndBackgroundStartup$")
	command.Env = []string{
		verifyDataDirFailureHelperEnv + "=1",
		"MOESEKAI_PRODUCTION=true",
		"TZ=UTC",
		"WORKSPACE_MODE=disabled",
		"WEB_DIR=/app/web",
		"DB_PATH=/data/moesekai.db",
		"DATA_DIR=" + dataDir,
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `standalone production binary requires DATA_DIR to remain exactly "/data"`) {
		t.Fatalf("production DATA_DIR override err=%v output=%q", err, output)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production DATA_DIR override touched data directory: %v", err)
	}
}

func TestProductionTimezoneOverrideFailsBeforePersistentDataAndBackgroundStartup(t *testing.T) {
	if os.Getenv(verifyTimezoneFailureHelperEnv) == "1" {
		os.Args = []string{"moesekai-server", "--verify-runtime"}
		main()
		return
	}
	databasePath := filepath.Join(t.TempDir(), "must-not-exist", "moesekai.db")
	command := exec.Command(os.Args[0], "-test.run=^TestProductionTimezoneOverrideFailsBeforePersistentDataAndBackgroundStartup$")
	command.Env = []string{
		verifyTimezoneFailureHelperEnv + "=1",
		"MOESEKAI_PRODUCTION=true",
		"TZ=Pacific/Honolulu",
		"WORKSPACE_MODE=disabled",
		"WEB_DIR=/app/web",
		"DB_PATH=" + databasePath,
		"DATA_DIR=/data",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `standalone production binary requires TZ to remain exactly "UTC"`) {
		t.Fatalf("production TZ override err=%v output=%q", err, output)
	}
	if _, err := os.Stat(filepath.Dir(databasePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production TZ override touched database parent: %v", err)
	}
}

func TestVerifyWorkspaceCLIProductionModeFailures(t *testing.T) {
	if os.Getenv(verifyWorkspaceFailureHelperEnv) == "1" {
		os.Args = []string{"moesekai-server", "--verify-workspace"}
		main()
		return
	}
	for _, test := range []struct {
		name    string
		env     []string
		message string
	}{
		{name: "missing", message: "WORKSPACE_MODE is required"},
		{name: "disabled residue", env: []string{"WORKSPACE_MODE=disabled", "WORKSPACE_WEB_DIR="}, message: "must both be unset"},
		{name: "external production", env: []string{"WORKSPACE_MODE=external"}, message: `must be exactly "disabled" in production`},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestVerifyWorkspaceCLIProductionModeFailures$")
			command.Env = append([]string{
				verifyWorkspaceFailureHelperEnv + "=1",
				"MOESEKAI_PRODUCTION=true",
				"TZ=UTC",
			}, test.env...)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.message) {
				t.Fatalf("production %s err=%v output=%q", test.name, err, output)
			}
		})
	}
}

func TestHTTPServerShutdownDrainsInflightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	state := &lifecycle.State{}
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	workEntered := false
	mux.HandleFunc("/api/work", func(w http.ResponseWriter, _ *http.Request) {
		workEntered = true
		w.WriteHeader(http.StatusNoContent)
	})
	server := newHTTPServer("", lifecycleMiddleware(state, mux))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	requestResult := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String() + "/slow")
		if err == nil {
			response.Body.Close()
		}
		requestResult <- err
	}()
	<-started
	state.Drain()
	rejected, err := http.Get("http://" + listener.Addr().String() + "/api/work")
	if err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusServiceUnavailable || workEntered {
		t.Fatalf("post-drain request status=%d entered=%v", rejected.StatusCode, workEntered)
	}
	shutdownResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownResult <- server.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownResult:
		t.Fatalf("shutdown returned before request drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-requestResult; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownResult; err != nil {
		t.Fatal(err)
	}
	if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve result = %v", err)
	}
	state.Wait()
}

func TestBlockedReadinessProbeIsJoinedBeforeShutdownReturns(t *testing.T) {
	if os.Getenv(blockedProbeHelperEnv) == "1" {
		runBlockedProbeShutdownHelper(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestBlockedReadinessProbeIsJoinedBeforeShutdownReturns$")
	command.Env = append(os.Environ(), blockedProbeHelperEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("blocked readiness subprocess failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "probe-joined-before-close") {
		t.Fatalf("blocked readiness subprocess output = %q", output)
	}
}

func runBlockedProbeShutdownHelper(t *testing.T) {
	t.Helper()
	state := &lifecycle.State{}
	entered := make(chan struct{})
	finished := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		time.Sleep(20 * time.Millisecond)
		close(finished)
	})
	server := newHTTPServer("", lifecycleMiddleware(state, mux))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String() + "/readyz")
		if requestErr == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("readiness probe did not enter")
	}
	shutdownErr := lifecycle.RunShutdown(lifecycle.ShutdownConfig{Budget: 500 * time.Millisecond, Drain: 10 * time.Millisecond},
		func(string, ...any) {}, func(int) { t.Fatal("shutdown watchdog fired") },
		func(ctx context.Context) error {
			state.Drain()
			return server.Shutdown(ctx)
		}, func() {
			state.StopProbes()
			_ = server.Close()
		}, func() error {
			if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			state.Wait()
			select {
			case <-finished:
				fmt.Fprintln(os.Stdout, "probe-joined-before-close")
				return nil
			default:
				return errors.New("shutdown returned before readiness probe")
			}
		})
	if shutdownErr == nil || !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v", shutdownErr)
	}
}
