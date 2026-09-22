package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A token generation change must close the account's live SSE streams, not only
// reject its next request: otherwise a deleted account keeps receiving
// broadcasts until the next heartbeat revalidation.
func TestTokenGenerationChangesRevokeLiveStreams(t *testing.T) {
	h := setupLegacyAPI(t)
	created := doJSON(t, http.MethodPost, h.server.URL+"/api/admin/users", h.token, map[string]string{
		"username": "bob", "password": "another-strong-password", "role": "editor",
	})
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create user status = %d", created.StatusCode)
	}

	for _, step := range []struct {
		name     string
		password string
		do       func() *http.Response
	}{
		{"password change", "another-strong-password", func() *http.Response {
			return doJSON(t, http.MethodPut, h.server.URL+"/api/admin/users", h.token, map[string]string{
				"username": "bob", "password": "third-strong-password",
			})
		}},
		{"role change", "third-strong-password", func() *http.Response {
			return doJSON(t, http.MethodPut, h.server.URL+"/api/admin/users", h.token, map[string]string{
				"username": "bob", "role": "admin",
			})
		}},
		{"delete", "third-strong-password", func() *http.Response {
			return doJSON(t, http.MethodDelete, h.server.URL+"/api/admin/users?username=bob", h.token, nil)
		}},
		{"refresh", "", func() *http.Response {
			return doJSON(t, http.MethodPost, h.server.URL+"/api/auth/refresh", h.token, nil)
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			token := h.token
			if step.password != "" {
				token = loginToken(t, h, "bob", step.password)
			}
			stream, err := http.DefaultClient.Do(bearerSSERequest(t, h.server.URL, token))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			if stream.StatusCode != http.StatusOK {
				t.Fatalf("SSE status = %d", stream.StatusCode)
			}
			closed := make(chan struct{})
			go func() {
				_, _ = io.ReadAll(stream.Body)
				close(closed)
			}()

			response := step.do()
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s status = %d", step.name, response.StatusCode)
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s left the revoked account's SSE stream open", step.name)
			}
		})
	}
}

func loginToken(t *testing.T, h *legacyAPIHarness, username, password string) string {
	t.Helper()
	response := doJSON(t, http.MethodPost, h.server.URL+"/api/auth/login", "", map[string]string{
		"username": username, "password": password,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login %s status = %d", username, response.StatusCode)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	return login.Token
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
