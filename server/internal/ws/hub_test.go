package ws

import (
	"bufio"
	"io"
	"net"
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

// hijackedResponseWriter hands a caller-owned net.Conn to the WebSocket
// handler, so the test decides when a server-side write completes.
type hijackedResponseWriter struct {
	conn   net.Conn
	rw     *bufio.ReadWriter
	header http.Header
}

func (w *hijackedResponseWriter) Header() http.Header { return w.header }

func (w *hijackedResponseWriter) Write(body []byte) (int, error) { return w.conn.Write(body) }

func (w *hijackedResponseWriter) WriteHeader(int) {}

func (w *hijackedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.rw, nil
}

// A stalled peer must not freeze the hub. ws.Close writes a close frame under
// the connection's write mutex, so a hub path that closes a client while
// holding h.mu queues every other hub operation -- including Broadcast to
// healthy clients and new /ws registrations -- behind that one socket write.
func TestWebSocketHubOperationsProceedDuringSlowClientClose(t *testing.T) {
	hub := NewHub(nil)
	defer hub.Close()

	// net.Pipe never buffers, so while nothing reads the client end every
	// server-side write blocks, including the close frame.
	serverEnd, clientEnd := net.Pipe()
	defer clientEnd.Close()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		reader := bufio.NewReader(serverEnd)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		writer := &hijackedResponseWriter{
			conn:   serverEnd,
			rw:     bufio.NewReadWriter(reader, bufio.NewWriter(serverEnd)),
			header: http.Header{},
		}
		hub.Handler(func(*http.Request) string { return "stalled" }, nil)(writer, request)
	}()

	config, err := websocket.NewConfig("ws://localhost/ws", "http://localhost/")
	if err != nil {
		t.Fatalf("websocket config: %v", err)
	}
	if _, err := websocket.NewClient(config, clientEnd); err != nil {
		t.Fatalf("websocket handshake: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	hub.mu.RLock()
	var stalled *client
	for _, c := range hub.clients {
		stalled = c
	}
	hub.mu.RUnlock()
	if stalled == nil {
		t.Fatal("stalled client was not registered")
	}

	revokeDone := make(chan struct{})
	go func() {
		defer close(revokeDone)
		hub.RevokeUser("stalled")
	}()

	probeDone := make(chan struct{})
	defer func() {
		// Let the close frame drain so the blocked teardown can finish.
		go func() { _, _ = io.Copy(io.Discard, clientEnd) }()
		<-revokeDone
		<-probeDone
		<-handlerDone
	}()

	// disconnect closes c.done immediately before the blocking ws.Close.
	select {
	case <-stalled.done:
	case <-time.After(5 * time.Second):
		t.Fatal("revoke never started tearing the stalled client down")
	}

	go func() {
		defer close(probeDone)
		hub.ClientCount()
		hub.Broadcast(EventEntryUpdated, map[string]string{"key": "healthy"})
	}()

	select {
	case <-probeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("hub operations blocked behind a slow client close")
	}
}
