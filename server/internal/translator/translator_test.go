package translator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// externalDNSQueries counts DNS queries. Every test source is a loopback IP
// literal, so any query means a source fell through to a public default.
var externalDNSQueries atomic.Int64

func TestMain(m *testing.M) {
	_ = os.Setenv("MOESEKAI_PRODUCTION", "false")
	_ = os.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "true")
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(context.Context, string, string) (net.Conn, error) {
		externalDNSQueries.Add(1)
		return nil, errors.New("translator tests must not resolve external hosts")
	}
	code := m.Run()
	if queries := externalDNSQueries.Load(); queries > 0 {
		fmt.Fprintf(os.Stderr, "translator tests attempted %d external DNS queries; configure every upstream source locally\n", queries)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func openTranslatorConfig(t *testing.T) *config.Config {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/translator.db")
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

func TestOrderedEpisodesKeepDuplicatePositionalLines(t *testing.T) {
	episodes := toOrderedEpisodes(map[string]builtEpisode{"1": {
		episodeNo: "1", scenarioID: "scenario", title: "title",
		talkKeys: []string{"same"}, talkData: map[string]string{"same": "legacy"},
		lines: []store.OrderedLine{
			{JPKey: "same", Text: "first", Source: "cn"},
			{JPKey: "same", Text: "second", Source: "cn"},
		},
	}}, "cn")
	if len(episodes) != 1 || len(episodes[0].Lines) != 2 || episodes[0].Lines[0].Text != "first" || episodes[0].Lines[1].Text != "second" {
		t.Fatalf("ordered duplicate lines = %+v", episodes)
	}
}

func openTestTranslator(t *testing.T) (*Translator, *store.EventStore, *config.Config) {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/translator.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	cfg, err := config.New(database, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(database)
	es := store.NewEventStore(database)
	return New(s, es, cfg), es, cfg
}

func openCatalogTestTranslator(t *testing.T) (*Translator, *db.DB, *config.Config) {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/catalog-extraction.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	cfg, err := config.New(database, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	return New(store.New(database), nil, cfg), database, cfg
}

var fallbackSourceKeys = map[string]string{
	config.KeyUpstreamJPMasterdataURL: config.KeyUpstreamJPMasterdataFallbackURL,
	config.KeyUpstreamCNMasterdataURL: config.KeyUpstreamCNMasterdataFallbackURL,
	config.KeyUpstreamJPAssetsURL:     config.KeyUpstreamJPAssetsFallbackURL,
	config.KeyUpstreamCNAssetsURL:     config.KeyUpstreamCNAssetsFallbackURL,
}

// configureSourceURLs sets each given primary source and repeats it as that
// source's fallback, which collapses the chain to the one local source. An
// empty fallback setting would re-enable the public default mirror.
func configureSourceURLs(t *testing.T, cfg *config.Config, primaries map[string]string) {
	t.Helper()
	settings := make(map[string]string, 2*len(primaries))
	for key, url := range primaries {
		fallback, ok := fallbackSourceKeys[key]
		if !ok {
			t.Fatalf("%s is not a primary source key", key)
		}
		settings[key], settings[fallback] = url, url
	}
	if _, err := cfg.SetMany(settings); err != nil {
		t.Fatal(err)
	}
}

// configureLocalSources points all four sources at paths of one test server.
func configureLocalSources(t *testing.T, cfg *config.Config, baseURL string) {
	t.Helper()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: baseURL + "/jp-master",
		config.KeyUpstreamCNMasterdataURL: baseURL + "/cn-master",
		config.KeyUpstreamJPAssetsURL:     baseURL + "/jp-assets",
		config.KeyUpstreamCNAssetsURL:     baseURL + "/cn-assets",
	})
}

func TestBuildAndParseXMLRoundTrip(t *testing.T) {
	texts := []string{"こんにちは", "A & B", "<tag>", "三", ""}
	xml := buildXMLInput(texts)
	// Simulate an LLM echoing translations back in the expected format.
	resp := "<translations>"
	for i := range texts {
		resp += "<t id=\"" + strconv.Itoa(i+1) + "\">译" + strconv.Itoa(i+1) + "</t>"
	}
	resp += "</translations>"
	got := parseXMLTranslations(resp, len(texts))
	want := []string{"译1", "译2", "译3", "译4", "译5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse mismatch:\n got %v\nwant %v\nxml=%s", got, want, xml)
	}
}

func TestParseXMLStripsThinkAndHandlesGaps(t *testing.T) {
	content := `<think>reasoning here</think><t id="1">甲</t><t id="3">丙</t>`
	got := parseXMLTranslations(content, 3)
	want := []string{"甲", "", "丙"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestXMLEscapeUnescapeRoundTrip(t *testing.T) {
	in := `a & b < c > d`
	if out := xmlUnescape(xmlEscape(in)); out != in {
		t.Errorf("round trip failed: %q -> %q", in, out)
	}
}

func TestCollectPairBlanksWhenEqual(t *testing.T) {
	m := map[string]string{}
	collectPair(m, "同じ", "同じ") // jp==cn means untranslated
	if m["同じ"] != "" {
		t.Errorf("expected blank cn when jp==cn, got %q", m["同じ"])
	}
	collectPair(m, "日本語", "中文")
	if m["日本語"] != "中文" {
		t.Errorf("expected translated value, got %q", m["日本語"])
	}
}

func TestExtractMusicIsReadOnlyAndReturnsCatalog(t *testing.T) {
	tr, database, cfg := openCatalogTestTranslator(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/musics.json":
			fmt.Fprint(w, `[{"id":41,"title":"test-title","isNewlyWrittenMusic":true,"lyricist":"test-lyricist","composer":"test-composer","arranger":"-"}]`)
		case "/musicVocals.json":
			fmt.Fprint(w, `[{"id":41,"caption":"test-vocal-caption"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureSourceURLs(t, cfg, map[string]string{config.KeyUpstreamJPMasterdataURL: upstream.URL})

	fields, catalog, err := tr.extractMusic()
	if err != nil {
		t.Fatal(err)
	}
	if fields["title"].Pairs["test-title"] != "" || fields["vocalCaption"].Pairs["test-vocal-caption"] != "" {
		t.Fatalf("music extraction fields = %+v", fields)
	}
	if len(catalog) != 1 || catalog[0].MusicID != 41 || catalog[0].JapaneseTitle != "test-title" ||
		catalog[0].ProducerMetadata != "test-lyricist | test-composer" || !catalog[0].IsNewlyWrittenMusic {
		t.Fatalf("music extraction catalog = %+v", catalog)
	}
	var persisted int
	if err := database.QueryRow(`SELECT COUNT(*) FROM catalog_music WHERE music_id=41`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("read-only extraction persisted %d catalog records", persisted)
	}
}

func TestExtractCharactersSurfacesPerformerCatalogFetchFailure(t *testing.T) {
	tr, _, cfg := openCatalogTestTranslator(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/characterProfiles.json":
			fmt.Fprint(w, `[{"characterId":7,"hobby":"synthetic-hobby"}]`)
		case "/gameCharacters.json":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: upstream.URL,
		config.KeyUpstreamCNMasterdataURL: upstream.URL,
	})

	fields, catalog, err := tr.extractCharacters()
	if err == nil || !strings.Contains(err.Error(), "gameCharacters.json") || fields != nil || catalog != nil {
		t.Fatalf("performer fetch fields=%+v catalog=%+v err=%v", fields, catalog, err)
	}
}

func TestMusicCategoryApplyRollsBackTranslationAndCatalogTogether(t *testing.T) {
	tr, database, cfg := openCatalogTestTranslator(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/musics.json":
			fmt.Fprint(w, `[{"id":41,"title":"first-test-title"},{"id":42,"title":"failing-test-title"}]`)
		case "/musicVocals.json":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureSourceURLs(t, cfg, map[string]string{config.KeyUpstreamJPMasterdataURL: upstream.URL})
	fields, catalog, err := tr.extractMusic()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TRIGGER fail_music_catalog_insert BEFORE INSERT ON catalog_music
		WHEN NEW.music_id=42 BEGIN SELECT RAISE(ABORT, 'catalog insert failed'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := tr.store.ApplyCNCategoryWithCatalog("music", fields, catalog, nil); err == nil || !strings.Contains(err.Error(), "catalog insert failed") {
		t.Fatalf("music category apply error = %v", err)
	}
	var persistedCatalog, persistedEntries int
	if err := database.QueryRow(`SELECT COUNT(*) FROM catalog_music WHERE music_id IN (41, 42)`).Scan(&persistedCatalog); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM entries WHERE category='music'`).Scan(&persistedEntries); err != nil {
		t.Fatal(err)
	}
	if persistedCatalog != 0 || persistedEntries != 0 {
		t.Fatalf("partial music apply persisted catalog=%d entries=%d", persistedCatalog, persistedEntries)
	}
}

func TestTraceMapDedup(t *testing.T) {
	tm := newTraceMap("name")
	tm.add("name", "テスト", 1)
	tm.add("name", "テスト", 1) // duplicate
	tm.add("name", "テスト", 2)
	tm.add("name", "", 3)    // empty jp ignored
	tm.add("name", "テスト", 0) // zero id ignored
	if got := tm["name"]["テスト"]; !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("trace dedup: got %v", got)
	}
}

func TestNormalizeStorySource(t *testing.T) {
	cases := map[string]string{
		"official_cn":        "official_cn",
		"official_cn_legacy": "official_cn",
		"cn":                 "official_cn",
		"llm":                "llm",
		"human":              "human",
		"jp_pending":         "jp_pending",
		"":                   "jp_pending",
	}
	for in, want := range cases {
		if got := normalizeStorySource(in); got != want {
			t.Errorf("normalizeStorySource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchJPScenarioUsesHealthyFallbackImmediately(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(525)
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"TalkData":[{"Body":"hello"}]}`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPAssetsURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPAssetsFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)

	result, err := tr.fetchJPScenarioJSON("event_story/test/scenario/1")
	if err != nil {
		t.Fatal(err)
	}
	if !scenarioHasTalkData(result) {
		t.Fatalf("fallback result missing TalkData: %#v", result)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("dead primary was retried before fallback: primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestDefaultJPAssetSourceIsHealthyMirror(t *testing.T) {
	tr := &Translator{}
	bases := tr.jpAssetBases()
	if len(bases) == 0 || bases[0] != "https://assets.unipjsk.com/ondemand" {
		t.Fatalf("unexpected default JP asset sources: %v", bases)
	}
}

func TestMasterdataFallsBackToSecondarySource(t *testing.T) {
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
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPMasterdataURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPMasterdataFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)

	items, err := tr.fetchMasterdata("events.json", "jp")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("unexpected fallback result: items=%v primary=%d fallback=%d", items, primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestRateLimitedMasterdataSourceIsRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer upstream.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPMasterdataURL, upstream.URL)
	cfg.Set(config.KeyUpstreamJPMasterdataFallbackURL, upstream.URL)
	tr := New(nil, nil, cfg)

	items, err := tr.fetchMasterdata("events.json", "jp")
	if err != nil {
		t.Fatalf("rate-limited source was not retried: %v", err)
	}
	if len(items) != 1 || calls.Load() != 2 {
		t.Fatalf("unexpected retry result: items=%v calls=%d", items, calls.Load())
	}
}

// A rate-limited category must be skipped by cn-sync, not abort the whole run.
func TestRateLimitedFailureIsTransient(t *testing.T) {
	err := fmt.Errorf("GET https://upstream.invalid/events.json: http 429: %s", "slow down")
	if !isTransientErr(err) {
		t.Fatalf("http 429 is not classified as transient: %v", err)
	}
}

func TestCNMasterdataFallsBackToSecondarySource(t *testing.T) {
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
		fmt.Fprint(w, `[{"id":2,"name":"中文"}]`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamCNMasterdataURL, primary.URL)
	cfg.Set(config.KeyUpstreamCNMasterdataFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)

	items, err := tr.fetchMasterdata("events.json", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || getInt(items[0], "id") != 2 || primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("unexpected CN fallback: items=%v primary=%d fallback=%d", items, primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestCNScenarioFallsBackToSecondarySource(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"TalkData":[{"Body":"中文台词"}]}`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamCNAssetsURL, primary.URL)
	cfg.Set(config.KeyUpstreamCNAssetsFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)

	result, err := tr.fetchCNScenarioJSON("event_story/test/scenario/1")
	if err != nil {
		t.Fatal(err)
	}
	if !scenarioHasTalkData(result) {
		t.Fatalf("CN fallback result missing TalkData: %#v", result)
	}
}

func TestMasterdataHedgesSlowPrimary(t *testing.T) {
	primaryStarted := make(chan struct{}, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case primaryStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPMasterdataURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPMasterdataFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)
	tr.hedgeDelay = 20 * time.Millisecond

	started := time.Now()
	items, err := tr.fetchMasterdata("events.json", "jp")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || time.Since(started) > time.Second {
		t.Fatalf("slow primary was not hedged promptly: items=%v elapsed=%s", items, time.Since(started))
	}
	select {
	case <-primaryStarted:
	default:
		t.Fatal("primary source was not attempted")
	}
}

func TestBuildJPPendingEpisodesFetchesConcurrently(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maxActive.Load()
			if current <= seen || maxActive.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		scenarioID := strings.TrimSuffix(parts[len(parts)-1], ".json")
		fmt.Fprintf(w, `{"ScenarioId":%q,"Snippets":[],"TalkData":[{"Body":"line","WindowDisplayName":"name"}],"SpecialEffectData":[],"AppearCharacters":[]}`, scenarioID)
	}))
	defer server.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPAssetsURL, server.URL)
	cfg.Set(config.KeyUpstreamFetchConcurrency, "4")
	tr := New(nil, nil, cfg)
	story := map[string]any{
		"assetbundleName": "asset",
		"eventStoryEpisodes": []any{
			map[string]any{"episodeNo": float64(1), "scenarioId": "one", "title": "1"},
			map[string]any{"episodeNo": float64(2), "scenarioId": "two", "title": "2"},
			map[string]any{"episodeNo": float64(3), "scenarioId": "three", "title": "3"},
			map[string]any{"episodeNo": float64(4), "scenarioId": "four", "title": "4"},
		},
	}

	episodes, errs := tr.buildJPPendingEpisodes(story)
	if len(errs) != 0 || len(episodes) != 4 {
		t.Fatalf("unexpected build result: episodes=%d errors=%d", len(episodes), len(errs))
	}
	if maxActive.Load() < 2 {
		t.Fatalf("scenario requests were not concurrent; max=%d", maxActive.Load())
	}
	for _, episode := range episodes {
		if len(episode.lines) != 2 || episode.lines[0].Field != "body" || episode.lines[0].ScenarioPosition != 0 ||
			episode.lines[1].Field != "speaker" || episode.lines[1].ScenarioPosition != 1 {
			t.Fatalf("JP positional fields = %+v", episode.lines)
		}
	}
}

func TestOfficialEpisodesKeepJPFieldsWhenChineseMissingOrEqual(t *testing.T) {
	jp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ScenarioId":"scenario","Snippets":[],"TalkData":[{"Body":"同文","WindowDisplayName":"初音ミク"},{"Body":"未翻译","WindowDisplayName":"鏡音リン"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer jp.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"TalkData":[{"Body":"同文","WindowDisplayName":"初音ミク"},{"Body":"未翻译","WindowDisplayName":"鏡音リン"}]}`)
	}))
	defer cn.Close()
	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPAssetsURL, jp.URL)
	cfg.Set(config.KeyUpstreamCNAssetsURL, cn.URL)
	tr := New(nil, nil, cfg)
	jpStory := map[string]any{
		"assetbundleName":    "asset",
		"eventStoryEpisodes": []any{map[string]any{"episodeNo": float64(1), "scenarioId": "scenario", "title": "原題"}},
	}
	cnStory := map[string]any{
		"eventStoryEpisodes": []any{map[string]any{"episodeNo": float64(1), "title": "原題"}},
	}
	episodes, hasTalk, _, errs := tr.buildOfficialCNEpisodes(jpStory, cnStory)
	if len(errs) != 0 || hasTalk {
		t.Fatalf("official episode errors=%v hasTalk=%v", errs, hasTalk)
	}
	episode, ok := episodes["1"]
	if !ok || len(episode.lines) != 4 {
		t.Fatalf("missing JP-only positional fields: %+v", episodes)
	}
	want := []struct {
		position int
		field    string
		key      string
	}{{0, "body", "同文"}, {1, "speaker", "初音ミク"}, {2, "body", "未翻译"}, {3, "speaker", "鏡音リン"}}
	for index, expected := range want {
		line := episode.lines[index]
		if line.ScenarioPosition != expected.position || line.Field != expected.field || line.JPKey != expected.key || line.Text != "" {
			t.Fatalf("line %d = %+v, want %+v", index, line, expected)
		}
	}
}

func TestRetryOfficialTalkLengthMismatchPreservesExistingEvent(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(101, model.EventStoryMeta{Source: model.SourceLLM}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "old", Title: "旧标题", TitleSource: model.SourceLLM,
		TalkKeys: []string{"旧原文"}, TalkData: map[string]string{"旧原文": "保留译文"},
		TalkSources: map[string]string{"旧原文": model.SourceLLM},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := events.Detail(101)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jp-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":101,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"scenario","title":"原題"}]}]`)
		case "/cn-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":101,"eventStoryEpisodes":[{"episodeNo":1,"title":"标题"}]}]`)
		case "/cn-master/events.json":
			fmt.Fprint(w, `[{"id":101}]`)
		case "/jp-assets/event_story/asset/scenario/scenario.json":
			fmt.Fprint(w, `{"ScenarioId":"scenario","Snippets":[],"TalkData":[{"Body":"一"},{"Body":"二"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
		case "/cn-assets/event_story/asset/scenario/scenario.json":
			fmt.Fprint(w, `{"TalkData":[{"Body":"第一"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureLocalSources(t, cfg, upstream.URL)

	result, err := tr.RetryEventStorySync(101)
	if err == nil || !strings.Contains(err.Error(), "TalkData length mismatch") || result != nil {
		t.Fatalf("retry mismatch result=%v err=%v", result, err)
	}
	after, detailErr := events.Detail(101)
	if detailErr != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("mismatched official retry changed event: after=%+v before=%+v err=%v", after, before, detailErr)
	}
}

func TestRetryOfficialPreservesEmptyEpisodeIdentity(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jp-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":102,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"empty","title":""},{"episodeNo":2,"scenarioId":"content","title":"原題"}]}]`)
		case "/cn-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":102,"eventStoryEpisodes":[{"episodeNo":1,"title":""},{"episodeNo":2,"title":"标题"}]}]`)
		case "/cn-master/events.json":
			fmt.Fprint(w, `[{"id":102}]`)
		case "/jp-assets/event_story/asset/scenario/empty.json":
			fmt.Fprint(w, `{"ScenarioId":"empty","Snippets":[],"TalkData":[],"SpecialEffectData":[],"AppearCharacters":[]}`)
		case "/cn-assets/event_story/asset/scenario/empty.json":
			fmt.Fprint(w, `{"TalkData":[]}`)
		case "/jp-assets/event_story/asset/scenario/content.json":
			fmt.Fprint(w, `{"ScenarioId":"content","Snippets":[],"TalkData":[{"Body":"原文"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
		case "/cn-assets/event_story/asset/scenario/content.json":
			fmt.Fprint(w, `{"TalkData":[{"Body":"译文"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureLocalSources(t, cfg, upstream.URL)

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := tr.RetryEventStorySync(102)
		if err != nil || result["source"] != "official_cn" || result["episodes"] != 2 {
			t.Fatalf("retry %d result=%v err=%v", attempt, result, err)
		}
	}
	detail, err := events.Detail(102)
	if err != nil {
		t.Fatal(err)
	}
	empty, ok := detail.Episodes["1"]
	if len(detail.Episodes) != 2 || !ok || empty.ScenarioID != "empty" || empty.Title != "" || len(empty.TalkData) != 0 {
		t.Fatalf("empty official episode was dropped: %+v", detail)
	}
}

func TestRetryResponseUsesPersistedPendingSourceAfterPartialAI(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jp-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":103,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"scenario","title":""}]}]`)
		case "/cn-master/eventStories.json", "/cn-master/events.json":
			fmt.Fprint(w, `[]`)
		case "/jp-assets/event_story/asset/scenario/scenario.json":
			fmt.Fprint(w, `{"ScenarioId":"scenario","Snippets":[],"TalkData":[{"Body":"一"},{"Body":"二"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
		case "/llm/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]string{"content": `<translations><t id="1">译一</t></translations>`},
			}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureLocalSources(t, cfg, upstream.URL)
	for key, value := range map[string]string{
		config.KeyOpenAIAPIKey: "test", config.KeyOpenAIBaseURL: upstream.URL + "/llm",
		config.KeyOpenAIModel: "test-model", config.KeyBatchSize: "20", config.KeyRateDelayMS: "0",
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}

	result, err := tr.RetryEventStorySync(103)
	if err != nil || result["source"] != "jp_pending" || result["translated"] != 1 {
		t.Fatalf("partial AI retry result=%v err=%v", result, err)
	}
	detail, err := events.Detail(103)
	if err != nil || detail.Meta.Source != "jp_pending" || detail.Episodes["1"].TalkData["一"] != "译一" || detail.Episodes["1"].TalkData["二"] != "" {
		t.Fatalf("partial AI detail=%+v err=%v", detail, err)
	}
}

func TestJPPendingEpisodeFailureDoesNotPartiallyReplaceEvent(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/two.json") {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"ScenarioId":"one","Snippets":[],"TalkData":[{"Body":"一","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer assets.Close()
	configureSourceURLs(t, cfg, map[string]string{config.KeyUpstreamJPAssetsURL: assets.URL})
	stories := []map[string]any{{
		"eventId": float64(91), "assetbundleName": "asset",
		"eventStoryEpisodes": []any{
			map[string]any{"episodeNo": float64(1), "scenarioId": "one", "title": "一"},
			map[string]any{"episodeNo": float64(2), "scenarioId": "two", "title": "二"},
		},
	}}
	outcome, err := tr.fillEventStoriesJPPending(stories, 1, map[int]store.EventSyncState{}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 0 || len(outcome.PartialErrors) != 1 {
		t.Fatalf("partial JP outcome = %+v", outcome)
	}
	exists, err := events.Exists(91)
	if err != nil || exists {
		t.Fatalf("partial JP event exists=%v err=%v", exists, err)
	}
}

func TestMalformedEventEpisodeIdentitiesFailClosedBeforeFetch(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(104, model.EventStoryMeta{Source: model.SourceLLM}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "old", Title: "旧标题", TitleSource: model.SourceLLM,
		TalkKeys: []string{"旧原文"}, TalkData: map[string]string{"旧原文": "保留译文"},
		TalkSources: map[string]string{"旧原文": model.SourceLLM},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := events.Detail(104)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer assets.Close()
	configureSourceURLs(t, cfg, map[string]string{config.KeyUpstreamJPAssetsURL: assets.URL})
	cases := map[string][]any{
		"non-object":         {"bad"},
		"non-positive":       {map[string]any{"episodeNo": float64(0), "scenarioId": "one"}},
		"duplicate episode":  {map[string]any{"episodeNo": float64(1), "scenarioId": "one"}, map[string]any{"episodeNo": float64(1), "scenarioId": "two"}},
		"missing scenario":   {map[string]any{"episodeNo": float64(1), "scenarioId": ""}},
		"duplicate scenario": {map[string]any{"episodeNo": float64(1), "scenarioId": "one"}, map[string]any{"episodeNo": float64(2), "scenarioId": "one"}},
	}
	for name, rawEpisodes := range cases {
		t.Run(name, func(t *testing.T) {
			outcome, err := tr.fillEventStoriesJPPending([]map[string]any{{
				"eventId": float64(104), "assetbundleName": "asset", "eventStoryEpisodes": rawEpisodes,
			}}, 1, map[int]store.EventSyncState{}, 0, 1)
			if err != nil || outcome.Processed != 0 || len(outcome.PartialErrors) == 0 {
				t.Fatalf("malformed outcome=%+v err=%v", outcome, err)
			}
			after, detailErr := events.Detail(104)
			if detailErr != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("malformed build changed event: after=%+v before=%+v err=%v", after, before, detailErr)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("malformed identities triggered %d scenario requests", requests.Load())
	}

	validJP := map[string]any{"assetbundleName": "asset", "eventStoryEpisodes": []any{
		map[string]any{"episodeNo": float64(1), "scenarioId": "one"},
	}}
	invalidCN := map[string]any{"eventStoryEpisodes": []any{
		map[string]any{"episodeNo": float64(1)}, map[string]any{"episodeNo": float64(1)},
	}}
	if episodes, _, _, errs := tr.buildOfficialCNEpisodes(validJP, invalidCN); len(episodes) != 0 || len(errs) == 0 {
		t.Fatalf("duplicate CN identities episodes=%+v errors=%v", episodes, errs)
	}
}

func TestOfficialEpisodeFailureDoesNotPartiallyReplaceExistingEvent(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(93, model.EventStoryMeta{Source: model.SourceLLM}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "old", Title: "旧标题", TitleSource: model.SourceHuman,
		TalkKeys: []string{"旧原文"}, TalkData: map[string]string{"旧原文": "保留译文"}, TalkSources: map[string]string{"旧原文": model.SourceLLM},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := events.Detail(93)
	if err != nil {
		t.Fatal(err)
	}
	jpMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") {
			fmt.Fprint(w, `[{"eventId":93,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"one","title":"一"},{"episodeNo":2,"scenarioId":"two","title":"二"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer jpMaster.Close()
	cnMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/eventStories.json"):
			fmt.Fprint(w, `[{"eventId":93,"eventStoryEpisodes":[{"episodeNo":1,"title":"一中"},{"episodeNo":2,"title":"二中"}]}]`)
		case strings.HasSuffix(r.URL.Path, "/events.json"):
			fmt.Fprint(w, `[{"id":93}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cnMaster.Close()
	jpAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/two.json") {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"ScenarioId":"one","Snippets":[],"TalkData":[{"Body":"一","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer jpAssets.Close()
	cnAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"TalkData":[{"Body":"一中","WindowDisplayName":"角色"}]}`)
	}))
	defer cnAssets.Close()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: jpMaster.URL,
		config.KeyUpstreamCNMasterdataURL: cnMaster.URL,
		config.KeyUpstreamJPAssetsURL:     jpAssets.URL,
		config.KeyUpstreamCNAssetsURL:     cnAssets.URL,
	})
	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 0 || len(outcome.PartialErrors) == 0 {
		t.Fatalf("official partial outcome = %+v", outcome)
	}
	after, err := events.Detail(93)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("official partial sync changed event: after=%+v before=%+v err=%v", after, before, err)
	}
}

func TestRetryEventStorySyncFallbackFailurePreservesExistingEvent(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(95, model.EventStoryMeta{Source: model.SourceLLM}, []store.OrderedEpisode{
		{EpisodeNo: "1", ScenarioID: "old-one", Title: "旧一", TalkKeys: []string{"旧一"}, TalkData: map[string]string{"旧一": "保留一"}},
		{EpisodeNo: "2", ScenarioID: "old-two", Title: "旧二", TalkKeys: []string{"旧二"}, TalkData: map[string]string{"旧二": "保留二"}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := events.Detail(95)
	if err != nil {
		t.Fatal(err)
	}
	jpMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") {
			fmt.Fprint(w, `[{"eventId":95,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"one","title":"一"},{"episodeNo":2,"scenarioId":"two","title":"二"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer jpMaster.Close()
	cnMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") || strings.HasSuffix(r.URL.Path, "/events.json") {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer cnMaster.Close()
	jpAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/two.json") {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"ScenarioId":"one","Snippets":[],"TalkData":[{"Body":"一","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer jpAssets.Close()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: jpMaster.URL,
		config.KeyUpstreamCNMasterdataURL: cnMaster.URL,
		config.KeyUpstreamJPAssetsURL:     jpAssets.URL,
	})
	result, err := tr.RetryEventStorySync(95)
	if err == nil || !strings.Contains(err.Error(), "incomplete JP episode fetch") || result != nil {
		t.Fatalf("retry result=%v err=%v", result, err)
	}
	after, detailErr := events.Detail(95)
	if detailErr != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed retry changed event: after=%+v before=%+v err=%v", after, before, detailErr)
	}
}

func TestSyncBackfillsSkippedHumanEventScenarioWithoutChangingTranslations(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(92, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "human-scenario", Title: "人工标题", TitleSource: model.SourceHuman,
		TalkKeys: []string{"原文"}, TalkData: map[string]string{"原文": "人工译文"}, TalkSources: map[string]string{"原文": model.SourcePinned},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := events.Detail(92)
	if err != nil {
		t.Fatal(err)
	}
	jpMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") {
			fmt.Fprint(w, `[{"eventId":92,"assetbundleName":"human-asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"human-scenario","title":"原題"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer jpMaster.Close()
	cnMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") || strings.HasSuffix(r.URL.Path, "/events.json") {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer cnMaster.Close()
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ScenarioId":"human-scenario","Snippets":[{"Action":1,"ReferenceIndex":0}],"TalkData":[{"Body":"原文","WindowDisplayName":"角色","Voices":[]}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer assets.Close()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: jpMaster.URL,
		config.KeyUpstreamCNMasterdataURL: cnMaster.URL,
		config.KeyUpstreamJPAssetsURL:     assets.URL,
	})
	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 0 || len(outcome.PartialErrors) != 0 {
		t.Fatalf("human backfill outcome = %+v", outcome)
	}
	after, err := events.Detail(92)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("scenario backfill changed translations: after=%+v before=%+v err=%v", after, before, err)
	}
	missing, err := events.MissingScenarioEpisodes(92)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing scenarios=%+v err=%v", missing, err)
	}
	snapshot, err := events.EpisodeSnapshot(92, "1", model.LocaleChinese)
	if err != nil || snapshot.Scenario.ScenarioID != "human-scenario" || snapshot.Segments[1].Text != "人工译文" {
		t.Fatalf("backfilled snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRollingScenarioReplacementBackfillPreservesRecoveryLocalesAndRestoresExactExport(t *testing.T) {
	path := t.TempDir() + "/rolling-backfill.db"
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	oldCanonical, oldDigest, err := store.CanonicalizeEventScenario(map[string]any{
		"ScenarioId": "old-scenario", "Snippets": []any{},
		"TalkData": []any{
			map[string]any{"Body": "共有原文", "WindowDisplayName": "旧話者", "Voices": []any{}},
			map[string]any{"Body": "変更前", "WindowDisplayName": "", "Voices": []any{}},
		},
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, "old-scenario")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	events := store.NewEventStore(database)
	if err := events.ImportOrdered(88, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "old-scenario", ScenarioCanonicalJSON: oldCanonical, ScenarioSHA256: oldDigest,
		Title: "旧人工标题", TitleSource: model.SourceHuman, SourceTitle: "旧原題",
		TalkKeys:    []string{"共有原文", "変更前"},
		TalkData:    map[string]string{"共有原文": "人工共享", "変更前": "人工旧译"},
		TalkSources: map[string]string{"共有原文": model.SourceHuman, "変更前": model.SourcePinned},
		Lines: []store.OrderedLine{
			{JPKey: "共有原文", Text: "人工共享", Source: model.SourceHuman, ScenarioPosition: 0, Field: "body"},
			{JPKey: "旧話者", Text: "", Source: model.SourceUnknown, ScenarioPosition: 1, Field: "speaker"},
			{JPKey: "変更前", Text: "人工旧译", Source: model.SourcePinned, ScenarioPosition: 2, Field: "body"},
		},
	}}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	oldDetail, err := events.DetailLocale(88, model.LocaleEnglish)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	var sharedSegment, changedSegment model.EventStorySegment
	for _, segment := range oldDetail.Episodes["1"].Segments {
		switch segment.Japanese {
		case "共有原文":
			sharedSegment = segment
		case "変更前":
			changedSegment = segment
		}
	}
	if sharedSegment.ID == "" || changedSegment.ID == "" {
		database.Close()
		t.Fatalf("old source segments=%+v", oldDetail.Episodes["1"].Segments)
	}
	if err := events.UpdateLineLocale(88, "1", "共有原文", sharedSegment.ID, sharedSegment.SourceHash,
		"Shared English", model.SourceHuman, "talk", model.LocaleEnglish, "editor"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := events.UpdateLineLocale(88, "1", "変更前", changedSegment.ID, changedSegment.SourceHash,
		"Old English", model.SourceHuman, "talk", model.LocaleEnglish, "editor"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM event_stories WHERE event_id=88`,
		`INSERT INTO event_stories(event_id, source, version, last_updated) VALUES (88, 'human', '2', 200)`,
		`INSERT INTO event_story_episodes(event_id, episode_no, scenario_id, title, title_source, position)
		 VALUES (88, '1', 'new-scenario', '新人工标题', 'human', 0)`,
		`INSERT INTO event_story_lines(event_id, episode_no, jp_key, cn_text, source, position)
		 VALUES (88, '1', '共有原文', '人工共享', 'human', 0)`,
		`INSERT INTO event_story_lines(event_id, episode_no, jp_key, cn_text, source, position)
		 VALUES (88, '1', '変更後', '人工新译', 'pinned', 1)`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			legacy.Close()
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var unmatchedBefore int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM event_story_segment_localizations
		WHERE segment_id=? AND locale=? AND text='Old English'`, changedSegment.ID, model.LocaleEnglish).Scan(&unmatchedBefore); err != nil || unmatchedBefore != 1 {
		t.Fatalf("unmatched recovery locale before backfill=%d err=%v", unmatchedBefore, err)
	}
	cfg, err := config.New(reopened, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	contentStore := store.New(reopened)
	events = store.NewEventStore(reopened)
	tr := New(contentStore, events, cfg)
	jpMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") {
			fmt.Fprint(w, `[{"eventId":88,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"new-scenario","title":"新原題"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer jpMaster.Close()
	cnMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") || strings.HasSuffix(r.URL.Path, "/events.json") {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer cnMaster.Close()
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ScenarioId":"new-scenario","Snippets":[],"TalkData":[{"Body":"共有原文","WindowDisplayName":"新話者","Voices":[]},{"Body":"変更後","WindowDisplayName":"","Voices":[]}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer assets.Close()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: jpMaster.URL,
		config.KeyUpstreamCNMasterdataURL: cnMaster.URL,
		config.KeyUpstreamJPAssetsURL:     assets.URL,
	})
	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil || outcome.Processed != 0 || len(outcome.PartialErrors) != 0 {
		t.Fatalf("rolling backfill outcome=%+v err=%v", outcome, err)
	}
	missing, err := events.MissingScenarioEpisodes(88)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing after backfill=%+v err=%v", missing, err)
	}
	snapshot, err := events.EpisodeSnapshot(88, "1", model.LocaleEnglish)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{}
	for _, segment := range snapshot.Segments {
		texts[segment.Japanese] = segment.Text
		if segment.Japanese == "変更前" || segment.ID == changedSegment.ID {
			t.Fatalf("snapshot exposed stale segment=%+v", segment)
		}
	}
	if texts["共有原文"] != "Shared English" || texts["変更後"] != "" {
		t.Fatalf("backfilled snapshot texts=%+v", texts)
	}

	exported, err := contentStore.ExportEventContent()
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range exported.Segments {
		if segment.ScenarioID != "new-scenario" || segment.SegmentID == changedSegment.ID {
			t.Fatalf("exported stale segment=%+v", segment)
		}
	}
	for _, localization := range exported.Localizations {
		if localization.SegmentID == changedSegment.ID || localization.Text == "Old English" {
			t.Fatalf("exported stale localization=%+v", localization)
		}
	}
	destinationDB, err := db.Open(t.TempDir() + "/rolling-restore.db")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationDB.Close()
	destinationEvents := store.NewEventStore(destinationDB)
	if err := destinationEvents.ImportOrdered(88, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "new-scenario", Title: "新人工标题", TitleSource: model.SourceHuman,
		TalkKeys: []string{"共有原文", "変更後"}, TalkData: map[string]string{"共有原文": "人工共享", "変更後": "人工新译"},
		TalkSources: map[string]string{"共有原文": model.SourceHuman, "変更後": model.SourcePinned},
	}}); err != nil {
		t.Fatal(err)
	}
	destinationStore := store.New(destinationDB)
	if err := destinationStore.ImportTranslationContent(nil, exported, store.LyricsContentExport{}); err != nil {
		t.Fatal(err)
	}
	restored, err := destinationEvents.EpisodeSnapshot(88, "1", model.LocaleEnglish)
	if err != nil || !reflect.DeepEqual(restored.Segments, snapshot.Segments) || restored.Scenario.SHA256 != snapshot.Scenario.SHA256 {
		t.Fatalf("restored snapshot=%+v want=%+v err=%v", restored, snapshot, err)
	}
	var unmatchedAfter int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM event_story_segment_localizations
		WHERE segment_id=? AND locale=? AND text='Old English'`, changedSegment.ID, model.LocaleEnglish).Scan(&unmatchedAfter); err != nil || unmatchedAfter != 1 {
		t.Fatalf("unmatched recovery locale after export=%d err=%v", unmatchedAfter, err)
	}
}

func TestScenarioBackfillFetchesOnlyMissingEpisodes(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	oneJSON, _, err := store.CanonicalizeEventScenario(map[string]any{
		"ScenarioId": "one", "Snippets": []any{}, "TalkData": []any{},
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, "one")
	if err != nil {
		t.Fatal(err)
	}
	twoJSON, twoSHA, err := store.CanonicalizeEventScenario(map[string]any{
		"ScenarioId": "two", "Snippets": []any{}, "TalkData": []any{},
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, "two")
	if err != nil {
		t.Fatal(err)
	}
	if err := events.ImportOrdered(94, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{
		{EpisodeNo: "1", ScenarioID: "one"},
		{EpisodeNo: "2", ScenarioID: "two"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := events.BackfillScenarios(94, []store.OrderedEpisode{{
		EpisodeNo: "2", ScenarioID: "two", ScenarioCanonicalJSON: twoJSON, ScenarioSHA256: twoSHA,
	}}); err != nil {
		t.Fatal(err)
	}
	var oneRequests, twoRequests atomic.Int32
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/one.json"):
			oneRequests.Add(1)
			fmt.Fprint(w, oneJSON)
		case strings.HasSuffix(r.URL.Path, "/two.json"):
			twoRequests.Add(1)
			http.Error(w, "unrelated failure", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer assets.Close()
	configureSourceURLs(t, cfg, map[string]string{config.KeyUpstreamJPAssetsURL: assets.URL})
	states, _, err := events.EventSyncStates()
	if err != nil {
		t.Fatal(err)
	}
	outcome := eventStorySyncOutcome{}
	tr.backfillMissingEventScenarios([]map[string]any{{
		"eventId": float64(94), "assetbundleName": "asset",
		"eventStoryEpisodes": []any{
			"unrelated malformed entry",
			map[string]any{"episodeNo": float64(1), "scenarioId": "one"},
			map[string]any{"episodeNo": float64(2), "scenarioId": "two"},
			map[string]any{"episodeNo": float64(2), "scenarioId": "unrelated-duplicate"},
			map[string]any{"episodeNo": float64(3), "scenarioId": "one"},
		},
	}}, states, &outcome, 0, 1)
	if len(outcome.PartialErrors) != 0 || oneRequests.Load() != 1 || twoRequests.Load() != 0 {
		t.Fatalf("backfill outcome=%+v requests one=%d two=%d", outcome, oneRequests.Load(), twoRequests.Load())
	}
	missing, err := events.MissingScenarioEpisodes(94)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing scenarios=%+v err=%v", missing, err)
	}
}

func TestOfficialSyncRechecksProtectedEditInsideImportTransaction(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	canonical, digest, err := store.CanonicalizeEventScenario(map[string]any{
		"ScenarioId": "scenario", "Snippets": []any{},
		"TalkData":          []any{map[string]any{"Body": "旧原文", "WindowDisplayName": "角色", "Voices": []any{}}},
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, "scenario")
	if err != nil {
		t.Fatal(err)
	}
	if err := events.ImportOrdered(96, model.EventStoryMeta{Source: model.SourceLLM}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario", ScenarioCanonicalJSON: canonical, ScenarioSHA256: digest,
		TalkKeys: []string{"旧原文"}, TalkData: map[string]string{"旧原文": "旧机器译文"}, TalkSources: map[string]string{"旧原文": model.SourceLLM},
	}}); err != nil {
		t.Fatal(err)
	}
	jpMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") {
			fmt.Fprint(w, `[{"eventId":96,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"scenario","title":"原題"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer jpMaster.Close()
	cnMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/eventStories.json"):
			fmt.Fprint(w, `[{"eventId":96,"eventStoryEpisodes":[{"episodeNo":1,"title":"标题"}]}]`)
		case strings.HasSuffix(r.URL.Path, "/events.json"):
			fmt.Fprint(w, `[{"id":96}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cnMaster.Close()
	editResult := make(chan error, 1)
	var edited atomic.Bool
	jpAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if edited.CompareAndSwap(false, true) {
			editResult <- events.UpdateLine(96, "1", "旧原文", "网络窗口人工编辑", model.SourceHuman, "talk")
		}
		fmt.Fprint(w, `{"ScenarioId":"scenario","Snippets":[],"TalkData":[{"Body":"新原文","WindowDisplayName":"角色","Voices":[]}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer jpAssets.Close()
	cnAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"TalkData":[{"Body":"官方译文","WindowDisplayName":"角色"}]}`)
	}))
	defer cnAssets.Close()
	configureSourceURLs(t, cfg, map[string]string{
		config.KeyUpstreamJPMasterdataURL: jpMaster.URL,
		config.KeyUpstreamCNMasterdataURL: cnMaster.URL,
		config.KeyUpstreamJPAssetsURL:     jpAssets.URL,
		config.KeyUpstreamCNAssetsURL:     cnAssets.URL,
	})
	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-editResult; err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 0 {
		t.Fatalf("protected official sync outcome=%+v", outcome)
	}
	detail, err := events.Detail(96)
	if err != nil || detail.Episodes["1"].TalkData["旧原文"] != "网络窗口人工编辑" ||
		detail.Episodes["1"].TalkSources["旧原文"] != model.SourceHuman {
		t.Fatalf("protected official detail=%+v err=%v", detail, err)
	}
}

func TestJPPendingSyncRechecksProtectedEventCreatedDuringFetch(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	created := make(chan error, 1)
	var once atomic.Bool
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if once.CompareAndSwap(false, true) {
			created <- events.ImportOrdered(97, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{{
				EpisodeNo: "1", ScenarioID: "manual", Title: "人工标题", TitleSource: model.SourceHuman,
				TalkKeys: []string{"人工原文"}, TalkData: map[string]string{"人工原文": "人工译文"}, TalkSources: map[string]string{"人工原文": model.SourceHuman},
			}})
		}
		fmt.Fprint(w, `{"ScenarioId":"remote","Snippets":[],"TalkData":[{"Body":"远端原文","WindowDisplayName":"角色","Voices":[]}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer assets.Close()
	configureSourceURLs(t, cfg, map[string]string{config.KeyUpstreamJPAssetsURL: assets.URL})
	stories := []map[string]any{{
		"eventId": float64(97), "assetbundleName": "asset",
		"eventStoryEpisodes": []any{map[string]any{"episodeNo": float64(1), "scenarioId": "remote", "title": "远端标题"}},
	}}
	outcome, err := tr.fillEventStoriesJPPending(stories, 1, map[int]store.EventSyncState{}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 0 {
		t.Fatalf("protected JP-pending outcome=%+v", outcome)
	}
	detail, err := events.Detail(97)
	if err != nil || detail.Episodes["1"].Title != "人工标题" || detail.Episodes["1"].TalkData["人工原文"] != "人工译文" {
		t.Fatalf("protected JP-pending detail=%+v err=%v", detail, err)
	}
}

func TestAutomaticEventAIRechecksProtectedEditAfterNetwork(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(98, model.EventStoryMeta{Source: "jp_pending"}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario", TalkKeys: []string{"原文"},
		TalkData: map[string]string{"原文": ""}, TalkSources: map[string]string{"原文": model.SourceUnknown},
	}}); err != nil {
		t.Fatal(err)
	}
	editResult := make(chan error, 1)
	var edited atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if edited.CompareAndSwap(false, true) {
			editResult <- events.UpdateLine(98, "1", "原文", "人工译文", model.SourceHuman, "talk")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]string{"content": `<translations><t id="1">机器译文</t></translations>`},
		}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()
	if err := cfg.Set(config.KeyOpenAIAPIKey, "test"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyOpenAIBaseURL, server.URL); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyOpenAIModel, "test-model"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyLLMMaxRetries, "0"); err != nil {
		t.Fatal(err)
	}
	translated, err := tr.autoTranslateEventStory(98)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-editResult; err != nil {
		t.Fatal(err)
	}
	if translated != 0 {
		t.Fatalf("protected automatic translation count=%d", translated)
	}
	detail, err := events.Detail(98)
	if err != nil || detail.Meta.Source != "jp_pending" || detail.Episodes["1"].TalkData["原文"] != "人工译文" ||
		detail.Episodes["1"].TalkSources["原文"] != model.SourceHuman {
		t.Fatalf("protected automatic detail=%+v err=%v", detail, err)
	}
}

func TestManualEventAIRechecksProtectedEditAfterNetwork(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(89, model.EventStoryMeta{Source: "jp_pending"}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario", TalkKeys: []string{"原文"},
		TalkData: map[string]string{"原文": ""}, TalkSources: map[string]string{"原文": model.SourceUnknown},
	}}); err != nil {
		t.Fatal(err)
	}
	editResult := make(chan error, 1)
	var edited atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if edited.CompareAndSwap(false, true) {
			editResult <- events.UpdateLine(89, "1", "原文", "网络窗口人工译文", model.SourcePinned, "talk")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]string{"content": `<translations><t id="1">机器译文</t></translations>`},
		}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()
	for key, value := range map[string]string{
		config.KeyOpenAIAPIKey: "test", config.KeyOpenAIBaseURL: server.URL,
		config.KeyOpenAIModel: "test-model", config.KeyLLMMaxRetries: "0",
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	translated, err := tr.translateEventStory(89, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-editResult; err != nil {
		t.Fatal(err)
	}
	if translated != 0 {
		t.Fatalf("protected manual translation count=%d", translated)
	}
	detail, err := events.Detail(89)
	if err != nil || detail.Meta.Source != "jp_pending" || detail.Episodes["1"].TalkData["原文"] != "网络窗口人工译文" ||
		detail.Episodes["1"].TalkSources["原文"] != model.SourcePinned {
		t.Fatalf("protected manual detail=%+v err=%v", detail, err)
	}
}

func TestManualEventAIFillsUntouchedGapInPartiallyHumanStory(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(105, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario", TalkKeys: []string{"人工原文", "待补原文"},
		TalkData:    map[string]string{"人工原文": "人工译文", "待补原文": ""},
		TalkSources: map[string]string{"人工原文": model.SourceHuman, "待补原文": model.SourceUnknown},
	}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]string{"content": `<translations><t id="1">机器补译</t></translations>`},
		}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()
	for key, value := range map[string]string{
		config.KeyOpenAIAPIKey: "test", config.KeyOpenAIBaseURL: server.URL,
		config.KeyOpenAIModel: "test-model", config.KeyLLMMaxRetries: "0",
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	translated, err := tr.translateEventStory(105, "openai")
	if err != nil || translated != 1 {
		t.Fatalf("manual gap fill translated=%d err=%v", translated, err)
	}
	detail, err := events.Detail(105)
	if err != nil || detail.Meta.Source != model.SourceHuman ||
		detail.Episodes["1"].TalkData["人工原文"] != "人工译文" ||
		detail.Episodes["1"].TalkSources["人工原文"] != model.SourceHuman ||
		detail.Episodes["1"].TalkData["待补原文"] != "机器补译" ||
		detail.Episodes["1"].TalkSources["待补原文"] != model.SourceLLM {
		t.Fatalf("manual gap fill detail=%+v err=%v", detail, err)
	}
}

func TestEventStoryTranslationPersistsBatchesAndResumes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		if call == 2 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"delta": map[string]string{"content": `<translations><t id="1">译一</t><t id="2">译二</t></translations>`},
			}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()

	tr, es, cfg := openTestTranslator(t)
	cfg.Set(config.KeyOpenAIAPIKey, "test")
	cfg.Set(config.KeyOpenAIBaseURL, server.URL)
	cfg.Set(config.KeyOpenAIModel, "test-model")
	cfg.Set(config.KeyBatchSize, "2")
	cfg.Set(config.KeyRateDelayMS, "0")
	cfg.Set(config.KeyLLMRequestTimeoutMS, "1000")
	cfg.Set(config.KeyLLMMaxRetries, "0")

	const eventID = 99
	keys := []string{"一", "二", "三", "四"}
	talkData := map[string]string{}
	talkSources := map[string]string{}
	for _, key := range keys {
		talkData[key] = ""
		talkSources[key] = model.SourceUnknown
	}
	if err := es.ImportOrdered(eventID, model.EventStoryMeta{Source: "jp_pending"}, []store.OrderedEpisode{{
		EpisodeNo:   "1",
		ScenarioID:  "scenario",
		TalkKeys:    keys,
		TalkData:    talkData,
		TalkSources: talkSources,
	}}); err != nil {
		t.Fatal(err)
	}

	var progress []int
	tr.SetProgress(func(_ string, _ string, current, _ int) {
		progress = append(progress, current)
	})
	count, err := tr.translateEventStory(eventID, "openai")
	if err == nil {
		t.Fatal("expected the second batch to fail")
	}
	if count != 2 {
		t.Fatalf("persisted count = %d, want 2", count)
	}
	remaining, err := es.UntranslatedTargets(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining after partial failure = %d, want 2", len(remaining))
	}
	for _, current := range progress {
		if current > 2 {
			t.Fatalf("progress advanced before failed batch was saved: %v", progress)
		}
	}
	if len(progress) == 0 || progress[0] != 0 {
		t.Fatalf("initial progress should be zero: %v", progress)
	}

	count, err = tr.translateEventStory(eventID, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("resume translated %d, want 2", count)
	}
	remaining, err = es.UntranslatedTargets(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining after resume = %d, want 0", len(remaining))
	}
}

func TestCallLLMHonorsRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr, _, cfg := openTestTranslator(t)
	cfg.Set(config.KeyOpenAIAPIKey, "test")
	cfg.Set(config.KeyOpenAIBaseURL, server.URL)
	cfg.Set(config.KeyLLMRequestTimeoutMS, "40")
	cfg.Set(config.KeyLLMMaxRetries, "0")

	started := time.Now()
	_, err := tr.callLLM("openai", []string{"待翻译"})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("request timeout was not enforced promptly: %s", elapsed)
	}
}

func TestLLMResponseLimitsJSONAndStreamingAggregates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(maxLLMResponseBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	tr, _, cfg := openTestTranslator(t)
	_ = cfg.Set(config.KeyOpenAIAPIKey, "test")
	_ = cfg.Set(config.KeyOpenAIBaseURL, server.URL)
	if _, err := tr.callOpenAI(context.Background(), "prompt", tr.snapshotConfig()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized JSON response error = %v", err)
	}
	var aggregate strings.Builder
	aggregate.WriteString(strings.Repeat("x", maxLLMResponseBytes))
	aggregateBytes := maxLLMResponseBytes
	if err := appendLLMText(&aggregate, "x", &aggregateBytes); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized stream aggregate error = %v", err)
	}
}

func TestAutomaticLLMFailureSkipsRetriesAndOpensCooldown(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	tr, _, cfg := openTestTranslator(t)
	cfg.Set(config.KeyOpenAIAPIKey, "test")
	cfg.Set(config.KeyOpenAIBaseURL, server.URL)
	cfg.Set(config.KeyLLMMaxRetries, "5")

	_, err := tr.callAutomaticLLM("openai", []string{"待翻译"}, nil)
	if err == nil {
		t.Fatal("expected automatic LLM call to fail")
	}
	if calls.Load() != 1 {
		t.Fatalf("automatic translation retried %d times, want one fail-fast attempt", calls.Load())
	}
	if reason, unavailable := tr.automaticLLMUnavailable(); !unavailable || !strings.Contains(reason, "unavailable") {
		t.Fatalf("LLM cooldown was not opened: unavailable=%v reason=%q", unavailable, reason)
	}
}

func TestCNSyncSkippedErrorIncludesDetails(t *testing.T) {
	result := CNSyncResult{}
	result.addSkipped("cards", fmt.Errorf("GET mirror/cards.json: i/o timeout"))
	result.addSkipped("cards", fmt.Errorf("GET fallback/cards.json: TLS handshake timeout"))

	if len(result.Skipped) != 1 {
		t.Fatalf("skipped categories were not deduplicated: %v", result.Skipped)
	}
	err := result.SkippedError()
	if err == nil || !strings.Contains(err.Error(), "cards: GET fallback/cards.json: TLS handshake timeout") {
		t.Fatalf("missing detailed skipped error: %v", err)
	}
}
