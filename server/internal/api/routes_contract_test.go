package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// The legacy write routes were deleted in favour of their producer-aware v1
// twins, so they must fall through to the JSON API catch-all.
func TestDeletedLegacyWriteRoutesReturn404(t *testing.T) {
	h := setupLegacyAPI(t)
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/entry"},
		{http.MethodPut, "/api/category/batch"},
		{http.MethodPut, "/api/lyrics/save"},
		{http.MethodPost, "/api/lyrics/translation-editions"},
		{http.MethodPost, "/api/lyrics/publish"},
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

func TestStrictLyricsSourceReviewImportRequiresProducerState(t *testing.T) {
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

	missing := strictRequest(t, h, http.MethodPost, "/api/editor/v1/admin/lyrics-source-reviews/import",
		map[string]any{"reviewId": reviewID}, nil)
	missing.Body.Close()
	if missing.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("missing producer state status = %d", missing.StatusCode)
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
