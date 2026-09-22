// Package sse provides a Server-Sent Events hub for pushing realtime updates to
// the console: translation edits, sync/translate progress, event-story changes,
// and backup status. SSE is one-directional (server -> client), proxy- and
// CDN-friendly, and far simpler than WebSockets for this read-mostly use case.
package sse

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event types broadcast to clients. Kept as constants so the frontend and
// backend agree on the wire vocabulary.
const (
	EventEntryUpdated       = "entry.updated"
	EventEntryLocaleUpdated = "entry.locale.updated"
	EventStoryUpdated       = "eventstory.updated"
	EventStoryLocaleUpdated = "eventstory.locale.updated"
	EventLyricsUpdated      = "lyrics.updated"
	EventSyncProgress       = "sync.progress"
	EventTranslateProgress  = "translate.progress"
	EventContentRestored    = "content.restored"
	EventPresenceSnapshot   = "presence.snapshot"
	EventPresenceJoined     = "presence.joined"
	EventPresenceLeft       = "presence.left"
	EventGateStatus         = "gate.status"
	EventPing               = "ping"
)

// Message is a single SSE payload. Data is marshaled to JSON.
type Message struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

type client struct {
	id         uint64
	user       string
	ch         chan Message
	done       chan struct{}
	abortWrite func()
	closeOnce  sync.Once
}

const (
	clientQueueSize          = 32
	maxHubClients            = 128
	maxHubClientsPerUser     = 8
	defaultHeartbeatInterval = 5 * time.Second
)

var (
	errHubClosed       = errors.New("SSE hub is closed")
	errHubCapacity     = errors.New("SSE connection limit reached")
	errUserHubCapacity = errors.New("SSE per-user connection limit reached")
)

func (c *client) disconnect() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.abortWrite != nil {
			c.abortWrite()
		}
		close(c.ch)
	})
}

// Hub fans out messages to all connected clients.
type Hub struct {
	mu                sync.RWMutex
	clients           map[uint64]*client
	nextID            atomic.Uint64
	closed            bool
	closeOnce         sync.Once
	heartbeatInterval time.Duration
}

func NewHub() *Hub {
	return &Hub{clients: map[uint64]*client{}, heartbeatInterval: defaultHeartbeatInterval}
}

// Broadcast sends a message to every connected client. A client whose queue is
// full is disconnected: silently dropping a mutation hint would let that
// client continue editing from state it cannot know is stale.
func (h *Hub) Broadcast(event string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastLocked(Message{Event: event, Data: data}, 0)
}

func (h *Hub) broadcastLocked(msg Message, excludeID uint64) {
	for id, c := range h.clients {
		if id == excludeID {
			continue
		}
		select {
		case <-c.done:
			continue
		default:
		}
		select {
		case c.ch <- msg:
		default:
			c.disconnect()
		}
	}
}

// ClientCount returns the number of connected clients (for status/debug).
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) add(user string) (*client, error) {
	client, _, _, err := h.addWithAbort(user, nil)
	return client, err
}

func (h *Hub) addWithAbort(user string, abortWrite func()) (*client, []string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, false, errHubClosed
	}
	if len(h.clients) >= maxHubClients {
		return nil, nil, false, errHubCapacity
	}
	userClients := 0
	for _, existing := range h.clients {
		if existing.user != user {
			continue
		}
		select {
		case <-existing.done:
			continue
		default:
		}
		userClients++
	}
	if userClients >= maxHubClientsPerUser {
		return nil, nil, false, errUserHubCapacity
	}
	c := &client{id: h.nextID.Add(1), user: user, ch: make(chan Message, clientQueueSize), done: make(chan struct{}), abortWrite: abortWrite}
	h.clients[c.id] = c
	return c, h.onlineUsersLocked(), userClients == 0, nil
}

func (h *Hub) onlineUsersLocked() []string {
	seen := make(map[string]struct{}, len(h.clients))
	users := make([]string, 0, len(h.clients))
	for _, c := range h.clients {
		if c.user == "" {
			continue
		}
		select {
		case <-c.done:
			continue
		default:
		}
		if _, ok := seen[c.user]; ok {
			continue
		}
		seen[c.user] = struct{}{}
		users = append(users, c.user)
	}
	sort.Strings(users)
	return users
}

// Close promptly disconnects all streams and rejects new ones. It is safe to
// call more than once during overlapping shutdown paths.
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		for _, c := range h.clients {
			c.disconnect()
		}
		h.mu.Unlock()
	})
}

func (h *Hub) remove(id uint64) {
	h.mu.Lock()
	c, ok := h.clients[id]
	if ok {
		delete(h.clients, id)
		c.disconnect()
	}
	users := h.onlineUsersLocked()
	closed := h.closed
	h.mu.Unlock()
	if ok && c.user != "" && !closed && !containsUser(users, c.user) {
		h.Broadcast(EventPresenceLeft, map[string]any{"user": c.user, "users": users})
	}
}

func containsUser(users []string, user string) bool {
	for _, candidate := range users {
		if candidate == user {
			return true
		}
	}
	return false
}

// RevokeUser immediately closes every live stream for an account whose token
// generation or role changed.
func (h *Hub) RevokeUser(user string) {
	h.mu.Lock()
	for _, c := range h.clients {
		if c.user == user {
			c.disconnect()
		}
	}
	h.mu.Unlock()
}

// Handler streams events to one client over SSE. The caller must wrap this with
// auth middleware (the username is read from the request context if present).
// The initial roster snapshot is written privately; joined/left notifications
// are sent only when a username gains or loses its last active connection.
// initialFn supplies state a fresh client cannot reconstruct from later
// broadcasts alone; it is written privately right after the roster snapshot.
func (h *Hub) Handler(usernameFn func(*http.Request) string, validFn func(*http.Request) bool, expiresAtFn func(*http.Request) time.Time, initialFn func() []Message) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		user := ""
		if usernameFn != nil {
			user = usernameFn(r)
		}
		controller := http.NewResponseController(w)
		c, users, firstForUser, err := h.addWithAbort(user, func() {
			_ = controller.SetWriteDeadline(time.Now())
		})
		if err != nil {
			if errors.Is(err, errHubCapacity) || errors.Is(err, errUserHubCapacity) {
				w.Header().Set("Retry-After", "3")
				http.Error(w, "too many SSE connections", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		defer h.remove(c.id)
		h.setSSEHeaders(w)

		// Send a private roster snapshot first. Presence contains usernames only.
		if user != "" && strings.TrimSpace(r.Header.Get("X-SSE-Presence")) == "1" {
			if !writeEvent(w, Message{Event: EventPresenceSnapshot, Data: map[string]any{"users": users}}) {
				return
			}
			flusher.Flush()
		}

		if initialFn != nil {
			for _, msg := range initialFn() {
				if !writeEvent(w, msg) {
					return
				}
			}
			flusher.Flush()
		}

		// Initial comment + retry hint so the browser reconnects quickly.
		if _, err := fmt.Fprintf(w, ": connected\nretry: 3000\n\n"); err != nil {
			return
		}
		flusher.Flush()
		if user != "" && firstForUser {
			h.mu.Lock()
			if !h.closed {
				h.broadcastLocked(Message{Event: EventPresenceJoined, Data: map[string]any{"user": user, "users": h.onlineUsersLocked()}}, c.id)
			}
			h.mu.Unlock()
		}

		// Keep the heartbeat below the shortest known 15-second intermediary idle
		// timeout so quiet streams remain live without a reconciliation cycle.
		heartbeatInterval := h.heartbeatInterval
		if heartbeatInterval <= 0 {
			heartbeatInterval = defaultHeartbeatInterval
		}
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		var expiryTimer *time.Timer
		var expiry <-chan time.Time
		if expiresAtFn != nil {
			if expiresAt := expiresAtFn(r); !expiresAt.IsZero() {
				delay := time.Until(expiresAt)
				if delay < 0 {
					delay = 0
				}
				expiryTimer = time.NewTimer(delay)
				expiry = expiryTimer.C
				defer expiryTimer.Stop()
			}
		}

		ctx := r.Context()
		for {
			// Prefer disconnect over draining a closed client's buffered queue.
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			default:
			}
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-expiry:
				return
			case <-ticker.C:
				if validFn != nil && !validFn(r) {
					return
				}
				if !writeEvent(w, Message{Event: EventPing, Data: time.Now().Unix()}) {
					return
				}
				flusher.Flush()
			case msg, ok := <-c.ch:
				if !ok {
					return
				}
				select {
				case <-c.done:
					return
				default:
				}
				if validFn != nil && !validFn(r) {
					return
				}
				if !writeEvent(w, msg) {
					return
				}
				flusher.Flush()
			}
		}
	}
}

func (h *Hub) setSSEHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-cache, no-transform, no-store, must-revalidate")
	hd.Set("Pragma", "no-cache")
	hd.Set("Expires", "0")
	hd.Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx, EdgeOne, etc.) so events flush immediately.
	hd.Set("X-Accel-Buffering", "no")
}

// writeEvent serializes one SSE frame. Returns false if the write failed.
func writeEvent(w http.ResponseWriter, msg Message) bool {
	payload, err := json.Marshal(msg.Data)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, payload); err != nil {
		return false
	}
	return true
}
