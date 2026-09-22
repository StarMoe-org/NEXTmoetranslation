package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/lyricssource"
)

func decodeProviderTargets(t *testing.T, response *http.Response) []lyricsProviderTargetItem {
	t.Helper()
	defer response.Body.Close()
	var body struct {
		Items []lyricsProviderTargetItem `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Items
}

func registryPageTitle(t *testing.T, h *legacyAPIHarness, musicID int) string {
	t.Helper()
	for _, target := range h.api.lyricsRegistry.current().SekaipediaTargets() {
		if target.MusicID == musicID {
			return target.PageTitle
		}
	}
	return ""
}

func TestLyricsProviderTargetsListReturnsTheSeededSekaipediaMap(t *testing.T) {
	h := setupLegacyAPI(t)
	response := authorizedRequest(t, h, http.MethodGet, "/api/admin/lyrics-providers/sekaipedia/targets", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", response.StatusCode)
	}
	items := decodeProviderTargets(t, response)
	if len(items) != 37 {
		t.Fatalf("listed targets=%d want=37", len(items))
	}
	if items[0].MusicID != 50 || items[0].PageTitle != "Blessing" || len(items[0].Aliases) != 0 ||
		items[0].UpdatedBy != "migration-v36" {
		t.Fatalf("first item=%+v", items[0])
	}
	if items[6].MusicID != 148 || items[6].ResolvedPageTitle != "Ray" || len(items[6].Aliases) != 1 ||
		items[6].Aliases[0].CatalogContributor != "藤原 基央" || items[6].Aliases[0].ProviderContributor != "Motoo Fujiwara" {
		t.Fatalf("resolved-title item=%+v", items[6])
	}
	if got := registryPageTitle(t, h, 148); got != "ray" {
		t.Fatalf("registry page title for 148=%q", got)
	}
}

func TestLyricsProviderTargetUpsertInstallsTheNewRegistryWithoutRestart(t *testing.T) {
	h := setupLegacyAPI(t)
	if got := registryPageTitle(t, h, 728); got != "" {
		t.Fatalf("music 728 was already bound to %q", got)
	}
	response := authorizedRequest(t, h, http.MethodPut, "/api/admin/lyrics-providers/sekaipedia/targets/728",
		map[string]any{
			"pageTitle": "Mirai",
			"aliases":   []map[string]string{{"catalogContributor": "みきとP", "providerContributor": "MikitoP"}},
		})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upsert status=%d body=%s", response.StatusCode, body)
	}
	var item lyricsProviderTargetItem
	if err := json.NewDecoder(response.Body).Decode(&item); err != nil {
		t.Fatal(err)
	}
	if item.MusicID != 728 || item.PageTitle != "Mirai" || item.UpdatedBy != "alice" ||
		len(item.Aliases) != 1 || item.Aliases[0].ProviderContributor != "MikitoP" {
		t.Fatalf("upserted item=%+v", item)
	}
	if got := registryPageTitle(t, h, 728); got != "Mirai" {
		t.Fatalf("reloaded registry page title for 728=%q", got)
	}
	targets := h.api.lyricsRegistry.current().SekaipediaTargets()
	if len(targets) != 38 {
		t.Fatalf("reloaded registry targets=%d want=38", len(targets))
	}
	previous := 0
	for _, target := range targets {
		if target.MusicID <= previous {
			t.Fatalf("reloaded registry is not ordered by music ID: %d after %d", target.MusicID, previous)
		}
		previous = target.MusicID
	}
}

func TestLyricsProviderTargetUpsertRejectsADuplicateListTitleWithoutWriting(t *testing.T) {
	h := setupLegacyAPI(t)
	response := authorizedRequest(t, h, http.MethodPut, "/api/admin/lyrics-providers/sekaipedia/targets/728",
		map[string]any{"pageTitle": "blessing"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate title status=%d", response.StatusCode)
	}
	var contract struct {
		Error   string   `json:"error"`
		Details []string `json:"details"`
	}
	if err := json.NewDecoder(response.Body).Decode(&contract); err != nil {
		t.Fatal(err)
	}
	if contract.Error != "invalid_provider_target" || len(contract.Details) != 1 ||
		contract.Details[0] != "Sekaipedia page-target map contains a duplicate List page title" {
		t.Fatalf("duplicate title contract=%+v", contract)
	}
	targets, _, err := h.store.LyricsProviderTargets(string(lyricssource.ProviderSekaipedia))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 37 {
		t.Fatalf("rejected mutation wrote the store: targets=%d", len(targets))
	}
	if got := registryPageTitle(t, h, 728); got != "" {
		t.Fatalf("rejected mutation installed a registry target: %q", got)
	}
}

func TestLyricsProviderTargetDeleteRemovesTheTargetAndItsAliases(t *testing.T) {
	h := setupLegacyAPI(t)
	response := authorizedRequest(t, h, http.MethodDelete, "/api/admin/lyrics-providers/sekaipedia/targets/334", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete status=%d", response.StatusCode)
	}
	items := decodeProviderTargets(t, authorizedRequest(t, h, http.MethodGet,
		"/api/admin/lyrics-providers/sekaipedia/targets", nil))
	if len(items) != 36 {
		t.Fatalf("targets after delete=%d want=36", len(items))
	}
	for _, item := range items {
		if item.MusicID == 334 {
			t.Fatalf("deleted target survived: %+v", item)
		}
	}
	_, aliases, err := h.store.LyricsProviderTargets(string(lyricssource.ProviderSekaipedia))
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 24 {
		t.Fatalf("aliases after delete=%d want=24", len(aliases))
	}
	if got := registryPageTitle(t, h, 334); got != "" {
		t.Fatalf("deleted target is still installed: %q", got)
	}
	missing := authorizedRequest(t, h, http.MethodDelete, "/api/admin/lyrics-providers/sekaipedia/targets/334", nil)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("repeated delete status=%d", missing.StatusCode)
	}
}

func TestLyricsProviderTargetRoutesRejectEditors(t *testing.T) {
	h := setupLegacyAPI(t)
	editor, err := h.api.auth.CreateUser("editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/lyrics-providers/sekaipedia/targets"},
		{http.MethodPut, "/api/admin/lyrics-providers/sekaipedia/targets/728"},
		{http.MethodDelete, "/api/admin/lyrics-providers/sekaipedia/targets/334"},
	} {
		response := doJSON(t, operation.method, h.server.URL+operation.path, token, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s status=%d, want 403", operation.method, operation.path, response.StatusCode)
		}
	}
	targets, _, err := h.store.LyricsProviderTargets(string(lyricssource.ProviderSekaipedia))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 37 {
		t.Fatalf("editor mutations changed the store: targets=%d", len(targets))
	}
}
