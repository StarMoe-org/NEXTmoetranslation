package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/httpx"
)

func TestSeedConfigFromEnvSeedsTheSideStorySourceBases(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "side-story-seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	configuration, err := config.New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LYRICS_DISCOVERY_ENABLED", "false")
	t.Setenv("LYRICS_FETCH_REVISION_ENABLED", "false")
	seeds := map[string]string{
		"UPSTREAM_EN_MASTERDATA_URL":          config.KeyUpstreamENMasterdataURL,
		"UPSTREAM_EN_MASTERDATA_FALLBACK_URL": config.KeyUpstreamENMasterdataFallbackURL,
		"UPSTREAM_JP_SCRIPTS_URL":             config.KeyUpstreamJPScriptsURL,
		"UPSTREAM_JP_SCRIPTS_FALLBACK_URL":    config.KeyUpstreamJPScriptsFallbackURL,
		"UPSTREAM_CN_SCRIPTS_URL":             config.KeyUpstreamCNScriptsURL,
		"UPSTREAM_EN_SCRIPTS_URL":             config.KeyUpstreamENScriptsURL,
	}
	for env := range seeds {
		t.Setenv(env, "https://test-"+strings.ToLower(strings.ReplaceAll(env, "_", "-"))+".example.test/base")
	}
	if err := seedConfigFromEnv(configuration); err != nil {
		t.Fatal(err)
	}
	for env, key := range seeds {
		want := "https://test-" + strings.ToLower(strings.ReplaceAll(env, "_", "-")) + ".example.test/base"
		if got := configuration.Get(key); got != want {
			t.Fatalf("%s seeded %s=%q, want %q", env, key, got, want)
		}
	}
}

func clearSideStoryBackfillEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"SIDE_STORY_BACKFILL_ENABLED", "SIDE_STORY_BACKFILL_INTERVAL_MS",
		"SIDE_STORY_BACKFILL_BATCH", "SIDE_STORY_BACKFILL_REQUEST_DELAY_MS"} {
		t.Setenv(key, "")
	}
}

func TestSideStoryBackfillConfigurationDefaultsAndIsStrict(t *testing.T) {
	clearSideStoryBackfillEnv(t)
	options, err := sideStoryBackfillOptionsFromEnv()
	if err != nil || !options.Enabled || options.Interval != time.Minute || options.Batch != 30 || options.RequestDelay != time.Second {
		t.Fatalf("default side story options=%+v err=%v", options, err)
	}
	t.Setenv("SIDE_STORY_BACKFILL_ENABLED", "false")
	t.Setenv("SIDE_STORY_BACKFILL_INTERVAL_MS", "120000")
	t.Setenv("SIDE_STORY_BACKFILL_BATCH", "5")
	t.Setenv("SIDE_STORY_BACKFILL_REQUEST_DELAY_MS", "250")
	options, err = sideStoryBackfillOptionsFromEnv()
	if err != nil || options.Enabled || options.Interval != 2*time.Minute || options.Batch != 5 || options.RequestDelay != 250*time.Millisecond {
		t.Fatalf("overridden side story options=%+v err=%v", options, err)
	}
	for key, invalid := range map[string][]string{
		"SIDE_STORY_BACKFILL_ENABLED":          {"flase", "yes"},
		"SIDE_STORY_BACKFILL_INTERVAL_MS":      {"0", "999", "abc"},
		"SIDE_STORY_BACKFILL_BATCH":            {"0", "501", "05", "many"},
		"SIDE_STORY_BACKFILL_REQUEST_DELAY_MS": {"0", "99", "60001"},
	} {
		for _, value := range invalid {
			clearSideStoryBackfillEnv(t)
			t.Setenv(key, value)
			if _, err := sideStoryBackfillOptionsFromEnv(); err == nil {
				t.Fatalf("side story backfill accepted %s=%q", key, value)
			}
		}
	}
}

func TestServicesConnectTheSideStoryRunnerAndStopItOnShutdown(t *testing.T) {
	clearSideStoryBackfillEnv(t)
	t.Setenv("TRANSLATE_SCHEDULER_ENABLED", "false")
	t.Setenv("LYRICS_DISCOVERY_ENABLED", "false")
	t.Setenv("LYRICS_FETCH_REVISION_ENABLED", "false")
	t.Setenv("TRANSLATOR_ACCOUNTS", "")
	database, err := db.Open(filepath.Join(t.TempDir(), "side-story-wiring.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	svc := buildServices(startupEnv{dataDir: t.TempDir()},
		runtimeSettings{jwtSecret: "side-story-wiring-secret-at-least-32-bytes"}, database)
	if svc.sideStory == nil || svc.sideStory.Translator != svc.translator {
		t.Fatalf("side story backfill is not built on the process translator: %+v", svc.sideStory)
	}

	user, err := svc.auth.CreateUser("test-side-story-editor", "test-password-0001", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.auth.IssueToken(user)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	svc.api.RegisterRoutes(mux)
	request := httptest.NewRequest(http.MethodGet, "/api/editor/v1/stories/sync", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state"`) {
		t.Fatalf("side story sync status=%d body=%s", response.Code, response.Body.String())
	}

	svc.sideStory.Start()
	svc.cancel(newHTTPServer("127.0.0.1:0", mux))
	done := make(chan struct{})
	go func() {
		svc.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not join the side story backfill")
	}
}

func TestAfterContentRestoreRefreshesTheSideStoryCatalog(t *testing.T) {
	clearSideStoryBackfillEnv(t)
	t.Setenv("SIDE_STORY_BACKFILL_INTERVAL_MS", "3600000")
	t.Setenv("SIDE_STORY_BACKFILL_REQUEST_DELAY_MS", "100")
	t.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "true")
	t.Setenv("LYRICS_DISCOVERY_ENABLED", "false")
	t.Setenv("LYRICS_FETCH_REVISION_ENABLED", "false")
	t.Setenv("TRANSLATOR_ACCOUNTS", "")
	var mu sync.Mutex
	masterdata := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "-master/") {
			mu.Lock()
			masterdata++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()
	requests := func() int {
		mu.Lock()
		defer mu.Unlock()
		return masterdata
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "side-story-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	svc := buildServices(startupEnv{dataDir: t.TempDir()},
		runtimeSettings{jwtSecret: "side-story-wiring-secret-at-least-32-bytes"}, database)
	sources := map[string]string{}
	for _, key := range []string{config.KeyUpstreamJPMasterdataURL, config.KeyUpstreamJPMasterdataFallbackURL} {
		sources[key] = upstream.URL + "/jp-master"
	}
	for _, key := range []string{config.KeyUpstreamCNMasterdataURL, config.KeyUpstreamCNMasterdataFallbackURL} {
		sources[key] = upstream.URL + "/cn-master"
	}
	for _, key := range []string{config.KeyUpstreamENMasterdataURL, config.KeyUpstreamENMasterdataFallbackURL} {
		sources[key] = upstream.URL + "/en-master"
	}
	for _, key := range []string{config.KeyUpstreamJPScriptsURL, config.KeyUpstreamJPScriptsFallbackURL,
		config.KeyUpstreamCNScriptsURL, config.KeyUpstreamENScriptsURL} {
		sources[key] = upstream.URL + "/scripts"
	}
	if _, err := svc.cfg.SetMany(sources); err != nil {
		t.Fatal(err)
	}
	svc.sideStory.Start()
	defer func() {
		svc.sideStory.Stop()
		svc.sideStory.Wait()
	}()
	waitUntil := func(what string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitUntil("the first catalog refresh", func() bool {
		state := svc.sideStory.SideStoryBackfillState()
		return state.CatalogRefreshedAt != "" && !state.Running
	})
	first := requests()
	if first == 0 {
		t.Fatal("the first round fetched no masterdata")
	}

	afterContentRestore(svc.collab, svc.sideStory)()
	waitUntil("a catalog refresh after the restore", func() bool { return requests() >= 2*first })
}
