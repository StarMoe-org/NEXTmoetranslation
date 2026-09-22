package lyricssource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/httpx"
	"moesekai/server/internal/model"
)

const (
	ProviderVocaloidFandom     = model.LyricsSourceProviderVocaloidFandom
	ProviderMoegirl            = model.LyricsSourceProviderMoegirl
	ProviderMoegirlPublicExact = model.LyricsSourceProviderMoegirlPublicExact
	ProviderSekaipedia         = model.LyricsSourceProviderSekaipedia

	OriginVocaloidFandom     = model.LyricsSourceOriginVocaloidFandom
	OriginMoegirl            = model.LyricsSourceOriginMoegirl
	OriginMoegirlPublicExact = model.LyricsSourceOriginMoegirlPublicExact
	OriginSekaipedia         = model.LyricsSourceOriginSekaipedia

	moegirlAPI                       = "https://moegirl.icu/api.php"
	sekaipediaAPI                    = "https://www.sekaipedia.org/w/api.php"
	sekaipediaRightsText             = "Creative Commons Attribution-ShareAlike 4.0 International (CC BY-SA 4.0)"
	defaultProviderCrawlDelay        = 10 * time.Second
	defaultProviderCacheTTL          = 10 * time.Second
	maxProviderContributorAliases    = 64
	maxProviderContributorAliasBytes = 256
)

// FixedIndex identifies an immutable MediaWiki index revision used to discover
// provider-owned target pages. The index body is always fetched by revid and
// re-hashed before any link is trusted.
type FixedIndex struct {
	PageID            int
	RevisionID        int
	RevisionTimestamp string
	SHA1              string
	ContentSHA256     string
	RawSHA256         string
	Title             string
}

// ProviderContributorAlias is one reviewed, song-scoped catalog contributor
// identity mapped to the identity used by a provider. Alias plans are optional
// configuration; no provider receives implicit catalog exceptions.
type ProviderContributorAlias struct {
	MusicID             int
	CatalogContributor  string
	ProviderContributor string
}

// SekaipediaPageTarget binds one catalog music ID to an exact reviewed
// romanized MediaWiki page title. Ordinary discovery still requires the title
// in the immutable Sekaipedia List; exact recovery may bind a later page that
// is absent from that historical List. It is plan data, not a transliteration
// rule.
type SekaipediaPageTarget struct {
	MusicID           int
	PageTitle         string
	ResolvedPageTitle string
	FixedRevision     *FixedIndex
}

type ExactPublicFileBinding struct {
	Path      string
	SizeBytes int64
	SHA256    string
}

type ExactPublicPageTarget struct {
	MusicID          int
	PageURL          string
	PageTitle        string
	JapaneseTitle    string
	PageID           int
	RevisionID       int
	FetchedAt        string
	RawHTML          ExactPublicFileBinding
	ExtractionReport ExactPublicFileBinding
}

type fixedAuthorityAcquisition struct {
	page     wikiPage
	evidence IndexEvidence
}

type fixedAuthorityResolution struct {
	done         chan struct{}
	cancel       context.CancelFunc
	participants int
	completed    bool
	abandoned    bool
	acquisition  fixedAuthorityAcquisition
	err          error
}

// fixedAuthorityCache retains only fully validated configured immutable
// acquisitions. It is intentionally separate from the TTL response cache so a
// provider keeps the original acquisition identity for its entire lifetime.
type fixedAuthorityCache struct {
	mu             sync.Mutex
	values         map[FixedIndex]fixedAuthorityAcquisition
	inflight       map[FixedIndex]*fixedAuthorityResolution
	admissionClone func(fixedAuthorityAcquisition) fixedAuthorityAcquisition
}

func (cache *fixedAuthorityCache) resolve(
	ctx context.Context,
	fixed FixedIndex,
	resolver func(context.Context) (fixedAuthorityAcquisition, error),
) (fixedAuthorityAcquisition, error) {
	if ctx == nil || resolver == nil {
		return fixedAuthorityAcquisition{}, ErrMalformedResponse
	}
	if err := ctx.Err(); err != nil {
		return fixedAuthorityAcquisition{}, err
	}

	cache.mu.Lock()
	if cached, found := cache.values[fixed]; found {
		cache.mu.Unlock()
		return cloneFixedAuthorityAcquisition(cached), nil
	}
	if cache.inflight == nil {
		cache.inflight = make(map[FixedIndex]*fixedAuthorityResolution)
	}
	if pending := cache.inflight[fixed]; pending != nil {
		if !pending.abandoned && (!pending.completed || pending.err == nil) {
			pending.participants++
			cache.mu.Unlock()
			return cache.await(ctx, fixed, pending)
		}
		delete(cache.inflight, fixed)
	}
	workCtx, cancel := context.WithCancel(context.Background())
	pending := &fixedAuthorityResolution{
		done: make(chan struct{}), cancel: cancel, participants: 1,
	}
	cache.inflight[fixed] = pending
	cache.mu.Unlock()

	go func() {
		acquisition, err := resolver(workCtx)
		if contextErr := workCtx.Err(); contextErr != nil {
			acquisition = fixedAuthorityAcquisition{}
			err = contextErr
		}
		cache.finish(fixed, pending, acquisition, err)
	}()
	return cache.await(ctx, fixed, pending)
}

func (cache *fixedAuthorityCache) await(
	ctx context.Context,
	fixed FixedIndex,
	pending *fixedAuthorityResolution,
) (fixedAuthorityAcquisition, error) {
	if err := ctx.Err(); err != nil {
		cache.mu.Lock()
		cache.releaseParticipantLocked(fixed, pending, true)
		cache.mu.Unlock()
		return fixedAuthorityAcquisition{}, err
	}

	// Stopping this callback is the admission claim. A true result proves that
	// cancellation had not begun and prevents it from winning later; a false
	// result means the caller can no longer admit a successful acquisition.
	claimActiveCaller := context.AfterFunc(ctx, func() {})
	defer claimActiveCaller()

	select {
	case <-ctx.Done():
		err := ctx.Err()
		cache.mu.Lock()
		cache.releaseParticipantLocked(fixed, pending, true)
		cache.mu.Unlock()
		return fixedAuthorityAcquisition{}, err
	case <-pending.done:
		cache.mu.Lock()
		if pending.participants <= 0 || !pending.completed {
			cache.mu.Unlock()
			return fixedAuthorityAcquisition{}, ErrMalformedResponse
		}
		if pending.err != nil {
			err := pending.err
			cache.releaseParticipantLocked(fixed, pending, false)
			cache.mu.Unlock()
			return fixedAuthorityAcquisition{}, err
		}
		if cached, found := cache.values[fixed]; found {
			if !claimActiveCaller() {
				err := ctx.Err()
				cache.releaseParticipantLocked(fixed, pending, true)
				cache.mu.Unlock()
				return fixedAuthorityAcquisition{}, err
			}
			if cache.inflight[fixed] == pending {
				delete(cache.inflight, fixed)
			}
			cache.releaseParticipantLocked(fixed, pending, false)
			cache.mu.Unlock()
			return cloneFixedAuthorityAcquisition(cached), nil
		}
		if err := ctx.Err(); err != nil {
			cache.releaseParticipantLocked(fixed, pending, true)
			cache.mu.Unlock()
			return fixedAuthorityAcquisition{}, err
		}
		acquisition := pending.acquisition
		cache.mu.Unlock()

		// Raw provider responses can make this clone large. Prepare it outside the
		// cache lock, then atomically claim the still-active caller before admission.
		admitted := cache.cloneAdmissionAcquisition(acquisition)

		cache.mu.Lock()
		if pending.participants <= 0 || !pending.completed || pending.err != nil {
			cache.mu.Unlock()
			return fixedAuthorityAcquisition{}, ErrMalformedResponse
		}
		if !claimActiveCaller() {
			err := ctx.Err()
			cache.releaseParticipantLocked(fixed, pending, true)
			cache.mu.Unlock()
			return fixedAuthorityAcquisition{}, err
		}
		if cached, found := cache.values[fixed]; found {
			acquisition = cached
		} else {
			if cache.values == nil {
				cache.values = make(map[FixedIndex]fixedAuthorityAcquisition)
			}
			cache.values[fixed] = admitted
		}
		if cache.inflight[fixed] == pending {
			delete(cache.inflight, fixed)
		}
		cache.releaseParticipantLocked(fixed, pending, false)
		cache.mu.Unlock()
		return cloneFixedAuthorityAcquisition(acquisition), nil
	}
}

func (cache *fixedAuthorityCache) cloneAdmissionAcquisition(acquisition fixedAuthorityAcquisition) fixedAuthorityAcquisition {
	if cache.admissionClone != nil {
		return cache.admissionClone(acquisition)
	}
	return cloneFixedAuthorityAcquisition(acquisition)
}

// releaseParticipantLocked transitions a canceled resolution to abandoned only
// when its final active caller leaves. Removing it from inflight before
// cancellation makes the transition atomic with respect to a new resolver.
func (cache *fixedAuthorityCache) releaseParticipantLocked(
	fixed FixedIndex,
	pending *fixedAuthorityResolution,
	canceled bool,
) {
	if pending.participants <= 0 {
		return
	}
	pending.participants--
	if pending.participants != 0 {
		return
	}
	if canceled {
		pending.abandoned = true
		if cache.inflight[fixed] == pending {
			delete(cache.inflight, fixed)
		}
	}
	pending.cancel()
}

// finish publishes completion to the current participants but never admits a
// successful acquisition to the provider-lifetime cache. A waiter must still
// have an active caller context and accept the result before it becomes cached.
func (cache *fixedAuthorityCache) finish(
	fixed FixedIndex,
	pending *fixedAuthorityResolution,
	acquisition fixedAuthorityAcquisition,
	err error,
) {
	if err == nil {
		acquisition = cloneFixedAuthorityAcquisition(acquisition)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if pending.completed {
		return
	}
	pending.acquisition = acquisition
	pending.err = err
	pending.completed = true
	if err != nil || pending.abandoned || pending.participants == 0 {
		if cache.inflight[fixed] == pending {
			delete(cache.inflight, fixed)
		}
	}
	close(pending.done)
	pending.cancel()
}

func cloneFixedAuthorityAcquisition(acquisition fixedAuthorityAcquisition) fixedAuthorityAcquisition {
	cloned := acquisition
	cloned.page.categories = cloneIdentityCategories(acquisition.page.categories)
	cloned.page.rawResponse = append([]byte(nil), acquisition.page.rawResponse...)
	cloned.page.indexEvidenceRefs = cloneIndexEvidenceRefs(acquisition.page.indexEvidenceRefs)
	cloned.page.indexEvidence = cloneStrictIndexEvidence(acquisition.page.indexEvidence)
	cloned.evidence.Categories = cloneIdentityCategories(acquisition.evidence.Categories)
	cloned.evidence.Raw = append([]byte(nil), acquisition.evidence.Raw...)
	return cloned
}

// cloneIdentityCategories preserves the distinction between an absent category
// identity and a known, present-but-empty category identity.
func cloneIdentityCategories(categories []string) []string {
	if categories == nil {
		return nil
	}
	cloned := make([]string, len(categories))
	copy(cloned, categories)
	return cloned
}

func cloneStrictIndexEvidence(input []IndexEvidence) []IndexEvidence {
	if input == nil {
		return nil
	}
	result := make([]IndexEvidence, len(input))
	for index, evidence := range input {
		result[index] = evidence
		result[index].Categories = cloneIdentityCategories(evidence.Categories)
		result[index].Raw = append([]byte(nil), evidence.Raw...)
	}
	return result
}

func hasConfiguredFixedIndex(config ProviderConfig, fixed FixedIndex) bool {
	for _, configured := range config.Indexes {
		if configured == fixed {
			return true
		}
	}
	return false
}

// ProviderConfig owns one provider's endpoint, fixed indexes, reviewed alias
// plan, rate limiter, and cache. Providers never share endpoint or cache state;
// a registry only aggregates their typed results.
type ProviderConfig struct {
	Provider             model.LyricsSourceProvider
	Enabled              bool
	Origin               string
	APIEndpoint          string
	Indexes              []FixedIndex
	ContributorAliases   []ProviderContributorAlias
	SekaipediaTargets    []SekaipediaPageTarget
	ExactPublicTargets   []ExactPublicPageTarget
	RightsText           string
	CrawlDelay           time.Duration
	CacheTTL             time.Duration
	RecoveryExactCapture bool
	RecoveryRevision     *FixedIndex
}

// DefaultProviderConfigs returns the two fallback providers retained for
// compatibility with callers that explicitly construct the legacy registry.
// Sekaipedia has no compiled mutable authority: callers must configure it from
// an immutable reviewed plan.
func DefaultProviderConfigs() []ProviderConfig {
	return []ProviderConfig{
		{
			Provider: ProviderVocaloidFandom, Enabled: true,
			Origin: OriginVocaloidFandom, APIEndpoint: vocaloidWikiAPI,
			CrawlDelay: defaultProviderCrawlDelay, CacheTTL: defaultProviderCacheTTL,
		},
		{
			Provider: ProviderMoegirl, Enabled: true,
			Origin: OriginMoegirl, APIEndpoint: moegirlAPI,
			Indexes: []FixedIndex{{
				PageID: 488279, RevisionID: 8073049, SHA1: "d15e3eae65f3516d9b93b7644315574648379a3b",
				Title: "世界计划 彩色舞台 feat. 初音未来/歌曲",
			}},
			CrawlDelay: defaultProviderCrawlDelay, CacheTTL: defaultProviderCacheTTL,
		},
	}
}

// ReviewedSekaipediaProviderConfig returns the currently reviewed Sekaipedia
// authority: the fixed "List of songs" revision plus optional exact, reviewed
// music-ID-to-page-title and contributor-alias maps. The target map lets
// ordinary discovery resolve a catalog song to its romanized Sekaipedia page;
// the alias map lets the romanized provider credits match the Japanese catalog
// credits. Songs absent from the maps fall through to the legacy fallback
// providers. Empty maps are valid.
func ReviewedSekaipediaProviderConfig(targets []SekaipediaPageTarget, aliases []ProviderContributorAlias) ProviderConfig {
	return ProviderConfig{
		Provider: ProviderSekaipedia, Enabled: true,
		Origin: OriginSekaipedia, APIEndpoint: sekaipediaAPI,
		RightsText: sekaipediaRightsText,
		Indexes: []FixedIndex{{
			PageID:            268,
			RevisionID:        340860,
			RevisionTimestamp: "2026-08-14T16:09:47Z",
			SHA1:              "1c6a0edcf6b63222f5f947e5bd30147ebe8e4a4b",
			ContentSHA256:     "f8ea893f3bb9d5928f87c664ceeb74d187fe19b0820edc2b3fa604093e0e27d7",
			RawSHA256:         "b381f24fa9d584d1aa58ab9a33030e7a557293beb77e145823505ab14a86cc88",
			Title:             "List of songs",
		}},
		SekaipediaTargets:  cloneSekaipediaPageTargets(targets),
		ContributorAliases: cloneProviderContributorAliases(aliases),
		CrawlDelay:         defaultProviderCrawlDelay, CacheTTL: defaultProviderCacheTTL,
	}
}

func (config ProviderConfig) validate() error {
	if !model.IsValidLyricsSourceProvider(config.Provider) {
		return fmt.Errorf("unsupported lyrics source provider %q", config.Provider)
	}
	wantOrigin := OriginVocaloidFandom
	wantEndpointPath := "/api.php"
	exactPublic := false
	switch config.Provider {
	case ProviderMoegirl:
		wantOrigin = OriginMoegirl
	case ProviderMoegirlPublicExact:
		wantOrigin = OriginMoegirlPublicExact
		exactPublic = true
	case ProviderSekaipedia:
		wantOrigin = OriginSekaipedia
		wantEndpointPath = "/w/api.php"
	}
	if config.Origin != wantOrigin {
		return fmt.Errorf("provider %q requires origin %q", config.Provider, wantOrigin)
	}
	if exactPublic {
		if config.APIEndpoint != "" {
			return errors.New("moegirl_public_exact does not accept an ICU or MediaWiki API endpoint")
		}
	} else {
		endpoint, err := url.Parse(config.APIEndpoint)
		if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Fragment != "" ||
			endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Path != wantEndpointPath ||
			endpoint.Scheme+"://"+endpoint.Host != config.Origin {
			return fmt.Errorf("provider %q requires its canonical HTTPS %s endpoint", config.Provider, wantEndpointPath)
		}
	}
	if config.CrawlDelay < defaultProviderCrawlDelay {
		return fmt.Errorf("provider %q crawl delay must be at least %s", config.Provider, defaultProviderCrawlDelay)
	}
	if config.CacheTTL < defaultProviderCacheTTL {
		return fmt.Errorf("provider %q cache TTL must be at least %s", config.Provider, defaultProviderCacheTTL)
	}
	if err := validateProviderContributorAliases(config.Provider, config.ContributorAliases); err != nil {
		return err
	}
	if err := validateSekaipediaPageTargets(config.Provider, config.SekaipediaTargets); err != nil {
		return err
	}
	if err := validateExactPublicPageTargets(config.Provider, config.ExactPublicTargets); err != nil {
		return err
	}
	if config.Provider == ProviderVocaloidFandom {
		if len(config.Indexes) != 0 || config.RightsText != "" || config.RecoveryRevision != nil {
			return errors.New("vocaloid_fandom does not accept fixed indexes, recovery revisions, or provider rights metadata")
		}
	}
	if config.Provider == ProviderMoegirl {
		if config.RightsText != "" || len(config.Indexes) == 0 || len(config.Indexes) > 16 || config.RecoveryRevision != nil {
			return errors.New("moegirl requires one to sixteen fixed indexes and no recovery revision or provider rights metadata")
		}
		seen := map[int]struct{}{}
		for _, index := range config.Indexes {
			if index.PageID <= 0 || index.RevisionID <= 0 || !HasCanonicalSHA1(index.SHA1) ||
				index.RevisionTimestamp != "" || index.ContentSHA256 != "" || index.RawSHA256 != "" ||
				strings.TrimSpace(index.Title) == "" || strings.TrimSpace(index.Title) != index.Title {
				return errors.New("moegirl fixed index identity is incomplete")
			}
			if _, exists := seen[index.PageID]; exists {
				return errors.New("moegirl fixed index page is duplicated")
			}
			seen[index.PageID] = struct{}{}
		}
	}
	if config.Provider == ProviderMoegirlPublicExact {
		if config.RightsText != "" || len(config.Indexes) != 0 || len(config.ContributorAliases) != 0 ||
			config.RecoveryRevision != nil || !config.RecoveryExactCapture || len(config.ExactPublicTargets) == 0 {
			return errors.New("moegirl_public_exact requires only exact public-page artifact targets")
		}
	}
	if config.Provider == ProviderSekaipedia {
		if config.RightsText != sekaipediaRightsText || len(config.Indexes) != 1 {
			return errors.New("sekaipedia requires the verified CC BY-SA 4.0 metadata and one fixed song-index authority")
		}
		index := config.Indexes[0]
		revisionTimestamp, timestampErr := time.Parse(time.RFC3339Nano, index.RevisionTimestamp)
		if sekaipediaAuthorityEvidenceID(index) == "" || !HasCanonicalSHA1(index.SHA1) ||
			!canonicalIndexEvidenceSHA256.MatchString(index.ContentSHA256) ||
			!canonicalIndexEvidenceSHA256.MatchString(index.RawSHA256) || timestampErr != nil ||
			revisionTimestamp.UTC().Format(time.RFC3339Nano) != index.RevisionTimestamp ||
			!strings.HasSuffix(index.RevisionTimestamp, "Z") {
			return errors.New("sekaipedia fixed song-index authority identity is invalid")
		}
		if config.RecoveryRevision != nil {
			if !config.RecoveryExactCapture || validateRecoverySekaipediaRevision(*config.RecoveryRevision) != nil {
				return errors.New("sekaipedia exact recovery revision identity is invalid")
			}
		}
	}
	return nil
}

func validateSekaipediaPageTargets(
	provider model.LyricsSourceProvider,
	targets []SekaipediaPageTarget,
) error {
	if len(targets) == 0 {
		return nil
	}
	if provider != ProviderSekaipedia || len(targets) > 10_000 {
		return errors.New("only Sekaipedia accepts a bounded exact page-target map")
	}
	seenTitles := make(map[string]struct{}, len(targets))
	seenResolvedTitles := make(map[string]struct{}, len(targets))
	seenFixedPages := make(map[int]struct{})
	seenFixedRevisions := make(map[int]struct{})
	seenFixedResponses := make(map[string]struct{})
	lastMusicID := 0
	for _, target := range targets {
		resolvedTitle := target.ResolvedPageTitle
		if resolvedTitle == "" {
			resolvedTitle = target.PageTitle
		}
		if target.MusicID <= lastMusicID || !validSekaipediaTargetTitle(target.PageTitle) ||
			!validSekaipediaTargetTitle(resolvedTitle) {
			return errors.New("Sekaipedia page-target map is invalid or not ordered by music ID")
		}
		key := strings.ToLower(strings.ReplaceAll(target.PageTitle, "_", " "))
		if _, duplicate := seenTitles[key]; duplicate {
			return errors.New("Sekaipedia page-target map contains a duplicate List page title")
		}
		resolvedKey := strings.ToLower(strings.ReplaceAll(resolvedTitle, "_", " "))
		if _, duplicate := seenResolvedTitles[resolvedKey]; duplicate {
			return errors.New("Sekaipedia page-target map contains a duplicate resolved page title")
		}
		seenTitles[key] = struct{}{}
		seenResolvedTitles[resolvedKey] = struct{}{}
		if target.FixedRevision != nil {
			fixed := *target.FixedRevision
			rawSHA256 := fixed.RawSHA256
			fixed.RawSHA256 = ""
			if !canonicalIndexEvidenceSHA256.MatchString(rawSHA256) || validateRecoverySekaipediaRevision(fixed) != nil {
				return errors.New("Sekaipedia fixed song revision target is invalid")
			}
			if _, duplicate := seenFixedPages[fixed.PageID]; duplicate {
				return errors.New("Sekaipedia fixed song page target is duplicated")
			}
			if _, duplicate := seenFixedRevisions[fixed.RevisionID]; duplicate {
				return errors.New("Sekaipedia fixed song revision target is duplicated")
			}
			if _, duplicate := seenFixedResponses[rawSHA256]; duplicate {
				return errors.New("Sekaipedia fixed song response target is duplicated")
			}
			seenFixedPages[fixed.PageID] = struct{}{}
			seenFixedRevisions[fixed.RevisionID] = struct{}{}
			seenFixedResponses[rawSHA256] = struct{}{}
		}
		lastMusicID = target.MusicID
	}
	return nil
}

func validSekaipediaTargetTitle(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00#[]{}<>|")
}

func validateExactPublicPageTargets(
	provider model.LyricsSourceProvider,
	targets []ExactPublicPageTarget,
) error {
	if provider != ProviderMoegirlPublicExact {
		if len(targets) != 0 {
			return errors.New("only moegirl_public_exact accepts exact public-page targets")
		}
		return nil
	}
	if len(targets) == 0 || len(targets) > 16 {
		return errors.New("moegirl_public_exact requires a bounded exact target set")
	}
	lastMusicID := 0
	seenPages := make(map[int]struct{}, len(targets))
	seenRevisions := make(map[int]struct{}, len(targets))
	for _, target := range targets {
		urlTarget, err := MoegirlPageURLTargetForURL(target.PageURL)
		fetchedAt, timestampErr := time.Parse(time.RFC3339Nano, target.FetchedAt)
		if target.MusicID <= lastMusicID || target.PageID <= 0 || target.RevisionID <= 0 ||
			err != nil || urlTarget.PageTitle != target.PageTitle || !validSekaipediaTargetTitle(target.JapaneseTitle) ||
			timestampErr != nil || fetchedAt.Location() != time.UTC ||
			fetchedAt.UTC().Format(time.RFC3339Nano) != target.FetchedAt ||
			validateExactPublicFileBinding(target.RawHTML, maxResponseBytes) != nil ||
			validateExactPublicFileBinding(target.ExtractionReport, 1<<20) != nil ||
			target.RawHTML.Path == target.ExtractionReport.Path {
			return errors.New("moegirl_public_exact target identity is invalid")
		}
		if _, duplicate := seenPages[target.PageID]; duplicate {
			return errors.New("moegirl_public_exact page ID is duplicated")
		}
		if _, duplicate := seenRevisions[target.RevisionID]; duplicate {
			return errors.New("moegirl_public_exact revision ID is duplicated")
		}
		seenPages[target.PageID] = struct{}{}
		seenRevisions[target.RevisionID] = struct{}{}
		lastMusicID = target.MusicID
	}
	return nil
}

func validateExactPublicFileBinding(binding ExactPublicFileBinding, maxBytes int64) error {
	if !validExactPublicFilePath(binding.Path) || binding.SizeBytes <= 0 || binding.SizeBytes > maxBytes ||
		!canonicalIndexEvidenceSHA256.MatchString(binding.SHA256) {
		return errors.New("exact public-page file binding is invalid")
	}
	return nil
}

func validExactPublicFilePath(value string) bool {
	if value == "" || filepath.Clean(value) != value {
		return false
	}
	return strings.HasPrefix(value, "/private/tmp/") ||
		strings.HasPrefix(value, "/Volumes/Amia/Akiyama_mizuki/Coding/sessions/")
}

func validateProviderContributorAliases(
	provider model.LyricsSourceProvider,
	aliases []ProviderContributorAlias,
) error {
	if len(aliases) == 0 {
		return nil
	}
	if provider != ProviderSekaipedia {
		return fmt.Errorf("provider %q does not accept contributor aliases", provider)
	}
	if len(aliases) > maxProviderContributorAliases {
		return errors.New("provider contributor alias plan is too large")
	}
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		catalogKey, catalogOK := providerContributorAliasKey(alias.CatalogContributor)
		providerKey, providerOK := providerContributorAliasKey(alias.ProviderContributor)
		if alias.MusicID <= 0 || !catalogOK || !providerOK || catalogKey == providerKey {
			return errors.New("provider contributor alias is invalid")
		}
		key := strconv.Itoa(alias.MusicID) + "\x00" + catalogKey
		if _, duplicate := seen[key]; duplicate {
			return errors.New("provider contributor alias is duplicated or ambiguous")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func providerContributorAliasKey(value string) (string, bool) {
	if value == "" || len(value) > maxProviderContributorAliasBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return "", false
	}
	contributors, ok := splitTopLevelContributors(value)
	if !ok || len(contributors) != 1 || contributors[0] != value {
		return "", false
	}
	key := normalizeTitle(value)
	return key, key != ""
}

func cloneProviderContributorAliases(input []ProviderContributorAlias) []ProviderContributorAlias {
	if input == nil {
		return nil
	}
	return append([]ProviderContributorAlias(nil), input...)
}

func cloneSekaipediaPageTargets(input []SekaipediaPageTarget) []SekaipediaPageTarget {
	if input == nil {
		return nil
	}
	result := append([]SekaipediaPageTarget(nil), input...)
	for index := range result {
		if result[index].FixedRevision != nil {
			fixed := *result[index].FixedRevision
			result[index].FixedRevision = &fixed
		}
	}
	return result
}

type sourceProvider interface {
	ProviderID() model.LyricsSourceProvider
	Search(context.Context, MusicIdentity) ([]Candidate, error)
	FetchFixedCandidateRevision(context.Context, MusicIdentity, Candidate) (FixedRevision, error)
}

type fandomProvider struct {
	client *Client
}

func (provider *fandomProvider) ProviderID() model.LyricsSourceProvider {
	return ProviderVocaloidFandom
}

func (provider *fandomProvider) Search(ctx context.Context, identity MusicIdentity) ([]Candidate, error) {
	return provider.client.Search(ctx, identity)
}

func (provider *fandomProvider) FetchFixedCandidateRevision(ctx context.Context, identity MusicIdentity, candidate Candidate) (FixedRevision, error) {
	return provider.client.FetchFixedCandidateRevision(ctx, identity, candidate)
}

// Registry routes reviewed candidates by provider identity. Its authority
// chain is deterministic and fail-closed: the first accepted provider candidate
// stops fallback, while closed provider-local failures retain their reason.
type Registry struct {
	providers map[model.LyricsSourceProvider]sourceProvider
	order     []model.LyricsSourceProvider
}

func NewRegistry(configs ...ProviderConfig) (*Registry, error) {
	providers := make(map[model.LyricsSourceProvider]sourceProvider, len(configs))
	for _, config := range configs {
		if !config.Enabled {
			continue
		}
		if err := config.validate(); err != nil {
			return nil, err
		}
		if _, exists := providers[config.Provider]; exists {
			return nil, fmt.Errorf("lyrics source provider %q is configured more than once", config.Provider)
		}
		client := newMediaWikiClient(config.APIEndpoint, config.CrawlDelay, config.CacheTTL, nil)
		switch config.Provider {
		case ProviderVocaloidFandom:
			providers[config.Provider] = &fandomProvider{client: client}
		case ProviderMoegirl:
			providers[config.Provider] = newMoegirlProvider(config, client)
		case ProviderSekaipedia:
			providers[config.Provider] = newSekaipediaProvider(config, client)
		}
	}
	if len(providers) == 0 {
		return nil, errors.New("at least one lyrics source provider must be enabled")
	}
	order := make([]model.LyricsSourceProvider, 0, len(providers))
	for _, providerID := range []model.LyricsSourceProvider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom} {
		if _, exists := providers[providerID]; exists {
			order = append(order, providerID)
		}
	}
	return &Registry{providers: providers, order: order}, nil
}

func newRegistryWithProviders(providers ...sourceProvider) (*Registry, error) {
	registry := &Registry{providers: map[model.LyricsSourceProvider]sourceProvider{}}
	for _, provider := range providers {
		if provider == nil || !model.IsValidLyricsSourceProvider(provider.ProviderID()) {
			return nil, errors.New("registry provider is nil or invalid")
		}
		if _, exists := registry.providers[provider.ProviderID()]; exists {
			return nil, errors.New("registry provider is duplicated")
		}
		registry.providers[provider.ProviderID()] = provider
	}
	for _, providerID := range []model.LyricsSourceProvider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom} {
		if _, exists := registry.providers[providerID]; exists {
			registry.order = append(registry.order, providerID)
		}
	}
	if len(registry.order) == 0 {
		return nil, errors.New("registry requires at least one provider")
	}
	return registry, nil
}

func (registry *Registry) FetchFixedCandidateRevision(ctx context.Context, identity MusicIdentity, candidate Candidate) (FixedRevision, error) {
	if registry == nil {
		return FixedRevision{}, errors.New("lyrics source registry is not configured")
	}
	providerID := candidate.Provider
	if providerID == "" {
		providerID = ProviderVocaloidFandom
	}
	provider := registry.providers[providerID]
	if provider == nil {
		return FixedRevision{}, fmt.Errorf("registry preview provider %q is not configured: %w", providerID, ErrMalformedResponse)
	}
	return provider.FetchFixedCandidateRevision(ctx, identity, candidate)
}

var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
	defaultRegistryErr  error
)

// DefaultRegistry is shared by legacy fallback discovery and fixed-fetch
// executors so each fallback provider has one process-wide crawl limiter and
// response cache. Sekaipedia must be constructed from reviewed immutable
// authority data and is therefore never added implicitly here.
func DefaultRegistry() (*Registry, error) {
	defaultRegistryOnce.Do(func() {
		defaultRegistry, defaultRegistryErr = NewRegistry(DefaultProviderConfigs()...)
	})
	return defaultRegistry, defaultRegistryErr
}

func newMediaWikiClient(endpoint string, minInterval, cacheTTL time.Duration, httpClient *http.Client) *Client {
	return newMediaWikiClientWithSafety(endpoint, minInterval, cacheTTL, httpClient, nil)
}

func newMediaWikiClientWithSafety(
	endpoint string,
	minInterval time.Duration,
	cacheTTL time.Duration,
	httpClient *http.Client,
	safety *RecoveryProviderSafety,
) *Client {
	if httpClient == nil {
		httpClient = httpx.NewUpstreamClientWithOptions(httpx.UpstreamClientOptions{
			Timeout: 12 * time.Second, DialTimeout: 10 * time.Second, TLSHandshakeTimeout: 12 * time.Second,
			ResponseHeaderTimeout: 12 * time.Second, Policy: httpx.UpstreamPolicyFromEnvironment(), AllowQuery: true,
		})
	}
	client := &Client{
		endpoint: endpoint, httpClient: httpClient, minInterval: minInterval, cacheTTL: cacheTTL,
		cache: map[string]cacheEntry{}, inflight: map[string]*inflightRequest{},
		requestSlots: make(chan struct{}, maxInflightRequests), rateToken: make(chan struct{}, 1),
		recoverySafety: safety,
	}
	client.rateToken <- struct{}{}
	return client
}

func candidateProvider(candidate Candidate) model.LyricsSourceProvider {
	if candidate.Provider == "" {
		return ProviderVocaloidFandom
	}
	return candidate.Provider
}

func canonicalRevisionURL(provider model.LyricsSourceProvider, title string, revisionID int) string {
	switch provider {
	case ProviderMoegirl:
		canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
		query := canonical.Query()
		if revisionID > 0 {
			query.Set("oldid", strconv.Itoa(revisionID))
		}
		query.Set("title", title)
		canonical.RawQuery = query.Encode()
		return canonical.String()
	case ProviderSekaipedia:
		canonical := url.URL{
			Scheme: "https", Host: "www.sekaipedia.org", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_"),
		}
		if revisionID > 0 {
			query := canonical.Query()
			query.Set("oldid", strconv.Itoa(revisionID))
			canonical.RawQuery = query.Encode()
		}
		return canonical.String()
	default:
		return canonicalURL(title, revisionID)
	}
}

func canonicalFetchedAt(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
