package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscompose"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsextractionplan"
	"moesekai/server/internal/lyricsoutcomeartifact"
	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricsproviderpolicy"
	"moesekai/server/internal/lyricsrecovery"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

const (
	releaseCatalogPath              = "/private/tmp/moesekai-external-runtime/today-release-20260803/sekaipedia-url-preflight-20260803T130023Z-37903/canonical-catalog-698-v1/catalog.db"
	releaseCatalogSHA256            = "5d1d0e13aeb5a3033197a7242c1076dab6dd7b3956684ee066e99a97527d1607"
	releaseCatalogTargetCount       = 698
	releaseCatalogMusicIDsSHA256    = "238a220613adb5dec06e9e28ff58358879557f0b6101b672ad69eaf57d5cb4d5"
	releaseExactMoegirlURL          = "https://zh.moegirl.org.cn/%E4%BA%BF%E5%B9%B4%E7%88%B1%E6%81%8B"
	releaseExactMoegirlRawPath      = "/private/tmp/moesekai-external-runtime/today-release-20260803/sekaipedia-url-preflight-20260803T130023Z-37903/moegirl-795-proxy-url-preflight/response.html"
	releaseExactMoegirlReportPath   = "/private/tmp/moesekai-external-runtime/today-release-20260803/sekaipedia-url-preflight-20260803T130023Z-37903/moegirl-795-public-html-extraction-v1/report.json"
	releaseExactMoegirlRawSHA256    = "7ef2eda347b9e57f0fd2f3d6912bc4183158173b74f2a2e6fa1eb88083bcd6be"
	releaseExactMoegirlReportSHA256 = "b02112ed132ca06293c64ad939020b6196ca77d1f8541b8a2f4e821b0c8d76ec"
	releaseRequiredMediaWikiMaxlag  = 5
	releaseRequiredProviderInFlight = 1
)

var compactRootForbiddenFields = map[string]struct{}{
	"title": {}, "lyrics": {}, "text": {}, "raw": {}, "translation": {},
	"romaji": {}, "romanization": {}, "romanized": {}, "path": {}, "timestamp": {},
}

type freshOptions struct {
	SourceRoot            string
	PlanPath              string
	PlanSHA256            string
	CatalogPath           string
	LedgerPath            string
	AcquisitionSetPath    string
	ProviderOutcomesPath  string
	SongResultsPath       string
	EvidencePackPath      string
	RootManifestPath      string
	ImportManifestPath    string
	ImportEvidencePath    string
	ValidationReceiptPath string
}

type freshResult struct {
	RootSHA256              string
	CatalogTargets          int
	Complete                int
	AcquisitionCount        int
	EvidenceCount           int
	ShardCount              int
	ImportBatchSHA256       string
	ValidationReceiptSHA256 string
}

func runValidateFresh(ctx context.Context, arguments []string) (freshResult, error) {
	var opts freshOptions
	flags := flag.NewFlagSet("validate-fresh", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.SourceRoot, "source-root", "", "exact source root bound by the recovery plan")
	flags.StringVar(&opts.PlanPath, "plan", "", "canonical recovery plan")
	flags.StringVar(&opts.PlanSHA256, "plan-sha256", "", "expected canonical recovery plan SHA-256")
	flags.StringVar(&opts.CatalogPath, "catalog", releaseCatalogPath, "reviewed immutable 698 catalog")
	flags.StringVar(&opts.LedgerPath, "ledger", "", "fresh acquisition ledger")
	flags.StringVar(&opts.AcquisitionSetPath, "acquisition-set", "", "fresh exact acquisition set")
	flags.StringVar(&opts.ProviderOutcomesPath, "provider-outcomes", "", "fresh provider-outcome directory")
	flags.StringVar(&opts.SongResultsPath, "song-results", "", "fresh song-result directory")
	flags.StringVar(&opts.EvidencePackPath, "evidence-pack", "", "fresh evidence-pack directory")
	flags.StringVar(&opts.RootManifestPath, "root-manifest", "", "fresh compact root manifest")
	flags.StringVar(&opts.ImportManifestPath, "import-manifest", "", "closed staging manifest prepared from the fresh root")
	flags.StringVar(&opts.ImportEvidencePath, "import-evidence-receipt", "", "private evidence receipt for the import manifest")
	flags.StringVar(&opts.ValidationReceiptPath, "validation-receipt", "", "new immutable content-addressed validation receipt")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return freshResult{}, errors.New("validate-fresh requires only explicit named flags")
	}
	if err := validateFreshOptions(opts); err != nil {
		return freshResult{}, err
	}
	return validateFresh(ctx, opts)
}

func validateCompiledProviderSafetyContract() error {
	if lyricsproviderpolicy.MediaWikiMaxlagV1 != releaseRequiredMediaWikiMaxlag ||
		lyricsproviderpolicy.DefaultMaxActualNetworkInFlightV1 != releaseRequiredProviderInFlight ||
		lyricsproviderpolicy.MaxAcquirerProcessesV1 != 1 ||
		lyricsproviderpolicy.DefaultActionBatchCeilingV1 != 1 ||
		lyricsproviderpolicy.ConcurrencyAccountingActualNetworkRequests != "actual_network_requests" ||
		lyricsproviderpolicy.CrossProcessCoordinationRetainedGlobalLock != "retained_global_live_acquisition_lock" ||
		lyricsproviderpolicy.RetryAfterRuleNeverShorten != "at_least_header_never_shorten" ||
		lyricsproviderpolicy.CooldownScopeProviderWide != "provider_wide" ||
		lyricsproviderpolicy.CooldownPersistencePersistent != "persistent" ||
		lyricsproviderpolicy.MaxlagClassificationProviderOverload != "retryable_provider_overload" ||
		lyricsproviderpolicy.BackoffModeExponentialExtendOnly != "exponential_extend_only" ||
		lyricsproviderpolicy.RuleProhibited != "prohibited" {
		return errors.New("compiled provider safety contract does not retain maxlag, complete Retry-After, global serialization, and no-evasion rules")
	}
	specs := lyricsproviderpolicy.CompiledProviderSpecsV1()
	if len(specs) != 3 {
		return errors.New("compiled provider safety contract does not contain the exact three providers")
	}
	return nil
}

func validateFreshOptions(opts freshOptions) error {
	if !lowerSHA256Pattern.MatchString(opts.PlanSHA256) {
		return errors.New("-plan-sha256 must be a canonical lowercase SHA-256")
	}
	paths := []string{
		opts.SourceRoot, opts.PlanPath, opts.CatalogPath, opts.LedgerPath, opts.AcquisitionSetPath,
		opts.ProviderOutcomesPath, opts.SongResultsPath, opts.EvidencePackPath, opts.RootManifestPath,
		opts.ImportManifestPath, opts.ImportEvidencePath, opts.ValidationReceiptPath,
	}
	for _, path := range paths {
		if !canonicalAbsolutePath(path) {
			return errors.New("validate-fresh paths must be explicit canonical absolute paths")
		}
	}
	if opts.CatalogPath != releaseCatalogPath {
		return errors.New("validate-fresh accepts only the reviewed immutable 698 catalog path")
	}
	return nil
}

func validateFresh(ctx context.Context, opts freshOptions) (result freshResult, returnErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	planBody, planFileSHA256, err := readPinnedRegular(opts.PlanPath, "recovery plan", lyricsextractionplan.MaxPlanBytes, 0o600)
	if err != nil {
		return result, err
	}
	plan, planSHA, err := lyricsextractionplan.CheckRecovery(planBody, opts.PlanSHA256)
	if err != nil {
		return result, err
	}
	if err := validateReleasePlan(plan, planSHA, opts); err != nil {
		return result, err
	}
	if err := lyricsextractionplan.VerifyRecoverySourceSnapshot(opts.SourceRoot, plan); err != nil {
		return result, err
	}
	runtime, err := lyricsrecovery.RuntimeConfigFromPlan(plan)
	if err != nil {
		return result, err
	}
	if runtime.MediaWikiMaxlag != releaseRequiredMediaWikiMaxlag ||
		runtime.MaxActualNetworkInFlight != releaseRequiredProviderInFlight {
		return result, errors.New("recovery plan does not retain maxlag=5 and one actual provider request in flight")
	}
	if err := validateCompiledProviderSafetyContract(); err != nil {
		return result, err
	}
	catalog, verification, err := lyricsrecovery.OpenCatalogAgainstPlan(ctx, opts.CatalogPath, plan.Catalog)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, catalog.Close()) }()
	if verification.RecordCount != releaseCatalogTargetCount || verification.SourceSHA256 != releaseCatalogSHA256 ||
		verification.MusicIDsSHA256 != releaseCatalogMusicIDsSHA256 ||
		!reflect.DeepEqual(catalog.MusicIDs(), plan.Scope.MusicIDs) {
		return result, errors.New("reviewed catalog verification does not equal the exact ordered 698 plan scope")
	}

	setBody, setFileSHA256, err := readPinnedRegular(opts.AcquisitionSetPath, "acquisition set", lyricsrecovery.MaxAcquisitionSetBytes, 0o600)
	if err != nil {
		return result, err
	}
	set, err := lyricsrecovery.DecodeAcquisitionSet(setBody)
	if err != nil {
		return result, err
	}
	if err := lyricsrecovery.ValidateAcquisitionSetAuthorization(
		set, plan.PlanID, planSHA, plan.Scope.MusicIDs, runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		return result, err
	}

	rootBody, rootFileSHA256, err := readPinnedRegular(opts.RootManifestPath, "root manifest", maxFreshRootBytes, 0o600)
	if err != nil {
		return result, err
	}
	if err := rejectJSONKeys(rootBody, compactRootForbiddenFields, "compact root manifest"); err != nil {
		return result, err
	}
	root, err := lyricsrootmanifest.DecodeCanonical(rootBody)
	if err != nil {
		return result, err
	}
	if err := validateReleaseRoot(root, plan, planSHA); err != nil {
		return result, err
	}
	resolver, err := lyricsevidencepack.OpenResolver(opts.EvidencePackPath)
	if err != nil {
		return result, err
	}
	if err := lyricsrootmanifest.ValidateAgainstPack(root, resolver); err != nil {
		return result, err
	}

	ledger, err := lyricsacquisition.OpenLedger(ctx, opts.LedgerPath)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, ledger.Close()) }()

	results, acquisitionIDs, err := validateFreshSongs(ctx, plan, runtime, catalog, ledger, set, root, opts)
	if err != nil {
		return result, err
	}
	if err := exactLedgerManifestEntries(opts.LedgerPath, acquisitionIDs); err != nil {
		return result, err
	}
	catalogFile, err := validationFileFromPath(opts.CatalogPath, "reviewed release catalog", maxCiphertextBytes, 0o444)
	if err != nil {
		return result, err
	}
	if catalogFile.SHA256 != verification.SourceSHA256 || catalogFile.ByteCount != verification.SizeBytes {
		return result, errors.New("reviewed release catalog changed during validation")
	}
	manifestBody, manifestFileSHA256, err := readPinnedRegular(opts.ImportManifestPath, "import manifest", lyricsstaging.MaxManifestBytes, 0o600)
	if err != nil {
		return result, err
	}
	manifest, err := lyricsstaging.DecodeManifest(manifestBody)
	if err != nil {
		return result, err
	}
	canonicalManifest, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil || !bytes.Equal(canonicalManifest, manifestBody) {
		return result, errors.New("import manifest is not the canonical producer encoding")
	}
	evidenceBody, evidenceFileSHA256, err := readPinnedRegular(opts.ImportEvidencePath, "import evidence receipt", lyricsstaging.MaxPrivateEvidenceReceiptBytes, 0o600)
	if err != nil {
		return result, err
	}
	importEvidence, err := lyricsstaging.DecodePrivateEvidenceReceipt(evidenceBody)
	if err != nil {
		return result, err
	}
	canonicalEvidence, err := lyricsstaging.MarshalPrivateEvidenceReceipt(importEvidence)
	if err != nil || !bytes.Equal(canonicalEvidence, evidenceBody) {
		return result, errors.New("import evidence receipt is not the canonical producer encoding")
	}
	if err := validateImportInputsAgainstFreshRoot(manifest, importEvidence, root, results); err != nil {
		return result, err
	}
	if err := lyricsextractionplan.VerifyRecoverySourceSnapshot(opts.SourceRoot, plan); err != nil {
		return result, fmt.Errorf("reverify recovery source snapshot before receipt publication: %w", err)
	}

	ledgerBinding, err := hashPrivateTree(opts.LedgerPath, "acquisition ledger")
	if err != nil {
		return result, err
	}
	providerOutcomesBinding, err := hashPrivateTree(opts.ProviderOutcomesPath, "provider outcomes")
	if err != nil {
		return result, err
	}
	songResultsBinding, err := hashPrivateTree(opts.SongResultsPath, "song results")
	if err != nil {
		return result, err
	}
	evidencePackBinding, err := hashPrivateTree(opts.EvidencePackPath, "evidence pack")
	if err != nil {
		return result, err
	}
	validationReceipt, err := buildReleaseValidationReceipt(
		opts, plan, planFileSHA256, int64(len(planBody)), verification.SizeBytes,
		set, setFileSHA256, int64(len(setBody)), root, rootFileSHA256, int64(len(rootBody)),
		manifest, manifestFileSHA256, int64(len(manifestBody)), importEvidence, evidenceFileSHA256,
		int64(len(evidenceBody)), ledgerBinding, providerOutcomesBinding, songResultsBinding,
		evidencePackBinding, len(acquisitionIDs),
	)
	if err != nil {
		return result, err
	}
	if err := publishReleaseValidationReceipt(validationReceipt); err != nil {
		return result, err
	}

	result = freshResult{
		RootSHA256: root.RootSHA256, CatalogTargets: root.Coverage.Total, Complete: root.Coverage.Complete,
		AcquisitionCount: len(acquisitionIDs), EvidenceCount: root.Coverage.UniqueEvidenceCount,
		ShardCount: resolver.ValidatedShardCount(), ImportBatchSHA256: manifest.BatchSHA256,
		ValidationReceiptSHA256: validationReceipt.ReceiptSHA256,
	}
	return result, nil
}

func validateReleasePlan(plan lyricsextractionplan.RecoveryPlan, planSHA string, opts freshOptions) error {
	if planSHA != opts.PlanSHA256 || plan.Catalog.Path != releaseCatalogPath ||
		plan.Catalog.SourceSHA256 != releaseCatalogSHA256 || plan.Catalog.RecordCount != releaseCatalogTargetCount ||
		plan.Catalog.MusicIDsSHA256 != releaseCatalogMusicIDsSHA256 ||
		plan.Scope.Kind != lyricsextractionplan.RecoveryScopeFinal || len(plan.Scope.MusicIDs) != releaseCatalogTargetCount {
		return errors.New("recovery plan is not the reviewed final 698 release scope")
	}
	if err := validateReleaseProviderScopes(plan); err != nil {
		return err
	}
	orderedDigest, err := lyricscontract.OrderedMusicIDsSHA256(plan.Scope.MusicIDs)
	if err != nil || orderedDigest != releaseCatalogMusicIDsSHA256 {
		return errors.New("recovery plan ordered 698 catalog digest does not match the reviewed release digest")
	}
	if plan.Outputs.Ledger != opts.LedgerPath || plan.Outputs.AcquisitionSet != opts.AcquisitionSetPath ||
		plan.Outputs.ProviderOutcomes != opts.ProviderOutcomesPath || plan.Outputs.SongResults != opts.SongResultsPath ||
		plan.Outputs.EvidencePack != opts.EvidencePackPath || plan.Outputs.RootManifest != opts.RootManifestPath {
		return errors.New("validator paths do not exactly match the immutable recovery plan outputs")
	}
	return nil
}

func validateReleaseProviderScopes(plan lyricsextractionplan.RecoveryPlan) error {
	wantOrder := []lyricsextractionplan.Provider{
		lyricsextractionplan.ProviderSekaipedia,
		lyricsextractionplan.ProviderMoegirlPublicExact,
	}
	if !reflect.DeepEqual(plan.Providers.Order, wantOrder) || len(plan.Providers.Configurations) != 2 {
		return errors.New("release provider order must be Sekaipedia plus one offline exact-public Moegirl exception")
	}
	wantSekaipedia := make([]int, 0, releaseCatalogTargetCount-1)
	found795 := false
	for _, musicID := range plan.Scope.MusicIDs {
		if musicID == 795 {
			found795 = true
			continue
		}
		wantSekaipedia = append(wantSekaipedia, musicID)
	}
	if !found795 || len(wantSekaipedia) != releaseCatalogTargetCount-1 {
		return errors.New("release scope does not contain exactly one music ID 795 exception")
	}
	sekaipedia := plan.Providers.Configurations[0]
	exact := plan.Providers.Configurations[1]
	if sekaipedia.Provider != lyricsextractionplan.ProviderSekaipedia ||
		!reflect.DeepEqual(sekaipedia.MusicIDs, wantSekaipedia) ||
		len(sekaipedia.SekaipediaTargets) != len(wantSekaipedia) || len(sekaipedia.ExactPublicTargets) != 0 {
		return errors.New("release Sekaipedia provider scope must equal the other 697 catalog songs")
	}
	if exact.Provider != lyricsextractionplan.ProviderMoegirlPublicExact ||
		!reflect.DeepEqual(exact.MusicIDs, []int{795}) || len(exact.Authorities) != 0 ||
		len(exact.ContributorAliases) != 0 || len(exact.SekaipediaTargets) != 0 ||
		len(exact.ExactPublicTargets) != 1 {
		return errors.New("release exact-public provider must authorize only music ID 795 and no API authority")
	}
	target := exact.ExactPublicTargets[0]
	if target.MusicID != 795 || target.PageURL != releaseExactMoegirlURL || target.PageTitle != "亿年爱恋" ||
		target.JapaneseTitle != "一億年恋してる" || target.PageID != 649688 || target.RevisionID != 8500224 ||
		target.FetchedAt != "2026-08-03T14:58:50.501307Z" ||
		target.RawHTML.Path != releaseExactMoegirlRawPath || target.RawHTML.SizeBytes != 128236 ||
		target.RawHTML.SHA256 != releaseExactMoegirlRawSHA256 ||
		target.ExtractionReport.Path != releaseExactMoegirlReportPath || target.ExtractionReport.SizeBytes != 6344 ||
		target.ExtractionReport.SHA256 != releaseExactMoegirlReportSHA256 {
		return errors.New("release music ID 795 is not bound to the reviewed complete zh.moegirl.org.cn URL and exact artifacts")
	}
	return nil
}

func validateReleaseRoot(root lyricsrootmanifest.Manifest, plan lyricsextractionplan.RecoveryPlan, planSHA string) error {
	if root.Scope.Kind != lyricsrootmanifest.ScopeFinal || root.Catalog.RecordCount != releaseCatalogTargetCount ||
		root.Catalog.SourceSHA256 != releaseCatalogSHA256 || root.Catalog.MusicIDsSHA256 != releaseCatalogMusicIDsSHA256 ||
		root.Plan.PlanID != plan.PlanID || root.Plan.SHA256 != planSHA || len(root.Songs) != releaseCatalogTargetCount ||
		root.Coverage.Total != releaseCatalogTargetCount || root.Coverage.Complete != releaseCatalogTargetCount ||
		root.Coverage.CatalogReview != 0 || root.Coverage.GameSizeEvidence != 0 || root.Coverage.Ambiguous != 0 ||
		root.Coverage.Missing != 0 || root.Coverage.Incomplete != 0 || root.Coverage.Failed != 0 {
		return errors.New("fresh release root must be final, compact, and complete for exactly 698 catalog targets")
	}
	for index, song := range root.Songs {
		if song.MusicID != plan.Scope.MusicIDs[index] || song.State != lyricscontract.CoverageComplete {
			return errors.New("fresh release root songs do not exactly match the ordered 698 plan scope")
		}
	}
	return nil
}

func validateFreshSongs(
	ctx context.Context,
	plan lyricsextractionplan.RecoveryPlan,
	runtime lyricsrecovery.RuntimeConfig,
	catalog *lyricsrecovery.Catalog,
	ledger *lyricsacquisition.Ledger,
	set lyricsrecovery.AcquisitionSet,
	root lyricsrootmanifest.Manifest,
	opts freshOptions,
) (map[int]lyricsrecovery.SongResult, map[string]struct{}, error) {
	expectedOutcomes := make(map[string]struct{}, root.Coverage.ProviderOutcomeRefCount)
	expectedResults := make(map[string]struct{}, len(root.Songs))
	for _, song := range root.Songs {
		expectedResults[fmt.Sprintf("music-%d-%s.json", song.MusicID, song.ResultSHA256)] = struct{}{}
		for _, outcome := range song.ProviderOutcomes {
			expectedOutcomes[fmt.Sprintf("music-%d-%s-%s.json", song.MusicID, outcome.Provider, outcome.SHA256)] = struct{}{}
		}
	}
	if err := exactRegularDirectoryEntries(opts.ProviderOutcomesPath, "provider outcomes", expectedOutcomes); err != nil {
		return nil, nil, err
	}
	if err := exactRegularDirectoryEntries(opts.SongResultsPath, "song results", expectedResults); err != nil {
		return nil, nil, err
	}

	results := make(map[int]lyricsrecovery.SongResult, len(root.Songs))
	acquisitionIDs := make(map[string]struct{})
	for index, rootSong := range root.Songs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		resultPath := filepath.Join(opts.SongResultsPath, fmt.Sprintf("music-%d-%s.json", rootSong.MusicID, rootSong.ResultSHA256))
		publishedResult, err := lyricsrecovery.OpenSongResult(resultPath)
		if err != nil {
			return nil, nil, err
		}
		rootRef, err := lyricsrecovery.RootSongRef(publishedResult)
		if err != nil || !reflect.DeepEqual(rootRef, rootSong) || publishedResult.Full == nil {
			return nil, nil, fmt.Errorf("music %d published song result does not exactly match the compact root", rootSong.MusicID)
		}
		if err := lyricscontract.ValidatePersistedPerformerMetadata(*publishedResult.Full); err != nil {
			return nil, nil, fmt.Errorf("music %d persisted performer metadata is unsafe", rootSong.MusicID)
		}
		orderedProviders, err := set.OrderedProviders(rootSong.MusicID)
		if err != nil {
			return nil, nil, err
		}
		if len(rootSong.ProviderOutcomes) != len(orderedProviders) {
			return nil, nil, fmt.Errorf("music %d provider outcome prefix does not match the acquisition set", rootSong.MusicID)
		}
		identity, err := catalog.MusicIdentity(ctx, rootSong.MusicID)
		if err != nil {
			return nil, nil, err
		}
		firstReplay, err := lyricsrecovery.ReplaySong(ctx, rootSong.MusicID, identity, plan.Versions.ProviderPolicy, runtime, ledger, orderedProviders)
		if err != nil {
			return nil, nil, fmt.Errorf("music %d exact AcquisitionID replay failed", rootSong.MusicID)
		}
		secondReplay, err := lyricsrecovery.ReplaySong(ctx, rootSong.MusicID, identity, plan.Versions.ProviderPolicy, runtime, ledger, orderedProviders)
		if err != nil {
			return nil, nil, fmt.Errorf("music %d deterministic replay verification failed", rootSong.MusicID)
		}
		effectiveOrder, err := runtime.ProviderOrderForMusicID(rootSong.MusicID)
		if err != nil {
			return nil, nil, err
		}
		if err := validateFallbackPrefix(firstReplay, effectiveOrder); err != nil {
			return nil, nil, fmt.Errorf("music %d provider fallback chain is invalid: %w", rootSong.MusicID, err)
		}
		if err := compareReplayPair(firstReplay, secondReplay); err != nil {
			return nil, nil, fmt.Errorf("music %d deterministic replay changed: %w", rootSong.MusicID, err)
		}
		if err := compareReplayToPublished(firstReplay, publishedResult, opts.ProviderOutcomesPath, orderedProviders, acquisitionIDs, ledger, ctx); err != nil {
			return nil, nil, fmt.Errorf("music %d replay/publication mismatch: %w", rootSong.MusicID, err)
		}
		if rootSong.MusicID != plan.Scope.MusicIDs[index] {
			return nil, nil, errors.New("root song order changed during validation")
		}
		results[rootSong.MusicID] = publishedResult
	}
	return results, acquisitionIDs, nil
}

func validateFallbackPrefix(replay lyricsrecovery.ReplayResult, order []model.LyricsSourceProvider) error {
	if len(order) == 0 || len(replay.Providers) == 0 || len(replay.Providers) > len(order) ||
		replay.Providers[0].Artifact.Provider != order[0] {
		return errors.New("provider evaluation does not begin with the song-scoped immutable provider")
	}
	if order[0] != model.LyricsSourceProviderSekaipedia &&
		order[0] != model.LyricsSourceProviderMoegirlPublicExact {
		return errors.New("release song scope begins with an unauthorized provider")
	}
	if order[0] == model.LyricsSourceProviderMoegirlPublicExact &&
		(len(order) != 1 || len(replay.Providers) != 1) {
		return errors.New("exact public Moegirl scope must contain one offline provider and no fallback")
	}
	inputs := make([]lyricscompose.FixedArtifactInput, 0, len(replay.Providers))
	for index, provider := range replay.Providers {
		if provider.Artifact.Provider != order[index] || provider.Outcome.Provider != order[index] {
			return errors.New("provider evaluation is gapped or reordered")
		}
		if provider.Outcome.Status == lyricsprovideroutcome.StatusCandidate {
			if provider.Fixed == nil || provider.Artifact.Candidate == nil {
				return errors.New("candidate provider terminal is incomplete")
			}
			inputs = append(inputs, lyricscompose.FixedArtifactInput{
				SourceKey: provider.Artifact.OutcomeID, LogicalRenditionKey: provider.Artifact.Candidate.RenditionKey,
				Fixed: *provider.Fixed,
			})
			if index+1 < len(replay.Providers) {
				_, err := lyricscompose.ComposeFixedArtifacts(inputs)
				if !errors.Is(err, lyricscompose.ErrComponentsIncomplete) {
					return errors.New("candidate fallback was not required by incomplete Full/Game components")
				}
			}
		} else if index+1 < len(replay.Providers) && !lyricsrecovery.AllowsFallback(provider.Outcome.Provider, provider.Outcome) {
			return errors.New("non-candidate fallback reason is not in the closed reviewed policy")
		}
	}
	if replay.Composition == nil {
		return errors.New("complete release song has no authoritative Full composition")
	}
	return nil
}

func compareReplayPair(left, right lyricsrecovery.ReplayResult) error {
	if left.MusicID != right.MusicID || len(left.Providers) != len(right.Providers) {
		return errors.New("provider boundary changed")
	}
	for index := range left.Providers {
		leftBody, leftErr := lyricsoutcomeartifact.MarshalCanonical(left.Providers[index].Artifact)
		rightBody, rightErr := lyricsoutcomeartifact.MarshalCanonical(right.Providers[index].Artifact)
		if leftErr != nil || rightErr != nil || !bytes.Equal(leftBody, rightBody) {
			return errors.New("provider outcome artifact bytes changed")
		}
	}
	leftResult, leftErr := lyricsrecovery.NewSongResult(left)
	rightResult, rightErr := lyricsrecovery.NewSongResult(right)
	if leftErr != nil || rightErr != nil {
		return errors.New("song result could not be rebuilt")
	}
	leftBody, leftErr := lyricsrecovery.MarshalSongResult(leftResult)
	rightBody, rightErr := lyricsrecovery.MarshalSongResult(rightResult)
	if leftErr != nil || rightErr != nil || !bytes.Equal(leftBody, rightBody) {
		return errors.New("composed song result bytes changed")
	}
	return nil
}

func compareReplayToPublished(
	replay lyricsrecovery.ReplayResult,
	published lyricsrecovery.SongResult,
	outcomeDirectory string,
	orderedProviders []lyricsrecovery.ProviderAcquisitionSet,
	acquisitionIDs map[string]struct{},
	ledger *lyricsacquisition.Ledger,
	ctx context.Context,
) error {
	rebuilt, err := lyricsrecovery.NewSongResult(replay)
	if err != nil {
		return err
	}
	rebuiltBody, err := lyricsrecovery.MarshalSongResult(rebuilt)
	if err != nil {
		return err
	}
	publishedBody, err := lyricsrecovery.MarshalSongResult(published)
	if err != nil || !bytes.Equal(rebuiltBody, publishedBody) {
		return errors.New("published song result is not byte-identical to exact replay")
	}
	for index, replayed := range replay.Providers {
		terminal := orderedProviders[index]
		if terminal.Provider != replayed.Artifact.Provider || terminal.Status != replayed.Artifact.Status ||
			terminal.ReasonCode != replayed.Artifact.ReasonCode || terminal.Phase != replayed.Artifact.Phase ||
			terminal.Counts != replayed.Artifact.Counts {
			return errors.New("acquisition terminal does not exactly match its ProviderOutcome")
		}
		name, err := lyricsoutcomeartifact.FileName(replayed.Artifact)
		if err != nil {
			return err
		}
		stored, err := lyricsoutcomeartifact.Open(filepath.Join(outcomeDirectory, name))
		if err != nil {
			return err
		}
		storedBody, err := lyricsoutcomeartifact.MarshalCanonical(stored)
		replayedBody, replayErr := lyricsoutcomeartifact.MarshalCanonical(replayed.Artifact)
		if err != nil || replayErr != nil || !bytes.Equal(storedBody, replayedBody) {
			return errors.New("persisted ProviderOutcome is not byte-identical to exact replay")
		}
		refsByAcquisition := make(map[string]lyricsoutcomeartifact.AcquisitionRef, len(stored.Acquisitions))
		for _, ref := range stored.Acquisitions {
			refsByAcquisition[ref.AcquisitionID] = ref
		}
		if len(refsByAcquisition) != len(terminal.AcquisitionIDs) {
			return errors.New("ProviderOutcome acquisition references do not equal the exact acquisition set")
		}
		for _, acquisitionID := range terminal.AcquisitionIDs {
			id := string(acquisitionID)
			acquisitionIDs[id] = struct{}{}
			acquired, err := ledger.ReplayByAcquisitionID(ctx, acquisitionID)
			if err != nil || !acquired.ReplayOnly || string(acquired.AcquisitionID) != id {
				return errors.New("exact AcquisitionID did not replay from the fresh ledger")
			}
			evidence, err := lyricsevidencepack.EvidenceRefFromAcquisition(acquired)
			if err != nil {
				return err
			}
			artifactRef, found := refsByAcquisition[id]
			if !found || artifactRef.EvidenceID != evidence.EvidenceID || artifactRef.SHA256 != evidence.SHA256 ||
				artifactRef.EnvelopeSHA256 != evidence.EnvelopeSHA256 || artifactRef.AcquisitionID != evidence.AcquisitionID {
				return errors.New("ProviderOutcome does not retain the exact acquisition/evidence identity")
			}
		}
	}
	return nil
}

func orderedEvidenceUnion(root lyricsrootmanifest.Manifest) ([]lyricscontract.EvidenceRef, error) {
	byID := make(map[string]lyricscontract.EvidenceRef, root.Coverage.UniqueEvidenceCount)
	byAcquisition := make(map[string]lyricscontract.EvidenceRef, root.Coverage.UniqueAcquisitionCount)
	for _, song := range root.Songs {
		for _, ref := range song.SelectedEvidence {
			if prior, found := byID[ref.EvidenceID]; found && prior != ref {
				return nil, errors.New("fresh root contains a conflicting EvidenceID union")
			}
			if prior, found := byAcquisition[ref.AcquisitionID]; found && prior != ref {
				return nil, errors.New("fresh root contains a conflicting AcquisitionID union")
			}
			byID[ref.EvidenceID] = ref
			byAcquisition[ref.AcquisitionID] = ref
		}
	}
	if len(byID) != root.Coverage.UniqueEvidenceCount || len(byAcquisition) != root.Coverage.UniqueAcquisitionCount {
		return nil, errors.New("fresh root evidence union does not match its complete coverage counters")
	}
	refs := make([]lyricscontract.EvidenceRef, 0, len(byID))
	for _, ref := range byID {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].EvidenceID < refs[right].EvidenceID })
	return refs, nil
}
