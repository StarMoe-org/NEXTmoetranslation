package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"sync"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/store"
)

// lyricsSourceRegistry keeps the provider registry swappable so an admin edit
// of the stored Sekaipedia map takes effect without a restart. It satisfies
// lyricsSourceClient itself, so the search and preview handlers always read the
// currently installed registry.
type lyricsSourceRegistry struct {
	mu       sync.RWMutex
	registry *lyricssource.Registry
}

func (h *lyricsSourceRegistry) set(registry *lyricssource.Registry) {
	h.mu.Lock()
	h.registry = registry
	h.mu.Unlock()
}

func (h *lyricsSourceRegistry) current() *lyricssource.Registry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.registry
}

func (h *lyricsSourceRegistry) Search(ctx context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
	return h.current().Search(ctx, identity)
}

func (h *lyricsSourceRegistry) Preview(ctx context.Context, identity lyricssource.MusicIdentity, pageID, revisionID int) (lyricssource.Preview, error) {
	return h.current().Preview(ctx, identity, pageID, revisionID)
}

func sekaipediaProviderConfigs(
	targets []store.LyricsProviderPageTarget,
	aliases []store.LyricsProviderContributorAlias,
) []lyricssource.ProviderConfig {
	pageTargets := make([]lyricssource.SekaipediaPageTarget, 0, len(targets))
	for _, target := range targets {
		pageTargets = append(pageTargets, lyricssource.SekaipediaPageTarget{
			MusicID:           target.MusicID,
			PageTitle:         target.PageTitle,
			ResolvedPageTitle: target.ResolvedPageTitle,
		})
	}
	contributorAliases := make([]lyricssource.ProviderContributorAlias, 0, len(aliases))
	for _, alias := range aliases {
		contributorAliases = append(contributorAliases, lyricssource.ProviderContributorAlias{
			MusicID:             alias.MusicID,
			CatalogContributor:  alias.CatalogContributor,
			ProviderContributor: alias.ProviderContributor,
		})
	}
	return append([]lyricssource.ProviderConfig{
		lyricssource.ReviewedSekaipediaProviderConfig(pageTargets, contributorAliases),
	}, lyricssource.DefaultProviderConfigs()...)
}

func loadSekaipediaRegistry(s *store.Store) (*lyricssource.Registry, error) {
	targets, aliases, err := s.LyricsProviderTargets(string(lyricssource.ProviderSekaipedia))
	if err != nil {
		return nil, err
	}
	return lyricssource.NewRegistry(sekaipediaProviderConfigs(targets, aliases)...)
}

// reloadLyricsSourceRegistry installs the stored provider map on the running
// server. It runs after a committed mutation, whose candidate map was already
// accepted by the registry validator.
func (s *Server) reloadLyricsSourceRegistry() error {
	registry, err := loadSekaipediaRegistry(s.store)
	if err != nil {
		return err
	}
	s.lyricsRegistry.set(registry)
	return nil
}

type lyricsProviderAliasItem struct {
	CatalogContributor  string `json:"catalogContributor"`
	ProviderContributor string `json:"providerContributor"`
}

type lyricsProviderTargetItem struct {
	MusicID           int                       `json:"musicId"`
	PageTitle         string                    `json:"pageTitle"`
	ResolvedPageTitle string                    `json:"resolvedPageTitle"`
	Aliases           []lyricsProviderAliasItem `json:"aliases"`
	UpdatedAt         int64                     `json:"updatedAt"`
	UpdatedBy         string                    `json:"updatedBy"`
}

func lyricsProviderTargetItems(
	targets []store.LyricsProviderPageTarget,
	aliases []store.LyricsProviderContributorAlias,
) []lyricsProviderTargetItem {
	grouped := map[int][]lyricsProviderAliasItem{}
	for _, alias := range aliases {
		grouped[alias.MusicID] = append(grouped[alias.MusicID], lyricsProviderAliasItem{
			CatalogContributor:  alias.CatalogContributor,
			ProviderContributor: alias.ProviderContributor,
		})
	}
	items := make([]lyricsProviderTargetItem, 0, len(targets))
	for _, target := range targets {
		item := lyricsProviderTargetItem{
			MusicID:           target.MusicID,
			PageTitle:         target.PageTitle,
			ResolvedPageTitle: target.ResolvedPageTitle,
			Aliases:           grouped[target.MusicID],
			UpdatedAt:         target.UpdatedAt,
			UpdatedBy:         target.UpdatedBy,
		}
		if item.Aliases == nil {
			item.Aliases = []lyricsProviderAliasItem{}
		}
		items = append(items, item)
	}
	return items
}

// handleLyricsProviderTargets lists the stored Sekaipedia page-target map.
//
// GET /api/admin/lyrics-providers/sekaipedia/targets
func (s *Server) handleLyricsProviderTargets(w http.ResponseWriter, _ *http.Request) {
	targets, aliases, err := s.store.LyricsProviderTargets(string(lyricssource.ProviderSekaipedia))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": lyricsProviderTargetItems(targets, aliases)})
}

// handleLyricsProviderTarget upserts or deletes one song's page target.
//
// PUT    /api/admin/lyrics-providers/sekaipedia/targets/{musicId} {pageTitle, resolvedPageTitle?, aliases?}
// DELETE /api/admin/lyrics-providers/sekaipedia/targets/{musicId}
func (s *Server) handleLyricsProviderTarget(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.handleUpsertLyricsProviderTarget(w, r)
	case http.MethodDelete:
		s.handleDeleteLyricsProviderTarget(w, r)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func lyricsProviderTargetMusicID(w http.ResponseWriter, r *http.Request) (int, bool) {
	musicID, err := strconv.Atoi(r.PathValue("musicId"))
	if err != nil || musicID <= 0 {
		writeContractError(w, http.StatusBadRequest, "invalid_music_id",
			[]string{"musicId must be a positive integer"}, nil)
		return 0, false
	}
	return musicID, true
}

// Mutations validate the whole would-be map against a snapshot before
// writing, so they run one at a time to keep the stored map valid.
func (s *Server) handleUpsertLyricsProviderTarget(w http.ResponseWriter, r *http.Request) {
	musicID, ok := lyricsProviderTargetMusicID(w, r)
	if !ok {
		return
	}
	s.lyricsProviderMu.Lock()
	defer s.lyricsProviderMu.Unlock()
	var request struct {
		PageTitle         string                    `json:"pageTitle"`
		ResolvedPageTitle string                    `json:"resolvedPageTitle"`
		Aliases           []lyricsProviderAliasItem `json:"aliases"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	provider := string(lyricssource.ProviderSekaipedia)
	targets, aliases, err := s.store.LyricsProviderTargets(provider)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	target := store.LyricsProviderPageTarget{
		MusicID:           musicID,
		PageTitle:         request.PageTitle,
		ResolvedPageTitle: request.ResolvedPageTitle,
	}
	songAliases := make([]store.LyricsProviderContributorAlias, 0, len(request.Aliases))
	for _, alias := range request.Aliases {
		songAliases = append(songAliases, store.LyricsProviderContributorAlias{
			MusicID:             musicID,
			CatalogContributor:  alias.CatalogContributor,
			ProviderContributor: alias.ProviderContributor,
		})
	}
	candidateTargets, candidateAliases := replaceLyricsProviderTarget(targets, aliases, target, songAliases)
	if _, err := lyricssource.NewRegistry(sekaipediaProviderConfigs(candidateTargets, candidateAliases)...); err != nil {
		writeContractError(w, http.StatusUnprocessableEntity, "invalid_provider_target", []string{err.Error()}, nil)
		return
	}
	if err := s.store.UpsertLyricsProviderTarget(provider, target, songAliases, currentUser(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.reloadLyricsSourceRegistry(); err != nil {
		log.Printf("[lyrics] provider target reload failed: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeLyricsProviderTargetItem(w, provider, musicID)
}

func (s *Server) handleDeleteLyricsProviderTarget(w http.ResponseWriter, r *http.Request) {
	musicID, ok := lyricsProviderTargetMusicID(w, r)
	if !ok {
		return
	}
	s.lyricsProviderMu.Lock()
	defer s.lyricsProviderMu.Unlock()
	provider := string(lyricssource.ProviderSekaipedia)
	removed, err := s.store.DeleteLyricsProviderTarget(provider, musicID, currentUser(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeContractError(w, http.StatusNotFound, "provider_target_not_found", nil, nil)
		return
	}
	if err := s.reloadLyricsSourceRegistry(); err != nil {
		log.Printf("[lyrics] provider target reload failed: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"musicId": musicID, "deleted": true})
}

func (s *Server) writeLyricsProviderTargetItem(w http.ResponseWriter, provider string, musicID int) {
	targets, aliases, err := s.store.LyricsProviderTargets(provider)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, item := range lyricsProviderTargetItems(targets, aliases) {
		if item.MusicID == musicID {
			writeJSON(w, http.StatusOK, item)
			return
		}
	}
	writeContractError(w, http.StatusNotFound, "provider_target_not_found", nil, nil)
}

// replaceLyricsProviderTarget builds the map a mutation would store, keeping
// the music-ID order the registry validator requires.
func replaceLyricsProviderTarget(
	targets []store.LyricsProviderPageTarget,
	aliases []store.LyricsProviderContributorAlias,
	target store.LyricsProviderPageTarget,
	songAliases []store.LyricsProviderContributorAlias,
) ([]store.LyricsProviderPageTarget, []store.LyricsProviderContributorAlias) {
	mergedTargets := make([]store.LyricsProviderPageTarget, 0, len(targets)+1)
	inserted := false
	for _, existing := range targets {
		switch {
		case existing.MusicID == target.MusicID:
			continue
		case existing.MusicID > target.MusicID && !inserted:
			mergedTargets = append(mergedTargets, target)
			inserted = true
		}
		mergedTargets = append(mergedTargets, existing)
	}
	if !inserted {
		mergedTargets = append(mergedTargets, target)
	}
	mergedAliases := make([]store.LyricsProviderContributorAlias, 0, len(aliases)+len(songAliases))
	for _, existing := range aliases {
		if existing.MusicID != target.MusicID {
			mergedAliases = append(mergedAliases, existing)
		}
	}
	mergedAliases = append(mergedAliases, songAliases...)
	return mergedTargets, mergedAliases
}
