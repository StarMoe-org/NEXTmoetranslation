package lyricssource

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/model"
)

// recoveryCanonicalEndpoint maps a live MediaWiki provider to the API endpoint
// this package already owns. Values are pinned to the reviewed provider policy
// table by TestRecoveryCanonicalEndpointMatchesProviderPolicy.
func recoveryCanonicalEndpoint(provider model.LyricsSourceProvider) (string, bool) {
	switch provider {
	case ProviderVocaloidFandom:
		return vocaloidWikiAPI, true
	case ProviderMoegirl:
		return moegirlAPI, true
	case ProviderSekaipedia:
		return sekaipediaAPI, true
	default:
		return "", false
	}
}

// RecoveryProviderConfig constructs one provider configuration from reviewed
// plan data while retaining endpoint, origin, and rights ownership here.
func RecoveryProviderConfig(
	provider model.LyricsSourceProvider,
	crawlDelay time.Duration,
	cacheTTL time.Duration,
	indexes []FixedIndex,
	aliases []ProviderContributorAlias,
	targetSets ...[]SekaipediaPageTarget,
) (ProviderConfig, error) {
	if len(targetSets) > 1 {
		return ProviderConfig{}, errors.New("recovery provider accepts at most one exact page-target map")
	}
	var targets []SekaipediaPageTarget
	if len(targetSets) == 1 {
		targets = cloneSekaipediaPageTargets(targetSets[0])
	}
	endpoint, ok := recoveryCanonicalEndpoint(provider)
	if !ok {
		return ProviderConfig{}, errors.New("recovery provider is unsupported")
	}
	config := ProviderConfig{
		Provider: provider, Enabled: true, APIEndpoint: endpoint,
		Indexes:              append([]FixedIndex(nil), indexes...),
		ContributorAliases:   cloneProviderContributorAliases(aliases),
		SekaipediaTargets:    cloneSekaipediaPageTargets(targets),
		CrawlDelay:           crawlDelay,
		CacheTTL:             cacheTTL,
		RecoveryExactCapture: true,
	}
	switch provider {
	case ProviderSekaipedia:
		if len(indexes) != 1 || !canonicalIndexEvidenceSHA256.MatchString(indexes[0].ContentSHA256) {
			return ProviderConfig{}, errors.New("recovery Sekaipedia authority requires exact revision-content SHA-256")
		}
		config.Origin = OriginSekaipedia
		config.RightsText = sekaipediaRightsText
	case ProviderMoegirl:
		config.Origin = OriginMoegirl
	case ProviderVocaloidFandom:
		config.Origin = OriginVocaloidFandom
	}
	if err := config.validate(); err != nil {
		return ProviderConfig{}, err
	}
	return cloneRecoveryProviderConfig(config), nil
}

func RecoveryExactPublicProviderConfig(
	crawlDelay time.Duration,
	cacheTTL time.Duration,
	targets []ExactPublicPageTarget,
) (ProviderConfig, error) {
	config := ProviderConfig{
		Provider: ProviderMoegirlPublicExact, Enabled: true,
		Origin: OriginMoegirlPublicExact, APIEndpoint: "",
		Indexes: []FixedIndex{}, ContributorAliases: []ProviderContributorAlias{},
		SekaipediaTargets:  []SekaipediaPageTarget{},
		ExactPublicTargets: append([]ExactPublicPageTarget(nil), targets...),
		CrawlDelay:         crawlDelay, CacheTTL: cacheTTL, RecoveryExactCapture: true,
	}
	if err := config.validate(); err != nil {
		return ProviderConfig{}, err
	}
	return cloneRecoveryProviderConfig(config), nil
}

func cloneRecoveryProviderConfig(config ProviderConfig) ProviderConfig {
	config.Indexes = append([]FixedIndex(nil), config.Indexes...)
	config.ContributorAliases = cloneProviderContributorAliases(config.ContributorAliases)
	config.SekaipediaTargets = cloneSekaipediaPageTargets(config.SekaipediaTargets)
	config.ExactPublicTargets = append([]ExactPublicPageTarget(nil), config.ExactPublicTargets...)
	if config.RecoveryRevision != nil {
		revision := *config.RecoveryRevision
		config.RecoveryRevision = &revision
	}
	return config
}

// BindRecoverySekaipediaRevision returns a defensive canary-only provider
// configuration that may fetch exactly one plan-pinned song revision. Raw
// envelope digests are deliberately discarded here: only the immutable
// page/revision/timestamp/MediaWiki SHA-1/content SHA-256 tuple is authority.
func BindRecoverySekaipediaRevision(config ProviderConfig, revision FixedIndex) (ProviderConfig, error) {
	if config.Provider != ProviderSekaipedia || !config.RecoveryExactCapture {
		return ProviderConfig{}, errors.New("exact Sekaipedia recovery revision requires recovery capture")
	}
	revision.RawSHA256 = ""
	if err := validateRecoverySekaipediaRevision(revision); err != nil {
		return ProviderConfig{}, err
	}
	result := cloneRecoveryProviderConfig(config)
	result.RecoveryRevision = &revision
	if err := result.validate(); err != nil {
		return ProviderConfig{}, err
	}
	return cloneRecoveryProviderConfig(result), nil
}

func validateRecoverySekaipediaRevision(revision FixedIndex) error {
	timestamp, err := time.Parse(time.RFC3339Nano, revision.RevisionTimestamp)
	if revision.PageID <= 0 || revision.RevisionID <= 0 ||
		strings.TrimSpace(revision.Title) == "" || strings.TrimSpace(revision.Title) != revision.Title ||
		!HasCanonicalSHA1(revision.SHA1) || !canonicalIndexEvidenceSHA256.MatchString(revision.ContentSHA256) ||
		revision.RawSHA256 != "" || err != nil || !strings.HasSuffix(revision.RevisionTimestamp, "Z") ||
		timestamp.Location() != time.UTC || timestamp.UTC().Format(time.RFC3339Nano) != revision.RevisionTimestamp {
		return errors.New("exact Sekaipedia recovery revision identity is invalid")
	}
	return nil
}

// RecoveryHTTPResponse is the exact bounded completed response presented to a
// recovery transport before status classification or provider parsing.
type RecoveryHTTPResponse struct {
	Action              string
	CanonicalRequestURL string
	FetchedAt           time.Time
	StatusCode          int
	Status              string
	Header              http.Header
	Raw                 []byte
}

// RecoveryHTTPTransport supplies exact response time, durable pre-parse raw
// retention, and strict semantic acquisition admission in addition to the
// standard HTTP round trip. Offline transports prove the bytes already exist
// in an exact acquisition before parsing.
type RecoveryHTTPTransport interface {
	http.RoundTripper
	RecoveryFetchedAt(*http.Request, *http.Response) (time.Time, error)
	RecoveryRetainResponse(context.Context, *http.Request, *http.Response, RecoveryHTTPResponse) error
	RecoveryCommitResponse(context.Context, RecoveryHTTPResponse) error
}

type recoveryOfflineTransport interface {
	RecoveryHTTPTransport
	RecoveryOffline() bool
}

// recoveryOfflineRequestTransport identifies one exact request that is served
// from already-validated immutable bytes. Such a request must not consume the
// provider-wide actual-HTTP slot or crawl delay.
type recoveryOfflineRequestTransport interface {
	RecoveryRequestOffline(*http.Request) bool
}

// RecoveryProviderSafety is provider-wide request state shared by fresh
// recovery registries. Its private fields prevent callers from shortening a
// Retry-After cooldown or minting additional in-flight capacity.
type RecoveryProviderSafety struct {
	rateMu        sync.Mutex
	lastRequest   time.Time
	cooldownUntil time.Time
	rateToken     chan struct{}

	actualHTTPOnce  sync.Once
	actualHTTPToken chan struct{}
}

func NewRecoveryProviderSafety() *RecoveryProviderSafety {
	safety := &RecoveryProviderSafety{rateToken: make(chan struct{}, 1)}
	safety.rateToken <- struct{}{}
	return safety
}

// NewRecoveryRegistry constructs only explicitly configured providers and
// requires a non-default recovery transport for every one of them.
func NewRecoveryRegistry(
	configs []ProviderConfig,
	transports map[model.LyricsSourceProvider]RecoveryHTTPTransport,
) (*Registry, error) {
	return newRecoveryRegistry(configs, transports, nil)
}

// NewRecoveryRegistryWithProviderSafety preserves provider-wide delay,
// Retry-After, and one-actual-request state across fresh song-local registries.
func NewRecoveryRegistryWithProviderSafety(
	configs []ProviderConfig,
	transports map[model.LyricsSourceProvider]RecoveryHTTPTransport,
	safety map[model.LyricsSourceProvider]*RecoveryProviderSafety,
) (*Registry, error) {
	if safety == nil {
		return nil, errors.New("recovery provider safety is required")
	}
	return newRecoveryRegistry(configs, transports, safety)
}

func newRecoveryRegistry(
	configs []ProviderConfig,
	transports map[model.LyricsSourceProvider]RecoveryHTTPTransport,
	safety map[model.LyricsSourceProvider]*RecoveryProviderSafety,
) (*Registry, error) {
	providers := make(map[model.LyricsSourceProvider]sourceProvider, len(configs))
	for _, configured := range configs {
		config := cloneRecoveryProviderConfig(configured)
		if !config.Enabled {
			continue
		}
		if err := config.validate(); err != nil {
			return nil, err
		}
		if config.Provider == ProviderMoegirlPublicExact {
			return nil, errors.New("moegirl_public_exact is an offline exact artifact provider, not a MediaWiki registry provider")
		}
		transport := transports[config.Provider]
		if transport == nil {
			return nil, errors.New("recovery provider transport is required")
		}
		if _, duplicate := providers[config.Provider]; duplicate {
			return nil, errors.New("recovery provider is configured more than once")
		}
		runtimeCrawlDelay := config.CrawlDelay
		if offline, ok := transport.(recoveryOfflineTransport); ok && offline.RecoveryOffline() {
			runtimeCrawlDelay = 0
		}
		var providerSafety *RecoveryProviderSafety
		if safety != nil {
			providerSafety = safety[config.Provider]
			if providerSafety == nil || cap(providerSafety.rateToken) != 1 {
				return nil, errors.New("recovery provider-wide safety state is required")
			}
		}
		client := newMediaWikiClientWithSafety(
			config.APIEndpoint, runtimeCrawlDelay, config.CacheTTL,
			&http.Client{Transport: transport}, providerSafety,
		)
		switch config.Provider {
		case ProviderSekaipedia:
			providers[config.Provider] = newSekaipediaProvider(config, client)
		case ProviderMoegirl:
			providers[config.Provider] = newMoegirlProvider(config, client)
		case ProviderVocaloidFandom:
			providers[config.Provider] = &fandomProvider{client: client}
		}
	}
	if len(providers) == 0 {
		return nil, errors.New("recovery registry requires at least one provider")
	}
	registry := &Registry{providers: providers}
	for _, provider := range []model.LyricsSourceProvider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom} {
		if providers[provider] != nil {
			registry.order = append(registry.order, provider)
		}
	}
	return registry, nil
}
