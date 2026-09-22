package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/store"
)

// TestSSEBroadcastOnEdit connects an SSE client, performs an entry edit through
// the API, and asserts the client receives an entry.updated event.
func TestSSEBroadcastOnEdit(t *testing.T) {
	ts, token := setup(t)

	// Open the SSE stream with the normal session bearer token.
	req := bearerSSERequest(t, ts.URL, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sse connect status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("sse Content-Type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache, no-transform, no-store, must-revalidate" {
		t.Fatalf("sse Cache-Control = %q", got)
	}
	if resp.Header.Get("Pragma") != "no-cache" || resp.Header.Get("Expires") != "0" {
		t.Fatalf("sse legacy cache headers = %#v", resp.Header)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("sse X-Accel-Buffering = %q", got)
	}

	// Read events in a goroutine until we see entry.updated or time out.
	got := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if event == "entry.updated" {
					var data map[string]any
					_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data)
					got <- data
					return
				}
			}
		}
	}()

	// Give the client a moment to register before broadcasting.
	time.Sleep(100 * time.Millisecond)

	body, _ := json.Marshal(map[string]string{
		"category": "cards", "field": "prefix", "key": "こんにちは",
		"text": "你好（已校对）", "source": "human",
	})
	editReq, _ := http.NewRequest("PUT", ts.URL+"/api/editor/v1/entry", bytes.NewReader(body))
	editReq.Header.Set("Authorization", "Bearer "+token)
	editReq.Header.Set(loadedProducerStateHeader, loadedGateState(t, ts, token))
	editResp, err := http.DefaultClient.Do(editReq)
	if err != nil {
		t.Fatal(err)
	}
	editResp.Body.Close()

	select {
	case data := <-got:
		if data["key"] != "こんにちは" || data["text"] != "你好（已校对）" {
			t.Errorf("unexpected event payload: %+v", data)
		}
		if data["user"] != "alice" {
			t.Errorf("expected user alice, got %v", data["user"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for entry.updated SSE event")
	}
}

// TestSSERequiresAuth verifies the stream rejects unauthenticated clients.
func TestSSERequiresAuth(t *testing.T) {
	ts, _ := setup(t)
	resp, err := http.Get(ts.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func bearerSSERequest(t *testing.T, serverURL, token string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, serverURL+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestSSEClosesWhenTokenGenerationChanges(t *testing.T) {
	ts, token := setup(t)
	request := bearerSSERequest(t, ts.URL, token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	closed := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(response.Body)
		close(closed)
	}()
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": "replacement-password-123"})
	update, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/admin/users", bytes.NewReader(body))
	update.Header.Set("Authorization", "Bearer "+token)
	updated, err := http.DefaultClient.Do(update)
	if err != nil {
		t.Fatal(err)
	}
	updated.Body.Close()
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("password update status = %d", updated.StatusCode)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("revoked user retained live SSE stream")
	}
}

func TestSSEValidationRechecksBearerTokenGeneration(t *testing.T) {
	h := setupLegacyAPI(t)
	request := bearerSSERequest(t, h.server.URL, h.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE bearer status = %d", response.StatusCode)
	}

	closed := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(response.Body)
		close(closed)
	}()
	if _, err := h.db.Exec(`UPDATE users SET token_version = token_version + 1 WHERE username = 'alice'`); err != nil {
		t.Fatal(err)
	}
	h.api.hub.Broadcast("generation.check", map[string]bool{"changed": true})

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream retained a bearer token from a revoked generation")
	}
}

func TestLegacySSENoopDoesNotBroadcast(t *testing.T) {
	ts, token := setup(t)
	req := bearerSSERequest(t, ts.URL, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "event: ") {
				// The connect-time producer status is not a mutation broadcast.
				if event := strings.TrimSpace(strings.TrimPrefix(line, "event: ")); event != "gate.status" {
					events <- event
					return
				}
			}
		}
	}()

	noopBody, _ := json.Marshal(map[string]string{
		"category": "cards", "field": "prefix", "key": "こんにちは",
		"text": "你好", "source": "cn",
	})
	editReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/editor/v1/entry", bytes.NewReader(noopBody))
	editReq.Header.Set("Authorization", "Bearer "+token)
	editReq.Header.Set(loadedProducerStateHeader, loadedGateState(t, ts, token))
	editResp, err := http.DefaultClient.Do(editReq)
	if err != nil {
		t.Fatal(err)
	}
	defer editResp.Body.Close()
	var result map[string]string
	if err := json.NewDecoder(editResp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["status"] != "noop" {
		t.Fatalf("status = %q", result["status"])
	}

	select {
	case event := <-events:
		t.Fatalf("noop broadcast unexpected SSE event %q", event)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestLegacySSEEventStoryUpdatePayload(t *testing.T) {
	ts, token := setup(t)
	req := bearerSSERequest(t, ts.URL, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "eventstory.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data)
				got <- data
				return
			}
		}
	}()

	body, _ := json.Marshal(map[string]any{
		"eventId": 1, "episodeNo": "1", "jpKey": "おはよう",
		"cnText": "早安", "source": "human", "entryType": "talk",
	})
	editReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/editor/v1/event-story/update", bytes.NewReader(body))
	editReq.Header.Set("Authorization", "Bearer "+token)
	editReq.Header.Set(loadedProducerStateHeader, loadedGateState(t, ts, token))
	editResp, err := http.DefaultClient.Do(editReq)
	if err != nil {
		t.Fatal(err)
	}
	editResp.Body.Close()

	select {
	case data := <-got:
		if data["eventId"] != float64(1) || data["episodeNo"] != "1" || data["jpKey"] != "おはよう" ||
			data["cnText"] != "早安" || data["source"] != "human" || data["entryType"] != "talk" || data["user"] != "alice" {
			t.Fatalf("eventstory.updated payload = %#v", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for eventstory.updated SSE event")
	}
}

func TestPromoteEventStoryBroadcastPreservesClientID(t *testing.T) {
	ts, token := setup(t)
	request := bearerSSERequest(t, ts.URL, token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	events := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewReader(response.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "eventstory.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					events <- data
				}
				return
			}
		}
	}()

	body, err := json.Marshal(map[string]any{"eventId": 1, "clientId": "promote-window"})
	if err != nil {
		t.Fatal(err)
	}
	promoteRequest, err := http.NewRequest(http.MethodPost, ts.URL+"/api/editor/v1/event-story/promote-human", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	promoteRequest.Header.Set("Authorization", "Bearer "+token)
	promoteRequest.Header.Set(loadedProducerStateHeader, loadedGateState(t, ts, token))
	promoteResponse, err := http.DefaultClient.Do(promoteRequest)
	if err != nil {
		t.Fatal(err)
	}
	promoteResponse.Body.Close()
	if promoteResponse.StatusCode != http.StatusOK {
		t.Fatalf("promote event story status=%d", promoteResponse.StatusCode)
	}

	select {
	case data := <-events:
		if len(data) != 4 || data["eventId"] != float64(1) || data["promote"] != "human" ||
			data["clientId"] != "promote-window" || data["user"] != "alice" {
			t.Fatalf("promote eventstory.updated payload=%#v", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for promote eventstory.updated SSE event")
	}
}

func TestLyricsRenditionBroadcastIncludesOnlyOneActualMutationTarget(t *testing.T) {
	h := setupLegacyAPI(t)
	request := bearerSSERequest(t, h.server.URL, h.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	events := make(chan map[string]any, 2)
	go func() {
		reader := bufio.NewReader(response.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "lyrics.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					events <- data
				}
				event = ""
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	h.api.broadcastLyricsRenditionUpdated(store.LyricsRenditionDocument{MusicID: 765, Revision: 2, TranslationEditionKey: "main"},
		[]store.LyricsRenditionMutationTarget{{RenditionKey: "sekai", Side: "game", Locale: "zh-CN"}}, "game-window", "alice")
	select {
	case data := <-events:
		if len(data) != 8 || data["musicId"] != float64(765) || data["revision"] != float64(2) || data["editionKey"] != "main" ||
			data["clientId"] != "game-window" || data["user"] != "alice" || data["renditionKey"] != "sekai" ||
			data["side"] != "game" || data["locale"] != "zh-CN" {
			t.Fatalf("single-target lyrics.updated payload=%#v", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for single-target lyrics.updated")
	}

	h.api.broadcastLyricsRenditionUpdated(store.LyricsRenditionDocument{MusicID: 765, Revision: 3, TranslationEditionKey: "alternate"},
		[]store.LyricsRenditionMutationTarget{
			{RenditionKey: "sekai", Side: "full", Locale: "zh-CN"},
			{RenditionKey: "sekai", Side: "credits", Locale: "zh-CN"},
		}, "multi-window", "bob")
	select {
	case data := <-events:
		if len(data) != 5 || data["musicId"] != float64(765) || data["revision"] != float64(3) || data["editionKey"] != "alternate" ||
			data["clientId"] != "multi-window" || data["user"] != "bob" {
			t.Fatalf("multi-target lyrics.updated payload=%#v", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for multi-target lyrics.updated")
	}
}

func TestLyricsMutationsBroadcastCollaborationEvents(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)
	request := bearerSSERequest(t, h.server.URL, h.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	events := make(chan map[string]any, 8)
	go func() {
		reader := bufio.NewReader(response.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "lyrics.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					events <- data
				}
				event = ""
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	await := func(clientID string, revision int) {
		t.Helper()
		select {
		case data := <-events:
			if len(data) != 4 || data["musicId"] != float64(10) || data["revision"] != float64(revision) ||
				data["clientId"] != clientID || data["user"] != "alice" {
				t.Fatalf("lyrics.updated payload=%#v", data)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for lyrics.updated clientId=%q", clientID)
		}
	}
	assertNoEvent := func(action string) {
		t.Helper()
		select {
		case data := <-events:
			t.Fatalf("%s broadcast unexpected lyrics event=%#v", action, data)
		case <-time.After(200 * time.Millisecond):
		}
	}

	saveBody, _ := json.Marshal(apiLyrics())
	var savePayload map[string]any
	if err := json.Unmarshal(saveBody, &savePayload); err != nil {
		t.Fatal(err)
	}
	savePayload["clientId"] = "save-window"
	savedResponse := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", savePayload)
	savedBody, err := io.ReadAll(savedResponse.Body)
	savedResponse.Body.Close()
	if err != nil || savedResponse.StatusCode != http.StatusOK || bytes.Contains(savedBody, []byte(`"clientId"`)) {
		t.Fatalf("save status=%d body=%s err=%v", savedResponse.StatusCode, savedBody, err)
	}
	await("save-window", 1)
	savePayload["revision"] = 1
	noopSave := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", savePayload)
	noopSave.Body.Close()
	if noopSave.StatusCode != http.StatusOK {
		t.Fatalf("noop save status=%d", noopSave.StatusCode)
	}
	assertNoEvent("noop save")

	published := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]any{
		"musicId": 10, "revision": 1, "clientId": "publish-window",
	})
	published.Body.Close()
	if published.StatusCode != http.StatusOK {
		t.Fatalf("publish status=%d", published.StatusCode)
	}
	await("publish-window", 1)
	noopPublish := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]any{
		"musicId": 10, "revision": 1, "clientId": "noop-publish-window",
	})
	noopPublish.Body.Close()
	if noopPublish.StatusCode != http.StatusOK {
		t.Fatalf("noop publish status=%d", noopPublish.StatusCode)
	}
	assertNoEvent("noop publish")

	unpublished := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/unpublish", map[string]any{
		"musicId": 10, "revision": 1, "clientId": "unpublish-window",
	})
	unpublished.Body.Close()
	if unpublished.StatusCode != http.StatusOK {
		t.Fatalf("unpublish status=%d", unpublished.StatusCode)
	}
	await("unpublish-window", 1)
	noopUnpublish := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/unpublish", map[string]any{
		"musicId": 10, "revision": 1, "clientId": "noop-unpublish-window",
	})
	noopUnpublish.Body.Close()
	if noopUnpublish.StatusCode != http.StatusOK {
		t.Fatalf("noop unpublish status=%d", noopUnpublish.StatusCode)
	}
	assertNoEvent("noop unpublish")

	// Structural edits must round-trip the server's explicit ruby representation;
	// the earlier no-op save intentionally exercises the legacy omitted-ruby path.
	if err := json.Unmarshal(savedBody, &savePayload); err != nil {
		t.Fatal(err)
	}
	lines := savePayload["lines"].([]any)
	segments := lines[0].(map[string]any)["segments"].([]any)
	firstSegment := segments[0].(map[string]any)
	firstSegment["performerIds"] = nil
	savePayload["revision"] = 1
	savePayload["clientId"] = "null-performers"
	nullPerformers := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", savePayload)
	nullPerformersBody, err := io.ReadAll(nullPerformers.Body)
	nullPerformers.Body.Close()
	if err != nil || nullPerformers.StatusCode != http.StatusOK {
		t.Fatalf("null performerIds save status=%d body=%s err=%v", nullPerformers.StatusCode, nullPerformersBody, err)
	}
	await("null-performers", 2)

	delete(firstSegment, "performerIds")
	savePayload["revision"] = 2
	savePayload["clientId"] = "omitted-performers"
	omittedPerformers := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", savePayload)
	omittedPerformers.Body.Close()
	if omittedPerformers.StatusCode != http.StatusOK {
		t.Fatalf("omitted performerIds retry status=%d", omittedPerformers.StatusCode)
	}
	assertNoEvent("omitted performerIds retry")

	savePayload["revision"] = 0
	failedSave := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", savePayload)
	failedSave.Body.Close()
	if failedSave.StatusCode != http.StatusConflict {
		t.Fatalf("failed save status=%d", failedSave.StatusCode)
	}
	failedPublish := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]any{
		"musicId": 10, "revision": 99, "clientId": "failed-window",
	})
	failedPublish.Body.Close()
	if failedPublish.StatusCode != http.StatusConflict {
		t.Fatalf("failed publish status=%d", failedPublish.StatusCode)
	}
	select {
	case data := <-events:
		t.Fatalf("failed lyrics mutation broadcast=%#v", data)
	case <-time.After(300 * time.Millisecond):
	}
}

// The console tracks producer runs over SSE, so the stream must carry the gate
// status on connect and again on every producer transition.
func TestSSECarriesEditorGateStatus(t *testing.T) {
	h := setupLegacyAPI(t)
	response, err := http.DefaultClient.Do(bearerSSERequest(t, h.server.URL, h.token))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", response.StatusCode)
	}

	gates := make(chan map[string]any, 4)
	go func() {
		reader := bufio.NewReader(response.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "gate.status" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					gates <- data
				}
				event = ""
			}
		}
	}()

	await := func(step string) map[string]any {
		t.Helper()
		select {
		case data := <-gates:
			return data
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for the %s gate.status", step)
			return nil
		}
	}

	connected := await("connect")
	if connected["running"] != false || connected["generation"] != float64(0) {
		t.Fatalf("connect gate.status = %#v", connected)
	}

	release, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	started := await("producer start")
	if started["running"] != true || started["generation"] != float64(1) || started["completedGeneration"] != float64(0) {
		t.Fatalf("producer start gate.status = %#v", started)
	}

	release()
	finished := await("producer finish")
	if finished["running"] != false || finished["completedGeneration"] != float64(1) {
		t.Fatalf("producer finish gate.status = %#v", finished)
	}
}
