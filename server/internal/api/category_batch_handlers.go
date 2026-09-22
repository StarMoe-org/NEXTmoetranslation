package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/model"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
)

// handleCategorySnapshot returns every field in a category from one explicit
// locale transaction, together with the opaque batch-write base revision.
func (s *Server) handleCategorySnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	if !model.IsValidCategory(category) {
		writeContractError(w, http.StatusBadRequest, "invalid_category", []string{"supported category required"}, nil)
		return
	}
	locale, explicit, ok := requestLocale(w, r, "")
	if !ok {
		return
	}
	if !explicit {
		writeContractError(w, http.StatusBadRequest, "locale_required", []string{"explicit locale required"}, nil)
		return
	}
	snapshot, err := s.store.CategorySnapshotLocale(category, locale)
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleCategoryBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Category     string                      `json:"category"`
		Locale       string                      `json:"locale"`
		BaseRevision string                      `json:"baseRevision"`
		Updates      []model.CategoryEntryUpdate `json:"updates"`
		ClientID     string                      `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !model.IsValidCategory(req.Category) {
		writeContractError(w, http.StatusBadRequest, "invalid_category", []string{"supported category required"}, nil)
		return
	}
	locale, explicit, ok := requestLocale(w, r, req.Locale)
	if !ok {
		return
	}
	if !explicit {
		writeContractError(w, http.StatusBadRequest, "locale_required", []string{"explicit locale required"}, nil)
		return
	}
	if locale == model.LocaleJapanese {
		writeContractError(w, http.StatusBadRequest, "read_only_locale", []string{"locale is read-only"}, nil)
		return
	}
	if strings.TrimSpace(req.BaseRevision) == "" {
		writeContractError(w, http.StatusBadRequest, "base_revision_required", []string{"baseRevision required"}, nil)
		return
	}
	for _, update := range req.Updates {
		if !model.IsValidSource(update.Source) {
			writeContractError(w, http.StatusBadRequest, "invalid_source", []string{"unsupported translation source"}, nil)
			return
		}
	}
	result, err := s.store.UpdateCategoryLocale(req.Category, locale, req.BaseRevision, currentUser(r), req.Updates)
	if errors.Is(err, store.ErrCategoryRevisionConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "revision_conflict", "details": []string{"category changed; reload before saving"},
			"current": result.Snapshot,
		})
		return
	}
	if errors.Is(err, store.ErrEntryIdentityConflict) {
		writeContractError(w, http.StatusConflict, "identity_conflict", []string{"category entry identity changed; reload before saving"}, nil)
		return
	}
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}

	for _, changed := range result.Changed {
		payload := map[string]any{
			"category": req.Category, "field": changed.Field, "key": changed.Key,
			"text": changed.Text, "source": changed.Source, "locale": locale,
			"user": currentUser(r), "clientId": req.ClientID,
		}
		event := sse.EventEntryUpdated
		if locale != model.LocaleChinese {
			event = sse.EventEntryLocaleUpdated
		}
		s.broadcast(event, payload)
	}
	if len(result.Changed) > 0 {
		s.rebuildCategoryAsset(req.Category)
		// Music titles are copied into the public lyrics index, which only a
		// full rebuild regenerates.
		if s.fileService != nil && req.Category == "music" && changedCategoryFields(result.Changed)["title"] {
			s.fileService.PublishNow()
		}
	}
	type categoryBatchResponse struct {
		model.CategoryLocaleSnapshot
		Updated int `json:"updated"`
	}
	writeJSON(w, http.StatusOK, categoryBatchResponse{CategoryLocaleSnapshot: result.Snapshot, Updated: len(result.Changed)})
}

func changedCategoryFields(changed []model.CategoryEntryUpdate) map[string]bool {
	fields := make(map[string]bool, len(changed))
	for _, item := range changed {
		fields[item.Field] = true
	}
	return fields
}

type projectionStatusWithSongResponse struct {
	filesvc.ProjectionStatus
	Song *filesvc.SongProvenance `json:"song"`
}

func (s *Server) handleProjectionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.projection == nil {
		writeContractError(w, http.StatusServiceUnavailable, "projection_unavailable", nil, nil)
		return
	}

	musicIDStr := r.URL.Query().Get("musicId")
	if musicIDStr == "" {
		writeJSON(w, http.StatusOK, s.projection.Status())
		return
	}

	musicID, err := strconv.Atoi(musicIDStr)
	if err != nil || musicID <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid musicId")
		return
	}

	status := s.projection.Status()
	var song *filesvc.SongProvenance
	if s.fileService != nil {
		if p, ok := s.fileService.SongProvenance(musicID); ok {
			song = &p
		}
	} else if prov, ok := s.projection.(interface {
		SongProvenance(int) (filesvc.SongProvenance, bool)
	}); ok {
		if p, ok := prov.SongProvenance(musicID); ok {
			song = &p
		}
	}

	writeJSON(w, http.StatusOK, projectionStatusWithSongResponse{
		ProjectionStatus: status,
		Song:             song,
	})
}

func (s *Server) handleProjectionPublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.projection == nil {
		writeContractError(w, http.StatusServiceUnavailable, "projection_unavailable", nil, nil)
		return
	}
	if s.fileService != nil {
		s.fileService.PublishNow()
	} else if publisher, ok := s.projection.(interface{ PublishNow() }); ok {
		publisher.PublishNow()
	}
	writeJSON(w, http.StatusOK, s.projection.Status())
}

func (s *Server) handleSearchStatus(w http.ResponseWriter, r *http.Request) {
	if s.search == nil {
		writeContractError(w, http.StatusServiceUnavailable, "search_unavailable", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, s.search.Status())
}
