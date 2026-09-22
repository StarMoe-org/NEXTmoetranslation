package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeEventMutationPreservesClientID(t *testing.T) {
	for name, testCase := range map[string]struct {
		body         string
		wantClientID string
	}{
		"current client": {body: `{"eventId":42,"clientId":"tab-123"}`, wantClientID: "tab-123"},
		"legacy client":  {body: `{"eventId":42}`},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/event-story/retry", strings.NewReader(testCase.body))
			recorder := httptest.NewRecorder()

			eventID, clientID, ok := decodeEventMutation(recorder, req)
			if !ok {
				t.Fatalf("decodeEventMutation rejected valid request: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if eventID != 42 || clientID != testCase.wantClientID {
				t.Fatalf("decoded mutation = (%d, %q), want (42, %q)", eventID, clientID, testCase.wantClientID)
			}
		})
	}
}

func TestDecodeEventMutationRejectsOversizedClientID(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/event-story/retry", strings.NewReader(
		`{"eventId":42,"clientId":"`+strings.Repeat("x", 129)+`"}`,
	))
	recorder := httptest.NewRecorder()

	if _, _, ok := decodeEventMutation(recorder, req); ok {
		t.Fatal("decodeEventMutation accepted oversized clientId")
	}
	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "clientId too long") {
		t.Fatalf("oversized clientId response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPromoteEventStoryHumanReportsMissingStory(t *testing.T) {
	h := setupLegacyAPI(t)
	response := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/event-story/promote-human",
		map[string]any{"eventId": 4242})
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "event story not found") {
		t.Fatalf("promote of a missing story status=%d body=%s", response.StatusCode, body)
	}
	var promoted int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM event_stories WHERE event_id=4242`).Scan(&promoted); err != nil {
		t.Fatal(err)
	}
	if promoted != 0 {
		t.Fatalf("missing story rows = %d", promoted)
	}
}
