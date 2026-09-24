package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/model"
	"moesekai/server/internal/translator"
)

func TestEditorGateStatusAndStrictHeaderContract(t *testing.T) {
	h := setupLegacyAPI(t)
	unauthorized, err := http.Get(h.server.URL + "/api/editor-gate/status")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}

	statusResponse := authorizedRequest(t, h, http.MethodGet, "/api/editor-gate/status", nil)
	defer statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK || statusResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("gate status=%d cache=%q", statusResponse.StatusCode, statusResponse.Header.Get("Cache-Control"))
	}
	var status editorgate.Status
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Version != 1 || status.InstanceID == "" || status.Revision != 0 || status.Generation != 0 ||
		status.CompletedGeneration != 0 || status.Running || status.LastRun != "" {
		t.Fatalf("initial gate status = %+v", status)
	}

	// Content mutations without the header fall back to the lenient admission
	// and reach their handler; the producer-only backup push still requires it.
	aliases := []struct {
		method string
		path   string
		strict bool
	}{
		{http.MethodPut, "/api/editor/v1/entry", false},
		{http.MethodPut, "/api/editor/v1/category/batch", false},
		{http.MethodPut, "/api/editor/v1/event-story/update", false},
		{http.MethodPost, "/api/editor/v1/event-story/promote-human", false},
		{http.MethodPut, "/api/editor/v1/lyrics/save", false},
		{http.MethodPost, "/api/editor/v1/lyrics/publish", false},
		{http.MethodPost, "/api/editor/v1/lyrics/unpublish", false},
		{http.MethodPost, "/api/editor/v1/backup/push", true},
	}
	for _, alias := range aliases {
		response := strictRequest(t, h, alias.method, alias.path, nil, nil)
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if alias.strict {
			if response.StatusCode != http.StatusPreconditionRequired {
				t.Fatalf("missing header %s status = %d", alias.path, response.StatusCode)
			}
			continue
		}
		if response.StatusCode == http.StatusPreconditionRequired || body["error"] == "loaded producer state required" ||
			body["error"] == "invalid loaded producer state" {
			t.Fatalf("missing header %s was rejected by the gate: status=%d body=%v", alias.path, response.StatusCode, body)
		}
	}

	overSafe := strconv.FormatUint(editorgate.MaxSafeCounter+1, 10)
	malformed := []string{
		"", "***:0:0", status.InstanceID, status.InstanceID + ":", status.InstanceID + ":0",
		status.InstanceID + "::0", status.InstanceID + ":0:", status.InstanceID + ":00:0",
		status.InstanceID + ":+1:0", status.InstanceID + ":0:00", status.InstanceID + ":0:+1",
		status.InstanceID + ":" + overSafe + ":0", status.InstanceID + ":0:" + overSafe,
		status.InstanceID + ":0:0:0",
	}
	for _, value := range malformed {
		response := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{}, []string{value})
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed header %q status = %d", value, response.StatusCode)
		}
	}
	duplicate := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{}, []string{
		loadedState(status), loadedState(status),
	})
	duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate header status = %d", duplicate.StatusCode)
	}
	restarted := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{}, []string{
		base64.RawURLEncoding.EncodeToString([]byte("different-process")) + ":0:0",
	})
	restarted.Body.Close()
	if restarted.StatusCode != http.StatusConflict {
		t.Fatalf("restart instance status = %d", restarted.StatusCode)
	}

	success := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "cn-key", "text": "strict edit", "source": model.SourceHuman,
	}, []string{loadedState(status)})
	success.Body.Close()
	if success.StatusCode != http.StatusOK {
		t.Fatalf("strict success status = %d", success.StatusCode)
	}
}

func TestStrictContentMutationWithoutHeaderUsesLenientAdmission(t *testing.T) {
	h := setupLegacyAPI(t)
	entryText := func() string {
		t.Helper()
		var text string
		if err := h.db.QueryRow(`SELECT cn_text FROM entries WHERE category='cards' AND field='prefix' AND jp_key='cn-key'`).Scan(&text); err != nil {
			t.Fatal(err)
		}
		return text
	}
	edit := func(text string, headerValues []string) *http.Response {
		t.Helper()
		return strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
			"category": "cards", "field": "prefix", "key": "cn-key", "text": text, "source": model.SourceHuman,
		}, headerValues)
	}

	idle := edit("agent edit", nil)
	idle.Body.Close()
	if idle.StatusCode != http.StatusOK || entryText() != "agent edit" {
		t.Fatalf("headerless edit while idle status=%d text=%q", idle.StatusCode, entryText())
	}

	releaseProducer, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	running := edit("must not persist", nil)
	var runningBody map[string]any
	if err := json.NewDecoder(running.Body).Decode(&runningBody); err != nil {
		running.Body.Close()
		t.Fatal(err)
	}
	running.Body.Close()
	if running.StatusCode != http.StatusConflict || runningBody["error"] != "producer is running; reload before saving" ||
		entryText() != "agent edit" {
		t.Fatalf("headerless edit while producing status=%d body=%v text=%q", running.StatusCode, runningBody, entryText())
	}
	stale := h.api.editorGate.Status()
	releaseProducer()

	malformed := edit("must not persist", []string{"not-a-producer-state"})
	malformed.Body.Close()
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed header status=%d", malformed.StatusCode)
	}
	staleResponse := edit("must not persist", []string{loadedState(stale)})
	var current editorgate.Status
	if err := json.NewDecoder(staleResponse.Body).Decode(&current); err != nil {
		staleResponse.Body.Close()
		t.Fatal(err)
	}
	staleResponse.Body.Close()
	if staleResponse.StatusCode != http.StatusConflict || current != h.api.editorGate.Status() || entryText() != "agent edit" {
		t.Fatalf("stale header status=%d body=%+v text=%q", staleResponse.StatusCode, current, entryText())
	}

	after := edit("agent edit after producer", nil)
	after.Body.Close()
	if after.StatusCode != http.StatusOK || entryText() != "agent edit after producer" {
		t.Fatalf("headerless edit after producer status=%d text=%q", after.StatusCode, entryText())
	}
}

func TestStrictEditorMutationCarriesAtomicallyAcceptedStatusInContext(t *testing.T) {
	h := setupLegacyAPI(t)
	loaded := h.api.editorGate.Status()
	var accepted editorgate.Status
	handler := h.api.strictEditorMutation(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		accepted, ok = acceptedEditorStatus(r)
		if !ok {
			t.Fatal("strict handler did not receive accepted producer status")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPut, "/strict", nil)
	request.Header.Set(loadedProducerStateHeader, loadedState(loaded))
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("strict context response status = %d", response.Code)
	}
	if accepted != loaded {
		t.Fatalf("accepted context status = %+v, want %+v", accepted, loaded)
	}
}

func TestStrictEditorRevisionOnlyMismatchReturns409WithoutMutation(t *testing.T) {
	h := setupLegacyAPI(t)
	loaded := h.api.editorGate.Status()
	var auditsBefore int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&auditsBefore); err != nil {
		t.Fatal(err)
	}
	staleRevision := loaded
	staleRevision.Revision++
	response := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "cn-key", "text": "must not persist", "source": model.SourceHuman,
	}, []string{loadedState(staleRevision)})
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("revision-only mismatch status=%d", response.StatusCode)
	}
	var current editorgate.Status
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if current != loaded {
		t.Fatalf("revision-only mismatch status body=%+v want=%+v", current, loaded)
	}
	var text string
	if err := h.db.QueryRow(`SELECT cn_text FROM entries WHERE category='cards' AND field='prefix' AND jp_key='cn-key'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	var auditsAfter int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&auditsAfter); err != nil {
		t.Fatal(err)
	}
	if text != "官方" || auditsAfter != auditsBefore {
		t.Fatalf("revision-only mismatch mutated text=%q audits=%d want=%d", text, auditsAfter, auditsBefore)
	}
}

func TestProducerRejectsStrictEditors(t *testing.T) {
	h := setupLegacyAPI(t)
	loaded := h.api.editorGate.Status()
	releaseProducer, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}

	strict := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "cn-key", "text": "must not persist", "source": model.SourceHuman,
	}, []string{loadedState(loaded)})
	if strict.StatusCode != http.StatusConflict {
		strict.Body.Close()
		t.Fatalf("strict producer conflict status = %d", strict.StatusCode)
	}
	var conflict map[string]any
	if err := json.NewDecoder(strict.Body).Decode(&conflict); err != nil {
		strict.Body.Close()
		t.Fatal(err)
	}
	strict.Body.Close()
	if len(conflict) != 7 || conflict["version"] != float64(1) || conflict["running"] != true || conflict["generation"] != float64(1) {
		t.Fatalf("strict conflict producer status = %#v", conflict)
	}

	releaseProducer()

	stale := strictRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{}, []string{loadedState(loaded)})
	defer stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale generation status = %d", stale.StatusCode)
	}
}

func TestTranslationProducerCompletesGateWithoutChangingTranslateStatusShape(t *testing.T) {
	h := setupLegacyAPI(t)
	result, err := h.api.translator.ManualAITranslate(translator.AITranslateRequest{})
	if err == nil || result.Category != "" {
		t.Fatalf("invalid producer result=%+v err=%v", result, err)
	}
	gate := h.api.editorGate.Status()
	if gate.Running || gate.Generation != 1 || gate.CompletedGeneration != 1 || gate.Revision != 2 || gate.LastRun == "" {
		t.Fatalf("translation producer gate = %+v", gate)
	}

	response := authorizedRequest(t, h, http.MethodGet, "/api/translate/status", nil)
	defer response.Body.Close()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(body) != 2 || body["translator"] == nil || body["clients"] == nil {
		t.Fatalf("translate status shape status=%d keys=%v", response.StatusCode, body)
	}
	var translatorStatus map[string]any
	if err := json.Unmarshal(body["translator"], &translatorStatus); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"running": true, "lastRun": true, "lastMode": true, "lastError": true, "lastNote": true}
	for key := range translatorStatus {
		if !allowed[key] {
			t.Fatalf("translate status gained field %q: %#v", key, translatorStatus)
		}
	}
}

func TestInvalidTranslationSourcesNeverPersist(t *testing.T) {
	h := setupLegacyAPI(t)

	entry := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "cn-key", "text": "invalid entry", "source": "official_cn",
	})
	entry.Body.Close()
	if entry.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid entry source status = %d", entry.StatusCode)
	}
	var entryText string
	if err := h.db.QueryRow(`SELECT cn_text FROM entries WHERE category='cards' AND field='prefix' AND jp_key='cn-key'`).Scan(&entryText); err != nil || entryText != "官方" {
		t.Fatalf("invalid entry persisted text=%q err=%v", entryText, err)
	}

	snapshotResponse := authorizedRequest(t, h, http.MethodGet, "/api/category/snapshot?category=cards&locale=en-US", nil)
	var snapshot model.CategoryLocaleSnapshot
	if err := json.NewDecoder(snapshotResponse.Body).Decode(&snapshot); err != nil {
		snapshotResponse.Body.Close()
		t.Fatal(err)
	}
	snapshotResponse.Body.Close()
	batch := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/category/batch", map[string]any{
		"category": "cards", "locale": model.LocaleEnglish, "baseRevision": snapshot.Revision,
		"updates": []map[string]string{
			{"field": "prefix", "key": "cn-key", "text": "valid-looking", "source": model.SourceHuman},
			{"field": "prefix", "key": "human-key", "text": "invalid", "source": "official_cn"},
		},
	})
	batch.Body.Close()
	if batch.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid batch source status = %d", batch.StatusCode)
	}
	var localized int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM entry_localizations WHERE category='cards' AND locale='en-US'`).Scan(&localized); err != nil || localized != 0 {
		t.Fatalf("invalid batch persisted rows=%d err=%v", localized, err)
	}

	event := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "cnText": "invalid event", "source": "official_cn", "entryType": "talk",
	})
	event.Body.Close()
	if event.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid event source status = %d", event.StatusCode)
	}
	var eventText string
	if err := h.db.QueryRow(`SELECT cn_text FROM event_story_lines WHERE event_id=42 AND episode_no='1' AND jp_key='二'`).Scan(&eventText); err != nil || eventText != "第二句" {
		t.Fatalf("invalid event persisted text=%q err=%v", eventText, err)
	}
}

func loadedState(status editorgate.Status) string {
	return status.InstanceID + ":" + strconv.FormatUint(status.Revision, 10) + ":" +
		strconv.FormatUint(status.CompletedGeneration, 10)
}

// strictAs sends a v1 mutation as token with the current producer-state proof.
func strictAs(t *testing.T, h *legacyAPIHarness, method, path, token string, body any) *http.Response {
	t.Helper()
	return strictRequestAs(t, h, method, path, token, body, []string{loadedState(h.api.editorGate.Status())})
}

func strictRequest(t *testing.T, h *legacyAPIHarness, method, path string, body any, headerValues []string) *http.Response {
	t.Helper()
	return strictRequestAs(t, h, method, path, h.token, body, headerValues)
}

func strictRequestAs(t *testing.T, h *legacyAPIHarness, method, path, token string, body any, headerValues []string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, value := range headerValues {
		request.Header.Add(loadedProducerStateHeader, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
