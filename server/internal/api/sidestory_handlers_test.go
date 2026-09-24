package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
)

var sideStoryAPINow = time.Unix(1790000000, 0)

type fakeSideStoryRunner struct {
	mu            sync.Mutex
	state         store.SideStoryBackfillState
	triggerResult bool
	triggers      []bool
	scripts       map[string]string // "kind/id/episode" -> canonical JSON
	scriptErr     error
	refresh       []store.SideStoryEpisodeApply
	refreshErr    error
	ai            store.SideStoryAIResult
	aiErr         error
	aiCalls       []string
}

func (f *fakeSideStoryRunner) SideStoryBackfillState() store.SideStoryBackfillState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeSideStoryRunner) TriggerSideStoryBackfill(refreshCatalog bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers = append(f.triggers, refreshCatalog)
	return f.triggerResult
}

func (f *fakeSideStoryRunner) RefreshSideStoryContext(_ context.Context, _, _ string) ([]store.SideStoryEpisodeApply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refresh, f.refreshErr
}

func (f *fakeSideStoryRunner) AITranslateSideStoryContext(_ context.Context, kind, storyID, locale, episodeKey, provider string) (store.SideStoryAIResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aiCalls = append(f.aiCalls, strings.Join([]string{kind, storyID, locale, episodeKey, provider}, "|"))
	return f.ai, f.aiErr
}

func (f *fakeSideStoryRunner) FetchSideStoryJPScriptContext(_ context.Context, kind, storyID, episodeKey string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scriptErr != nil {
		return "", "", f.scriptErr
	}
	canonical, ok := f.scripts[kind+"/"+storyID+"/"+episodeKey]
	if !ok {
		return "", "", sql.ErrNoRows
	}
	sum := sha256.Sum256([]byte(canonical))
	return canonical, hex.EncodeToString(sum[:]), nil
}

// sideStoryFileRecorder records the incremental side-story rebuilds a handler
// requests.
type sideStoryFileRecorder struct {
	*recordingFileService
	mu       sync.Mutex
	rebuilds []string
}

func (f *sideStoryFileRecorder) RebuildSideStory(kind, storyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuilds = append(f.rebuilds, kind+"/"+storyID)
	return nil
}

func (f *sideStoryFileRecorder) taken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.rebuilds
	f.rebuilds = nil
	return out
}

type sideStoryAPIHarness struct {
	*legacyAPIHarness
	runner      *fakeSideStoryRunner
	files       *sideStoryFileRecorder
	changes     atomic.Int64
	editorToken string
	cardScript  store.SideStoryScript
}

type sideStoryAPITalk struct {
	speaker, body string
	close         int
	voice         bool
}

// sideStoryAPIScenario builds a decoded Unity scenario whose snippets show every
// talk in order, then one scene effect.
func sideStoryAPIScenario(t *testing.T, scenarioID string, talks ...sideStoryAPITalk) any {
	t.Helper()
	talkData, snippets := []any{}, []any{}
	for index, talk := range talks {
		voices := []any{}
		if talk.voice {
			voices = append(voices, map[string]any{"VoiceId": "test_voice_" + scenarioID, "Volume": 1, "Character2dId": 12})
		}
		talkData = append(talkData, map[string]any{
			"WindowDisplayName": talk.speaker, "Body": talk.body, "Voices": voices, "WhenFinishCloseWindow": talk.close,
		})
		snippets = append(snippets, map[string]any{"Action": 1, "ReferenceIndex": index})
	}
	snippets = append(snippets, map[string]any{"Action": 6, "ReferenceIndex": 0})
	encoded, err := json.Marshal(map[string]any{
		"ScenarioId": scenarioID, "Snippets": snippets, "TalkData": talkData,
		"SpecialEffectData": []any{map[string]any{"EffectType": 8, "StringVal": "テスト場所"}},
		"AppearCharacters":  []any{map[string]any{"Character2dId": 12, "CostumeType": "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func applySideStoryAPIScript(t *testing.T, h *legacyAPIHarness, kind, id, episode string, value any, scenarioID string) store.SideStoryScript {
	t.Helper()
	script, err := store.ParseSideStoryScript(value, scenarioID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ApplySideStoryFetchesContext(context.Background(), []store.SideStoryEpisodeFetch{{
		Kind: kind, StoryID: id, EpisodeKey: episode, JP: store.SideStoryFetchOutcome{Attempted: true, Script: &script},
	}}, sideStoryAPINow); err != nil {
		t.Fatal(err)
	}
	return script
}

// setupSideStoryAPI seeds test card 501 (episode 1 fetched, 2 not), test card
// 502 (nothing fetched) and one fetched test area talk.
func setupSideStoryAPI(t *testing.T) *sideStoryAPIHarness {
	t.Helper()
	legacy := setupLegacyAPI(t)
	ctx := context.Background()
	cardEpisode := func(key string) store.SideStoryCatalogEpisode {
		scenario := "test_card_501_0" + key
		return store.SideStoryCatalogEpisode{Key: key, ScenarioID: scenario, TitleJP: "テスト話" + key, Position: 1,
			JPAssetPath: "character/member/test_card_501/" + scenario}
	}
	if _, err := legacy.store.SyncSideStoryCatalogContext(ctx, store.SideStoryKindCard, []store.SideStoryCatalogStory{
		{Kind: "card", StoryID: "501", Title: "テストカード", CharacterID: 1, ReleasedAt: 1790000000000,
			Episodes: []store.SideStoryCatalogEpisode{cardEpisode("1"), cardEpisode("2")}},
		{Kind: "card", StoryID: "502", Title: "テストカード二", CharacterID: 2, ReleasedAt: 1790000001000,
			Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: "test_card_502_01", Position: 1, JPAssetPath: "character/member/test_card_502/test_card_502_01"}}},
	}, sideStoryAPINow); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.store.SyncSideStoryCatalogContext(ctx, store.SideStoryKindArea, []store.SideStoryCatalogStory{
		{Kind: "area", StoryID: "areatalk_test_01", Title: "テストエリア", AreaID: 5, AreaCategory: "grade1", ActionSetID: 1234,
			Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: "areatalk_test_01", JPAssetPath: "scenario/actionset/group12/areatalk_test_01"}}},
	}, sideStoryAPINow); err != nil {
		t.Fatal(err)
	}
	cardValue := sideStoryAPIScenario(t, "test_card_501_01",
		sideStoryAPITalk{speaker: "テスト話者甲", body: "テスト台詞一", voice: true},
		sideStoryAPITalk{speaker: "テスト話者乙_制服", body: "  テスト台詞二  ", close: 1},
		sideStoryAPITalk{speaker: "テスト話者甲", body: "テスト台詞一"},
		sideStoryAPITalk{body: "テスト台詞三"})
	cardScript := applySideStoryAPIScript(t, legacy, "card", "501", "1", cardValue, "test_card_501_01")
	areaValue := sideStoryAPIScenario(t, "areatalk_test_01", sideStoryAPITalk{speaker: "テスト話者甲", body: "テスト区域台詞"})
	areaScript := applySideStoryAPIScript(t, legacy, "area", "areatalk_test_01", "1", areaValue, "areatalk_test_01")

	editor, err := legacy.api.auth.CreateUser("story-editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	editorToken, _, err := legacy.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	h := &sideStoryAPIHarness{
		legacyAPIHarness: legacy, editorToken: editorToken, cardScript: cardScript,
		files: &sideStoryFileRecorder{recordingFileService: &recordingFileService{}},
		runner: &fakeSideStoryRunner{
			state: store.SideStoryBackfillState{Enabled: true}, triggerResult: true,
			scripts: map[string]string{
				"card/501/1": cardScript.CanonicalJSON, "area/areatalk_test_01/1": areaScript.CanonicalJSON,
			},
		},
	}
	legacy.api.SetSideStoryRunner(h.runner)
	legacy.api.SetFileService(h.files)
	legacy.store.OnChange(func() { h.changes.Add(1) })
	return h
}

// call sends a request as token with the current producer-state proof and
// decodes the JSON answer into out when it is non-nil.
func (h *sideStoryAPIHarness) call(t *testing.T, method, path, token string, body, out any) int {
	t.Helper()
	response := strictAs(t, h.legacyAPIHarness, method, path, token, body)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s status=%d body=%s: %v", method, path, response.StatusCode, data, err)
		}
	}
	return response.StatusCode
}

type sideStoryAPIError struct {
	Error   string          `json:"error"`
	Details []string        `json:"details"`
	Current json.RawMessage `json:"current"`
}

func (h *sideStoryAPIHarness) expectError(t *testing.T, method, path, token string, body any, status int, code string) sideStoryAPIError {
	t.Helper()
	var decoded sideStoryAPIError
	if got := h.call(t, method, path, token, body, &decoded); got != status || decoded.Error != code {
		t.Fatalf("%s %s = %d %+v, want %d %s", method, path, got, decoded, status, code)
	}
	return decoded
}

// sideStoryEvents streams sidestory.updated payloads; it returns once the
// stream is registered with the hub.
func sideStoryEvents(t *testing.T, h *legacyAPIHarness) <-chan map[string]any {
	t.Helper()
	response, err := http.DefaultClient.Do(bearerSSERequest(t, h.server.URL, h.token))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	events := make(chan map[string]any, 16)
	ready := make(chan struct{})
	go func() {
		reader := bufio.NewReader(response.Body)
		var event string
		registered := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
				if !registered {
					registered = true
					close(ready)
				}
				continue
			}
			if event == "sidestory.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					events <- data
				}
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream did not open")
	}
	return events
}

func nextSideStoryEvent(t *testing.T, events <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sidestory.updated")
		return nil
	}
}

func TestSideStoryRoutesSeparateEditorsFromAdmins(t *testing.T) {
	h := setupSideStoryAPI(t)
	edit := map[string]any{"lines": []map[string]any{{"jp": "テスト台詞一", "text": "测试台词一"}}}
	for _, route := range []struct {
		method, path string
		body         any
		editorOK     bool
	}{
		{http.MethodGet, "/api/editor/v1/stories?kind=card", nil, true},
		{http.MethodGet, "/api/editor/v1/stories/sync", nil, true},
		{http.MethodPost, "/api/editor/v1/stories/sync", map[string]any{"refreshCatalog": false}, false},
		{http.MethodGet, "/api/editor/v1/story/card/501", nil, true},
		{http.MethodPut, "/api/editor/v1/story/card/501/1", edit, true},
		{http.MethodPost, "/api/editor/v1/story/card/501/ai", map[string]any{}, false},
		{http.MethodPost, "/api/editor/v1/story/card/501/refresh", map[string]any{}, false},
		{http.MethodGet, "/api/editor/v1/story/card/501/1/snapshot?locale=zh-CN", nil, true},
	} {
		anonymous := doJSON(t, route.method, h.server.URL+route.path, "", route.body)
		anonymous.Body.Close()
		if anonymous.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", route.method, route.path, anonymous.StatusCode)
		}
		status := h.call(t, route.method, route.path, h.editorToken, route.body, nil)
		if route.editorOK && status != http.StatusOK || !route.editorOK && status != http.StatusForbidden {
			t.Fatalf("editor %s %s = %d, editor allowed %v", route.method, route.path, status, route.editorOK)
		}
		if admin := h.call(t, route.method, route.path, h.token, route.body, nil); admin/100 != 2 {
			t.Fatalf("admin %s %s = %d", route.method, route.path, admin)
		}
	}
}

func TestSideStoryRoutesWithoutARunnerAnswerUnavailable(t *testing.T) {
	h := setupSideStoryAPI(t)
	h.api.SetSideStoryRunner(nil)
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/editor/v1/stories/sync", nil},
		{http.MethodPost, "/api/editor/v1/stories/sync", map[string]any{"refreshCatalog": true}},
		{http.MethodPost, "/api/editor/v1/story/card/501/ai", map[string]any{"locale": "zh-CN"}},
		{http.MethodPost, "/api/editor/v1/story/card/501/refresh", map[string]any{}},
		{http.MethodGet, "/api/editor/v1/story/card/501/1/snapshot?locale=zh-CN", nil},
	} {
		h.expectError(t, route.method, route.path, h.token, route.body, http.StatusServiceUnavailable, "side_story_unavailable")
	}
	var list sideStoryListResponse
	if status := h.call(t, http.MethodGet, "/api/editor/v1/stories?kind=card", h.token, nil, &list); status != http.StatusOK || len(list.Stories) != 2 {
		t.Fatalf("list without runner = %d %+v", status, list)
	}
}

func TestSideStoryListFiltersByStatusAndValidatesQuery(t *testing.T) {
	h := setupSideStoryAPI(t)
	var list sideStoryListResponse
	if status := h.call(t, http.MethodGet, "/api/editor/v1/stories?kind=card", h.token, nil, &list); status != http.StatusOK ||
		list.Kind != "card" || list.Locale != "zh-CN" || len(list.Stories) != 2 || list.Stories[0].ID != "502" || list.Stories[1].Status != "untranslated" {
		t.Fatalf("card list = %d %+v", status, list)
	}
	if status := h.call(t, http.MethodGet, "/api/editor/v1/stories?kind=card&status=pending&locale=en-US", h.token, nil, &list); status != http.StatusOK ||
		list.Locale != "en-US" || len(list.Stories) != 1 || list.Stories[0].ID != "502" {
		t.Fatalf("pending list = %d %+v", status, list)
	}
	var raw map[string]json.RawMessage
	if status := h.call(t, http.MethodGet, "/api/editor/v1/stories?kind=area&status=translated", h.token, nil, &raw); status != http.StatusOK ||
		string(raw["stories"]) != "[]" {
		t.Fatalf("empty filtered list = %d %s", status, raw["stories"])
	}
	for _, query := range []string{"", "kind=event", "kind=card&locale=ja-JP", "kind=card&locale=fr-FR", "kind=card&status=done"} {
		h.expectError(t, http.MethodGet, "/api/editor/v1/stories?"+query, h.token, nil, http.StatusBadRequest, "invalid_request")
	}
}

func TestSideStoryDetailValidatesIdentityAndLocale(t *testing.T) {
	h := setupSideStoryAPI(t)
	var detail store.SideStoryDetail
	if status := h.call(t, http.MethodGet, "/api/editor/v1/story/area/areatalk_test_01?locale=en-US", h.token, nil, &detail); status != http.StatusOK ||
		detail.Locale != "en-US" || detail.ActionSetID != 1234 || len(detail.Episodes) != 1 || detail.Episodes[0].Lines[0].JP != "テスト区域台詞" {
		t.Fatalf("area detail = %d %+v", status, detail)
	}
	if status := h.call(t, http.MethodGet, "/api/editor/v1/story/card/501", h.token, nil, &detail); status != http.StatusOK ||
		detail.Locale != "zh-CN" || len(detail.Episodes) != 2 || !detail.Episodes[0].Fetched || detail.Episodes[1].Fetched {
		t.Fatalf("card detail = %d %+v", status, detail)
	}
	h.expectError(t, http.MethodGet, "/api/editor/v1/story/card/999", h.token, nil, http.StatusNotFound, "not_found")
	h.expectError(t, http.MethodGet, "/api/editor/v1/story/area/areatalk_missing", h.token, nil, http.StatusNotFound, "not_found")
	for _, path := range []string{
		"/api/editor/v1/story/event/501", "/api/editor/v1/story/card/0501", "/api/editor/v1/story/card/1234567890",
		"/api/editor/v1/story/card/test_card_501_01", "/api/editor/v1/story/area/-areatalk", "/api/editor/v1/story/area/area%20talk",
		"/api/editor/v1/story/card/501?locale=ja-JP", "/api/editor/v1/story/card/501?locale=zh-TW",
	} {
		h.expectError(t, http.MethodGet, path, h.token, nil, http.StatusBadRequest, "invalid_request")
	}
}

func TestSideStoryUpdateWritesPublishesAndReportsConflicts(t *testing.T) {
	h := setupSideStoryAPI(t)
	events := sideStoryEvents(t, h.legacyAPIHarness)
	path := "/api/editor/v1/story/card/501/1"
	zero := 0
	var updated sideStoryUpdateResponse
	if status := h.call(t, http.MethodPut, path, h.editorToken, map[string]any{
		"lines":    []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试台词一", ExpectedRevision: &zero}},
		"clientId": "test-client",
	}, &updated); status != http.StatusOK || updated.Status != "ok" || updated.Kind != "card" || updated.ID != "501" ||
		updated.Episode != "1" || updated.Locale != "zh-CN" || updated.Updated != 1 || updated.Unchanged != 0 ||
		len(updated.Lines) != 1 || updated.Lines[0].Revision != 1 || updated.Lines[0].Source != "human" || updated.Lines[0].UpdatedBy != "story-editor" {
		t.Fatalf("update = %d %+v", status, updated)
	}
	event := nextSideStoryEvent(t, events)
	lines, _ := event["lines"].([]any)
	first, _ := lines[0].(map[string]any)
	if event["kind"] != "card" || event["id"] != "501" || event["episode"] != "1" || event["locale"] != "zh-CN" ||
		event["action"] != "update" || event["user"] != "story-editor" || event["clientId"] != "test-client" ||
		len(lines) != 1 || first["jp"] != "テスト台詞一" || first["text"] != "测试台词一" || first["revision"] != float64(1) {
		t.Fatalf("update event = %#v", event)
	}
	if rebuilds := h.files.taken(); !reflect.DeepEqual(rebuilds, []string{"card/501"}) || h.changes.Load() != 1 {
		t.Fatalf("update rebuilds=%v changes=%d", rebuilds, h.changes.Load())
	}

	if status := h.call(t, http.MethodPut, path, h.token, map[string]any{
		"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试台词一"}},
	}, &updated); status != http.StatusOK || updated.Updated != 0 || updated.Unchanged != 1 {
		t.Fatalf("unchanged update = %d %+v", status, updated)
	}
	if rebuilds := h.files.taken(); len(rebuilds) != 0 || h.changes.Load() != 1 {
		t.Fatalf("unchanged update rebuilds=%v changes=%d", rebuilds, h.changes.Load())
	}
	if status := h.call(t, http.MethodPut, path, h.token, map[string]any{
		"locale": "en-US", "lines": []store.SideStoryLineEdit{{JP: "テスト話者甲", Text: "Test speaker", Source: "llm"}},
	}, &updated); status != http.StatusOK || updated.Locale != "en-US" || updated.Updated != 1 || updated.Lines[0].Source != "llm" {
		t.Fatalf("en-US update = %d %+v", status, updated)
	}
	if event := nextSideStoryEvent(t, events); event["locale"] != "en-US" || event["user"] != "alice" {
		t.Fatalf("the unchanged edit was broadcast, or the en-US one was not: %#v", event)
	}

	stale := h.expectError(t, http.MethodPut, path, h.token, map[string]any{
		"lines": []store.SideStoryLineEdit{
			{JP: "テスト台詞一", Text: "测试台词一改", ExpectedRevision: &zero},
			{JP: "テスト台詞二", Text: "测试台词二", ExpectedRevision: &zero},
		},
	}, http.StatusConflict, "revision_conflict")
	var conflicts struct {
		Conflicts []store.SideStoryLineConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(stale.Current, &conflicts); err != nil || !reflect.DeepEqual(conflicts.Conflicts, []store.SideStoryLineConflict{{
		JP: "テスト台詞一", ExpectedRevision: 0, CurrentRevision: 1, CurrentText: "测试台词一", CurrentSource: "human",
	}}) {
		t.Fatalf("conflict current = %s err=%v", stale.Current, err)
	}
	unknown := h.expectError(t, http.MethodPut, path, h.token, map[string]any{
		"lines": []store.SideStoryLineEdit{{JP: "テスト台詞二", Text: "测试台词二"}, {JP: "存在しない台詞", Text: "测试"}},
	}, http.StatusUnprocessableEntity, "unknown_lines")
	if string(unknown.Current) != `{"lines":["存在しない台詞"]}` {
		t.Fatalf("unknown current = %s", unknown.Current)
	}
	var detail store.SideStoryDetail
	h.call(t, http.MethodGet, "/api/editor/v1/story/card/501", h.token, nil, &detail)
	for _, line := range detail.Episodes[0].Lines {
		if line.JP == "テスト台詞二" && line.Revision != 0 {
			t.Fatalf("a rejected batch wrote %+v", line)
		}
	}

	h.expectError(t, http.MethodPut, "/api/editor/v1/story/card/999/1", h.token, map[string]any{
		"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试"}},
	}, http.StatusNotFound, "not_found")
	valid := map[string]any{"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试"}}}
	for _, invalid := range []struct {
		path string
		body any
	}{
		{"/api/editor/v1/story/card/501/3", valid},
		{"/api/editor/v1/story/area/areatalk_test_01/2", valid},
		{"/api/editor/v1/story/card/abc/1", valid},
		{path, map[string]any{"locale": "ja-JP", "lines": valid["lines"]}},
		{path, map[string]any{"lines": []store.SideStoryLineEdit{}}},
		{path, map[string]any{"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试\x00"}}}},
		{path, map[string]any{"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试", Source: "official"}}}},
		{path, map[string]any{"lines": valid["lines"], "clientId": strings.Repeat("c", 129)}},
	} {
		h.expectError(t, http.MethodPut, invalid.path, h.token, invalid.body, http.StatusBadRequest, "invalid_request")
	}
	if rebuilds := h.files.taken(); len(rebuilds) != 1 {
		t.Fatalf("rejected updates rebuilt %v", rebuilds)
	}
}

func TestSideStoryUpdateIsAStrictContentMutation(t *testing.T) {
	h := setupSideStoryAPI(t)
	body := map[string]any{"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试台词一"}}}
	malformed := strictRequest(t, h.legacyAPIHarness, http.MethodPut, "/api/editor/v1/story/card/501/1", body, []string{"not-a-producer-state"})
	malformed.Body.Close()
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed producer state = %d", malformed.StatusCode)
	}
	restarted := strictRequest(t, h.legacyAPIHarness, http.MethodPut, "/api/editor/v1/story/card/501/1", body,
		[]string{"ZGlmZmVyZW50LXByb2Nlc3M:0:0"})
	restarted.Body.Close()
	if restarted.StatusCode != http.StatusConflict {
		t.Fatalf("stale producer state = %d", restarted.StatusCode)
	}
	release, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lenient := doJSON(t, http.MethodPut, h.server.URL+"/api/editor/v1/story/card/501/1", h.token, body)
	lenient.Body.Close()
	if lenient.StatusCode != http.StatusConflict {
		t.Fatalf("update during a producer job = %d", lenient.StatusCode)
	}
}

func TestSideStoryBackfillRoutes(t *testing.T) {
	h := setupSideStoryAPI(t)
	h.runner.state = store.SideStoryBackfillState{Enabled: true, LastRoundAt: "2026-09-24T00:00:00Z", LastRound: store.SideStoryRoundSummary{Fetched: 3}}
	var status struct {
		State  store.SideStoryBackfillState           `json:"state"`
		Totals map[string]store.SideStoryKindProgress `json:"totals"`
	}
	if code := h.call(t, http.MethodGet, "/api/editor/v1/stories/sync", h.editorToken, nil, &status); code != http.StatusOK ||
		!reflect.DeepEqual(status.State, h.runner.state) || status.Totals["card"].Stories != 2 || status.Totals["card"].Fetched != 1 ||
		status.Totals["area"].Episodes != 1 {
		t.Fatalf("sync status = %d %+v", code, status)
	}
	var started struct {
		Started bool                         `json:"started"`
		State   store.SideStoryBackfillState `json:"state"`
	}
	if code := h.call(t, http.MethodPost, "/api/editor/v1/stories/sync", h.token, map[string]any{"refreshCatalog": true}, &started); code != http.StatusAccepted ||
		!started.Started || !started.State.Enabled {
		t.Fatalf("trigger = %d %+v", code, started)
	}
	h.runner.triggerResult = false
	h.runner.state.Running = true
	if code := h.call(t, http.MethodPost, "/api/editor/v1/stories/sync", h.token, map[string]any{}, &started); code != http.StatusAccepted ||
		started.Started || !started.State.Running {
		t.Fatalf("trigger while running = %d %+v", code, started)
	}
	h.runner.state = store.SideStoryBackfillState{}
	h.expectError(t, http.MethodPost, "/api/editor/v1/stories/sync", h.token, map[string]any{}, http.StatusConflict, "backfill_disabled")
	if !reflect.DeepEqual(h.runner.triggers, []bool{true, false, false}) {
		t.Fatalf("triggers = %v", h.runner.triggers)
	}
}

func TestSideStoryAITranslatesThroughTheRunner(t *testing.T) {
	h := setupSideStoryAPI(t)
	events := sideStoryEvents(t, h.legacyAPIHarness)
	h.runner.ai = store.SideStoryAIResult{Translated: 4, Remaining: 1}
	var result store.SideStoryAIResult
	if code := h.call(t, http.MethodPost, "/api/editor/v1/story/area/areatalk_test_01/ai", h.token,
		map[string]any{"locale": "en-US", "episode": "1", "provider": "test-provider", "clientId": "test-client"}, &result); code != http.StatusOK ||
		result != h.runner.ai {
		t.Fatalf("ai = %d %+v", code, result)
	}
	if code := h.call(t, http.MethodPost, "/api/editor/v1/story/card/501/ai", h.token, map[string]any{}, &result); code != http.StatusOK {
		t.Fatalf("default ai = %d", code)
	}
	if !reflect.DeepEqual(h.runner.aiCalls, []string{"area|areatalk_test_01|en-US|1|test-provider", "card|501|zh-CN||"}) {
		t.Fatalf("ai calls = %v", h.runner.aiCalls)
	}
	event := nextSideStoryEvent(t, events)
	if _, hasLines := event["lines"]; hasLines || !reflect.DeepEqual(event, map[string]any{
		"kind": "area", "id": "areatalk_test_01", "episode": "1", "locale": "en-US", "action": "ai", "user": "alice", "clientId": "test-client",
	}) {
		t.Fatalf("ai event = %#v", event)
	}
	if rebuilds := h.files.taken(); !reflect.DeepEqual(rebuilds, []string{"area/areatalk_test_01", "card/501"}) || h.changes.Load() != 2 {
		t.Fatalf("ai rebuilds=%v changes=%d", rebuilds, h.changes.Load())
	}

	h.runner.ai = store.SideStoryAIResult{}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{translator.ErrRunning, http.StatusConflict, "already_running"},
		{editorgate.ErrProducerRunning, http.StatusConflict, "already_running"},
		{editorgate.ErrDraining, http.StatusServiceUnavailable, "draining"},
		{sql.ErrNoRows, http.StatusNotFound, "not_found"},
		{fmt.Errorf("test provider failed"), http.StatusInternalServerError, "internal_error"},
	} {
		h.runner.aiErr = failure.err
		h.expectError(t, http.MethodPost, "/api/editor/v1/story/card/501/ai", h.token, map[string]any{}, failure.status, failure.code)
	}
	h.runner.aiErr = nil
	for _, body := range []map[string]any{{"locale": "ja-JP"}, {"episode": "3"}, {"clientId": strings.Repeat("c", 129)}} {
		h.expectError(t, http.MethodPost, "/api/editor/v1/story/card/501/ai", h.token, body, http.StatusBadRequest, "invalid_request")
	}
	if rebuilds := h.files.taken(); len(rebuilds) != 0 {
		t.Fatalf("failed ai requests rebuilt %v", rebuilds)
	}
}

// A run that fails after committing batches publishes them; a run that saved
// nothing publishes nothing.
func TestSideStoryAIPublishesExactlyWhenItSavedLines(t *testing.T) {
	h := setupSideStoryAPI(t)
	events := sideStoryEvents(t, h.legacyAPIHarness)
	h.runner.ai = store.SideStoryAIResult{Remaining: 3}
	var result store.SideStoryAIResult
	if code := h.call(t, http.MethodPost, "/api/editor/v1/story/area/areatalk_test_01/ai", h.token,
		map[string]any{"clientId": "test-client"}, &result); code != http.StatusOK || result != h.runner.ai {
		t.Fatalf("empty ai = %d %+v", code, result)
	}
	if rebuilds := h.files.taken(); len(rebuilds) != 0 || h.changes.Load() != 0 {
		t.Fatalf("empty ai rebuilds=%v changes=%d", rebuilds, h.changes.Load())
	}

	h.runner.ai = store.SideStoryAIResult{Translated: 2, Remaining: 1}
	h.runner.aiErr = errors.New("card 501 batch 2/2 failed after saving 2/3: test provider failed")
	failed := h.expectError(t, http.MethodPost, "/api/editor/v1/story/card/501/ai", h.token,
		map[string]any{"clientId": "test-client"}, http.StatusInternalServerError, "internal_error")
	if want := []string{h.runner.aiErr.Error(), "translated lines saved before the failure: 2"}; !reflect.DeepEqual(failed.Details, want) {
		t.Fatalf("failed ai details = %q, want %q", failed.Details, want)
	}
	// The empty run broadcast nothing, so the first event is the failed run's.
	if event := nextSideStoryEvent(t, events); !reflect.DeepEqual(event, map[string]any{
		"kind": "card", "id": "501", "episode": "", "locale": "zh-CN", "action": "ai", "user": "alice", "clientId": "test-client",
	}) {
		t.Fatalf("failed ai event = %#v", event)
	}
	if rebuilds := h.files.taken(); !reflect.DeepEqual(rebuilds, []string{"card/501"}) || h.changes.Load() != 1 {
		t.Fatalf("failed ai rebuilds=%v changes=%d", rebuilds, h.changes.Load())
	}
}

func TestSideStoryRefreshReturnsTheRunnerApplies(t *testing.T) {
	h := setupSideStoryAPI(t)
	events := sideStoryEvents(t, h.legacyAPIHarness)
	h.runner.refresh = []store.SideStoryEpisodeApply{{Kind: "card", StoryID: "501", EpisodeKey: "1", Fetched: true, CNState: "absent", ENState: "imported", OfficialWritten: 2}}
	var result struct {
		Episodes []store.SideStoryEpisodeApply `json:"episodes"`
	}
	if code := h.call(t, http.MethodPost, "/api/editor/v1/story/card/501/refresh", h.token, map[string]any{"clientId": "test-client"}, &result); code != http.StatusOK ||
		!reflect.DeepEqual(result.Episodes, h.runner.refresh) {
		t.Fatalf("refresh = %d %+v", code, result)
	}
	if event := nextSideStoryEvent(t, events); !reflect.DeepEqual(event, map[string]any{
		"kind": "card", "id": "501", "episode": "", "locale": "", "action": "refresh", "user": "alice", "clientId": "test-client",
	}) {
		t.Fatalf("refresh event = %#v", event)
	}
	if rebuilds := h.files.taken(); !reflect.DeepEqual(rebuilds, []string{"card/501"}) || h.changes.Load() != 1 {
		t.Fatalf("refresh rebuilds=%v changes=%d", rebuilds, h.changes.Load())
	}
	h.runner.refreshErr = sql.ErrNoRows
	h.expectError(t, http.MethodPost, "/api/editor/v1/story/card/999/refresh", h.token, map[string]any{}, http.StatusNotFound, "not_found")
	h.runner.refreshErr = errors.New("test upstream is down")
	h.expectError(t, http.MethodPost, "/api/editor/v1/story/card/501/refresh", h.token, map[string]any{}, http.StatusBadGateway, "upstream_unavailable")
	h.runner.refreshErr = editorgate.ErrProducerRunning
	h.expectError(t, http.MethodPost, "/api/editor/v1/story/card/501/refresh", h.token, map[string]any{}, http.StatusConflict, "already_running")
	h.expectError(t, http.MethodPost, "/api/editor/v1/story/area/bad%20id/refresh", h.token, map[string]any{}, http.StatusBadRequest, "invalid_request")
}

// A PUT publishes the story's public file at once through the real file
// service, before any debounced full rebuild.
func TestSideStoryUpdatePublishesThePublicFileIncrementally(t *testing.T) {
	h := setupSideStoryAPI(t)
	service := filesvc.New(h.store, h.events, files.NewGenerator(h.store, h.events, ""))
	h.api.SetFileService(service)
	get := func(path string) (int, string) {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response.Code, response.Body.String()
	}
	if code, _ := get("/files/translation/cardStory/card_501.json"); code != http.StatusNotFound {
		t.Fatalf("file before the edit = %d", code)
	}
	if code := h.call(t, http.MethodPut, "/api/editor/v1/story/card/501/1", h.token, map[string]any{
		"lines": []store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试台词一"}},
	}, nil); code != http.StatusOK {
		t.Fatalf("update = %d", code)
	}
	code, body := get("/files/translation/cardStory/card_501.json")
	if code != http.StatusOK || !strings.Contains(body, `"テスト台詞一": "测试台词一"`) || !strings.Contains(body, `"source": "human"`) {
		t.Fatalf("file after the edit = %d %s", code, body)
	}
	if code, _ := get("/files/v2/en-US/translation/cardStory/card_501.json"); code != http.StatusNotFound {
		t.Fatalf("en-US file without an en-US line = %d", code)
	}
}

type consoleSourceTalk struct {
	Speaker       string   `json:"speaker"`
	Text          string   `json:"text"`
	Voices        []string `json:"voices"`
	Volume        []int    `json:"volume"`
	Chara2D       int      `json:"chara2d"`
	TalkDataIndex *int     `json:"talkDataIndex"`
}

type consoleSnapshot struct {
	Kind     string                     `json:"kind"`
	ID       string                     `json:"id"`
	Episode  string                     `json:"episode"`
	Locale   string                     `json:"locale"`
	Revision string                     `json:"revision"`
	Segments []sideStorySnapshotSegment `json:"segments"`
	Scenario struct {
		ScenarioID    string              `json:"scenarioId"`
		FileName      string              `json:"fileName"`
		SHA256        string              `json:"sha256"`
		ParserVersion int                 `json:"parserVersion"`
		RawJSON       string              `json:"rawJson"`
		SourceTalks   []consoleSourceTalk `json:"sourceTalks"`
	} `json:"scenario"`
}

func consoleSplitSpeaker(value string) string { return strings.SplitN(value, "_", 2)[0] }

func consoleInteger(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) {
		return 0, false
	}
	return int(number), true
}

// consoleParseSourceTalks ports parseScenarioSourceTalks from
// web/src/lib/event-txt-import.mjs.
func consoleParseSourceTalks(t *testing.T, rawJSON string) (string, []consoleSourceTalk) {
	t.Helper()
	var raw struct {
		ScenarioID        string           `json:"ScenarioId"`
		Snippets          []map[string]any `json:"Snippets"`
		TalkData          []map[string]any `json:"TalkData"`
		SpecialEffectData []map[string]any `json:"SpecialEffectData"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil || raw.ScenarioID == "" || raw.Snippets == nil || raw.TalkData == nil || raw.SpecialEffectData == nil {
		t.Fatalf("console rejects the scenario JSON structure: %v", err)
	}
	talks := []consoleSourceTalk{}
	for _, snippet := range raw.Snippets {
		action, actionOK := consoleInteger(snippet["Action"])
		reference, referenceOK := consoleInteger(snippet["ReferenceIndex"])
		if !actionOK || !referenceOK || reference < 0 {
			continue
		}
		switch action {
		case 1:
			if reference >= len(raw.TalkData) {
				continue
			}
			data := raw.TalkData[reference]
			speaker, _ := data["WindowDisplayName"].(string)
			text, _ := data["Body"].(string)
			talk := consoleSourceTalk{Speaker: consoleSplitSpeaker(speaker), Text: text, TalkDataIndex: &reference}
			voices, _ := data["Voices"].([]any)
			for index, rawVoice := range voices {
				voice, _ := rawVoice.(map[string]any)
				if id, ok := voice["VoiceId"].(string); ok {
					talk.Voices = append(talk.Voices, id)
				}
				volume, _ := voice["Volume"].(float64)
				talk.Volume = append(talk.Volume, int(volume))
				if character, ok := voice["Character2dId"].(float64); ok && index == 0 {
					talk.Chara2D = int(character)
				}
			}
			talks = append(talks, talk)
			if closeWindow, ok := data["WhenFinishCloseWindow"].(float64); ok && closeWindow != 0 {
				talks = append(talks, consoleSourceTalk{})
			}
		case 6:
			if reference >= len(raw.SpecialEffectData) {
				continue
			}
			effect := raw.SpecialEffectData[reference]
			speaker := map[float64]string{8: "场景", 18: "左上场景", 23: "选项"}[effect["EffectType"].(float64)]
			if speaker == "" {
				continue
			}
			text, _ := effect["StringVal"].(string)
			talks = append(talks, consoleSourceTalk{Speaker: speaker, Text: text}, consoleSourceTalk{})
		}
	}
	if last := len(talks) - 1; last >= 0 && talks[last].Speaker == "" && talks[last].Text == "" {
		talks = talks[:last]
	}
	return raw.ScenarioID, talks
}

// assertConsoleAcceptsSnapshot ports snapshotScenarioState and
// validateEventEpisodeSnapshot from web/src/lib/event-txt-import.mjs, with the
// event identity replaced by kind, id and episode.
func assertConsoleAcceptsSnapshot(t *testing.T, snapshot consoleSnapshot) {
	t.Helper()
	if snapshot.Revision == "" || snapshot.Kind == "" || snapshot.ID == "" || snapshot.Episode == "" {
		t.Fatalf("snapshot identity or revision missing: %+v", snapshot)
	}
	name := snapshot.Scenario.FileName
	if !strings.HasSuffix(strings.ToLower(name), ".json") || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") ||
		strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		t.Fatalf("unsafe scenario file name %q", name)
	}
	if snapshot.Scenario.ParserVersion != 1 || snapshot.Scenario.SourceTalks == nil || snapshot.Segments == nil {
		t.Fatalf("snapshot structure %+v", snapshot.Scenario)
	}
	scenarioID, talks := consoleParseSourceTalks(t, snapshot.Scenario.RawJSON)
	if scenarioID != snapshot.Scenario.ScenarioID || len(talks) != len(snapshot.Scenario.SourceTalks) {
		t.Fatalf("scenario %q has %d talks, snapshot names %q with %d", scenarioID, len(talks), snapshot.Scenario.ScenarioID, len(snapshot.Scenario.SourceTalks))
	}
	for index, talk := range talks {
		provided := snapshot.Scenario.SourceTalks[index]
		if talk.Speaker != provided.Speaker || talk.Text != provided.Text || !reflect.DeepEqual(talk.TalkDataIndex, provided.TalkDataIndex) ||
			talk.Chara2D != provided.Chara2D || len(talk.Voices)+len(provided.Voices) > 0 && !reflect.DeepEqual(talk.Voices, provided.Voices) ||
			len(talk.Volume)+len(provided.Volume) > 0 && !reflect.DeepEqual(talk.Volume, provided.Volume) {
			t.Fatalf("SourceTalk mismatch at %d: parsed %+v provided %+v", index, talk, provided)
		}
	}
	byPosition := map[int]sideStorySnapshotSegment{}
	for _, segment := range snapshot.Segments {
		if segment.Kind == "title" {
			continue
		}
		if _, duplicate := byPosition[segment.Position]; duplicate || segment.Position < 0 {
			t.Fatalf("invalid or duplicate segment position %d", segment.Position)
		}
		byPosition[segment.Position] = segment
	}
	for _, talk := range talks {
		if talk.TalkDataIndex == nil {
			continue
		}
		if body, ok := byPosition[*talk.TalkDataIndex*2]; ok && body.Japanese != talk.Text {
			t.Fatalf("body mismatch at TalkData %d: %q vs %q", *talk.TalkDataIndex, body.Japanese, talk.Text)
		}
		if speaker, ok := byPosition[*talk.TalkDataIndex*2+1]; ok && consoleSplitSpeaker(speaker.Japanese) != talk.Speaker {
			t.Fatalf("speaker mismatch at TalkData %d: %q vs %q", *talk.TalkDataIndex, speaker.Japanese, talk.Speaker)
		}
	}
	expected := strings.TrimPrefix(strings.ToLower(snapshot.Scenario.SHA256), "sha256:")
	sum := sha256.Sum256([]byte(snapshot.Scenario.RawJSON))
	if len(expected) != 64 || hex.EncodeToString(sum[:]) != expected {
		t.Fatalf("scenario SHA-256 %q does not match rawJson", snapshot.Scenario.SHA256)
	}
}

func TestSideStorySnapshotSatisfiesTheConsoleImporter(t *testing.T) {
	h := setupSideStoryAPI(t)
	if code := h.call(t, http.MethodPut, "/api/editor/v1/story/card/501/1", h.token, map[string]any{
		"lines": []store.SideStoryLineEdit{{JP: "テスト台詞二", Text: "测试台词二"}},
	}, nil); code != http.StatusOK {
		t.Fatalf("seed edit = %d", code)
	}
	path := "/api/editor/v1/story/card/501/1/snapshot?locale=zh-CN"
	var snapshot consoleSnapshot
	if code := h.call(t, http.MethodGet, path, h.editorToken, nil, &snapshot); code != http.StatusOK {
		t.Fatalf("snapshot = %d", code)
	}
	assertConsoleAcceptsSnapshot(t, snapshot)
	speakerTalk := func(id string, position int, japanese string) sideStorySnapshotSegment {
		return sideStorySnapshotSegment{ID: id, Kind: "talk", Position: position, Japanese: japanese}
	}
	edited := speakerTalk("テスト台詞二", 2, "  テスト台詞二  ")
	edited.Text, edited.Source, edited.Revision = "测试台词二", "human", 1
	if want := []sideStorySnapshotSegment{
		speakerTalk("テスト台詞一", 0, "テスト台詞一"), speakerTalk("テスト話者甲", 1, "テスト話者甲"),
		edited, speakerTalk("テスト話者乙_制服", 3, "テスト話者乙"),
		speakerTalk("テスト台詞一", 4, "テスト台詞一"), speakerTalk("テスト話者甲", 5, "テスト話者甲"),
		speakerTalk("テスト台詞三", 6, "テスト台詞三"),
	}; !reflect.DeepEqual(snapshot.Segments, want) {
		t.Fatalf("segments\n got %+v\nwant %+v", snapshot.Segments, want)
	}
	if snapshot.Kind != "card" || snapshot.ID != "501" || snapshot.Episode != "1" || snapshot.Locale != "zh-CN" ||
		snapshot.Scenario.ScenarioID != "test_card_501_01" || snapshot.Scenario.FileName != "test_card_501_01.json" ||
		snapshot.Scenario.SHA256 != h.cardScript.SHA256 || snapshot.Scenario.RawJSON != h.cardScript.CanonicalJSON ||
		len(snapshot.Scenario.SourceTalks) != 6 {
		t.Fatalf("snapshot scenario %+v", snapshot)
	}

	// The importer saves one batch keyed by segment id with the snapshot revisions.
	edits := []store.SideStoryLineEdit{}
	for _, segment := range snapshot.Segments {
		if segment.Position == 0 || segment.Position == 2 {
			revision := segment.Revision
			edits = append(edits, store.SideStoryLineEdit{JP: segment.ID, Text: "测试导入" + segment.ID, ExpectedRevision: &revision})
		}
	}
	var updated sideStoryUpdateResponse
	if code := h.call(t, http.MethodPut, "/api/editor/v1/story/card/501/1", h.token, map[string]any{"lines": edits}, &updated); code != http.StatusOK || updated.Updated != 2 {
		t.Fatalf("import batch = %d %+v", code, updated)
	}
	var after consoleSnapshot
	if code := h.call(t, http.MethodGet, path, h.token, nil, &after); code != http.StatusOK || after.Revision == snapshot.Revision {
		t.Fatalf("snapshot revision after an edit = %d %q (before %q)", code, after.Revision, snapshot.Revision)
	}
	var english consoleSnapshot
	if code := h.call(t, http.MethodGet, "/api/editor/v1/story/card/501/1/snapshot?locale=en-US", h.token, nil, &english); code != http.StatusOK ||
		english.Locale != "en-US" || english.Segments[2].Text != "" || english.Segments[2].Revision != 0 {
		t.Fatalf("en-US snapshot = %d %+v", code, english.Segments)
	}

	var area consoleSnapshot
	if code := h.call(t, http.MethodGet, "/api/editor/v1/story/area/areatalk_test_01/1/snapshot?locale=en-US", h.token, nil, &area); code != http.StatusOK ||
		area.ID != "areatalk_test_01" || len(area.Segments) != 2 {
		t.Fatalf("area snapshot = %d %+v", code, area)
	}
	assertConsoleAcceptsSnapshot(t, area)
}

// Some real card scripts carry a leftover label in ScenarioId; the console
// compares scenario.scenarioId with the rawJson's own ScenarioId.
func TestSideStorySnapshotOfALabelledScriptSatisfiesTheConsoleImporter(t *testing.T) {
	h := setupSideStoryAPI(t)
	label := "test_card_501_02 のコピー"
	value := sideStoryAPIScenario(t, label, sideStoryAPITalk{speaker: "テスト話者甲", body: "テスト台詞四"})
	script := applySideStoryAPIScript(t, h.legacyAPIHarness, "card", "501", "2", value, label)
	h.runner.scripts["card/501/2"] = script.CanonicalJSON
	var snapshot consoleSnapshot
	if code := h.call(t, http.MethodGet, "/api/editor/v1/story/card/501/2/snapshot?locale=zh-CN", h.editorToken, nil, &snapshot); code != http.StatusOK {
		t.Fatalf("snapshot = %d", code)
	}
	assertConsoleAcceptsSnapshot(t, snapshot)
	if snapshot.Scenario.ScenarioID != label || snapshot.Scenario.FileName != "test_card_501_02.json" {
		t.Fatalf("snapshot scenario %q file %q", snapshot.Scenario.ScenarioID, snapshot.Scenario.FileName)
	}
}

func TestSideStorySnapshotRejectsMissingOrChangedScripts(t *testing.T) {
	h := setupSideStoryAPI(t)
	h.expectError(t, http.MethodGet, "/api/editor/v1/story/card/501/2/snapshot?locale=zh-CN", h.token, nil, http.StatusConflict, "script_not_fetched")
	h.expectError(t, http.MethodGet, "/api/editor/v1/story/card/999/1/snapshot?locale=zh-CN", h.token, nil, http.StatusNotFound, "not_found")
	for _, path := range []string{
		"/api/editor/v1/story/card/501/1/snapshot", "/api/editor/v1/story/card/501/1/snapshot?locale=ja-JP",
		"/api/editor/v1/story/card/501/3/snapshot?locale=zh-CN", "/api/editor/v1/story/area/areatalk_test_01/2/snapshot?locale=zh-CN",
		"/api/editor/v1/story/card/x501/1/snapshot?locale=zh-CN",
	} {
		h.expectError(t, http.MethodGet, path, h.token, nil, http.StatusBadRequest, "invalid_request")
	}
	changed := sideStoryAPIScenario(t, "test_card_501_01", sideStoryAPITalk{speaker: "テスト話者甲", body: "テスト台詞改"})
	script, err := store.ParseSideStoryScript(changed, "test_card_501_01")
	if err != nil {
		t.Fatal(err)
	}
	h.runner.scripts["card/501/1"] = script.CanonicalJSON
	h.expectError(t, http.MethodGet, "/api/editor/v1/story/card/501/1/snapshot?locale=zh-CN", h.token, nil, http.StatusConflict, "script_changed")
	h.runner.scriptErr = errors.New("test upstream is down")
	h.expectError(t, http.MethodGet, "/api/editor/v1/story/card/501/1/snapshot?locale=zh-CN", h.token, nil, http.StatusBadGateway, "upstream_unavailable")
}
