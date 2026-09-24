// Package api wires the HTTP routes for the console (JWT-authenticated, no-cache)
// API. Public, cacheable file serving lives in package filesvc and is mounted
// separately under /files/*.
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/collab"
	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
	"moesekai/server/internal/upstream"
)

type lyricsSourceClient interface {
	Search(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error)
	Preview(context.Context, lyricssource.MusicIdentity, int, int) (lyricssource.Preview, error)
}

// Server holds the dependencies shared by all console handlers.
type Server struct {
	store                   *store.Store
	eventStore              *store.EventStore
	auth                    *auth.Auth
	cfg                     *config.Config
	hub                     *sse.Hub
	translator              *translator.Translator
	upstream                *upstream.Watcher
	backup                  *backup.Manager
	lyricsSrc               lyricsSourceClient
	lyricsRegistry          *lyricsSourceRegistry
	lyricsProviderMu        sync.Mutex
	authAttempts            *authAttemptLimiter
	editorGate              *editorgate.Gate
	lyricsImportMu          sync.Mutex
	lyricsImports           map[string]lyricsImportGrant
	lyricsInvariantReporter func(string, ...any)
	projection              interface {
		Status() filesvc.ProjectionStatus
	}
	fileService publicFileService
	search      interface {
		Status() searchindex.Status
	}
	collab      *collab.Service
	sideStories SideStoryRunner
}

// publicFileService publishes single-entity changes to the public files ahead
// of the debounced full rebuild.
type publicFileService interface {
	RebuildEvent(eventID int) error
	RebuildCategory(category string) error
	RebuildSideStory(kind, storyID string) error
	PublishNow()
	Status() filesvc.ProjectionStatus
	SongProvenance(musicID int) (filesvc.SongProvenance, bool)
}

func (s *Server) SetCollab(service *collab.Service) {
	s.collab = service
}

func (s *Server) SetSearchStatus(provider interface {
	Status() searchindex.Status
}) {
	s.search = provider
}

// SetProjectionStatus connects the authenticated status API to the public file
// service without changing existing Server construction contracts.
func (s *Server) SetProjectionStatus(provider interface {
	Status() filesvc.ProjectionStatus
}) {
	s.projection = provider
	if fs, ok := provider.(publicFileService); ok {
		s.fileService = fs
	}
}

func (s *Server) SetFileService(fs publicFileService) {
	s.fileService = fs
	s.projection = fs
}

// SetSideStoryRunner connects the card-story and area-talk routes that fetch
// upstream scripts or run AI; without one they answer 503.
func (s *Server) SetSideStoryRunner(runner SideStoryRunner) {
	s.sideStories = runner
}

func (s *Server) rebuildEventAsset(eventID int) {
	if s.fileService != nil {
		if err := s.fileService.RebuildEvent(eventID); err != nil {
			log.Printf("[filesvc] rebuild event %d failed: %v", eventID, err)
		}
	}
}

func (s *Server) rebuildSideStoryAsset(kind, storyID string) {
	if s.fileService != nil {
		if err := s.fileService.RebuildSideStory(kind, storyID); err != nil {
			log.Printf("[filesvc] rebuild side story %s/%s failed: %v", kind, storyID, err)
		}
	}
}

func (s *Server) rebuildCategoryAsset(category string) {
	if s.fileService != nil {
		if err := s.fileService.RebuildCategory(category); err != nil {
			log.Printf("[filesvc] rebuild category %s failed: %v", category, err)
		}
	}
}

// broadcast sends an SSE event if a hub is configured (it may be nil in tests).
func (s *Server) broadcast(event string, data any) {
	if s.hub != nil {
		s.hub.Broadcast(event, data)
	}
}

// revokeUser closes an account's live streams and its collaboration rooms after
// its token generation changed. The hub and the collaboration service may be
// nil in tests.
func (s *Server) revokeUser(user string) {
	if s.hub != nil {
		s.hub.RevokeUser(user)
	}
	if s.collab != nil {
		s.collab.RevokeUser(user)
	}
}

func (s *Server) reportLyricsInvariant(format string, args ...any) {
	if s.lyricsInvariantReporter != nil {
		s.lyricsInvariantReporter(format, args...)
	}
}

func (s *Server) contentMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		releaseEditor, err := s.editorGate.BeginEditorContext(r.Context())
		if err != nil {
			writeEditorAdmissionError(w, err)
			return
		}
		defer releaseEditor()
		release, err := s.store.LockContentSharedContext(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "request canceled")
			return
		}
		defer release()
		next(w, r)
	}
}

// getOnly rejects write methods on read-only endpoints, which otherwise answer
// 200 to any method because their handlers never inspect r.Method.
func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next(w, r)
	}
}

func writeEditorAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, editorgate.ErrProducerRunning) {
		writeErr(w, http.StatusConflict, "producer is running; reload before saving")
		return
	}
	writeErr(w, http.StatusServiceUnavailable, "request canceled")
}

// currentUser returns the authenticated username, or "" if unauthenticated.
func currentUser(r *http.Request) string {
	if claims, ok := auth.FromContext(r.Context()); ok {
		return claims.Username
	}
	return ""
}
