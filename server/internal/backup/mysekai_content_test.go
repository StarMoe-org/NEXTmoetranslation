package backup

import (
	"context"
	"encoding/json"
	"moesekai/server/internal/store"
	"strings"
	"testing"
)

func TestMysekaiTranslationContentRoundTrip(t *testing.T) {
	h := setupLegacyBackup(t)
	h.store.SetMysekaiFontProvider(testMysekaiFonts{})
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	raw := []byte(`{"schema":"moly-text-source/1","region":"jp","revision":"` + hash + `","scripts":[{"key":"synthetic","lines":[{"id":"line","kind":"text","step":0,"text":"Original","sourceHash":"` + hash + `"}]}]}`)
	if _, e := h.store.ImportMysekaiSource(ctx, raw, "", "admin"); e != nil {
		t.Fatal(e)
	}
	if _, e := h.store.SaveMysekai(ctx, store.MysekaiSave{Region: "jp", Locale: "zh-CN", ScriptKey: "synthetic", SourceRevision: hash, Edits: []store.MysekaiEdit{{ID: "line", SourceHash: hash, Text: "Translation"}}}, "editor"); e != nil {
		t.Fatal(e)
	}
	page, e := h.store.MysekaiList(ctx, "jp", "zh-CN", "", "", "", 40)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.store.SetMysekaiPublication(ctx, store.MysekaiPublicationRequest{Region: "jp", Locale: "zh-CN", SourceRevision: hash, DraftRevision: page.DraftRevision}, "publish", "admin"); e != nil {
		t.Fatal(e)
	}
	before, e := h.store.MysekaiPublicFiles(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dir, e := materializeTranslationContentFromStore(t.TempDir(), h.store)
	if e != nil {
		t.Fatal(e)
	}
	content, present, e := readTranslationContent(dir)
	if e != nil || !present {
		t.Fatalf("read backup: %v", e)
	}
	if len(content.Mysekai.Records) == 0 {
		t.Fatal("MYSEKAI omitted")
	}
	if e = h.manager.importTranslationContent(content, true); e != nil {
		t.Fatal(e)
	}
	after, e := h.store.MysekaiPublicFiles(ctx)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("public immutable history changed on backup restore")
	}
	detail, e := h.store.MysekaiDetail(ctx, "jp", "zh-CN", "synthetic")
	if e != nil || detail.Drafts["line"].Text != "Translation" {
		t.Fatalf("draft missing after restore: %v", e)
	}
}
