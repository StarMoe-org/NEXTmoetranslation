package translator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractMysekaiPropagatesJPGenreAndTagFetchErrors(t *testing.T) {
	files := map[string]string{
		"/jp-master/mysekaiFixtures.json":          `[{"id":1,"name":"テスト家具","flavorText":"テスト説明"}]`,
		"/cn-master/mysekaiFixtures.json":          `[{"id":1,"name":"测试家具","flavorText":"测试说明"}]`,
		"/jp-master/mysekaiFixtureMainGenres.json": `[{"id":2,"name":"テスト分類"}]`,
		"/cn-master/mysekaiFixtureMainGenres.json": `[{"id":2,"name":"测试分类"}]`,
		"/jp-master/mysekaiFixtureTags.json":       `[{"id":3,"name":"テストタグ"}]`,
		"/cn-master/mysekaiFixtureTags.json":       `[{"id":3,"name":"测试标签"}]`,
	}
	for _, tc := range []struct {
		name, missing string
	}{
		{"complete", ""},
		{"jp genre", "/jp-master/mysekaiFixtureMainGenres.json"},
		{"jp tag", "/jp-master/mysekaiFixtureTags.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, ok := files[r.URL.Path]
				if !ok || r.URL.Path == tc.missing {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, body)
			}))
			defer upstream.Close()
			tr, _, cfg := openTestTranslator(t)
			configureLocalSources(t, cfg, upstream.URL)

			fields, err := tr.extractMysekai()
			if tc.missing == "" {
				if err != nil {
					t.Fatal(err)
				}
				if fields["genre"].Pairs["テスト分類"] != "测试分类" || fields["tag"].Pairs["テストタグ"] != "测试标签" {
					t.Fatalf("mysekai genre/tag fields = %+v", fields)
				}
				return
			}
			file := tc.missing[strings.LastIndex(tc.missing, "/")+1:]
			if err == nil || !strings.Contains(err.Error(), file) || fields != nil {
				t.Fatalf("missing %s: fields=%+v err=%v", file, fields, err)
			}
		})
	}
}
