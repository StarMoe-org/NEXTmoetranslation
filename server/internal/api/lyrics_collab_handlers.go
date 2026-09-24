package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/collab"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func (s *Server) handleLyricsCollabTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.collab == nil {
		writeErr(w, http.StatusServiceUnavailable, "collaboration unavailable")
		return
	}
	musicID, ok := positivePathInt(w, r, "musicId")
	if !ok {
		return
	}
	var request struct {
		MusicID int `json:"musicId"`
	}
	if err := decodeOptional(r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if request.MusicID != 0 && request.MusicID != musicID {
		writeContractError(w, http.StatusUnprocessableEntity, "source_drift", []string{"body musicId must match the path"}, nil)
		return
	}
	claims, _ := auth.FromContext(r.Context())
	producerStatus, accepted := acceptedEditorStatus(r)
	if !accepted {
		writeErr(w, http.StatusPreconditionRequired, "loaded producer state required")
		return
	}
	ticket, err := s.collab.IssueTicket(r.Context(), claims, auth.BearerTokenFromRequest(r), musicID, producerStatus)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrLyricsNotFound):
			writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		case errors.Is(err, editorgate.ErrProducerRunning), errors.Is(err, collab.ErrAuthorityDrift):
			writeContractError(w, http.StatusConflict, "source_drift", []string{"reload lyrics before joining collaboration"}, nil)
		case errors.Is(err, collab.ErrTicketCapacity):
			writeContractError(w, http.StatusServiceUnavailable, "collaboration_capacity", nil, nil)
		default:
			writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}

func (s *Server) handleLyricsCheckpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.collab == nil {
		writeErr(w, http.StatusServiceUnavailable, "collaboration unavailable")
		return
	}
	musicID, ok := positivePathInt(w, r, "musicId")
	if !ok {
		return
	}
	var request struct {
		ClientID string `json:"clientId"`
	}
	if err := decodeOptional(r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	document, changed, err := s.collab.Checkpoint(r.Context(), musicID, currentUser(r))
	if err != nil {
		var conflict *collab.CheckpointConflict
		switch {
		case errors.As(err, &conflict):
			status := http.StatusUnprocessableEntity
			if conflict.Code == "revision_conflict" {
				status = http.StatusConflict
			}
			writeContractError(w, status, conflict.Code, conflict.Details, conflict.Current)
		case errors.Is(err, collab.ErrSchemaMismatch), errors.Is(err, collab.ErrDocumentMismatch):
			writeContractError(w, http.StatusUnprocessableEntity, "invalid_collaboration_document", nil, nil)
		case errors.Is(err, store.ErrLyricsNotFound), errors.Is(err, sql.ErrNoRows):
			writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		default:
			writeLyricsError(w, err)
		}
		return
	}
	if changed {
		switch value := document.(type) {
		case model.SongLyrics:
			s.broadcastLyricsDocumentUpdated(value.MusicID, value.Revision, request.ClientID, currentUser(r))
		case store.LyricsRenditionDocument:
			s.broadcastLyricsDocumentUpdated(value.MusicID, value.Revision, request.ClientID, currentUser(r))
			// A source-v3 save is served at once, as a rendition save is.
			if s.fileService != nil {
				s.fileService.PublishNow()
			}
		}
	}
	writeJSON(w, http.StatusOK, document)
}

func positivePathInt(w http.ResponseWriter, r *http.Request, key string) (int, bool) {
	raw := r.PathValue(key)
	if raw == "" {
		writeContractError(w, http.StatusBadRequest, "invalid_path", []string{key + " must be a positive integer"}, nil)
		return 0, false
	}
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil || value <= 0 || fmt.Sprintf("%d", value) != raw {
		writeContractError(w, http.StatusBadRequest, "invalid_path", []string{key + " must be a positive integer"}, nil)
		return 0, false
	}
	return value, true
}
