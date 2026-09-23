package api

import (
	"log"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
	"moesekai/server/internal/upstream"
)

// NewServer assembles the collaborators shared by the console handlers. The
// editor gate is variadic because tests construct a Server without one.
func NewServer(s *store.Store, es *store.EventStore, a *auth.Auth, cfg *config.Config, hub *sse.Hub, tr *translator.Translator, up *upstream.Watcher, bk *backup.Manager, gates ...*editorgate.Gate) *Server {
	gate := shareEditorGate(gates, tr, bk)
	registry, lyricsSrc := newLyricsSource(s)
	publishGateStatus(gate, hub)
	return &Server{
		store: s, eventStore: es, auth: a, cfg: cfg, hub: hub, translator: tr,
		upstream: up, backup: bk, lyricsSrc: lyricsSrc, lyricsRegistry: registry,
		authAttempts:            newAuthAttemptLimiter(10, 5*time.Minute, 8192),
		editorGate:              gate,
		lyricsImports:           map[string]lyricsImportGrant{},
		lyricsInvariantReporter: log.Printf,
	}
}

// shareEditorGate hands the caller's gate (or a new one) to every component that
// admits its own work, so they all read the same producer state.
func shareEditorGate(gates []*editorgate.Gate, tr *translator.Translator, bk *backup.Manager) *editorgate.Gate {
	var gate *editorgate.Gate
	if len(gates) > 0 {
		gate = gates[0]
	}
	if gate == nil {
		gate = editorgate.MustNew()
	}
	if tr != nil {
		tr.SetEditorGate(gate)
	}
	if bk != nil {
		bk.SetEditorGate(gate)
	}
	return gate
}

// newLyricsSource returns the swappable registry and the source the handlers
// read. The online editor tries Sekaipedia first, then the legacy fallback
// providers. The reviewed Sekaipedia authority is compiled in; its
// music-ID-to-page-title and contributor-alias maps are schema-v36 database data
// an admin can edit without a redeploy.
func newLyricsSource(s *store.Store) (*lyricsSourceRegistry, lyricsSourceClient) {
	var lyricsSrc lyricsSourceClient = lyricssource.New()
	registry := &lyricsSourceRegistry{}
	if storedRegistry, err := loadSekaipediaRegistry(s); err == nil {
		registry.set(storedRegistry)
		lyricsSrc = registry
	} else {
		log.Printf("[lyrics] source registry unavailable; falling back to vocaloid_fandom: %v", err)
	}
	return registry, lyricsSrc
}

// publishGateStatus streams producer transitions to connected clients: they are
// the one piece of editor state a client cannot derive from the mutation events
// it receives.
func publishGateStatus(gate *editorgate.Gate, hub *sse.Hub) {
	if hub == nil {
		return
	}
	gate.OnChange(func(status editorgate.Status) {
		hub.Broadcast(sse.EventGateStatus, status)
	})
}
