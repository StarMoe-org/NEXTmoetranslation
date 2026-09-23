package lyricsextractionplan

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

func ValidateRecovery(plan RecoveryPlan) error {
	return validateRecovery(plan, false)
}

// ValidateRecoveryForInspection accepts the one historical recovery-plan-v2
// shape that predates the exact Sekaipedia List replay AcquisitionID (and its
// matching revision-content SHA-256). The returned plan remains inspection-only:
// every operational validator and encoder continues to call ValidateRecovery.
func ValidateRecoveryForInspection(plan RecoveryPlan) error {
	return validateRecovery(plan, true)
}

func validateRecovery(plan RecoveryPlan, allowHistoricalListInspection bool) error {
	historicalListInspection := allowHistoricalListInspection && plan.SekaipediaCanary != nil &&
		plan.SekaipediaCanary.List.AcquisitionID == ""
	if plan.SchemaVersion != RecoverySchemaVersionV2 || plan.CanonicalEncoding != RecoveryCanonicalEncodingV2 ||
		plan.DigestAlgorithm != RecoveryDigestAlgorithmV2 {
		return errors.New("lyrics recovery plan has an unsupported version or canonical encoding")
	}
	if !validID(plan.PlanID) || !validID(plan.Scope.ScopeID) {
		return errors.New("lyrics recovery plan or scope identity is invalid")
	}
	createdAt, err := parseCanonicalTimestamp(plan.CreatedAt)
	if err != nil {
		return fmt.Errorf("lyrics recovery plan createdAt: %w", err)
	}
	capturedAt, err := validateRecoverySourceSnapshot(plan.SourceSnapshot)
	if err != nil {
		return fmt.Errorf("lyrics recovery source snapshot: %w", err)
	}
	if capturedAt.After(createdAt) {
		return errors.New("lyrics recovery source snapshot capturedAt is after plan createdAt")
	}
	if err := validateRecoveryCatalog(plan.Catalog); err != nil {
		return err
	}
	if err := validateRecoveryScope(plan.Scope, plan.Catalog); err != nil {
		return err
	}
	if err := validateRecoveryProviders(
		plan.Providers, plan.Execution, plan.Scope.MusicIDs, createdAt, historicalListInspection,
	); err != nil {
		return err
	}
	if !recoveryVersionsRegistered(plan.Versions, plan.Providers.Order) {
		return errors.New("lyrics recovery plan versions are not registered by this binary")
	}
	if err := validateRecoveryExecution(plan.Execution, plan.Scope.MusicIDs); err != nil {
		return err
	}
	if err := validateRecoverySekaipediaCanary(
		plan.SekaipediaCanary, plan.Providers, plan.Execution, createdAt, historicalListInspection,
	); err != nil {
		return err
	}
	if err := validateRecoveryOutputs(plan.Outputs, plan.Catalog.Path); err != nil {
		return err
	}
	if !reflect.DeepEqual(plan.Deployment, RequiredDeploymentPolicy()) {
		return errors.New("lyrics recovery plan must retain every compiled HOLD")
	}
	return nil
}

func recoveryVersionsRegistered(candidate RecoveryVersions, order []Provider) bool {
	current := CompiledRecoveryVersions()
	if recoveryProviderOrderContains(order, ProviderMoegirlPublicExact) {
		var err error
		current, err = CompiledScopedRecoveryVersions(order)
		if err != nil {
			return false
		}
	}
	if reflect.DeepEqual(candidate, current) {
		return true
	}
	historical, err := historicalRecoveryVersionsV1(nil)
	if recoveryProviderOrderContains(order, ProviderMoegirlPublicExact) {
		historical, err = historicalRecoveryVersionsV1(order)
	}
	if err != nil {
		return false
	}
	if reflect.DeepEqual(candidate, historical) {
		return true
	}
	// Recovery-plan-v2 bytes created before ruby versioning omitted the field.
	// Accept only the all-empty historical shape, then compare every other
	// version field and parser identity against the immutable v1 vocabulary.
	normalized := candidate
	for index := range normalized.Parsers {
		if normalized.Parsers[index].RubyGeneratorVersion != "" {
			return false
		}
		switch normalized.Parsers[index].Provider {
		case ProviderSekaipedia:
			normalized.Parsers[index].RubyGeneratorVersion = historicalRegisteredSekaipediaRuby
		case ProviderMoegirl, ProviderMoegirlPublicExact, ProviderVocaloidFandom:
			normalized.Parsers[index].RubyGeneratorVersion = historicalRegisteredStructuredRuby
		default:
			return false
		}
	}
	return reflect.DeepEqual(normalized, historical)
}

func validateRecoveryCatalog(catalog RecoveryCatalogBinding) error {
	if !validRecoveryAbsolutePath(catalog.Path, true) || catalog.SizeBytes <= 0 || catalog.SizeBytes > MaxCatalogDatabaseBytes ||
		!canonicalSHA256.MatchString(catalog.SourceSHA256) || !canonicalSHA256.MatchString(catalog.IdentitySHA256) ||
		!canonicalSHA256.MatchString(catalog.MusicIDsSHA256) {
		return errors.New("lyrics recovery catalog exact identity is invalid")
	}
	if catalog.SchemaVersion != CatalogSchemaVersion || catalog.RuntimeSchemaVersion < CatalogSchemaVersion ||
		catalog.RuntimeSchemaVersion > MaximumCatalogRuntimeSchema || catalog.RecordCount <= 0 ||
		catalog.RecordCount > MaxRecoveryScopeMusicIDs ||
		catalog.IdentityPolicyVersion != CompiledEffectiveVersions().Policies.CatalogIdentity {
		return errors.New("lyrics recovery catalog schema or policy binding is invalid")
	}
	return nil
}

func validateRecoveryScope(scope RecoveryScopeBinding, catalog RecoveryCatalogBinding) error {
	if scope.MusicIDs == nil || len(scope.MusicIDs) == 0 || len(scope.MusicIDs) > catalog.RecordCount {
		return errors.New("lyrics recovery scope must contain an explicit bounded music ID set")
	}
	last := 0
	for _, musicID := range scope.MusicIDs {
		if musicID <= last {
			return errors.New("lyrics recovery scope music IDs must be positive, unique, and strictly increasing")
		}
		last = musicID
	}
	switch scope.Kind {
	case RecoveryScopeFinal:
		musicIDsSHA256, err := recoveryOrderedMusicIDsSHA256(scope.MusicIDs)
		if scope.SupersedesRootID != "" || scope.SupersedesRootSHA256 != "" || len(scope.MusicIDs) != catalog.RecordCount ||
			err != nil || musicIDsSHA256 != catalog.MusicIDsSHA256 {
			return errors.New("final lyrics recovery scope must exactly cover the catalog without a parent")
		}
	case RecoveryScopePartial, RecoveryScopeRetry:
		if !validID(scope.SupersedesRootID) || !canonicalSHA256.MatchString(scope.SupersedesRootSHA256) ||
			len(scope.MusicIDs) >= catalog.RecordCount {
			return errors.New("partial or retry lyrics recovery scope requires an exact parent and true subset")
		}
	default:
		return errors.New("lyrics recovery scope kind is invalid")
	}
	return nil
}

func recoveryOrderedMusicIDsSHA256(musicIDs []int) (string, error) {
	if len(musicIDs) == 0 || len(musicIDs) > MaxRecoveryScopeMusicIDs {
		return "", errors.New("catalog ordered music IDs must have a positive bounded count")
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-root-catalog-ordered-music-ids-v1\x00"))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(musicIDs)))
	_, _ = digest.Write(encoded[:])
	lastMusicID := 0
	for index, musicID := range musicIDs {
		if musicID <= 0 || index > 0 && musicID <= lastMusicID {
			return "", errors.New("catalog ordered music IDs must be positive, strictly increasing, and unique")
		}
		binary.BigEndian.PutUint64(encoded[:], uint64(musicID))
		_, _ = digest.Write(encoded[:])
		lastMusicID = musicID
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validateRecoveryProviders(
	providers RecoveryProviderConfiguration,
	execution RecoveryExecutionSettings,
	scopeMusicIDs []int,
	createdAt time.Time,
	allowHistoricalListInspection bool,
) error {
	if len(providers.Configurations) != len(providers.Order) {
		return errors.New("lyrics recovery provider configurations do not exactly match the selected authority chain")
	}
	scope := make(map[int]struct{}, len(scopeMusicIDs))
	for _, musicID := range scopeMusicIDs {
		scope[musicID] = struct{}{}
	}
	providerScopes, scoped, err := validateRecoveryProviderScopes(providers, scope)
	if err != nil {
		return err
	}
	if !scoped {
		canonicalChain := []Provider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom}
		sekaipediaOnly := []Provider{ProviderSekaipedia}
		if !reflect.DeepEqual(providers.Order, canonicalChain) && !reflect.DeepEqual(providers.Order, sekaipediaOnly) {
			return errors.New("unscoped lyrics recovery provider order must be Sekaipedia-only or the complete canonical authority chain")
		}
	}
	expected := providers.Order
	for index, configured := range providers.Configurations {
		provider := expected[index]
		if configured.Provider != provider || configured.Mode != ProviderModeActive || configured.Authorities == nil ||
			configured.ContributorAliases == nil {
			return fmt.Errorf("lyrics recovery provider %q configuration is incomplete", provider)
		}
		if configured.CrawlDelayMillis < CompiledSafetyFloors().ProviderCrawlDelayMillis ||
			configured.CrawlDelayMillis > CompiledHardCeilings().ProviderCrawlDelayMillis ||
			configured.CacheTTLMillis < CompiledSafetyFloors().ProviderCacheTTLMillis ||
			configured.CacheTTLMillis > CompiledHardCeilings().ProviderCacheTTLMillis {
			return fmt.Errorf("lyrics recovery provider %q scheduling is outside compiled bounds", provider)
		}
		switch provider {
		case ProviderSekaipedia:
			if len(configured.Authorities) != 1 {
				return errors.New("lyrics recovery Sekaipedia requires exactly one active authority")
			}
		case ProviderMoegirl:
			if len(configured.Authorities) == 0 || len(configured.Authorities) > MaxAuthoritiesPerProvider {
				return errors.New("lyrics recovery legacy Moegirl API requires a bounded authority set")
			}
		case ProviderMoegirlPublicExact:
			if len(configured.Authorities) != 0 || len(configured.ContributorAliases) != 0 {
				return errors.New("lyrics recovery exact public Moegirl accepts neither API authorities nor contributor aliases")
			}
		case ProviderVocaloidFandom:
			if len(configured.Authorities) != 0 || len(configured.ContributorAliases) != 0 {
				return errors.New("lyrics recovery Fandom accepts neither fixed authorities nor contributor aliases")
			}
		}
		for authorityIndex, authority := range configured.Authorities {
			if authority.Disposition != AuthorityActive || authority.Role != AuthorityRoleSongIndex ||
				authorityIndex > 0 && provider == ProviderSekaipedia {
				return fmt.Errorf("lyrics recovery provider %q authority disposition is invalid", provider)
			}
			if err := validateFixedAuthority(provider, authority, CompiledHardCeilings(), createdAt); err != nil {
				return err
			}
			if provider == ProviderSekaipedia && !canonicalSHA256.MatchString(authority.ContentSHA256) &&
				!(allowHistoricalListInspection && authority.ContentSHA256 == "") {
				return errors.New("lyrics recovery Sekaipedia authority requires exact revision-content SHA-256")
			}
		}
		providerScope := scope
		if scoped {
			providerScope = providerScopes[provider]
		}
		if err := validateRecoveryAliases(provider, configured.ContributorAliases, providerScope); err != nil {
			return err
		}
		if err := validateRecoverySekaipediaTargets(provider, configured.SekaipediaTargets, providerScope, createdAt); err != nil {
			return err
		}
		if err := validateRecoveryExactPublicTargets(
			provider, configured.ExactPublicTargets, providerScope, createdAt,
		); err != nil {
			return err
		}
	}
	if execution.ProviderResponseBytes != lyricsproviderpolicy.ResponseSizeCeilingBytesV1 {
		return errors.New("lyrics recovery response ceiling does not match the provider policy")
	}
	return nil
}

func validateRecoveryProviderScopes(
	providers RecoveryProviderConfiguration,
	planScope map[int]struct{},
) (map[Provider]map[int]struct{}, bool, error) {
	hasScoped := false
	hasUnscoped := false
	for _, configured := range providers.Configurations {
		if len(configured.MusicIDs) == 0 {
			hasUnscoped = true
		} else {
			hasScoped = true
		}
	}
	if !hasScoped {
		return nil, false, nil
	}
	if hasUnscoped || !recoveryProviderOrderIsCanonicalSubsequence(providers.Order) {
		return nil, false, errors.New("scoped lyrics recovery providers require one non-empty canonical music-ID scope per selected provider")
	}

	result := make(map[Provider]map[int]struct{}, len(providers.Configurations))
	assigned := make(map[int]Provider, len(planScope))
	for index, configured := range providers.Configurations {
		if configured.Provider != providers.Order[index] {
			return nil, false, errors.New("scoped lyrics recovery provider configurations are reordered")
		}
		providerScope := make(map[int]struct{}, len(configured.MusicIDs))
		lastMusicID := 0
		for _, musicID := range configured.MusicIDs {
			if musicID <= lastMusicID {
				return nil, false, errors.New("scoped lyrics recovery provider music IDs must be positive, unique, and strictly increasing")
			}
			if _, allowed := planScope[musicID]; !allowed {
				return nil, false, errors.New("scoped lyrics recovery provider music ID is outside the plan scope")
			}
			if _, duplicate := assigned[musicID]; duplicate {
				return nil, false, errors.New("scoped lyrics recovery provider music-ID scopes overlap")
			}
			providerScope[musicID] = struct{}{}
			assigned[musicID] = configured.Provider
			lastMusicID = musicID
		}
		result[configured.Provider] = providerScope
	}
	if len(assigned) != len(planScope) {
		return nil, false, errors.New("scoped lyrics recovery provider music-ID scopes do not exactly partition the plan scope")
	}
	return result, true, nil
}

func recoveryProviderOrderIsCanonicalSubsequence(order []Provider) bool {
	legacy := []Provider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom}
	exactPublic := []Provider{ProviderSekaipedia, ProviderMoegirlPublicExact}
	return providerOrderIsSubsequence(order, legacy) || providerOrderIsSubsequence(order, exactPublic)
}

func providerOrderIsSubsequence(order, canonical []Provider) bool {
	if len(order) == 0 || len(order) > len(canonical) {
		return false
	}
	next := 0
	for _, provider := range order {
		found := false
		for next < len(canonical) {
			candidate := canonical[next]
			next++
			if candidate == provider {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func recoveryProviderOrderContains(order []Provider, wanted Provider) bool {
	for _, provider := range order {
		if provider == wanted {
			return true
		}
	}
	return false
}

// EffectiveRecoveryProviderOrder returns the provider chain authorized for one
// music ID. Historical unscoped plans retain their complete global prefix;
// scoped plans assign every song to exactly one provider and therefore cannot
// silently invoke fallback providers outside that immutable assignment.
func EffectiveRecoveryProviderOrder(
	providers RecoveryProviderConfiguration,
	musicID int,
) ([]Provider, error) {
	if musicID <= 0 || len(providers.Order) == 0 || len(providers.Configurations) != len(providers.Order) {
		return nil, errors.New("lyrics recovery provider scope lookup is invalid")
	}
	hasScoped := false
	hasUnscoped := false
	for index, configured := range providers.Configurations {
		if configured.Provider != providers.Order[index] {
			return nil, errors.New("lyrics recovery provider scope lookup is reordered")
		}
		if len(configured.MusicIDs) == 0 {
			hasUnscoped = true
		} else {
			hasScoped = true
		}
	}
	if !hasScoped {
		return append([]Provider(nil), providers.Order...), nil
	}
	if hasUnscoped || !recoveryProviderOrderIsCanonicalSubsequence(providers.Order) {
		return nil, errors.New("lyrics recovery provider scopes are incomplete")
	}
	var result []Provider
	for _, configured := range providers.Configurations {
		index := sort.SearchInts(configured.MusicIDs, musicID)
		if index < len(configured.MusicIDs) && configured.MusicIDs[index] == musicID {
			result = append(result, configured.Provider)
		}
	}
	if len(result) != 1 {
		return nil, errors.New("lyrics recovery music ID is not assigned to exactly one provider")
	}
	return result, nil
}

func validateRecoverySekaipediaTargets(
	provider Provider,
	targets []RecoverySekaipediaPageTarget,
	scope map[int]struct{},
	createdAt time.Time,
) error {
	if len(targets) == 0 {
		return nil
	}
	if provider != ProviderSekaipedia || len(targets) > MaxRecoverySekaipediaTargets || len(targets) != len(scope) {
		return errors.New("lyrics recovery Sekaipedia page targets must exactly cover the provider scope")
	}
	seenTitles := make(map[string]struct{}, len(targets))
	seenResolvedTitles := make(map[string]struct{}, len(targets))
	seenFixedPageIDs := make(map[int]struct{})
	seenFixedRevisionIDs := make(map[int]struct{})
	seenFixedRawSHA256 := make(map[string]struct{})
	lastMusicID := 0
	for _, target := range targets {
		if target.MusicID <= lastMusicID {
			return errors.New("lyrics recovery Sekaipedia page targets must be uniquely ordered by music ID")
		}
		resolvedTitle := target.ResolvedPageTitle
		if resolvedTitle == "" {
			resolvedTitle = target.PageTitle
		}
		if _, allowed := scope[target.MusicID]; !allowed || !validRecoverySekaipediaPageTitle(target.PageTitle) ||
			!validRecoverySekaipediaPageTitle(resolvedTitle) {
			return errors.New("lyrics recovery Sekaipedia page target is invalid or outside the plan scope")
		}
		key := strings.ToLower(strings.ReplaceAll(target.PageTitle, "_", " "))
		if _, duplicate := seenTitles[key]; duplicate {
			return errors.New("lyrics recovery Sekaipedia List page title is duplicated")
		}
		resolvedKey := strings.ToLower(strings.ReplaceAll(resolvedTitle, "_", " "))
		if _, duplicate := seenResolvedTitles[resolvedKey]; duplicate {
			return errors.New("lyrics recovery Sekaipedia resolved page title is duplicated")
		}
		seenTitles[key] = struct{}{}
		seenResolvedTitles[resolvedKey] = struct{}{}
		if target.FixedRevision != nil {
			fixed := *target.FixedRevision
			fixedAt, err := parseCanonicalTimestamp(fixed.RevisionTimestamp)
			if fixed.PageID <= 0 || fixed.PageID > MaxMediaWikiIdentity || fixed.RevisionID <= 0 ||
				fixed.RevisionID > MaxMediaWikiIdentity || !canonicalSHA1.MatchString(fixed.SHA1) ||
				!canonicalSHA256.MatchString(fixed.ContentSHA256) || !canonicalSHA256.MatchString(fixed.RawResponseSHA256) ||
				err != nil || fixedAt.After(createdAt) {
				return errors.New("lyrics recovery fixed Sekaipedia song revision identity is invalid")
			}
			if _, duplicate := seenFixedPageIDs[fixed.PageID]; duplicate {
				return errors.New("lyrics recovery fixed Sekaipedia song page ID is duplicated")
			}
			if _, duplicate := seenFixedRevisionIDs[fixed.RevisionID]; duplicate {
				return errors.New("lyrics recovery fixed Sekaipedia song revision ID is duplicated")
			}
			if _, duplicate := seenFixedRawSHA256[fixed.RawResponseSHA256]; duplicate {
				return errors.New("lyrics recovery fixed Sekaipedia song response digest is duplicated")
			}
			seenFixedPageIDs[fixed.PageID] = struct{}{}
			seenFixedRevisionIDs[fixed.RevisionID] = struct{}{}
			seenFixedRawSHA256[fixed.RawResponseSHA256] = struct{}{}
		}
		lastMusicID = target.MusicID
	}
	return nil
}

func validRecoverySekaipediaPageTitle(value string) bool {
	return value != "" && len(value) <= MaxRecoveryPageTitleBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00#[]{}<>|")
}

func validateRecoveryExactPublicTargets(
	provider Provider,
	targets []RecoveryExactPublicPageTarget,
	scope map[int]struct{},
	createdAt time.Time,
) error {
	if provider != ProviderMoegirlPublicExact {
		if len(targets) != 0 {
			return errors.New("only moegirl_public_exact accepts exact public-page targets")
		}
		return nil
	}
	if len(targets) == 0 || len(targets) > MaxRecoveryExactPublicTargets || len(targets) != len(scope) {
		return errors.New("moegirl_public_exact targets must exactly cover its provider scope")
	}
	seenPageIDs := make(map[int]struct{}, len(targets))
	seenRevisionIDs := make(map[int]struct{}, len(targets))
	seenURLs := make(map[string]struct{}, len(targets))
	lastMusicID := 0
	for _, target := range targets {
		if target.MusicID <= lastMusicID || target.PageID <= 0 || target.PageID > MaxMediaWikiIdentity ||
			target.RevisionID <= 0 || target.RevisionID > MaxMediaWikiIdentity ||
			!validRecoveryExactPublicURL(target.PageURL, target.PageTitle) ||
			!validRecoveryPublicTitle(target.JapaneseTitle) {
			return errors.New("moegirl_public_exact target identity is invalid")
		}
		if _, allowed := scope[target.MusicID]; !allowed {
			return errors.New("moegirl_public_exact target is outside its provider scope")
		}
		fetchedAt, err := parseCanonicalTimestamp(target.FetchedAt)
		if err != nil || fetchedAt.After(createdAt) {
			return errors.New("moegirl_public_exact target fetchedAt is invalid")
		}
		if err := validateRecoveryExactPublicFile(target.RawHTML, lyricsproviderpolicy.ResponseSizeCeilingBytesV1); err != nil {
			return fmt.Errorf("moegirl_public_exact raw HTML: %w", err)
		}
		if err := validateRecoveryExactPublicFile(target.ExtractionReport, MaxPlanBytes); err != nil {
			return fmt.Errorf("moegirl_public_exact extraction report: %w", err)
		}
		if target.RawHTML.Path == target.ExtractionReport.Path {
			return errors.New("moegirl_public_exact raw HTML and extraction report paths alias")
		}
		if _, duplicate := seenPageIDs[target.PageID]; duplicate {
			return errors.New("moegirl_public_exact page ID is duplicated")
		}
		if _, duplicate := seenRevisionIDs[target.RevisionID]; duplicate {
			return errors.New("moegirl_public_exact revision ID is duplicated")
		}
		if _, duplicate := seenURLs[target.PageURL]; duplicate {
			return errors.New("moegirl_public_exact page URL is duplicated")
		}
		seenPageIDs[target.PageID] = struct{}{}
		seenRevisionIDs[target.RevisionID] = struct{}{}
		seenURLs[target.PageURL] = struct{}{}
		lastMusicID = target.MusicID
	}
	return nil
}

func validateRecoveryExactPublicFile(binding RecoveryFileBinding, maxBytes int) error {
	if !validRecoveryDataPath(binding.Path) || binding.SizeBytes <= 0 || binding.SizeBytes > int64(maxBytes) ||
		!canonicalSHA256.MatchString(binding.SHA256) {
		return errors.New("exact file identity is invalid")
	}
	return nil
}

func validRecoveryExactPublicURL(value, pageTitle string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host != "zh.moegirl.org.cn" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" ||
		!validRecoveryPublicTitle(pageTitle) {
		return false
	}
	decoded, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil || decoded != pageTitle || strings.Contains(decoded, "/") {
		return false
	}
	canonical := (&url.URL{Scheme: "https", Host: "zh.moegirl.org.cn", Path: "/" + pageTitle}).String()
	return value == canonical
}

func validRecoveryPublicTitle(value string) bool {
	return value != "" && len(value) <= MaxRecoveryPageTitleBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00#[]{}<>|")
}

func validateRecoveryAliases(provider Provider, aliases []RecoveryContributorAlias, scope map[int]struct{}) error {
	if len(aliases) > MaxRecoveryAliases || provider != ProviderSekaipedia && len(aliases) != 0 {
		return errors.New("lyrics recovery contributor aliases are outside the Sekaipedia-only bound")
	}
	last := ""
	for _, alias := range aliases {
		if _, allowed := scope[alias.MusicID]; !allowed || !validRecoveryAliasText(alias.CatalogContributor) ||
			!validRecoveryAliasText(alias.ProviderContributor) || alias.CatalogContributor == alias.ProviderContributor {
			return errors.New("lyrics recovery contributor alias is invalid or outside the plan scope")
		}
		key := fmt.Sprintf("%020d\x00%s\x00%s", alias.MusicID, alias.CatalogContributor, alias.ProviderContributor)
		if last != "" && last >= key {
			return errors.New("lyrics recovery contributor aliases must be unique and canonically ordered")
		}
		last = key
	}
	return nil
}

func validRecoveryAliasText(value string) bool {
	if value == "" || len(value) > MaxRecoveryAliasBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	return true
}

func validateRecoveryExecution(execution RecoveryExecutionSettings, scope []int) error {
	ceilings := CompiledHardCeilings()
	floors := CompiledSafetyFloors()
	if execution.MaxAttempts < 1 || execution.MaxAttempts > ceilings.Attempts ||
		execution.RequestTimeoutMillis < floors.RequestTimeoutMillis || execution.RequestTimeoutMillis > ceilings.RequestTimeoutMillis ||
		execution.RetryDelayMillis < floors.RetryDelayMillis || execution.RetryDelayMillis > ceilings.RetryDelayMillis ||
		execution.ProviderResponseBytes != lyricsproviderpolicy.ResponseSizeCeilingBytesV1 ||
		execution.MaxActualNetworkInFlight != RecoveryMaxActualInFlight || execution.MediaWikiMaxlag != RecoveryRequiredMaxlag ||
		execution.LiveCanaryMusicIDs == nil || len(execution.LiveCanaryMusicIDs) > MaxRecoveryLiveCanarySongs {
		return errors.New("lyrics recovery execution settings violate the compiled provider safety contract")
	}
	allowed := make(map[int]struct{}, len(scope))
	for _, musicID := range scope {
		allowed[musicID] = struct{}{}
	}
	last := 0
	for _, musicID := range execution.LiveCanaryMusicIDs {
		if musicID <= last {
			return errors.New("lyrics recovery live-canary music IDs are not uniquely ordered")
		}
		if _, ok := allowed[musicID]; !ok {
			return errors.New("lyrics recovery live-canary music ID is outside the recovery scope")
		}
		last = musicID
	}
	return nil
}

func validateRecoverySekaipediaCanary(
	canary *RecoverySekaipediaCanaryPlan,
	providers RecoveryProviderConfiguration,
	execution RecoveryExecutionSettings,
	createdAt time.Time,
	allowHistoricalListInspection bool,
) error {
	if canary == nil {
		return nil
	}
	if len(execution.LiveCanaryMusicIDs) == 0 || canary.Songs == nil ||
		len(canary.Songs) != len(execution.LiveCanaryMusicIDs) || len(canary.Songs) > MaxRecoveryLiveCanarySongs {
		return errors.New("lyrics recovery Sekaipedia canary is not exactly bound to the selected music IDs")
	}
	var authority *FixedAuthority
	for index := range providers.Configurations {
		configured := &providers.Configurations[index]
		if configured.Provider == ProviderSekaipedia && len(configured.Authorities) == 1 {
			authority = &configured.Authorities[0]
			break
		}
	}
	list := canary.List
	if !canonicalSHA256.MatchString(list.AcquisitionID) &&
		!(allowHistoricalListInspection && list.AcquisitionID == "") {
		return errors.New("lyrics recovery Sekaipedia canary List exact replay acquisition ID is invalid")
	}
	if authority == nil || list.PageID != authority.PageID || list.RevisionID != authority.RevisionID ||
		list.RevisionTimestamp != authority.RevisionTimestamp || list.SHA1 != authority.SHA1 ||
		list.ContentSHA256 != authority.ContentSHA256 || list.RawResponseSHA256 != authority.RawSHA256 {
		return errors.New("lyrics recovery Sekaipedia canary List identity does not exactly match its fixed authority")
	}
	if err := validateRecoverySekaipediaCanaryRevision(list, createdAt, !allowHistoricalListInspection); err != nil {
		return fmt.Errorf("lyrics recovery Sekaipedia canary List: %w", err)
	}
	seenPages := make(map[int]struct{}, len(canary.Songs))
	seenRevisions := make(map[int]struct{}, len(canary.Songs))
	for index, song := range canary.Songs {
		if song.MusicID != execution.LiveCanaryMusicIDs[index] ||
			!validBoundedText(song.CatalogTitle, CompiledHardCeilings().CandidateTitleBytes) ||
			!validBoundedText(song.ProviderTitle, CompiledHardCeilings().CandidateTitleBytes) ||
			!canonicalSHA256.MatchString(song.ContentSHA256) || !canonicalSHA256.MatchString(song.RawResponseSHA256) {
			return errors.New("lyrics recovery Sekaipedia canary song identity is invalid or out of order")
		}
		revision := RecoverySekaipediaCanaryRevision{
			PageID: song.PageID, RevisionID: song.RevisionID, RevisionTimestamp: song.RevisionTimestamp,
			SHA1: song.SHA1, ContentSHA256: song.ContentSHA256, RawResponseSHA256: song.RawResponseSHA256,
		}
		if err := validateRecoverySekaipediaCanaryRevision(revision, createdAt, true); err != nil {
			return fmt.Errorf("lyrics recovery Sekaipedia canary music %d: %w", song.MusicID, err)
		}
		if _, duplicate := seenPages[song.PageID]; duplicate {
			return errors.New("lyrics recovery Sekaipedia canary song pages must be unique")
		}
		if _, duplicate := seenRevisions[song.RevisionID]; duplicate {
			return errors.New("lyrics recovery Sekaipedia canary song revisions must be unique")
		}
		seenPages[song.PageID] = struct{}{}
		seenRevisions[song.RevisionID] = struct{}{}
	}
	return nil
}

func validateRecoverySekaipediaCanaryRevision(
	revision RecoverySekaipediaCanaryRevision,
	createdAt time.Time,
	requireContentSHA256 bool,
) error {
	if revision.PageID <= 0 || revision.PageID > MaxMediaWikiIdentity ||
		revision.RevisionID <= 0 || revision.RevisionID > MaxMediaWikiIdentity ||
		!canonicalSHA1.MatchString(revision.SHA1) ||
		(requireContentSHA256 && !canonicalSHA256.MatchString(revision.ContentSHA256)) ||
		(!requireContentSHA256 && revision.ContentSHA256 != "" && !canonicalSHA256.MatchString(revision.ContentSHA256)) ||
		!canonicalSHA256.MatchString(revision.RawResponseSHA256) {
		return errors.New("page, revision, SHA-1, revision-content SHA-256, or raw response evidence identity is invalid")
	}
	timestamp, err := parseCanonicalTimestamp(revision.RevisionTimestamp)
	if err != nil {
		return fmt.Errorf("revision timestamp: %w", err)
	}
	if timestamp.After(createdAt) {
		return errors.New("revision timestamp is after plan createdAt")
	}
	return nil
}

func validateRecoveryOutputs(outputs RecoveryOutputs, catalogPath string) error {
	if outputs.Publication != RecoveryOutputCreateExclusive || outputs.Confidentiality != RecoveryOutputPrivate ||
		outputs.FileMode != 0o600 || outputs.DirectoryMode != 0o700 {
		return errors.New("lyrics recovery outputs must be private create-exclusive mode-0600/0700")
	}
	paths := []string{outputs.Ledger, outputs.AcquisitionSet, outputs.ProviderOutcomes, outputs.SongResults, outputs.EvidencePack, outputs.RootManifest}
	seen := map[string]struct{}{catalogPath: {}}
	for _, value := range paths {
		if !validRecoveryDataPath(value) {
			return errors.New("lyrics recovery output path is invalid")
		}
		if _, duplicate := seen[value]; duplicate {
			return errors.New("lyrics recovery output path aliases another plan path")
		}
		seen[value] = struct{}{}
	}
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	for index := 1; index < len(ordered); index++ {
		if strings.HasPrefix(ordered[index], ordered[index-1]+"/") {
			return errors.New("lyrics recovery output paths must not nest within each other")
		}
	}
	return nil
}

func validRecoveryDataPath(value string) bool {
	return validRecoveryAbsolutePath(value, false)
}
func validRecoveryAbsolutePath(value string, allowDatabase bool) bool {
	if value == "" || len(value) > MaxRecoveryOutputPathBytes || !utf8.ValidString(value) ||
		containsShellFragment(value) || !canonicalPath.MatchString(value) || !recoveryPrivatePathAllowed(value) ||
		strings.Contains(value, "//") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "production") || strings.Contains(lower, "/moesekai.db") {
		return false
	}
	extension := strings.ToLower(path.Ext(value))
	if allowDatabase {
		return extension == ".db" || extension == ".sqlite" || extension == ".sqlite3"
	}
	return extension != ".db" && extension != ".sqlite" && extension != ".sqlite3"
}

// recoveryPrivatePathAllowed retains the historical private-tmp boundary while
// permitting an explicitly selected external session tree. The active run path
// itself is not authority for arbitrary siblings: only canonical descendants of
// its immediate sessions root are accepted.
func recoveryPrivatePathAllowed(value string) bool {
	if strings.HasPrefix(value, "/private/tmp/") {
		return true
	}
	activeRoot := os.Getenv("MOESEKAI_SESSION_ROOT")
	if activeRoot == "" || strings.TrimSpace(activeRoot) != activeRoot || !filepath.IsAbs(activeRoot) ||
		filepath.Clean(activeRoot) != activeRoot {
		return false
	}
	sessionsRoot := filepath.Dir(activeRoot)
	relative, err := filepath.Rel(sessionsRoot, value)
	return err == nil && relative != "." && filepath.IsLocal(relative)
}
