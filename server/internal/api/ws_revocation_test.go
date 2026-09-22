package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

// A token generation change must also close the account's collaboration rooms:
// the Yjs layer otherwise revalidates its connections only every 20 seconds.
func TestTokenGenerationChangesCloseCollaborationRooms(t *testing.T) {
	h := setupLyricsCollabAPI(t)
	proof := h.legacy.api.editorGate.Status()
	response := collabTicketRequest(t, h, &proof, map[string]any{"musicId": 42})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ticket status = %d", response.StatusCode)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(response.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/yjs/lyrics/42?ticket=" + url.QueryEscape(ticket.Ticket)
	connection, upgrade, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if upgrade != nil {
			upgrade.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()

	refresh := doJSON(t, http.MethodPost, h.server.URL+"/api/auth/refresh", h.legacy.token, nil)
	refresh.Body.Close()
	if refresh.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d", refresh.StatusCode)
	}

	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			var readErr net.Error
			if errors.As(err, &readErr) && readErr.Timeout() {
				t.Fatal("token generation change left the collaboration room open")
			}
			break
		}
	}
}
