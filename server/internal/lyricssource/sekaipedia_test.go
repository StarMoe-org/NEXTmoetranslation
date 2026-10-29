package lyricssource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/model"
)

func TestSekaipediaProviderRequiresExplicitImmutableAuthorityConfiguration(t *testing.T) {
	fallbacks := DefaultProviderConfigs()
	if len(fallbacks) != 2 || fallbacks[0].Provider != ProviderVocaloidFandom || fallbacks[1].Provider != ProviderMoegirl {
		t.Fatalf("default fallback provider order = %+v", fallbacks)
	}
	defaultRegistry, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, found := defaultRegistry.providers[ProviderSekaipedia]; found {
		t.Fatal("default registry compiled an implicit mutable Sekaipedia authority")
	}

	sekaipedia := historicalSekaipediaProviderConfig()
	if sekaipedia.Origin != OriginSekaipedia || sekaipedia.APIEndpoint != "https://www.sekaipedia.org/w/api.php" ||
		sekaipedia.RightsText != "Creative Commons Attribution-ShareAlike 4.0 International (CC BY-SA 4.0)" ||
		len(sekaipedia.Indexes) != 1 || !validSekaipediaAuthorityBinding(sekaipedia.Indexes[0]) {
		t.Fatalf("explicit Sekaipedia metadata = %+v", sekaipedia)
	}
	index := sekaipedia.Indexes[0]
	if got := canonicalRevisionURL(ProviderSekaipedia, index.Title, index.RevisionID); got !=
		"https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193" {
		t.Fatalf("canonical List revision URL = %q", got)
	}
	configs := append([]ProviderConfig{sekaipedia}, fallbacks...)
	registry, err := NewRegistry(configs...)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(registry.order) != "[sekaipedia moegirl vocaloid_fandom]" {
		t.Fatalf("registry order = %v", registry.order)
	}
	if _, ok := registry.providers[ProviderSekaipedia].(*sekaipediaProvider); !ok {
		t.Fatalf("registered Sekaipedia provider = %T", registry.providers[ProviderSekaipedia])
	}
}

func TestSekaipediaRejectsRedirectAwayFromExactWAPIEndpoint(t *testing.T) {
	var redirectedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/w/api.php":
			http.Redirect(w, r, "/api.php", http.StatusFound)
		case "/api.php":
			redirectedRequests.Add(1)
			http.Error(w, "wrong endpoint", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newMediaWikiClient(server.URL+"/w/api.php", 0, time.Hour, server.Client())
	provider := newSekaipediaProvider(historicalSekaipediaProviderConfig(), client)
	if _, err := provider.fetchExactRevision(context.Background(), 335193, false); err == nil ||
		!strings.Contains(err.Error(), "redirect changed endpoint path") {
		t.Fatalf("Sekaipedia endpoint redirect error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("redirected /api.php endpoint received %d requests", redirectedRequests.Load())
	}
}

func TestSekaipediaFixedListAuthorityStrictlyMapsRokiAndJourney(t *testing.T) {
	body := readSekaipediaFixture(t, "testdata/sekaipedia-list-335193.json")
	page, err := parsePageResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if page.pageID != 268 || page.revisionID != 335193 || page.title != "List of songs" ||
		page.sha1 != "b216a827f88c59f5e954a120027832fe9cd74413" ||
		canonicalFetchedAt(page.revisionTimestamp) != "2026-07-27T16:29:13Z" ||
		fmt.Sprint(page.categories) != "[Lists Project SEKAI]" {
		t.Fatalf("List identity = %+v", page)
	}
	targets, err := parseSekaipediaListAuthority(page.content)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 698 {
		t.Fatalf("linked List targets = %d, want 698", len(targets))
	}
	for _, title := range []string{"Roki", "Journey"} {
		target, found, err := selectSekaipediaListTarget(targets, title)
		if err != nil || !found || target.pageTitle != title || target.display != title {
			t.Fatalf("List target %q = %+v found=%t err=%v", title, target, found, err)
		}
	}
	if _, found, err := selectSekaipediaListTarget(targets, "ロキ"); err != nil || found {
		t.Fatalf("Japanese redirect title was trusted without canonical resolution: found=%t err=%v", found, err)
	}

	duplicated := strings.Replace(page.content, "| [[Roki]]", "| [[Roki]]\n| ignored\n|-\n| [[Roki]]", 1)
	if _, err := parseSekaipediaListAuthority(duplicated); err == nil {
		t.Fatal("duplicate or malformed fixed List target was accepted")
	}
}

func TestSekaipediaRokiFixedRevisionAcquisitionAndProjection(t *testing.T) {
	fixture := newSekaipediaFixtureServer(t)
	defer fixture.Close()
	provider := fixture.Provider(t)
	identity := rokiSekaipediaIdentity()

	candidates, err := provider.Search(context.Background(), identity)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Roki candidates=%+v err=%v", candidates, err)
	}
	candidate := candidates[0]
	if candidate.Provider != ProviderSekaipedia || candidate.Origin != OriginSekaipedia || candidate.PageID != 398 ||
		candidate.Title != "Roki" || candidate.RevisionID != 330574 ||
		candidate.RevisionTimestamp != "2026-07-15T07:59:12Z" ||
		candidate.SHA1 != "29198603574701b81b34198e63343930abd3d9a2" ||
		candidate.RawSHA256 != "3f57e7a5cfabf6d9997a2392d8f52fe40b13b95af1312c3f8857e13f405c3ebd" ||
		candidate.CanonicalURL != "https://www.sekaipedia.org/wiki/Roki?oldid=330574" ||
		candidate.Section != "Lyrics/Full Version" || candidate.RenditionKey != "full-sekai" ||
		candidate.VersionReason != model.LyricsSourceVersionReasonTaggedFullAndGame ||
		ValidateCandidateIndexEvidence(candidate) != nil {
		t.Fatalf("Roki candidate = %+v", candidate)
	}
	if len(candidate.IndexEvidence) != 2 ||
		!isFixedSekaipediaAuthorityEvidence(candidate.IndexEvidence[0], historicalSekaipediaAuthority()) ||
		candidate.IndexEvidence[0].EvidenceID != sekaipediaRevisionAcquisitionEvidenceID(
			historicalSekaipediaAuthorityEvidenceID(), candidate.IndexEvidence[0].FetchedAt, candidate.IndexEvidence[0].RawSHA256,
		) ||
		candidate.IndexEvidence[0].RevisionTimestamp != "2026-07-27T16:29:13Z" ||
		candidate.IndexEvidence[0].RawSHA256 != "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd" {
		t.Fatalf("Roki List evidence = %+v", candidate.IndexEvidence)
	}
	songEvidence := candidate.IndexEvidence[1]
	if songEvidence.EvidenceID != sekaipediaRevisionAcquisitionEvidenceID(
		sekaipediaSongEvidenceID(candidate.PageID, candidate.RevisionID), songEvidence.FetchedAt, songEvidence.RawSHA256,
	) ||
		songEvidence.PageID != candidate.PageID || songEvidence.RevisionID != candidate.RevisionID ||
		songEvidence.RevisionTimestamp != candidate.RevisionTimestamp || songEvidence.MediaWikiSHA1 != candidate.SHA1 ||
		songEvidence.Title != candidate.Title || songEvidence.CanonicalURL != candidate.CanonicalURL ||
		songEvidence.FetchedAt != candidate.FetchedAt || !equalCandidateCategories(songEvidence.Categories, candidate.Categories) {
		t.Fatalf("Roki song revision evidence = %+v", songEvidence)
	}

	fixed, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Provider != ProviderSekaipedia || !fixed.RevisionTimestamp.Equal(time.Date(2026, 7, 15, 7, 59, 12, 0, time.UTC)) ||
		fixed.RawSHA256 != candidate.RawSHA256 || len(fixed.Wikitext) == 0 ||
		!bytes.Equal(fixed.Wikitext, sekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)) || fixed.Document == nil ||
		fixed.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
		len(fixed.FixedIdentities) != 1 || len(fixed.Document.Renditions) != 2 ||
		fixed.FixedIdentities[0].RevisionTimestamp != candidate.RevisionTimestamp {
		t.Fatalf("Roki fixed revision = %+v document=%+v", fixed, fixed.Document)
	}
	byKey := make(map[string]*model.LyricsSourceRendition, len(fixed.Document.Renditions))
	for index := range fixed.Document.Renditions {
		rendition := &fixed.Document.Renditions[index]
		byKey[rendition.RenditionKey] = rendition
	}
	sekai := byKey["sekai"]
	alternate := byKey["alternate-another-vocal-len-kaito"]
	if sekai == nil || sekai.Full == nil || sekai.Game == nil || len(sekai.Full.Performers) != 2 ||
		sekai.Provenance.FullPerformerSegmentation == nil || sekai.Provenance.GamePerformerSegmentation == nil ||
		sekai.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection || len(sekai.Full.Lines) != 64 ||
		len(sekai.Game.Lines) != 25 || len(sekai.Relation.LineIDs) != 25 ||
		alternate == nil || alternate.Full != nil || alternate.Game == nil ||
		alternate.SourceKind != model.LyricsSourceRenditionAlternate ||
		alternate.Relation.Kind != model.LyricsSourceRenditionRelationNone {
		t.Fatalf("Roki native renditions = %+v", fixed.Document.Renditions)
	}
	wantRokiGameIDs := make([]string, 0, 25)
	for index := 1; index <= 21; index++ {
		wantRokiGameIDs = append(wantRokiGameIDs, fmt.Sprintf("full-%06d", index))
	}
	for index := 61; index <= 64; index++ {
		wantRokiGameIDs = append(wantRokiGameIDs, fmt.Sprintf("full-%06d", index))
	}
	if fmt.Sprint(sekai.Relation.LineIDs) != fmt.Sprint(wantRokiGameIDs) {
		t.Fatalf("Roki canonical Game projection = %v, want %v", sekai.Relation.LineIDs, wantRokiGameIDs)
	}
	firstRuby := sekai.Full.Lines[0].Segments[0].Ruby
	if sekai.Full.RubyGeneratorVersion != sekaipediaRubyGeneratorVersion || len(firstRuby) != 2 ||
		firstRuby[0].Text != "さあ " || firstRuby[0].Reading != "" || firstRuby[0].ReadingEvidence != nil ||
		firstRuby[1].Text != "眠眠打破" || firstRuby[1].Reading != "みんみんだは" {
		t.Fatalf("Roki transient kana derivation = version %q ruby %+v", sekai.Full.RubyGeneratorVersion, firstRuby)
	}
	evidence := firstRuby[1].ReadingEvidence
	if evidence == nil || evidence.Kind != model.LyricsSourceReadingEvidenceSourceTransliteration ||
		evidence.FixedIdentityKey != fixed.FixedIdentities[0].RenditionKey || evidence.RenditionKey != "sekai" ||
		evidence.Side != model.LyricsSourceRenditionSideFull || evidence.SourceRowOrdinal <= 0 ||
		evidence.SourceSegmentOrdinal <= 0 || evidence.GeneratorVersion != "" {
		t.Fatalf("Roki source reading evidence=%+v", evidence)
	}
	documentBody, err := json.Marshal(fixed.Document)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte(`"ichika"`), []byte(`"miku"`), []byte("Hoshino Ichika"),
		[]byte("Hatsune Miku"), []byte("sekaipedia-romaji-kana-v1"),
	} {
		if bytes.Contains(documentBody, forbidden) {
			t.Fatalf("source-local performer or ruby vocabulary escaped fixed document: %s", documentBody)
		}
	}
	if err := model.ValidateLyricsSourceDocument(*fixed.Document); err != nil {
		t.Fatalf("Roki document: %v", err)
	}
	if fixture.Requests() != 3 {
		t.Fatalf("Roki bounded requests = %d, want 3", fixture.Requests())
	}
}

func TestSekaipediaFixedListSurvivesClientCacheExpiryWhileSongReacquires(t *testing.T) {
	fixture := newSekaipediaFixtureServer(t)
	defer fixture.Close()
	provider := fixture.Provider(t)
	identity := rokiSekaipediaIdentity()

	first, err := provider.Search(context.Background(), identity)
	if err != nil || len(first) != 1 || len(first[0].IndexEvidence) != 2 {
		t.Fatalf("first candidates=%+v err=%v", first, err)
	}
	original := first[0]
	original.IndexEvidenceRefs = cloneIndexEvidenceRefs(first[0].IndexEvidenceRefs)
	original.IndexEvidence = cloneIndexEvidence(first[0].IndexEvidence)
	firstList, firstSong := original.IndexEvidence[0], original.IndexEvidence[1]
	first[0].IndexEvidence[0].Raw[0] ^= 0xff
	first[0].IndexEvidence[0].Categories[0] = "mutated"

	provider.client.mu.Lock()
	for key, cached := range provider.client.cache {
		cached.createdAt = time.Now().Add(-2 * provider.client.cacheTTL)
		provider.client.cache[key] = cached
	}
	provider.client.mu.Unlock()

	second, err := provider.Search(context.Background(), identity)
	if err != nil || len(second) != 1 || len(second[0].IndexEvidence) != 2 {
		t.Fatalf("second candidates=%+v err=%v", second, err)
	}
	secondList, secondSong := second[0].IndexEvidence[0], second[0].IndexEvidence[1]
	if !indexEvidenceEqual(secondList, firstList) {
		t.Fatalf("provider-lifetime List acquisition changed after client cache expiry:\nfirst=%+v\nsecond=%+v",
			firstList, secondList)
	}
	if secondSong.EvidenceID == firstSong.EvidenceID || secondSong.FetchedAt == firstSong.FetchedAt ||
		secondSong.RawSHA256 != firstSong.RawSHA256 || !bytes.Equal(secondSong.Raw, firstSong.Raw) {
		t.Fatalf("dynamic song page did not reacquire with a distinct identity:\nfirst=%+v\nsecond=%+v",
			firstSong, secondSong)
	}
	if got := fixture.ListRequests(); got != 1 {
		t.Fatalf("List requests after client cache expiry = %d, want 1", got)
	}
	if got := fixture.TitleRequests(); got != 2 {
		t.Fatalf("dynamic title requests after client cache expiry = %d, want 2", got)
	}

	independent := fixture.Provider(t)
	third, err := independent.Search(context.Background(), identity)
	if err != nil || len(third) != 1 || len(third[0].IndexEvidence) != 2 {
		t.Fatalf("independent candidates=%+v err=%v", third, err)
	}
	thirdList := third[0].IndexEvidence[0]
	if thirdList.EvidenceID == firstList.EvidenceID || thirdList.FetchedAt == firstList.FetchedAt ||
		thirdList.RawSHA256 != firstList.RawSHA256 || !bytes.Equal(thirdList.Raw, firstList.Raw) {
		t.Fatalf("independent List acquisition did not receive a distinct identity:\nfirst=%+v\nthird=%+v",
			firstList, thirdList)
	}
	if got := fixture.ListRequests(); got != 2 {
		t.Fatalf("List requests across independent providers = %d, want 2", got)
	}
	if err := ValidateCandidatesIndexEvidence([]Candidate{original, second[0], third[0]}); err != nil {
		t.Fatalf("reused and independent Sekaipedia acquisitions conflicted: %v", err)
	}
}

func TestSekaipediaFixedAuthorityPreservesKnownEmptyCategoryIdentity(t *testing.T) {
	fixture := newSekaipediaFixtureServer(t)
	defer fixture.Close()

	var response map[string]any
	if err := json.Unmarshal(fixture.pages["Roki"], &response); err != nil {
		t.Fatal(err)
	}
	query, ok := response["query"].(map[string]any)
	if !ok {
		t.Fatal("Roki fixture query has unexpected shape")
	}
	pages, ok := query["pages"].([]any)
	if !ok || len(pages) != 1 {
		t.Fatal("Roki fixture pages have unexpected shape")
	}
	page, ok := pages[0].(map[string]any)
	if !ok {
		t.Fatal("Roki fixture page has unexpected shape")
	}
	page["categories"] = []any{}
	withoutCategories, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	fixture.pages["Roki"] = withoutCategories
	fixture.revisions[330574] = withoutCategories

	provider := fixture.Provider(t)
	identity := rokiSekaipediaIdentity()
	var candidate Candidate
	for attempt := range 2 {
		candidates, err := provider.Search(context.Background(), identity)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("search %d candidates=%+v err=%v", attempt+1, candidates, err)
		}
		candidate = candidates[0]
		if candidate.Categories == nil || len(candidate.Categories) != 0 || len(candidate.IndexEvidence) != 2 ||
			candidate.IndexEvidence[1].Categories == nil || len(candidate.IndexEvidence[1].Categories) != 0 ||
			!validSekaipediaCandidate(candidate) || ValidateCandidateIndexEvidence(candidate) != nil {
			t.Fatalf("search %d lost known-empty category identity: %+v", attempt+1, candidate)
		}
	}

	fixed, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Categories == nil || len(fixed.Categories) != 0 || len(fixed.IndexEvidence) != 2 ||
		fixed.IndexEvidence[1].Categories == nil || len(fixed.IndexEvidence[1].Categories) != 0 ||
		len(fixed.FixedIdentities) == 0 || fixed.Document == nil {
		t.Fatalf("fixed revision lost known-empty category identity: %+v", fixed)
	}
	for index, identity := range fixed.FixedIdentities {
		if identity.Categories == nil || len(identity.Categories) != 0 {
			t.Fatalf("fixed identity %d categories = %#v", index, identity.Categories)
		}
	}
	if err := model.ValidateLyricsSourceDocument(*fixed.Document); err != nil {
		t.Fatalf("known-empty category document: %v", err)
	}
	if fixture.ListRequests() != 1 || fixture.TitleRequests() != 1 || fixture.Requests() != 3 {
		t.Fatalf("known-empty category requests total/list/title = %d/%d/%d, want 3/1/1",
			fixture.Requests(), fixture.ListRequests(), fixture.TitleRequests())
	}
}

func TestSekaipediaConcurrentFirstFixedListAccessResolvesOnce(t *testing.T) {
	listBody := readSekaipediaFixture(t, "testdata/sekaipedia-list-335193.json")
	config := historicalSekaipediaProviderConfig()
	fixed := config.Indexes[0]
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("revids") != strconv.Itoa(fixed.RevisionID) || r.URL.Query().Get("maxlag") != mediaWikiMaxLag {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(listBody)
	}))
	defer server.Close()
	config.APIEndpoint = server.URL + "/w/api.php"
	config.CrawlDelay = 0
	provider := newSekaipediaProvider(
		config,
		newMediaWikiClient(config.APIEndpoint, 0, time.Hour, server.Client()),
	)

	const callers = 16
	begin := make(chan struct{})
	results := make(chan fixedAuthorityAcquisition, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-begin
			acquisition, err := provider.acquireFixedIndex(context.Background(), fixed)
			results <- acquisition
			errs <- err
		}()
	}
	close(begin)
	<-started
	waitForFixedAuthorityParticipants(t, &provider.fixedAuthorities, fixed, callers)
	close(release)

	acquisitions := make([]fixedAuthorityAcquisition, 0, callers)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		acquisitions = append(acquisitions, <-results)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("concurrent fixed-List requests = %d, want 1", got)
	}
	expected := cloneFixedAuthorityAcquisition(acquisitions[0])
	for index, acquisition := range acquisitions[1:] {
		if !indexEvidenceEqual(acquisition.evidence, expected.evidence) ||
			!bytes.Equal(acquisition.page.rawResponse, expected.page.rawResponse) ||
			acquisition.page.fetchedAt != expected.page.fetchedAt {
			t.Fatalf("concurrent List acquisition %d changed: %+v", index+1, acquisition)
		}
	}
	acquisitions[0].evidence.Raw[0] ^= 0xff
	acquisitions[0].evidence.Categories[0] = "mutated"
	acquisitions[0].page.rawResponse[0] ^= 0xff
	acquisitions[0].page.categories[0] = "mutated"
	cached, err := provider.acquireFixedIndex(context.Background(), fixed)
	if err != nil || !indexEvidenceEqual(cached.evidence, expected.evidence) ||
		!bytes.Equal(cached.page.rawResponse, expected.page.rawResponse) ||
		!equalCandidateCategories(cached.page.categories, expected.page.categories) {
		t.Fatalf("defensive fixed-List clone = %+v err=%v", cached, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cached fixed-List requests = %d, want 1", got)
	}
}

func TestSekaipediaFixedListFailuresAreNotCached(t *testing.T) {
	listBody := readSekaipediaFixture(t, "testdata/sekaipedia-list-335193.json")
	for _, test := range []struct {
		name           string
		wantErr        error
		wantHTTPStatus int
		first          func(http.ResponseWriter)
	}{
		{
			name: "transport error", wantHTTPStatus: http.StatusServiceUnavailable,
			first: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
		},
		{
			name: "malformed response", wantErr: ErrMalformedResponse,
			first: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("{"))
			},
		},
		{
			name: "revision content drift", wantErr: ErrRevisionChanged,
			first: func(w http.ResponseWriter) {
				fixed := historicalSekaipediaProviderConfig().Indexes[0]
				_, _ = w.Write(semanticSekaipediaRevisionResponse(
					t, fixed, "tampered revision content\n", nil,
				))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := historicalSekaipediaProviderConfig()
			fixed := config.Indexes[0]
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("revids") != strconv.Itoa(fixed.RevisionID) {
					http.Error(w, "unexpected query", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if attempts.Add(1) == 1 {
					test.first(w)
					return
				}
				_, _ = w.Write(listBody)
			}))
			defer server.Close()
			config.APIEndpoint = server.URL + "/w/api.php"
			config.CrawlDelay = 0
			provider := newSekaipediaProvider(
				config,
				newMediaWikiClient(config.APIEndpoint, 0, time.Hour, server.Client()),
			)

			_, firstErr := provider.acquireFixedIndex(context.Background(), fixed)
			if test.wantHTTPStatus != 0 {
				var httpErr *HTTPError
				if !errors.As(firstErr, &httpErr) || httpErr.StatusCode != test.wantHTTPStatus {
					t.Fatalf("first fixed-List HTTP error = %v, want status %d", firstErr, test.wantHTTPStatus)
				}
			} else if !errors.Is(firstErr, test.wantErr) {
				t.Fatalf("first fixed-List error = %v, want %v", firstErr, test.wantErr)
			}
			acquisition, err := provider.acquireFixedIndex(context.Background(), fixed)
			if err != nil || acquisition.evidence.EvidenceID == "" {
				t.Fatalf("retry acquisition=%+v err=%v", acquisition, err)
			}
			if _, err := provider.acquireFixedIndex(context.Background(), fixed); err != nil {
				t.Fatal(err)
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("fixed-List attempts = %d, want failed attempt plus one successful retry", got)
			}
		})
	}
}

func TestSekaipediaCanceledFixedListAcquisitionIsNotCached(t *testing.T) {
	listBody := readSekaipediaFixture(t, "testdata/sekaipedia-list-335193.json")
	config := historicalSekaipediaProviderConfig()
	fixed := config.Indexes[0]
	var attempts atomic.Int32
	started := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			startedOnce.Do(func() { close(started) })
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(listBody)
	}))
	defer server.Close()
	config.APIEndpoint = server.URL + "/w/api.php"
	config.CrawlDelay = 0
	provider := newSekaipediaProvider(
		config,
		newMediaWikiClient(config.APIEndpoint, 0, time.Hour, server.Client()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := provider.acquireFixedIndex(ctx, fixed)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fixed-List error = %v", err)
	}
	waitForFixedAuthorityIdle(t, &provider.fixedAuthorities, fixed)
	if _, err := provider.acquireFixedIndex(context.Background(), fixed); err != nil {
		t.Fatalf("fixed-List retry after cancellation: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("fixed-List attempts after cancellation = %d, want 2", got)
	}
}

func TestSekaipediaJourneyRetainsBothFixedPeersAcrossCatalogPolicies(t *testing.T) {
	for _, test := range []struct {
		name                string
		policy              PerformerSegmentationPolicy
		wantArtifactSection string
		wantArtifactKey     string
	}{
		{
			name:                "SEKAI eligible selects SEKAI compatibility view without suppressing VIRTUAL SINGER",
			policy:              PerformerSegmentationSekaiEligible,
			wantArtifactSection: "Lyrics/SEKAI",
			wantArtifactKey:     "full-sekai",
		},
		{
			name:                "catalog disabled selects VIRTUAL SINGER compatibility view without suppressing SEKAI",
			policy:              PerformerSegmentationDisabled,
			wantArtifactSection: "Lyrics/VIRTUAL SINGER",
			wantArtifactKey:     "full-vocaloid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSekaipediaFixtureServer(t)
			defer fixture.Close()
			provider := fixture.Provider(t)
			identity := journeySekaipediaIdentity(test.policy)
			candidates, err := provider.Search(context.Background(), identity)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("Journey candidates=%+v err=%v", candidates, err)
			}
			candidate := candidates[0]
			if candidate.PageID != 28040 || candidate.RevisionID != 326737 ||
				candidate.RevisionTimestamp != "2026-07-01T17:11:50Z" ||
				candidate.SHA1 != "a0b581baeb63a282df204f9df0bbf9c3795ef86a" ||
				candidate.RawSHA256 != "283902b0fddb486691f9e7ed2961ab6ff707a141c04e553dd73852a9a2d23e34" ||
				candidate.VersionReason != model.LyricsSourceVersionReasonUntaggedUncutIdentity {
				t.Fatalf("Journey candidate = %+v", candidate)
			}
			fixed, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidate)
			if err != nil {
				t.Fatal(err)
			}
			document := fixed.Document
			if document == nil || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
				len(document.FixedIdentities) != 1 || len(document.Renditions) != 2 ||
				document.FixedIdentities[0].Section != test.wantArtifactSection ||
				document.FixedIdentities[0].RenditionKey != test.wantArtifactKey {
				t.Fatalf("Journey native document = %+v", document)
			}
			byKey := make(map[string]model.LyricsSourceRendition, len(document.Renditions))
			for _, rendition := range document.Renditions {
				byKey[rendition.RenditionKey] = rendition
			}
			for _, expected := range []struct {
				key  string
				kind model.LyricsSourceRenditionKind
				path string
			}{
				{key: "sekai", kind: model.LyricsSourceRenditionSekai, path: "SEKAI"},
				{key: "vocaloid", kind: model.LyricsSourceRenditionVocaloid, path: "VIRTUAL SINGER"},
			} {
				rendition, ok := byKey[expected.key]
				if !ok || rendition.SourceKind != expected.kind || rendition.Full == nil || rendition.Game != nil ||
					len(rendition.SourceTabPaths) != 1 || len(rendition.SourceTabPaths[0]) != 1 ||
					rendition.SourceTabPaths[0][0] != expected.path || len(rendition.Full.Performers) != 6 ||
					rendition.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceSourceComplete ||
					rendition.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceNone ||
					rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
					len(rendition.Relation.LineIDs) != len(rendition.Full.Lines) || rendition.PrivateReview == nil {
					t.Fatalf("Journey %s rendition=%+v", expected.key, rendition)
				}
				for index, line := range rendition.Full.Lines {
					if rendition.Relation.LineIDs[index] != line.ID || len(line.Segments) == 0 {
						t.Fatalf("Journey %s line %d relation/segments=%+v", expected.key, index+1, line)
					}
					for _, segment := range line.Segments {
						if len(segment.PerformerIDs) == 0 {
							t.Fatalf("Journey %s line %d lost source performer attribution: %+v", expected.key, index+1, line)
						}
					}
				}
			}
			if err := model.ValidateLyricsSourceDocument(*document); err != nil {
				t.Fatalf("Journey document: %v", err)
			}
		})
	}
}

func TestSekaipediaJourneyVirtualSingerTransientRomanizationDerivesKana(t *testing.T) {
	content := sekaipediaFixturePageContent(t, "Journey")
	lyricsSection, err := sekaipediaTopLevelSection(content, "Lyrics")
	if err != nil {
		t.Fatal(err)
	}
	tabs, _, err := parseSekaipediaLyricsTabs(lyricsSection)
	if err != nil {
		t.Fatal(err)
	}
	set := sekaipediaSingerSet{kind: "vocaloid", ids: []string{"miku", "rin", "len", "luka", "meiko", "kaito"}}
	templates, err := parseSekaipediaTemplateSequence(tabs["VIRTUAL SINGER"])
	if err != nil {
		t.Fatal(err)
	}
	for templateIndex, template := range templates[1 : len(templates)-1] {
		params, err := sekaipediaNamedParameters(template.fields, map[string]bool{
			"japanese": true, "romaji": true, "english": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		japaneseLines, err := parseSekaipediaLyricColumn(params["japanese"], set)
		if err != nil {
			t.Fatal(err)
		}
		romajiLines, err := parseSekaipediaLyricColumn(params["romaji"], set)
		if err != nil {
			t.Fatal(err)
		}
		if len(japaneseLines) != len(romajiLines) {
			t.Fatalf("template %d line topology differs", templateIndex+1)
		}
		for lineIndex := range japaneseLines {
			if len(japaneseLines[lineIndex].segments) != len(romajiLines[lineIndex].segments) {
				t.Fatalf("template %d line %d segment topology differs", templateIndex+1, lineIndex+1)
			}
			for segmentIndex := range japaneseLines[lineIndex].segments {
				japanese := japaneseLines[lineIndex].segments[segmentIndex].text
				ruby, ok := deriveSekaipediaRuby(japanese, romajiLines[lineIndex].segments[segmentIndex].text)
				if !ok || rubySpansText(ruby) != japanese {
					t.Fatalf("template %d line %d segment %d failed transient kana derivation for %q",
						templateIndex+1, lineIndex+1, segmentIndex+1, japanese)
				}
			}
		}
	}
}

func TestSekaipediaParserFailsClosedOnMalformedMissingStaleAndMismatch(t *testing.T) {
	roki := sekaipediaFixturePageContent(t, "Roki")
	journey := sekaipediaFixturePageContent(t, "Journey")
	for _, test := range []struct {
		name      string
		content   string
		policy    PerformerSegmentationPolicy
		wantError bool
	}{
		{
			name:      "malformed Lyrics head",
			content:   strings.Replace(roki, "{{Lyrics head", "{{Lyrics unknown", 1),
			policy:    PerformerSegmentationSekaiEligible,
			wantError: true,
		},
		{
			name:      "malformed Lyrics line",
			content:   strings.Replace(roki, "{{Lyrics line", "{{Lyrics row", 1),
			policy:    PerformerSegmentationSekaiEligible,
			wantError: true,
		},
		{
			name:      "malformed Lyrics tail",
			content:   strings.Replace(roki, "{{Lyrics tail|", "{{Lyrics tail|unexpected=1|", 1),
			policy:    PerformerSegmentationSekaiEligible,
			wantError: true,
		},
		{
			name:      "missing Lyrics section",
			content:   strings.Replace(roki, "== Lyrics ==", "== Words ==", 1),
			policy:    PerformerSegmentationSekaiEligible,
			wantError: true,
		},
		{
			name: "stale same-lyrics note",
			content: strings.Replace(journey, sekaipediaSameLyricsNote,
				"''The game cut and full versions contains the same lyrics.''", 1),
			policy:    PerformerSegmentationSekaiEligible,
			wantError: true,
		},
		{
			name:      "Game text is not a Full subsequence",
			content:   strings.Replace(roki, "さあ 眠眠打破", "さあ 眠眠打破 mismatch", 1),
			policy:    PerformerSegmentationSekaiEligible,
			wantError: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseSekaipediaSong(test.content, test.policy)
			if test.wantError {
				if err == nil {
					t.Fatal("invalid fixed revision was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("independent Full/Game extraction was rejected: %v", err)
			}
			if parsed.Game == nil || len(parsed.GameLineIndexes) != 0 ||
				parsed.ReasonCode != model.LyricsSourceVersionReasonTaggedFullAndGame {
				t.Fatalf("independent Full/Game extraction=%+v", parsed)
			}
		})
	}
}

func TestSekaipediaGameProjectionUsesCanonicalLeftmostJapaneseRowSubsequence(t *testing.T) {
	row := func(text string, stanza bool, performerIDs ...string) sekaipediaColumnLine {
		return sekaipediaColumnLine{
			stanzaBreakBefore: stanza,
			segments: []sekaipediaColumnSegment{{
				text: text, performerIDs: append([]string(nil), performerIDs...),
			}},
		}
	}
	full := []sekaipediaColumnLine{row("同じ", false), row("同じ", false), row("終わり", false)}
	game := []sekaipediaColumnLine{row("同じ", false), row("終わり", false)}
	projection, err := sekaipediaOrderedSubsequence(full, game)
	if err != nil || fmt.Sprint(projection) != "[0 2]" {
		t.Fatalf("canonical repeated-row projection = %v err=%v", projection, err)
	}

	structuredFull := []sekaipediaColumnLine{
		row("同じ", false, "miku"),
		row("同じ", true, "rin"),
		row("終わり", false),
	}
	structuredGame := []sekaipediaColumnLine{row("同じ", true, "rin"), row("終わり", false)}
	projection, err = sekaipediaOrderedSubsequence(structuredFull, structuredGame)
	if err != nil || fmt.Sprint(projection) != "[1 2]" {
		t.Fatalf("Japanese-row structured projection = %v err=%v", projection, err)
	}

	performerMismatch := []sekaipediaColumnLine{row("同じ", true, "miku"), row("終わり", false)}
	if projection, err = sekaipediaOrderedSubsequence(structuredFull, performerMismatch); !errors.Is(err, ErrUnsupportedTable) || projection != nil {
		t.Fatalf("performer-mismatched projection = %v err=%v", projection, err)
	}

	stanzaMismatchFull := []sekaipediaColumnLine{row("同じ", true), row("終わり", false)}
	stanzaMismatchGame := []sekaipediaColumnLine{row("同じ", false), row("終わり", false)}
	if projection, err = sekaipediaOrderedSubsequence(stanzaMismatchFull, stanzaMismatchGame); !errors.Is(err, ErrUnsupportedTable) || projection != nil {
		t.Fatalf("stanza-mismatched projection = %v err=%v", projection, err)
	}
}

func TestSekaipediaGameProjectionTextFallbackRequiresUniqueEmbedding(t *testing.T) {
	row := func(text string, stanza bool, performerIDs ...string) sekaipediaColumnLine {
		return sekaipediaColumnLine{
			stanzaBreakBefore: stanza,
			segments: []sekaipediaColumnSegment{{
				text: text, performerIDs: append([]string(nil), performerIDs...),
			}},
		}
	}
	full := []sekaipediaColumnLine{
		row("最初", false, "miku"), row("対象", true, "rin"), row("終わり", false),
	}
	game := []sekaipediaColumnLine{row("対象", false, "miku"), row("終わり", true)}
	projection, err := sekaipediaUniqueTextSubsequence(full, game)
	if err != nil || fmt.Sprint(projection) != "[1 2]" {
		t.Fatalf("unique text projection=%v err=%v", projection, err)
	}
	ambiguousFull := []sekaipediaColumnLine{row("同じ", false), row("同じ", true), row("終わり", false)}
	ambiguousGame := []sekaipediaColumnLine{row("同じ", false), row("終わり", false)}
	if projection, err = sekaipediaUniqueTextSubsequence(ambiguousFull, ambiguousGame); !errors.Is(err, ErrAmbiguous) || projection != nil {
		t.Fatalf("ambiguous text projection=%v err=%v", projection, err)
	}
}

func TestSekaipediaSingerAliasesAndAggregatesFailClosed(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "miku"}}
	for name, value := range map[string]string{
		"zero match":        "Unknown Singer",
		"duplicate alias":   "Miku, Hatsune Miku",
		"unknown aggregate": "Everyone",
		"unproven VS":       "VS",
		"unproven leaders":  "Unitleaders",
	} {
		t.Run(name, func(t *testing.T) {
			candidateSet := set
			if strings.HasPrefix(name, "unproven") {
				candidateSet = sekaipediaSingerSet{}
			}
			if _, err := resolveSekaipediaSingerList(value, candidateSet, true); err == nil {
				t.Fatalf("singer value %q was accepted", value)
			}
		})
	}

	ambiguous := append([]sekaipediaSinger(nil), sekaipediaSingers...)
	ambiguous[1].aliases = append(append([]string(nil), ambiguous[1].aliases...), "Ichika")
	if _, _, err := buildSekaipediaSingerAliases(ambiguous); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous versioned alias table error = %v", err)
	}

	partial, err := parseSekaipediaLyricColumn("{{Lyric|Ichika|歌う}} untagged", set)
	if err != nil || len(partial) != 1 || len(partial[0].segments) != 2 ||
		partial[0].segments[0].text != "歌う" ||
		!equalStrings(partial[0].segments[0].performerIDs, []string{"ichika"}) ||
		partial[0].segments[1].text != "untagged" || len(partial[0].segments[1].performerIDs) != 0 {
		t.Fatalf("mixed tagged/plain segmentation=%+v err=%v", partial, err)
	}
	vsSet := sekaipediaSingerSet{kind: "vocaloid", ids: []string{"miku", "rin", "len", "luka", "meiko", "kaito"}}
	ids, err := resolveSekaipediaSingerList("VS", vsSet, true)
	if err != nil || fmt.Sprint(ids) != "[miku rin len luka meiko kaito]" {
		t.Fatalf("proven finite VS aggregate = %v err=%v", ids, err)
	}
	leadersSet := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "minori", "kohane", "tsukasa", "kanade", "miku"}}
	ids, err = resolveSekaipediaSingerList("Unitleaders, Miku", leadersSet, true)
	if err != nil || fmt.Sprint(ids) != "[ichika minori kohane tsukasa kanade miku]" {
		t.Fatalf("proven finite Unitleaders aggregate = %v err=%v", ids, err)
	}
}

func TestSekaipediaPartialAlignmentPreservesJapanesePerformerTagsWithGeneratedRuby(t *testing.T) {
	roki := sekaipediaFixturePageContent(t, "Roki")
	fullOffset := strings.Index(roki, "Full Version")
	if fullOffset < 0 {
		t.Fatal("missing Full fixture tab")
	}
	romajiOffset := strings.Index(roki[fullOffset:], "| romaji")
	if romajiOffset < 0 {
		t.Fatal("missing Full romaji fixture column")
	}
	romajiOffset += fullOffset
	for _, test := range []struct {
		name        string
		replacement string
	}{
		{name: "singer topology differs", replacement: "{{Lyric|Miku|saa minmin daha}}"},
		{name: "transient text differs", replacement: "{{Lyric|Ichika|utterly unrelated}}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutatedTail := strings.Replace(roki[romajiOffset:], "{{Lyric|Ichika|saa minmin daha}}", test.replacement, 1)
			if mutatedTail == roki[romajiOffset:] {
				t.Fatal("fixed fixture mutation did not apply")
			}
			parsed, err := parseSekaipediaSong(roki[:romajiOffset]+mutatedTail, PerformerSegmentationSekaiEligible)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Full.Lines) == 0 || parsed.Full.Lines[0].Japanese != "さあ 眠眠打破" ||
				len(parsed.Full.Performers) != 2 || parsed.Full.RubyGeneratorVersion != sekaipediaRubyGeneratorVersion || !parsed.AuthoritativeStructured {
				t.Fatalf("partial alignment lost Japanese performer evidence or generated ruby = %+v", parsed.Full)
			}
			for _, line := range parsed.Full.Lines {
				var text strings.Builder
				for _, segment := range line.Segments {
					text.WriteString(segment.Text)
					if len(segment.PerformerIDs) == 0 || rubySpansText(segment.Ruby) != segment.Text ||
						!rubySpansCoverKanji(segment.Ruby) {
						t.Fatalf("partial alignment did not preserve source segment with complete ruby: %+v", line)
					}
				}
				if text.String() != line.Japanese {
					t.Fatalf("partial alignment changed Japanese text: %+v", line)
				}
			}
			wantProjection := make([]int, 0, 25)
			for index := 0; index <= 20; index++ {
				wantProjection = append(wantProjection, index)
			}
			for index := 60; index <= 63; index++ {
				wantProjection = append(wantProjection, index)
			}
			if fmt.Sprint(parsed.GameLineIndexes) != fmt.Sprint(wantProjection) {
				t.Fatalf("partial alignment changed canonical Game projection = %v, want %v", parsed.GameLineIndexes, wantProjection)
			}
		})
	}
}

func TestSekaipediaSelectedJapaneseWikitextIsCanonicalAndBounded(t *testing.T) {
	lines := []StructuredLine{
		{Japanese: "最初"},
		{Japanese: "次", StanzaBreakBefore: true},
		{Japanese: "最後"},
	}
	if got := string(sekaipediaFixedJapaneseWikitext(lines)); got != "最初\n\n次\n最後" {
		t.Fatalf("selected-Japanese serialization = %q", got)
	}
	invalid := append([]StructuredLine(nil), lines...)
	invalid[1].Japanese = "複数\n行"
	if got := sekaipediaFixedJapaneseWikitext(invalid); got != nil {
		t.Fatalf("multiline selected-Japanese input serialized as %q", got)
	}

	boundary := make([]StructuredLine, maxExtractedTextBytes/maxExtractedLineBytes)
	for index := range boundary {
		boundary[index].Japanese = strings.Repeat("a", maxExtractedLineBytes)
	}
	serialized := sekaipediaFixedJapaneseWikitext(boundary)
	if len(serialized) != maxExtractedTextBytes+len(boundary)-1 {
		t.Fatalf("maximum valid selected-Japanese serialization bytes = %d", len(serialized))
	}
	tooLarge := append(append([]StructuredLine(nil), boundary...), StructuredLine{Japanese: "a"})
	if got := sekaipediaFixedJapaneseWikitext(tooLarge); got != nil {
		t.Fatalf("oversized selected-Japanese input serialized with %d bytes", len(got))
	}
}

func TestSekaipediaRomanizationNeverLeavesSelectedJapaneseBoundary(t *testing.T) {
	fixture := newSekaipediaFixtureServer(t)
	defer fixture.Close()
	provider := fixture.Provider(t)
	identity := journeySekaipediaIdentity(PerformerSegmentationDisabled)
	candidates, err := provider.Search(context.Background(), identity)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	fixed, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(fixed.Wikitext) == 0 ||
		!bytes.Equal(fixed.Wikitext, sekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)) ||
		!bytes.Contains(fixed.Wikitext, []byte("溜めてきた")) ||
		bytes.Contains(bytes.ToLower(fixed.Wikitext), []byte("tamete kita")) {
		t.Fatalf("Sekaipedia selected-Japanese boundary = %q", fixed.Wikitext)
	}
	for name, value := range map[string]any{
		"candidate":  candidates[0],
		"fixed":      fixed,
		"document":   fixed.Document,
		"extraction": fixed.Extraction,
	} {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		lower := bytes.ToLower(body)
		if bytes.Contains(lower, []byte("tamete kita")) || bytes.Contains(lower, []byte(`"romaji"`)) ||
			bytes.Contains(lower, []byte(`"romanized"`)) || bytes.Contains(body, []byte(sekaipediaRightsText)) {
			t.Fatalf("%s leaked transient romanization or unpublished rights metadata: %s", name, body)
		}
	}
}

func TestSekaipediaFixedRevisionDriftFailsClosed(t *testing.T) {
	fixture := newSekaipediaFixtureServer(t)
	defer fixture.Close()
	provider := fixture.Provider(t)
	identity := rokiSekaipediaIdentity()
	candidates, err := provider.Search(context.Background(), identity)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	fixture.driftTarget.Store(true)
	if _, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidates[0]); !errors.Is(err, ErrRevisionChanged) {
		t.Fatalf("fixed revision drift error = %v", err)
	}
}
