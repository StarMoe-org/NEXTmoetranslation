package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"moesekai/server/internal/model"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
)

// handleEventStories lists event story summaries.
//
// GET /api/event-stories
func (s *Server) handleEventStories(w http.ResponseWriter, r *http.Request) {
	locale, explicit, ok := requestLocale(w, r, "")
	if !ok {
		return
	}
	var stories []model.EventStorySummary
	var err error
	if explicit {
		stories, err = s.eventStore.ListLocale(locale)
	} else {
		stories, err = s.eventStore.List()
	}
	if err != nil {
		writeLocaleInternalError(w, explicit, err)
		return
	}
	if stories == nil {
		stories = []model.EventStorySummary{}
	}
	if !explicit {
		for i := range stories {
			stories[i].AllOfficialTagged = false
		}
	}
	writeJSON(w, http.StatusOK, stories)
}

// handleEventStory returns one event story's full detail.
//
// GET /api/event-story?eventId=123
func (s *Server) handleEventStory(w http.ResponseWriter, r *http.Request) {
	id, ok := parseEventID(w, r.URL.Query().Get("eventId"))
	if !ok {
		return
	}
	locale, explicit, localeOK := requestLocale(w, r, "")
	if !localeOK {
		return
	}
	var detail model.EventStoryDetail
	var err error
	if explicit {
		detail, err = s.eventStore.DetailLocale(id, locale)
	} else {
		detail, err = s.eventStore.Detail(id)
	}
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "event story not found")
		return
	}
	if err != nil {
		writeLocaleInternalError(w, explicit, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleEventEpisodeSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	eventID, ok := parseEventID(w, r.URL.Query().Get("eventId"))
	if !ok {
		return
	}
	episodeNo := strings.TrimSpace(r.URL.Query().Get("episodeNo"))
	if episodeNo == "" {
		writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"episodeNo required"}, nil)
		return
	}
	locale, explicit, localeOK := requestLocale(w, r, "")
	if !localeOK {
		return
	}
	if !explicit {
		writeContractError(w, http.StatusBadRequest, "locale_required", []string{"explicit locale required"}, nil)
		return
	}
	snapshot, err := s.eventStore.EpisodeSnapshot(eventID, episodeNo, locale)
	if err == sql.ErrNoRows {
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		return
	}
	if errors.Is(err, store.ErrEventScenarioConflict) {
		writeContractError(w, http.StatusConflict, "scenario_conflict", nil, nil)
		return
	}
	if errors.Is(err, store.ErrEventScenarioInvalid) {
		writeContractError(w, http.StatusInternalServerError, "scenario_invalid", nil, nil)
		return
	}
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// handleUpdateEventStory updates one talk line or episode title.
//
// PUT /api/editor/v1/event-story/update {eventId, episodeNo, jpKey, cnText, source, entryType}
func (s *Server) handleUpdateEventStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		EventID    int     `json:"eventId"`
		EpisodeNo  string  `json:"episodeNo"`
		JpKey      string  `json:"jpKey"`
		CnText     string  `json:"cnText"`
		Source     string  `json:"source"`
		EntryType  string  `json:"entryType"`
		Locale     string  `json:"locale"`
		SegmentID  string  `json:"segmentId"`
		SourceHash *string `json:"sourceHash"`
		Revision   *int    `json:"revision"`
		ClientID   string  `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.EventID <= 0 || req.EpisodeNo == "" {
		writeErr(w, http.StatusBadRequest, "eventId and episodeNo required")
		return
	}
	if req.EntryType != "title" && req.JpKey == "" {
		writeErr(w, http.StatusBadRequest, "jpKey required for talk entries")
		return
	}
	if req.Source == "" {
		req.Source = "human"
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
	if explicit && (req.SegmentID == "" || req.SourceHash == nil || strings.TrimSpace(req.EpisodeNo) == "") {
		writeContractError(w, http.StatusBadRequest, "source_identity_required", []string{"segmentId, sourceHash, and episodeNo are required"}, nil)
		return
	}
	if explicit && req.EntryType != "title" && req.EntryType != "talk" {
		writeContractError(w, http.StatusBadRequest, "source_identity_required", []string{"entryType must be title or talk"}, nil)
		return
	}
	if explicit && ((req.EntryType == "talk" && req.JpKey == "") || (req.EntryType == "title" && req.JpKey != "")) {
		writeContractError(w, http.StatusBadRequest, "source_identity_required", []string{"jpKey is required for talk entries and must be empty for titles"}, nil)
		return
	}
	var err error
	if explicit {
		sourceHash := ""
		if req.SourceHash != nil {
			sourceHash = *req.SourceHash
		}
		err = s.eventStore.UpdateLineLocaleRevision(req.EventID, req.EpisodeNo, req.JpKey, req.SegmentID, sourceHash, req.CnText, req.Source, req.EntryType, locale, currentUser(r), req.Revision)
	} else {
		err = s.eventStore.UpdateLine(req.EventID, req.EpisodeNo, req.JpKey, req.CnText, req.Source, req.EntryType)
	}
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "target line not found")
		return
	}
	if errors.Is(err, store.ErrEventSourceConflict) {
		writeContractError(w, http.StatusConflict, "source_conflict", []string{"event source identity changed; reload before saving"}, nil)
		return
	}
	if errors.Is(err, store.ErrEventRevisionConflict) {
		writeContractError(w, http.StatusConflict, "revision_conflict", []string{"event translation changed; reload before saving"}, nil)
		return
	}
	if err != nil {
		writeLocaleInternalError(w, explicit, err)
		return
	}
	s.rebuildEventAsset(req.EventID)
	s.store.NotifyChange() // event story files are regenerated too
	payload := map[string]any{
		"eventId":   req.EventID,
		"episodeNo": req.EpisodeNo,
		"jpKey":     req.JpKey,
		"cnText":    req.CnText,
		"source":    req.Source,
		"entryType": req.EntryType,
		"user":      currentUser(r),
		"clientId":  req.ClientID,
	}
	if explicit {
		payload["locale"] = locale
	}
	if req.SegmentID != "" {
		payload["segmentId"] = req.SegmentID
	}
	if req.Revision != nil {
		payload["revision"] = *req.Revision + 1
	}
	event := sse.EventStoryUpdated
	if explicit && locale != model.LocaleChinese {
		event = sse.EventStoryLocaleUpdated
	}
	s.broadcast(event, payload)
	if explicit && req.Revision != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "revision": *req.Revision + 1})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePromoteEventStoryHuman marks an entire story as human-edited.
//
// POST /api/editor/v1/event-story/promote-human {eventId, clientId}
func (s *Server) handlePromoteEventStoryHuman(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, clientID, ok := decodeEventMutation(w, r)
	if !ok {
		return
	}
	if err := s.eventStore.PromoteHuman(id); err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "event story not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.rebuildEventAsset(id)
	s.store.NotifyChange()
	s.broadcast(sse.EventStoryUpdated, map[string]any{
		"eventId":  id,
		"promote":  "human",
		"user":     currentUser(r),
		"clientId": clientID,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// parseEventID parses and validates an event id from a string.
func parseEventID(w http.ResponseWriter, raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "eventId required")
		return 0, false
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid eventId")
		return 0, false
	}
	return id, true
}

// decodeEventMutation reads {eventId, clientId} from a JSON body.
func decodeEventMutation(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	var req struct {
		EventID  int    `json:"eventId"`
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return 0, "", false
	}
	if req.EventID <= 0 {
		writeErr(w, http.StatusBadRequest, "eventId required")
		return 0, "", false
	}
	clientID, ok := validateEventClientID(w, req.ClientID)
	if !ok {
		return 0, "", false
	}
	return req.EventID, clientID, true
}

func validateEventClientID(w http.ResponseWriter, clientID string) (string, bool) {
	if len(clientID) > 128 {
		writeErr(w, http.StatusBadRequest, "clientId too long")
		return "", false
	}
	return clientID, true
}
