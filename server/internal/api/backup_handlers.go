package api

import (
	"errors"
	"net/http"
	"time"

	"moesekai/server/internal/backup"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/sse"
)

// handleBackupStatus reports backup/restore state.
//
// GET /api/backup/status
func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.backup.Status())
}

// handleBackupPush triggers an immediate backup to all enabled targets.
//
// POST /api/editor/v1/backup/push
func (s *Server) handleBackupPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup not configured")
		return
	}
	results, err := s.backup.BackupAllContext(r.Context())
	if err != nil {
		// Partial success still returns the per-target breakdown.
		writeJSON(w, backupErrorStatus(err), map[string]any{
			"error":   err.Error(),
			"results": results,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "results": results})
}

// handleBackupRestore restores translations from a target ("s3" or "git").
//
// POST /api/backup/restore {target, confirmation:"RESTORE:<target>"}
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup not configured")
		return
	}
	var req struct {
		Target       string `json:"target"`
		Confirmation string `json:"confirmation"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Target == "" {
		writeErr(w, http.StatusBadRequest, "target required (s3 or git)")
		return
	}
	if req.Target != "s3" && req.Target != "git" {
		writeErr(w, http.StatusBadRequest, "invalid restore target")
		return
	}
	if req.Confirmation != "RESTORE:"+req.Target {
		writeContractError(w, http.StatusBadRequest, "restore_confirmation_required",
			[]string{"confirmation must exactly match RESTORE:" + req.Target}, nil)
		return
	}
	res, err := s.backup.RestoreFromAsContext(r.Context(), req.Target, currentUser(r))
	if err != nil {
		writeErr(w, backupErrorStatus(err), err.Error())
		return
	}
	s.broadcast(sse.EventContentRestored, map[string]any{
		"target": req.Target, "user": currentUser(r), "restoredAt": time.Now().UTC().Format(time.RFC3339),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"categories":   res.Categories,
		"entries":      res.Entries,
		"eventStories": res.EventStories,
		"warnings":     res.Warnings,
	})
}

func backupErrorStatus(err error) int {
	switch {
	case errors.Is(err, backup.ErrBusy), errors.Is(err, editorgate.ErrProducerRunning):
		return http.StatusConflict
	case errors.Is(err, backup.ErrDraining), errors.Is(err, editorgate.ErrDraining):
		return http.StatusServiceUnavailable
	case errors.Is(err, backup.ErrInvalidRestoreTarget):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
