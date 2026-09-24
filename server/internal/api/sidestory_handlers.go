package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/model"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
)

// SideStoryRunner is the translator side of the card-story and area-talk
// routes: backfill control, upstream re-fetches and on-demand AI.
type SideStoryRunner interface {
	SideStoryBackfillState() store.SideStoryBackfillState
	TriggerSideStoryBackfill(refreshCatalog bool) bool
	RefreshSideStoryContext(ctx context.Context, kind, storyID string) ([]store.SideStoryEpisodeApply, error)
	AITranslateSideStoryContext(ctx context.Context, kind, storyID, locale, episodeKey, provider string) (store.SideStoryAIResult, error)
	FetchSideStoryJPScriptContext(ctx context.Context, kind, storyID, episodeKey string) (canonicalJSON, sha256 string, err error)
}

const maxSideStoryClientIDBytes = 128

type sideStoryListResponse struct {
	Kind    string                   `json:"kind"`
	Locale  string                   `json:"locale"`
	Stories []store.SideStorySummary `json:"stories"`
}

type sideStoryUpdateResponse struct {
	Status  string `json:"status"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Episode string `json:"episode"`
	Locale  string `json:"locale"`
	store.SideStoryUpdateResult
}

type sideStoryUpdatedEvent struct {
	Kind     string                     `json:"kind"`
	ID       string                     `json:"id"`
	Episode  string                     `json:"episode"`
	Locale   string                     `json:"locale"`
	Action   string                     `json:"action"`
	Lines    []store.SideStoryLineState `json:"lines,omitempty"`
	User     string                     `json:"user"`
	ClientID string                     `json:"clientId"`
}

type sideStorySnapshotSegment struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Position   int    `json:"position"`
	Japanese   string `json:"japanese"`
	SourceHash string `json:"sourceHash"`
	Text       string `json:"text"`
	Source     string `json:"source"`
	Revision   int    `json:"revision"`
}

type sideStorySnapshot struct {
	Kind     string                             `json:"kind"`
	ID       string                             `json:"id"`
	Episode  string                             `json:"episode"`
	Locale   string                             `json:"locale"`
	Revision string                             `json:"revision"`
	Segments []sideStorySnapshotSegment         `json:"segments"`
	Scenario store.EventEpisodeScenarioSnapshot `json:"scenario"`
}

// GET /api/editor/v1/stories?kind=card|area&locale=&status=
func (s *Server) handleSideStories(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	kind := query.Get("kind")
	if !store.ValidSideStoryKind(kind) {
		writeSideStoryInvalid(w, "kind must be card or area")
		return
	}
	locale, ok := sideStoryLocale(w, query.Get("locale"), false)
	if !ok {
		return
	}
	status := query.Get("status")
	switch status {
	case "", "pending", "untranslated", "partial", "translated":
	default:
		writeSideStoryInvalid(w, "status must be pending, untranslated, partial or translated")
		return
	}
	stories, err := s.store.ListSideStoriesContext(r.Context(), kind, locale)
	if err != nil {
		writeSideStoryStoreError(w, err)
		return
	}
	if status != "" {
		filtered := stories[:0]
		for _, story := range stories {
			if story.Status == status {
				filtered = append(filtered, story)
			}
		}
		stories = filtered
	}
	writeJSON(w, http.StatusOK, sideStoryListResponse{Kind: kind, Locale: locale, Stories: stories})
}

// GET /api/editor/v1/stories/sync
func (s *Server) handleSideStorySyncStatus(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.sideStoryRunner(w)
	if !ok {
		return
	}
	totals, err := s.store.SideStoryProgressContext(r.Context())
	if err != nil {
		writeSideStoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": runner.SideStoryBackfillState(), "totals": totals})
}

// POST /api/editor/v1/stories/sync {refreshCatalog}
func (s *Server) handleSideStorySync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshCatalog bool `json:"refreshCatalog"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	runner, ok := s.sideStoryRunner(w)
	if !ok {
		return
	}
	started := runner.TriggerSideStoryBackfill(req.RefreshCatalog)
	state := runner.SideStoryBackfillState()
	if !started && !state.Enabled {
		writeContractError(w, http.StatusConflict, "backfill_disabled", []string{"the side-story backfill is disabled"}, nil)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": started, "state": state})
}

// GET /api/editor/v1/story/{kind}/{id}?locale=
func (s *Server) handleSideStory(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := sideStoryPath(w, r)
	if !ok {
		return
	}
	locale, ok := sideStoryLocale(w, r.URL.Query().Get("locale"), false)
	if !ok {
		return
	}
	detail, err := s.store.SideStoryDetailContext(r.Context(), kind, id, locale)
	if err != nil {
		writeSideStoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// PUT /api/editor/v1/story/{kind}/{id}/{episode} {locale, lines, clientId}
func (s *Server) handleUpdateSideStory(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := sideStoryPath(w, r)
	if !ok {
		return
	}
	episode, ok := sideStoryEpisode(w, kind, r.PathValue("episode"), false)
	if !ok {
		return
	}
	var req struct {
		Locale   string                    `json:"locale"`
		Lines    []store.SideStoryLineEdit `json:"lines"`
		ClientID string                    `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	locale, ok := sideStoryLocale(w, req.Locale, false)
	if !ok || !validSideStoryClientID(w, req.ClientID) {
		return
	}
	user := currentUser(r)
	result, err := s.store.UpdateSideStoryLinesContext(r.Context(), kind, id, episode, locale, user, req.Lines, time.Now())
	if err != nil {
		writeSideStoryStoreError(w, err)
		return
	}
	if result.Updated > 0 {
		s.publishSideStory(sideStoryUpdatedEvent{
			Kind: kind, ID: id, Episode: episode, Locale: locale, Action: "update",
			Lines: result.Lines, User: user, ClientID: req.ClientID,
		})
	}
	writeJSON(w, http.StatusOK, sideStoryUpdateResponse{
		Status: "ok", Kind: kind, ID: id, Episode: episode, Locale: locale, SideStoryUpdateResult: result,
	})
}

// POST /api/editor/v1/story/{kind}/{id}/ai {locale, episode, provider, clientId}
func (s *Server) handleSideStoryAI(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := sideStoryPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Locale   string `json:"locale"`
		Episode  string `json:"episode"`
		Provider string `json:"provider"`
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	locale, ok := sideStoryLocale(w, req.Locale, false)
	if !ok || !validSideStoryClientID(w, req.ClientID) {
		return
	}
	episode, ok := sideStoryEpisode(w, kind, req.Episode, true)
	if !ok {
		return
	}
	runner, ok := s.sideStoryRunner(w)
	if !ok {
		return
	}
	result, err := runner.AITranslateSideStoryContext(r.Context(), kind, id, locale, episode, req.Provider)
	if err != nil {
		writeSideStoryRunnerError(w, err, http.StatusInternalServerError, "internal_error")
		return
	}
	s.publishSideStory(sideStoryUpdatedEvent{
		Kind: kind, ID: id, Episode: episode, Locale: locale, Action: "ai", User: currentUser(r), ClientID: req.ClientID,
	})
	writeJSON(w, http.StatusOK, result)
}

// POST /api/editor/v1/story/{kind}/{id}/refresh {clientId}
func (s *Server) handleRefreshSideStory(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := sideStoryPath(w, r)
	if !ok {
		return
	}
	var req struct {
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &req) || !validSideStoryClientID(w, req.ClientID) {
		return
	}
	runner, ok := s.sideStoryRunner(w)
	if !ok {
		return
	}
	episodes, err := runner.RefreshSideStoryContext(r.Context(), kind, id)
	if err != nil {
		writeSideStoryRunnerError(w, err, http.StatusBadGateway, "upstream_unavailable")
		return
	}
	if episodes == nil {
		episodes = []store.SideStoryEpisodeApply{}
	}
	s.publishSideStory(sideStoryUpdatedEvent{
		Kind: kind, ID: id, Action: "refresh", User: currentUser(r), ClientID: req.ClientID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"episodes": episodes})
}

// GET /api/editor/v1/story/{kind}/{id}/{episode}/snapshot?locale=
//
// The live JP script must hash to the stored script_sha256, so the segments
// built from it name exactly the lines a PUT accepts.
func (s *Server) handleSideStorySnapshot(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := sideStoryPath(w, r)
	if !ok {
		return
	}
	episodeKey, ok := sideStoryEpisode(w, kind, r.PathValue("episode"), false)
	if !ok {
		return
	}
	locale, ok := sideStoryLocale(w, r.URL.Query().Get("locale"), true)
	if !ok {
		return
	}
	runner, ok := s.sideStoryRunner(w)
	if !ok {
		return
	}
	detail, err := s.store.SideStoryDetailContext(r.Context(), kind, id, locale)
	if err != nil {
		writeSideStoryStoreError(w, err)
		return
	}
	var episode *store.SideStoryEpisodeDetail
	for index := range detail.Episodes {
		if detail.Episodes[index].Key == episodeKey {
			episode = &detail.Episodes[index]
		}
	}
	if episode == nil {
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		return
	}
	if !episode.Fetched {
		writeContractError(w, http.StatusConflict, "script_not_fetched", []string{"the Japanese script of this episode has not been fetched yet"}, nil)
		return
	}
	canonical, digest, err := runner.FetchSideStoryJPScriptContext(r.Context(), kind, id, episodeKey)
	if err != nil {
		writeSideStoryRunnerError(w, err, http.StatusBadGateway, "upstream_unavailable")
		return
	}
	if digest != episode.ScriptSHA256 {
		writeContractError(w, http.StatusConflict, "script_changed", []string{"the Japanese script changed upstream; refresh the story before importing"}, nil)
		return
	}
	sourceTalks, err := store.ParseEventSourceTalks(canonical)
	if err != nil {
		log.Printf("[api] side story %s/%s/%s source talks: %v", kind, id, episodeKey, err)
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	segments, err := sideStorySnapshotSegments(canonical, episode.Lines)
	if err != nil {
		log.Printf("[api] side story %s/%s/%s segments: %v", kind, id, episodeKey, err)
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, sideStorySnapshot{
		Kind: kind, ID: id, Episode: episodeKey, Locale: locale,
		Revision: sideStorySnapshotRevision(locale, digest, episode.Lines), Segments: segments,
		Scenario: store.EventEpisodeScenarioSnapshot{
			ScenarioID: episode.ScenarioID, FileName: episode.ScenarioID + ".json", SHA256: digest,
			ParserVersion: store.EventScenarioParserVersion, RawJSON: canonical, SourceTalks: sourceTalks,
		},
	})
}

// sideStorySnapshotSegments emits, for TalkData index i, the body at 2i and the
// speaker at 2i+1 with the text the console's parser reads for that index,
// each carrying its line's translation.
func sideStorySnapshotSegments(canonical string, lines []store.SideStoryLineState) ([]sideStorySnapshotSegment, error) {
	var script struct {
		TalkData []struct {
			Body              string `json:"Body"`
			WindowDisplayName string `json:"WindowDisplayName"`
		} `json:"TalkData"`
	}
	if err := json.Unmarshal([]byte(canonical), &script); err != nil {
		return nil, err
	}
	byKey := make(map[string]store.SideStoryLineState, len(lines))
	for _, line := range lines {
		byKey[line.JP] = line
	}
	segment := func(key string, position int, japanese string) sideStorySnapshotSegment {
		line := byKey[key]
		return sideStorySnapshotSegment{
			ID: key, Kind: "talk", Position: position, Japanese: japanese,
			Text: line.Text, Source: line.Source, Revision: line.Revision,
		}
	}
	segments := []sideStorySnapshotSegment{}
	for index, talk := range script.TalkData {
		if key := strings.TrimSpace(talk.Body); key != "" {
			segments = append(segments, segment(key, index*2, talk.Body))
		}
		if key := strings.TrimSpace(talk.WindowDisplayName); key != "" {
			segments = append(segments, segment(key, index*2+1, strings.SplitN(talk.WindowDisplayName, "_", 2)[0]))
		}
	}
	return segments, nil
}

// sideStorySnapshotRevision changes whenever the script or any line revision
// of the episode in locale changes.
func sideStorySnapshotRevision(locale, scriptSHA256 string, lines []store.SideStoryLineState) string {
	pairs := make([]string, len(lines))
	for index, line := range lines {
		pairs[index] = line.JP + "\x00" + strconv.Itoa(line.Revision)
	}
	sort.Strings(pairs)
	digest := sha256.New()
	digest.Write([]byte(locale + "\x00" + scriptSHA256))
	for _, pair := range pairs {
		digest.Write([]byte("\x00" + pair))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func (s *Server) publishSideStory(event sideStoryUpdatedEvent) {
	s.rebuildSideStoryAsset(event.Kind, event.ID)
	s.store.NotifyChange()
	s.broadcast(sse.EventSideStoryUpdated, event)
}

func (s *Server) sideStoryRunner(w http.ResponseWriter) (SideStoryRunner, bool) {
	if s.sideStories == nil {
		writeContractError(w, http.StatusServiceUnavailable, "side_story_unavailable", nil, nil)
		return nil, false
	}
	return s.sideStories, true
}

func sideStoryPath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !store.ValidSideStoryKind(kind) {
		writeSideStoryInvalid(w, "kind must be card or area")
		return "", "", false
	}
	if !store.ValidSideStoryID(kind, id) {
		writeSideStoryInvalid(w, "invalid "+kind+" story id")
		return "", "", false
	}
	return kind, id, true
}

// sideStoryEpisode validates an episode key; optional admits "" (every episode).
func sideStoryEpisode(w http.ResponseWriter, kind, key string, optional bool) (string, bool) {
	if (optional && key == "") || store.ValidSideStoryEpisodeKey(kind, key) {
		return key, true
	}
	writeSideStoryInvalid(w, "invalid "+kind+" episode")
	return "", false
}

// sideStoryLocale defaults to zh-CN unless required; ja-JP is the read-only source.
func sideStoryLocale(w http.ResponseWriter, locale string, required bool) (string, bool) {
	if locale == "" && !required {
		return model.LocaleChinese, true
	}
	if !store.ValidSideStoryLocale(locale) {
		writeSideStoryInvalid(w, "locale must be zh-CN or en-US")
		return "", false
	}
	return locale, true
}

func validSideStoryClientID(w http.ResponseWriter, clientID string) bool {
	if len(clientID) > maxSideStoryClientIDBytes {
		writeSideStoryInvalid(w, "clientId must be at most 128 bytes")
		return false
	}
	return true
}

func writeSideStoryInvalid(w http.ResponseWriter, detail string) {
	writeContractError(w, http.StatusBadRequest, "invalid_request", []string{detail}, nil)
}

func writeSideStoryStoreError(w http.ResponseWriter, err error) {
	var unknown *store.SideStoryUnknownLinesError
	var conflict *store.SideStoryRevisionConflictError
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
	case errors.As(err, &unknown):
		writeContractError(w, http.StatusUnprocessableEntity, "unknown_lines", nil, map[string]any{"lines": unknown.Lines})
	case errors.As(err, &conflict):
		writeContractError(w, http.StatusConflict, "revision_conflict", nil, map[string]any{"conflicts": conflict.Conflicts})
	case errors.Is(err, store.ErrSideStoryInvalid):
		writeSideStoryInvalid(w, err.Error())
	default:
		log.Printf("[api] side story request failed: %v", err)
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
	}
}

// writeSideStoryRunnerError maps runner failures; anything unrecognised gets
// the route's fallback status and code.
func writeSideStoryRunnerError(w http.ResponseWriter, err error, status int, code string) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
	case errors.Is(err, store.ErrSideStoryInvalid):
		writeSideStoryInvalid(w, err.Error())
	case translator.IsDraining(err):
		writeContractError(w, http.StatusServiceUnavailable, "draining", []string{err.Error()}, nil)
	case translator.IsAlreadyRunning(err):
		writeContractError(w, http.StatusConflict, "already_running", []string{err.Error()}, nil)
	default:
		log.Printf("[api] side story runner failed: %v", err)
		writeContractError(w, status, code, []string{err.Error()}, nil)
	}
}
