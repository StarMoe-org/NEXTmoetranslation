package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

func TestUpdateEntryAcceptsLongMultilineGachaInfoDescription(t *testing.T) {
	h := setupLegacyAPI(t)
	var key strings.Builder
	key.WriteString("期間限定ガチャ「テスト用ガチャ」開催！\n\n【注意事項】\n")
	for note := 1; note <= 60; note++ {
		fmt.Fprintf(&key, "※合成された注意事項その%02dです。<b>表示</b> & 確率は変更される場合があります。\n", note)
	}
	jpKey := key.String()
	text := strings.ReplaceAll(jpKey, "注意事項", "注意事项")
	if len(jpKey) < 4<<10 {
		t.Fatalf("fixture key is %d bytes, want a several-KB description", len(jpKey))
	}
	if _, err := h.store.ImportCategory("gachaInfo", model.Category{"description": {
		jpKey: {Source: model.SourceUnknown, Ids: []string{"101"}},
	}}); err != nil {
		t.Fatal(err)
	}

	response := strictAs(t, h, http.MethodPut, "/api/editor/v1/entry?response=correlated-v1", h.token, map[string]string{
		"category": "gachaInfo", "field": "description", "key": jpKey, "text": text, "source": model.SourceHuman,
	})
	defer response.Body.Close()
	var saved map[string]string
	if err := json.NewDecoder(response.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || saved["status"] != "ok" || saved["key"] != jpKey || saved["text"] != text {
		t.Fatalf("update status=%d response status=%q key-match=%v text-match=%v error=%q",
			response.StatusCode, saved["status"], saved["key"] == jpKey, saved["text"] == text, saved["error"])
	}

	listed := authGET(t, h.server, h.token, "/api/entries?"+url.Values{
		"category": {"gachaInfo"}, "field": {"description"},
	}.Encode())
	defer listed.Body.Close()
	var entries []model.EntryWithKey
	if err := json.NewDecoder(listed.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != jpKey || entries[0].Text != text || entries[0].Source != model.SourceHuman {
		t.Fatalf("persisted gachaInfo entries = %d, want the exact key and text", len(entries))
	}
}
