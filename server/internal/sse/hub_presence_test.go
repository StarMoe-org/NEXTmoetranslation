package sse

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPresenceSnapshotAndMembershipLifecycle(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(hub.Handler(func(r *http.Request) string { return r.Header.Get("X-Test-User") }, nil, nil, nil))
	defer server.Close()

	alice := openPresenceStream(t, server.URL, "alice")
	defer alice.response.Body.Close()
	assertPresenceEvent(t, alice.events, EventPresenceSnapshot, map[string]any{"users": []any{"alice"}})

	bob := openPresenceStream(t, server.URL, "bob")
	defer bob.response.Body.Close()
	assertPresenceEvent(t, bob.events, EventPresenceSnapshot, map[string]any{"users": []any{"alice", "bob"}})
	assertPresenceEvent(t, alice.events, EventPresenceJoined, map[string]any{"user": "bob", "users": []any{"alice", "bob"}})

	bob.response.Body.Close()
	assertPresenceEvent(t, alice.events, EventPresenceLeft, map[string]any{"user": "bob", "users": []any{"alice"}})
}

func TestPresenceDeduplicatesMultipleTabsForOneUser(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(hub.Handler(func(r *http.Request) string { return r.Header.Get("X-Test-User") }, nil, nil, nil))
	defer server.Close()

	alice := openPresenceStream(t, server.URL, "alice")
	defer alice.response.Body.Close()
	assertPresenceEvent(t, alice.events, EventPresenceSnapshot, map[string]any{"users": []any{"alice"}})

	second := openPresenceStream(t, server.URL, "alice")
	defer second.response.Body.Close()
	assertPresenceEvent(t, second.events, EventPresenceSnapshot, map[string]any{"users": []any{"alice"}})
	select {
	case event := <-alice.events:
		if event.name == EventPresenceJoined || event.name == EventPresenceLeft {
			t.Fatalf("duplicate tab changed presence: %#v", event)
		}
	case <-time.After(100 * time.Millisecond):
	}

	second.response.Body.Close()
	select {
	case event := <-alice.events:
		if event.name == EventPresenceLeft {
			t.Fatalf("closing one tab removed the user: %#v", event)
		}
	case <-time.After(100 * time.Millisecond):
	}

	alice.response.Body.Close()
	select {
	case event := <-alice.events:
		if event.name == EventPresenceLeft {
			t.Fatalf("closing the first of two tabs removed the user: %#v", event)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

type presenceStream struct {
	response *http.Response
	events   chan presenceEvent
}

type presenceEvent struct {
	name string
	data map[string]any
}

func openPresenceStream(t *testing.T, serverURL, user string) presenceStream {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, serverURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Test-User", user)
	request.Header.Set("X-SSE-Presence", "1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("presence status=%d", response.StatusCode)
	}
	events := make(chan presenceEvent, 8)
	go func() {
		reader := bufio.NewReader(response.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					events <- presenceEvent{name: event, data: data}
				}
			}
		}
	}()
	return presenceStream{response: response, events: events}
}

func assertPresenceEvent(t *testing.T, events <-chan presenceEvent, name string, want map[string]any) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-events:
			if got.name != name {
				continue
			}
			if got.name == EventPresenceSnapshot {
				users, ok := got.data["users"].([]any)
				if !ok || len(users) != len(want["users"].([]any)) {
					t.Fatalf("presence users=%#v want=%#v", got.data["users"], want["users"])
				}
			}
			if got.name == EventPresenceJoined || got.name == EventPresenceLeft {
				if got.data["user"] != want["user"] && want["user"] != nil {
					t.Fatalf("presence user=%v want=%v", got.data["user"], want["user"])
				}
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for presence event %q", name)
		}
	}
}
