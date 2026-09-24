package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"moesekai/server/internal/auth"
)

// The legacy write routes were deleted in favour of their producer-aware v1
// twins, so they must fall through to the JSON API catch-all.
func TestDeletedLegacyWriteRoutesReturn404(t *testing.T) {
	h := setupLegacyAPI(t)
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/category/batch"},
		{http.MethodPost, "/api/lyrics/translation-editions"},
		{http.MethodPost, "/api/lyrics/unpublish"},
		{http.MethodPut, "/api/event-story/update"},
		{http.MethodPost, "/api/event-story/promote-human"},
		{http.MethodPost, "/api/backup/push"},
	} {
		response := doJSON(t, route.method, h.server.URL+route.path, h.token, map[string]any{})
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]string
		if response.StatusCode != http.StatusNotFound || json.Unmarshal(body, &decoded) != nil || decoded["error"] != "not found" {
			t.Fatalf("%s %s status=%d body=%s", route.method, route.path, response.StatusCode, body)
		}
	}
}

// Agent tooling still posts to /api/entry, /api/lyrics/save and
// /api/lyrics/publish; each must answer exactly like its v1 twin for every
// caller and producer-state header.
func TestAgentAliasRoutesMatchTheirV1Twins(t *testing.T) {
	h := setupLegacyAPI(t)
	editor, err := h.api.auth.CreateUser("alias-editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	editorToken, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	restarted := base64.RawURLEncoding.EncodeToString([]byte("different-process")) + ":0:0"
	for _, pair := range []struct {
		method    string
		alias     string
		twin      string
		adminOnly bool
	}{
		{http.MethodPut, "/api/entry", "/api/editor/v1/entry", false},
		{http.MethodPut, "/api/lyrics/save", "/api/editor/v1/lyrics/save", false},
		{http.MethodPost, "/api/lyrics/publish", "/api/editor/v1/lyrics/publish", true},
	} {
		for _, caller := range []struct {
			name    string
			token   string
			headers []string
			status  int
		}{
			{name: "anonymous", status: http.StatusUnauthorized},
			{name: "editor without header", token: editorToken},
			{name: "admin without header", token: h.token},
			{name: "admin with malformed header", token: h.token, headers: []string{"not-a-producer-state"}, status: http.StatusBadRequest},
			{name: "admin with stale header", token: h.token, headers: []string{restarted}, status: http.StatusConflict},
		} {
			twinStatus, twinBody := routeResponse(t, h, pair.method, pair.twin, caller.token, caller.headers)
			aliasStatus, aliasBody := routeResponse(t, h, pair.method, pair.alias, caller.token, caller.headers)
			if aliasStatus != twinStatus || !bytes.Equal(aliasBody, twinBody) {
				t.Fatalf("%s %s (%s) = %d %s, twin %s = %d %s", pair.method, pair.alias, caller.name,
					aliasStatus, aliasBody, pair.twin, twinStatus, twinBody)
			}
			want := caller.status
			if caller.name == "editor without header" && pair.adminOnly {
				want = http.StatusForbidden
			}
			if want != 0 && aliasStatus != want {
				t.Fatalf("%s %s (%s) status = %d, want %d: %s", pair.method, pair.alias, caller.name, aliasStatus, want, aliasBody)
			}
			if want == 0 && (aliasStatus == http.StatusNotFound || aliasStatus == http.StatusForbidden ||
				aliasStatus == http.StatusUnauthorized || aliasStatus == http.StatusPreconditionRequired) {
				t.Fatalf("%s %s (%s) was not admitted: %d %s", pair.method, pair.alias, caller.name, aliasStatus, aliasBody)
			}
		}
	}
}

func routeResponse(t *testing.T, h *legacyAPIHarness, method, path, token string, headers []string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, h.server.URL+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for _, value := range headers {
		request.Header.Add(loadedProducerStateHeader, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func TestStrictLyricsSourceReviewImportRejectsMalformedProducerState(t *testing.T) {
	h := setupLegacyAPI(t)
	reviewID := seedArtifactReviewAPI(t, h)
	approve := authorizedRequest(t, h, http.MethodPut, "/api/admin/lyrics-source-reviews/decision", map[string]any{
		"reviewId": reviewID, "gate": "overall", "decision": "approved", "expectedVersion": 1,
		"idempotencyKey": "strict-import-approved-0001", "note": "",
	})
	approve.Body.Close()
	if approve.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d", approve.StatusCode)
	}

	malformed := strictRequest(t, h, http.MethodPost, "/api/editor/v1/admin/lyrics-source-reviews/import",
		map[string]any{"reviewId": reviewID}, []string{"not-a-producer-state"})
	malformed.Body.Close()
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed producer state status = %d", malformed.StatusCode)
	}

	response := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/admin/lyrics-source-reviews/import",
		map[string]any{"reviewId": reviewID})
	defer response.Body.Close()
	var result lyricsSourceReviewImportResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !result.Changed || result.ReviewID != reviewID || result.Lyrics.Revision != 1 {
		t.Fatalf("strict import status=%d body=%+v", response.StatusCode, result)
	}
}

// The read-only handlers never inspect r.Method, so the registration has to
// reject write methods for them.
func TestReadOnlyRoutesRejectWriteMethods(t *testing.T) {
	h := setupLegacyAPI(t)
	for _, path := range []string{
		"/api/categories",
		"/api/entries?category=cards&field=prefix",
		"/api/event-stories",
		"/api/event-story?eventId=42",
		"/api/backup/status",
		"/api/translate/status",
		"/api/search/status",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			response := doJSON(t, method, h.server.URL+path, h.token, map[string]any{})
			response.Body.Close()
			if response.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status = %d, want 405", method, path, response.StatusCode)
			}
			if got := response.Header.Get("Allow"); got != "GET, HEAD" {
				t.Fatalf("%s %s Allow = %q", method, path, got)
			}
		}
		read := doJSON(t, http.MethodGet, h.server.URL+path, h.token, nil)
		read.Body.Close()
		if read.StatusCode == http.StatusMethodNotAllowed {
			t.Fatalf("GET %s was rejected as 405", path)
		}
	}
}

// The side-story routes are method-scoped patterns: their own method reaches
// the handler, any other method falls through to the JSON API catch-all.
func TestSideStoryRoutesAreMethodScoped(t *testing.T) {
	h := setupLegacyAPI(t)
	caughtAll := func(method, path string) (bool, []byte) {
		t.Helper()
		var body any
		if method != http.MethodGet {
			body = map[string]any{}
		}
		response := authorizedRequest(t, h, method, path, body)
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		return response.StatusCode == http.StatusNotFound && json.Unmarshal(data, &decoded) == nil && decoded["error"] == "not found", data
	}
	for _, route := range []struct {
		method, path, wrongMethod string
	}{
		{http.MethodGet, "/api/editor/v1/stories?kind=card", http.MethodPost},
		{http.MethodGet, "/api/editor/v1/stories/sync", http.MethodPut},
		{http.MethodPost, "/api/editor/v1/stories/sync", http.MethodDelete},
		{http.MethodGet, "/api/editor/v1/story/card/1", http.MethodPut},
		{http.MethodPut, "/api/editor/v1/story/card/1/1", http.MethodGet},
		{http.MethodPost, "/api/editor/v1/story/card/1/ai", http.MethodGet},
		{http.MethodPost, "/api/editor/v1/story/area/areatalk_01/refresh", http.MethodDelete},
		{http.MethodGet, "/api/editor/v1/story/card/1/1/snapshot?locale=zh-CN", http.MethodPost},
	} {
		if missed, data := caughtAll(route.method, route.path); missed {
			t.Fatalf("%s %s did not reach its handler: %s", route.method, route.path, data)
		}
		if routed, data := caughtAll(route.wrongMethod, route.path); !routed {
			t.Fatalf("%s %s = %s, want the JSON catch-all", route.wrongMethod, route.path, data)
		}
	}
}
