package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestLyricsTranslationEditionEditorRoutesAndFailClosedSelection(t *testing.T) {
	h := setupLegacyAPI(t)
	seedAPISourceV3Lyrics(t, h, 765)

	response := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=765", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initial detail status=%d", response.StatusCode)
	}
	var initial store.LyricsRenditionDocument
	if err := json.NewDecoder(response.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	if initial.TranslationEditionKey != "main" || initial.DefaultTranslationEditionKey != "main" || len(initial.TranslationEditions) != 1 {
		t.Fatalf("initial edition selector=%+v", initial)
	}

	createdResponse := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/translation-editions", map[string]any{
		"musicId": 765, "revision": initial.Revision, "operation": "create",
		"editionKey": "alternate", "label": "另一译本", "clientId": "edition-window",
	})
	defer createdResponse.Body.Close()
	if createdResponse.StatusCode != http.StatusOK {
		t.Fatalf("create edition status=%d", createdResponse.StatusCode)
	}
	var created store.LyricsRenditionDocument
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.TranslationEditionKey != "alternate" || created.DefaultTranslationEditionKey != "main" || len(created.TranslationEditions) != 2 {
		t.Fatalf("created edition=%+v", created)
	}

	selected := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=765&translationEditionKey=alternate", nil)
	defer selected.Body.Close()
	if selected.StatusCode != http.StatusOK {
		t.Fatalf("selected edition status=%d", selected.StatusCode)
	}
	var selectedDocument store.LyricsRenditionDocument
	if err := json.NewDecoder(selected.Body).Decode(&selectedDocument); err != nil {
		t.Fatal(err)
	}
	if selectedDocument.TranslationEditionKey != "alternate" {
		t.Fatalf("selected edition=%q", selectedDocument.TranslationEditionKey)
	}

	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/api/lyrics/detail?musicId=765&translationEditionKey=Bad", want: http.StatusBadRequest},
		{path: "/api/lyrics/detail?musicId=765&translationEditionKey=missing", want: http.StatusNotFound},
		{path: "/api/lyrics/detail?musicId=765&translationEditionKey=main&translationEditionKey=alternate", want: http.StatusBadRequest},
		{path: "/api/lyrics/detail?musicId=765&translationEditionKey=main;ignored=x", want: http.StatusBadRequest},
	} {
		response := authorizedRequest(t, h, http.MethodGet, test.path, nil)
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Fatalf("GET %s status=%d want=%d", test.path, response.StatusCode, test.want)
		}
	}

	unknown := strictTranslationEditionRequest(t, h, map[string]any{
		"musicId": 765, "revision": created.Revision, "operation": "rename",
		"editionKey": "alternate", "label": "重命名", "clientId": "strict-window", "unexpected": true,
	})
	defer unknown.Body.Close()
	if unknown.StatusCode != http.StatusBadRequest {
		t.Fatalf("strict unknown-field status=%d", unknown.StatusCode)
	}

	invalidUnion := strictTranslationEditionRequest(t, h, map[string]any{
		"musicId": 765, "revision": created.Revision, "operation": "set-default",
		"editionKey": "alternate", "label": "not allowed", "clientId": "strict-window",
	})
	defer invalidUnion.Body.Close()
	if invalidUnion.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid union status=%d", invalidUnion.StatusCode)
	}
}

func TestLyricsTranslationEditionMetadataBroadcastIsSongLevel(t *testing.T) {
	h := setupLegacyAPI(t)
	seedAPISourceV3Lyrics(t, h, 766)

	initialResponse := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=766", nil)
	defer initialResponse.Body.Close()
	if initialResponse.StatusCode != http.StatusOK {
		t.Fatalf("initial detail status=%d", initialResponse.StatusCode)
	}
	var initial store.LyricsRenditionDocument
	if err := json.NewDecoder(initialResponse.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}

	streamContext, cancelStream := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStream()
	streamRequest := bearerSSERequest(t, h.server.URL, h.token).WithContext(streamContext)
	stream, err := http.DefaultClient.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("SSE status=%d content-type=%q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	events := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewReader(stream.Body)
		var event string
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "lyrics.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil {
					events <- data
				}
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	created := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/translation-editions", map[string]any{
		"musicId": 766, "revision": initial.Revision, "operation": "create",
		"editionKey": "alternate", "label": "另一译本", "clientId": "metadata-window",
	})
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create edition status=%d", created.StatusCode)
	}

	select {
	case data := <-events:
		if data["musicId"] != float64(766) || data["revision"] != float64(initial.Revision+1) ||
			data["clientId"] != "metadata-window" || data["user"] != "alice" {
			t.Fatalf("song-level edition metadata payload=%#v", data)
		}
		for _, field := range []string{"editionKey", "renditionKey", "side", "locale"} {
			if _, scoped := data[field]; scoped {
				t.Fatalf("song-level edition metadata event unexpectedly included %s: %#v", field, data)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for song-level edition metadata event")
	}
}

func TestLegacyLyricsRejectExplicitTranslationEditionSelection(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)

	saved := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", apiLyrics())
	saved.Body.Close()
	if saved.StatusCode != http.StatusOK {
		t.Fatalf("legacy save status=%d", saved.StatusCode)
	}

	plain := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=10", nil)
	plain.Body.Close()
	if plain.StatusCode != http.StatusOK {
		t.Fatalf("legacy detail status=%d", plain.StatusCode)
	}

	explicit := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=10&translationEditionKey=main", nil)
	defer explicit.Body.Close()
	if explicit.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy explicit edition status=%d want=%d", explicit.StatusCode, http.StatusNotFound)
	}
	var body map[string]any
	if err := json.NewDecoder(explicit.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "translation_edition_not_found" {
		t.Fatalf("legacy explicit edition body=%#v", body)
	}

	for _, path := range []string{
		"/api/lyrics/detail?musicId=10&translationEditionKey=Bad",
		"/api/lyrics/detail?musicId=10&translationEditionKey=",
	} {
		invalid := authorizedRequest(t, h, http.MethodGet, path, nil)
		var invalidBody map[string]any
		decodeErr := json.NewDecoder(invalid.Body).Decode(&invalidBody)
		invalid.Body.Close()
		if invalid.StatusCode != http.StatusBadRequest || decodeErr != nil || invalidBody["error"] != "invalid_translation_edition" {
			t.Fatalf("legacy invalid edition GET %s status=%d body=%#v decodeErr=%v", path, invalid.StatusCode, invalidBody, decodeErr)
		}
	}
}

func strictTranslationEditionRequest(t *testing.T, h *legacyAPIHarness, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/editor/v1/lyrics/translation-editions", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(loadedProducerStateHeader, loadedState(h.api.editorGate.Status()))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func seedAPISourceV3Lyrics(t *testing.T, h *legacyAPIHarness, musicID int) {
	t.Helper()
	identityKey := "sekai-source"
	ref := &model.LyricsSourceComponentRef{RenditionKey: identityKey}
	identity := model.LyricsSourceFixedIdentity{
		Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
		PageID: 2202, RevisionID: 1202, SHA1: strings.Repeat("a", 40), Title: "API source v3",
		CanonicalURL:      "https://www.sekaipedia.org/wiki/API_source_v3?oldid=1202",
		RevisionTimestamp: "2026-08-14T00:00:00Z", FetchedAt: "2026-08-14T00:01:00Z",
		Categories: []string{"Test"}, Section: "Lyrics", RenditionKey: identityKey,
		CompositionRenditionKey: "sekai", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{EvidenceID: "api-source-v3", SHA256: strings.Repeat("b", 64)}},
	}
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersionV3,
		FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
		Renditions: []model.LyricsSourceRendition{{
			RenditionKey: "sekai", SourceKind: model.LyricsSourceRenditionSekai,
			SourceTabPaths:        []model.LyricsSourceTabPath{{"SEKAI Version"}},
			ReasonCode:            model.LyricsSourceVersionReasonUntaggedFullOnly,
			FullPerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
			GamePerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
			Full: &model.LyricsSourceFull{
				Version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"}, Performers: []model.LyricsSourcePerformer{},
				Lines: []model.LyricsSourceFullLine{{
					ID: "full-000001", Text: "la",
					Segments:             []model.LyricsSourceSegment{{Text: "la", PerformerIDs: []string{}, Ruby: []model.LyricsSourceRubySpan{{Text: "la"}}}},
					TrailingPerformerIDs: []string{},
				}},
			},
			Relation: model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
			Provenance: model.LyricsSourceRenditionProvenance{
				FullText: ref, RelationEvidence: *ref, VersionEvidence: *ref,
			},
		}},
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("API source-v3 fixture: %v", err)
	}
	documentJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	documentDigest := sha256.Sum256(documentJSON)
	if _, err := h.db.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (?,?)`, musicID, "API source v3"); err != nil {
		t.Fatal(err)
	}
	result, err := h.db.Exec(`INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?)`, musicID, 3, "", string(documentJSON), hex.EncodeToString(documentDigest[:]), strings.Repeat("c", 64), int64(1786665600))
	if err != nil {
		t.Fatal(err)
	}
	documentID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	identityDigest := sha256.Sum256(identityJSON)
	categoriesJSON, _ := json.Marshal(identity.Categories)
	evidenceJSON, _ := json.Marshal(identity.IndexEvidenceRefs)
	if _, err := h.db.Exec(`INSERT INTO song_lyrics_source_artifacts
		(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
		 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, identity.Provider, identity.RenditionKey,
		identity.Origin, identity.PageID, identity.RevisionID, identity.RevisionTimestamp, identity.SHA1, identity.Title,
		identity.CanonicalURL, identity.FetchedAt, string(categoriesJSON), identity.Section, identity.CompositionRenditionKey,
		identity.VersionReason, string(evidenceJSON), string(identityJSON), hex.EncodeToString(identityDigest[:]), 1,
		strings.Repeat("d", 64), strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		digest := sha256.Sum256([]byte(hex.EncodeToString(documentDigest[:]) + "\x00" + binding.ComponentKey + "\x00" + binding.FixedIdentityKey))
		if _, err := h.db.Exec(`INSERT INTO song_lyrics_component_contributions
			(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, documentID,
			binding.ComponentKey, binding.FixedIdentityKey, hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec(`INSERT INTO song_lyrics_rendition_localizations
		(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
		VALUES (?,?,?,?,?,?,?,?)`, documentID, "sekai", "zh-CN", "API Translator", "", int64(1786665600), "seed", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO song_lyrics_rendition_translation_lines
		(document_id,rendition_key,locale,position,text) VALUES (?,?,?,?,?)`, documentID, "sekai", "zh-CN", 0, "默认译文"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,?,?)`, int64(1786665600), "seed", "test.seed", fmt.Sprintf("musicId=%d", musicID)); err != nil {
		t.Fatal(err)
	}
}
