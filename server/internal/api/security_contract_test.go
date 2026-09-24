package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/auth"
)

func TestEditorsCannotTriggerAdministrativeOperations(t *testing.T) {
	h := setupLegacyAPI(t)
	editor, err := h.api.auth.CreateUser("editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/editor/v1/event-story/promote-human"},
		{http.MethodPost, "/api/event-story/retry"},
		{http.MethodPost, "/api/event-story/reorder"},
		{http.MethodPost, "/api/translate/cn-sync"},
		{http.MethodPost, "/api/translate/ai"},
		{http.MethodPost, "/api/translate/ai-all"},
		{http.MethodPost, "/api/translate/ai-story"},
		{http.MethodPost, "/api/editor/v1/backup/push"},
		{http.MethodGet, "/api/lyrics/source/search"},
		{http.MethodPost, "/api/lyrics/source/preview"},
		{http.MethodGet, "/api/admin/lyrics-source-reviews"},
		{http.MethodGet, "/api/admin/lyrics-source-reviews/detail?reviewId=1"},
		{http.MethodPost, "/api/admin/lyrics-source-reviews/import"},
		{http.MethodPut, "/api/admin/lyrics-source-reviews/decision"},
		{http.MethodPut, "/api/admin/lyrics-source-reviews/candidate-selection"},
		{http.MethodPost, "/api/editor/v1/stories/sync"},
		{http.MethodPost, "/api/editor/v1/story/card/1/ai"},
		{http.MethodPost, "/api/editor/v1/story/area/areatalk_01/refresh"},
	} {
		response := doJSON(t, operation.method, h.server.URL+operation.path, token, map[string]any{})
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("editor %s status = %d", operation.path, response.StatusCode)
		}
	}
}

func TestGETRefreshNeverRotatesToken(t *testing.T) {
	h := setupLegacyAPI(t)
	refresh := doJSON(t, http.MethodGet, h.server.URL+"/api/auth/refresh", h.token, nil)
	refresh.Body.Close()
	if refresh.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET refresh status = %d", refresh.StatusCode)
	}
	me := doJSON(t, http.MethodGet, h.server.URL+"/api/auth/me", h.token, nil)
	me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("GET refresh consumed the token; /me status = %d", me.StatusCode)
	}
}

func TestStaleAdminTokensFailAuthentication(t *testing.T) {
	for _, test := range []struct {
		name   string
		revoke func(*legacyAPIHarness) error
	}{
		{name: "role", revoke: func(h *legacyAPIHarness) error { return h.api.auth.SetRole("alice", auth.RoleEditor) }},
		{name: "password", revoke: func(h *legacyAPIHarness) error { return h.api.auth.SetPassword("alice", "new-password") }},
		{name: "delete", revoke: func(h *legacyAPIHarness) error { return h.api.auth.DeleteUser("alice") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setupLegacyAPI(t)
			if _, err := h.api.auth.CreateUser("backup-admin", "strong-password-456", auth.RoleAdmin); err != nil {
				t.Fatal(err)
			}
			if err := test.revoke(h); err != nil {
				t.Fatal(err)
			}
			response := doJSON(t, http.MethodGet, h.server.URL+"/api/admin/users", h.token, nil)
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("stale %s token status = %d", test.name, response.StatusCode)
			}
		})
	}
}

func TestQueryTokenAuthenticationIsRejectedEverywhere(t *testing.T) {
	h := setupLegacyAPI(t)
	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "normal API", method: http.MethodGet, path: "/api/categories"},
		{name: "admin API", method: http.MethodGet, path: "/api/admin/users"},
		{name: "review API", method: http.MethodPut, path: "/api/admin/lyrics-source-reviews/decision"},
		{name: "strict API", method: http.MethodPut, path: "/api/editor/v1/entry"},
		{name: "side story API", method: http.MethodGet, path: "/api/editor/v1/stories?kind=card"},
		{name: "side story mutation", method: http.MethodPut, path: "/api/editor/v1/story/card/1/1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, h.server.URL+test.path+"?token="+h.token, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("query token status = %d", response.StatusCode)
			}
		})
	}

	response, err := http.Get(h.server.URL + "/sse?token=" + h.token)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("SSE query token status = %d", response.StatusCode)
	}

	bearer := bearerSSERequest(t, h.server.URL, h.token)
	authorized, err := http.DefaultClient.Do(bearer)
	if err != nil {
		t.Fatal(err)
	}
	defer authorized.Body.Close()
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("SSE bearer status = %d", authorized.StatusCode)
	}
	if got := authorized.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("SSE Content-Type = %q", got)
	}
}

func TestPublicAuthAttemptLimitsUseRemoteAddrAndAccount(t *testing.T) {
	t.Run("account across addresses", func(t *testing.T) {
		h := setupLegacyAPI(t)
		h.api.authAttempts = newAuthAttemptLimiter(2, time.Minute, 128)
		for attempt := 0; attempt < 3; attempt++ {
			response := invokePublicAuth(t, h.api.handleLogin, "203.0.113."+strconv.Itoa(attempt+1)+":1234", "198.51.100."+strconv.Itoa(attempt+1), map[string]string{
				"username": "alice", "password": "wrong",
			})
			want := http.StatusUnauthorized
			if attempt == 2 {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("account attempt %d status = %d", attempt, response.Code)
			}
		}
	})

	t.Run("address ignores forwarding headers", func(t *testing.T) {
		h := setupLegacyAPI(t)
		h.api.authAttempts = newAuthAttemptLimiter(2, time.Minute, 128)
		for attempt, username := range []string{"missing-1", "missing-2", "missing-3"} {
			response := invokePublicAuth(t, h.api.handleLogin, "203.0.113.10:4321", "198.51.100."+strconv.Itoa(attempt+1), map[string]string{
				"username": username, "password": "wrong",
			})
			want := http.StatusUnauthorized
			if attempt == 2 {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("IP attempt %d status = %d", attempt, response.Code)
			}
		}
	})

	t.Run("setup", func(t *testing.T) {
		h := setupLegacyAPI(t)
		h.api.authAttempts = newAuthAttemptLimiter(2, time.Minute, 128)
		for attempt := 0; attempt < 3; attempt++ {
			response := invokePublicAuth(t, h.api.handleSetup, "203.0.113.20:4321", "198.51.100.20", map[string]string{
				"username": "candidate", "password": "strong-password-123",
			})
			want := http.StatusConflict
			if attempt == 2 {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("setup attempt %d status = %d", attempt, response.Code)
			}
		}
	})
}

func TestPublicAuthAttemptLimiterBoundsTrackedKeys(t *testing.T) {
	limiter := newAuthAttemptLimiter(100, time.Hour, 4)
	for attempt := 0; attempt < 20; attempt++ {
		limiter.allow("login", "203.0.113."+strconv.Itoa(attempt)+":1234", "user-"+strconv.Itoa(attempt))
	}
	if len(limiter.attempts) > limiter.maxEntries {
		t.Fatalf("tracked keys = %d, max = %d", len(limiter.attempts), limiter.maxEntries)
	}
}

func TestPublicAuthAttemptLimiterPreservesCaseSensitiveAccountIdentity(t *testing.T) {
	limiter := newAuthAttemptLimiter(1, time.Minute, 8)
	if !limiter.allow("login", "203.0.113.1:1234", "Alice") {
		t.Fatal("first Alice attempt was blocked")
	}
	if !limiter.allow("login", "203.0.113.2:1234", "alice") {
		t.Fatal("distinct case-sensitive alice account shared Alice's bucket")
	}
	if limiter.allow("login", "203.0.113.3:1234", " Alice ") {
		t.Fatal("trim-equivalent Alice spelling bypassed its account limit")
	}
	if limiter.allow("login", "203.0.113.4:1234", "alice") {
		t.Fatal("alice account limit was not retained")
	}
	if len(limiter.attempts) != 4 {
		t.Fatalf("tracked keys = %d, want two IP and two account buckets", len(limiter.attempts))
	}
}

func TestPublicAuthAttemptLimiterDoesNotEvictActiveDenialsUnderChurn(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newAuthAttemptLimiter(2, time.Minute, 4)
	limiter.now = func() time.Time { return now }
	if !limiter.allow("login", "203.0.113.1:1234", "target") ||
		!limiter.allow("login", "203.0.113.1:1234", "target") {
		t.Fatal("target attempts were unexpectedly blocked before the limit")
	}
	if !limiter.allow("login", "203.0.113.2:1234", "filler") {
		t.Fatal("filler attempt was unexpectedly blocked")
	}
	for attempt := 0; attempt < 1000; attempt++ {
		limiter.allow("login", "198.51.100."+strconv.Itoa(attempt)+":4321", "churn-"+strconv.Itoa(attempt))
	}
	if limiter.allow("login", "203.0.113.1:9999", "target") {
		t.Fatal("capacity churn reset the denied target account")
	}
	if limiter.allow("login", "203.0.113.1:9999", "different-account") {
		t.Fatal("capacity churn reset the denied target IP")
	}
	if len(limiter.attempts) != limiter.maxEntries {
		t.Fatalf("tracked keys = %d, want %d", len(limiter.attempts), limiter.maxEntries)
	}
	now = now.Add(time.Minute)
	if !limiter.allow("login", "192.0.2.1:1234", "fresh") {
		t.Fatal("expired heap entries did not release capacity")
	}
}

func TestPublicAuthAttemptLimiterHashesLargeAccounts(t *testing.T) {
	limiter := newAuthAttemptLimiter(2, time.Minute, 4)
	largeAccount := strings.Repeat("a", maxJSONBodyBytes)
	if !limiter.allow("login", "203.0.113.1:1234", largeAccount) {
		t.Fatal("first large-account attempt was blocked")
	}
	if len(limiter.attempts) != 2 {
		t.Fatalf("large account created %d keys", len(limiter.attempts))
	}
	for key := range limiter.attempts {
		if len(key) != sha256.Size {
			t.Fatalf("attempt key size = %d", len(key))
		}
	}
}

func invokePublicAuth(t *testing.T, handler http.HandlerFunc, remoteAddr, forwardedFor string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	request.RemoteAddr = remoteAddr
	request.Header.Set("X-Forwarded-For", forwardedFor)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}
