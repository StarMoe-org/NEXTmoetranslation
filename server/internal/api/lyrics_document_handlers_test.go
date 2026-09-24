package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lyricsperformers"
	"moesekai/server/internal/model"
	"moesekai/server/internal/publiclyricsbundle"
	"moesekai/server/internal/store"
)

const lyricsDocumentRoute = "/api/editor/v1/lyrics/document"

type lyricsDocumentFileService struct {
	*filesvc.Service
	publishNow int
}

func (f *lyricsDocumentFileService) PublishNow() {
	f.publishNow++
	f.Service.Rebuild()
}

func setupLyricsDocumentAPI(t *testing.T, musicID int) (*legacyAPIHarness, *lyricsDocumentFileService) {
	t.Helper()
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: musicID, JapaneseTitle: "合成試験曲",
		Vocals: []model.CatalogVocalSignal{
			{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 1, CharacterSequence: 1},
			{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 2, CharacterSequence: 2},
			{VocalID: 2, VocalType: "virtual_singer", CharacterType: "game_character", CharacterID: 21, CharacterSequence: 1},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	service := &lyricsDocumentFileService{Service: filesvc.New(h.store, h.events, files.NewGenerator(h.store, h.events, ""))}
	h.api.SetFileService(service)
	service.Rebuild()
	return h, service
}

func lyricsDocumentAPIBody(musicID int) map[string]any {
	return map[string]any{
		"musicId":           musicID,
		"source":            map[string]any{"url": "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242"},
		"translationCredit": "合成译者",
		"renditions": []map[string]any{{
			"key": "sekai", "game": "cut",
			"lines": []map[string]any{
				{"ja": "{試験|しけん}の{歌|うた}をうたう", "zh": "唱起测试之歌", "inGame": true},
				{"ja": "らららと{合成|ごうせい}する", "zh": "啦啦啦地合成", "stanzaBreakBefore": true},
			},
		}},
	}
}

func readLyricsDocumentResponse(t *testing.T, response *http.Response) (int, map[string]json.RawMessage, []byte) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("status=%d body=%s: %v", response.StatusCode, body, err)
	}
	return response.StatusCode, decoded, body
}

func serveLyricsDocument(service *lyricsDocumentFileService, musicID int) (int, []byte) {
	request := httptest.NewRequest(http.MethodGet, store.LyricsDocumentPublicPath(musicID), nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

func TestLyricsDocumentRoutePublishesTheServedV3Detail(t *testing.T) {
	const musicID = 990801
	h, service := setupLyricsDocumentAPI(t, musicID)

	dryBody := lyricsDocumentAPIBody(musicID)
	dryBody["dryRun"] = true
	status, dry, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, dryBody))
	if status != http.StatusOK || string(dry["dryRun"]) != "true" || string(dry["revision"]) != "2" ||
		string(dry["publicPath"]) != `"/files/translation/lyrics/music_990801.json"` {
		t.Fatalf("dry run status=%d body=%s", status, raw)
	}
	if code, _ := serveLyricsDocument(service, musicID); code != http.StatusNotFound || service.publishNow != 0 {
		t.Fatalf("dry run published: served=%d publishNow=%d", code, service.publishNow)
	}

	// Agent scripts send no producer-state header.
	status, published, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPut, h.server.URL+lyricsDocumentRoute, h.token,
		lyricsDocumentAPIBody(musicID)))
	if status != http.StatusOK || string(published["dryRun"]) != "false" || string(published["revision"]) != "2" || service.publishNow != 1 {
		t.Fatalf("publish status=%d publishNow=%d body=%s", status, service.publishNow, raw)
	}
	code, served := serveLyricsDocument(service, musicID)
	if code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(served), bytes.TrimSpace(published["document"])) {
		t.Fatalf("served status=%d\nserved=%s\nresponse=%s", code, served, published["document"])
	}
	detail, err := store.DecodePublicLyricsV3Detail(served)
	if err != nil {
		t.Fatal(err)
	}
	rendition := detail.Renditions[0]
	if detail.Version != 3 || detail.Revision != 2 || rendition.Game == nil || len(rendition.Game.Lines) != 1 {
		t.Fatalf("served detail=%+v", detail)
	}
	readings := map[string]string{}
	for _, line := range rendition.Full.Lines {
		for _, span := range line.Segments[0].Ruby {
			if span.Reading != "" {
				readings[span.Text] = span.Reading
			}
		}
	}
	if readings["試験"] != "しけん" || readings["歌"] != "うた" || readings["合成"] != "ごうせい" {
		t.Fatalf("served readings=%v", readings)
	}

	stale := lyricsDocumentAPIBody(musicID)
	stale["expectedRevision"] = 1
	status, conflict, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, stale))
	if status != http.StatusConflict || string(conflict["error"]) != `"revision_conflict"` || string(conflict["current"]) != `{"revision":2}` {
		t.Fatalf("stale status=%d body=%s", status, raw)
	}
	if service.publishNow != 1 {
		t.Fatalf("a conflict published: publishNow=%d", service.publishNow)
	}
}

func TestLyricsDocumentRouteErrors(t *testing.T) {
	const musicID = 990802
	h, service := setupLyricsDocumentAPI(t, musicID)

	invalid := lyricsDocumentAPIBody(musicID)
	invalid["renditions"].([]map[string]any)[0]["lines"] = []map[string]any{{"ja": "{試|し}験", "zh": "测试"}}
	status, body, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, invalid))
	var issues []map[string]any
	if status != http.StatusUnprocessableEntity || string(body["error"]) != `"invalid_lyrics_document"` ||
		json.Unmarshal(body["issues"], &issues) != nil || len(issues) != 1 {
		t.Fatalf("invalid status=%d body=%s", status, raw)
	}
	if issues[0]["rendition"] != "sekai" || issues[0]["side"] != "full" || issues[0]["line"] != float64(0) ||
		issues[0]["field"] != "ja" || issues[0]["message"] != "kanji without a {kanji|reading}: 「験」 at character 5" {
		t.Fatalf("issue=%v", issues[0])
	}

	unpinned := lyricsDocumentAPIBody(musicID)
	unpinned["source"] = map[string]any{"url": "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90"}
	if status, body, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, unpinned)); status != http.StatusUnprocessableEntity ||
		string(body["error"]) != `"source_revision_required"` || body["issues"] != nil {
		t.Fatalf("unpinned status=%d body=%s", status, raw)
	}

	if status, body, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, lyricsDocumentAPIBody(990899))); status != http.StatusNotFound ||
		string(body["error"]) != `"not_found"` {
		t.Fatalf("unknown music status=%d body=%s", status, raw)
	}

	unknownField := lyricsDocumentAPIBody(musicID)
	unknownField["renditions"].([]map[string]any)[0]["ruby"] = true
	if status, _, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, unknownField)); status != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", status, raw)
	}

	if status, _, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPost, lyricsDocumentRoute, lyricsDocumentAPIBody(musicID))); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d body=%s", status, raw)
	}

	editor, err := h.api.auth.CreateUser("document-editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	editorToken, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPut, h.server.URL+lyricsDocumentRoute, editorToken, lyricsDocumentAPIBody(musicID))); status != http.StatusForbidden {
		t.Fatalf("editor status=%d body=%s", status, raw)
	}
	if status, _, raw := readLyricsDocumentResponse(t, strictRequest(t, h, http.MethodPut, lyricsDocumentRoute, lyricsDocumentAPIBody(musicID), []string{"not-a-producer-state"})); status != http.StatusBadRequest {
		t.Fatalf("malformed producer state status=%d body=%s", status, raw)
	}
	if service.publishNow != 0 {
		t.Fatalf("a rejected request published: publishNow=%d", service.publishNow)
	}
}

// A song already served from the embedded public bundle is taken over by the
// database publication, whose revision stays above the bundle's.
func TestLyricsDocumentRouteTakesOverAnEmbeddedBundleSong(t *testing.T) {
	metadata, err := publiclyricsbundle.CatalogRuntimeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	musicIDs := make([]int, 0, len(metadata))
	for musicID, item := range metadata {
		if item.HasDetail && item.Revision > 0 {
			musicIDs = append(musicIDs, musicID)
		}
	}
	if len(musicIDs) == 0 {
		t.Fatal("embedded bundle has no detail")
	}
	sort.Ints(musicIDs)
	musicID := musicIDs[0]
	bundleRevision := metadata[musicID].Revision
	h, service := setupLyricsDocumentAPI(t, musicID)
	before, ok := service.SongProvenance(musicID)
	if !ok || before.Source != filesvc.SongSourceBundle || before.Revision != bundleRevision {
		t.Fatalf("bundle provenance before=%+v ok=%t", before, ok)
	}

	body := lyricsDocumentAPIBody(musicID)
	body["expectedRevision"] = bundleRevision
	status, published, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, body))
	if status != http.StatusOK {
		t.Fatalf("publish over bundle status=%d body=%s", status, raw)
	}
	var revision int
	if err := json.Unmarshal(published["revision"], &revision); err != nil || revision != bundleRevision+1 {
		t.Fatalf("revision=%s, bundle revision=%d", published["revision"], bundleRevision)
	}
	after, ok := service.SongProvenance(musicID)
	if !ok || after.Source == filesvc.SongSourceBundle || after.Revision != revision {
		t.Fatalf("provenance after=%+v ok=%t", after, ok)
	}
	code, served := serveLyricsDocument(service, musicID)
	if code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(served), bytes.TrimSpace(published["document"])) {
		t.Fatalf("served status=%d body=%s", code, served)
	}
}

func TestLyricsDocumentExportRouteReturnsTheServedSongAsARequest(t *testing.T) {
	const musicID = 990803
	h, service := setupLyricsDocumentAPI(t, musicID)
	editor, err := h.api.auth.CreateUser("export-editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	editorToken, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	exportURL := fmt.Sprintf("%s%s?musicId=%d", h.server.URL, lyricsDocumentRoute, musicID)

	// With nothing served or stored the 404 names the expectedRevision of a
	// creating PUT.
	status, body, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, exportURL, editorToken, nil))
	if status != http.StatusNotFound || string(body["error"]) != `"not_found"` || string(body["current"]) != `{"revision":0}` ||
		!bytes.Contains(body["details"], []byte("expectedRevision 0")) {
		t.Fatalf("unserved song status=%d body=%s", status, raw)
	}
	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, exportURL+"&from=elsewhere", editorToken, nil)); status != http.StatusBadRequest {
		t.Fatalf("unknown from status=%d body=%s", status, raw)
	}
	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, exportURL, "", nil)); status != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", status, raw)
	}
	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, h.server.URL+lyricsDocumentRoute+"?musicId=x", editorToken, nil)); status != http.StatusBadRequest {
		t.Fatalf("invalid musicId status=%d body=%s", status, raw)
	}

	status, published, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, lyricsDocumentAPIBody(musicID)))
	if status != http.StatusOK {
		t.Fatalf("publish status=%d body=%s", status, raw)
	}
	// An editor reads without a producer-state header while the producer holds
	// the content gate.
	releaseProducer, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	status, exported, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, exportURL, editorToken, nil))
	releaseProducer()
	if status != http.StatusOK || string(exported["musicId"]) != fmt.Sprint(musicID) || string(exported["servedVersion"]) != "3" ||
		string(exported["servedRevision"]) != string(published["revision"]) || string(exported["warnings"]) != "[]" ||
		string(exported["from"]) != `"served"` {
		t.Fatalf("export status=%d body=%s", status, raw)
	}
	var document map[string]any
	if err := json.Unmarshal(exported["document"], &document); err != nil {
		t.Fatal(err)
	}
	renditions, _ := document["renditions"].([]any)
	if document["expectedRevision"] != float64(2) || len(renditions) != 1 ||
		document["source"].(map[string]any)["url"] != lyricsDocumentAPIBody(musicID)["source"].(map[string]any)["url"] {
		t.Fatalf("exported document=%s", exported["document"])
	}
	rendition := renditions[0].(map[string]any)
	lines := rendition["lines"].([]any)
	if rendition["game"] != "cut" || len(lines) != 2 || lines[0].(map[string]any)["ja"] != "{試験|しけん}の{歌|うた}をうたう" ||
		lines[0].(map[string]any)["inGame"] != true || lines[1].(map[string]any)["stanzaBreakBefore"] != true {
		t.Fatalf("exported rendition=%v", rendition)
	}

	// The exported document is accepted by PUT as it stands.
	var request map[string]any
	if err := json.Unmarshal(exported["document"], &request); err != nil {
		t.Fatal(err)
	}
	request["dryRun"] = true
	if status, dry, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, request)); status != http.StatusOK ||
		string(dry["changes"]) != `{"against":"served","changed":false}` {
		t.Fatalf("PUT of the export status=%d body=%s", status, raw)
	}
	// An edit reports what it changes.
	edited := lyricsDocumentAPIBody(musicID)
	edited["expectedRevision"], edited["dryRun"] = 2, true
	edited["renditions"].([]map[string]any)[0]["lines"].([]map[string]any)[1]["zh"] = "啦啦啦合成了"
	status, dry, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, edited))
	var changes store.LyricsDocumentChanges
	if status != http.StatusOK || json.Unmarshal(dry["changes"], &changes) != nil || !changes.Changed || len(changes.Renditions) != 1 ||
		len(changes.Renditions[0].Sides) != 1 || !reflect.DeepEqual(changes.Renditions[0].Sides[0].Changed, []store.LyricsDocumentLineChange{{
		Before: 1, After: 1, Fields: []string{"zh"}, Chinese: &store.LyricsDocumentTextChange{Before: "啦啦啦地合成", After: "啦啦啦合成了"},
	}}) {
		t.Fatalf("edit dry run status=%d body=%s", status, raw)
	}

	// from=database reads the stored document, which is what the site serves.
	status, fromDatabase, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, exportURL+"&from=database", editorToken, nil))
	if status != http.StatusOK || string(fromDatabase["from"]) != `"database"` || !bytes.Equal(fromDatabase["document"], exported["document"]) ||
		string(fromDatabase["servedRevision"]) != string(published["revision"]) {
		t.Fatalf("database export status=%d body=%s", status, raw)
	}
	// A withdrawn song is served by nobody, so GET falls back to the database.
	if _, _, err := h.store.SetSourceV3LyricsWithdrawn(musicID, 2, true, "document-admin"); err != nil {
		t.Fatal(err)
	}
	service.Rebuild()
	status, fallback, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, exportURL, editorToken, nil))
	if status != http.StatusOK || string(fallback["from"]) != `"database"` || string(fallback["servedRevision"]) != "0" ||
		!bytes.Contains(fallback["warnings"], []byte(store.LyricsDocumentExportWarningWithdrawn)) ||
		!bytes.Equal(fallback["document"], exported["document"]) {
		t.Fatalf("withdrawn export status=%d body=%s", status, raw)
	}
}

// lyricsDocumentRoundTripTree decodes a v3 detail into a JSON tree. With
// allowlist set it normalizes, on both sides, the fields a PUT of the export
// regenerates or that pjsk.moe renders identically:
//   - revision and updatedAt: PUT assigns the next revision and the publish time.
//   - line ids and relation.lineIds: ids are positional names PUT regenerates
//     (full-000001, game-000001); the projection is compared by the Full
//     positions it selects.
//   - sourceTabPaths: where on the wiki page the text was found. It is source
//     provenance PUT derives from the label, and pjsk.moe does not render it.
//   - provenance: compared as the ordered distinct attributions, which is the
//     list pjsk.moe shows. Component names follow the shape PUT builds (a
//     projected Game has no game_* components, PUT adds relation and version).
//   - performers[].color equal to the audited registry colour: pjsk.moe falls
//     back to the same registry colour when the detail omits it.
//   - game.version.label: pjsk.moe shows rendition.label (the Full label, or
//     the Game label of a Game-only rendition) and only validates side labels;
//     PUT labels both sides with the rendition label.
//   - boundaries between adjacent segments with the same single performer or
//     no performer, and between adjacent unannotated ruby spans: both render
//     identically, and the export joins them into one request segment and one
//     markup string. pjsk.moe draws each multi-performer segment as its own
//     gradient, so those boundaries are compared.
func lyricsDocumentRoundTripTree(t *testing.T, detail store.PublicLyricsV3DetailDocument, allowlist bool) any {
	t.Helper()
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if allowlist {
		var normalized store.PublicLyricsV3DetailDocument
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			t.Fatal(err)
		}
		registryColors := map[string]string{}
		for _, performer := range lyricsperformers.All() {
			registryColors[performer.SourceID] = performer.Color
		}
		normalized.Revision, normalized.UpdatedAt = 0, ""
		for index := range normalized.Renditions {
			rendition := &normalized.Renditions[index]
			rendition.SourceTabPaths = nil
			for performerIndex, performer := range rendition.Performers {
				if performer.Color == registryColors[performer.PerformerID] {
					rendition.Performers[performerIndex].Color = ""
				}
			}
			if rendition.Game != nil {
				rendition.Game.Version.Label = ""
			}
			var attributions []store.PublicLyricsV3ComponentAttribution
			seen := map[store.PublicLyricsV3ComponentAttribution]bool{}
			for _, attribution := range rendition.Provenance {
				attribution.Component = ""
				if !seen[attribution] {
					seen[attribution] = true
					attributions = append(attributions, attribution)
				}
			}
			rendition.Provenance = attributions
			positions := map[string]string{}
			for _, side := range []*store.PublicLyricsV3Side{rendition.Full, rendition.Game} {
				if side == nil {
					continue
				}
				for lineIndex := range side.Lines {
					line := &side.Lines[lineIndex]
					if side == rendition.Full {
						positions[line.ID] = fmt.Sprintf("full#%d", lineIndex)
					}
					line.ID = ""
					line.Segments = mergeLyricsDocumentRoundTripSegments(line.Segments)
				}
			}
			for lineIndex, lineID := range rendition.Relation.LineIDs {
				rendition.Relation.LineIDs[lineIndex] = positions[lineID]
			}
		}
		if encoded, err = json.Marshal(normalized); err != nil {
			t.Fatal(err)
		}
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

func mergeLyricsDocumentRoundTripSegments(segments []store.PublicLyricsV3Segment) []store.PublicLyricsV3Segment {
	var merged []store.PublicLyricsV3Segment
	for _, segment := range segments {
		last := len(merged) - 1
		if last >= 0 && len(segment.PerformerIDs) <= 1 && fmt.Sprint(merged[last].PerformerIDs) == fmt.Sprint(segment.PerformerIDs) {
			merged[last].Text += segment.Text
			merged[last].Ruby = append(merged[last].Ruby, segment.Ruby...)
			continue
		}
		segment.Ruby = append([]store.PublicLyricsV3RubySpan(nil), segment.Ruby...)
		merged = append(merged, segment)
	}
	for index := range merged {
		var spans []store.PublicLyricsV3RubySpan
		for _, span := range merged[index].Ruby {
			last := len(spans) - 1
			if last >= 0 && span.Reading == "" && spans[last].Reading == "" {
				spans[last].Text += span.Text
				continue
			}
			spans = append(spans, span)
		}
		merged[index].Ruby = spans
	}
	return merged
}

// lyricsDocumentRoundTripDiffs lists the JSON paths where two trees differ.
func lyricsDocumentRoundTripDiffs(path string, left, right any, diffs []string) []string {
	switch l := left.(type) {
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok {
			return append(diffs, path)
		}
		keys := map[string]bool{}
		for key := range l {
			keys[key] = true
		}
		for key := range r {
			keys[key] = true
		}
		sorted := make([]string, 0, len(keys))
		for key := range keys {
			sorted = append(sorted, key)
		}
		sort.Strings(sorted)
		for _, key := range sorted {
			diffs = lyricsDocumentRoundTripDiffs(path+"."+key, l[key], r[key], diffs)
		}
		return diffs
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return append(diffs, path)
		}
		for index := range l {
			diffs = lyricsDocumentRoundTripDiffs(fmt.Sprintf("%s[%d]", path, index), l[index], r[index], diffs)
		}
		return diffs
	default:
		if !reflect.DeepEqual(left, right) {
			return append(diffs, path)
		}
		return diffs
	}
}

// lyricsDocumentRoundTripPermittedWarnings are the export warnings a bundle
// song may still carry; every other code fails the round trip.
var lyricsDocumentRoundTripPermittedWarnings = map[string]string{
	store.LyricsDocumentExportWarningSourceUnsupported: "PUT accepts Fandom, Sekaipedia and moegirl.icu revision URLs; a moegirl_public_exact attribution is refused",
	store.LyricsDocumentExportWarningSourceReencoded:   "PUT stores the revision URL in its canonical url.PathEscape encoding",
	store.LyricsDocumentExportWarningSourceDiffers:     "PUT attributes the whole song to one source revision",
	store.LyricsDocumentExportWarningGameDiffers:       "same and cut repeat the Full line, so a Game that differs is republished as independent",
	store.LyricsDocumentExportWarningGameProjection:    "an unordered projection is republished as independent",
	store.LyricsDocumentExportWarningSideLabel:         "the Game label is allowlisted: pjsk.moe does not render side labels",
}

// lyricsDocumentRoundTripExplanations names, for each path where a compiled
// export may differ from the served detail after the allowlist, the export
// warnings that account for the difference.
var lyricsDocumentRoundTripExplanations = []struct {
	path  *regexp.Regexp
	codes []string
}{
	{regexp.MustCompile(`^\.renditions\[\]\.relation\..*$`), []string{
		store.LyricsDocumentExportWarningGameDiffers, store.LyricsDocumentExportWarningGameProjection}},
	{regexp.MustCompile(`^\.renditions\[\]\.provenance`), []string{
		store.LyricsDocumentExportWarningSourceDiffers, store.LyricsDocumentExportWarningSourceReencoded,
		store.LyricsDocumentExportWarningSourceUnsupported, store.LyricsDocumentExportWarningSourceMissing}},
}

// lyricsDocumentRoundTripUnexplained returns the differing paths that no
// warning on the same rendition (or on the whole document) accounts for.
func lyricsDocumentRoundTripUnexplained(diffs []string, original store.PublicLyricsV3DetailDocument, warnings []store.LyricsDocumentExportWarning) []string {
	renditionIndex := regexp.MustCompile(`^\.renditions\[(\d+)\]`)
	var unexplained []string
	for _, diff := range diffs {
		generic := regexp.MustCompile(`\[\d+\]`).ReplaceAllString(diff, "[]")
		renditionKey := ""
		if match := renditionIndex.FindStringSubmatch(diff); match != nil {
			var index int
			fmt.Sscan(match[1], &index)
			if index < len(original.Renditions) {
				renditionKey = original.Renditions[index].Key
			}
		}
		explained := false
		for _, explanation := range lyricsDocumentRoundTripExplanations {
			if !explanation.path.MatchString(generic) {
				continue
			}
			for _, warning := range warnings {
				if (warning.Rendition == "" || renditionKey == "" || warning.Rendition == renditionKey) &&
					sliceContains(explanation.codes, warning.Code) {
					explained = true
				}
			}
		}
		if !explained {
			unexplained = append(unexplained, diff)
		}
	}
	return unexplained
}

func sliceContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// lyricsDocumentRoundTripStats accumulates the bundle round trip: outcomes
// per song, songs per differing path and per warning code, and the lines the
// per-line comparison checked and found different.
type lyricsDocumentRoundTripStats struct {
	outcomes, paths, warnings map[string]int
	lines, lineDiffs          int
	// multiPerformerBoundaries counts, per song, the served lines where two
	// adjacent segments have the same several performers.
	multiPerformerBoundaries map[int]int
}

// lyricsDocumentRoundTrip exports a served detail, publishes the export with
// PUT, which reloads the stored song and requires the validated detail, and
// compares the result with the served detail. It records "identical", "allowlisted", "explained" or "put_rejected", and
// fails the test for a warning outside the permitted set, a difference no
// warning names, any per-line difference, or a change summary that is not
// empty.
func lyricsDocumentRoundTrip(t *testing.T, s *store.Store, musicID int, served []byte, bundleRevision int, stats *lyricsDocumentRoundTripStats) string {
	t.Helper()
	export, err := s.ExportLyricsDocument(context.Background(), musicID, served, bundleRevision)
	if err != nil {
		t.Fatalf("music %d export: %v", musicID, err)
	}
	warned := map[string]bool{}
	for _, warning := range export.Warnings {
		if _, permitted := lyricsDocumentRoundTripPermittedWarnings[warning.Code]; !permitted {
			t.Errorf("music %d: export warns %s (%s/%s): %s", musicID, warning.Code, warning.Rendition, warning.Side, warning.Message)
		}
		if !warned[warning.Code] {
			warned[warning.Code] = true
			stats.warnings[warning.Code]++
		}
	}
	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Document))
	decoder.DisallowUnknownFields()
	var request store.LyricsDocumentRequest
	if err := decoder.Decode(&request); err != nil || !reflect.DeepEqual(request, export.Document) {
		t.Fatalf("music %d export JSON does not decode to its request: %v", musicID, err)
	}
	result, err := s.PublishLyricsDocumentServed(context.Background(), request,
		store.LyricsDocumentServed{BundleRevision: bundleRevision, Detail: served}, "round-trip")
	if err == nil && (result.Changes.Changed || result.Changes.Against != store.LyricsDocumentChangesAgainstServed) {
		t.Errorf("music %d: the unchanged export reports changes %+v", musicID, result.Changes)
	}
	if err != nil {
		var documentErr *store.LyricsDocumentError
		if !errors.As(err, &documentErr) || !lyricsDocumentRoundTripRejectionWarned(documentErr, export.Warnings) {
			t.Errorf("music %d: PUT rejects the export without a matching warning: %v %+v", musicID, err, documentErr)
		} else {
			t.Logf("music %d: PUT rejects the export as warned: %s %v %+v", musicID, documentErr.Code, documentErr.Details, documentErr.Issues)
		}
		stats.paths["(put rejected)"]++
		return "put_rejected"
	}
	// Exporting the published song again gives the same request, so a later
	// GET -> PUT keeps the song as it stands.
	again, err := s.ExportLyricsDocument(context.Background(), musicID, result.Document, bundleRevision)
	if err != nil {
		t.Fatalf("music %d second export: %v", musicID, err)
	}
	fixedPoint := export.Document
	fixedPoint.ExpectedRevision, fixedPoint.Source.URL = &result.Revision, again.Document.Source.URL
	if len(again.Warnings) != 0 || !reflect.DeepEqual(again.Document, fixedPoint) {
		t.Errorf("music %d: the published song exports differently (warnings %v)", musicID, again.Warnings)
	}
	produced, err := store.DecodePublicLyricsV3Detail(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.DecodePublicLyricsV3Detail(served)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendition := range original.Renditions {
		for _, side := range []*store.PublicLyricsV3Side{rendition.Full, rendition.Game} {
			for _, line := range lyricsDocumentRoundTripSideLines(side) {
				for index := 1; index < len(line.Segments); index++ {
					if ids := line.Segments[index].PerformerIDs; len(ids) > 1 && fmt.Sprint(ids) == fmt.Sprint(line.Segments[index-1].PerformerIDs) {
						stats.multiPerformerBoundaries[musicID]++
						break
					}
				}
			}
		}
	}
	lines, lineDiffs := lyricsDocumentRoundTripLineDiffs(original, produced)
	stats.lines += lines
	stats.lineDiffs += len(lineDiffs)
	if len(lineDiffs) > 0 {
		t.Errorf("music %d lines differ: %v", musicID, lineDiffs[:min(len(lineDiffs), 8)])
	}
	// PUT always assigns the next revision and the publish time.
	original.Revision, original.UpdatedAt = produced.Revision, produced.UpdatedAt
	if len(lyricsDocumentRoundTripDiffs("", lyricsDocumentRoundTripTree(t, original, false), lyricsDocumentRoundTripTree(t, produced, false), nil)) == 0 {
		return "identical"
	}
	diffs := lyricsDocumentRoundTripDiffs("", lyricsDocumentRoundTripTree(t, original, true), lyricsDocumentRoundTripTree(t, produced, true), nil)
	if len(diffs) == 0 {
		return "allowlisted"
	}
	seen := map[string]bool{}
	for _, diff := range diffs {
		generic := regexp.MustCompile(`\[\d+\]`).ReplaceAllString(diff, "[]")
		if !seen[generic] {
			seen[generic] = true
			stats.paths[generic]++
		}
	}
	if unexplained := lyricsDocumentRoundTripUnexplained(diffs, original, export.Warnings); len(unexplained) > 0 {
		t.Errorf("music %d differs without a warning at %v", musicID, unexplained[:min(len(unexplained), 8)])
	}
	return "explained"
}

func lyricsDocumentRoundTripSideLines(side *store.PublicLyricsV3Side) []store.PublicLyricsV3Line {
	if side == nil {
		return nil
	}
	return side.Lines
}

// lyricsDocumentRoundTripLineDiffs compares every served line with the line
// at the same position of the same rendition side after the round trip: ja,
// zh, en, stanza break, segment text, per-segment performers in order, ruby
// and trailing performers. Adjacent segments with the same single performer
// or none and adjacent unannotated ruby spans are joined on both sides first.
func lyricsDocumentRoundTripLineDiffs(original, produced store.PublicLyricsV3DetailDocument) (int, []string) {
	byKey := map[string]store.PublicLyricsV3Rendition{}
	for _, rendition := range produced.Renditions {
		byKey[rendition.Key] = rendition
	}
	lines := 0
	var diffs []string
	for _, served := range original.Renditions {
		compiled, found := byKey[served.Key]
		if !found {
			diffs = append(diffs, served.Key+": rendition missing")
			continue
		}
		for _, side := range []struct {
			name             string
			served, compiled *store.PublicLyricsV3Side
		}{{"full", served.Full, compiled.Full}, {"game", served.Game, compiled.Game}} {
			if side.served == nil || side.compiled == nil {
				if (side.served == nil) != (side.compiled == nil) {
					diffs = append(diffs, served.Key+"/"+side.name+": side presence")
				}
				continue
			}
			if len(side.served.Lines) != len(side.compiled.Lines) {
				diffs = append(diffs, fmt.Sprintf("%s/%s: %d lines, compiled %d", served.Key, side.name, len(side.served.Lines), len(side.compiled.Lines)))
				continue
			}
			for index, want := range side.served.Lines {
				got := side.compiled.Lines[index]
				lines++
				comparable := func(line store.PublicLyricsV3Line) string {
					encoded, _ := json.Marshal([]any{line.Japanese, line.Chinese, line.English, line.StanzaBreakBefore,
						mergeLyricsDocumentRoundTripSegments(line.Segments), line.TrailingPerformerIDs})
					return string(encoded)
				}
				if comparable(want) != comparable(got) {
					diffs = append(diffs, fmt.Sprintf("%s/%s/%d", served.Key, side.name, index))
				}
			}
		}
	}
	return lines, diffs
}

// lyricsDocumentRoundTripRejectionWarned reports whether every PUT issue is on
// a line or field an export warning names.
func lyricsDocumentRoundTripRejectionWarned(err *store.LyricsDocumentError, warnings []store.LyricsDocumentExportWarning) bool {
	if err == nil {
		return false
	}
	if err.Code == store.LyricsDocumentErrorSourceRevision {
		for _, warning := range warnings {
			if warning.Code == store.LyricsDocumentExportWarningSourceUnsupported || warning.Code == store.LyricsDocumentExportWarningSourceMissing {
				return true
			}
		}
		return false
	}
	if len(err.Issues) == 0 {
		return false
	}
	for _, issue := range err.Issues {
		matched := false
		for _, warning := range warnings {
			if warning.Rendition != issue.Rendition {
				continue
			}
			if issue.Field == "ja" && warning.Line != nil && issue.Line != nil && *warning.Line == *issue.Line &&
				sliceContains([]string{store.LyricsDocumentExportWarningRubyMissing, store.LyricsDocumentExportWarningRubyUnsupported}, warning.Code) {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// Every song detail of the embedded public bundle is exported, published by
// PUT and compared with the served detail.
func TestLyricsDocumentExportRoundTripsTheEmbeddedBundle(t *testing.T) {
	assets, err := publiclyricsbundle.Load()
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := publiclyricsbundle.CatalogRuntimeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	var musicIDs []int
	for key := range assets {
		var musicID int
		if n, _ := fmt.Sscanf(key, "translation/lyrics/music_%d.json", &musicID); n == 1 {
			musicIDs = append(musicIDs, musicID)
		}
	}
	sort.Ints(musicIDs)
	if len(musicIDs) == 0 {
		t.Fatal("embedded bundle has no song detail")
	}
	h := setupLegacyAPI(t)
	records := make([]store.MusicCatalogRecord, len(musicIDs))
	for index, musicID := range musicIDs {
		records[index] = store.MusicCatalogRecord{MusicID: musicID, JapaneseTitle: fmt.Sprintf("合成曲%d", musicID)}
	}
	if err := h.store.UpsertMusicCatalog(records); err != nil {
		t.Fatal(err)
	}
	stats := &lyricsDocumentRoundTripStats{outcomes: map[string]int{}, paths: map[string]int{}, warnings: map[string]int{},
		multiPerformerBoundaries: map[int]int{}}
	for _, musicID := range musicIDs {
		body := assets[fmt.Sprintf("translation/lyrics/music_%d.json", musicID)]
		stats.outcomes[lyricsDocumentRoundTrip(t, h.store, musicID, body, metadata[musicID].Revision, stats)]++
	}
	t.Logf("songs checked=%d identical=%d allowlisted-only=%d differing-with-matching-warnings=%d put-rejected-with-matching-warnings=%d",
		len(musicIDs), stats.outcomes["identical"], stats.outcomes["allowlisted"], stats.outcomes["explained"], stats.outcomes["put_rejected"])
	t.Logf("songs per warning code: %v", stats.warnings)
	t.Logf("songs per differing path: %v", stats.paths)
	t.Logf("lines compared=%d differing=%d", stats.lines, stats.lineDiffs)
	t.Logf("lines with adjacent segments of the same several performers per song: %v", stats.multiPerformerBoundaries)
	if stats.lines == 0 {
		t.Fatal("the per-line comparison checked no line")
	}
	// Those boundaries are compared, so the per-line check covers them.
	if len(stats.multiPerformerBoundaries) == 0 {
		t.Fatal("the bundle has no line with adjacent segments of the same several performers")
	}
}

// A served legacy publication is taken over through its export: the public
// lines stay as served and the song becomes an editor document.
func TestLyricsDocumentTakeoverRoutePublishesTheServedSongAsADocument(t *testing.T) {
	const musicID = 990804
	const takeoverRoute = lyricsDocumentRoute + "/takeover"
	h, service := setupLyricsDocumentAPI(t, musicID)
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{
		{PerformerID: 1, JapaneseName: "歌唱者"}, {PerformerID: 2, JapaneseName: "歌唱者二"},
	}); err != nil {
		t.Fatal(err)
	}
	takeoverBody := func(expected any) map[string]any {
		body := map[string]any{"musicId": musicID}
		if expected != nil {
			body["expectedRevision"] = expected
		}
		return body
	}
	// A song the site does not serve has nothing to take over.
	if status, body, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPost, takeoverRoute, takeoverBody(0))); status != http.StatusNotFound ||
		string(body["error"]) != `"not_found"` {
		t.Fatalf("unserved takeover status=%d body=%s", status, raw)
	}
	saved, _, err := h.store.SaveImportedLyricsMutation(model.SongLyrics{
		MusicID: musicID, Attribution: "合成译者",
		SourceURL: "https://vocaloid.fandom.com/wiki/Takeover_Song?oldid=4321", SourcePageID: 99, SourceRevisionID: 4321,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{
			// A v1 publication serves no ruby, so the lines are kana.
			{ID: "line-1", Order: 0, Japanese: "うたう", Chinese: "歌唱",
				Segments: []model.LyricSegment{{Text: "うた", PerformerIDs: []int{1}}, {Text: "う", PerformerIDs: []int{1, 2}}}},
			{ID: "line-2", Order: 1, Japanese: "らら", Chinese: "啦啦", StanzaBreakBefore: true,
				Segments: []model.LyricSegment{{Text: "らら", PerformerIDs: []int{1}}}},
		},
	}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.PublishLyrics(musicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	service.Rebuild()
	_, before := serveLyricsDocument(service, musicID)

	editor, err := h.api.auth.CreateUser("takeover-editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	editorToken, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPost, h.server.URL+takeoverRoute, editorToken, takeoverBody(saved.Revision))); status != http.StatusForbidden {
		t.Fatalf("editor takeover status=%d body=%s", status, raw)
	}
	if status, body, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPost, takeoverRoute, takeoverBody(saved.Revision+5))); status != http.StatusConflict ||
		string(body["error"]) != `"revision_conflict"` || string(body["current"]) != fmt.Sprintf(`{"revision":%d}`, saved.Revision) {
		t.Fatalf("stale takeover status=%d body=%s", status, raw)
	}
	if status, body, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPost, takeoverRoute, takeoverBody(nil))); status != http.StatusUnprocessableEntity ||
		string(body["error"]) != `"expected_revision_required"` || string(body["current"]) != fmt.Sprintf(`{"revision":%d}`, saved.Revision) {
		t.Fatalf("takeover without expectedRevision status=%d body=%s", status, raw)
	}
	if service.publishNow != 0 {
		t.Fatalf("a refused takeover published: publishNow=%d", service.publishNow)
	}

	status, taken, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPost, takeoverRoute, takeoverBody(saved.Revision)))
	if status != http.StatusOK || string(taken["dryRun"]) != "false" || string(taken["revision"]) != fmt.Sprint(saved.Revision+1) ||
		string(taken["changes"]) != `{"against":"served","changed":false}` || taken["warnings"] == nil || service.publishNow != 1 {
		t.Fatalf("takeover status=%d publishNow=%d body=%s", status, service.publishNow, raw)
	}
	code, after := serveLyricsDocument(service, musicID)
	if code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(after), bytes.TrimSpace(taken["document"])) {
		t.Fatalf("served after takeover status=%d body=%s", code, after)
	}
	var legacy store.PublicLyricsDetailDocument
	if err := json.Unmarshal(before, &legacy); err != nil {
		t.Fatal(err)
	}
	produced, err := store.DecodePublicLyricsV3Detail(after)
	if err != nil {
		t.Fatal(err)
	}
	type comparableLine struct {
		Japanese, Chinese string
		StanzaBreak       bool
		Segments          []string
		Readings          []string
	}
	var want, got []comparableLine
	for _, line := range legacy.Lines {
		value := comparableLine{Japanese: line.Japanese, Chinese: line.Chinese, StanzaBreak: line.StanzaBreakBefore}
		for _, segment := range line.Segments {
			value.Segments = append(value.Segments, segment.Text)
			for _, span := range segment.Ruby {
				if span.Reading != "" {
					value.Readings = append(value.Readings, span.Text+"|"+span.Reading)
				}
			}
		}
		want = append(want, value)
	}
	for _, line := range produced.Renditions[0].Full.Lines {
		value := comparableLine{Japanese: line.Japanese, Chinese: line.Chinese, StanzaBreak: line.StanzaBreakBefore}
		for _, segment := range line.Segments {
			value.Segments = append(value.Segments, segment.Text)
			for _, span := range segment.Ruby {
				if span.Reading != "" {
					value.Readings = append(value.Readings, span.Text+"|"+span.Reading)
				}
			}
		}
		got = append(got, value)
	}
	if len(produced.Renditions) != 1 || len(want) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("takeover lines=%+v, served %+v", got, want)
	}
	if _, err := h.store.GetLyricsRenditionDocument(musicID); err != nil {
		t.Fatalf("the taken-over song has no editor document: %v", err)
	}
}

func TestLyricsDocumentRubyRouteSuggestsReadingsForAnySignedInUser(t *testing.T) {
	h := setupLegacyAPI(t)
	editor, err := h.api.auth.CreateUser("ruby-editor", "strong-password-123", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	editorToken, _, err := h.api.auth.IssueToken(editor)
	if err != nil {
		t.Fatal(err)
	}
	route := h.server.URL + lyricsDocumentRoute + "/ruby"

	// An editor gets suggestions without a producer-state header while the
	// producer holds the content gate.
	releaseProducer, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	status, body, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPost, route, editorToken, map[string]any{
		"lines": []string{"試験の歌", "{試|し}験", "ルルル"},
	}))
	releaseProducer()
	wantLines := `[{"ja":"{試験|しけん}の{歌|うた}","suggested":[{"character":0,"text":"試験","reading":"しけん"},{"character":9,"text":"歌","reading":"うた"}],"problems":[]},` +
		`{"ja":"{試|し}験","suggested":[],"problems":["kanji without a {kanji|reading}: 「験」 at character 5"]},` +
		`{"ja":"ルルル","suggested":[],"problems":[]}]`
	if status != http.StatusOK || string(body["lines"]) != wantLines || len(body["generator"]) <= 2 {
		t.Fatalf("suggestion status=%d body=%s", status, raw)
	}

	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPost, route, "", map[string]any{"lines": []string{"歌"}})); status != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", status, raw)
	}
	if response := doJSON(t, http.MethodGet, route, editorToken, nil); response.StatusCode == http.StatusOK {
		response.Body.Close()
		t.Fatal("GET answered the POST-only ruby route")
	} else {
		response.Body.Close()
	}
	tooMany := make([]string, maxLyricsDocumentRubyLines+1)
	for index := range tooMany {
		tooMany[index] = "歌"
	}
	for _, test := range []struct {
		body    any
		details string
	}{
		{map[string]any{"lines": []string{}}, `["lines must list 1 to 2000 ja markups; it lists 0"]`},
		{map[string]any{"lines": tooMany}, `["lines must list 1 to 2000 ja markups; it lists 2001"]`},
		{map[string]any{"lines": []string{"歌", "一行\n二行"}}, `["lines[1] must be one line of at most 8192 bytes"]`},
	} {
		status, body, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPost, route, editorToken, test.body))
		if status != http.StatusBadRequest || string(body["error"]) != `"invalid_request"` || string(body["details"]) != test.details {
			t.Fatalf("invalid request status=%d body=%s", status, raw)
		}
	}
	if status, _, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodPost, route, editorToken, map[string]any{"lines": []string{"歌"}, "musicId": 1})); status != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", status, raw)
	}
}

func TestLyricsDocumentExportWaitsForAPendingPublication(t *testing.T) {
	const musicID = 990811
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: musicID, JapaneseTitle: "合成試験曲",
		Vocals: []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 1, CharacterSequence: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	service := filesvc.New(h.store, h.events, files.NewGenerator(h.store, h.events, ""))
	service.SetDebounce(time.Hour)
	h.api.SetFileService(service)
	service.Start()
	defer func() {
		service.Stop()
		service.Wait()
	}()
	status, published, raw := readLyricsDocumentResponse(t, authorizedRequest(t, h, http.MethodPut, lyricsDocumentRoute, lyricsDocumentAPIBody(musicID)))
	if status != http.StatusOK {
		t.Fatalf("publish status=%d body=%s", status, raw)
	}
	if !service.AwaitPublished(context.Background(), 10*time.Second) {
		t.Fatal("the first publish was not published")
	}

	// A second publish whose rebuild still sits in the hour-long debounce
	// window: the site serves the first revision until something hurries it.
	body := lyricsDocumentAPIBody(musicID)
	body["expectedRevision"] = json.RawMessage(published["revision"])
	body["renditions"].([]map[string]any)[0]["lines"].([]map[string]any)[1]["zh"] = "啦啦啦地合成了"
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var request store.LyricsDocumentRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	detail, _ := service.PublicLyricsDetail(musicID)
	second, err := h.store.PublishLyricsDocumentServed(context.Background(), request, store.LyricsDocumentServed{Detail: detail}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	service.Trigger()
	if !service.Status().Pending {
		t.Fatal("the second publish is not pending")
	}

	status, exported, raw := readLyricsDocumentResponse(t, doJSON(t, http.MethodGet, fmt.Sprintf("%s%s?musicId=%d", h.server.URL, lyricsDocumentRoute, musicID), h.token, nil))
	if status != http.StatusOK || string(exported["servedRevision"]) != fmt.Sprint(second.Revision) || string(exported["warnings"]) != "[]" ||
		!bytes.Contains(exported["document"], []byte(`"zh":"啦啦啦地合成了"`)) {
		t.Fatalf("export after the pending publish status=%d body=%s", status, raw)
	}
}
