package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/httpx"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("MOESEKAI_PRODUCTION", "false")
	_ = os.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "true")
	os.Exit(m.Run())
}

func openWatcherConfig(t *testing.T) *config.Config {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/watcher.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	cfg, err := config.New(database, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter("120", now)
	if got != 2*time.Minute {
		t.Fatalf("parseRetryAfter seconds = %s, want 2m", got)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter("Thu, 09 Jul 2026 12:30:00 GMT", now)
	if got != 30*time.Minute {
		t.Fatalf("parseRetryAfter date = %s, want 30m", got)
	}
}

func TestRateLimitCooldownFallsBackToAtLeastOneHour(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := rateLimitCooldown("", now, 5*time.Minute)
	if got != time.Hour {
		t.Fatalf("rateLimitCooldown fallback = %s, want 1h", got)
	}
}

func TestRateLimitCooldownCapsFallback(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := rateLimitCooldown("", now, 12*time.Hour)
	if got != maxFallbackCooldown {
		t.Fatalf("rateLimitCooldown capped fallback = %s, want %s", got, maxFallbackCooldown)
	}
}

func TestExpandVersionURLDefaultsToMirror(t *testing.T) {
	got := expandVersionURL("", "owner/repo", "main")
	want := "https://metadata.pjsk.moe/jp/versions/current_version.json"
	if got != want {
		t.Fatalf("expandVersionURL default = %q, want %q", got, want)
	}
}

func TestExpandVersionURLTemplate(t *testing.T) {
	got := expandVersionURL("https://cdn.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json", "owner/repo", "dev")
	want := "https://cdn.jsdelivr.net/gh/owner/repo@dev/versions/current_version.json"
	if got != want {
		t.Fatalf("expandVersionURL template = %q, want %q", got, want)
	}
}

func TestUnsetSchedulerPerformsNoBaselinePendingOrContentWrites(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })
	var requests, syncCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"dataVersion":"100"}`)
	}))
	defer server.Close()
	cfg := openWatcherConfig(t)
	if err := cfg.Set(config.KeyUpstreamVersionURL, server.URL); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyUpstreamVersionFallbackURL, server.URL); err != nil {
		t.Fatal(err)
	}
	watcher := New(cfg, func() error {
		syncCalls.Add(1)
		return nil
	}, Options{Interval: 5 * time.Millisecond})
	watcher.Start()
	t.Cleanup(func() { watcher.Stop(); watcher.Wait() })
	time.Sleep(30 * time.Millisecond)
	if requests.Load() != 0 || syncCalls.Load() != 0 {
		t.Fatalf("unset scheduler performed network/content work: requests=%d syncs=%d", requests.Load(), syncCalls.Load())
	}
	if cfg.Get(config.KeyUpstreamLastDataVersion) != "" || cfg.Get(config.KeyUpstreamPendingDataVersion) != "" {
		t.Fatalf("unset scheduler persisted version state: baseline=%q pending=%q", cfg.Get(config.KeyUpstreamLastDataVersion), cfg.Get(config.KeyUpstreamPendingDataVersion))
	}
	if watcher.Status().Enabled {
		t.Fatal("unset scheduler reported enabled")
	}
}

func TestExplicitlyEnabledSchedulerBaselinesAndSyncsChanges(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })
	var version atomic.Value
	version.Store("100")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"dataVersion":%q}`, version.Load().(string))
	}))
	defer server.Close()
	cfg := openWatcherConfig(t)
	for key, value := range map[string]string{
		config.KeySchedulerOn:                "true",
		config.KeyUpstreamVersionURL:         server.URL,
		config.KeyUpstreamVersionFallbackURL: server.URL,
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var syncCalls atomic.Int32
	watcher := New(cfg, func() error {
		syncCalls.Add(1)
		return nil
	}, Options{Interval: 5 * time.Millisecond})
	watcher.Start()
	t.Cleanup(func() { watcher.Stop(); watcher.Wait() })
	waitForWatcher(t, func() bool { return cfg.Get(config.KeyUpstreamLastDataVersion) == "100" })
	if syncCalls.Load() != 0 {
		t.Fatalf("initial baseline triggered %d content syncs", syncCalls.Load())
	}
	version.Store("101")
	waitForWatcher(t, func() bool {
		return syncCalls.Load() == 1 && cfg.Get(config.KeyUpstreamLastDataVersion) == "101" && cfg.Get(config.KeyUpstreamPendingDataVersion) == ""
	})
	if !watcher.Status().Enabled {
		t.Fatal("explicit true scheduler did not report enabled")
	}
}

func waitForWatcher(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("timed out waiting for watcher state")
	}
}

func TestFetchVersionFallsBackToSecondarySource(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })

	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"appVersion":"1","dataVersion":"2","assetVersion":"3"}`)
	}))
	defer fallback.Close()

	cfg := openWatcherConfig(t)
	if err := cfg.Set(config.KeyUpstreamVersionURL, primary.URL); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyUpstreamVersionFallbackURL, fallback.URL); err != nil {
		t.Fatal(err)
	}
	w := New(cfg, nil, Options{})

	info, source, err := w.fetchVersion()
	if err != nil {
		t.Fatal(err)
	}
	if info.DataVersion != "2" || source != fallback.URL {
		t.Fatalf("unexpected fallback result: info=%+v source=%q", info, source)
	}
	if primaryCalls.Load() > 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("unexpected calls: primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestSplitVersionTemplates(t *testing.T) {
	got := splitVersionTemplates("https://one.example/a, https://two.example/b\nhttps://three.example/c")
	if len(got) != 3 {
		t.Fatalf("unexpected templates: %v", got)
	}
}

func TestFetchVersionRacesSlowPrimary(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })

	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"dataVersion":"fast"}`)
	}))
	defer fallback.Close()

	cfg := openWatcherConfig(t)
	cfg.Set(config.KeyUpstreamVersionURL, primary.URL)
	cfg.Set(config.KeyUpstreamVersionFallbackURL, fallback.URL)
	w := New(cfg, nil, Options{})

	started := time.Now()
	info, source, err := w.fetchVersion()
	if err != nil {
		t.Fatal(err)
	}
	if info.DataVersion != "fast" || source != fallback.URL || time.Since(started) > time.Second {
		t.Fatalf("slow primary was not raced: info=%+v source=%q elapsed=%s", info, source, time.Since(started))
	}
}

func TestRecordSyncSuccessClearsStaleError(t *testing.T) {
	cfg := openWatcherConfig(t)
	w := New(cfg, nil, Options{})
	w.setStatus(func(s *Status) {
		s.LastError = "old timeout"
		s.LastErrorAt = "old"
		s.ConsecutiveFailures = 2
	})

	w.RecordSyncResult("", nil)
	status := w.Status()
	if status.LastError != "" || status.LastErrorAt != "" || status.ConsecutiveFailures != 0 {
		t.Fatalf("stale error was not cleared: %+v", status)
	}
	if status.LastSync == "" || status.LastSuccess == "" {
		t.Fatalf("sync timestamps not recorded: %+v", status)
	}
}

func TestCheckNowLegacyTriggerSemantics(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })

	var version atomic.Value
	version.Store("100")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"dataVersion":%q}`, version.Load().(string))
	}))
	defer server.Close()

	cfg := openWatcherConfig(t)
	if err := cfg.Set(config.KeyUpstreamVersionURL, server.URL); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyUpstreamVersionFallbackURL, server.URL); err != nil {
		t.Fatal(err)
	}
	var syncCalls atomic.Int32
	w := New(cfg, func() error {
		syncCalls.Add(1)
		return nil
	}, Options{})

	if _, err := w.CheckNow(false); err != nil {
		t.Fatal(err)
	}
	if syncCalls.Load() != 0 {
		t.Fatalf("first observed version triggered %d syncs", syncCalls.Load())
	}
	if _, err := w.CheckNow(false); err != nil {
		t.Fatal(err)
	}
	if syncCalls.Load() != 0 {
		t.Fatalf("unchanged version triggered %d syncs", syncCalls.Load())
	}
	if _, err := w.CheckNow(true); err != nil {
		t.Fatal(err)
	}
	if syncCalls.Load() != 1 {
		t.Fatalf("forced check syncs = %d, want 1", syncCalls.Load())
	}
	version.Store("101")
	status, err := w.CheckNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if syncCalls.Load() != 2 || status.LastDataVersion != "101" || status.ChangeDetectedAt == "" {
		t.Fatalf("changed check status=%+v syncs=%d", status, syncCalls.Load())
	}
}

func TestFailedSyncKeepsChangedVersionPendingForRetry(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })
	var version atomic.Value
	version.Store("100")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"dataVersion":%q}`, version.Load().(string))
	}))
	defer server.Close()
	cfg := openWatcherConfig(t)
	_ = cfg.Set(config.KeyUpstreamVersionURL, server.URL)
	_ = cfg.Set(config.KeyUpstreamVersionFallbackURL, server.URL)
	var calls atomic.Int32
	watcher := New(cfg, func() error {
		if calls.Add(1) == 1 {
			return fmt.Errorf("transient sync failure")
		}
		return nil
	}, Options{})
	if _, err := watcher.CheckNow(false); err != nil {
		t.Fatal(err)
	}
	version.Store("101")
	if status, err := watcher.CheckNow(false); err == nil || status.LastDataVersion != "100" || status.PendingDataVersion != "101" {
		t.Fatalf("failed sync consumed version: status=%+v err=%v", status, err)
	}
	status, err := watcher.CheckNow(false)
	if err != nil || calls.Load() != 2 || status.LastDataVersion != "101" || status.PendingDataVersion != "" {
		t.Fatalf("pending version was not retried: status=%+v calls=%d err=%v", status, calls.Load(), err)
	}
}

func TestVersionPublishedDuringSyncStaysPending(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })
	var version atomic.Value
	version.Store("100")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"dataVersion":%q}`, version.Load().(string))
	}))
	defer server.Close()
	cfg := openWatcherConfig(t)
	_ = cfg.Set(config.KeyUpstreamVersionURL, server.URL)
	_ = cfg.Set(config.KeyUpstreamVersionFallbackURL, server.URL)
	var watcher *Watcher
	var calls atomic.Int32
	watcher = New(cfg, func() error {
		if calls.Add(1) == 1 {
			// upstream publishes 102 while the sync of 101 is still running
			version.Store("102")
			if _, err := watcher.fetchAndCompare(); err != nil {
				t.Errorf("mid-sync check failed: %v", err)
			}
		}
		return nil
	}, Options{})
	if _, err := watcher.CheckNow(false); err != nil {
		t.Fatal(err)
	}
	version.Store("101")
	status, err := watcher.CheckNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if status.LastDataVersion != "101" || status.PendingDataVersion != "102" {
		t.Fatalf("sync recorded a version it never processed: %+v", status)
	}
	if cfg.Get(config.KeyUpstreamLastDataVersion) != "101" || cfg.Get(config.KeyUpstreamPendingDataVersion) != "102" {
		t.Fatalf("persisted state = last %q pending %q, want 101/102",
			cfg.Get(config.KeyUpstreamLastDataVersion), cfg.Get(config.KeyUpstreamPendingDataVersion))
	}
	status, err = watcher.CheckNow(false)
	if err != nil || calls.Load() != 2 || status.LastDataVersion != "102" || status.PendingDataVersion != "" {
		t.Fatalf("mid-sync version was not fetched next tick: status=%+v calls=%d err=%v", status, calls.Load(), err)
	}
}

func TestFailedVersionPersistsAcrossWatcherRestart(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })
	var version atomic.Value
	version.Store("100")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"dataVersion":%q}`, version.Load().(string))
	}))
	defer server.Close()
	databasePath := t.TempDir() + "/persistent-watcher.db"
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.New(database, "test-key")
	_ = cfg.Set(config.KeyUpstreamVersionURL, server.URL)
	_ = cfg.Set(config.KeyUpstreamVersionFallbackURL, server.URL)
	first := New(cfg, func() error { return fmt.Errorf("transient") }, Options{})
	if _, err := first.CheckNow(false); err != nil {
		t.Fatal(err)
	}
	version.Store("101")
	if _, err := first.CheckNow(false); err == nil {
		t.Fatal("changed version unexpectedly synced")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloadedConfig, err := config.New(reopened, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(reloadedConfig, func() error { return nil }, Options{})
	if status := restarted.Status(); status.LastDataVersion != "100" || status.PendingDataVersion != "101" {
		t.Fatalf("persisted state not restored: %+v", status)
	}
	status, err := restarted.CheckNow(false)
	if err != nil || status.LastDataVersion != "101" || status.PendingDataVersion != "" {
		t.Fatalf("restart did not retry pending version: status=%+v err=%v", status, err)
	}
}

func TestVersionResponseSizeIsBounded(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(maxVersionResponseBytes+1))
		_, _ = w.Write(make([]byte, maxVersionResponseBytes+1))
	}))
	defer server.Close()
	cfg := openWatcherConfig(t)
	_ = cfg.Set(config.KeyUpstreamVersionURL, server.URL)
	_ = cfg.Set(config.KeyUpstreamVersionFallbackURL, server.URL)
	if _, _, err := New(cfg, nil, Options{}).fetchVersion(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized version response error = %v", err)
	}
}
