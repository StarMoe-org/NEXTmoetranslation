package lyricssource

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func readSekaipediaFixture(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func rokiSekaipediaIdentity() MusicIdentity {
	return MusicIdentity{
		MusicID: 2, JapaneseTitle: "ロキ", ProducerMetadata: "MikitoP",
		Lyricist: "MikitoP", Composer: "MikitoP", Arranger: "MikitoP",
		PerformerSegmentationPolicy: PerformerSegmentationSekaiEligible,
	}
}

func journeySekaipediaIdentity(policy PerformerSegmentationPolicy) MusicIdentity {
	return MusicIdentity{
		MusicID: 235, JapaneseTitle: "Journey", ProducerMetadata: "DECO*27",
		Lyricist: "DECO*27", Composer: "DECO*27", Arranger: "Rockwell",
		PerformerSegmentationPolicy: policy,
	}
}

func historicalSekaipediaProviderConfig() ProviderConfig {
	return ProviderConfig{
		Provider: ProviderSekaipedia, Enabled: true,
		Origin: OriginSekaipedia, APIEndpoint: sekaipediaAPI,
		Indexes: []FixedIndex{{
			PageID: 268, RevisionID: 335193, RevisionTimestamp: "2026-07-27T16:29:13Z",
			SHA1:          "b216a827f88c59f5e954a120027832fe9cd74413",
			ContentSHA256: "aaddff2922548aab7e522124ff2bad86427501930d549c9d94c9b4e473c35f92",
			RawSHA256:     "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
			Title:         "List of songs",
		}},
		SekaipediaTargets: []SekaipediaPageTarget{
			{MusicID: 2, PageTitle: "Roki"},
			{MusicID: 235, PageTitle: "Journey"},
		},
		RightsText: sekaipediaRightsText,
		CrawlDelay: defaultProviderCrawlDelay, CacheTTL: defaultProviderCacheTTL,
	}
}

func historicalSekaipediaAuthority() FixedIndex {
	return historicalSekaipediaProviderConfig().Indexes[0]
}

func historicalSekaipediaAuthorityEvidenceID() string {
	return sekaipediaAuthorityEvidenceID(historicalSekaipediaAuthority())
}

type sekaipediaFixtureServer struct {
	*httptest.Server
	list             []byte
	pages            map[string][]byte
	revisions        map[int][]byte
	requests         atomic.Int32
	listRequests     atomic.Int32
	revisionRequests atomic.Int32
	titleRequests    atomic.Int32
	driftTarget      atomic.Bool
}

func newSekaipediaFixtureServer(t *testing.T) *sekaipediaFixtureServer {
	t.Helper()
	fixture := &sekaipediaFixtureServer{
		list:      readSekaipediaFixture(t, "testdata/sekaipedia-list-335193.json"),
		pages:     map[string][]byte{},
		revisions: map[int][]byte{},
	}
	for _, title := range []string{"Roki", "Journey"} {
		body := sekaipediaFixturePageResponse(t, title)
		fixture.pages[title] = body
		page, err := parsePageResponse(body)
		if err != nil {
			t.Fatal(err)
		}
		fixture.revisions[page.revisionID] = body
	}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/w/api.php" {
			http.Error(w, "wrong Sekaipedia endpoint", http.StatusBadRequest)
			return
		}
		query := r.URL.Query()
		if query.Get("action") != "query" || query.Get("format") != "json" || query.Get("formatversion") != "2" ||
			query.Get("maxlag") != mediaWikiMaxLag || query.Get("prop") != "revisions|categories" ||
			query.Get("rvprop") != "ids|timestamp|sha1|content" || query.Get("rvslots") != "main" || query.Get("cllimit") != "max" {
			http.Error(w, "unbounded or noncanonical query", http.StatusBadRequest)
			return
		}
		var body []byte
		switch {
		case query.Get("revids") == "335193":
			fixture.listRequests.Add(1)
			if query.Has("titles") || query.Has("pageids") || query.Has("rvlimit") || query.Has("redirects") || query.Has("generator") {
				http.Error(w, "List request was not exact", http.StatusBadRequest)
				return
			}
			body = fixture.list
		case query.Get("revids") != "":
			fixture.revisionRequests.Add(1)
			if query.Has("titles") || query.Has("pageids") || query.Has("rvlimit") || query.Has("redirects") || query.Has("generator") {
				http.Error(w, "revision request was not exact", http.StatusBadRequest)
				return
			}
			revisionID, _ := strconv.Atoi(query.Get("revids"))
			body = fixture.revisions[revisionID]
			if fixture.driftTarget.Load() && revisionID == 330574 {
				body = bytes.Replace(body, []byte(`"sha1":"29198603574701b81b34198e63343930abd3d9a2"`),
					[]byte(`"sha1":"0000000000000000000000000000000000000000"`), 1)
			}
		case query.Get("titles") != "":
			fixture.titleRequests.Add(1)
			if query.Get("redirects") != "1" || query.Get("rvlimit") != "1" || query.Has("revids") || query.Has("pageids") {
				http.Error(w, "title request was not bounded", http.StatusBadRequest)
				return
			}
			title := query.Get("titles")
			if title == "ロキ" {
				title = "Roki"
			}
			body = fixture.pages[title]
		}
		if len(body) == 0 {
			http.Error(w, "missing fixture", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	return fixture
}

func (fixture *sekaipediaFixtureServer) Provider(t *testing.T) *sekaipediaProvider {
	t.Helper()
	config := historicalSekaipediaProviderConfig()
	config.APIEndpoint = fixture.URL + "/w/api.php"
	config.CrawlDelay = 0
	config.CacheTTL = time.Hour
	return newSekaipediaProvider(config, newMediaWikiClient(config.APIEndpoint, 0, time.Hour, fixture.Client()))
}

func (fixture *sekaipediaFixtureServer) Requests() int {
	return int(fixture.requests.Load())
}

func (fixture *sekaipediaFixtureServer) ListRequests() int {
	return int(fixture.listRequests.Load())
}

func (fixture *sekaipediaFixtureServer) RevisionRequests() int {
	return int(fixture.revisionRequests.Load())
}

func (fixture *sekaipediaFixtureServer) TitleRequests() int {
	return int(fixture.titleRequests.Load())
}

func sekaipediaFixturePageResponse(t *testing.T, title string) []byte {
	t.Helper()
	body := readSekaipediaFixture(t, "testdata/sekaipedia-sample-revisions.json")
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	var query struct {
		Pages []json.RawMessage `json:"pages"`
	}
	if err := json.Unmarshal(root["query"], &query); err != nil {
		t.Fatal(err)
	}
	var selected json.RawMessage
	for _, page := range query.Pages {
		var identity struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(page, &identity); err != nil {
			t.Fatal(err)
		}
		if identity.Title == title {
			selected = append(json.RawMessage(nil), page...)
		}
	}
	if selected == nil {
		t.Fatalf("missing fixture page %q", title)
	}
	queryBody, err := json.Marshal(map[string]any{"pages": []json.RawMessage{selected}})
	if err != nil {
		t.Fatal(err)
	}
	root["query"] = queryBody
	filtered, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return filtered
}

func sekaipediaFixturePageContent(t *testing.T, title string) string {
	t.Helper()
	page, err := parsePageResponse(sekaipediaFixturePageResponse(t, title))
	if err != nil {
		t.Fatal(err)
	}
	return page.content
}

func semanticSekaipediaRevisionResponse(
	t *testing.T,
	authority FixedIndex,
	content string,
	pageInfo map[string]any,
) []byte {
	t.Helper()
	page := map[string]any{
		"pageid": authority.PageID, "ns": 0, "title": authority.Title, "categories": []any{},
		"revisions": []any{map[string]any{
			"revid": authority.RevisionID, "timestamp": authority.RevisionTimestamp, "sha1": authority.SHA1,
			"slots": map[string]any{"main": map[string]any{
				"contentmodel": "wikitext", "contentformat": "text/x-wiki", "content": content,
			}},
		}},
	}
	for key, value := range pageInfo {
		page[key] = value
	}
	body, err := json.Marshal(map[string]any{
		"batchcomplete": true, "query": map[string]any{"pages": []any{page}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSekaipediaFixtureDigestsAreFixed(t *testing.T) {
	for path, want := range map[string]string{
		"testdata/sekaipedia-list-335193.json":      "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
		"testdata/sekaipedia-sample-revisions.json": "9d673bf793f66f371f13a47b7e58733b71212f6dc6ece11742fdbc8f4e466347",
	} {
		body := readSekaipediaFixture(t, path)
		if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != want {
			t.Fatalf("fixture %s digest = %s want %s", path, got, want)
		}
	}
}
