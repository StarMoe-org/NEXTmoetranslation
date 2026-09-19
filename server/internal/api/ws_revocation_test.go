package api

import (
	"net/http"
	"sync"
	"testing"
)

type recordingWsHub struct {
	mu      sync.Mutex
	revoked []string
}

func (h *recordingWsHub) Broadcast(string, any) {}

func (h *recordingWsHub) BroadcastGateStatus() {}

func (h *recordingWsHub) RevokeUser(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revoked = append(h.revoked, user)
}

func (h *recordingWsHub) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.revoked...)
}

// A token generation change must close the account's WebSocket streams too, not
// only its SSE streams: otherwise a deleted account keeps receiving broadcasts
// until the next heartbeat revalidation.
func TestTokenGenerationChangesRevokeWebSocketStreams(t *testing.T) {
	h := setupLegacyAPI(t)
	hub := &recordingWsHub{}
	h.api.SetWsHub(hub)

	created := doJSON(t, http.MethodPost, h.server.URL+"/api/admin/users", h.token, map[string]string{
		"username": "bob", "password": "another-strong-password", "role": "editor",
	})
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create user status = %d", created.StatusCode)
	}

	for _, step := range []struct {
		name string
		do   func() *http.Response
		want string
	}{
		{"password change", func() *http.Response {
			return doJSON(t, http.MethodPut, h.server.URL+"/api/admin/users", h.token, map[string]string{
				"username": "bob", "password": "third-strong-password",
			})
		}, "bob"},
		{"role change", func() *http.Response {
			return doJSON(t, http.MethodPut, h.server.URL+"/api/admin/users", h.token, map[string]string{
				"username": "bob", "role": "admin",
			})
		}, "bob"},
		{"delete", func() *http.Response {
			return doJSON(t, http.MethodDelete, h.server.URL+"/api/admin/users?username=bob", h.token, nil)
		}, "bob"},
		{"refresh", func() *http.Response {
			return doJSON(t, http.MethodPost, h.server.URL+"/api/auth/refresh", h.token, nil)
		}, "alice"},
	} {
		before := len(hub.calls())
		response := step.do()
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", step.name, response.StatusCode)
		}
		calls := hub.calls()
		if len(calls) <= before {
			t.Fatalf("%s did not revoke any WebSocket stream", step.name)
		}
		if got := calls[len(calls)-1]; got != step.want {
			t.Fatalf("%s revoked %q, want %q", step.name, got, step.want)
		}
	}
}
