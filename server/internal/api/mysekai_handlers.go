package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"moesekai/server/internal/store"
)

func mysekaiError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrMysekaiConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "content not found")
	default:
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	}
}
func (s *Server) handleMysekaiList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 40
	if v := q.Get("limit"); v != "" {
		var err error
		limit, err = strconv.Atoi(v)
		if err != nil {
			writeErr(w, 400, "invalid limit")
			return
		}
	}
	p, err := s.store.MysekaiList(r.Context(), q.Get("region"), q.Get("locale"), q.Get("q"), q.Get("state"), q.Get("cursor"), limit)
	if err != nil {
		mysekaiError(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) handleMysekaiFontCoverage(w http.ResponseWriter, r *http.Request) {
	c, e := s.store.MysekaiFontCoverage(r.Context())
	if e != nil {
		writeErr(w, http.StatusServiceUnavailable, e.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}
func (s *Server) handleMysekaiDetail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, e := s.store.MysekaiDetail(r.Context(), q.Get("region"), q.Get("locale"), q.Get("key"))
	if e != nil {
		mysekaiError(w, e)
		return
	}
	writeJSON(w, 200, d)
}
func (s *Server) handleMysekaiSource(w http.ResponseWriter, r *http.Request) {
	// Full source catalogs are deliberately accepted only on this admin route;
	// normal editor requests keep the shared, smaller JSON request limit.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil || validateUniqueJSON(raw) != nil {
		writeErr(w, 400, "invalid source catalog")
		return
	}
	var request struct {
		Catalog          json.RawMessage `json:"catalog"`
		ExpectedRevision string          `json:"expectedRevision"`
	}
	if !decodeJSONBody(w, raw, &request) {
		return
	}
	c, e := s.store.ImportMysekaiSource(r.Context(), request.Catalog, request.ExpectedRevision, currentUser(r))
	if e != nil {
		mysekaiError(w, e)
		return
	}
	s.broadcast("mysekai_updated", map[string]any{"region": c.Region, "sourceRevision": c.Revision})
	writeJSON(w, 200, map[string]any{"region": c.Region, "sourceRevision": c.Revision, "scripts": len(c.Scripts)})
}
func (s *Server) handleMysekaiSave(w http.ResponseWriter, r *http.Request) {
	var request store.MysekaiSave
	if !decodeBody(w, r, &request) {
		return
	}
	d, e := s.store.SaveMysekai(r.Context(), request, currentUser(r))
	if e != nil {
		mysekaiError(w, e)
		return
	}
	s.broadcast("mysekai_updated", map[string]any{"region": request.Region, "locale": request.Locale, "key": request.ScriptKey})
	writeJSON(w, 200, d)
}
func (s *Server) handleMysekaiPublicationHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e := s.store.MysekaiPublicationHistory(r.Context(), q.Get("region"), q.Get("locale"))
	if e != nil {
		mysekaiError(w, e)
		return
	}
	writeJSON(w, 200, h)
}
func (s *Server) handleMysekaiPublication(w http.ResponseWriter, r *http.Request) {
	var request store.MysekaiPublicationRequest
	if !decodeBody(w, r, &request) {
		return
	}
	c, e := s.store.SetMysekaiPublication(r.Context(), request, r.PathValue("action"), currentUser(r))
	if e != nil {
		mysekaiError(w, e)
		return
	}
	ready := false
	if s.fileService != nil {
		s.fileService.PublishNow()
		if awaiter, ok := s.fileService.(projectionAwaiter); ok {
			ready = awaiter.AwaitPublished(r.Context(), 5*time.Second)
		}
	}
	s.broadcast("mysekai_updated", map[string]any{"region": request.Region, "locale": request.Locale, "publication": c.Publication})
	status := http.StatusOK
	if !ready {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{"channel": c, "projectionReady": ready})
}
