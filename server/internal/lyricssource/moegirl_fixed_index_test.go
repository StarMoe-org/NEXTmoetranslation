package lyricssource

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/model"
)

func TestMoegirlFixedIndexDiscoveryAndFixedRevisionFetch(t *testing.T) {
	indexBody, err := os.ReadFile("testdata/moegirl-index-fixed.wiki")
	if err != nil {
		t.Fatal(err)
	}
	targetBody, err := os.ReadFile("testdata/moegirl-section-full-game.wiki")
	if err != nil {
		t.Fatal(err)
	}
	indexSHA := fmt.Sprintf("%x", sha1.Sum(indexBody))
	targetSHA := fmt.Sprintf("%x", sha1.Sum(targetBody))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query := r.URL.Query()
		if query.Get("maxlag") != mediaWikiMaxLag {
			t.Fatalf("MediaWiki request missing maxlag: %s", r.URL.RawQuery)
		}
		var body []byte
		var pageID, revisionID int
		var title string
		var categories []string
		switch {
		case query.Get("revids") == "11":
			body, pageID, revisionID, title, categories = indexBody, 1, 11, "固定索引", []string{"Index", "Project SEKAI"}
		case query.Get("titles") == "合成演唱歌曲/原创歌曲":
			body, pageID, revisionID, title, categories = targetBody, 2, 22, "合成演唱歌曲/原创歌曲", []string{"游戏音乐", "歌曲"}
		case query.Get("revids") == "22":
			body, pageID, revisionID, title, categories = targetBody, 2, 22, "合成演唱歌曲/原创歌曲", []string{"游戏音乐", "歌曲"}
		default:
			t.Fatalf("unexpected MediaWiki query: %s", r.URL.RawQuery)
		}
		sha := fmt.Sprintf("%x", sha1.Sum(body))
		response := map[string]any{"query": map[string]any{"pages": map[string]any{
			fmt.Sprint(pageID): map[string]any{
				"pageid": pageID, "title": title,
				"categories": mediaWikiCategories(categories),
				"revisions": []any{map[string]any{
					"revid": revisionID, "sha1": sha,
					"slots": map[string]any{"main": map[string]any{"content": string(body)}},
				}},
			},
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newMediaWikiClient(server.URL, 0, time.Hour, server.Client())
	provider := newMoegirlProvider(ProviderConfig{
		Provider: ProviderMoegirl, Enabled: true, Origin: OriginMoegirl, APIEndpoint: moegirlAPI,
		Indexes:    []FixedIndex{{PageID: 1, RevisionID: 11, SHA1: indexSHA, Title: "固定索引"}},
		CrawlDelay: 10 * time.Second, CacheTTL: time.Hour,
	}, client)
	identity := MusicIdentity{MusicID: 10, JapaneseTitle: "合成試験曲", ProducerMetadata: "制作者",
		Lyricist: "制作者", Composer: "制作者", Arranger: "制作者",
		PerformerSegmentationPolicy: PerformerSegmentationSekaiEligible}
	candidates, err := provider.Search(context.Background(), identity)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	candidate := candidates[0]
	if candidate.Provider != ProviderMoegirl || candidate.Origin != OriginMoegirl || candidate.PageID != 2 ||
		candidate.RevisionID != 22 || candidate.SHA1 != targetSHA || candidate.Section != "合成试验曲/歌词" ||
		candidate.RenditionKey != "full-sekai" || candidate.VersionReason != model.LyricsSourceVersionReasonTaggedFullAndGame ||
		candidate.CanonicalURL != "https://moegirl.icu/index.php?oldid=22&title="+url.QueryEscape("合成演唱歌曲/原创歌曲") ||
		len(candidate.IndexEvidenceRefs) != 1 {
		t.Fatalf("candidate identity = %+v", candidate)
	}
	if len(candidate.IndexEvidence) != 1 {
		t.Fatalf("candidate index evidence = %+v", candidate.IndexEvidence)
	}
	indexEvidence := candidate.IndexEvidence[0]
	wantEvidenceID := mediaWikiRevisionAcquisitionEvidenceID(
		ProviderMoegirl, "search:moegirl:1", indexEvidence.FetchedAt, indexEvidence.RawSHA256,
	)
	if candidate.IndexEvidenceRefs[0].EvidenceID != wantEvidenceID || indexEvidence.EvidenceID != wantEvidenceID {
		t.Fatalf("fixed-index acquisition identity = %+v refs=%+v", indexEvidence, candidate.IndexEvidenceRefs)
	}
	indexDigest := sha256.Sum256(indexBody)
	if indexEvidence.Kind != IndexEvidenceKindMediaWikiRevision || indexEvidence.Provider != ProviderMoegirl ||
		indexEvidence.Origin != OriginMoegirl || indexEvidence.PageID != 1 || indexEvidence.RevisionID != 11 ||
		indexEvidence.MediaWikiSHA1 != indexSHA || indexEvidence.Title != "固定索引" ||
		indexEvidence.CanonicalURL != canonicalRevisionURL(ProviderMoegirl, "固定索引", 11) ||
		fmt.Sprint(indexEvidence.Categories) != "[Index Project SEKAI]" || indexEvidence.CanonicalRequestURL != "" ||
		!bytes.Equal(indexEvidence.Raw, indexBody) || indexEvidence.RawSHA256 != fmt.Sprintf("%x", indexDigest) ||
		indexEvidence.SHA256 != indexEvidence.RawSHA256 || candidate.IndexEvidenceRefs[0].SHA256 != indexEvidence.SHA256 {
		t.Fatalf("fixed-index evidence = %+v", indexEvidence)
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, indexEvidence.FetchedAt)
	if err != nil || canonicalFetchedAt(fetchedAt) != indexEvidence.FetchedAt || ValidateCandidateIndexEvidence(candidate) != nil {
		t.Fatalf("fixed-index evidence timestamp or resolution invalid: %+v err=%v", indexEvidence, err)
	}
	cachedCandidates, err := provider.Search(context.Background(), identity)
	if err != nil || len(cachedCandidates) != 1 || !indexEvidenceEqual(cachedCandidates[0].IndexEvidence[0], indexEvidence) {
		t.Fatalf("cached fixed-index evidence changed: %+v err=%v", cachedCandidates, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("cached repeated discovery made %d requests, want 2", got)
	}

	fixed, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Provider != ProviderMoegirl || fixed.Origin != OriginMoegirl || fixed.Document == nil ||
		len(fixed.IndexEvidence) != 1 || !indexEvidenceEqual(fixed.IndexEvidence[0], indexEvidence) ||
		len(fixed.FixedIdentities) != 2 || fixed.FixedIdentities[0].FetchedAt == "" ||
		fixed.FixedIdentities[0].FetchedAt != canonicalFetchedAt(fixed.FetchedAt) ||
		fixed.Document.Full.Version.Kind != "sekai" || len(fixed.Extraction.Performers) != 2 ||
		len(fixed.Document.Full.Performers) != 2 || fixed.Document.Provenance.PerformerSegmentation == nil ||
		len(fixed.Document.Full.Lines) != 5 || len(fixed.Document.Full.Lines[4].Segments) != 3 ||
		!equalStrings(fixed.Document.Full.Lines[4].Segments[0].PerformerIDs, []string{"初音未来"}) ||
		len(fixed.Document.Full.Lines[4].Segments[1].PerformerIDs) != 0 ||
		!equalStrings(fixed.Document.Full.Lines[4].Segments[2].PerformerIDs, []string{"镜音铃"}) ||
		fixed.Document.Game == nil || len(fixed.Document.Game.Lines) != 4 ||
		fixed.Document.Game.Lines[0].Text != "共通の理由" || fixed.Document.Game.Lines[1].Text != "同じ歌" ||
		fixed.Document.Game.Lines[2].Text != "二人で先へ　編み合わせて" || fixed.Document.Game.Lines[3].Text != "弱さ歌おう" ||
		fixed.Document.GameProjection == nil || len(fixed.Document.GameProjection.LineIDs) != 4 {
		t.Fatalf("fixed identity = %+v document=%+v", fixed, fixed.Document)
	}
	if err := model.ValidateLyricsSourceDocument(*fixed.Document); err != nil {
		t.Fatalf("fixed document: %v", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("fixed revid fetch requests = %d, want 3", got)
	}
}

func TestMoegirlFixedIndexSurvivesClientCacheExpiryAndIndependentProviderReacquires(t *testing.T) {
	const pageTitle = "再取得対象頁"
	indexBody := "* [[" + pageTitle + "#合成试验曲|合成試験曲]]\n"
	provider, requests := newMoegirlSearchTestProvider(t, indexBody, map[string]moegirlSearchTestPage{
		pageTitle: {pageID: 2, revisionID: 22, body: moegirlMatchingTestSection("合成试验曲", "歌う")},
	})
	identity := MusicIdentity{
		MusicID: 10, JapaneseTitle: "合成試験曲", ProducerMetadata: "制作者",
		Lyricist: "制作者", Composer: "制作者", Arranger: "制作者",
		PerformerSegmentationPolicy: PerformerSegmentationSekaiEligible,
	}

	first, err := provider.Search(context.Background(), identity)
	if err != nil || len(first) != 1 || len(first[0].IndexEvidence) != 1 {
		t.Fatalf("first candidates=%+v err=%v", first, err)
	}
	original := first[0]
	original.IndexEvidenceRefs = cloneIndexEvidenceRefs(first[0].IndexEvidenceRefs)
	original.IndexEvidence = cloneIndexEvidence(first[0].IndexEvidence)
	firstEvidence := original.IndexEvidence[0]
	first[0].IndexEvidence[0].Raw[0] ^= 0xff
	first[0].IndexEvidence[0].Categories[0] = "mutated"

	provider.client.mu.Lock()
	for key, cached := range provider.client.cache {
		cached.createdAt = time.Now().Add(-2 * provider.client.cacheTTL)
		provider.client.cache[key] = cached
	}
	provider.client.mu.Unlock()

	second, err := provider.Search(context.Background(), identity)
	if err != nil || len(second) != 1 || len(second[0].IndexEvidence) != 1 {
		t.Fatalf("second candidates=%+v err=%v", second, err)
	}
	secondEvidence := second[0].IndexEvidence[0]
	if !indexEvidenceEqual(firstEvidence, secondEvidence) {
		t.Fatalf("provider-lifetime fixed acquisition changed after client cache expiry:\nfirst=%+v\nsecond=%+v",
			firstEvidence, secondEvidence)
	}
	if got := requests["固定索引"].Load(); got != 1 {
		t.Fatalf("fixed-index requests after client cache expiry = %d, want 1", got)
	}
	if got := requests[pageTitle].Load(); got != 2 {
		t.Fatalf("dynamic page requests after client cache expiry = %d, want 2", got)
	}

	independent := newMoegirlProvider(
		provider.config,
		newMediaWikiClient(provider.client.endpoint, 0, time.Hour, provider.client.httpClient),
	)
	third, err := independent.Search(context.Background(), identity)
	if err != nil || len(third) != 1 || len(third[0].IndexEvidence) != 1 {
		t.Fatalf("independent candidates=%+v err=%v", third, err)
	}
	thirdEvidence := third[0].IndexEvidence[0]
	if thirdEvidence.EvidenceID == firstEvidence.EvidenceID || thirdEvidence.FetchedAt == firstEvidence.FetchedAt ||
		thirdEvidence.RawSHA256 != firstEvidence.RawSHA256 || !bytes.Equal(thirdEvidence.Raw, firstEvidence.Raw) {
		t.Fatalf("independent exact acquisition did not receive a distinct identity:\nfirst=%+v\nthird=%+v",
			firstEvidence, thirdEvidence)
	}
	if got := requests["固定索引"].Load(); got != 2 {
		t.Fatalf("fixed-index requests across independent providers = %d, want 2", got)
	}
	if err := ValidateCandidatesIndexEvidence([]Candidate{original, second[0], third[0]}); err != nil {
		t.Fatalf("reused and independent exact acquisitions conflicted: %v", err)
	}
}

func TestMoegirlFixedAuthorityPreservesKnownEmptyCategoryIdentity(t *testing.T) {
	const indexBody = "* [[対象頁#合成试验曲|合成試験曲]]\n"
	const targetTitle = "対象頁"
	targetBody := moegirlMatchingTestSection("合成试验曲", "歌う")
	fixed := FixedIndex{
		PageID: 1, RevisionID: 11, SHA1: fmt.Sprintf("%x", sha1.Sum([]byte(indexBody))), Title: "固定索引",
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.URL.Query().Get("revids") == strconv.Itoa(fixed.RevisionID):
			writePageResponseWithCategories(w, fixed.PageID, fixed.RevisionID, fixed.SHA1, fixed.Title, indexBody, []string{})
		case r.URL.Query().Get("titles") == targetTitle, r.URL.Query().Get("revids") == "22":
			writePageResponseWithCategories(w, 2, 22, fmt.Sprintf("%x", sha1.Sum([]byte(targetBody))), targetTitle, targetBody, []string{})
		default:
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	provider := newMoegirlProvider(
		ProviderConfig{Provider: ProviderMoegirl, Origin: OriginMoegirl, Indexes: []FixedIndex{fixed}},
		newMediaWikiClient(server.URL, 0, time.Hour, server.Client()),
	)
	identity := MusicIdentity{
		MusicID: 10, JapaneseTitle: "合成試験曲", ProducerMetadata: "制作者",
		Lyricist: "制作者", Composer: "制作者", Arranger: "制作者",
		PerformerSegmentationPolicy: PerformerSegmentationSekaiEligible,
	}

	var candidate Candidate
	for attempt := range 2 {
		candidates, err := provider.Search(context.Background(), identity)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("search %d candidates=%+v err=%v", attempt+1, candidates, err)
		}
		candidate = candidates[0]
		if candidate.Categories == nil || len(candidate.Categories) != 0 ||
			len(candidate.IndexEvidence) != 1 || candidate.IndexEvidence[0].Categories == nil ||
			len(candidate.IndexEvidence[0].Categories) != 0 || ValidateCandidateIndexEvidence(candidate) != nil {
			t.Fatalf("search %d lost known-empty category identity: %+v", attempt+1, candidate)
		}
	}

	fixedRevision, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if fixedRevision.Categories == nil || len(fixedRevision.Categories) != 0 ||
		len(fixedRevision.IndexEvidence) != 1 || fixedRevision.IndexEvidence[0].Categories == nil ||
		len(fixedRevision.IndexEvidence[0].Categories) != 0 || len(fixedRevision.FixedIdentities) == 0 {
		t.Fatalf("fixed revision lost known-empty category identity: %+v", fixedRevision)
	}
	for index, identity := range fixedRevision.FixedIdentities {
		if identity.Categories == nil || len(identity.Categories) != 0 {
			t.Fatalf("fixed identity %d categories = %#v", index, identity.Categories)
		}
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("known-empty category requests = %d, want fixed index, cached title, and exact revision", got)
	}
}

func TestFixedAuthorityDefensiveClonePreservesCategoryPresence(t *testing.T) {
	acquisition := fixedAuthorityAcquisition{
		page: wikiPage{
			categories: []string{},
			indexEvidence: []IndexEvidence{
				{Categories: nil},
				{Categories: []string{}},
			},
		},
		evidence: IndexEvidence{Categories: []string{}},
	}
	cloned := cloneFixedAuthorityAcquisition(acquisition)
	if cloned.page.categories == nil || cloned.evidence.Categories == nil ||
		cloned.page.indexEvidence[0].Categories != nil || cloned.page.indexEvidence[1].Categories == nil {
		t.Fatalf("category presence changed during defensive clone: page=%#v evidence=%#v nested=%#v",
			cloned.page.categories, cloned.evidence.Categories, cloned.page.indexEvidence)
	}

	nilCategories := cloneFixedAuthorityAcquisition(fixedAuthorityAcquisition{})
	if nilCategories.page.categories != nil || nilCategories.evidence.Categories != nil {
		t.Fatalf("absent categories became present: page=%#v evidence=%#v",
			nilCategories.page.categories, nilCategories.evidence.Categories)
	}
}

func TestFixedAuthoritySuccessfulCompletionRequiresActiveWaiterAdmission(t *testing.T) {
	fixed := FixedIndex{PageID: 1, RevisionID: 11, SHA1: strings.Repeat("a", 40), Title: "fixed"}
	acquisition := fixedAuthorityAcquisition{page: wikiPage{pageID: 1, categories: []string{}}}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := &fixedAuthorityResolution{done: make(chan struct{}), cancel: cancel, participants: 1}
	cache := fixedAuthorityCache{inflight: map[FixedIndex]*fixedAuthorityResolution{fixed: pending}}

	cache.finish(fixed, pending, acquisition, nil)
	cache.mu.Lock()
	_, cachedBeforeAdmission := cache.values[fixed]
	stillInflight := cache.inflight[fixed] == pending
	completed := pending.completed
	cache.mu.Unlock()
	if cachedBeforeAdmission || !stillInflight || !completed {
		t.Fatalf("finish admitted or discarded successful completion: cached=%t inflight=%t completed=%t",
			cachedBeforeAdmission, stillInflight, completed)
	}
	accepted, err := cache.await(context.Background(), fixed, pending)
	if err != nil || accepted.page.pageID != acquisition.page.pageID || accepted.page.categories == nil {
		t.Fatalf("active waiter acceptance=%+v err=%v", accepted, err)
	}
	cache.mu.Lock()
	cached, found := cache.values[fixed]
	remaining := cache.inflight[fixed]
	cache.mu.Unlock()
	if !found || cached.page.categories == nil || remaining != nil {
		t.Fatalf("active waiter did not admit completion: cached=%+v found=%t inflight=%p", cached, found, remaining)
	}
}

func TestFixedAuthoritySoleCanceledWaiterCannotAdmitCompletedSuccess(t *testing.T) {
	fixed := FixedIndex{PageID: 1, RevisionID: 11, SHA1: strings.Repeat("a", 40), Title: "fixed"}
	first := fixedAuthorityAcquisition{page: wikiPage{pageID: 1, categories: []string{}}}
	_, workCancel := context.WithCancel(context.Background())
	defer workCancel()
	pending := &fixedAuthorityResolution{done: make(chan struct{}), cancel: workCancel, participants: 1}
	cache := fixedAuthorityCache{inflight: map[FixedIndex]*fixedAuthorityResolution{fixed: pending}}

	cache.finish(fixed, pending, first, nil)
	cache.mu.Lock()
	_, cachedByFinish := cache.values[fixed]
	cache.mu.Unlock()
	if cachedByFinish {
		t.Fatal("finish cached a success before any caller accepted it")
	}

	callerCtx, callerCancel := context.WithCancel(context.Background())
	callerCancel()
	if _, err := cache.await(callerCtx, fixed, pending); !errors.Is(err, context.Canceled) {
		t.Fatalf("sole canceled waiter error = %v", err)
	}
	cache.mu.Lock()
	_, cachedAfterCancellation := cache.values[fixed]
	remaining := cache.inflight[fixed]
	abandoned := pending.abandoned
	cache.mu.Unlock()
	if cachedAfterCancellation || remaining != nil || !abandoned {
		t.Fatalf("canceled waiter admitted completion: cached=%t inflight=%p abandoned=%t",
			cachedAfterCancellation, remaining, abandoned)
	}

	var retries atomic.Int32
	second, err := cache.resolve(context.Background(), fixed, func(context.Context) (fixedAuthorityAcquisition, error) {
		retries.Add(1)
		return fixedAuthorityAcquisition{page: wikiPage{pageID: 2, categories: []string{}}}, nil
	})
	if err != nil || second.page.pageID != 2 || retries.Load() != 1 {
		t.Fatalf("healthy retry acquisition=%+v retries=%d err=%v", second, retries.Load(), err)
	}
}

func TestFixedAuthoritySoleCallerCancellationDuringAdmissionCloneDoesNotCache(t *testing.T) {
	fixed := FixedIndex{PageID: 1, RevisionID: 11, SHA1: strings.Repeat("a", 40), Title: "fixed"}
	admissionStarted := make(chan struct{})
	releaseAdmission := make(chan struct{})
	var cloneCalls atomic.Int32
	var attempts atomic.Int32
	cache := fixedAuthorityCache{
		admissionClone: func(acquisition fixedAuthorityAcquisition) fixedAuthorityAcquisition {
			if cloneCalls.Add(1) == 1 {
				close(admissionStarted)
				<-releaseAdmission
			}
			return cloneFixedAuthorityAcquisition(acquisition)
		},
	}
	resolver := func(context.Context) (fixedAuthorityAcquisition, error) {
		pageID := int(attempts.Add(1))
		return fixedAuthorityAcquisition{page: wikiPage{pageID: pageID, categories: []string{}}}, nil
	}

	callerCtx, callerCancel := context.WithCancel(context.Background())
	result := make(chan struct {
		acquisition fixedAuthorityAcquisition
		err         error
	}, 1)
	go func() {
		acquisition, err := cache.resolve(callerCtx, fixed, resolver)
		result <- struct {
			acquisition fixedAuthorityAcquisition
			err         error
		}{acquisition: acquisition, err: err}
	}()

	select {
	case <-admissionStarted:
	case <-time.After(time.Second):
		t.Fatal("fixed-authority admission clone did not start")
	}
	cache.mu.Lock()
	pending := cache.inflight[fixed]
	cache.mu.Unlock()
	if pending == nil {
		t.Fatal("admission-phase resolution was not retained as inflight")
	}
	callerCancel()
	close(releaseAdmission)

	select {
	case completed := <-result:
		if !errors.Is(completed.err, context.Canceled) || completed.acquisition.page.pageID != 0 || completed.acquisition.evidence.EvidenceID != "" {
			t.Fatalf("admission-phase cancellation acquisition=%+v err=%v", completed.acquisition, completed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("admission-phase cancellation did not return")
	}
	cache.mu.Lock()
	_, cached := cache.values[fixed]
	remaining := cache.inflight[fixed]
	abandoned := pending.abandoned
	cache.mu.Unlock()
	if cached || remaining != nil || !abandoned {
		t.Fatalf("admission-phase cancellation cached=%t inflight=%p abandoned=%t", cached, remaining, abandoned)
	}

	healthy, err := cache.resolve(context.Background(), fixed, resolver)
	if err != nil || healthy.page.pageID != 2 || attempts.Load() != 2 {
		t.Fatalf("healthy retry after admission cancellation=%+v attempts=%d err=%v", healthy, attempts.Load(), err)
	}
	cachedHealthy, err := cache.resolve(context.Background(), fixed, resolver)
	if err != nil || cachedHealthy.page.pageID != 2 || attempts.Load() != 2 {
		t.Fatalf("healthy retry was not cached=%+v attempts=%d err=%v", cachedHealthy, attempts.Load(), err)
	}
}

func TestFixedAuthorityHealthyCallerDoesNotJoinAbandonedResolution(t *testing.T) {
	fixed := FixedIndex{PageID: 1, RevisionID: 11, SHA1: strings.Repeat("a", 40), Title: "fixed"}
	var cache fixedAuthorityCache
	var attempts atomic.Int32
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })

	resolver := func(workCtx context.Context) (fixedAuthorityAcquisition, error) {
		switch attempts.Add(1) {
		case 1:
			close(firstStarted)
			<-workCtx.Done()
			close(firstCanceled)
			<-releaseFirst
			return fixedAuthorityAcquisition{page: wikiPage{pageID: 1, categories: []string{}}}, nil
		case 2:
			close(secondStarted)
			return fixedAuthorityAcquisition{page: wikiPage{pageID: 2, categories: []string{}}}, nil
		default:
			return fixedAuthorityAcquisition{}, errors.New("unexpected fixed-authority resolution")
		}
	}

	callerCtx, callerCancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := cache.resolve(callerCtx, fixed, resolver)
		firstResult <- err
	}()
	<-firstStarted
	cache.mu.Lock()
	firstPending := cache.inflight[fixed]
	cache.mu.Unlock()
	if firstPending == nil {
		t.Fatal("first fixed-authority resolution was not registered")
	}
	callerCancel()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first canceled caller error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first canceled caller did not return")
	}
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("abandoned resolution was not canceled")
	}

	secondResult := make(chan struct {
		acquisition fixedAuthorityAcquisition
		err         error
	}, 1)
	go func() {
		acquisition, err := cache.resolve(context.Background(), fixed, resolver)
		secondResult <- struct {
			acquisition fixedAuthorityAcquisition
			err         error
		}{acquisition: acquisition, err: err}
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("healthy caller joined the abandoned resolution")
	}
	second := <-secondResult
	if second.err != nil || second.acquisition.page.pageID != 2 || attempts.Load() != 2 {
		t.Fatalf("healthy acquisition=%+v attempts=%d err=%v", second.acquisition, attempts.Load(), second.err)
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	select {
	case <-firstPending.done:
	case <-time.After(time.Second):
		t.Fatal("abandoned resolution did not finish")
	}
	cached, err := cache.resolve(context.Background(), fixed, resolver)
	if err != nil || cached.page.pageID != 2 || attempts.Load() != 2 {
		t.Fatalf("abandoned completion replaced healthy cache: acquisition=%+v attempts=%d err=%v",
			cached, attempts.Load(), err)
	}
}

func TestMoegirlConcurrentFirstFixedIndexAccessResolvesOnce(t *testing.T) {
	const indexBody = "* [[対象頁#合成试验曲|合成試験曲]]\n"
	const indexTitle = "固定索引"
	fixed := FixedIndex{
		PageID: 1, RevisionID: 11, SHA1: fmt.Sprintf("%x", sha1.Sum([]byte(indexBody))), Title: indexTitle,
	}
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("revids") != strconv.Itoa(fixed.RevisionID) {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		startedOnce.Do(func() { close(started) })
		<-release
		writePageResponseWithCategories(w, fixed.PageID, fixed.RevisionID, fixed.SHA1, fixed.Title, indexBody, []string{"索引"})
	}))
	defer server.Close()
	provider := newMoegirlProvider(
		ProviderConfig{Provider: ProviderMoegirl, Origin: OriginMoegirl, Indexes: []FixedIndex{fixed}},
		newMediaWikiClient(server.URL, 0, time.Hour, server.Client()),
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
		t.Fatalf("concurrent fixed-index requests = %d, want 1", got)
	}
	expected := cloneFixedAuthorityAcquisition(acquisitions[0])
	for index, acquisition := range acquisitions[1:] {
		if !indexEvidenceEqual(acquisition.evidence, expected.evidence) ||
			!bytes.Equal(acquisition.page.rawResponse, expected.page.rawResponse) ||
			acquisition.page.fetchedAt != expected.page.fetchedAt {
			t.Fatalf("concurrent acquisition %d changed: %+v", index+1, acquisition)
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
		t.Fatalf("defensive fixed-index clone = %+v err=%v", cached, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cached fixed-index requests = %d, want 1", got)
	}
}

func TestMoegirlFixedIndexFailuresAreNotCached(t *testing.T) {
	const indexBody = "* [[対象頁#合成试验曲|合成試験曲]]\n"
	const indexTitle = "固定索引"
	fixed := FixedIndex{
		PageID: 1, RevisionID: 11, SHA1: fmt.Sprintf("%x", sha1.Sum([]byte(indexBody))), Title: indexTitle,
	}
	for _, test := range []struct {
		name           string
		wantErr        error
		wantHTTPStatus int
		first          func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "transport error", wantHTTPStatus: http.StatusServiceUnavailable,
			first: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
		},
		{
			name: "malformed response", wantErr: ErrMalformedResponse,
			first: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("{"))
			},
		},
		{
			name: "revision and SHA drift", wantErr: ErrRevisionChanged,
			first: func(w http.ResponseWriter, _ *http.Request) {
				drifted := indexBody + "drift"
				writePageResponseWithCategories(w, fixed.PageID, fixed.RevisionID,
					fmt.Sprintf("%x", sha1.Sum([]byte(drifted))), fixed.Title, drifted, []string{"索引"})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("revids") != strconv.Itoa(fixed.RevisionID) {
					http.Error(w, "unexpected query", http.StatusBadRequest)
					return
				}
				if attempts.Add(1) == 1 {
					test.first(w, r)
					return
				}
				writePageResponseWithCategories(w, fixed.PageID, fixed.RevisionID, fixed.SHA1, fixed.Title, indexBody, []string{"索引"})
			}))
			defer server.Close()
			provider := newMoegirlProvider(
				ProviderConfig{Provider: ProviderMoegirl, Origin: OriginMoegirl, Indexes: []FixedIndex{fixed}},
				newMediaWikiClient(server.URL, 0, time.Hour, server.Client()),
			)

			_, firstErr := provider.acquireFixedIndex(context.Background(), fixed)
			if test.wantHTTPStatus != 0 {
				var httpErr *HTTPError
				if !errors.As(firstErr, &httpErr) || httpErr.StatusCode != test.wantHTTPStatus {
					t.Fatalf("first fixed-index HTTP error = %v, want status %d", firstErr, test.wantHTTPStatus)
				}
			} else if !errors.Is(firstErr, test.wantErr) {
				t.Fatalf("first fixed-index error = %v, want %v", firstErr, test.wantErr)
			}
			acquisition, err := provider.acquireFixedIndex(context.Background(), fixed)
			if err != nil || acquisition.evidence.EvidenceID == "" {
				t.Fatalf("retry acquisition=%+v err=%v", acquisition, err)
			}
			if _, err := provider.acquireFixedIndex(context.Background(), fixed); err != nil {
				t.Fatal(err)
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("fixed-index attempts = %d, want failed attempt plus one successful retry", got)
			}
		})
	}
}

func TestMoegirlCanceledFixedIndexAcquisitionIsNotCached(t *testing.T) {
	const indexBody = "* [[対象頁#合成试验曲|合成試験曲]]\n"
	fixed := FixedIndex{
		PageID: 1, RevisionID: 11, SHA1: fmt.Sprintf("%x", sha1.Sum([]byte(indexBody))), Title: "固定索引",
	}
	var attempts atomic.Int32
	started := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			startedOnce.Do(func() { close(started) })
			<-r.Context().Done()
			return
		}
		writePageResponseWithCategories(w, fixed.PageID, fixed.RevisionID, fixed.SHA1, fixed.Title, indexBody, []string{"索引"})
	}))
	defer server.Close()
	provider := newMoegirlProvider(
		ProviderConfig{Provider: ProviderMoegirl, Origin: OriginMoegirl, Indexes: []FixedIndex{fixed}},
		newMediaWikiClient(server.URL, 0, time.Hour, server.Client()),
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
		t.Fatalf("canceled fixed-index error = %v", err)
	}
	waitForFixedAuthorityIdle(t, &provider.fixedAuthorities, fixed)
	if _, err := provider.acquireFixedIndex(context.Background(), fixed); err != nil {
		t.Fatalf("fixed-index retry after cancellation: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("fixed-index attempts after cancellation = %d, want 2", got)
	}
}
