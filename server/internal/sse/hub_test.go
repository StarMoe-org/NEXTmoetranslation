package sse

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type blockingSSEWriter struct {
	header       http.Header
	writeStarted chan struct{}
	releaseWrite chan struct{}
}

func (w *blockingSSEWriter) Header() http.Header { return w.header }
func (w *blockingSSEWriter) WriteHeader(int)     {}
func (w *blockingSSEWriter) Flush()              {}
func (w *blockingSSEWriter) Write([]byte) (int, error) {
	select {
	case <-w.writeStarted:
	default:
		close(w.writeStarted)
	}
	<-w.releaseWrite
	return 0, io.ErrClosedPipe
}
func (w *blockingSSEWriter) SetWriteDeadline(time.Time) error {
	select {
	case <-w.releaseWrite:
	default:
		close(w.releaseWrite)
	}
	return nil
}

func TestStreamClosesAtTokenExpiry(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(hub.Handler(func(*http.Request) string { return "editor" },
		func(*http.Request) bool { return true }, func(*http.Request) time.Time { return time.Now().Add(40 * time.Millisecond) }, nil))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(response.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE stream survived token expiry")
	}
}

func TestHeartbeatArrivesBeforeShortIntermediaryIdleTimeout(t *testing.T) {
	if defaultHeartbeatInterval != 5*time.Second {
		t.Fatalf("default heartbeat = %s, want 5s", defaultHeartbeatInterval)
	}
	hub := NewHub()
	hub.heartbeatInterval = 10 * time.Millisecond
	server := httptest.NewServer(hub.Handler(nil, nil, nil, nil))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("SSE content type = %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache, no-transform, no-store, must-revalidate" {
		t.Fatalf("SSE cache control = %q", got)
	}
	if response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Expires") != "0" {
		t.Fatalf("SSE legacy cache headers = %#v", response.Header)
	}
	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	deadline := time.After(time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE stream closed before heartbeat")
			}
			if line == "event: "+EventPing {
				return
			}
		case <-deadline:
			t.Fatal("SSE heartbeat was not flushed")
		}
	}
}

func TestCloseDisconnectsStreamsAndRejectsNewClients(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(hub.Handler(nil, nil, nil, nil))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if hub.ClientCount() != 1 {
		t.Fatalf("connected clients = %d", hub.ClientCount())
	}
	closed := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(response.Body)
		close(closed)
	}()
	hub.Close()
	hub.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("SSE stream remained open after hub close")
	}
	newResponse, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	newResponse.Body.Close()
	if newResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new stream status after close = %d", newResponse.StatusCode)
	}
}

func TestQueueOverflowDisconnectsSlowClientInsteadOfDropping(t *testing.T) {
	hub := NewHub()
	client, err := hub.add("slow-editor")
	if err != nil {
		t.Fatalf("client was not added: %v", err)
	}
	for index := 0; index <= clientQueueSize; index++ {
		hub.Broadcast(EventEntryUpdated, map[string]int{"revision": index})
	}
	if hub.ClientCount() != 1 {
		t.Fatalf("overflowed client was removed before handler cleanup: clients=%d", hub.ClientCount())
	}
	for range client.ch {
	}
	if _, open := <-client.ch; open {
		t.Fatal("overflowed client queue remained open")
	}
	select {
	case <-client.done:
	default:
		t.Fatal("overflow did not immediately cancel the client")
	}
	hub.remove(client.id)
	if hub.ClientCount() != 0 {
		t.Fatalf("removed overflow client count = %d", hub.ClientCount())
	}
}

func TestOverflowInterruptsBlockedTransportWrite(t *testing.T) {
	hub := NewHub()
	writer := &blockingSSEWriter{
		header: http.Header{}, writeStarted: make(chan struct{}), releaseWrite: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		hub.Handler(func(*http.Request) string { return "slow-editor" }, nil, nil, nil)(writer, httptest.NewRequest(http.MethodGet, "/sse", nil))
		close(done)
	}()
	select {
	case <-writer.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not enter blocked write")
	}
	for index := 0; index <= clientQueueSize; index++ {
		hub.Broadcast(EventEntryUpdated, index)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("overflow did not interrupt blocked SSE transport")
	}
	if hub.ClientCount() != 0 {
		t.Fatalf("blocked transport client remained counted: %d", hub.ClientCount())
	}
}

func TestHubEnforcesPerUserAndGlobalConnectionLimits(t *testing.T) {
	hub := NewHub()
	for index := 0; index < maxHubClientsPerUser; index++ {
		if _, err := hub.add("same-user"); err != nil {
			t.Fatalf("connection %d rejected: %v", index, err)
		}
	}
	if _, err := hub.add("same-user"); !errors.Is(err, errUserHubCapacity) {
		t.Fatalf("per-user overflow error = %v", err)
	}
	for index := maxHubClientsPerUser; index < maxHubClients; index++ {
		if _, err := hub.add(fmt.Sprintf("user-%d", index)); err != nil {
			t.Fatalf("global connection %d rejected early: %v", index, err)
		}
	}
	if _, err := hub.add("one-more-user"); !errors.Is(err, errHubCapacity) {
		t.Fatalf("global overflow error = %v", err)
	}
}

func TestHandlerWritesInitialEventsOnConnect(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(hub.Handler(nil, nil, nil, func() []Message {
		return []Message{{Event: EventGateStatus, Data: map[string]any{"running": true}}}
	}))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	event, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	data, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(event) != "event: "+EventGateStatus || strings.TrimSpace(data) != `data: {"running":true}` {
		t.Fatalf("initial frame = %q %q", event, data)
	}
}

// The console matches the side-story event names literally.
func TestSideStoryEventNamesMatchTheConsoleVocabulary(t *testing.T) {
	if EventSideStoryUpdated != "sidestory.updated" || EventSideStorySync != "sidestory.sync" {
		t.Fatalf("side-story events = %q, %q", EventSideStoryUpdated, EventSideStorySync)
	}
}
