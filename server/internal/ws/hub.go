// Package ws provides a WebSocket hub for pushing realtime updates and
// editor gate status to the console.
package ws

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"moesekai/server/internal/editorgate"

	"golang.org/x/net/websocket"
)

// Event types broadcast to clients over WebSocket.
const (
	EventEntryUpdated       = "entry.updated"
	EventEntryLocaleUpdated = "entry.locale.updated"
	EventStoryUpdated       = "eventstory.updated"
	EventStoryLocaleUpdated = "eventstory.locale.updated"
	EventLyricsUpdated      = "lyrics.updated"
	EventSyncProgress       = "sync.progress"
	EventTranslateProgress  = "translate.progress"
	EventContentRestored    = "content.restored"
	EventGateStatus         = "gate.status"
	EventPing               = "ping"
	EventPong               = "pong"
)

// Message is a single WebSocket JSON payload.
type Message struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

type ClientMsg struct {
	Type string `json:"type"`
}

type client struct {
	id        uint64
	user      string
	ws        *websocket.Conn
	ch        chan Message
	done      chan struct{}
	closeOnce sync.Once
}

const (
	clientQueueSize          = 64
	maxHubClients            = 128
	maxHubClientsPerUser     = 8
	defaultHeartbeatInterval = 20 * time.Second
	defaultClientReadTimeout = 75 * time.Second
	websocketWriteTimeout    = 5 * time.Second
)

var (
	errHubClosed       = errors.New("WebSocket hub is closed")
	errHubCapacity     = errors.New("WebSocket connection limit reached")
	errUserHubCapacity = errors.New("WebSocket per-user connection limit reached")
)

// disconnect tears the connection down exactly once.
//
// c.ch is deliberately left open: the writer goroutine calls disconnect without
// holding h.mu, while Broadcast sends on c.ch under h.mu, so closing the channel
// here would race those sends. The writer already exits on c.done, and the
// channel is collected with the client.
func (c *client) disconnect() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// Hub manages active WebSocket connections and broadcasts events.
type Hub struct {
	mu                sync.RWMutex
	clients           map[uint64]*client
	nextID            atomic.Uint64
	closed            bool
	closeOnce         sync.Once
	editorGate        *editorgate.Gate
	heartbeatInterval time.Duration
	clientReadTimeout time.Duration
}

func NewHub(gate *editorgate.Gate) *Hub {
	return &Hub{
		clients:           map[uint64]*client{},
		editorGate:        gate,
		heartbeatInterval: defaultHeartbeatInterval,
		clientReadTimeout: defaultClientReadTimeout,
	}
}

// Broadcast sends a message to all connected WebSocket clients.
func (h *Hub) Broadcast(event string, data any) {
	msg := Message{Event: event, Data: data}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
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

// BroadcastGateStatus pushes the current editorgate.Status to all clients.
func (h *Hub) BroadcastGateStatus() {
	if h.editorGate == nil {
		return
	}
	h.Broadcast(EventGateStatus, h.editorGate.Status())
}

// ClientCount returns the number of active WebSocket connections.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) add(user string, ws *websocket.Conn) (*client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errHubClosed
	}
	if len(h.clients) >= maxHubClients {
		return nil, errHubCapacity
	}
	userClients := 0
	for _, existing := range h.clients {
		if existing.user == user {
			userClients++
		}
	}
	if userClients >= maxHubClientsPerUser {
		return nil, errUserHubCapacity
	}
	c := &client{
		id:   h.nextID.Add(1),
		user: user,
		ws:   ws,
		ch:   make(chan Message, clientQueueSize),
		done: make(chan struct{}),
	}
	h.clients[c.id] = c
	return c, nil
}

func (h *Hub) remove(id uint64) {
	h.mu.Lock()
	if c, ok := h.clients[id]; ok {
		delete(h.clients, id)
		c.disconnect()
	}
	h.mu.Unlock()
}

// RevokeUser closes WebSocket streams for a given user.
func (h *Hub) RevokeUser(user string) {
	h.mu.Lock()
	for _, c := range h.clients {
		if c.user == user {
			c.disconnect()
		}
	}
	h.mu.Unlock()
}

// Close disconnects all WebSocket clients.
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

// Handler handles WebSocket connections.
func (h *Hub) Handler(usernameFn func(*http.Request) string, validFn func(*http.Request) bool) http.HandlerFunc {
	wsServer := websocket.Server{
		Handshake: func(config *websocket.Config, req *http.Request) error {
			return nil
		},
		Handler: websocket.Handler(func(ws *websocket.Conn) {
			r := ws.Request()
			user := ""
			if usernameFn != nil {
				user = usernameFn(r)
			}
			c, err := h.add(user, ws)
			if err != nil {
				return
			}
			defer h.remove(c.id)

			// Immediately push current editor gate status upon connection.
			if h.editorGate != nil {
				c.ch <- Message{Event: EventGateStatus, Data: h.editorGate.Status()}
			}

			// Writer goroutine: sends queued messages to client over WS.
			go func() {
				ticker := time.NewTicker(h.heartbeatInterval)
				defer ticker.Stop()
				for {
					select {
					case <-c.done:
						return
					case msg := <-c.ch:
						_ = ws.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
						if err := websocket.JSON.Send(ws, msg); err != nil {
							c.disconnect()
							return
						}
					case <-ticker.C:
						if validFn != nil && !validFn(r) {
							c.disconnect()
							return
						}
						_ = ws.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
						if err := websocket.JSON.Send(ws, Message{Event: EventPing, Data: time.Now().Unix()}); err != nil {
							c.disconnect()
							return
						}
					}
				}
			}()

			// Reader goroutine (main WS handler context): the client answers each
			// outbound heartbeat with {"type":"pong"}. Any valid client message
			// refreshes the deadline, while pong itself has no application side effect.
			for {
				var clientMsg ClientMsg
				if err := ws.SetReadDeadline(time.Now().Add(h.clientReadTimeout)); err != nil {
					return
				}
				if err := websocket.JSON.Receive(ws, &clientMsg); err != nil {
					return
				}
				switch clientMsg.Type {
				case "check_sync", "ping":
					if h.editorGate != nil {
						select {
						case c.ch <- Message{Event: EventGateStatus, Data: h.editorGate.Status()}:
						default:
						}
					}
				case "pong":
					// Heartbeat acknowledgement only.
				}
			}
		}),
	}

	return func(w http.ResponseWriter, r *http.Request) {
		wsServer.ServeHTTP(w, r)
	}
}
