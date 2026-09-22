package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
)

type legacyAPIHarness struct {
	server *httptest.Server
	api    *Server
	db     *db.DB
	store  *store.Store
	events *store.EventStore
	token  string
}

func setupLegacyAPI(t *testing.T) *legacyAPIHarness {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "legacy-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	s := store.New(database)
	es := store.NewEventStore(database)
	a := auth.New(database, "legacy-contract-secret-at-least-32-bytes", time.Hour)
	cfg, err := config.New(database, "legacy-contract-master-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser("alice", "strong-password-123", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportCategory("cards", model.Category{
		"prefix": {
			"cn-key":      {Text: "官方", Source: model.SourceCN, Ids: []string{"1"}},
			"human-key":   {Text: "人工", Source: model.SourceHuman, Ids: []string{"2"}},
			"pinned-key":  {Text: "锁定", Source: model.SourcePinned},
			"llm-key":     {Text: "机器", Source: model.SourceLLM},
			"unknown-key": {Text: "", Source: model.SourceUnknown},
			"custom-key":  {Text: "自定义", Source: "legacy-custom"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := es.ImportOrdered(42, model.EventStoryMeta{
		Source: "official_cn", Version: "1.0", LastUpdated: 1700000000,
	}, []store.OrderedEpisode{{
		EpisodeNo:   "1",
		ScenarioID:  "scenario-1",
		Title:       "标题",
		TitleSource: model.SourceCN,
		TalkKeys:    []string{"二", "一 & <"},
		TalkData: map[string]string{
			"二":     "第二句",
			"一 & <": "第一句 & <",
		},
		TalkSources: map[string]string{
			"二":     model.SourceHuman,
			"一 & <": model.SourceCN,
		},
		SpeakerNames: map[string]string{"二": "角色"},
	}}); err != nil {
		t.Fatal(err)
	}

	hub := sse.NewHub()
	srv := NewServer(s, es, a, cfg, hub, translator.New(s, es, cfg), nil, nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/auth/login", "", map[string]string{
		"username": "alice", "password": "strong-password-123",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	if login.Token == "" {
		t.Fatal("login returned an empty token")
	}
	return &legacyAPIHarness{server: ts, api: srv, db: database, store: s, events: es, token: login.Token}
}

func TestLegacyReadAPIGolden(t *testing.T) {
	h := setupLegacyAPI(t)
	tests := []struct {
		name    string
		path    string
		fixture string
	}{
		{"categories", "/api/categories", "categories.json"},
		{"entries source filter", "/api/entries?category=cards&field=prefix&source=human", "entries-human.json"},
		{"entries empty array", "/api/entries?category=cards&field=missing", "entries-empty.json"},
		{"event summaries", "/api/event-stories", "event-stories.json"},
		{"event detail", "/api/event-story?eventId=42", "event-story.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := authorizedRequest(t, h, http.MethodGet, tt.path, nil)
			defer resp.Body.Close()
			assertLegacyAPIResponse(t, resp, http.StatusOK, tt.fixture)
		})
	}
}

func TestLegacyEntryUpdateContractGolden(t *testing.T) {
	h := setupLegacyAPI(t)
	changes := 0
	h.store.OnChange(func() { changes++ })

	update := map[string]string{
		"category": "cards", "field": "prefix", "key": "cn-key",
		"text": "人工修订", "source": "human",
	}
	resp := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/entry", update)
	defer resp.Body.Close()
	assertLegacyAPIResponse(t, resp, http.StatusOK, "entry-ok.json")
	if changes != 1 {
		t.Fatalf("change hooks after update = %d, want 1", changes)
	}
	var text, source, ids, updatedBy string
	if err := h.db.QueryRow(`SELECT cn_text, source, ids_json, updated_by FROM entries
		WHERE category='cards' AND field='prefix' AND jp_key='cn-key'`).Scan(&text, &source, &ids, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if text != "人工修订" || source != "human" || ids != `["1"]` || updatedBy != "alice" {
		t.Fatalf("persisted row = text:%q source:%q ids:%q user:%q", text, source, ids, updatedBy)
	}

	noop := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/entry", update)
	defer noop.Body.Close()
	assertLegacyAPIResponse(t, noop, http.StatusOK, "entry-noop.json")
	if changes != 1 {
		t.Fatalf("noop fired a change hook; count = %d", changes)
	}

	// Stale source identities are rejected instead of creating a row in the
	// currently selected destination field.
	insert := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
		"category": "cards", "field": "legacy-field", "key": "new-key",
		"text": "legacy text", "source": "human",
	})
	defer insert.Body.Close()
	if insert.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(insert.Body)
		t.Fatalf("stale insert status = %d: %s", insert.StatusCode, body)
	}
	var inserted int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM entries WHERE category='cards' AND field='legacy-field' AND jp_key='new-key'`).Scan(&inserted); err != nil || inserted != 0 {
		t.Fatalf("stale insert count=%d err=%v", inserted, err)
	}

	bad := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
		"category": "eventStory", "field": "title", "key": "x",
	})
	defer bad.Body.Close()
	assertLegacyAPIResponse(t, bad, http.StatusBadRequest, "entry-error.json")
}

func TestLegacyEventUpdateContractGolden(t *testing.T) {
	h := setupLegacyAPI(t)
	resp := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二",
		"cnText": "第二句校对", "source": "", "entryType": "talk",
	})
	defer resp.Body.Close()
	assertLegacyAPIResponse(t, resp, http.StatusOK, "event-update-ok.json")

	detail, err := h.events.Detail(42)
	if err != nil {
		t.Fatal(err)
	}
	if got := detail.Episodes["1"].TalkData["二"]; got != "第二句校对" {
		t.Fatalf("updated text = %q", got)
	}
	if got := detail.Episodes["1"].TalkSources["二"]; got != model.SourceHuman {
		t.Fatalf("blank source defaulted to %q, want human", got)
	}

	missing := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "missing",
		"cnText": "x", "source": "human", "entryType": "talk",
	})
	defer missing.Body.Close()
	assertLegacyAPIResponse(t, missing, http.StatusNotFound, "event-update-error.json")
}

func TestLegacyAuthSetupLoginRefreshGolden(t *testing.T) {
	ts := setupEmpty(t)

	status := mustGet(t, ts.URL+"/api/auth/setup-status")
	defer status.Body.Close()
	assertLegacyAPIResponse(t, status, http.StatusOK, "auth-setup-status.json")

	setupResp := doJSON(t, http.MethodPost, ts.URL+"/api/auth/setup", "", map[string]string{
		"username": "root", "password": "strong-password-123",
	})
	setupToken := normalizeAuthResponse(t, setupResp, "auth-setup.json")

	meReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+setupToken)
	meResp, err := http.DefaultClient.Do(meReq)
	if err != nil {
		t.Fatal(err)
	}
	defer meResp.Body.Close()
	assertLegacyAPIResponse(t, meResp, http.StatusOK, "auth-me.json")

	loginResp := doJSON(t, http.MethodPost, ts.URL+"/api/auth/login", "", map[string]string{
		"username": "root", "password": "strong-password-123",
	})
	loginToken := normalizeAuthResponse(t, loginResp, "auth-login.json")

	refreshResp := doJSON(t, http.MethodPost, ts.URL+"/api/auth/refresh", loginToken, nil)
	normalizeAuthResponse(t, refreshResp, "auth-refresh.json")
}

func normalizeAuthResponse(t *testing.T, resp *http.Response, fixture string) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth status = %d: %s", resp.StatusCode, body)
	}
	assertNoStoreJSONHeaders(t, resp)
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	token, ok := body["token"].(string)
	if !ok || token == "" {
		t.Fatalf("auth response token = %#v", body["token"])
	}
	if _, ok := body["expiresAt"].(float64); !ok {
		t.Fatalf("auth response expiresAt = %#v", body["expiresAt"])
	}
	body["token"] = "<jwt>"
	body["expiresAt"] = float64(0)
	assertGoldenJSONValue(t, body, fixture)
	return token
}

// authorizedRequest sends the producer-state proof the v1 routes require, taken
// from the gate status a freshly loaded client would have seen.
func authorizedRequest(t *testing.T, h *legacyAPIHarness, method, path string, body any) *http.Response {
	t.Helper()
	if strings.HasPrefix(path, "/api/editor/v1/") {
		return strictRequest(t, h, method, path, body, []string{loadedState(h.api.editorGate.Status())})
	}
	return doJSON(t, method, h.server.URL+path, h.token, body)
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func assertLegacyAPIResponse(t *testing.T, resp *http.Response, status int, fixture string) {
	t.Helper()
	if resp.StatusCode != status {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, body)
	}
	assertNoStoreJSONHeaders(t, resp)
	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	assertGoldenJSONValue(t, body, fixture)
}

func assertNoStoreJSONHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func assertGoldenJSONValue(t *testing.T, got any, fixture string) {
	t.Helper()
	wantBytes, err := os.ReadFile(filepath.Join("testdata", "legacy", fixture))
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("legacy fixture %s mismatch\ngot:\n%s\nwant:\n%s", fixture, gotJSON, wantBytes)
	}
}
