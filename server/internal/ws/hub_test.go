package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/editorgate"

	"golang.org/x/net/websocket"
)

func TestWebSocketHubBroadcastAndGateStatus(t *testing.T) {
	gate := editorgate.MustNew()
	hub := NewHub(gate)
	defer hub.Close()

	handler := hub.Handler(func(_ *http.Request) string { return "testuser" }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, err := websocket.Dial(wsURL, "", "http://localhost/")
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	defer ws.Close()

	// Expect initial gate.status event immediately upon connection.
	var initMsg Message
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Receive(ws, &initMsg); err != nil {
		t.Fatalf("failed to receive initial gate.status: %v", err)
	}
	if initMsg.Event != EventGateStatus {
		t.Fatalf("got event %q, want %q", initMsg.Event, EventGateStatus)
	}

	// Test broadcast
	hub.Broadcast(EventEntryUpdated, map[string]string{"key": "test_key"})

	var bcastMsg Message
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Receive(ws, &bcastMsg); err != nil {
		t.Fatalf("failed to receive broadcast event: %v", err)
	}
	if bcastMsg.Event != EventEntryUpdated {
		t.Fatalf("got event %q, want %q", bcastMsg.Event, EventEntryUpdated)
	}
}

func TestWebSocketHubPongKeepsConnectionAliveWithoutGateStatus(t *testing.T) {
	gate := editorgate.MustNew()
	hub := NewHub(gate)
	hub.heartbeatInterval = 10 * time.Millisecond
	hub.clientReadTimeout = 150 * time.Millisecond
	defer hub.Close()

	handler := hub.Handler(func(_ *http.Request) string { return "heartbeat-user" }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, err := websocket.Dial(wsURL, "", "http://localhost/")
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	defer ws.Close()

	var initial Message
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	if err := websocket.JSON.Receive(ws, &initial); err != nil {
		t.Fatalf("failed to receive initial gate status: %v", err)
	}
	if initial.Event != EventGateStatus {
		t.Fatalf("initial event = %q, want %q", initial.Event, EventGateStatus)
	}

	// Reply to enough heartbeats to cross the configured read timeout. A pong
	// must extend connection liveness without producing another gate status.
	pingCount := 0
	deadline := time.Now().Add(2 * time.Second)
	for pingCount < 20 {
		var msg Message
		_ = ws.SetReadDeadline(deadline)
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			t.Fatalf("connection closed while acknowledging heartbeat %d: %v", pingCount, err)
		}
		switch msg.Event {
		case EventGateStatus:
			t.Fatal("pong unexpectedly triggered gate.status")
		case EventPing:
			if err := websocket.JSON.Send(ws, ClientMsg{Type: "pong"}); err != nil {
				t.Fatalf("failed to send pong %d: %v", pingCount, err)
			}
			pingCount++
		}
	}

	hub.Broadcast(EventEntryUpdated, map[string]string{"key": "after-heartbeats"})
	deadline = time.Now().Add(2 * time.Second)
	for {
		var msg Message
		_ = ws.SetReadDeadline(deadline)
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			t.Fatalf("silent connection did not receive broadcast after heartbeats: %v", err)
		}
		if msg.Event == EventEntryUpdated {
			break
		}
		if msg.Event == EventGateStatus {
			t.Fatal("pong unexpectedly triggered gate.status before broadcast")
		}
		if msg.Event == EventPing {
			if err := websocket.JSON.Send(ws, ClientMsg{Type: "pong"}); err != nil {
				t.Fatalf("failed to send pong while awaiting broadcast: %v", err)
			}
		}
	}
}

func TestWebSocketHubDisconnectsClientThatDoesNotPong(t *testing.T) {
	hub := NewHub(nil)
	hub.heartbeatInterval = 10 * time.Millisecond
	hub.clientReadTimeout = 60 * time.Millisecond
	defer hub.Close()

	handler := hub.Handler(func(_ *http.Request) string { return "silent-user" }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, err := websocket.Dial(wsURL, "", "http://localhost/")
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	defer ws.Close()

	deadline := time.Now().Add(time.Second)
	pingCount := 0
	for {
		var msg Message
		_ = ws.SetReadDeadline(deadline)
		err := websocket.JSON.Receive(ws, &msg)
		if err != nil {
			break
		}
		if msg.Event == EventPing {
			pingCount++
		}
	}
	if pingCount == 0 {
		t.Fatal("silent client was disconnected before receiving a heartbeat")
	}
}

// A broadcast that races the writer goroutine's own teardown must not panic.
// disconnect() runs without h.mu (writer goroutine) while Broadcast sends on
// c.ch under h.mu, so closing c.ch in disconnect made this a send on a closed
// channel -- fatal, and unrecovered when Broadcast is called from a background
// goroutine such as the upstream sync callback.
func TestWebSocketHubBroadcastRacesClientDisconnect(t *testing.T) {
	hub := NewHub(nil)
	defer hub.Close()

	handler := hub.Handler(func(_ *http.Request) string { return "racer" }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	for i := 0; i < maxHubClientsPerUser; i++ {
		conn, err := websocket.Dial(wsURL, "", "http://localhost/")
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer conn.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount() < maxHubClientsPerUser && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if hub.ClientCount() == 0 {
		t.Fatal("no clients registered")
	}

	const rounds = 300
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			hub.Broadcast(EventEntryUpdated, map[string]string{"key": "racing"})
		}
	}()

	for i := 0; i < rounds; i++ {
		hub.mu.RLock()
		targets := make([]*client, 0, len(hub.clients))
		for _, c := range hub.clients {
			targets = append(targets, c)
		}
		hub.mu.RUnlock()
		for _, c := range targets {
			c.disconnect()
		}
	}

	wg.Wait()
}
