// Package lyricsrecovery integrates immutable recovery plans, exact acquisition
// replay, provider parsers, content-free outcomes, evidence packs, and roots.
package lyricsrecovery

import (
	"errors"
	"sort"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

const MaxSekaipediaCanarySongs = lyricsextractionplan.MaxRecoveryLiveCanarySongs

type SekaipediaCanarySongPlan = lyricsextractionplan.RecoverySekaipediaCanarySong

type SekaipediaCanaryPlan struct {
	RecoveryPlanID     string
	RecoveryPlanSHA256 string
	ListAcquisitionID  string
	List               lyricssource.FixedIndex
	Songs              []SekaipediaCanarySongPlan
}

type RuntimeConfig struct {
	Order                    []model.LyricsSourceProvider
	ProviderMusicIDs         map[model.LyricsSourceProvider][]int
	Providers                []lyricssource.ProviderConfig
	Authorities              map[model.LyricsSourceProvider][]lyricssource.FixedIndex
	Parsers                  map[model.LyricsSourceProvider]string
	RecoveryPlanID           string
	RecoveryPlanSHA256       string
	PolicyVersion            string
	MaxAttempts              int
	RequestTimeout           time.Duration
	RetryDelay               time.Duration
	ProviderResponseBytes    int
	MaxActualNetworkInFlight int
	MediaWikiMaxlag          int
	LiveCanaryMusicIDs       []int
	SekaipediaCanary         *SekaipediaCanaryPlan
}

func RuntimeConfigFromPlan(plan lyricsextractionplan.RecoveryPlan) (RuntimeConfig, error) {
	if err := lyricsextractionplan.ValidateRecovery(plan); err != nil {
		return RuntimeConfig{}, err
	}
	planSHA256, err := lyricsextractionplan.RecoveryCanonicalSHA256(plan)
	if err != nil {
		return RuntimeConfig{}, err
	}
	result := RuntimeConfig{
		Order:                    make([]model.LyricsSourceProvider, len(plan.Providers.Order)),
		ProviderMusicIDs:         make(map[model.LyricsSourceProvider][]int),
		Providers:                make([]lyricssource.ProviderConfig, len(plan.Providers.Configurations)),
		Authorities:              make(map[model.LyricsSourceProvider][]lyricssource.FixedIndex),
		Parsers:                  make(map[model.LyricsSourceProvider]string),
		RecoveryPlanID:           plan.PlanID,
		RecoveryPlanSHA256:       planSHA256,
		PolicyVersion:            plan.Versions.ProviderPolicy,
		MaxAttempts:              plan.Execution.MaxAttempts,
		RequestTimeout:           time.Duration(plan.Execution.RequestTimeoutMillis) * time.Millisecond,
		RetryDelay:               time.Duration(plan.Execution.RetryDelayMillis) * time.Millisecond,
		ProviderResponseBytes:    plan.Execution.ProviderResponseBytes,
		MaxActualNetworkInFlight: plan.Execution.MaxActualNetworkInFlight,
		MediaWikiMaxlag:          plan.Execution.MediaWikiMaxlag,
		LiveCanaryMusicIDs:       append([]int(nil), plan.Execution.LiveCanaryMusicIDs...),
	}
	for index, provider := range plan.Providers.Order {
		result.Order[index] = model.LyricsSourceProvider(provider)
	}
	for _, version := range plan.Versions.Parsers {
		result.Parsers[model.LyricsSourceProvider(version.Provider)] = version.ParserVersion
	}
	for index, configured := range plan.Providers.Configurations {
		provider := model.LyricsSourceProvider(configured.Provider)
		if len(configured.MusicIDs) > 0 {
			result.ProviderMusicIDs[provider] = append([]int(nil), configured.MusicIDs...)
		}
		authorities := make([]lyricssource.FixedIndex, len(configured.Authorities))
		for authorityIndex, authority := range configured.Authorities {
			authorities[authorityIndex] = lyricssource.FixedIndex{
				PageID: authority.PageID, RevisionID: authority.RevisionID,
				RevisionTimestamp: authority.RevisionTimestamp, SHA1: authority.SHA1,
				ContentSHA256: authority.ContentSHA256, RawSHA256: authority.RawSHA256, Title: authority.Title,
			}
		}
		aliases := make([]lyricssource.ProviderContributorAlias, len(configured.ContributorAliases))
		for aliasIndex, alias := range configured.ContributorAliases {
			aliases[aliasIndex] = lyricssource.ProviderContributorAlias{
				MusicID: alias.MusicID, CatalogContributor: alias.CatalogContributor,
				ProviderContributor: alias.ProviderContributor,
			}
		}
		targets := make([]lyricssource.SekaipediaPageTarget, len(configured.SekaipediaTargets))
		for targetIndex, target := range configured.SekaipediaTargets {
			targets[targetIndex] = lyricssource.SekaipediaPageTarget{
				MusicID: target.MusicID, PageTitle: target.PageTitle, ResolvedPageTitle: target.ResolvedPageTitle,
			}
			if target.FixedRevision != nil {
				fixed := target.FixedRevision
				targets[targetIndex].FixedRevision = &lyricssource.FixedIndex{
					PageID: fixed.PageID, RevisionID: fixed.RevisionID,
					RevisionTimestamp: fixed.RevisionTimestamp, SHA1: fixed.SHA1,
					ContentSHA256: fixed.ContentSHA256, RawSHA256: fixed.RawResponseSHA256,
					Title: target.ResolvedPageTitle,
				}
				if targets[targetIndex].FixedRevision.Title == "" {
					targets[targetIndex].FixedRevision.Title = target.PageTitle
				}
			}
		}
		exactPublicTargets := make([]lyricssource.ExactPublicPageTarget, len(configured.ExactPublicTargets))
		for targetIndex, target := range configured.ExactPublicTargets {
			exactPublicTargets[targetIndex] = lyricssource.ExactPublicPageTarget{
				MusicID: target.MusicID, PageURL: target.PageURL, PageTitle: target.PageTitle,
				JapaneseTitle: target.JapaneseTitle, PageID: target.PageID, RevisionID: target.RevisionID,
				FetchedAt: target.FetchedAt,
				RawHTML: lyricssource.ExactPublicFileBinding{
					Path: target.RawHTML.Path, SizeBytes: target.RawHTML.SizeBytes, SHA256: target.RawHTML.SHA256,
				},
				ExtractionReport: lyricssource.ExactPublicFileBinding{
					Path: target.ExtractionReport.Path, SizeBytes: target.ExtractionReport.SizeBytes,
					SHA256: target.ExtractionReport.SHA256,
				},
			}
		}
		var providerConfig lyricssource.ProviderConfig
		if provider == lyricssource.ProviderMoegirlPublicExact {
			providerConfig, err = lyricssource.RecoveryExactPublicProviderConfig(
				time.Duration(configured.CrawlDelayMillis)*time.Millisecond,
				time.Duration(configured.CacheTTLMillis)*time.Millisecond,
				exactPublicTargets,
			)
		} else {
			providerConfig, err = lyricssource.RecoveryProviderConfig(
				provider,
				time.Duration(configured.CrawlDelayMillis)*time.Millisecond,
				time.Duration(configured.CacheTTLMillis)*time.Millisecond,
				authorities,
				aliases,
				targets,
			)
		}
		if err != nil {
			return RuntimeConfig{}, err
		}
		result.Providers[index] = providerConfig
		result.Authorities[provider] = append([]lyricssource.FixedIndex(nil), authorities...)
	}
	if len(result.Order) != len(result.Providers) {
		return RuntimeConfig{}, errors.New("recovery runtime provider configuration is incomplete")
	}
	return cloneRuntimeConfig(result), nil
}

// WithSekaipediaCanaryPlan binds only exact identities carried by the same
// immutable recovery plan that produced runtime. Production code contains no
// mutable List, song, title, page, or revision literals.
func WithSekaipediaCanaryPlan(
	runtime RuntimeConfig,
	plan lyricsextractionplan.RecoveryPlan,
) (RuntimeConfig, error) {
	if err := lyricsextractionplan.ValidateRecovery(plan); err != nil || plan.SekaipediaCanary == nil {
		return RuntimeConfig{}, errors.New("Sekaipedia canary recovery plan binding is incomplete")
	}
	planSHA256, err := lyricsextractionplan.RecoveryCanonicalSHA256(plan)
	if err != nil || runtime.RecoveryPlanID != plan.PlanID || runtime.RecoveryPlanSHA256 != planSHA256 ||
		len(runtime.Order) == 0 || runtime.Order[0] != lyricssource.ProviderSekaipedia ||
		len(runtime.Authorities[lyricssource.ProviderSekaipedia]) != 1 ||
		len(plan.SekaipediaCanary.Songs) == 0 || len(plan.SekaipediaCanary.Songs) > MaxSekaipediaCanarySongs ||
		len(plan.SekaipediaCanary.Songs) != len(runtime.LiveCanaryMusicIDs) {
		return RuntimeConfig{}, errors.New("Sekaipedia canary recovery plan does not exactly match runtime")
	}
	list := runtime.Authorities[lyricssource.ProviderSekaipedia][0]
	plannedList := plan.SekaipediaCanary.List
	if list.PageID != plannedList.PageID || list.RevisionID != plannedList.RevisionID ||
		list.RevisionTimestamp != plannedList.RevisionTimestamp || list.SHA1 != plannedList.SHA1 ||
		list.ContentSHA256 != plannedList.ContentSHA256 || list.RawSHA256 != plannedList.RawResponseSHA256 {
		return RuntimeConfig{}, errors.New("Sekaipedia canary List does not exactly match the plan authority")
	}
	bound := append([]SekaipediaCanarySongPlan(nil), plan.SekaipediaCanary.Songs...)
	for index, song := range bound {
		if song.MusicID != runtime.LiveCanaryMusicIDs[index] {
			return RuntimeConfig{}, errors.New("Sekaipedia canary songs do not exactly match the plan-selected IDs")
		}
	}
	result := cloneRuntimeConfig(runtime)
	result.SekaipediaCanary = &SekaipediaCanaryPlan{
		RecoveryPlanID: runtime.RecoveryPlanID, RecoveryPlanSHA256: runtime.RecoveryPlanSHA256,
		ListAcquisitionID: plannedList.AcquisitionID, List: list, Songs: bound,
	}
	return cloneRuntimeConfig(result), nil
}

func (plan *SekaipediaCanaryPlan) song(musicID int) (SekaipediaCanarySongPlan, bool) {
	if plan == nil {
		return SekaipediaCanarySongPlan{}, false
	}
	for _, song := range plan.Songs {
		if song.MusicID == musicID {
			return song, true
		}
	}
	return SekaipediaCanarySongPlan{}, false
}

// ProviderOrderForMusicID returns the immutable per-song provider chain. An
// empty ProviderMusicIDs map preserves historical global-prefix behavior;
// scoped recovery plans authorize exactly one provider for each music ID.
func (config RuntimeConfig) ProviderOrderForMusicID(musicID int) ([]model.LyricsSourceProvider, error) {
	if musicID <= 0 || len(config.Order) == 0 {
		return nil, errors.New("recovery runtime provider scope lookup is invalid")
	}
	if len(config.ProviderMusicIDs) == 0 {
		return append([]model.LyricsSourceProvider(nil), config.Order...), nil
	}
	var result []model.LyricsSourceProvider
	for _, provider := range config.Order {
		musicIDs, configured := config.ProviderMusicIDs[provider]
		if !configured || len(musicIDs) == 0 {
			return nil, errors.New("recovery runtime provider scopes are incomplete")
		}
		index := sort.SearchInts(musicIDs, musicID)
		if index < len(musicIDs) && musicIDs[index] == musicID {
			result = append(result, provider)
		}
	}
	if len(result) != 1 {
		return nil, errors.New("recovery runtime music ID is not assigned to exactly one provider")
	}
	return result, nil
}

func (config RuntimeConfig) SekaipediaFixedRevision(
	musicID int,
) (lyricssource.FixedIndex, bool, error) {
	order, err := config.ProviderOrderForMusicID(musicID)
	if err != nil {
		return lyricssource.FixedIndex{}, false, err
	}
	if len(order) != 1 || order[0] != lyricssource.ProviderSekaipedia {
		return lyricssource.FixedIndex{}, false, nil
	}
	provider, err := runtimeProviderConfiguration(config, lyricssource.ProviderSekaipedia)
	if err != nil {
		return lyricssource.FixedIndex{}, false, err
	}
	index := sort.Search(len(provider.SekaipediaTargets), func(index int) bool {
		return provider.SekaipediaTargets[index].MusicID >= musicID
	})
	if index == len(provider.SekaipediaTargets) || provider.SekaipediaTargets[index].MusicID != musicID {
		return lyricssource.FixedIndex{}, false, errors.New("recovery runtime Sekaipedia target is missing")
	}
	if provider.SekaipediaTargets[index].FixedRevision == nil {
		return lyricssource.FixedIndex{}, false, nil
	}
	return *provider.SekaipediaTargets[index].FixedRevision, true, nil
}

func (config RuntimeConfig) ExactPublicTarget(
	musicID int,
) (lyricssource.ExactPublicPageTarget, bool, error) {
	order, err := config.ProviderOrderForMusicID(musicID)
	if err != nil {
		return lyricssource.ExactPublicPageTarget{}, false, err
	}
	if len(order) != 1 || order[0] != lyricssource.ProviderMoegirlPublicExact {
		return lyricssource.ExactPublicPageTarget{}, false, nil
	}
	provider, err := runtimeProviderConfiguration(config, lyricssource.ProviderMoegirlPublicExact)
	if err != nil {
		return lyricssource.ExactPublicPageTarget{}, false, err
	}
	index := sort.Search(len(provider.ExactPublicTargets), func(index int) bool {
		return provider.ExactPublicTargets[index].MusicID >= musicID
	})
	if index == len(provider.ExactPublicTargets) || provider.ExactPublicTargets[index].MusicID != musicID {
		return lyricssource.ExactPublicPageTarget{}, false, errors.New("recovery runtime exact public target is missing")
	}
	return provider.ExactPublicTargets[index], true, nil
}

func cloneRuntimeConfig(config RuntimeConfig) RuntimeConfig {
	config.Order = append([]model.LyricsSourceProvider(nil), config.Order...)
	providerMusicIDs := make(map[model.LyricsSourceProvider][]int, len(config.ProviderMusicIDs))
	for provider, musicIDs := range config.ProviderMusicIDs {
		providerMusicIDs[provider] = append([]int(nil), musicIDs...)
	}
	config.ProviderMusicIDs = providerMusicIDs
	config.LiveCanaryMusicIDs = append([]int(nil), config.LiveCanaryMusicIDs...)
	config.Providers = append([]lyricssource.ProviderConfig(nil), config.Providers...)
	for index := range config.Providers {
		provider := &config.Providers[index]
		provider.Indexes = append([]lyricssource.FixedIndex(nil), provider.Indexes...)
		provider.ContributorAliases = append([]lyricssource.ProviderContributorAlias(nil), provider.ContributorAliases...)
		provider.SekaipediaTargets = append([]lyricssource.SekaipediaPageTarget(nil), provider.SekaipediaTargets...)
		for targetIndex := range provider.SekaipediaTargets {
			if provider.SekaipediaTargets[targetIndex].FixedRevision != nil {
				fixed := *provider.SekaipediaTargets[targetIndex].FixedRevision
				provider.SekaipediaTargets[targetIndex].FixedRevision = &fixed
			}
		}
		provider.ExactPublicTargets = append([]lyricssource.ExactPublicPageTarget(nil), provider.ExactPublicTargets...)
		if provider.RecoveryRevision != nil {
			revision := *provider.RecoveryRevision
			provider.RecoveryRevision = &revision
		}
	}
	authorities := make(map[model.LyricsSourceProvider][]lyricssource.FixedIndex, len(config.Authorities))
	for provider, values := range config.Authorities {
		authorities[provider] = append([]lyricssource.FixedIndex(nil), values...)
	}
	config.Authorities = authorities
	parsers := make(map[model.LyricsSourceProvider]string, len(config.Parsers))
	for provider, version := range config.Parsers {
		parsers[provider] = version
	}
	config.Parsers = parsers
	if config.SekaipediaCanary != nil {
		canary := *config.SekaipediaCanary
		canary.Songs = append([]SekaipediaCanarySongPlan(nil), canary.Songs...)
		config.SekaipediaCanary = &canary
	}
	return config
}
