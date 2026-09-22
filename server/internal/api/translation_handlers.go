package api

import (
	"database/sql"
	"fmt"
	"net/http"

	"moesekai/server/internal/model"
	"moesekai/server/internal/sse"
)

// handleCategories returns per-field counts for all categories.
//
// GET /api/categories
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	locale, explicit, ok := requestLocale(w, r, "")
	if !ok {
		return
	}
	var cats []model.CategoryInfo
	var err error
	if explicit {
		cats, err = s.store.GetCategoriesLocale(locale)
	} else {
		cats, err = s.store.GetCategories()
	}
	if err != nil {
		writeLocaleInternalError(w, explicit, err)
		return
	}
	if cats == nil {
		cats = []model.CategoryInfo{}
	}
	writeJSON(w, http.StatusOK, cats)
}

// handleEntries returns entries for a category/field with optional source filter.
//
// GET /api/entries?category=&field=&source=
func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	field := r.URL.Query().Get("field")
	source := r.URL.Query().Get("source")
	if category == "" || field == "" {
		writeErr(w, http.StatusBadRequest, "category and field required")
		return
	}
	if !model.IsValidCategory(category) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported category: %s", category))
		return
	}
	locale, explicit, ok := requestLocale(w, r, "")
	if !ok {
		return
	}
	var entries []model.EntryWithKey
	var err error
	if explicit {
		entries, err = s.store.GetEntriesLocale(category, field, source, locale)
	} else {
		entries, err = s.store.GetEntries(category, field, source)
	}
	if err != nil {
		writeLocaleInternalError(w, explicit, err)
		return
	}
	if entries == nil {
		entries = []model.EntryWithKey{}
	}
	if !explicit {
		for i := range entries {
			entries[i].UpdatedAt = 0
		}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleUpdateEntry updates one translation entry.
//
// PUT /api/editor/v1/entry {category, field, key, text, source}
func (s *Server) handleUpdateEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	correlatedResponse := false
	if values, present := r.URL.Query()["response"]; present {
		if len(values) != 1 || values[0] != "correlated-v1" {
			writeErr(w, http.StatusBadRequest, "unsupported entry response contract")
			return
		}
		correlatedResponse = true
	}
	var req struct {
		Category string `json:"category"`
		Field    string `json:"field"`
		Key      string `json:"key"`
		Text     string `json:"text"`
		Source   string `json:"source"`
		Locale   string `json:"locale"`
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !model.IsValidCategory(req.Category) {
		writeErr(w, http.StatusBadRequest, "unsupported category")
		return
	}
	if req.Field == "" || req.Key == "" {
		writeErr(w, http.StatusBadRequest, "field and key required")
		return
	}
	if !model.IsValidSource(req.Source) {
		writeErr(w, http.StatusBadRequest, "invalid translation source")
		return
	}
	locale, explicit, ok := requestLocale(w, r, req.Locale)
	if !ok {
		return
	}
	if locale == model.LocaleJapanese {
		writeErr(w, http.StatusBadRequest, "locale is read-only")
		return
	}
	var status string
	var err error
	if explicit {
		status, err = s.store.UpdateEntryLocale(req.Category, req.Field, req.Key, req.Text, req.Source, currentUser(r), locale)
	} else {
		status, err = s.store.UpdateEntry(req.Category, req.Field, req.Key, req.Text, req.Source, currentUser(r))
	}
	if err != nil {
		if err == sql.ErrNoRows {
			writeErr(w, http.StatusConflict, "entry source identity changed; reload before saving")
			return
		}
		writeLocaleInternalError(w, explicit, err)
		return
	}
	if status == "ok" {
		s.rebuildCategoryAsset(req.Category)
		payload := map[string]any{
			"category": req.Category,
			"field":    req.Field,
			"key":      req.Key,
			"text":     req.Text,
			"source":   req.Source,
			"user":     currentUser(r),
			"clientId": req.ClientID,
		}
		if explicit {
			payload["locale"] = locale
		}
		event := sse.EventEntryUpdated
		if explicit && locale != model.LocaleChinese {
			event = sse.EventEntryLocaleUpdated
		}
		s.broadcast(event, payload)
	}
	if correlatedResponse {
		response := struct {
			Status   string `json:"status"`
			Category string `json:"category"`
			Field    string `json:"field"`
			Key      string `json:"key"`
			Text     string `json:"text"`
			Source   string `json:"source"`
			Locale   string `json:"locale,omitempty"`
		}{
			Status: status, Category: req.Category, Field: req.Field, Key: req.Key,
			Text: req.Text, Source: req.Source,
		}
		if explicit {
			response.Locale = locale
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}
