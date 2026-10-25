package textfonts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixture() Catalog {
	return Catalog{Schema: "moly-text-fonts/1", Revision: strings.Repeat("a", 64), BaseFonts: map[string]BaseFont{strings.Repeat("b", 64): {Coverage: Coverage{{32, 126}}}}, Fonts: []Font{{Reference: Reference{"objects/" + strings.Repeat("c", 64) + ".ttf", strings.Repeat("c", 64), 100}, Coverage: Coverage{{32, 126}, {0x4e00, 0x9fff}}, License: Reference{"objects/" + strings.Repeat("d", 64) + ".txt", strings.Repeat("d", 64), 12}}}}
}
func TestCatalogueCoverageAndFixedHTTPS(t *testing.T) {
	c := fixture()
	raw, _ := json.Marshal(c)
	parsed, e := Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	if len(parsed.Missing("<b>译文</b>")) != 0 || len(parsed.Missing("☃")) != 1 {
		t.Fatal("coverage mismatch")
	}
	for _, url := range []string{"http://example.test/fonts/latest.json", "https://user@example.test/fonts/latest.json", "https://example.test/fonts/latest.json?q=x"} {
		if _, e := New(url); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
	c.Fonts[0].Coverage = Coverage{{1, 10}, {9, 11}}
	raw, _ = json.Marshal(c)
	if _, e = Parse(raw); e == nil {
		t.Fatal("overlap accepted")
	}
	valid, _ := json.Marshal(fixture())
	if _, e = Parse([]byte(strings.Replace(string(valid), "[32,126]", "[32,126,127]", 1))); e == nil {
		t.Fatal("oversized coverage pair accepted")
	}
}
func TestCatalogueHistoryAndCache(t *testing.T) {
	c := fixture()
	raw, _ := json.Marshal(c)
	calls := 0
	bad := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if bad && strings.Contains(r.URL.Path, "revisions/") {
			w.Write([]byte("{}"))
		} else {
			w.Write(raw)
		}
	}))
	defer server.Close()
	loader, e := New(server.URL + "/fonts/latest.json")
	if e != nil {
		t.Fatal(e)
	}
	loader.client = server.Client()
	if _, e = loader.Load(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = loader.Load(context.Background()); e != nil || calls != 2 {
		t.Fatal("cache not reused")
	}
	loader.cached = nil
	bad = true
	if _, e = loader.Load(context.Background()); e == nil {
		t.Fatal("history mismatch accepted")
	}
}
