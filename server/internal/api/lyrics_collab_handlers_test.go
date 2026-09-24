package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	ysync "github.com/reearth/ygo/sync"
	"moesekai/server/internal/collab"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

type lyricsCollabAPIHarness struct {
	legacy  *legacyAPIHarness
	server  *httptest.Server
	service *collab.Service
}

func setupLyricsCollabAPI(t *testing.T) *lyricsCollabAPIHarness {
	t.Helper()
	legacy := setupLegacyAPI(t)
	if err := legacy.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 42, JapaneseTitle: "collaboration contract",
	}}); err != nil {
		t.Fatal(err)
	}
	service, err := collab.New(legacy.db, legacy.store, legacy.api.auth, legacy.api.editorGate)
	if err != nil {
		t.Fatal(err)
	}
	legacy.api.SetCollab(service)
	mux := http.NewServeMux()
	legacy.api.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("shutdown collaboration service: %v", err)
		}
	})
	return &lyricsCollabAPIHarness{legacy: legacy, server: server, service: service}
}

func collabTicketRequest(
	t *testing.T,
	h *lyricsCollabAPIHarness,
	proof *editorgate.Status,
	body any,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/editor/v1/lyrics/42/collab-ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request.Body = io.NopCloser(bytes.NewReader(encoded))
		request.ContentLength = int64(len(encoded))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+h.legacy.token)
	if proof != nil {
		request.Header.Set(loadedProducerStateHeader, loadedState(*proof))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestLyricsCollabTicketHTTPProducerProofContract(t *testing.T) {
	h := setupLyricsCollabAPI(t)

	unauthorized, err := http.Post(h.server.URL+"/api/editor/v1/lyrics/42/collab-ticket", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized ticket status=%d", unauthorized.StatusCode)
	}

	missing := collabTicketRequest(t, h, nil, map[string]any{})
	missing.Body.Close()
	if missing.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("missing producer proof status=%d", missing.StatusCode)
	}

	stale := h.legacy.api.editorGate.Status()
	releaseProducer, err := h.legacy.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	releaseProducer()
	staleResponse := collabTicketRequest(t, h, &stale, map[string]any{})
	defer staleResponse.Body.Close()
	if staleResponse.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(staleResponse.Body)
		t.Fatalf("stale producer proof status=%d body=%s", staleResponse.StatusCode, body)
	}
	var current editorgate.Status
	if err := json.NewDecoder(staleResponse.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if current != h.legacy.api.editorGate.Status() {
		t.Fatalf("stale proof response=%+v current=%+v", current, h.legacy.api.editorGate.Status())
	}

	accepted := h.legacy.api.editorGate.Status()
	response := collabTicketRequest(t, h, &accepted, map[string]any{"musicId": 42})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("ticket status=%d body=%s", response.StatusCode, body)
	}
	assertNoStoreJSONHeaders(t, response)
	var ticket map[string]any
	if err := json.NewDecoder(response.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	if len(ticket) != 3 || ticket["room"] != "lyrics-42-e1" || ticket["ticket"] == "" {
		t.Fatalf("ticket response=%#v", ticket)
	}
	expiresAt, ok := ticket["expiresAt"].(string)
	if !ok {
		t.Fatalf("ticket expiresAt=%#v", ticket["expiresAt"])
	}
	parsedExpiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !parsedExpiry.After(time.Now()) {
		t.Fatalf("ticket expiry=%q err=%v", expiresAt, err)
	}
}

func TestLyricsCollabTicketHTTPBodyIsClosed(t *testing.T) {
	h := setupLyricsCollabAPI(t)
	proof := h.legacy.api.editorGate.Status()

	for _, body := range []any{nil, map[string]any{}, map[string]any{"musicId": 42}} {
		response := collabTicketRequest(t, h, &proof, body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("valid optional body %#v status=%d", body, response.StatusCode)
		}
	}

	mismatch := collabTicketRequest(t, h, &proof, map[string]any{"musicId": 43})
	mismatch.Body.Close()
	if mismatch.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched body musicId status=%d", mismatch.StatusCode)
	}

	unknown := collabTicketRequest(t, h, &proof, map[string]any{"unexpected": true})
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown ticket body field status=%d", unknown.StatusCode)
	}
}

// collabHarnessRequest posts to the collaboration harness, whose mux has the
// collaboration routes, with the current producer-state proof.
func collabHarnessRequest(t *testing.T, h *lyricsCollabAPIHarness, path string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.legacy.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(loadedProducerStateHeader, loadedState(h.legacy.api.editorGate.Status()))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// collabDraftClient is one console tab: it holds a single ticket and
// connection to a song's room across edits and checkpoints.
type collabDraftClient struct {
	t          *testing.T
	connection *websocket.Conn
	local      *crdt.Doc
}

func openCollabDraft(t *testing.T, h *lyricsCollabAPIHarness, musicID int) *collabDraftClient {
	t.Helper()
	response := collabHarnessRequest(t, h, fmt.Sprintf("/api/editor/v1/lyrics/%d/collab-ticket", musicID), map[string]any{"musicId": musicID})
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	err := json.NewDecoder(response.Body).Decode(&ticket)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("ticket status=%d err=%v", response.StatusCode, err)
	}
	wsURL := "ws" + strings.TrimPrefix(h.server.URL, "http") + fmt.Sprintf("/yjs/lyrics/%d?ticket=", musicID) + url.QueryEscape(ticket.Ticket)
	connection, upgrade, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if upgrade != nil {
			upgrade.Body.Close()
		}
		t.Fatal(err)
	}
	client := &collabDraftClient{t: t, connection: connection, local: crdt.New()}
	t.Cleanup(func() { connection.Close() })
	client.sync()
	return client
}

// send writes one sync protocol message; outer y-websocket type 0 carries it.
func (c *collabDraftClient) send(message []byte) {
	c.t.Helper()
	if err := c.connection.WriteMessage(websocket.BinaryMessage, append([]byte{0}, message...)); err != nil {
		c.t.Fatal(err)
	}
}

// sync asks for everything the server has and applies the reply. The server
// answers one connection's messages in order, so the reply follows every
// update this client sent before it.
func (c *collabDraftClient) sync() {
	c.t.Helper()
	c.send(ysync.EncodeSyncStep1(c.local))
	if err := c.connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	for {
		_, message, err := c.connection.ReadMessage()
		if err != nil {
			c.t.Fatal(err)
		}
		if len(message) == 0 || message[0] != 0 {
			continue
		}
		kind, payload, err := ysync.ReadSyncMessage(message[1:])
		if err == nil && kind == ysync.MsgSyncStep2 {
			if err := crdt.ApplyUpdateV1(c.local, payload, nil); err != nil {
				c.t.Fatal(err)
			}
			return
		}
	}
}

// setFullChinese replaces the first Full zh-CN line of the source-v3 draft and
// returns once the server has applied it. A non-empty editionKey also points
// the draft at that translation edition, as the console does when it switches.
func (c *collabDraftClient) setFullChinese(editionKey, chinese string) {
	c.t.Helper()
	c.sync()
	root := c.local.GetMap("lyrics")
	renditions, _ := root.Get("renditions")
	rendition, _ := renditions.(*crdt.YArray).Get(0).(*crdt.YMap)
	full, _ := rendition.Get("full")
	lines, _ := full.(*crdt.YMap).Get("lines")
	line, _ := lines.(*crdt.YArray).Get(0).(*crdt.YMap)
	value, _ := line.Get("zh-CN")
	text, ok := value.(*crdt.YText)
	if !ok {
		c.t.Fatalf("draft zh-CN type=%T", value)
	}
	editionValue, _ := root.Get("translationEditionKey")
	before := c.local.StateVector()
	c.local.Transact(func(txn *crdt.Transaction) {
		if editionKey != "" {
			if editionText, isText := editionValue.(*crdt.YText); isText {
				editionText.Delete(txn, 0, editionText.Len())
				editionText.Insert(txn, 0, editionKey, nil)
			} else {
				root.Set(txn, "translationEditionKey", editionKey)
			}
		}
		if text.Len() > 0 {
			text.Delete(txn, 0, text.Len())
		}
		text.Insert(txn, 0, chinese, nil)
	})
	c.send(ysync.EncodeUpdate(crdt.EncodeStateAsUpdateV1(c.local, before)))
	c.sync()
}

// editCollabDraftChinese joins the room with a ticket and replaces the first
// Full zh-CN line of the source-v3 draft, returning once the server has
// applied the edit.
func editCollabDraftChinese(t *testing.T, h *lyricsCollabAPIHarness, musicID int, chinese string) {
	t.Helper()
	client := openCollabDraft(t, h, musicID)
	client.setFullChinese("", chinese)
	client.connection.Close()
}

// A checkpoint that changes a source-v3 document is a published save, so it
// requests the immediate rebuild like PUT /api/editor/v1/lyrics/save does.
func TestLyricsCheckpointOfAChangedSourceV3DocumentPublishesImmediately(t *testing.T) {
	h := setupLyricsCollabAPI(t)
	recorder := &recordingFileService{}
	h.legacy.api.SetFileService(recorder)
	const musicID = 766
	seedAPISourceV3Lyrics(t, h.legacy, musicID)
	checkpoint := func() store.LyricsRenditionDocument {
		t.Helper()
		response := collabHarnessRequest(t, h, fmt.Sprintf("/api/editor/v1/lyrics/%d/checkpoint", musicID), map[string]any{"clientId": "checkpoint-client"})
		defer response.Body.Close()
		var document store.LyricsRenditionDocument
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("checkpoint status=%d err=%v", response.StatusCode, err)
		}
		return document
	}

	editCollabDraftChinese(t, h, musicID, "合成译文")
	saved := checkpoint()
	if saved.Renditions[0].Full.Lines[0].Chinese != "合成译文" || recorder.publishNow.Load() != 1 {
		t.Fatalf("changed checkpoint zh=%q publishNow=%d", saved.Renditions[0].Full.Lines[0].Chinese, recorder.publishNow.Load())
	}
	if unchanged := checkpoint(); unchanged.Revision != saved.Revision || recorder.publishNow.Load() != 1 {
		t.Fatalf("unchanged checkpoint revision=%d/%d publishNow=%d", unchanged.Revision, saved.Revision, recorder.publishNow.Load())
	}
}

// Consoles before the relation.lineIds fix edited Full without touching the
// exact-projection Game rows; such a room still checkpoints and Game follows Full.
func TestLyricsCheckpointProjectsStaleExactProjectionGameRows(t *testing.T) {
	h := setupLyricsCollabAPI(t)
	h.legacy.api.SetFileService(&recordingFileService{})
	const musicID = 990812
	if err := h.legacy.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: musicID, JapaneseTitle: "合成試験曲",
		Vocals: []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 1, CharacterSequence: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	if status, _, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h.legacy, http.MethodPut, lyricsDocumentRoute,
		lyricsDocumentAPIBody(musicID))); status != http.StatusOK {
		t.Fatalf("document status=%d body=%s", status, raw)
	}

	editCollabDraftChinese(t, h, musicID, "改过的第一行")
	response := collabHarnessRequest(t, h, fmt.Sprintf("/api/editor/v1/lyrics/%d/checkpoint", musicID), map[string]any{"clientId": "stale-game-client"})
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	var saved store.LyricsRenditionDocument
	if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(body, &saved) != nil || len(saved.Renditions) != 1 {
		t.Fatalf("checkpoint status=%d err=%v body=%s", response.StatusCode, err, body)
	}
	rendition := saved.Renditions[0]
	if rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection || rendition.Game == nil || len(rendition.Game.Lines) != 1 ||
		rendition.Full.Lines[0].Chinese != "改过的第一行" || rendition.Game.Lines[0].Chinese != "改过的第一行" {
		t.Fatalf("checkpoint rendition=%+v", rendition)
	}
}

// A checkpoint that saves a non-default translation edition leaves the room
// fingerprinted on the default edition, so later checkpoints still save.
func TestLyricsCheckpointsOfANonDefaultEditionKeepTheRoomUsable(t *testing.T) {
	h := setupLyricsCollabAPI(t)
	h.legacy.api.SetFileService(&recordingFileService{})
	const musicID = 990813
	if err := h.legacy.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: musicID, JapaneseTitle: "合成試験曲",
		Vocals: []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 1, CharacterSequence: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	body := lyricsDocumentAPIBody(musicID)
	body["translationEditions"] = []map[string]any{{"key": "main", "label": "合成主译本"}, {"key": "alt", "label": "合成别译本"}}
	body["renditions"].([]map[string]any)[0]["lines"].([]map[string]any)[0]["zhEditions"] = map[string]any{"alt": "别译第一行"}
	if status, _, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h.legacy, http.MethodPut, lyricsDocumentRoute, body)); status != http.StatusOK {
		t.Fatalf("document status=%d body=%s", status, raw)
	}

	client := openCollabDraft(t, h, musicID)
	for _, chinese := range []string{"别译第一次修改", "别译第二次修改"} {
		client.setFullChinese("alt", chinese)
		response := collabHarnessRequest(t, h, fmt.Sprintf("/api/editor/v1/lyrics/%d/checkpoint", musicID), map[string]any{"clientId": "edition-client"})
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		var saved store.LyricsRenditionDocument
		if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(raw, &saved) != nil {
			t.Fatalf("checkpoint %q status=%d err=%v body=%s", chinese, response.StatusCode, err, raw)
		}
		if saved.TranslationEditionKey != "alt" || saved.Renditions[0].Full.Lines[0].Chinese != chinese {
			t.Fatalf("checkpoint %q saved edition=%q zh=%q", chinese, saved.TranslationEditionKey, saved.Renditions[0].Full.Lines[0].Chinese)
		}
	}
	main, err := h.legacy.store.GetLyricsRenditionDocumentEdition(musicID, "main", true)
	if err != nil || main.Renditions[0].Full.Lines[0].Chinese != "唱起测试之歌" {
		t.Fatalf("main edition err=%v zh=%q", err, main.Renditions[0].Full.Lines[0].Chinese)
	}
}
