package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/internal/publiclyricsbundle"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
)

func (s *Server) handleCatalogMusic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 200 {
		writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"q exceeds 200 characters"}, nil)
		return
	}
	newlyWritten := true
	if raw := r.URL.Query().Get("newlyWritten"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"newlyWritten must be true or false"}, nil)
			return
		}
		newlyWritten = value
	}
	limit, cursor, ok := pageParams(w, r)
	if !ok {
		return
	}
	result, err := s.store.CatalogMusic(query, newlyWritten, limit, cursor)
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	var bundleMetadata map[int]model.RuntimeLyricsMetadata
	if s.fileService == nil {
		metadata, err := publiclyricsbundle.CatalogRuntimeMetadata()
		if err != nil {
			writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
			return
		}
		bundleMetadata = metadata
	}
	for index := range result.Items {
		result.Items[index].RuntimeLyrics = s.catalogRuntimeLyrics(result.Items[index].MusicID, bundleMetadata)
	}
	writeJSON(w, http.StatusOK, result)
}

// catalogRuntimeLyrics describes what the public mirror currently serves for a
// song. With the file service wired it reflects the live projection (embedded
// bundle overlaid by database publications, minus explicit withdrawals);
// otherwise only the embedded release is known.
func (s *Server) catalogRuntimeLyrics(musicID int, bundleMetadata map[int]model.RuntimeLyricsMetadata) *model.RuntimeLyricsMetadata {
	if s.fileService == nil {
		metadata, ok := bundleMetadata[musicID]
		if !ok {
			return nil
		}
		metadata.AvailableVersions = append([]string{}, metadata.AvailableVersions...)
		return &metadata
	}
	provenance, ok := s.fileService.SongProvenance(musicID)
	if !ok {
		return nil
	}
	metadata := model.RuntimeLyricsMetadata{
		Source:            provenance.Source,
		State:             provenance.State,
		HasDetail:         provenance.HasDetail,
		AvailableVersions: append([]string{}, provenance.AvailableVersions...),
		Revision:          provenance.Revision,
		UpdatedAt:         provenance.UpdatedAt,
	}
	if provenance.Source == filesvc.SongSourceBundle {
		metadata.ReleaseID = publiclyricsbundle.ReleaseID
		metadata.ImmutableOverlay = true
		metadata.BatchSHA256 = publiclyricsbundle.BatchSHA256
		metadata.RootSHA256 = publiclyricsbundle.RootSHA256
	}
	return &metadata
}

func (s *Server) handleCatalogCharacters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result, err := s.store.CatalogPerformers()
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleLyricsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, cursor, ok := pageParams(w, r)
	if !ok {
		return
	}
	result, err := s.store.ListLyrics(limit, cursor)
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleLyricsDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"query parameters are malformed"}, nil)
		return
	}
	musicID, ok := positiveIntQuery(w, r, "musicId")
	if !ok {
		return
	}
	editionValues, explicitEdition := query["translationEditionKey"]
	if explicitEdition && len(editionValues) != 1 {
		writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"translationEditionKey must be provided once"}, nil)
		return
	}
	editionKey := ""
	if explicitEdition {
		editionKey = editionValues[0]
	}
	result, err := s.store.GetLyricsDocumentWithEdition(musicID, editionKey, explicitEdition)
	if errors.Is(err, store.ErrLyricsNotFound) {
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		return
	}
	if errors.Is(err, store.ErrLyricsTranslationEditionNotFound) {
		writeContractError(w, http.StatusNotFound, "translation_edition_not_found", nil, nil)
		return
	}
	if err != nil {
		var contractErr *store.LyricsRenditionContractError
		if errors.As(err, &contractErr) {
			writeContractError(w, http.StatusBadRequest, contractErr.Code, contractErr.Details, nil)
			return
		}
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleLyricsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	data, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if _, plural := shape["renditions"]; plural {
		var request struct {
			store.LyricsRenditionDocument
			ClientID string `json:"clientId"`
		}
		if !decodeJSONBody(w, data, &request) {
			return
		}
		result, changed, targets, err := s.store.SaveLyricsRenditionMutationWithTargets(request.LyricsRenditionDocument, currentUser(r))
		if err != nil {
			writeLyricsError(w, err)
			return
		}
		if changed {
			if s.collab != nil {
				if resetErr := s.resetLyricsCollaboration(result.MusicID); resetErr != nil {
					s.reportLyricsInvariant("[lyrics] collaboration rendition reset failed musicId=%d: %v", result.MusicID, resetErr)
				}
			}
			s.broadcastLyricsRenditionUpdated(result, targets, request.ClientID, currentUser(r))
			// Localization edits are the live publication path for source-v3
			// songs; rebuild the public overlay immediately like the legacy
			// publish route does.
			if s.fileService != nil {
				s.fileService.PublishNow()
			}
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	var request struct {
		model.SongLyrics
		ClientID          string `json:"clientId"`
		SourceImportToken string `json:"sourceImportToken"`
	}
	if !decodeJSONBody(w, data, &request) {
		return
	}
	var result model.SongLyrics
	var changed bool
	var err error
	if strings.TrimSpace(request.SourceImportToken) != "" {
		producerStatus, strict := acceptedEditorStatus(r)
		if !strict {
			writeErr(w, http.StatusPreconditionRequired, "verified source imports require the producer-aware editor route")
			return
		}
		if !currentUserIsAdmin(r) {
			writeContractError(w, http.StatusForbidden, "admin_required", []string{"only administrators can import an external lyrics source"}, nil)
			return
		}
		claim, claimErr := s.claimLyricsImportGrant(request.SourceImportToken, currentUser(r), request.MusicID, producerStatus)
		if claimErr != nil {
			if errors.Is(claimErr, errLyricsImportGrantBusy) {
				writeContractError(w, http.StatusConflict, "source_import_in_flight", []string{"the verified source preview is already being saved"}, nil)
			} else if errors.Is(claimErr, errLyricsImportGrantInvalid) {
				writeContractError(w, http.StatusUnprocessableEntity, "source_drift", []string{"the verified source preview expired or no longer matches this editor, music, or producer state"}, nil)
			} else {
				writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
			}
			return
		}
		claimResolved := false
		defer func() {
			if !claimResolved && !s.releaseLyricsImportGrant(claim) {
				s.invalidateLyricsImportGrant(claim.token)
				s.reportLyricsInvariant("[lyrics] import grant deferred release invariant failed musicId=%d", request.MusicID)
			}
		}()
		consumeClaim := func(stage string) bool {
			committed := s.commitLyricsImportGrant(claim)
			claimResolved = true
			if !committed {
				// The expected exact claim disappeared or changed ownership. Report
				// the invariant and fail closed by deleting this non-reusable token;
				// do not report a false save failure after a durable DB commit.
				s.invalidateLyricsImportGrant(claim.token)
				s.reportLyricsInvariant("[lyrics] import grant commit invariant failed stage=%s musicId=%d", stage, request.MusicID)
			}
			return committed
		}
		releaseClaim := func(stage string) {
			released := s.releaseLyricsImportGrant(claim)
			claimResolved = true
			if !released {
				// Retryability cannot be asserted when exact claim ownership was
				// lost. Delete the non-reusable token and surface the invariant
				// instead of silently leaving ambiguous authorization state.
				s.invalidateLyricsImportGrant(claim.token)
				s.reportLyricsInvariant("[lyrics] import grant release invariant failed stage=%s musicId=%d", stage, request.MusicID)
			}
		}
		catalogCurrent, catalogChecked, catalogErr := s.lyricsImportGrantCatalogCurrent(claim)
		if catalogErr != nil {
			if errors.Is(catalogErr, errLyricsImportGrantInvalid) {
				consumeClaim("catalog_identity_claim_invalid")
			} else {
				releaseClaim("catalog_identity_read_error")
			}
			writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
			return
		}
		if catalogChecked && !catalogCurrent {
			consumeClaim("catalog_identity_drift")
			writeContractError(w, http.StatusUnprocessableEntity, "source_drift", []string{"the catalog title or producer identity changed after the verified preview"}, nil)
			return
		}
		if request.Revision != 0 || !lyricsMatchSourcePreview(request.SongLyrics, claim.preview) {
			consumeClaim("deterministic_request_drift")
			writeContractError(w, http.StatusUnprocessableEntity, "source_drift", []string{"the verified source preview no longer matches this first-save draft"}, nil)
			return
		}
		result, changed, err = s.store.SaveImportedLyricsMutationWithCommit(request.SongLyrics, currentUser(r), func() {
			consumeClaim("post_save_commit")
		})
		if err != nil {
			if importedLyricsFailureIsTerminal(err) {
				consumeClaim("deterministic_store_error")
			} else {
				releaseClaim("retryable_store_error")
			}
			writeLyricsError(w, err)
			return
		}
		// The transaction committed and consumed the capability before any
		// synchronous change hook ran. Preserve the durable success response even
		// if a later notification hook mutates unrelated in-memory state.
	} else {
		result, changed, err = s.store.SaveLyricsMutation(request.SongLyrics, currentUser(r))
		if err != nil {
			writeLyricsError(w, err)
			return
		}
	}
	if changed {
		if s.collab != nil {
			if resetErr := s.resetLyricsCollaboration(result.MusicID); resetErr != nil {
				s.reportLyricsInvariant("[lyrics] collaboration reset failed musicId=%d: %v", result.MusicID, resetErr)
			}
		}
		s.broadcastLyricsUpdated(result, request.ClientID, currentUser(r))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleLyricsTranslationEditions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		store.LyricsTranslationEditionMutation
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	result, err := s.store.MutateLyricsTranslationEdition(request.LyricsTranslationEditionMutation, currentUser(r))
	if err != nil {
		writeLyricsError(w, err)
		return
	}
	// Edition metadata and the revision/default mirror are song-level state.
	// Every open edition must reconcile instead of only the returned edition.
	s.broadcastLyricsDocumentUpdated(result.MusicID, result.Revision, request.ClientID, currentUser(r))
	// Create/clone/rename/set-default change which translation the public
	// projection serves, so they publish immediately like a lyrics save.
	if s.fileService != nil {
		s.fileService.PublishNow()
	}
	writeJSON(w, http.StatusOK, result)
}

func importedLyricsFailureIsTerminal(err error) bool {
	var contractErr *store.LyricsContractError
	if !errors.As(err, &contractErr) {
		return false
	}
	// Source identity/provenance drift and an already-existing/nonzero-revision
	// first save cannot succeed with the issued capability. Draft validation
	// errors remain retryable so the same verified preview can be corrected.
	return contractErr.Code == "source_drift" || contractErr.Code == "revision_conflict"
}

func currentUserIsAdmin(r *http.Request) bool {
	claims, ok := auth.FromContext(r.Context())
	return ok && claims != nil && claims.Role == auth.RoleAdmin
}

func lyricsMatchSourcePreview(lyrics model.SongLyrics, preview lyricssource.Preview) bool {
	if !lyricssource.HasCanonicalSHA1(preview.SHA1) || !lyricssource.HasCanonicalSHA1(lyrics.SourceSHA1) {
		return false
	}
	if lyrics.SourceURL != preview.CanonicalURL || lyrics.SourcePageID != preview.PageID ||
		lyrics.SourceRevisionID != preview.RevisionID || lyrics.SourceSHA1 != preview.SHA1 ||
		lyrics.SourceFetchedAt != preview.FetchedAt || len(lyrics.Lines) != len(preview.Lines) {
		return false
	}
	for index, sourceLine := range preview.Lines {
		line := lyrics.Lines[index]
		if line.Order != index || line.Japanese != sourceLine.Japanese || line.StanzaBreakBefore != sourceLine.StanzaBreakBefore {
			return false
		}
		var japanese strings.Builder
		for _, segment := range line.Segments {
			japanese.WriteString(segment.Text)
			var rubyText strings.Builder
			for _, span := range segment.Ruby {
				rubyText.WriteString(span.Text)
			}
			if len(segment.Ruby) == 0 || rubyText.String() != segment.Text {
				return false
			}
		}
		if japanese.String() != sourceLine.Japanese {
			return false
		}
	}
	return true
}

func (s *Server) handleLyricsPublish(w http.ResponseWriter, r *http.Request) {
	s.handleLyricsPublication(w, r, true)
}

func (s *Server) handleLyricsUnpublish(w http.ResponseWriter, r *http.Request) {
	s.handleLyricsPublication(w, r, false)
}

func (s *Server) handleLyricsPublication(w http.ResponseWriter, r *http.Request, publish bool) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		MusicID  int    `json:"musicId"`
		Revision int    `json:"revision"`
		ClientID string `json:"clientId"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.MusicID <= 0 || request.Revision <= 0 {
		writeContractError(w, http.StatusUnprocessableEntity, "incomplete_publication", []string{"musicId and revision must be positive"}, nil)
		return
	}
	if _, pluralErr := s.store.GetLyricsRenditionDocument(request.MusicID); pluralErr == nil {
		writeContractError(w, http.StatusUnprocessableEntity, "unsupported_publication", []string{
			"source-v3 rendition publication is batch-bound to recovery Public v3 and cannot use the legacy publish route",
		}, nil)
		return
	} else if !errors.Is(pluralErr, store.ErrLyricsNotFound) {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	if _, err := s.store.GetLyrics(request.MusicID); err != nil {
		if errors.Is(err, store.ErrLyricsNotFound) {
			writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		} else {
			writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		}
		return
	}
	var result model.SongLyrics
	var changed bool
	var err error
	if publish {
		result, changed, err = s.store.PublishLyricsMutation(request.MusicID, request.Revision, currentUser(r))
	} else {
		result, changed, err = s.store.UnpublishLyricsMutation(request.MusicID, request.Revision, currentUser(r))
	}
	if err != nil {
		writeLyricsError(w, err)
		return
	}
	if changed {
		if s.collab != nil {
			if resetErr := s.resetLyricsCollaboration(result.MusicID); resetErr != nil {
				s.reportLyricsInvariant("[lyrics] collaboration publication reset failed musicId=%d: %v", result.MusicID, resetErr)
			}
		}
		s.broadcastLyricsUpdated(result, request.ClientID, currentUser(r))
		if s.fileService != nil {
			s.fileService.PublishNow()
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) broadcastLyricsUpdated(lyrics model.SongLyrics, clientID, user string) {
	s.broadcastLyricsDocumentUpdated(lyrics.MusicID, lyrics.Revision, clientID, user)
}

func (s *Server) resetLyricsCollaboration(musicID int) error {
	if s.collab == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.collab.ReplaceFromAuthoritative(ctx, musicID)
}

func (s *Server) broadcastLyricsDocumentUpdated(musicID, revision int, clientID, user string) {
	s.broadcast(sse.EventLyricsUpdated, map[string]any{
		"musicId": musicID, "revision": revision, "clientId": clientID, "user": user,
	})
}

func (s *Server) broadcastLyricsRenditionUpdated(saved store.LyricsRenditionDocument, targets []store.LyricsRenditionMutationTarget, clientID, user string) {
	payload := map[string]any{
		"musicId": saved.MusicID, "revision": saved.Revision, "clientId": clientID, "user": user,
	}
	if saved.TranslationEditionKey != "" {
		payload["editionKey"] = saved.TranslationEditionKey
	}
	if len(targets) == 1 {
		payload["renditionKey"] = targets[0].RenditionKey
		payload["side"] = targets[0].Side
		payload["locale"] = targets[0].Locale
	}
	s.broadcast(sse.EventLyricsUpdated, payload)
}

func (s *Server) handleLyricsSourceSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	musicID, ok := positiveIntQuery(w, r, "musicId")
	if !ok {
		return
	}
	identity, ok := s.lyricsSourceIdentity(w, musicID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	items, err := s.lyricsSrc.Search(ctx, identity)
	if err != nil {
		writeLyricsSourceError(w, err)
		return
	}
	if items == nil {
		items = []lyricssource.Candidate{}
	}
	if err := s.store.RecordAudit(currentUser(r), "lyrics.source.search", fmt.Sprintf("musicId=%d candidates=%d", musicID, len(items))); err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleLyricsSourcePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		MusicID    int `json:"musicId"`
		PageID     int `json:"pageId"`
		RevisionID int `json:"revisionId"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.MusicID <= 0 || request.PageID <= 0 || request.RevisionID <= 0 {
		writeContractError(w, http.StatusBadRequest, "invalid_source_request",
			[]string{"musicId, pageId, and revisionId must be positive integers"}, nil)
		return
	}
	if existing, err := s.store.GetLyrics(request.MusicID); err == nil && (existing.Status != "draft" || existing.SourceURL != "") {
		writeContractError(w, http.StatusUnprocessableEntity, "source_drift",
			[]string{"verified source previews are only available before the first lyrics save or on an unprovenanced draft"}, nil)
		return
	} else if err != nil && !errors.Is(err, store.ErrLyricsNotFound) {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	identity, ok := s.lyricsSourceIdentity(w, request.MusicID)
	if !ok {
		return
	}
	producerBefore := s.editorGate.Status()
	if !producerStatusStopped(producerBefore) {
		writeContractError(w, http.StatusConflict, "producer_state_changed",
			[]string{"the producer must be stopped and fully completed before previewing lyrics"}, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	preview, err := s.lyricsSrc.Preview(ctx, identity, request.PageID, request.RevisionID)
	if err != nil {
		writeLyricsSourceError(w, err)
		return
	}
	producerAfter := s.editorGate.Status()
	if !producerStatusStopped(producerAfter) || !sameProducerStatus(producerBefore, producerAfter) {
		writeContractError(w, http.StatusConflict, "producer_state_changed",
			[]string{"the producer state changed while the external source was being fetched; retry the preview"}, nil)
		return
	}
	identityAfter, ok := s.lyricsSourceIdentity(w, request.MusicID)
	if !ok {
		return
	}
	if identityAfter != identity {
		writeContractError(w, http.StatusUnprocessableEntity, "source_drift",
			[]string{"catalog title or producer identity changed while the external source was being fetched; retry the preview"}, nil)
		return
	}
	// Enforce the exact requested MediaWiki revision identity and authoritative
	// persisted-URL transport policy before issuing a grant, even when tests or
	// alternate clients implement lyricsSourceClient.
	if preview.PageID != request.PageID || preview.RevisionID != request.RevisionID ||
		!lyricssource.HasCanonicalSHA1(preview.SHA1) || store.ValidateLyricsSourceRevisionURL(preview.CanonicalURL, preview.RevisionID) != nil {
		writeLyricsSourceError(w, lyricssource.ErrMalformedResponse)
		return
	}
	// The network fetch intentionally does not hold the content lock. Recheck
	// first-save eligibility after it completes so a concurrent first save can
	// never leave behind an otherwise valid one-use import grant.
	if _, err := s.store.GetLyricsDocument(request.MusicID); err == nil {
		writeContractError(w, http.StatusUnprocessableEntity, "source_drift",
			[]string{"lyrics were saved while the external source was being fetched; reload the document"}, nil)
		return
	} else if !errors.Is(err, store.ErrLyricsNotFound) {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	var grantToken string
	err = s.store.WithLyricsFirstSaveEligibility(request.MusicID, func() error {
		var issueErr error
		grantToken, issueErr = s.issueLyricsImportGrant(currentUser(r), request.MusicID, preview, producerAfter, identityAfter)
		return issueErr
	})
	if err != nil {
		if errors.Is(err, store.ErrLyricsAlreadySaved) {
			writeContractError(w, http.StatusUnprocessableEntity, "source_drift",
				[]string{"lyrics were saved while the verified source grant was being issued; reload the document"}, nil)
			return
		}
		if errors.Is(err, lyricssource.ErrMalformedResponse) {
			writeLyricsSourceError(w, err)
		} else if errors.Is(err, errLyricsImportCapacity) {
			writeContractError(w, http.StatusServiceUnavailable, "source_import_capacity", []string{"all verified source grants are currently in use; retry later"}, nil)
		} else {
			writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		}
		return
	}
	if err := s.store.RecordAudit(currentUser(r), "lyrics.source.preview",
		fmt.Sprintf("musicId=%d pageId=%d revisionId=%d", request.MusicID, request.PageID, request.RevisionID)); err != nil {
		s.lyricsImportMu.Lock()
		delete(s.lyricsImports, grantToken)
		s.lyricsImportMu.Unlock()
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return
	}
	preview.ImportToken = grantToken
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) lyricsSourceIdentity(w http.ResponseWriter, musicID int) (lyricssource.MusicIdentity, bool) {
	identity, err := s.store.CatalogMusicIdentity(musicID)
	if err == sql.ErrNoRows {
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		return lyricssource.MusicIdentity{}, false
	}
	if err != nil {
		writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
		return lyricssource.MusicIdentity{}, false
	}
	if strings.TrimSpace(identity.ProducerMetadata) == "" {
		writeContractError(w, http.StatusUnprocessableEntity, "source_identity_incomplete",
			[]string{"catalog producer metadata is required before source matching"}, nil)
		return lyricssource.MusicIdentity{}, false
	}
	return lyricssource.MusicIdentity{
		MusicID: identity.MusicID, JapaneseTitle: identity.JapaneseTitle,
		ProducerMetadata: identity.ProducerMetadata, Lyricist: identity.Lyricist,
		Composer: identity.Composer, Arranger: identity.Arranger,
		PerformerSegmentationPolicy: lyricssource.PerformerSegmentationPolicyFromCatalogVocals(identity.Vocals),
	}, true
}

func writeLyricsSourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, lyricssource.ErrRevisionChanged):
		writeContractError(w, http.StatusUnprocessableEntity, "source_drift",
			[]string{"the selected source revision changed"}, nil)
	case errors.Is(err, lyricssource.ErrAmbiguous):
		writeContractError(w, http.StatusUnprocessableEntity, "source_identity_mismatch",
			[]string{"the selected page no longer matches the catalog identity"}, nil)
	case errors.Is(err, lyricssource.ErrRestrictedReprint):
		writeContractError(w, http.StatusUnprocessableEntity, "source_restricted",
			[]string{"the source page explicitly prohibits lyrics, text reuse, or transcription"}, nil)
	case errors.Is(err, lyricssource.ErrMissingLyrics), errors.Is(err, lyricssource.ErrUnsupportedTable), errors.Is(err, lyricssource.ErrLyricsTooLarge):
		writeContractError(w, http.StatusUnprocessableEntity, "source_unsupported",
			[]string{"the source lyrics cannot be extracted safely"}, nil)
	default:
		// Transport failures carry the upstream URL and query; keep them server-side.
		log.Printf("[lyrics-source] request failed: %v", err)
		writeContractError(w, http.StatusBadGateway, "source_unavailable", nil, nil)
	}
}

func writeLyricsError(w http.ResponseWriter, err error) {
	var renditionErr *store.LyricsRenditionContractError
	if errors.As(err, &renditionErr) {
		status := http.StatusUnprocessableEntity
		if renditionErr.Code == "revision_conflict" {
			status = http.StatusConflict
		}
		writeContractError(w, status, renditionErr.Code, renditionErr.Details, renditionErr.Current)
		return
	}
	var contractErr *store.LyricsContractError
	if errors.As(err, &contractErr) {
		status := http.StatusUnprocessableEntity
		if contractErr.Code == "revision_conflict" {
			status = http.StatusConflict
		}
		writeContractError(w, status, contractErr.Code, contractErr.Details, contractErr.Current)
		return
	}
	if err == store.ErrLyricsNotFound || err == sql.ErrNoRows {
		writeContractError(w, http.StatusNotFound, "not_found", nil, nil)
		return
	}
	writeContractError(w, http.StatusInternalServerError, "internal_error", nil, nil)
}

func writeContractError(w http.ResponseWriter, status int, code string, details []string, current any) {
	body := map[string]any{"error": code}
	if len(details) > 0 {
		body["details"] = details
	}
	if current != nil {
		body["current"] = current
	}
	writeJSON(w, status, body)
}

func positiveIntQuery(w http.ResponseWriter, r *http.Request, key string) (int, bool) {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || value <= 0 {
		writeContractError(w, http.StatusBadRequest, "invalid_query", []string{key + " must be a positive integer"}, nil)
		return 0, false
	}
	return value, true
}

func pageParams(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"limit must be between 1 and 100"}, nil)
			return 0, 0, false
		}
		limit = value
	}
	cursor := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			writeContractError(w, http.StatusBadRequest, "invalid_query", []string{"cursor must be a non-negative musicId"}, nil)
			return 0, 0, false
		}
		cursor = value
	}
	return limit, cursor, true
}
