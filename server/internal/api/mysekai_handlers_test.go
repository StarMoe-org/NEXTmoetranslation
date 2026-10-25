package api

import (
	"encoding/json"
	"moesekai/server/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestMysekaiEditorAPIAndPublicationAdmission(t *testing.T) {
	h := setupLegacyAPI(t)
	hash := strings.Repeat("a", 64)
	catalog := map[string]any{"schema": "moly-text-source/1", "region": "jp", "revision": hash, "scripts": []any{map[string]any{"key": "synthetic", "lines": []any{map[string]any{"id": "one", "kind": "text", "step": 0, "text": "Original", "sourceHash": hash}}}}}
	response := doJSON(t, "POST", h.server.URL+"/api/editor/v1/mysekai/source", h.token, map[string]any{"catalog": catalog, "expectedRevision": ""})
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("source status %d", response.StatusCode)
	}
	draft := store.MysekaiSave{Region: "jp", Locale: "zh-CN", ScriptKey: "synthetic", SourceRevision: hash, Edits: []store.MysekaiEdit{{ID: "one", SourceHash: hash, Text: "Translation"}}}
	response = doJSON(t, "PUT", h.server.URL+"/api/editor/v1/mysekai/drafts", h.token, draft)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("save status %d", response.StatusCode)
	}
	response = doJSON(t, "GET", h.server.URL+"/api/mysekai/scripts?region=jp&locale=zh-CN", h.token, nil)
	var page store.MysekaiPage
	err := json.NewDecoder(response.Body).Decode(&page)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	request := store.MysekaiPublicationRequest{Region: "jp", Locale: "zh-CN", SourceRevision: hash, DraftRevision: page.DraftRevision}
	// Administrator's loaded draft snapshot cannot publish edits made later.
	draft.Edits[0].ExpectedRevision = 1
	draft.Edits[0].Text = "New translation"
	response = doJSON(t, "PUT", h.server.URL+"/api/editor/v1/mysekai/drafts", h.token, draft)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("save status %d", response.StatusCode)
	}
	response = doJSON(t, "POST", h.server.URL+"/api/editor/v1/mysekai/publication/publish", h.token, request)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale publish %d", response.StatusCode)
	}
	response = doJSON(t, "GET", h.server.URL+"/api/mysekai/scripts?region=jp&locale=zh-CN", h.token, nil)
	err = json.NewDecoder(response.Body).Decode(&page)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	request.DraftRevision = page.DraftRevision
	response = doJSON(t, "POST", h.server.URL+"/api/editor/v1/mysekai/publication/publish", h.token, request)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("unwired projection must be pending: %d", response.StatusCode)
	}
	var result struct {
		ProjectionReady bool `json:"projectionReady"`
	}
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.ProjectionReady {
		t.Fatal("unwired projection reported ready")
	}
}
