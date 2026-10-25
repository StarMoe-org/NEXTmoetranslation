package store

import (
	"context"
	"encoding/json"
	"moesekai/server/internal/db"
	"moesekai/server/internal/textfonts"
	"path/filepath"
	"strings"
	"testing"
)

type testMysekaiFonts struct{}

func (testMysekaiFonts) Load(context.Context) (textfonts.Catalog, error) {
	return textfonts.Catalog{Fonts: []textfonts.Font{{Coverage: textfonts.Coverage{{32, 126}, {0x4e00, 0x9fff}}}}}, nil
}
func TestMysekaiFontGatePreservesDraftAndPublicPointer(t *testing.T) {
	database, e := db.Open(filepath.Join(t.TempDir(), "font.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer database.Close()
	s := New(database)
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	raw := []byte(`{"schema":"moly-text-source/1","region":"jp","revision":"` + hash + `","scripts":[{"key":"test","lines":[{"id":"a","kind":"text","step":0,"text":"Source","sourceHash":"` + hash + `"}]}]}`)
	if _, e = s.ImportMysekaiSource(ctx, raw, "", "admin"); e != nil {
		t.Fatal(e)
	}
	save := MysekaiSave{Region: "jp", Locale: "zh-CN", ScriptKey: "test", SourceRevision: hash, Edits: []MysekaiEdit{{ID: "a", SourceHash: hash, Text: "译文"}}}
	if _, e = s.SaveMysekai(ctx, save, "editor"); e != nil {
		t.Fatal(e)
	}
	page, _ := s.MysekaiList(ctx, "jp", "zh-CN", "", "", "", 40)
	request := MysekaiPublicationRequest{Region: "jp", Locale: "zh-CN", SourceRevision: hash, DraftRevision: page.DraftRevision}
	if _, e = s.SetMysekaiPublication(ctx, request, "publish", "admin"); e == nil {
		t.Fatal("missing font config published")
	}
	s.SetMysekaiFontProvider(testMysekaiFonts{})
	channel, e := s.SetMysekaiPublication(ctx, request, "publish", "admin")
	if e != nil {
		t.Fatal(e)
	}
	before, e := s.MysekaiPublicFiles(ctx)
	if e != nil {
		t.Fatal(e)
	}
	save.Edits[0].ExpectedRevision = 1
	save.Edits[0].Text = "☃"
	if _, e = s.SaveMysekai(ctx, save, "editor"); e != nil {
		t.Fatal("unsupported glyph draft must save", e)
	}
	after, e := s.MysekaiPublicFiles(ctx)
	if e != nil || string(before["mysekai/jp/zh-CN/latest.json"]) != string(after["mysekai/jp/zh-CN/latest.json"]) {
		t.Fatal("draft advanced public latest")
	}
	page, _ = s.MysekaiList(ctx, "jp", "zh-CN", "", "", "", 40)
	request.DraftRevision = page.DraftRevision
	request.ExpectedRevision = channel.Revision
	if _, e = s.SetMysekaiPublication(ctx, request, "publish", "admin"); e == nil || !strings.Contains(e.Error(), "U+2603") {
		t.Fatal("unsupported glyph published", e)
	}
	after, _ = s.MysekaiPublicFiles(ctx)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("refusal changed public files")
	}
}
