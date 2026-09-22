package api

import (
	"net/http"

	"moesekai/server/internal/sse"
	"moesekai/server/internal/translator"
)

// handleTranslateStatus reports translator + connected-client state.
//
// GET /api/translate/status
func (s *Server) handleTranslateStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"translator": s.translator.Status()}
	if s.hub != nil {
		resp["clients"] = s.hub.ClientCount()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCNSync triggers a full CN data sync. The work runs synchronously (the
// translator's single-run lock prevents overlap) and progress streams via SSE.
//
// POST /api/translate/cn-sync
func (s *Server) handleCNSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	syncVersion := ""
	if s.upstream != nil {
		syncVersion = s.upstream.SyncStartVersion()
	}
	result, err := s.translator.SyncCNOnlyContext(r.Context())
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if translator.IsDraining(err) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if s.upstream != nil {
			s.upstream.RecordSyncResult(syncVersion, err)
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.upstream != nil {
		s.upstream.RecordSyncResult(syncVersion, result.SkippedError())
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTranslateAI fills one field's empty entries via the LLM.
//
// POST /api/translate/ai {category, field, provider, limit}
func (s *Server) handleTranslateAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req translator.AITranslateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	result, err := s.translator.ManualAITranslateContext(r.Context(), req)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if translator.IsDraining(err) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTranslateAIAll fills every event story's untranslated lines via the LLM.
//
// POST /api/translate/ai-all {provider}
func (s *Server) handleTranslateAIAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	result, err := s.translator.AITranslateAllContext(r.Context(), req.Provider)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if translator.IsDraining(err) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTranslateAIStory fills one event story's untranslated lines via the LLM.
//
// POST /api/translate/ai-story {eventId, provider, clientId}
func (s *Server) handleTranslateAIStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		EventID  int    `json:"eventId"`
		Provider string `json:"provider"`
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.EventID <= 0 {
		writeErr(w, http.StatusBadRequest, "eventId required")
		return
	}
	clientID, ok := validateEventClientID(w, req.ClientID)
	if !ok {
		return
	}
	result, err := s.translator.AITranslateStoryContext(r.Context(), req.EventID, req.Provider)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if translator.IsDraining(err) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast(sse.EventStoryUpdated, map[string]any{
		"eventId": req.EventID, "action": "ai-translate", "clientId": clientID, "user": currentUser(r),
	})
	writeJSON(w, http.StatusOK, result)
}

// handleRetryEventStory re-fetches one event story from remote.
//
// POST /api/event-story/retry {eventId, clientId}
func (s *Server) handleRetryEventStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, clientID, ok := decodeEventMutation(w, r)
	if !ok {
		return
	}
	result, err := s.translator.RetryEventStorySyncContext(r.Context(), id)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if translator.IsDraining(err) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast(sse.EventStoryUpdated, map[string]any{
		"eventId": id, "action": "retry", "clientId": clientID, "user": currentUser(r),
	})
	writeJSON(w, http.StatusOK, result)
}

// handleReorderEventStory re-fetches remote dialogue order for one event story.
//
// POST /api/event-story/reorder {eventId, clientId}
func (s *Server) handleReorderEventStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, clientID, ok := decodeEventMutation(w, r)
	if !ok {
		return
	}
	result, err := s.translator.ReorderEventStoryContext(r.Context(), id)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if translator.IsDraining(err) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast(sse.EventStoryUpdated, map[string]any{
		"eventId": id, "action": "reorder", "clientId": clientID, "user": currentUser(r),
	})
	writeJSON(w, http.StatusOK, result)
}
