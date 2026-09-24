package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/publiclyricsbundle"
	"moesekai/server/internal/store"
)

// handleLyricsDocument publishes one song as a source-v3 document with ruby,
// Game sides and several renditions, which the public projection serves as
// the v3 detail returned in the response.
func (s *Server) handleLyricsDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request store.LyricsDocumentRequest
	if !decodeBody(w, r, &request) {
		return
	}
	served, ok := s.lyricsDocumentServed(w, request.MusicID)
	if !ok {
		return
	}
	result, err := s.store.PublishLyricsDocumentServed(r.Context(), request, served, currentUser(r))
	if err != nil {
		writeLyricsDocumentError(w, err)
		return
	}
	s.afterLyricsDocumentPublish(r, result)
	writeJSON(w, http.StatusOK, result)
}

// lyricsDocumentTakeoverRequest names the served song to take over and the
// revision a PUT of its export would send.
type lyricsDocumentTakeoverRequest struct {
	MusicID          int  `json:"musicId"`
	ExpectedRevision *int `json:"expectedRevision"`
}

// handleLyricsDocumentTakeover exports the served song as GET does and
// publishes that request as PUT does, so a song served from the recovery
// ledger, a legacy publication or the bundle becomes an editor document with
// unchanged public lyrics.
func (s *Server) handleLyricsDocumentTakeover(w http.ResponseWriter, r *http.Request) {
	var request lyricsDocumentTakeoverRequest
	if !decodeBody(w, r, &request) {
		return
	}
	served, ok := s.lyricsDocumentServed(w, request.MusicID)
	if !ok {
		return
	}
	result, err := s.store.TakeOverLyricsDocument(r.Context(), request.MusicID, request.ExpectedRevision, served, currentUser(r))
	if err != nil {
		writeLyricsDocumentError(w, err)
		return
	}
	s.afterLyricsDocumentPublish(r, result.LyricsDocumentResult)
	writeJSON(w, http.StatusOK, result)
}

// lyricsDocumentServed reads what the public site serves for musicID: the
// embedded bundle revision and the served detail bytes, nil when none.
func (s *Server) lyricsDocumentServed(w http.ResponseWriter, musicID int) (store.LyricsDocumentServed, bool) {
	var served store.LyricsDocumentServed
	if musicID <= 0 {
		return served, true
	}
	metadata, err := publiclyricsbundle.CatalogRuntimeMetadata()
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return served, false
	}
	served.BundleRevision = metadata[musicID].Revision
	if reader, ok := s.fileService.(publicLyricsDetailReader); ok {
		if detail, found := reader.PublicLyricsDetail(musicID); found {
			served.Detail = detail
		}
	}
	return served, true
}

func (s *Server) afterLyricsDocumentPublish(r *http.Request, result store.LyricsDocumentResult) {
	if result.DryRun {
		return
	}
	if resetErr := s.resetLyricsCollaboration(result.MusicID); resetErr != nil {
		s.reportLyricsInvariant("[lyrics] collaboration document reset failed musicId=%d: %v", result.MusicID, resetErr)
	}
	s.broadcastLyricsDocumentUpdated(result.MusicID, result.Revision, "", currentUser(r))
	if s.fileService != nil {
		s.fileService.PublishNow()
	}
}

// projectionAwaiter is the file service wait for pending publications.
type projectionAwaiter interface {
	AwaitPublished(ctx context.Context, timeout time.Duration) bool
}

// lyricsDocumentProjectionWait bounds how long a document route waits for a
// pending public-file rebuild before it reads the served detail.
const lyricsDocumentProjectionWait = 10 * time.Second

// awaitServedLyrics lets a pending public-file rebuild finish before a
// document route reads the served detail, so an export taken right after a
// publish carries that publish instead of the detail it replaced, and a PUT
// compares with it. It runs before any lock is taken. When the wait gives up
// the route still answers; an export then flags the stale detail with
// served_revision_differs.
func (s *Server) awaitServedLyrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if awaiter, ok := s.fileService.(projectionAwaiter); ok {
			awaiter.AwaitPublished(r.Context(), lyricsDocumentProjectionWait)
		}
		next(w, r)
	}
}

// publicLyricsDetailReader is the file service read accessor for the detail
// bytes served under /files/translation/lyrics/.
type publicLyricsDetailReader interface {
	PublicLyricsDetail(musicID int) ([]byte, bool)
}

// handleLyricsDocumentExport answers GET with one song converted into the
// request PUT accepts: from=served (the default) reads the served public
// detail and falls back to the database state when the site serves nothing;
// from=database reads the editable database state. It reads only, so it
// takes no producer state and no content mutation lock.
func (s *Server) handleLyricsDocumentExport(w http.ResponseWriter, r *http.Request) {
	musicID, ok := positiveIntQuery(w, r, "musicId")
	if !ok {
		return
	}
	from := r.URL.Query().Get("from")
	if from == "" {
		from = store.LyricsDocumentExportFromServed
	}
	if from != store.LyricsDocumentExportFromServed && from != store.LyricsDocumentExportFromDatabase {
		writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"from must be served or database"}, nil)
		return
	}
	if _, ok := s.fileService.(publicLyricsDetailReader); !ok {
		writeContractError(w, http.StatusServiceUnavailable, "projection_unavailable", nil, nil)
		return
	}
	served, ok := s.lyricsDocumentServed(w, musicID)
	if !ok {
		return
	}
	var export store.LyricsDocumentExport
	var err error
	if from == store.LyricsDocumentExportFromServed && served.Detail != nil {
		export, err = s.store.ExportLyricsDocument(r.Context(), musicID, served.Detail, served.BundleRevision)
	} else {
		export, err = s.store.ExportLyricsDocumentFromDatabase(r.Context(), musicID, served)
	}
	var documentErr *store.LyricsDocumentError
	if errors.As(err, &documentErr) {
		writeLyricsDocumentError(w, err)
		return
	} else if err != nil {
		log.Printf("[lyrics] document export failed musicId=%d: %v", musicID, err)
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, export)
}

// lyricsDocumentRubyRequest lists the ja markups to suggest ruby for.
type lyricsDocumentRubyRequest struct {
	Lines []string `json:"lines"`
}

// maxLyricsDocumentRubyLines bounds one ruby request; a whole song fits.
const maxLyricsDocumentRubyLines = 2000

// handleLyricsDocumentRuby answers POST with dictionary ruby for the kanji of
// each ja markup that has none. It reads and writes no song, so any signed-in
// user may call it and it takes no producer state or content lock.
func (s *Server) handleLyricsDocumentRuby(w http.ResponseWriter, r *http.Request) {
	var request lyricsDocumentRubyRequest
	if !decodeBody(w, r, &request) {
		return
	}
	var details []string
	if len(request.Lines) == 0 || len(request.Lines) > maxLyricsDocumentRubyLines {
		details = append(details, fmt.Sprintf("lines must list 1 to %d ja markups; it lists %d", maxLyricsDocumentRubyLines, len(request.Lines)))
	}
	for index, line := range request.Lines {
		if len(line) > store.MaxLyricsDocumentJapaneseLineBytes || strings.ContainsAny(line, "\r\n\x00") {
			details = append(details, fmt.Sprintf("lines[%d] must be one line of at most %d bytes", index, store.MaxLyricsDocumentJapaneseLineBytes))
		}
	}
	if len(details) > 0 {
		writeContractError(w, http.StatusBadRequest, "invalid_request", details, nil)
		return
	}
	lines := make([]store.LyricsDocumentRubySuggestion, len(request.Lines))
	for index, line := range request.Lines {
		suggestion, err := store.SuggestLyricsDocumentRuby(line)
		if err != nil {
			log.Printf("[lyrics] ruby suggestion failed: %v", err)
			writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
			return
		}
		lines[index] = suggestion
	}
	writeJSON(w, http.StatusOK, map[string]any{"generator": lyricssource.DeterministicRubyGeneratorVersion(), "lines": lines})
}

func writeLyricsDocumentError(w http.ResponseWriter, err error) {
	var documentErr *store.LyricsDocumentError
	if !errors.As(err, &documentErr) {
		log.Printf("[lyrics] document publish failed: %v", err)
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	status := http.StatusUnprocessableEntity
	switch documentErr.Code {
	case store.LyricsDocumentErrorRevisionConflict:
		status = http.StatusConflict
	case store.LyricsDocumentErrorNotFound:
		status = http.StatusNotFound
	}
	if len(documentErr.Issues) == 0 {
		writeContractError(w, status, documentErr.Code, documentErr.Details, documentErr.Current)
		return
	}
	body := map[string]any{"error": documentErr.Code, "issues": documentErr.Issues}
	if len(documentErr.Details) > 0 {
		body["details"] = documentErr.Details
	}
	if documentErr.Current != nil {
		body["current"] = documentErr.Current
	}
	writeJSON(w, status, body)
}
