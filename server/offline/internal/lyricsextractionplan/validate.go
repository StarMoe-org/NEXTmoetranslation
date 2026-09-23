package lyricsextractionplan

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	canonicalSHA1       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalSHA256     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalID         = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	canonicalEvidenceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	canonicalPath       = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

var (
	safeResumeErrorCodes = []string{
		"malformed_response", "rate_limited", "source_unavailable", "timeout",
	}
	safeResumeMissingReasons = []string{
		"no_search_hits", "title_mismatch", "credit_mismatch", "missing_song_signal",
	}
	safeResumeIncompleteCodes = []string{
		"ambiguous_source", "missing_lyrics", "unsupported_format",
	}
)

func Validate(plan Plan) error {
	if plan.SchemaVersion != SchemaVersionV1 || plan.CanonicalEncoding != CanonicalEncodingV1 ||
		plan.DigestAlgorithm != PlanDigestAlgorithm {
		return errors.New("extraction plan has an unsupported version or canonical encoding")
	}
	if !validID(plan.PlanID) {
		return errors.New("extraction plan has an invalid planId")
	}
	createdAt, err := parseCanonicalTimestamp(plan.CreatedAt)
	if err != nil {
		return fmt.Errorf("extraction plan createdAt: %w", err)
	}
	capturedAt, err := validateSourceSnapshot(plan.SourceSnapshot)
	if err != nil {
		return err
	}
	if capturedAt.After(createdAt) {
		return errors.New("source snapshot capturedAt is after plan createdAt")
	}
	if err := validateEffectiveVersions(plan.EffectiveVersions); err != nil {
		return err
	}
	if err := validateExecution(plan.Execution); err != nil {
		return err
	}
	inputs, err := validateInputs(plan.Inputs, plan.Execution.Ceilings)
	if err != nil {
		return err
	}
	if err := validateCatalog(plan.Catalog, inputs, plan.EffectiveVersions, plan.Execution.Ceilings); err != nil {
		return err
	}
	if err := validateProviders(plan.Providers, plan.Execution, createdAt); err != nil {
		return err
	}
	if err := validateResume(plan.Resume, inputs); err != nil {
		return err
	}
	if err := validateOutputs(plan.Outputs, plan.Inputs, plan.SourceSnapshot.Files); err != nil {
		return err
	}
	if !reflect.DeepEqual(plan.Deployment, RequiredDeploymentPolicy()) {
		return errors.New("deployment must retain every compiled extraction-plan v1 HOLD")
	}
	return nil
}

func validateCatalog(
	catalog CatalogIdentity,
	inputs map[string]InputIdentity,
	versions EffectiveVersions,
	ceilings HardCeilings,
) error {
	if !validID(catalog.InputID) {
		return errors.New("catalog has an invalid inputId")
	}
	input, ok := inputs[catalog.InputID]
	if !ok || input.Kind != InputCatalogDatabase {
		return errors.New("catalog inputId must resolve to the catalog_database identity")
	}
	if catalog.SchemaVersion != CatalogSchemaVersion ||
		catalog.RuntimeSchemaVersion < CatalogSchemaVersion ||
		catalog.RuntimeSchemaVersion > MaximumCatalogRuntimeSchema {
		return errors.New("catalog schema identity is unsupported")
	}
	if catalog.RecordCount < 1 || catalog.RecordCount > ceilings.CatalogRecords {
		return errors.New("catalog recordCount exceeds the plan collection ceiling")
	}
	if catalog.IdentityPolicyVersion != versions.Policies.CatalogIdentity {
		return errors.New("catalog identity policy does not match the plan effective policy")
	}
	return nil
}

func validateInputs(input []InputIdentity, ceilings HardCeilings) (map[string]InputIdentity, error) {
	if input == nil || len(input) == 0 || len(input) > MaxInputs {
		return nil, fmt.Errorf("inputs must contain between 1 and %d identities", MaxInputs)
	}
	byID := make(map[string]InputIdentity, len(input))
	paths := make(map[string]struct{}, len(input))
	catalogs := 0
	lastID := ""
	for index, identity := range input {
		if !validID(identity.ID) || (index > 0 && identity.ID <= lastID) {
			return nil, errors.New("input identities must have unique canonical IDs in ascending order")
		}
		lastID = identity.ID
		if !validDataPath(identity.Path) {
			return nil, fmt.Errorf("input %q has an unsafe data path", identity.ID)
		}
		if _, exists := paths[identity.Path]; exists {
			return nil, fmt.Errorf("input path %q is duplicated", identity.Path)
		}
		paths[identity.Path] = struct{}{}
		if !canonicalSHA256.MatchString(identity.SHA256) {
			return nil, fmt.Errorf("input %q has a noncanonical SHA-256", identity.ID)
		}
		var maximum int64
		switch identity.Kind {
		case InputCatalogDatabase:
			catalogs++
			maximum = MaxCatalogDatabaseBytes
		case InputResumeReport:
			maximum = int64(ceilings.PreflightReportBytes)
		case InputResumeCheckpoint:
			maximum = MaxResumeCheckpointBytes
		default:
			return nil, fmt.Errorf("input %q has unsupported kind %q", identity.ID, identity.Kind)
		}
		if identity.SizeBytes <= 0 || identity.SizeBytes > maximum {
			return nil, fmt.Errorf("input %q size exceeds its plan hard ceiling", identity.ID)
		}
		if _, exists := byID[identity.ID]; exists {
			return nil, fmt.Errorf("input identity %q is duplicated", identity.ID)
		}
		byID[identity.ID] = identity
	}
	if catalogs != 1 {
		return nil, errors.New("inputs must contain exactly one catalog_database identity")
	}
	return byID, nil
}

func validateSourceSnapshot(snapshot SourceSnapshot) (time.Time, error) {
	if snapshot.Algorithm != SnapshotAlgorithmV1 {
		return time.Time{}, errors.New("source snapshot uses an unsupported digest algorithm")
	}
	capturedAt, err := parseCanonicalTimestamp(snapshot.CapturedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("source snapshot capturedAt: %w", err)
	}
	if snapshot.Files == nil || len(snapshot.Files) == 0 || len(snapshot.Files) > MaxSourceSnapshotFiles {
		return time.Time{}, errors.New("source snapshot has an invalid file count")
	}
	var total int64
	lastPath := ""
	for index, file := range snapshot.Files {
		if !validDataPath(file.Path) || (index > 0 && file.Path <= lastPath) {
			return time.Time{}, errors.New("source snapshot file identities must use unique paths in ascending order")
		}
		lastPath = file.Path
		if file.SizeBytes <= 0 || file.SizeBytes > MaxSourceFileBytes {
			return time.Time{}, fmt.Errorf("source snapshot file %q exceeds its hard ceiling", file.Path)
		}
		if !canonicalSHA256.MatchString(file.SHA256) {
			return time.Time{}, fmt.Errorf("source snapshot file %q has a noncanonical SHA-256", file.Path)
		}
		total += file.SizeBytes
		if total > MaxSourceSnapshotBytes {
			return time.Time{}, errors.New("source snapshot exceeds its total byte ceiling")
		}
	}
	if !canonicalSHA256.MatchString(snapshot.SHA256) {
		return time.Time{}, errors.New("source snapshot has a noncanonical SHA-256")
	}
	digest, err := SourceSnapshotSHA256(snapshot.Files)
	if err != nil {
		return time.Time{}, err
	}
	if digest != snapshot.SHA256 {
		return time.Time{}, errors.New("source snapshot digest does not match its exact file identities")
	}
	return capturedAt, nil
}

func validateProviders(providers ProviderConfiguration, execution ExecutionSettings, createdAt time.Time) error {
	expectedOrder := []Provider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom}
	if providers.Order == nil || len(providers.Order) != len(expectedOrder) {
		return errors.New("provider order must contain the complete compiled provider union")
	}
	for index, provider := range providers.Order {
		if provider != expectedOrder[index] {
			return errors.New("provider order does not match the canonical authoritative order")
		}
	}
	if providers.Configurations == nil || len(providers.Configurations) != len(expectedOrder) {
		return errors.New("provider configurations must contain the complete compiled provider union")
	}

	seenAuthorityIdentity := map[string]struct{}{}
	totalAuthorities := 0
	for index, configured := range providers.Configurations {
		provider := expectedOrder[index]
		if configured.Provider != provider || configured.Provider != providers.Order[index] {
			return errors.New("provider configurations are not aligned with provider order")
		}
		if configured.Origin != providerOrigin(provider) {
			return fmt.Errorf("provider %q has an unapproved origin", provider)
		}
		if configured.Mode != ProviderModeActive {
			return fmt.Errorf("provider %q must remain active in extraction-plan v1", provider)
		}
		if configured.CrawlDelayMillis < execution.SafetyFloors.ProviderCrawlDelayMillis ||
			configured.CrawlDelayMillis > execution.Ceilings.ProviderCrawlDelayMillis ||
			configured.CacheTTLMillis < execution.SafetyFloors.ProviderCacheTTLMillis ||
			configured.CacheTTLMillis > execution.Ceilings.ProviderCacheTTLMillis {
			return fmt.Errorf("provider %q violates plan delay or cache safety bounds", provider)
		}
		if configured.Authorities == nil {
			return fmt.Errorf("provider %q authorities must be an explicit array", provider)
		}
		if len(configured.Authorities) > MaxAuthoritiesPerProvider {
			return fmt.Errorf("provider %q exceeds the fixed-authority ceiling", provider)
		}
		totalAuthorities += len(configured.Authorities)
		if totalAuthorities > MaxAuthorities {
			return errors.New("provider configuration exceeds the total fixed-authority ceiling")
		}
		if !isFixedIndexProvider(provider) {
			if len(configured.Authorities) != 0 {
				return fmt.Errorf("provider %q does not accept fixed authorities", provider)
			}
			continue
		}
		if len(configured.Authorities) == 0 {
			return fmt.Errorf("provider %q requires exactly one active song-index authority", provider)
		}

		activeSongIndexes := 0
		for authorityIndex, authority := range configured.Authorities {
			if authorityIndex == 0 && authority.Disposition != AuthorityActive {
				return fmt.Errorf("provider %q authority order must start with the active song index", provider)
			}
			if authorityIndex > 0 && authority.Disposition != AuthorityRetained {
				return fmt.Errorf("provider %q authorities after the active song index must be retained", provider)
			}
			if err := validateFixedAuthority(provider, authority, execution.Ceilings, createdAt); err != nil {
				return err
			}
			if authority.Disposition == AuthorityActive && authority.Role == AuthorityRoleSongIndex {
				activeSongIndexes++
			}
			identityKey := fmt.Sprintf("%s\x00%d\x00%d", provider, authority.PageID, authority.RevisionID)
			if _, duplicate := seenAuthorityIdentity[identityKey]; duplicate {
				return errors.New("fixed authority provider/page/revision identity is duplicated")
			}
			seenAuthorityIdentity[identityKey] = struct{}{}
		}
		if activeSongIndexes != 1 {
			return fmt.Errorf("provider %q requires exactly one active song-index authority", provider)
		}
	}
	return nil
}

func validateFixedAuthority(provider Provider, authority FixedAuthority, ceilings HardCeilings, createdAt time.Time) error {
	if authority.Disposition != AuthorityActive && authority.Disposition != AuthorityRetained {
		return fmt.Errorf("fixed authority %q has an invalid disposition", authority.EvidenceID)
	}
	if authority.Role != AuthorityRoleSongIndex {
		return fmt.Errorf("fixed authority %q has an unsupported authority role", authority.EvidenceID)
	}
	if authority.PageID <= 0 || authority.PageID > MaxMediaWikiIdentity ||
		authority.RevisionID <= 0 || authority.RevisionID > MaxMediaWikiIdentity ||
		!canonicalSHA1.MatchString(authority.SHA1) {
		return fmt.Errorf("fixed authority %q has an invalid page/revision/SHA-1 identity", authority.EvidenceID)
	}
	if !validBoundedText(authority.Title, ceilings.CandidateTitleBytes) {
		return fmt.Errorf("fixed authority %q has an invalid title", authority.EvidenceID)
	}
	if !utf8.ValidString(authority.CanonicalURL) || len(authority.CanonicalURL) > ceilings.CandidateURLBytes ||
		authority.CanonicalURL != FixedAuthorityCanonicalURL(provider, authority.Title, authority.RevisionID) {
		return fmt.Errorf("fixed authority %q has a noncanonical provider URL", authority.EvidenceID)
	}
	expectedEvidenceID, err := FixedAuthorityEvidenceID(
		provider, authority.Role, authority.PageID, authority.RevisionID, authority.Title,
	)
	if err != nil || !validEvidenceID(authority.EvidenceID) || authority.EvidenceID != expectedEvidenceID {
		return fmt.Errorf("fixed authority %q has a noncanonical derived evidence ID", authority.EvidenceID)
	}

	switch authority.CaptureProfile {
	case CaptureProfileMediaWikiAPIRevisionResponseV1:
		if provider != ProviderSekaipedia || !canonicalSHA256.MatchString(authority.RawSHA256) ||
			(authority.ContentSHA256 != "" && !canonicalSHA256.MatchString(authority.ContentSHA256)) ||
			authority.RevisionTimestamp == "" {
			return fmt.Errorf("fixed authority %q does not match its API-response capture profile", authority.EvidenceID)
		}
		revisionTimestamp, err := parseCanonicalTimestamp(authority.RevisionTimestamp)
		if err != nil {
			return fmt.Errorf("fixed authority %q revisionTimestamp: %w", authority.EvidenceID, err)
		}
		if revisionTimestamp.After(createdAt) {
			return fmt.Errorf("fixed authority %q revisionTimestamp is after plan createdAt", authority.EvidenceID)
		}
	case CaptureProfileMediaWikiRevisionContentV1:
		if provider != ProviderMoegirl || authority.RevisionTimestamp != "" || authority.ContentSHA256 != "" || authority.RawSHA256 != "" {
			return fmt.Errorf("fixed authority %q does not match its revision-content capture profile", authority.EvidenceID)
		}
	default:
		return fmt.Errorf("fixed authority %q has an unsupported capture profile", authority.EvidenceID)
	}
	return nil
}

func isFixedIndexProvider(provider Provider) bool {
	return provider == ProviderSekaipedia || provider == ProviderMoegirl
}

// FixedAuthorityCanonicalURL derives the provider's exact immutable revision
// URL. Mutable authority URLs belong in plans and are accepted only when they
// equal this algorithm's output.
func FixedAuthorityCanonicalURL(provider Provider, title string, revisionID int) string {
	switch provider {
	case ProviderSekaipedia:
		canonical := url.URL{
			Scheme: "https", Host: "www.sekaipedia.org", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_"),
		}
		query := canonical.Query()
		query.Set("oldid", strconv.Itoa(revisionID))
		canonical.RawQuery = query.Encode()
		return canonical.String()
	case ProviderMoegirl:
		canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
		query := canonical.Query()
		query.Set("oldid", strconv.Itoa(revisionID))
		query.Set("title", title)
		canonical.RawQuery = query.Encode()
		return canonical.String()
	default:
		return ""
	}
}

// FixedAuthorityEvidenceID derives the historical evidence base ID used by the
// existing fixed-index acquisition boundary. Each retained plan authority is
// therefore checked from its own immutable fields instead of a compiled value.
func FixedAuthorityEvidenceID(
	provider Provider,
	role AuthorityRole,
	pageID, revisionID int,
	title string,
) (string, error) {
	if role != AuthorityRoleSongIndex || pageID <= 0 || revisionID <= 0 {
		return "", errors.New("fixed authority evidence identity is unsupported")
	}
	var evidenceID string
	switch provider {
	case ProviderSekaipedia:
		titleKey, ok := authorityTitleEvidenceKey(title)
		if !ok {
			return "", errors.New("Sekaipedia authority title cannot form a canonical evidence identity")
		}
		evidenceID = fmt.Sprintf("authority:sekaipedia:%s:%d", titleKey, revisionID)
	case ProviderMoegirl:
		evidenceID = fmt.Sprintf("search:moegirl:%d", pageID)
	default:
		return "", errors.New("provider does not support fixed authority evidence identities")
	}
	if !validEvidenceID(evidenceID) {
		return "", errors.New("derived fixed authority evidence identity exceeds its canonical boundary")
	}
	return evidenceID, nil
}

func authorityTitleEvidenceKey(title string) (string, bool) {
	if !validBoundedText(title, MaxAuthorityTitleBytes) {
		return "", false
	}
	var result strings.Builder
	separatorPending := false
	for _, current := range title {
		switch {
		case current >= 'A' && current <= 'Z':
			if separatorPending && result.Len() > 0 {
				result.WriteByte('-')
			}
			separatorPending = false
			result.WriteByte(byte(current - 'A' + 'a'))
		case current >= 'a' && current <= 'z' || current >= '0' && current <= '9':
			if separatorPending && result.Len() > 0 {
				result.WriteByte('-')
			}
			separatorPending = false
			result.WriteRune(current)
		default:
			separatorPending = result.Len() > 0
		}
		if result.Len() > MaxIdentityBytes {
			return "", false
		}
	}
	return result.String(), result.Len() > 0
}

func validateEffectiveVersions(versions EffectiveVersions) error {
	registered := CompiledEffectiveVersions()
	if !reflect.DeepEqual(versions.Schemas, registered.Schemas) {
		return errors.New("effective schema versions do not match extraction-plan v1")
	}
	if !reflect.DeepEqual(versions.Policies, registered.Policies) {
		return errors.New("effective policy versions are not registered by this binary")
	}
	if !reflect.DeepEqual(versions.Parsers, registered.Parsers) &&
		!reflect.DeepEqual(versions.Parsers, historicalEffectiveVersionsV1().Parsers) {
		return errors.New("effective parser or ruby versions are not registered by this binary")
	}
	if !reflect.DeepEqual(versions.Algorithms, registered.Algorithms) {
		return errors.New("effective algorithm versions are not registered by this binary")
	}
	return nil
}

func validateExecution(execution ExecutionSettings) error {
	if err := validateHardCeilings(execution.Ceilings); err != nil {
		return err
	}
	if err := validateSafetyFloors(execution.SafetyFloors, execution.Ceilings); err != nil {
		return err
	}
	if execution.Concurrency < 1 || execution.Concurrency > execution.Ceilings.Concurrency ||
		execution.MaxAttempts < 1 || execution.MaxAttempts > execution.Ceilings.Attempts ||
		execution.RequestTimeoutMillis < execution.SafetyFloors.RequestTimeoutMillis ||
		execution.RequestTimeoutMillis > execution.Ceilings.RequestTimeoutMillis ||
		execution.RetryDelayMillis < execution.SafetyFloors.RetryDelayMillis ||
		execution.RetryDelayMillis > execution.Ceilings.RetryDelayMillis {
		return errors.New("typed execution settings violate plan bounds")
	}
	return nil
}

func validateHardCeilings(ceilings HardCeilings) error {
	compiled := CompiledHardCeilings()
	intBounds := []struct {
		name       string
		value, max int
	}{
		{"concurrency", ceilings.Concurrency, compiled.Concurrency},
		{"attempts", ceilings.Attempts, compiled.Attempts},
		{"catalogRecords", ceilings.CatalogRecords, compiled.CatalogRecords},
		{"catalogJsonBytes", ceilings.CatalogJSONBytes, compiled.CatalogJSONBytes},
		{"providerResponseBytes", ceilings.ProviderResponseBytes, compiled.ProviderResponseBytes},
		{"searchPages", ceilings.SearchPages, compiled.SearchPages},
		{"reportCandidates", ceilings.ReportCandidates, compiled.ReportCandidates},
		{"candidateTitleBytes", ceilings.CandidateTitleBytes, compiled.CandidateTitleBytes},
		{"candidateUrlBytes", ceilings.CandidateURLBytes, compiled.CandidateURLBytes},
		{"candidateCategoryBytes", ceilings.CandidateCategoryBytes, compiled.CandidateCategoryBytes},
		{"extractedLines", ceilings.ExtractedLines, compiled.ExtractedLines},
		{"extractedLineBytes", ceilings.ExtractedLineBytes, compiled.ExtractedLineBytes},
		{"extractedTextBytes", ceilings.ExtractedTextBytes, compiled.ExtractedTextBytes},
		{"indexEvidenceRawBytes", ceilings.IndexEvidenceRawBytes, compiled.IndexEvidenceRawBytes},
		{"fixedArtifacts", ceilings.FixedArtifacts, compiled.FixedArtifacts},
		{"preflightReportBytes", ceilings.PreflightReportBytes, compiled.PreflightReportBytes},
		{"evidenceReceiptBytes", ceilings.EvidenceReceiptBytes, compiled.EvidenceReceiptBytes},
		{"evidenceReceiptRawBytes", ceilings.EvidenceReceiptRawBytes, compiled.EvidenceReceiptRawBytes},
		{"lyricsSourceDocumentBytes", ceilings.LyricsSourceDocumentBytes, compiled.LyricsSourceDocumentBytes},
		{"lyricsSourceJsonDepth", ceilings.LyricsSourceJSONDepth, compiled.LyricsSourceJSONDepth},
	}
	for _, bound := range intBounds {
		if bound.value < 1 || bound.value > bound.max {
			return fmt.Errorf("execution hard ceiling %s exceeds the compiled safety envelope", bound.name)
		}
	}
	int64Bounds := []struct {
		name       string
		value, max int64
	}{
		{"requestTimeoutMillis", ceilings.RequestTimeoutMillis, compiled.RequestTimeoutMillis},
		{"retryDelayMillis", ceilings.RetryDelayMillis, compiled.RetryDelayMillis},
		{"providerCrawlDelayMillis", ceilings.ProviderCrawlDelayMillis, compiled.ProviderCrawlDelayMillis},
		{"providerCacheTtlMillis", ceilings.ProviderCacheTTLMillis, compiled.ProviderCacheTTLMillis},
	}
	for _, bound := range int64Bounds {
		if bound.value < 1 || bound.value > bound.max {
			return fmt.Errorf("execution hard ceiling %s exceeds the compiled safety envelope", bound.name)
		}
	}
	if ceilings.EvidenceReceiptRawBytes > ceilings.EvidenceReceiptBytes ||
		ceilings.EvidenceReceiptBytes > ceilings.PreflightReportBytes ||
		ceilings.ExtractedLineBytes > ceilings.ExtractedTextBytes ||
		ceilings.IndexEvidenceRawBytes > ceilings.ProviderResponseBytes {
		return errors.New("execution hard ceilings are internally inconsistent")
	}
	return nil
}

func validateSafetyFloors(floors SafetyFloors, ceilings HardCeilings) error {
	compiled := CompiledSafetyFloors()
	if floors.RequestTimeoutMillis < compiled.RequestTimeoutMillis || floors.RequestTimeoutMillis > ceilings.RequestTimeoutMillis ||
		floors.RetryDelayMillis < compiled.RetryDelayMillis || floors.RetryDelayMillis > ceilings.RetryDelayMillis ||
		floors.ProviderCrawlDelayMillis < compiled.ProviderCrawlDelayMillis ||
		floors.ProviderCrawlDelayMillis > ceilings.ProviderCrawlDelayMillis ||
		floors.ProviderCacheTTLMillis < compiled.ProviderCacheTTLMillis ||
		floors.ProviderCacheTTLMillis > ceilings.ProviderCacheTTLMillis {
		return errors.New("execution safety floors violate the compiled safety envelope or plan ceilings")
	}
	return nil
}

func validateResume(resume ResumePolicy, inputs map[string]InputIdentity) error {
	if resume.RetryErrorCodes == nil || resume.RetryMissingReasons == nil || resume.RetryIncompleteCodes == nil {
		return errors.New("resume code selections must be explicit arrays")
	}
	if err := validateOrderedSubset("resume error code", resume.RetryErrorCodes, safeResumeErrorCodes); err != nil {
		return err
	}
	if err := validateOrderedSubset("resume missing reason", resume.RetryMissingReasons, safeResumeMissingReasons); err != nil {
		return err
	}
	if err := validateOrderedSubset("resume incomplete code", resume.RetryIncompleteCodes, safeResumeIncompleteCodes); err != nil {
		return err
	}
	resumeInputs := 0
	for _, input := range inputs {
		if input.Kind != InputCatalogDatabase {
			resumeInputs++
		}
	}
	emptySelections := len(resume.RetryErrorCodes) == 0 && len(resume.RetryMissingReasons) == 0 &&
		len(resume.RetryIncompleteCodes) == 0 && !resume.RevalidateUniqueComplete
	switch resume.Mode {
	case ResumeFresh:
		if resume.InputID != "" || !emptySelections || resumeInputs != 0 {
			return errors.New("fresh resume policy must not carry a resume input or retry selections")
		}
	case ResumeReport:
		input, ok := inputs[resume.InputID]
		if !validID(resume.InputID) || !ok || input.Kind != InputResumeReport || resumeInputs != 1 {
			return errors.New("report resume policy must reference the sole resume_report input")
		}
		if emptySelections {
			return errors.New("report resume policy must select at least one safe retry or revalidation action")
		}
	case ResumeCheckpoint:
		input, ok := inputs[resume.InputID]
		if !validID(resume.InputID) || !ok || input.Kind != InputResumeCheckpoint || resumeInputs != 1 || !emptySelections {
			return errors.New("checkpoint resume policy must reference the sole checkpoint without report retry selections")
		}
	default:
		return fmt.Errorf("unsupported resume mode %q", resume.Mode)
	}
	return nil
}

func validateOutputs(outputs []OutputIdentity, inputs []InputIdentity, sourceFiles []SourceFileIdentity) error {
	if outputs == nil || len(outputs) != 3 {
		return errors.New("outputs must explicitly contain preflight report, staging manifest, and evidence receipt")
	}
	wantKinds := []OutputKind{OutputPreflightReport, OutputStagingManifest, OutputEvidenceReceipt}
	occupied := make(map[string]string, len(inputs)+len(sourceFiles)+len(outputs))
	for _, input := range inputs {
		occupied[input.Path] = "input"
	}
	for _, file := range sourceFiles {
		if previous := occupied[file.Path]; previous != "" {
			return fmt.Errorf("path %q duplicates a %s identity", file.Path, previous)
		}
		occupied[file.Path] = "source snapshot"
	}
	for index, output := range outputs {
		if output.Kind != wantKinds[index] || !validDataPath(output.Path) ||
			output.Publication != OutputPublicationCreateExclusive ||
			output.Confidentiality != OutputConfidentialityPrivate || output.FileMode != PrivateOutputFileMode {
			return errors.New("output identities do not match the closed private create-exclusive contract")
		}
		if previous := occupied[output.Path]; previous != "" {
			return fmt.Errorf("output path %q aliases a %s identity", output.Path, previous)
		}
		occupied[output.Path] = "output"
	}
	return nil
}

func validateOrderedSubset(name string, selected, allowed []string) error {
	positions := make(map[string]int, len(allowed))
	for index, value := range allowed {
		positions[value] = index
	}
	last := -1
	for _, value := range selected {
		position, ok := positions[value]
		if !ok {
			return fmt.Errorf("%s %q is not approved", name, value)
		}
		if position <= last {
			return fmt.Errorf("%ss must be unique and in canonical order", name)
		}
		last = position
	}
	return nil
}

func providerOrigin(provider Provider) string {
	switch provider {
	case ProviderSekaipedia:
		return OriginSekaipedia
	case ProviderMoegirl:
		return OriginMoegirl
	case ProviderVocaloidFandom:
		return OriginVocaloidFandom
	default:
		return ""
	}
}

func parseCanonicalTimestamp(value string) (time.Time, error) {
	if value == "" || len(value) > 64 || !utf8.ValidString(value) {
		return time.Time{}, errors.New("timestamp must be canonical UTC RFC3339Nano")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Unix() <= 0 || !strings.HasSuffix(value, "Z") || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("timestamp must be canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func validID(value string) bool {
	return len(value) <= MaxIdentityBytes && utf8.ValidString(value) && canonicalID.MatchString(value)
}

func validEvidenceID(value string) bool {
	return len(value) <= MaxEvidenceIDBytes && utf8.ValidString(value) && canonicalEvidenceID.MatchString(value)
}

func validBoundedText(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return false
		}
	}
	return true
}

func validDataPath(value string) bool {
	if value == "" || len(value) > MaxPathBytes || !utf8.ValidString(value) || containsShellFragment(value) ||
		!canonicalPath.MatchString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "//") ||
		path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
