package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"moesekai/server/internal/httpx"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
	"moesekai/server/offline/internal/lyricsprovidercoord"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
	"moesekai/server/offline/internal/lyricsrecovery"
	"moesekai/server/offline/internal/lyricsreview"
	"moesekai/server/offline/internal/lyricsrootmanifest"
)

const (
	liveCanaryAuthorization         = "MOESEKAI_RECOVERY_V2_LIVE_CANARY_AUTHORIZED"
	acquisitionAuthorization        = "MOESEKAI_RECOVERY_V2_ACQUISITION_AUTHORIZED"
	migrationAuthorization          = "MOESEKAI_RECOVERY_V2_REAL_MIGRATION_AUTHORIZED"
	liveStateProvisionAuthorization = "MOESEKAI_RECOVERY_V2_LIVE_STATE_PROVISION_AUTHORIZED"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type recoveryLiveOwnership interface {
	Wrap(lyricsproviderpolicy.Provider, http.RoundTripper) (http.RoundTripper, error)
	ResolveProvider(lyricsproviderpolicy.Provider) error
	Close() error
}

var acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
	return lyricsprovidercoord.AcquireDefault()
}

var provisionRecoveryLiveState = func() error {
	return provisionRecoveryLiveStateRoot(lyricsproviderpolicy.FixedLiveStateRootV1)
}

type options struct {
	mode                               string
	planPath                           string
	expectedPlanSHA256                 string
	sourceRoot                         string
	catalogPath                        string
	expectedCatalogSHA256              string
	expectedCatalogCount               int
	expectedCatalogMusicIDsSHA256      string
	ledgerPath                         string
	acquisitionSetPath                 string
	providerOutcomesPath               string
	songResultsPath                    string
	evidencePackPath                   string
	rootManifestPath                   string
	reviewDecisionManifestPath         string
	parentRootPath                     string
	rebindSourceLedgerPath             string
	rebindSourceAcquisitionSetPath     string
	rebindSupplementLedgerPath         string
	rebindSupplementAcquisitionSetPath string
	sekaipediaListReplayLedgerPath     string
	sekaipediaListReplayAcquisitionID  string
	acquisitionMusicIDs                []int
	liveCanaryToken                    string
	acquisitionToken                   string
	migrationToken                     string
	liveStateProvisionToken            string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	options, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	if options.mode == "provision-live-state" {
		if options.liveStateProvisionToken != liveStateProvisionAuthorization {
			_, err := fmt.Fprintln(output, "HOLD mode=provision-live-state authorization=missing network=HOLD cooldown=preserved")
			return err
		}
		if err := provisionRecoveryLiveState(); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "PASS mode=provision-live-state root=%s publication=create-exclusive network=HOLD cooldown=preserved\n",
			lyricsproviderpolicy.FixedLiveStateRootV1)
		return err
	}
	plan, planSHA, catalog, err := checkInputs(ctx, options)
	if err != nil {
		return err
	}
	defer catalog.Close()

	if options.mode == "check" {
		if err := checkOutputState(options, false, false); err != nil {
			return err
		}
		legacyListInspection := plan.SekaipediaCanary != nil && plan.SekaipediaCanary.List.AcquisitionID == ""
		_, err = fmt.Fprintf(output, "PASS mode=check planSha256=%s sourceSnapshotSha256=%s catalogCount=%d legacyListInspection=%t liveCanaryAuthority=HOLD network=HOLD migration=HOLD\n",
			planSHA, plan.SourceSnapshot.SHA256, plan.Catalog.RecordCount, legacyListInspection)
		return err
	}

	runtime, err := lyricsrecovery.RuntimeConfigFromPlan(plan)
	if err != nil {
		return err
	}

	switch options.mode {
	case "rebind":
		if err := checkOutputState(options, false, true); err != nil {
			return err
		}
		return runRebind(ctx, output, options, plan, planSHA, catalog, runtime)
	case "replay", "fixture-canary":
		if err := checkOutputState(options, true, false); err != nil {
			return err
		}
		setBody, err := readPinnedPrivateFile(options.acquisitionSetPath, lyricsrecovery.MaxAcquisitionSetBytes)
		if err != nil {
			return err
		}
		set, err := lyricsrecovery.DecodeAcquisitionSet(setBody)
		if err != nil {
			return err
		}
		if err := lyricsrecovery.ValidateAcquisitionSetAuthorization(
			set, plan.PlanID, planSHA, plan.Scope.MusicIDs, runtime.Order, runtime.ProviderMusicIDs,
		); err != nil {
			return err
		}
		parent, err := readParentRoot(options.parentRootPath, plan)
		if err != nil {
			return err
		}
		ledger, err := openCheckedRecoveryLedger(ctx, options.ledgerPath)
		if err != nil {
			return err
		}
		defer ledger.Close()
		return runReplay(ctx, output, options, plan, planSHA, catalog, runtime, ledger, set, parent)
	case "live-canary":
		if options.liveCanaryToken != liveCanaryAuthorization {
			_, err := fmt.Fprintln(output, "HOLD mode=live-canary authorization=missing network=HOLD")
			return err
		}
		if len(plan.Execution.LiveCanaryMusicIDs) != 1 || plan.SekaipediaCanary == nil ||
			!sha256Pattern.MatchString(plan.SekaipediaCanary.List.AcquisitionID) {
			return errors.New("live-canary plan contains no exact Sekaipedia List replay identity")
		}
		if options.sekaipediaListReplayAcquisitionID != "" &&
			options.sekaipediaListReplayAcquisitionID != plan.SekaipediaCanary.List.AcquisitionID {
			return errors.New("compatibility Sekaipedia List replay acquisition ID does not exactly match the immutable plan")
		}
		runtime, err = lyricsrecovery.WithSekaipediaCanaryPlan(runtime, plan)
		if err != nil {
			return err
		}
		if err := checkOutputState(options, false, true); err != nil {
			return err
		}
		return runLiveCanary(ctx, output, options, plan, planSHA, catalog, runtime)
	case "acquisition", "acquisition-subset":
		if options.liveCanaryToken != liveCanaryAuthorization {
			_, err := fmt.Fprintf(output, "HOLD mode=%s liveAuthorization=missing network=HOLD\n", options.mode)
			return err
		}
		if options.acquisitionToken != acquisitionAuthorization {
			_, err := fmt.Fprintf(output, "HOLD mode=%s acquisitionAuthorization=missing network=HOLD\n", options.mode)
			return err
		}
		if plan.SekaipediaCanary == nil || !sha256Pattern.MatchString(plan.SekaipediaCanary.List.AcquisitionID) {
			return errors.New("acquisition plan contains no exact Sekaipedia List replay identity")
		}
		if options.sekaipediaListReplayAcquisitionID != "" &&
			options.sekaipediaListReplayAcquisitionID != plan.SekaipediaCanary.List.AcquisitionID {
			return errors.New("compatibility Sekaipedia List replay acquisition ID does not exactly match the immutable plan")
		}
		runtime, err = lyricsrecovery.WithSekaipediaCanaryPlan(runtime, plan)
		if err != nil {
			return err
		}
		musicIDs := plan.Scope.MusicIDs
		if options.mode == "acquisition-subset" {
			musicIDs = options.acquisitionMusicIDs
			if err := validateAcquisitionSubset(plan, runtime, musicIDs); err != nil {
				return err
			}
		}
		if err := checkOutputState(options, false, true); err != nil {
			return err
		}
		return runAcquisition(ctx, output, options, plan, planSHA, catalog, runtime, musicIDs)
	case "migration":
		if options.migrationToken != migrationAuthorization {
			_, err := fmt.Fprintln(output, "HOLD mode=migration authorization=missing realMigration=HOLD")
			return err
		}
		_, err := fmt.Fprintln(output, "HOLD mode=migration authorization=present implementation=not-exposed realMigration=HOLD")
		return err
	default:
		return errors.New("lyrics recovery mode is invalid")
	}
}

func parseOptions(arguments []string) (options, error) {
	var options options
	var acquisitionMusicIDs string
	flags := flag.NewFlagSet("lyrics-recovery", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.mode, "mode", "check", "check|rebind|replay|fixture-canary|live-canary|acquisition|acquisition-subset|migration|provision-live-state")
	flags.StringVar(&options.planPath, "plan", "", "immutable recovery plan path")
	flags.StringVar(&options.expectedPlanSHA256, "expected-plan-sha256", "", "expected recovery plan SHA-256")
	flags.StringVar(&options.sourceRoot, "source-root", "", "canonical root containing the immutable recovery source snapshot")
	flags.StringVar(&options.catalogPath, "catalog", "", "immutable catalog path")
	flags.StringVar(&options.expectedCatalogSHA256, "expected-catalog-sha256", "", "expected catalog SHA-256")
	flags.IntVar(&options.expectedCatalogCount, "expected-catalog-count", 0, "expected catalog record count")
	flags.StringVar(&options.expectedCatalogMusicIDsSHA256, "expected-catalog-music-ids-sha256", "", "expected ordered music-ID SHA-256")
	flags.StringVar(&options.ledgerPath, "ledger", "", "private acquisition ledger path")
	flags.StringVar(&options.acquisitionSetPath, "acquisition-set", "", "exact ordered acquisition-set path")
	flags.StringVar(&options.providerOutcomesPath, "provider-outcomes", "", "private provider-outcome directory")
	flags.StringVar(&options.songResultsPath, "song-results", "", "private song-result directory")
	flags.StringVar(&options.evidencePackPath, "evidence-pack", "", "private evidence-pack directory")
	flags.StringVar(&options.rootManifestPath, "root-manifest", "", "compact root-manifest path")
	flags.StringVar(&options.reviewDecisionManifestPath, "review-decision-manifest", "", "content-free manual review decision manifest")
	flags.StringVar(&options.parentRootPath, "parent-root", "", "exact parent root for partial/retry")
	flags.StringVar(&options.rebindSourceLedgerPath, "rebind-source-ledger", "", "existing exact acquisition ledger copied without network access")
	flags.StringVar(&options.rebindSourceAcquisitionSetPath, "rebind-source-acquisition-set", "", "historical exact acquisition set whose terminals are regenerated")
	flags.StringVar(&options.rebindSupplementLedgerPath, "rebind-supplement-ledger", "", "optional exact supplemental ledger replacing its explicitly declared songs")
	flags.StringVar(&options.rebindSupplementAcquisitionSetPath, "rebind-supplement-acquisition-set", "", "optional plan-bound supplemental acquisition set replacing its explicitly declared songs")
	flags.StringVar(&options.sekaipediaListReplayLedgerPath, "sekaipedia-list-replay-ledger", "", "existing exact-acquisition source ledger for one plan-bound List replay")
	flags.StringVar(&options.sekaipediaListReplayAcquisitionID, "sekaipedia-list-replay-acquisition-id", "", "exact List acquisition ID in the source ledger")
	flags.StringVar(&acquisitionMusicIDs, "acquisition-music-ids", "", "strictly ordered comma-separated immutable-plan subset for acquisition-subset")
	flags.StringVar(&options.liveCanaryToken, "live-canary-authorization", "", "separate live-canary authorization")
	flags.StringVar(&options.acquisitionToken, "acquisition-authorization", "", "separate acquisition authorization")
	flags.StringVar(&options.migrationToken, "migration-authorization", "", "separate real-migration authorization")
	flags.StringVar(&options.liveStateProvisionToken, "live-state-provision-authorization", "", "separate fixed live-state provisioning authorization")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return options, errors.New("lyrics recovery requires only explicit named flags")
	}
	if acquisitionMusicIDs != "" {
		var err error
		options.acquisitionMusicIDs, err = parseAcquisitionMusicIDs(acquisitionMusicIDs)
		if err != nil {
			return options, err
		}
	}
	if options.mode == "provision-live-state" {
		if options.planPath != "" || options.expectedPlanSHA256 != "" || options.sourceRoot != "" ||
			options.catalogPath != "" || options.expectedCatalogSHA256 != "" || options.expectedCatalogCount != 0 ||
			options.expectedCatalogMusicIDsSHA256 != "" || options.ledgerPath != "" || options.acquisitionSetPath != "" ||
			options.providerOutcomesPath != "" || options.songResultsPath != "" || options.evidencePackPath != "" ||
			options.rootManifestPath != "" || options.reviewDecisionManifestPath != "" || options.parentRootPath != "" ||
			options.rebindSourceLedgerPath != "" || options.rebindSourceAcquisitionSetPath != "" ||
			options.rebindSupplementLedgerPath != "" || options.rebindSupplementAcquisitionSetPath != "" ||
			options.sekaipediaListReplayLedgerPath != "" || options.sekaipediaListReplayAcquisitionID != "" ||
			len(options.acquisitionMusicIDs) != 0 || options.liveCanaryToken != "" ||
			options.acquisitionToken != "" || options.migrationToken != "" {
			return options, errors.New("live-state provisioning accepts only its explicit mode and authorization")
		}
		return options, nil
	}
	if options.mode == "" || options.planPath == "" || options.sourceRoot == "" || options.catalogPath == "" ||
		!sha256Pattern.MatchString(options.expectedPlanSHA256) || !sha256Pattern.MatchString(options.expectedCatalogSHA256) ||
		options.expectedCatalogCount <= 0 || !sha256Pattern.MatchString(options.expectedCatalogMusicIDsSHA256) {
		return options, errors.New("lyrics recovery requires explicit plan and catalog identity pins")
	}
	for _, path := range []string{
		options.planPath, options.sourceRoot, options.catalogPath, options.ledgerPath, options.acquisitionSetPath,
		options.providerOutcomesPath, options.songResultsPath, options.evidencePackPath, options.rootManifestPath,
	} {
		if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return options, errors.New("lyrics recovery paths must be explicit canonical absolute paths")
		}
	}
	if options.reviewDecisionManifestPath != "" &&
		(strings.TrimSpace(options.reviewDecisionManifestPath) != options.reviewDecisionManifestPath ||
			!filepath.IsAbs(options.reviewDecisionManifestPath) ||
			filepath.Clean(options.reviewDecisionManifestPath) != options.reviewDecisionManifestPath ||
			!isRecoveryPrivateReplayPath(options.reviewDecisionManifestPath) ||
			strings.Contains(strings.ToLower(options.reviewDecisionManifestPath), "production")) {
		return options, errors.New("review decision manifest path is invalid")
	}
	rebindMode := options.mode == "rebind"
	primaryRebindSpecified := options.rebindSourceLedgerPath != "" || options.rebindSourceAcquisitionSetPath != ""
	supplementRebindSpecified := options.rebindSupplementLedgerPath != "" || options.rebindSupplementAcquisitionSetPath != ""
	if rebindMode {
		if !primaryRebindSpecified || options.rebindSourceLedgerPath == "" ||
			options.rebindSourceAcquisitionSetPath == "" {
			return options, errors.New("offline rebind requires exactly one explicit primary source ledger and acquisition set")
		}
		if supplementRebindSpecified && (options.rebindSupplementLedgerPath == "" ||
			options.rebindSupplementAcquisitionSetPath == "") {
			return options, errors.New("offline rebind supplement requires both its exact ledger and acquisition set")
		}
	} else if primaryRebindSpecified || supplementRebindSpecified {
		return options, errors.New("offline rebind sources are restricted to rebind mode")
	}
	if rebindMode {
		sourcePaths := []string{options.rebindSourceLedgerPath, options.rebindSourceAcquisitionSetPath}
		if supplementRebindSpecified {
			sourcePaths = append(sourcePaths, options.rebindSupplementLedgerPath, options.rebindSupplementAcquisitionSetPath)
		}
		seenSourcePaths := make(map[string]struct{}, len(sourcePaths))
		for _, sourcePath := range sourcePaths {
			if strings.TrimSpace(sourcePath) != sourcePath || !filepath.IsAbs(sourcePath) ||
				filepath.Clean(sourcePath) != sourcePath || !isRecoveryPrivateReplayPath(sourcePath) ||
				strings.Contains(strings.ToLower(sourcePath), "production") {
				return options, errors.New("offline rebind source identity is invalid")
			}
			if _, duplicate := seenSourcePaths[sourcePath]; duplicate {
				return options, errors.New("offline rebind source paths are not distinct")
			}
			seenSourcePaths[sourcePath] = struct{}{}
			for _, forbidden := range []string{
				options.planPath, options.sourceRoot, options.catalogPath, options.ledgerPath,
				options.acquisitionSetPath, options.providerOutcomesPath, options.songResultsPath,
				options.evidencePackPath, options.rootManifestPath,
			} {
				if sourcePath == forbidden {
					return options, errors.New("offline rebind source aliases a command input or output")
				}
			}
		}
		if options.parentRootPath != "" {
			return options, errors.New("offline rebind rejects a parent-root input")
		}
	}
	subsetMode := options.mode == "acquisition-subset"
	if subsetMode != (len(options.acquisitionMusicIDs) > 0) {
		return options, errors.New("acquisition-subset requires one explicit strictly ordered music-ID subset")
	}
	if options.parentRootPath != "" && (strings.TrimSpace(options.parentRootPath) != options.parentRootPath ||
		!filepath.IsAbs(options.parentRootPath) || filepath.Clean(options.parentRootPath) != options.parentRootPath) {
		return options, errors.New("lyrics recovery parent root path is invalid")
	}
	replayLedgerSet := options.sekaipediaListReplayLedgerPath != ""
	replayIDSet := options.sekaipediaListReplayAcquisitionID != ""
	replayMode := options.mode == "live-canary" || options.mode == "acquisition" ||
		options.mode == "acquisition-subset" || rebindMode
	if replayMode && !replayLedgerSet {
		return options, errors.New("acquisition or rebind requires an existing exact Sekaipedia List source ledger")
	}
	if rebindMode && !replayIDSet {
		return options, errors.New("offline rebind requires the exact plan-bound Sekaipedia List AcquisitionID")
	}
	if !replayMode && (replayLedgerSet || replayIDSet) {
		return options, errors.New("Sekaipedia List replay is restricted to acquisition or rebind modes")
	}
	if replayIDSet && !sha256Pattern.MatchString(options.sekaipediaListReplayAcquisitionID) {
		return options, errors.New("Sekaipedia List compatibility acquisition ID is invalid")
	}
	if replayLedgerSet {
		if strings.TrimSpace(options.sekaipediaListReplayLedgerPath) != options.sekaipediaListReplayLedgerPath ||
			!filepath.IsAbs(options.sekaipediaListReplayLedgerPath) ||
			filepath.Clean(options.sekaipediaListReplayLedgerPath) != options.sekaipediaListReplayLedgerPath ||
			!isRecoveryPrivateReplayPath(options.sekaipediaListReplayLedgerPath) ||
			strings.Contains(strings.ToLower(options.sekaipediaListReplayLedgerPath), "production") {
			return options, errors.New("Sekaipedia List replay source identity is invalid")
		}
		for _, forbidden := range []string{
			options.planPath, options.sourceRoot, options.catalogPath, options.ledgerPath,
			exactReplayRuntimeCopyPath(options.ledgerPath),
			options.acquisitionSetPath, options.providerOutcomesPath, options.songResultsPath,
			options.evidencePackPath, options.rootManifestPath,
			options.rebindSourceLedgerPath, options.rebindSourceAcquisitionSetPath,
			options.rebindSupplementLedgerPath, options.rebindSupplementAcquisitionSetPath,
		} {
			if forbidden != "" && options.sekaipediaListReplayLedgerPath == forbidden {
				return options, errors.New("Sekaipedia List replay source aliases a command input, output, or rebind source")
			}
		}
	}
	if options.liveStateProvisionToken != "" {
		return options, errors.New("live-state provisioning authorization is invalid outside provisioning mode")
	}
	return options, nil
}

func isRecoveryPrivateReplayPath(path string) bool {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if strings.HasPrefix(path, "/private/tmp/") {
		return true
	}
	activeRoot := os.Getenv("MOESEKAI_SESSION_ROOT")
	if activeRoot == "" || strings.TrimSpace(activeRoot) != activeRoot || !filepath.IsAbs(activeRoot) ||
		filepath.Clean(activeRoot) != activeRoot {
		return false
	}
	sessionsRoot := filepath.Dir(activeRoot)
	relative, err := filepath.Rel(sessionsRoot, path)
	return err == nil && relative != "." && filepath.IsLocal(relative)
}

func checkInputs(
	ctx context.Context,
	options options,
) (lyricsextractionplan.RecoveryPlan, string, *checkedRecoveryCatalog, error) {
	body, err := readPinnedPrivateFile(options.planPath, lyricsextractionplan.MaxPlanBytes)
	if err != nil {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, err
	}
	var (
		plan    lyricsextractionplan.RecoveryPlan
		planSHA string
	)
	if options.mode == "check" {
		plan, planSHA, err = lyricsextractionplan.CheckRecoveryForInspection(body, options.expectedPlanSHA256)
	} else {
		plan, planSHA, err = lyricsextractionplan.CheckRecovery(body, options.expectedPlanSHA256)
	}
	if err != nil {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, err
	}
	if err := verifyPinnedRecoverySourceSnapshot(options.sourceRoot, plan, options.mode == "check"); err != nil {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, err
	}
	if plan.Catalog.Path != options.catalogPath || plan.Catalog.SourceSHA256 != options.expectedCatalogSHA256 ||
		plan.Catalog.RecordCount != options.expectedCatalogCount ||
		plan.Catalog.MusicIDsSHA256 != options.expectedCatalogMusicIDsSHA256 {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, errors.New("command catalog pins do not exactly match the immutable plan")
	}
	if plan.Outputs.Ledger != options.ledgerPath || plan.Outputs.AcquisitionSet != options.acquisitionSetPath ||
		plan.Outputs.ProviderOutcomes != options.providerOutcomesPath || plan.Outputs.SongResults != options.songResultsPath ||
		plan.Outputs.EvidencePack != options.evidencePackPath || plan.Outputs.RootManifest != options.rootManifestPath {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, errors.New("command output paths do not exactly match the immutable plan")
	}
	if !isRecoveryPrivateReplayPath(options.catalogPath) || strings.Contains(strings.ToLower(options.catalogPath), "production") ||
		strings.Contains(strings.ToLower(options.catalogPath), "moesekai.db") {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, errors.New("production or non-private catalog paths are forbidden")
	}
	catalog, err := openCheckedRecoveryCatalog(ctx, options.catalogPath, plan.Catalog)
	if err != nil {
		return lyricsextractionplan.RecoveryPlan{}, "", nil, err
	}
	return plan, planSHA, catalog, nil
}

func checkOutputState(options options, replay bool, acquisition bool) error {
	if replay && acquisition {
		return errors.New("lyrics recovery output-state mode is invalid")
	}
	if replay {
		for _, path := range []string{options.providerOutcomesPath, options.songResultsPath, options.evidencePackPath, options.rootManifestPath} {
			if err := requireAbsentWithPrivateParent(path); err != nil {
				return err
			}
		}
		return nil
	}
	if acquisition {
		for _, path := range []string{options.ledgerPath, options.acquisitionSetPath, options.providerOutcomesPath, options.songResultsPath, options.evidencePackPath, options.rootManifestPath} {
			if err := requireAbsentWithPrivateParent(path); err != nil {
				return err
			}
		}
		forensicPath, err := lyricsrecovery.ForensicResponseStorePath(options.ledgerPath)
		if err != nil {
			return err
		}
		if err := requireAbsentWithPrivateParent(forensicPath); err != nil {
			return err
		}
		if options.sekaipediaListReplayLedgerPath != "" {
			return requireAbsentWithPrivateParent(exactReplayRuntimeCopyPath(options.ledgerPath))
		}
		return nil
	}
	for _, path := range []string{options.ledgerPath, options.acquisitionSetPath, options.providerOutcomesPath, options.songResultsPath, options.evidencePackPath, options.rootManifestPath} {
		if err := requireAbsentWithPrivateParent(path); err != nil {
			return err
		}
	}
	forensicPath, err := lyricsrecovery.ForensicResponseStorePath(options.ledgerPath)
	if err != nil {
		return err
	}
	return requireAbsentWithPrivateParent(forensicPath)
}

func runReplay(
	ctx context.Context,
	output io.Writer,
	options options,
	plan lyricsextractionplan.RecoveryPlan,
	planSHA string,
	catalog *checkedRecoveryCatalog,
	runtime lyricsrecovery.RuntimeConfig,
	ledger *checkedRecoveryLedger,
	set lyricsrecovery.AcquisitionSet,
	parent *lyricsrootmanifest.Manifest,
) error {
	if err := ledger.verify(); err != nil {
		return err
	}
	if err := lyricsoutcomeartifact.CreatePrivateDirectory(options.providerOutcomesPath); err != nil {
		return err
	}
	if err := lyricsoutcomeartifact.CreatePrivateDirectory(options.songResultsPath); err != nil {
		return err
	}
	var reviewResolver *lyricsreview.Resolver
	if options.reviewDecisionManifestPath != "" {
		loaded, err := lyricsreview.OpenResolver(
			options.reviewDecisionManifestPath, plan.PlanID, planSHA, plan.SourceSnapshot.SHA256,
		)
		if err != nil {
			return err
		}
		reviewResolver = &loaded
	}
	results := make([]lyricsrecovery.SongResult, 0, len(plan.Scope.MusicIDs))
	for _, musicID := range plan.Scope.MusicIDs {
		identity, err := catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			return err
		}
		orderedProviders, err := set.OrderedProviders(musicID)
		if err != nil {
			return err
		}
		replayed, err := lyricsrecovery.ReplaySong(
			ctx, musicID, identity, plan.Versions.ProviderPolicy, runtime, ledger.ledger, orderedProviders,
		)
		if err != nil {
			return err
		}
		verifiedReplay, err := lyricsrecovery.ReplaySong(
			ctx, musicID, identity, plan.Versions.ProviderPolicy, runtime, ledger.ledger, orderedProviders,
		)
		if err != nil {
			return err
		}
		if err := ledger.verify(); err != nil {
			return err
		}
		if err := requireByteIdenticalReplay(replayed, verifiedReplay); err != nil {
			return fmt.Errorf("music %d exact AcquisitionID replay: %w", musicID, err)
		}
		for _, provider := range replayed.Providers {
			if _, err := lyricsoutcomeartifact.PublishCreateExclusive(options.providerOutcomesPath, provider.Artifact); err != nil {
				return err
			}
		}
		result, err := lyricsrecovery.NewSongResult(replayed)
		if err != nil {
			return err
		}
		if reviewResolver != nil {
			resultObservation, outcomeObservations := lyricsrecovery.ReviewObservation(replayed, result)
			if err := reviewResolver.ValidateResult(resultObservation, outcomeObservations); err != nil {
				return fmt.Errorf("music %d manual review decision: %w", musicID, err)
			}
		}
		name, _ := lyricsrecovery.SongResultFileName(result)
		if err := lyricsrecovery.PublishSongResult(filepath.Join(options.songResultsPath, name), result); err != nil {
			return err
		}
		results = append(results, result)
		if _, err := fmt.Fprintf(output, "PASS mode=%s musicId=%d providerOutcomes=%d selectedEvidence=%d exactAcquisitionIDReplay=byte-identical\n",
			options.mode, musicID, len(replayed.Providers), len(result.SelectedEvidence)); err != nil {
			return err
		}
	}
	if err := ledger.verify(); err != nil {
		return err
	}
	root, err := lyricsrecovery.AssembleAndPublish(ctx, plan, planSHA, ledger.ledger, results, parent)
	if err != nil {
		return err
	}
	if err := ledger.verify(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "PASS mode=%s rootSha256=%s songs=%d network=HOLD migration=HOLD\n",
		options.mode, root.RootSHA256, len(root.Songs))
	return err
}

func requireByteIdenticalReplay(first, second lyricsrecovery.ReplayResult) error {
	if first.MusicID != second.MusicID || len(first.Providers) != len(second.Providers) {
		return errors.New("replay provider boundary changed")
	}
	for index := range first.Providers {
		left, leftErr := lyricsoutcomeartifact.MarshalCanonical(first.Providers[index].Artifact)
		right, rightErr := lyricsoutcomeartifact.MarshalCanonical(second.Providers[index].Artifact)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return errors.New("provider outcome artifact bytes changed")
		}
	}
	leftResult, leftErr := lyricsrecovery.NewSongResult(first)
	if leftErr != nil {
		return fmt.Errorf("first replay song result could not be rebuilt: %w", leftErr)
	}
	rightResult, rightErr := lyricsrecovery.NewSongResult(second)
	if rightErr != nil {
		return fmt.Errorf("verification replay song result could not be rebuilt: %w", rightErr)
	}
	left, leftErr := lyricsrecovery.MarshalSongResult(leftResult)
	if leftErr != nil {
		return fmt.Errorf("first replay song result failed canonical validation: %w", leftErr)
	}
	right, rightErr := lyricsrecovery.MarshalSongResult(rightResult)
	if rightErr != nil {
		return fmt.Errorf("verification replay song result failed canonical validation: %w", rightErr)
	}
	if !bytes.Equal(left, right) {
		return errors.New("composed song result bytes changed")
	}
	return nil
}

func newRecoveryLiveTransports(
	runtime lyricsrecovery.RuntimeConfig,
	ownership recoveryLiveOwnership,
) (map[model.LyricsSourceProvider]http.RoundTripper, error) {
	if runtime.RequestTimeout <= 0 || len(runtime.Order) == 0 || ownership == nil {
		return nil, errors.New("recovery live transports require ownership, a positive request timeout, and provider order")
	}
	boundedPhaseTimeout := func(maximum time.Duration) time.Duration {
		if runtime.RequestTimeout < maximum {
			return runtime.RequestTimeout
		}
		return maximum
	}
	live := make(map[model.LyricsSourceProvider]http.RoundTripper, len(runtime.Order))
	seen := make(map[model.LyricsSourceProvider]struct{}, len(runtime.Order))
	for _, provider := range runtime.Order {
		if !model.IsValidLyricsSourceProvider(provider) {
			return nil, errors.New("recovery live transport provider is invalid")
		}
		if _, duplicate := seen[provider]; duplicate {
			return nil, errors.New("recovery live transport provider order contains a duplicate")
		}
		seen[provider] = struct{}{}
		if provider == lyricssource.ProviderMoegirlPublicExact {
			continue
		}
		client := httpx.NewUpstreamClientWithOptions(httpx.UpstreamClientOptions{
			Timeout:               runtime.RequestTimeout,
			DialTimeout:           boundedPhaseTimeout(10 * time.Second),
			TLSHandshakeTimeout:   boundedPhaseTimeout(12 * time.Second),
			ResponseHeaderTimeout: boundedPhaseTimeout(12 * time.Second),
			Policy:                httpx.UpstreamPolicyFromEnvironment(),
			AllowQuery:            true,
		})
		if client == nil || client.Transport == nil {
			return nil, errors.New("recovery live transport construction failed closed")
		}
		coordinated, err := ownership.Wrap(lyricsproviderpolicy.Provider(provider), client.Transport)
		if err != nil {
			return nil, err
		}
		live[provider] = coordinated
	}
	return live, nil
}

func runLiveCanary(
	ctx context.Context,
	output io.Writer,
	options options,
	plan lyricsextractionplan.RecoveryPlan,
	planSHA string,
	catalog *checkedRecoveryCatalog,
	runtime lyricsrecovery.RuntimeConfig,
) (resultErr error) {
	if runtime.SekaipediaCanary == nil || plan.SekaipediaCanary == nil ||
		runtime.SekaipediaCanary.ListAcquisitionID != plan.SekaipediaCanary.List.AcquisitionID {
		return errors.New("live-canary runtime lost the immutable exact List replay identity")
	}
	source, err := readExactAcquisitionReplaySource(
		ctx,
		options.sekaipediaListReplayLedgerPath,
		exactReplayRuntimeCopyPath(options.ledgerPath),
		lyricsacquisition.AcquisitionID(runtime.SekaipediaCanary.ListAcquisitionID),
	)
	if err != nil {
		return err
	}

	ownership, err := acquireRecoveryLiveOwnership()
	if err != nil {
		if errors.Is(err, lyricsprovidercoord.ErrHold) {
			_, writeErr := fmt.Fprintln(output, "HOLD mode=live-canary liveOwnership=HOLD network=HOLD")
			return writeErr
		}
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ownership.Close()) }()

	live, err := newRecoveryLiveTransports(runtime, ownership)
	if err != nil {
		return err
	}
	listReplay, err := lyricsrecovery.NewPlanBoundSekaipediaListReplayTransport(
		source, runtime.SekaipediaCanary.List, live[model.LyricsSourceProviderSekaipedia],
	)
	if err != nil {
		return err
	}
	live[model.LyricsSourceProviderSekaipedia] = listReplay

	ledger, err := lyricsacquisition.CreateLedger(ctx, options.ledgerPath)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ledger.Close()) }()
	if err := lyricsoutcomeartifact.CreatePrivateDirectory(options.providerOutcomesPath); err != nil {
		return err
	}
	session, err := lyricsrecovery.NewAcquisitionSession(runtime, ledger, live)
	if err != nil {
		return err
	}
	songs := make([]lyricsrecovery.SongAcquisitionSet, 0, len(plan.Execution.LiveCanaryMusicIDs))
	for _, musicID := range plan.Execution.LiveCanaryMusicIDs {
		identity, err := catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			return err
		}
		providers, progress, diagnostic, err := session.AcquireSekaipediaCanarySong(ctx, musicID, identity)
		if err != nil {
			return err
		}
		if listReplay != nil && !listReplay.Consumed() {
			return errors.New("plan-bound Sekaipedia List replay was not consumed exactly once")
		}
		if len(providers) != 1 || len(progress) != 1 || progress[0].SekaipediaCanary == nil ||
			progress[0].EnterResult != diagnostic.EnterResult ||
			progress[0].FallbackReasonCode != diagnostic.FallbackReasonCode {
			return errors.New("live Sekaipedia canary terminal is incomplete")
		}
		diagnosticPath, err := lyricsrecovery.PublishSekaipediaCanaryDiagnostic(options.providerOutcomesPath, diagnostic)
		if err != nil {
			return err
		}
		if err := ownership.ResolveProvider(lyricsproviderpolicy.ProviderSekaipedia); err != nil {
			return err
		}
		songs = append(songs, lyricsrecovery.SongAcquisitionSet{MusicID: musicID, Providers: providers})
		exact := lyricsrecovery.SekaipediaCanaryCompleteCompositionStop(runtime, diagnostic)
		if _, err := fmt.Fprintf(output,
			"RESULT mode=live-canary musicId=%d provider=%s status=%s reasonCode=%s enterResult=%s fallbackReasonCode=%s exactRevisionEvidence=%t diagnostic=%s\n",
			diagnostic.MusicID, diagnostic.Provider, diagnostic.Status, diagnostic.ReasonCode,
			diagnostic.EnterResult, diagnostic.FallbackReasonCode, exact, diagnosticPath); err != nil {
			return err
		}
		if !exact {
			if _, err := fmt.Fprintf(output,
				"FAIL mode=live-canary musicId=%d enterResult=%s fallbackReasonCode=%s exactRevisionEvidence=false\n",
				diagnostic.MusicID, diagnostic.EnterResult, diagnostic.FallbackReasonCode); err != nil {
				return err
			}
			return fmt.Errorf("%w: music %d terminal=%s reason=%s",
				lyricsrecovery.ErrSekaipediaCanaryTerminal, diagnostic.MusicID,
				diagnostic.EnterResult, diagnostic.FallbackReasonCode)
		}
	}
	set, err := lyricsrecovery.NewAcquisitionSet(plan.PlanID, planSHA, runtime.Order, songs)
	if err != nil {
		return err
	}
	if err := lyricsrecovery.ValidateAcquisitionSetAuthorization(
		set, plan.PlanID, planSHA, plan.Execution.LiveCanaryMusicIDs, runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		return err
	}
	if err := lyricsrecovery.PublishAcquisitionSet(options.acquisitionSetPath, set); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output,
		"PASS mode=live-canary songs=%d setSha256=%s enterResult=%s exactRevisionEvidence=true network=authorized migration=HOLD\n",
		len(songs), set.SetSHA256, lyricsrecovery.ProviderOutcomeCompleteCompositionStop)
	return err
}

func runAcquisition(
	ctx context.Context,
	output io.Writer,
	options options,
	plan lyricsextractionplan.RecoveryPlan,
	planSHA string,
	catalog *checkedRecoveryCatalog,
	runtime lyricsrecovery.RuntimeConfig,
	musicIDs []int,
) (resultErr error) {
	if runtime.SekaipediaCanary == nil ||
		runtime.SekaipediaCanary.ListAcquisitionID != plan.SekaipediaCanary.List.AcquisitionID {
		return errors.New("acquisition runtime lost the immutable exact List replay identity")
	}
	source, err := readExactAcquisitionReplaySource(
		ctx,
		options.sekaipediaListReplayLedgerPath,
		exactReplayRuntimeCopyPath(options.ledgerPath),
		lyricsacquisition.AcquisitionID(runtime.SekaipediaCanary.ListAcquisitionID),
	)
	if err != nil {
		return err
	}
	ownership, err := acquireRecoveryLiveOwnership()
	if err != nil {
		if errors.Is(err, lyricsprovidercoord.ErrHold) {
			_, writeErr := fmt.Fprintf(output, "HOLD mode=%s liveOwnership=HOLD network=HOLD\n", options.mode)
			return writeErr
		}
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ownership.Close()) }()

	ledger, err := lyricsacquisition.CreateLedger(ctx, options.ledgerPath)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ledger.Close()) }()
	live, err := newRecoveryLiveTransports(runtime, ownership)
	if err != nil {
		return err
	}
	listReplay, err := lyricsrecovery.NewRepeatablePlanBoundSekaipediaListReplayTransport(
		source, runtime.SekaipediaCanary.List, live[model.LyricsSourceProviderSekaipedia],
	)
	if err != nil {
		return err
	}
	live[model.LyricsSourceProviderSekaipedia] = listReplay
	session, err := lyricsrecovery.NewAcquisitionSession(runtime, ledger, live)
	if err != nil {
		return err
	}
	songs := make([]lyricsrecovery.SongAcquisitionSet, 0, len(musicIDs))
	for _, musicID := range musicIDs {
		identity, err := catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			return err
		}
		providers, progress, err := session.AcquireSong(ctx, musicID, identity)
		if err != nil {
			return err
		}
		for _, item := range progress {
			if item.Provider == lyricssource.ProviderMoegirlPublicExact {
				continue
			}
			if err := ownership.ResolveProvider(lyricsproviderpolicy.Provider(item.Provider)); err != nil {
				return err
			}
		}
		songs = append(songs, lyricsrecovery.SongAcquisitionSet{MusicID: musicID, Providers: providers})
		for _, item := range progress {
			if _, err := fmt.Fprintf(output, "PASS mode=%s musicId=%d provider=%s acquisitions=%d status=%s retryable=%t\n",
				options.mode, item.MusicID, item.Provider, item.AcquisitionCount, item.Status, item.Retryable); err != nil {
				return err
			}
		}
	}
	expectedListReplays := 0
	for _, musicID := range musicIDs {
		providerOrder, err := runtime.ProviderOrderForMusicID(musicID)
		if err != nil {
			return err
		}
		if len(providerOrder) > 0 && providerOrder[0] == lyricssource.ProviderSekaipedia {
			expectedListReplays++
		}
	}
	if listReplay.ReplayCount() != expectedListReplays {
		return errors.New("acquisition did not replay the exact Sekaipedia List once per authorized Sekaipedia song")
	}
	sort.Slice(songs, func(left, right int) bool { return songs[left].MusicID < songs[right].MusicID })
	set, err := lyricsrecovery.NewAcquisitionSet(plan.PlanID, planSHA, runtime.Order, songs)
	if err != nil {
		return err
	}
	if err := lyricsrecovery.ValidateAcquisitionSetAuthorization(
		set, plan.PlanID, planSHA, musicIDs, runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		return err
	}
	if err := lyricsrecovery.PublishAcquisitionSet(options.acquisitionSetPath, set); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "PASS mode=%s songs=%d listReplays=%d setSha256=%s outcomes=HOLD migration=HOLD\n",
		options.mode, len(songs), listReplay.ReplayCount(), set.SetSHA256)
	return err
}

func readParentRoot(path string, plan lyricsextractionplan.RecoveryPlan) (*lyricsrootmanifest.Manifest, error) {
	if plan.Scope.Kind == lyricsextractionplan.RecoveryScopeFinal {
		if path != "" {
			return nil, errors.New("final recovery root rejects a parent")
		}
		return nil, nil
	}
	if path == "" {
		return nil, errors.New("partial or retry recovery root requires -parent-root")
	}
	body, err := readPinnedPrivateFile(path, lyricsrootmanifest.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	parent, err := lyricsrootmanifest.DecodeCanonical(body)
	if err != nil || parent.RootID != plan.Scope.SupersedesRootID || parent.RootSHA256 != plan.Scope.SupersedesRootSHA256 {
		return nil, errors.New("parent root does not match the immutable plan supersession")
	}
	return &parent, nil
}
