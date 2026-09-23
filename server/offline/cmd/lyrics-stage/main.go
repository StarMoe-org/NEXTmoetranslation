package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricscompose"
	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

const (
	maxConcurrency                        = 16
	maxAttempts                           = 5
	defaultRequestTimeout                 = 8 * time.Minute
	maxRequestTimeout                     = 10 * time.Minute
	maximumCompatibleCatalogRuntimeSchema = 36
)

var sqliteSidecarSuffixes = [...]string{"-wal", "-shm", "-journal"}

type options struct {
	ReportPath                string
	DatabasePath              string
	OutputPath                string
	EvidenceReceiptOutputPath string
	Concurrency               int
	MaxAttempts               int
	RequestTimeout            time.Duration
	RetryDelay                time.Duration
}

type sourceClient interface {
	FetchFixedCandidateRevision(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error)
}

type catalogSnapshotLoader func(context.Context, string) ([]catalogSnapshotItem, error)

type sourceClientFactory func() (sourceClient, error)

type draftBuilder func(lyricsstaging.PreflightItem, lyricscontract.CatalogIdentity, lyricsstaging.FixedArtifactBundle) (lyricsstaging.Draft, error)

type commandDependencies struct {
	NewSourceClient sourceClientFactory
	LoadCatalog     catalogSnapshotLoader
	BuildDraft      draftBuilder
}

func defaultCommandDependencies() commandDependencies {
	return commandDependencies{
		NewSourceClient: func() (sourceClient, error) {
			return lyricssource.DefaultRegistry()
		},
		LoadCatalog: loadCatalogSnapshot,
		BuildDraft:  lyricsstaging.BuildDraftFromFixedArtifacts,
	}
}

type fetchJob struct {
	item               lyricsstaging.PreflightItem
	hydratedCandidates []lyricssource.Candidate
}

type fetchResult struct {
	draft lyricsstaging.Draft
	err   error
}

type stagingResult struct {
	manifest              lyricsstaging.Manifest
	reportSHA256          string
	evidenceReceipt       lyricsstaging.PrivateEvidenceReceipt
	evidenceReceiptSHA256 string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lyrics stage: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer) error {
	return runContext(context.Background(), arguments, stdout)
}

func runContext(ctx context.Context, arguments []string, stdout io.Writer) error {
	return runContextWithDependencies(ctx, arguments, stdout, defaultCommandDependencies())
}

func runContextWithDependencies(ctx context.Context, arguments []string, stdout io.Writer, dependencies commandDependencies) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if dependencies.NewSourceClient == nil || dependencies.LoadCatalog == nil || dependencies.BuildDraft == nil {
		return errors.New("lyrics stage command dependencies are required")
	}
	flags := flag.NewFlagSet("lyrics-stage", flag.ContinueOnError)
	if stdout == nil {
		flags.SetOutput(io.Discard)
	} else {
		flags.SetOutput(stdout)
	}
	reportPath := flags.String("report", "", "complete lyrics-preflight JSON report")
	databasePath := flags.String("db", "", fmt.Sprintf("existing local schema-v%d through schema-v%d SQLite snapshot with the pinned v%d catalog contract, opened read-only", lyricsstaging.CatalogSchemaVersion, maximumCompatibleCatalogRuntimeSchema, lyricsstaging.CatalogSchemaVersion))
	outputPath := flags.String("output", "", "new private local staging manifest path")
	evidenceReceiptOutputPath := flags.String("evidence-receipt-output", "", "optional new private canonical EvidenceReceipt-v1 path for lyrics-import-stage -evidence-receipt")
	concurrency := flags.Int("concurrency", 4, "bounded fixed-revision fetch concurrency")
	attempts := flags.Int("max-attempts", 3, "maximum attempts per fixed-revision fetch")
	requestTimeout := flags.Duration("request-timeout", defaultRequestTimeout, "timeout for each fixed-revision operation, including client-side limiter waits")
	retryDelay := flags.Duration("retry-delay", 250*time.Millisecond, "initial retry delay")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	opts := options{
		ReportPath: *reportPath, DatabasePath: *databasePath, OutputPath: *outputPath,
		EvidenceReceiptOutputPath: *evidenceReceiptOutputPath,
		Concurrency:               *concurrency, MaxAttempts: *attempts, RequestTimeout: *requestTimeout, RetryDelay: *retryDelay,
	}
	if err := validateOptions(opts); err != nil {
		return err
	}
	source, err := dependencies.NewSourceClient()
	if err != nil {
		return fmt.Errorf("configure lyrics source providers: %w", err)
	}
	if source == nil {
		return errors.New("configure lyrics source providers: registry is required")
	}
	result, err := executeStagingWithDependencies(ctx, opts, source, dependencies.LoadCatalog, dependencies.BuildDraft)
	if err != nil {
		return err
	}
	if result.manifest.Preflight.ReportSHA256 != result.reportSHA256 {
		return errors.New("staging manifest lost its validated preflight report digest binding")
	}
	if result.evidenceReceipt.ReceiptSHA256 != result.evidenceReceiptSHA256 {
		return errors.New("projected stage evidence receipt digest changed before publication")
	}
	if opts.EvidenceReceiptOutputPath != "" {
		return writeManifestEvidencePairContext(
			ctx,
			opts.OutputPath,
			result.manifest,
			opts.EvidenceReceiptOutputPath,
			result.evidenceReceipt,
			result.evidenceReceiptSHA256,
		)
	}
	return writeManifestContext(ctx, opts.OutputPath, result.manifest, stdout)
}

func validateOptions(opts options) error {
	for name, value := range map[string]string{"-report": opts.ReportPath, "-db": opts.DatabasePath, "-output": opts.OutputPath} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if value != strings.TrimSpace(value) {
			return fmt.Errorf("%s must not have surrounding whitespace", name)
		}
	}
	if opts.ReportPath == "-" || opts.DatabasePath == "-" {
		return errors.New("-report and -db must identify existing regular files")
	}
	if opts.EvidenceReceiptOutputPath != strings.TrimSpace(opts.EvidenceReceiptOutputPath) {
		return errors.New("-evidence-receipt-output must not have surrounding whitespace")
	}
	if opts.EvidenceReceiptOutputPath == "-" {
		return errors.New("private evidence receipt requires a private file path; stdout is not supported")
	}
	if opts.Concurrency < 1 || opts.Concurrency > maxConcurrency {
		return fmt.Errorf("-concurrency must be between 1 and %d", maxConcurrency)
	}
	if opts.MaxAttempts < 1 || opts.MaxAttempts > maxAttempts {
		return fmt.Errorf("-max-attempts must be between 1 and %d", maxAttempts)
	}
	if opts.RequestTimeout <= 0 || opts.RequestTimeout > maxRequestTimeout {
		return errors.New("-request-timeout must be positive and at most 10m")
	}
	if opts.RetryDelay < 0 || opts.RetryDelay > 30*time.Second {
		return errors.New("-retry-delay must be between 0 and 30s")
	}
	return validatePaths(opts)
}

func validatePaths(opts options) error {
	reportPath, reportInfo, err := existingRegularFile(opts.ReportPath, "preflight report")
	if err != nil {
		return err
	}
	databasePath, databaseInfo, err := existingRegularFile(opts.DatabasePath, "local database")
	if err != nil {
		return err
	}
	if err := rejectSQLiteSidecars(databasePath); err != nil {
		return err
	}
	if filepath.Clean(reportPath) == filepath.Clean(databasePath) || os.SameFile(reportInfo, databaseInfo) {
		return errors.New("preflight report path must not be the local database path")
	}
	if opts.OutputPath == "-" {
		return errors.New("sensitive staging manifest requires a private file path; stdout is not supported")
	}
	outputPath, err := validatePrivateOutputPath(opts.OutputPath, "staging manifest")
	if err != nil {
		return err
	}
	if filepath.Clean(outputPath) == filepath.Clean(reportPath) || filepath.Clean(outputPath) == filepath.Clean(databasePath) {
		return errors.New("staging output path must be distinct from all inputs")
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(outputPath))
	if err != nil {
		return fmt.Errorf("resolve staging output directory: %w", err)
	}
	resolvedOutput := filepath.Join(resolvedParent, filepath.Base(outputPath))
	resolvedReport, err := filepath.EvalSymlinks(reportPath)
	if err != nil {
		return fmt.Errorf("resolve preflight report: %w", err)
	}
	resolvedDatabase, err := filepath.EvalSymlinks(databasePath)
	if err != nil {
		return fmt.Errorf("resolve local database: %w", err)
	}
	if resolvedOutput == resolvedReport || resolvedOutput == resolvedDatabase {
		return errors.New("staging output path must not resolve to an input path")
	}
	if opts.EvidenceReceiptOutputPath == "" {
		return requireNewPrivateOutputPath(outputPath, "staging manifest")
	}
	receiptOutput, err := validatePrivateOutputPath(opts.EvidenceReceiptOutputPath, "private evidence receipt")
	if err != nil {
		return err
	}
	if filepath.Clean(receiptOutput) == filepath.Clean(reportPath) ||
		filepath.Clean(receiptOutput) == filepath.Clean(databasePath) ||
		filepath.Clean(receiptOutput) == filepath.Clean(outputPath) {
		return errors.New("private evidence receipt output path must be distinct from all inputs and the staging manifest")
	}
	resolvedReceiptParent, err := filepath.EvalSymlinks(filepath.Dir(receiptOutput))
	if err != nil {
		return fmt.Errorf("resolve private evidence receipt output directory: %w", err)
	}
	resolvedReceipt := filepath.Join(resolvedReceiptParent, filepath.Base(receiptOutput))
	if resolvedReceipt == resolvedReport || resolvedReceipt == resolvedDatabase || resolvedReceipt == resolvedOutput {
		return errors.New("private evidence receipt output path must not resolve to an input or the staging manifest")
	}
	return requireAbsentPublicationPair(outputPath, receiptOutput)
}

func validatePrivateOutputPath(path, label string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s output path: %w", label, err)
	}
	parentInfo, err := os.Stat(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("inspect %s output directory: %w", label, err)
	}
	if !parentInfo.IsDir() {
		return "", fmt.Errorf("%s output parent must be a directory", label)
	}
	return absolute, nil
}

func requireNewPrivateOutputPath(path, label string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("create new %s: path already exists", label)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s output path: %w", label, err)
	}
	return nil
}

func requireAbsentPublicationPair(manifestPath, receiptPath string) error {
	manifestExists, err := pathEntryExists(manifestPath)
	if err != nil {
		return fmt.Errorf("inspect staging manifest publication path: %w", err)
	}
	receiptExists, err := pathEntryExists(receiptPath)
	if err != nil {
		return fmt.Errorf("inspect private evidence receipt publication path: %w", err)
	}
	if manifestExists != receiptExists {
		return fmt.Errorf(
			"incomplete staging publication pair exists (manifest=%t evidenceReceipt=%t); refusing to overwrite or delete either artifact",
			manifestExists,
			receiptExists,
		)
	}
	if manifestExists {
		return errors.New("create new staging manifest and evidence receipt pair: path already exists")
	}
	return nil
}

func pathEntryExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func existingRegularFile(path, label string) (string, os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s path: %w", label, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s path must identify a regular file", label)
	}
	return absolute, info, nil
}

func rejectSQLiteSidecars(databasePath string) error {
	basePaths, err := sqliteSidecarBasePaths(databasePath)
	if err != nil {
		return err
	}
	return rejectSQLiteSidecarsAtPaths(basePaths)
}

func sqliteSidecarBasePaths(databasePath string) ([]string, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve local database path for SQLite sidecar inspection: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve local database for SQLite sidecar inspection: %w", err)
	}
	basePaths := []string{absolute}
	if resolved != absolute {
		basePaths = append(basePaths, resolved)
	}
	return basePaths, nil
}

func rejectSQLiteSidecarsAtPaths(basePaths []string) error {
	seen := make(map[string]struct{}, len(basePaths))
	for _, basePath := range basePaths {
		if _, exists := seen[basePath]; exists {
			continue
		}
		seen[basePath] = struct{}{}
		for _, suffix := range sqliteSidecarSuffixes {
			sidecarPath := basePath + suffix
			if _, err := os.Lstat(sidecarPath); err == nil {
				return fmt.Errorf("local database must be a standalone offline SQLite snapshot: %s sidecar exists", suffix)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect local database %s sidecar: %w", suffix, err)
			}
		}
	}
	return nil
}

func execute(ctx context.Context, opts options, source sourceClient) (lyricsstaging.Manifest, error) {
	return executeWithDependencies(ctx, opts, source, loadCatalogSnapshot, lyricsstaging.BuildDraftFromFixedArtifacts)
}

// executeWithCatalogLoader is a narrow test seam proving that no fixed-source
// request is admitted until catalog loading has returned. The production loader
// closes and verifies its pinned immutable SQLite snapshot before returning.
func executeWithCatalogLoader(ctx context.Context, opts options, source sourceClient, loader catalogSnapshotLoader) (lyricsstaging.Manifest, error) {
	return executeWithDependencies(ctx, opts, source, loader, lyricsstaging.BuildDraftFromFixedArtifacts)
}

// executeWithDependencies keeps the provider fetch and private artifact builder
// independently injectable so the command can adopt the evolving T2/T3 wire
// adapter without changing catalog validation, retry, or publication logic.
func executeWithDependencies(ctx context.Context, opts options, source sourceClient, loader catalogSnapshotLoader, buildDraft draftBuilder) (lyricsstaging.Manifest, error) {
	result, err := executeStagingWithDependencies(ctx, opts, source, loader, buildDraft)
	if err != nil {
		return lyricsstaging.Manifest{}, err
	}
	return result.manifest, nil
}

func executeStagingWithDependencies(ctx context.Context, opts options, source sourceClient, loader catalogSnapshotLoader, buildDraft draftBuilder) (stagingResult, error) {
	if ctx == nil || source == nil {
		return stagingResult{}, errors.New("context and lyrics source client are required")
	}
	if loader == nil || buildDraft == nil {
		return stagingResult{}, errors.New("catalog snapshot loader and draft builder are required")
	}
	if err := ctx.Err(); err != nil {
		return stagingResult{}, err
	}
	if err := validateOptions(opts); err != nil {
		return stagingResult{}, err
	}
	reportBody, reportSHA, err := readBoundedFile(opts.ReportPath, lyricsstaging.MaxPreflightReportBytes, "preflight report")
	if err != nil {
		return stagingResult{}, err
	}
	report, evidenceResolver, err := lyricsstaging.DecodePreflightWithEvidenceResolver(reportBody)
	if err != nil {
		return stagingResult{}, err
	}
	if err := requireCanonicalPreflightBytes(reportBody, report); err != nil {
		return stagingResult{}, err
	}
	if report.EvidenceReceipt == nil || evidenceResolver == nil {
		return stagingResult{}, errors.New("complete preflight report requires its private evidence receipt")
	}
	if len(report.UniqueComplete) == 0 {
		return stagingResult{}, errors.New("complete preflight report contains no unique_complete items")
	}
	fullReceiptSHA256 := report.EvidenceReceipt.ReceiptSHA256
	identities, err := validateCatalogSnapshotWithLoader(ctx, opts.DatabasePath, report, loader)
	if err != nil {
		return stagingResult{}, err
	}
	drafts, err := fetchDrafts(ctx, opts, source, report, identities, evidenceResolver, buildDraft)
	if err != nil {
		return stagingResult{}, err
	}
	if err := requireCanonicalPreflightBytes(reportBody, report); err != nil {
		return stagingResult{}, fmt.Errorf("complete preflight report changed during staging: %w", err)
	}
	if report.EvidenceReceipt == nil || report.EvidenceReceipt.ReceiptSHA256 != fullReceiptSHA256 {
		return stagingResult{}, errors.New("complete preflight evidence receipt digest changed during staging")
	}
	manifest, err := lyricsstaging.NewManifestFromValidatedPreflight(report, reportSHA, drafts)
	if err != nil {
		return stagingResult{}, err
	}
	if manifest.Preflight.ReportSHA256 != reportSHA {
		return stagingResult{}, errors.New("staging manifest does not bind the exact validated preflight report digest")
	}
	manifestCandidates, err := lyricsstaging.EvidenceCandidatesFromValidatedManifest(manifest)
	if err != nil {
		return stagingResult{}, err
	}
	projectedReceipt, err := evidenceResolver.ProjectReceipt(manifestCandidates)
	if err != nil {
		return stagingResult{}, fmt.Errorf("project manifest-reachable private evidence: %w", err)
	}
	projectedReceiptSHA256 := projectedReceipt.ReceiptSHA256
	return stagingResult{
		manifest: manifest, reportSHA256: reportSHA,
		evidenceReceipt: projectedReceipt, evidenceReceiptSHA256: projectedReceiptSHA256,
	}, nil
}

func requireCanonicalPreflightBytes(body []byte, report lyricsstaging.PreflightReport) error {
	return lyricsstaging.RequireCanonicalPreflightBytes(body, report)
}

func fetchDrafts(ctx context.Context, opts options, source sourceClient, report lyricsstaging.PreflightReport,
	identities map[int]lyricscontract.CatalogIdentity, evidenceResolver *lyricsstaging.PrivateEvidenceResolver,
	buildDraft draftBuilder,
) ([]lyricsstaging.Draft, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	preparedJobs, err := hydrateFetchJobs(report.UniqueComplete, evidenceResolver)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobs := make(chan fetchJob)
	results := make(chan fetchResult, opts.Concurrency)
	workers := opts.Concurrency
	if workers > len(preparedJobs) {
		workers = len(preparedJobs)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workerGroup sync.WaitGroup
	workerGroup.Add(workers)
	for range workers {
		go func() {
			defer workerGroup.Done()
			for job := range jobs {
				identity := identities[job.item.MusicID]
				draft, err := fetchAndBuildDraft(
					workCtx, opts, source, job.item, identity, job.hydratedCandidates, evidenceResolver, buildDraft,
				)
				if err != nil {
					err = fmt.Errorf("music %d: %w", job.item.MusicID, err)
				}
				select {
				case <-workCtx.Done():
					return
				case results <- fetchResult{draft: draft, err: err}:
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(results)
		for _, job := range preparedJobs {
			select {
			case <-workCtx.Done():
				close(jobs)
				workerGroup.Wait()
				return
			case jobs <- job:
			}
		}
		close(jobs)
		workerGroup.Wait()
	}()
	drafts := make([]lyricsstaging.Draft, 0, len(report.UniqueComplete))
	var firstErr error
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
			cancel()
			continue
		}
		if firstErr == nil {
			drafts = append(drafts, result.draft)
		}
	}
	// Parent cancellation takes precedence over worker errors or a misleading
	// partial-manifest count failure. Internal cancellation after a worker error
	// does not affect ctx, so the original worker error is still returned below.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return drafts, nil
}

// hydrateFetchJobs resolves the complete report as one receipt-wide batch, so
// shared immutable authority evidence receives one defensive raw clone rather
// than one clone per song. The resulting candidates are then partitioned back
// into item-local work without copying their raw slices.
func hydrateFetchJobs(
	items []lyricsstaging.PreflightItem,
	evidenceResolver *lyricsstaging.PrivateEvidenceResolver,
) ([]fetchJob, error) {
	if evidenceResolver == nil {
		return nil, errors.New("private evidence resolver is required")
	}
	type candidateRange struct {
		start int
		end   int
	}
	ranges := make([]candidateRange, len(items))
	candidates := make([]lyricsstaging.CandidateIdentity, 0, len(items))
	jobs := make([]fetchJob, len(items))
	for index, item := range items {
		itemCandidates := item.FixedArtifactCandidates
		if len(itemCandidates) == 0 {
			if item.Candidate == nil {
				return nil, fmt.Errorf("music %d: unique_complete item has no fixed artifact candidate", item.MusicID)
			}
			itemCandidates = []lyricsstaging.CandidateIdentity{*item.Candidate}
		}
		ranges[index].start = len(candidates)
		candidates = append(candidates, itemCandidates...)
		ranges[index].end = len(candidates)
		jobs[index].item = item
	}
	hydrated, err := evidenceResolver.HydrateCandidates(candidates)
	if err != nil {
		return nil, fmt.Errorf("hydrate all exact artifact candidates: %w", err)
	}
	for index := range jobs {
		candidateRange := ranges[index]
		jobs[index].hydratedCandidates = hydrated[candidateRange.start:candidateRange.end:candidateRange.end]
	}
	return jobs, nil
}

func fetchAndBuildDraft(
	ctx context.Context,
	opts options,
	source sourceClient,
	item lyricsstaging.PreflightItem,
	identity lyricscontract.CatalogIdentity,
	hydratedCandidates []lyricssource.Candidate,
	evidenceResolver *lyricsstaging.PrivateEvidenceResolver,
	buildDraft draftBuilder,
) (lyricsstaging.Draft, error) {
	candidates := append([]lyricsstaging.CandidateIdentity{}, item.FixedArtifactCandidates...)
	if len(candidates) == 0 {
		if item.Candidate == nil {
			return lyricsstaging.Draft{}, errors.New("unique_complete item has no fixed artifact candidate")
		}
		candidates = []lyricsstaging.CandidateIdentity{*item.Candidate}
	}
	artifactKeys, err := lyricsstaging.ResolveArtifactRenditionKeys(candidates)
	if err != nil {
		return lyricsstaging.Draft{}, err
	}
	if len(hydratedCandidates) != len(candidates) {
		return lyricsstaging.Draft{}, errors.New("hydrated exact artifact candidate count does not match preflight")
	}
	if evidenceResolver == nil {
		return lyricsstaging.Draft{}, errors.New("private evidence resolver is required")
	}
	artifacts := make([]lyricsstaging.FixedArtifact, len(candidates))
	compositionInputs := make([]lyricscompose.FixedArtifactInput, len(candidates))
	for index, candidate := range candidates {
		hydrated := hydratedCandidates[index]
		fixed, err := retryFetch(ctx, opts, func(attemptCtx context.Context) (lyricssource.FixedRevision, error) {
			return source.FetchFixedCandidateRevision(attemptCtx, identity.SourceIdentity(), hydrated)
		})
		if err != nil {
			return lyricsstaging.Draft{}, fmt.Errorf("fetch artifact %q: %w", artifactKeys[index], err)
		}
		artifacts[index] = lyricsstaging.FixedArtifact{Candidate: candidate, Fixed: fixed}
		compositionInputs[index] = lyricscompose.FixedArtifactInput{
			SourceKey: artifactKeys[index], LogicalRenditionKey: candidate.RenditionKey, Fixed: fixed,
		}
	}
	composition, err := lyricscompose.ComposeFixedArtifacts(compositionInputs)
	if err != nil {
		return lyricsstaging.Draft{}, fmt.Errorf("compose exact artifacts: %w", err)
	}
	if item.CompositionReason == "" || composition.ReasonCode != item.CompositionReason ||
		len(composition.Full.Lines) != item.LineCount {
		return lyricsstaging.Draft{}, fmt.Errorf("%w: fixed artifact composition drifted from preflight", lyricsstaging.ErrManifestRebuildRequired)
	}
	if !sameSelectedArtifactKeys(artifactKeys, composition.SelectedSourceKeys) {
		return lyricsstaging.Draft{}, fmt.Errorf("%w: preflight retained artifacts without final component contributions", lyricsstaging.ErrManifestRebuildRequired)
	}
	primaryIndex := -1
	for index, candidate := range candidates {
		if artifactKeys[index] == composition.Components.FullText {
			if item.Candidate == nil || !reflect.DeepEqual(candidate, *item.Candidate) {
				return lyricsstaging.Draft{}, fmt.Errorf("%w: authoritative Full artifact drifted from preflight", lyricsstaging.ErrManifestRebuildRequired)
			}
			primaryIndex = index
			break
		}
	}
	if primaryIndex < 0 {
		return lyricsstaging.Draft{}, fmt.Errorf("%w: composition has no retained authoritative Full artifact", lyricsstaging.ErrManifestRebuildRequired)
	}
	boundPrimary, err := lyricscompose.BindFixedArtifactComposition(compositionInputs[primaryIndex], composition)
	if err != nil {
		return lyricsstaging.Draft{}, err
	}
	artifacts[primaryIndex].Fixed = boundPrimary
	bundle := lyricsstaging.FixedArtifactBundle{
		PostFetchState: lyricsstaging.PostFetchStateComplete, CompositionReason: composition.ReasonCode,
		Artifacts: artifacts, EvidenceResolver: evidenceResolver,
		Components: lyricsstaging.FixedArtifactComponentSelection{
			FullText:              composition.Components.FullText,
			PerformerSegmentation: composition.Components.PerformerSegmentation,
			GameProjection:        composition.Components.GameProjection,
			Ruby:                  composition.Components.Ruby,
			VersionEvidence:       composition.Components.VersionEvidence,
		},
	}
	return buildDraft(item, identity, bundle)
}

func sameSelectedArtifactKeys(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, key := range actual {
		actualSet[key] = struct{}{}
	}
	if len(actualSet) != len(actual) {
		return false
	}
	for _, key := range expected {
		if _, found := actualSet[key]; !found {
			return false
		}
	}
	return true
}

func retryFetch(ctx context.Context, opts options, operation func(context.Context) (lyricssource.FixedRevision, error)) (lyricssource.FixedRevision, error) {
	var last lyricssource.FixedRevision
	var lastErr error
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return lyricssource.FixedRevision{}, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, opts.RequestTimeout)
		value, operationErr := operation(attemptCtx)
		attemptErr := attemptCtx.Err()
		if attemptErr == nil {
			if deadline, ok := attemptCtx.Deadline(); ok && !time.Now().Before(deadline) {
				attemptErr = context.DeadlineExceeded
			}
		}
		parentErr := ctx.Err()
		cancel()
		switch {
		case attemptErr != nil:
			last = lyricssource.FixedRevision{}
			lastErr = attemptErr
		case parentErr != nil:
			last = lyricssource.FixedRevision{}
			lastErr = parentErr
		default:
			last = value
			lastErr = operationErr
		}
		if lastErr == nil || !retryableSourceError(lastErr) || attempt == opts.MaxAttempts {
			return last, lastErr
		}
		delay := retryDelay(opts.RetryDelay, attempt)
		if delay == 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return lyricssource.FixedRevision{}, ctx.Err()
		case <-timer.C:
		}
	}
	return last, lastErr
}

func retryDelay(base time.Duration, completedAttempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base
	for step := 1; step < completedAttempt; step++ {
		if delay >= 15*time.Second {
			return 30 * time.Second
		}
		delay *= 2
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func retryableSourceError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch {
	case errors.Is(err, lyricssource.ErrRestrictedReprint), errors.Is(err, lyricssource.ErrAmbiguous),
		errors.Is(err, lyricssource.ErrMissingLyrics), errors.Is(err, lyricssource.ErrRevisionChanged),
		errors.Is(err, lyricssource.ErrUnsupportedTable), errors.Is(err, lyricssource.ErrLyricsTooLarge),
		errors.Is(err, lyricssource.ErrMalformedResponse):
		return false
	}
	var httpError *lyricssource.HTTPError
	if errors.As(err, &httpError) {
		return httpError.StatusCode == 429 || httpError.StatusCode >= 500
	}
	var networkError interface{ Timeout() bool }
	if errors.As(err, &networkError) {
		return true
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return true
	}
	return true
}

type catalogSnapshotItem struct {
	MusicID            int
	JapaneseTitle      string
	ProducerMetadata   string
	Lyricist           string
	Composer           string
	Arranger           string
	Evidence           model.CatalogLyricsEvidence
	CatalogFingerprint string
}

type classifiedPreflightItem struct {
	class string
	item  lyricsstaging.PreflightItem
}

func validateCatalogSnapshot(ctx context.Context, databasePath string, report lyricsstaging.PreflightReport) (map[int]lyricscontract.CatalogIdentity, error) {
	return validateCatalogSnapshotWithLoader(ctx, databasePath, report, loadCatalogSnapshot)
}

func validateCatalogSnapshotWithLoader(ctx context.Context, databasePath string, report lyricsstaging.PreflightReport,
	loader catalogSnapshotLoader,
) (map[int]lyricscontract.CatalogIdentity, error) {
	if loader == nil {
		return nil, errors.New("catalog snapshot loader is required")
	}
	catalog, err := loader(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	reportItems := allPreflightItems(report)
	if len(catalog) != report.CatalogCount || len(reportItems) != report.CatalogCount {
		return nil, fmt.Errorf("complete preflight catalog count %d does not match local catalog snapshot count %d", report.CatalogCount, len(catalog))
	}

	reportByMusicID := make(map[int]classifiedPreflightItem, len(reportItems))
	for _, classified := range reportItems {
		reportByMusicID[classified.item.MusicID] = classified
	}
	records := make([]model.CatalogLyricsGroupingRecord, 0, len(catalog))
	catalogByMusicID := make(map[int]catalogSnapshotItem, len(catalog))
	for _, catalogItem := range catalog {
		classified, exists := reportByMusicID[catalogItem.MusicID]
		if !exists {
			return nil, fmt.Errorf("local catalog music %d is absent from the complete preflight report", catalogItem.MusicID)
		}
		if classified.item.JapaneseTitle != catalogItem.JapaneseTitle {
			return nil, fmt.Errorf("complete preflight music %d title does not match the local catalog snapshot", catalogItem.MusicID)
		}
		if classified.item.CatalogFingerprint != catalogItem.CatalogFingerprint {
			return nil, fmt.Errorf("complete preflight music %d fingerprint does not match the local catalog snapshot", catalogItem.MusicID)
		}
		records = append(records, model.CatalogLyricsGroupingRecord{
			MusicID: catalogItem.MusicID, Fingerprint: catalogItem.CatalogFingerprint, Evidence: catalogItem.Evidence,
		})
		catalogByMusicID[catalogItem.MusicID] = catalogItem
	}
	for _, classified := range reportItems {
		if _, exists := catalogByMusicID[classified.item.MusicID]; !exists {
			return nil, fmt.Errorf("complete preflight music %d is missing from the local catalog snapshot", classified.item.MusicID)
		}
	}

	targets := model.ClassifyCatalogLyricsTargets(records)
	if len(targets) != len(catalog) {
		return nil, errors.New("local catalog classifier did not return every music record")
	}
	for _, target := range targets {
		classified, exists := reportByMusicID[target.MusicID]
		if !exists {
			return nil, fmt.Errorf("local catalog classifier returned unknown music %d", target.MusicID)
		}
		if target.CatalogFingerprint != classified.item.CatalogFingerprint ||
			target.TargetMusicID != classified.item.TargetMusicID ||
			!equalOrderedMusicIDs(target.AssociationMusicIDs, classified.item.AssociationMusicIDs) ||
			!preflightClassMatchesTarget(classified.class, classified.item, target) {
			return nil, fmt.Errorf("complete preflight music %d classification does not match the local catalog snapshot", target.MusicID)
		}
	}

	identities := make(map[int]lyricscontract.CatalogIdentity, len(report.UniqueComplete))
	for _, item := range report.UniqueComplete {
		catalogItem := catalogByMusicID[item.MusicID]
		identities[item.MusicID] = lyricscontract.CatalogIdentity{
			MusicID: catalogItem.MusicID, JapaneseTitle: catalogItem.JapaneseTitle,
			ProducerMetadata: catalogItem.ProducerMetadata, Lyricist: catalogItem.Lyricist,
			Composer: catalogItem.Composer, Arranger: catalogItem.Arranger,
			Vocals:             append([]model.CatalogVocalSignal{}, catalogItem.Evidence.Vocals...),
			CatalogFingerprint: catalogItem.CatalogFingerprint,
		}
	}
	return identities, nil
}

var requiredCatalogContractColumns = map[string]struct {
	typeName string
	notNull  bool
	primary  bool
}{
	"music_id":                      {typeName: "INTEGER", primary: true},
	"title_ja":                      {typeName: "TEXT", notNull: true},
	"producer_metadata":             {typeName: "TEXT", notNull: true},
	"lyricist":                      {typeName: "TEXT", notNull: true},
	"composer":                      {typeName: "TEXT", notNull: true},
	"arranger":                      {typeName: "TEXT", notNull: true},
	"assetbundle_name":              {typeName: "TEXT", notNull: true},
	"version_hint":                  {typeName: "TEXT", notNull: true},
	"lyrics_version":                {typeName: "TEXT", notNull: true},
	"lyrics_evidence_presence_json": {typeName: "TEXT", notNull: true},
	"vocal_signals_json":            {typeName: "TEXT", notNull: true},
	"lyrics_catalog_fingerprint":    {typeName: "TEXT", notNull: true},
	"lyrics_catalog_policy_version": {typeName: "TEXT", notNull: true},
}

func validateCatalogRuntimeContract(ctx context.Context, transaction *sql.Tx) error {
	var minimumVersion, maximumVersion, versionCount int
	if err := transaction.QueryRowContext(ctx, `SELECT COALESCE(MIN(version),0), COALESCE(MAX(version),0), COUNT(*) FROM schema_migrations`).
		Scan(&minimumVersion, &maximumVersion, &versionCount); err != nil {
		return fmt.Errorf("read local database schema version: %w", err)
	}
	if minimumVersion != 1 || versionCount != maximumVersion ||
		maximumVersion < lyricsstaging.CatalogSchemaVersion || maximumVersion > maximumCompatibleCatalogRuntimeSchema {
		return fmt.Errorf("local database schema must be a contiguous v%d through v%d history with the pinned v%d catalog contract",
			lyricsstaging.CatalogSchemaVersion, maximumCompatibleCatalogRuntimeSchema, lyricsstaging.CatalogSchemaVersion)
	}

	rows, err := transaction.QueryContext(ctx, `PRAGMA table_info('catalog_music')`)
	if err != nil {
		return fmt.Errorf("inspect local catalog contract columns: %w", err)
	}
	found := make(map[string]struct {
		typeName string
		notNull  bool
		primary  bool
	})
	for rows.Next() {
		var columnID, notNull, primary int
		var name, typeName string
		var defaultValue any
		if err := rows.Scan(&columnID, &name, &typeName, &notNull, &defaultValue, &primary); err != nil {
			rows.Close()
			return fmt.Errorf("inspect local catalog contract column: %w", err)
		}
		if _, required := requiredCatalogContractColumns[name]; required {
			found[name] = struct {
				typeName string
				notNull  bool
				primary  bool
			}{typeName: strings.ToUpper(strings.TrimSpace(typeName)), notNull: notNull == 1, primary: primary == 1}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect local catalog contract columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close local catalog contract columns: %w", err)
	}
	for name, required := range requiredCatalogContractColumns {
		actual, exists := found[name]
		if !exists || actual.typeName != required.typeName || required.notNull && !actual.notNull ||
			required.primary && !actual.primary {
			return fmt.Errorf("local database does not satisfy the pinned v%d catalog contract column %s",
				lyricsstaging.CatalogSchemaVersion, name)
		}
	}
	return nil
}

func loadCatalogSnapshot(ctx context.Context, databasePath string) ([]catalogSnapshotItem, error) {
	return loadCatalogSnapshotWithHook(ctx, databasePath, nil)
}

// loadCatalogSnapshotWithHook exposes only the post-query/pre-close boundary so
// tests can prove that mutations are rejected before any source request starts.
func loadCatalogSnapshotWithHook(ctx context.Context, databasePath string, afterLoad func() error) (items []catalogSnapshotItem, err error) {
	database, err := openReadOnlyDatabase(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			closeErr = fmt.Errorf("close read-only local database: %w", closeErr)
			if err == nil {
				err = closeErr
			} else {
				err = errors.Join(err, closeErr)
			}
		}
	}()

	transaction := database.transaction
	if err := validateCatalogRuntimeContract(ctx, transaction); err != nil {
		return nil, err
	}

	rows, err := transaction.QueryContext(ctx, `SELECT music_id,title_ja,producer_metadata,lyricist,composer,arranger,
		assetbundle_name,version_hint,lyrics_version,lyrics_evidence_presence_json,vocal_signals_json,
		lyrics_catalog_fingerprint,lyrics_catalog_policy_version FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return nil, fmt.Errorf("query local pinned-v18 catalog contract: %w", err)
	}
	lastMusicID := 0
	for rows.Next() {
		var item catalogSnapshotItem
		var assetbundle, versionHint, lyricsVersion, presenceJSON, vocalsJSON, storedFingerprint, policyVersion string
		if err := rows.Scan(&item.MusicID, &item.JapaneseTitle, &item.ProducerMetadata, &item.Lyricist,
			&item.Composer, &item.Arranger, &assetbundle, &versionHint, &lyricsVersion, &presenceJSON,
			&vocalsJSON, &storedFingerprint, &policyVersion); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan local pinned-v18 catalog contract: %w", err)
		}
		if item.MusicID <= lastMusicID {
			rows.Close()
			return nil, errors.New("local catalog snapshot music IDs are not strictly ordered")
		}
		lastMusicID = item.MusicID
		if policyVersion != model.LyricsCatalogIdentityPolicyVersion {
			rows.Close()
			return nil, fmt.Errorf("local catalog music %d has unsupported lyrics identity policy %q", item.MusicID, policyVersion)
		}
		item.Evidence = model.CatalogLyricsEvidence{
			PolicyVersion: policyVersion, Title: item.JapaneseTitle, Lyricist: item.Lyricist,
			Composer: item.Composer, Arranger: item.Arranger, Assetbundle: assetbundle,
			VersionHint: versionHint, LyricsVersion: lyricsVersion,
		}
		if err := decodeCatalogEvidenceJSON([]byte(presenceJSON), &item.Evidence.Presence); err != nil {
			rows.Close()
			return nil, fmt.Errorf("local catalog music %d has malformed evidence presence: %w", item.MusicID, err)
		}
		if err := decodeCatalogEvidenceJSON([]byte(vocalsJSON), &item.Evidence.Vocals); err != nil {
			rows.Close()
			return nil, fmt.Errorf("local catalog music %d has malformed vocal signals: %w", item.MusicID, err)
		}
		computedFingerprint, err := model.CatalogLyricsEvidenceFingerprint(item.Evidence)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("recompute local catalog music %d fingerprint: %w", item.MusicID, err)
		}
		if storedFingerprint != computedFingerprint {
			rows.Close()
			return nil, fmt.Errorf("local catalog music %d stored fingerprint does not match its lyrics evidence", item.MusicID)
		}
		item.CatalogFingerprint = computedFingerprint
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read local pinned-v18 catalog contract: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close local catalog snapshot rows: %w", err)
	}
	if afterLoad != nil {
		if err := afterLoad(); err != nil {
			return nil, fmt.Errorf("after local catalog load: %w", err)
		}
	}
	if err := database.verifyCurrent("after catalog load"); err != nil {
		return nil, err
	}
	if err := database.Commit(); err != nil {
		return nil, fmt.Errorf("commit read-only local catalog snapshot: %w", err)
	}
	return items, nil
}

func allPreflightItems(report lyricsstaging.PreflightReport) []classifiedPreflightItem {
	classes := []struct {
		name  string
		items []lyricsstaging.PreflightItem
	}{
		{name: "catalog_review", items: report.CatalogReview},
		{name: "game_size_evidence", items: report.GameSizeEvidence},
		{name: "unique_complete", items: report.UniqueComplete},
		{name: "ambiguous", items: report.Ambiguous},
		{name: "missing", items: report.Missing},
		{name: "incomplete", items: report.Incomplete},
		{name: "error", items: report.Error},
	}
	result := make([]classifiedPreflightItem, 0, report.CatalogCount)
	for _, class := range classes {
		for _, item := range class.items {
			result = append(result, classifiedPreflightItem{class: class.name, item: item})
		}
	}
	return result
}

func preflightClassMatchesTarget(class string, item lyricsstaging.PreflightItem, target model.CatalogLyricsTarget) bool {
	switch target.Disposition {
	case model.LyricsCatalogTargetReview:
		return class == "catalog_review" && item.ReasonCode == target.ReasonCode
	case model.LyricsCatalogTargetGameSizeEvidence:
		return class == "game_size_evidence" && item.ReasonCode == target.ReasonCode
	case model.LyricsCatalogTargetFullTarget:
		return class == "unique_complete" || class == "ambiguous" || class == "missing" ||
			class == "incomplete" || class == "error"
	default:
		return false
	}
}

func equalOrderedMusicIDs(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func decodeCatalogEvidenceJSON(body []byte, target any) error {
	if len(body) == 0 || target == nil {
		return errors.New("JSON body and target are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value")
	}
	return nil
}

type readOnlyDatabase struct {
	database         *sql.DB
	connection       *sql.Conn
	transaction      *sql.Tx
	file             *os.File
	absolutePath     string
	descriptorPath   string
	sidecarBasePaths []string
	info             os.FileInfo
	size             int64
	digest           [sha256.Size]byte
}

func (database *readOnlyDatabase) Commit() error {
	if database == nil || database.transaction == nil {
		return errors.New("read-only local catalog snapshot is not active")
	}
	if err := database.transaction.Commit(); err != nil {
		return err
	}
	database.transaction = nil
	return nil
}

func (database *readOnlyDatabase) Close() error {
	return database.closeWithHook(nil)
}

// closeWithHook exposes the exact post-SQLite-close/pre-descriptor-close
// boundary for immutable-snapshot verification tests.
func (database *readOnlyDatabase) closeWithHook(afterSQLiteClose func() error) error {
	if database == nil {
		return nil
	}
	var result error
	hadSQLite := database.transaction != nil || database.connection != nil || database.database != nil
	if hadSQLite && database.file != nil {
		if err := database.verifyCurrent("before close"); err != nil {
			result = errors.Join(result, err)
		}
	}
	if database.transaction != nil {
		if err := database.transaction.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			result = errors.Join(result, fmt.Errorf("rollback catalog snapshot: %w", err))
		}
		database.transaction = nil
	}
	if database.connection != nil {
		if err := database.connection.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close dedicated catalog connection: %w", err))
		}
		database.connection = nil
	}
	if database.database != nil {
		if err := database.database.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close catalog connection pool: %w", err))
		}
		database.database = nil
	}
	if hadSQLite && database.file != nil {
		if afterSQLiteClose != nil {
			if err := afterSQLiteClose(); err != nil {
				result = errors.Join(result, fmt.Errorf("after SQLite close: %w", err))
			}
		}
		if err := database.verifyCurrent("after close"); err != nil {
			result = errors.Join(result, err)
		}
	}
	if database.file != nil {
		if err := database.file.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close pinned catalog descriptor: %w", err))
		}
		database.file = nil
	}
	return result
}

func (database *readOnlyDatabase) verifyCurrent(stage string) error {
	if database == nil || database.file == nil || database.info == nil {
		return errors.New("catalog snapshot is not active")
	}
	currentPaths, err := sqliteSidecarBasePaths(database.absolutePath)
	if err != nil {
		return fmt.Errorf("catalog snapshot path changed %s: %w", stage, err)
	}
	allPaths := append(append([]string{}, database.sidecarBasePaths...), currentPaths...)
	if err := rejectSQLiteSidecarsAtPaths(allPaths); err != nil {
		return fmt.Errorf("catalog snapshot is not standalone %s: %w", stage, err)
	}
	fileInfo, err := database.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect pinned catalog snapshot %s: %w", stage, err)
	}
	pathInfo, err := os.Stat(database.absolutePath)
	if err != nil || !fileInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(database.info, fileInfo) || !os.SameFile(database.info, pathInfo) ||
		fileInfo.Size() != database.size || pathInfo.Size() != database.size {
		return fmt.Errorf("catalog snapshot path, inode, or size changed %s", stage)
	}
	digest, err := hashPinnedFile(database.file, database.size)
	if err != nil {
		return fmt.Errorf("hash pinned catalog snapshot %s: %w", stage, err)
	}
	if digest != database.digest {
		return fmt.Errorf("catalog snapshot bytes changed %s", stage)
	}
	return nil
}

func hashPinnedFile(file *os.File, size int64) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if file == nil || size < 0 {
		return digest, errors.New("file and non-negative size are required")
	}
	hasher := sha256.New()
	read, err := io.Copy(hasher, io.NewSectionReader(file, 0, size))
	if err != nil {
		return digest, err
	}
	if read != size {
		return digest, errors.New("file size changed while hashing")
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func openReadOnlyDatabase(ctx context.Context, databasePath string) (*readOnlyDatabase, error) {
	return openReadOnlyDatabaseWithOpeners(ctx, databasePath, os.Open, sql.Open)
}

// openReadOnlyDatabaseWithOpeners pins the requested inode with an open file
// descriptor and opens that descriptor through SQLite in immutable read-only
// mode. The path and full file digest remain independently verifiable.
func openReadOnlyDatabaseWithOpeners(ctx context.Context, databasePath string,
	openFile func(string) (*os.File, error), openDatabase func(string, string) (*sql.DB, error),
) (*readOnlyDatabase, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if openFile == nil || openDatabase == nil {
		return nil, errors.New("catalog snapshot openers are required")
	}
	absolutePath, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve local database path: %w", err)
	}
	sidecarBasePaths, err := sqliteSidecarBasePaths(absolutePath)
	if err != nil {
		return nil, err
	}
	if err := rejectSQLiteSidecarsAtPaths(sidecarBasePaths); err != nil {
		return nil, err
	}
	inspectedInfo, err := os.Stat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("inspect local database: %w", err)
	}
	if !inspectedInfo.Mode().IsRegular() {
		return nil, errors.New("local database path must identify a regular file")
	}
	file, err := openFile(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("open local database snapshot: %w", err)
	}
	database := &readOnlyDatabase{file: file, absolutePath: absolutePath, sidecarBasePaths: sidecarBasePaths}
	fail := func(failure error) (*readOnlyDatabase, error) {
		if database.transaction != nil {
			_ = database.transaction.Rollback()
			database.transaction = nil
		}
		if database.connection != nil {
			_ = database.connection.Close()
			database.connection = nil
		}
		if database.database != nil {
			_ = database.database.Close()
			database.database = nil
		}
		if database.file != nil {
			_ = database.file.Close()
			database.file = nil
		}
		return nil, failure
	}

	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("inspect opened local database snapshot: %w", err))
	}
	pathInfo, err := os.Stat(absolutePath)
	if err != nil || !info.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(inspectedInfo, info) || !os.SameFile(inspectedInfo, pathInfo) ||
		info.Size() != inspectedInfo.Size() || pathInfo.Size() != inspectedInfo.Size() {
		return fail(errors.New("catalog snapshot path, inode, or size changed while being opened"))
	}
	database.info = info
	database.size = info.Size()
	database.digest, err = hashPinnedFile(file, database.size)
	if err != nil {
		return fail(fmt.Errorf("hash local database snapshot: %w", err))
	}
	if err := database.verifyCurrent("before SQLite open"); err != nil {
		return fail(err)
	}

	database.descriptorPath = fmt.Sprintf("/dev/fd/%d", file.Fd())
	databaseURL := &url.URL{Scheme: "file", Path: database.descriptorPath}
	query := databaseURL.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	query.Add("_pragma", "busy_timeout(5000)")
	databaseURL.RawQuery = query.Encode()
	database.database, err = openDatabase("sqlite", databaseURL.String())
	if err != nil {
		return fail(fmt.Errorf("open immutable local database: %w", err))
	}
	database.database.SetMaxOpenConns(1)
	database.database.SetMaxIdleConns(1)
	database.database.SetConnMaxLifetime(0)
	database.connection, err = database.database.Conn(ctx)
	if err != nil {
		return fail(fmt.Errorf("reserve immutable local database connection: %w", err))
	}
	database.transaction, err = database.connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail(fmt.Errorf("begin immutable local catalog snapshot: %w", err))
	}

	var sequence int
	var name, openedPath string
	if err := database.transaction.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&sequence, &name, &openedPath); err != nil {
		return fail(fmt.Errorf("verify opened local database: %w", err))
	}
	if sequence != 0 || name != "main" || openedPath == "" {
		return fail(errors.New("opened SQLite database does not match the pinned local file"))
	}
	if filepath.Clean(openedPath) != filepath.Clean(database.descriptorPath) {
		openedInfo, err := os.Stat(openedPath)
		if err != nil || !os.SameFile(info, openedInfo) || openedInfo.Size() != database.size {
			return fail(errors.New("opened SQLite database does not match the pinned local file"))
		}
	}
	var queryOnly, trustedSchema, attachedCount int
	if err := database.transaction.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		return fail(errors.New("read-only SQLite query_only defense is not active"))
	}
	if err := database.transaction.QueryRowContext(ctx, `PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil || trustedSchema != 0 {
		return fail(errors.New("read-only SQLite trusted_schema defense is not active"))
	}
	if err := database.transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_database_list`).Scan(&attachedCount); err != nil || attachedCount != 1 {
		return fail(errors.New("read-only SQLite attachment defense is not active"))
	}
	if err := database.verifyCurrent("after SQLite open"); err != nil {
		return fail(err)
	}
	return database, nil
}

func readBoundedFile(path string, maximum int64, label string) ([]byte, string, error) {
	return readBoundedFileWithOpener(path, maximum, label, os.Open)
}

func readBoundedFileWithOpener(path string, maximum int64, label string, openFile func(string) (*os.File, error)) ([]byte, string, error) {
	return readBoundedFileWithSnapshotHook(path, maximum, label, openFile, nil)
}

// readBoundedFileWithSnapshotHook pins the inspected inode and its full digest
// before reading. The hook exposes only the digest-pin/read boundary for race
// regression tests.
func readBoundedFileWithSnapshotHook(path string, maximum int64, label string,
	openFile func(string) (*os.File, error), afterDigestPin func() error,
) ([]byte, string, error) {
	if openFile == nil {
		return nil, "", errors.New("file opener is required")
	}
	absolute, info, err := existingRegularFile(path, label)
	if err != nil {
		return nil, "", err
	}
	if info.Size() <= 0 || info.Size() > maximum {
		return nil, "", fmt.Errorf("%s must be between 1 and %d bytes", label, maximum)
	}
	file, err := openFile(absolute)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("inspect opened %s: %w", label, err)
	}
	pathInfo, pathErr := os.Stat(absolute)
	if pathErr != nil || !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) || !os.SameFile(info, pathInfo) ||
		openedInfo.Size() != info.Size() || pathInfo.Size() != info.Size() {
		return nil, "", fmt.Errorf("%s changed between inspection and open", label)
	}
	pinnedDigest, err := hashPinnedFile(file, openedInfo.Size())
	if err != nil {
		return nil, "", fmt.Errorf("hash opened %s: %w", label, err)
	}
	if afterDigestPin != nil {
		if err := afterDigestPin(); err != nil {
			return nil, "", fmt.Errorf("after %s digest pin: %w", label, err)
		}
	}
	body, err := io.ReadAll(io.LimitReader(io.NewSectionReader(file, 0, openedInfo.Size()), maximum+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(body)) > maximum {
		return nil, "", fmt.Errorf("%s exceeds %d bytes", label, maximum)
	}
	if int64(len(body)) != openedInfo.Size() {
		return nil, "", fmt.Errorf("%s changed while being read", label)
	}
	bodyDigest := sha256.Sum256(body)
	if bodyDigest != pinnedDigest {
		return nil, "", fmt.Errorf("%s bytes changed after digest pin", label)
	}
	currentInfo, statErr := file.Stat()
	currentPathInfo, pathStatErr := os.Stat(absolute)
	if statErr != nil || pathStatErr != nil || !currentInfo.Mode().IsRegular() || !currentPathInfo.Mode().IsRegular() ||
		!os.SameFile(openedInfo, currentInfo) || !os.SameFile(openedInfo, currentPathInfo) ||
		currentInfo.Size() != openedInfo.Size() || currentPathInfo.Size() != openedInfo.Size() {
		return nil, "", fmt.Errorf("%s path, inode, or size changed while being read", label)
	}
	currentDigest, err := hashPinnedFile(file, openedInfo.Size())
	if err != nil {
		return nil, "", fmt.Errorf("rehash opened %s: %w", label, err)
	}
	if currentDigest != pinnedDigest {
		return nil, "", fmt.Errorf("%s bytes changed while being read", label)
	}
	return body, hex.EncodeToString(pinnedDigest[:]), nil
}

func writeManifest(outputPath string, manifest lyricsstaging.Manifest, stdout io.Writer) error {
	return writeManifestContext(context.Background(), outputPath, manifest, stdout)
}

func writeManifestContext(ctx context.Context, outputPath string, manifest lyricsstaging.Manifest, _ io.Writer) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if outputPath == "-" {
		return errors.New("sensitive staging manifest requires a private file path; stdout is not supported")
	}
	body, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFileNoReplaceContext(ctx, outputPath, body)
}

func writeManifestEvidencePairContext(
	ctx context.Context,
	manifestPath string,
	manifest lyricsstaging.Manifest,
	receiptPath string,
	receipt lyricsstaging.PrivateEvidenceReceipt,
	projectedReceiptSHA256 string,
) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if manifestPath == "-" || receiptPath == "-" {
		return errors.New("staging manifest and private evidence receipt require private file paths; stdout is not supported")
	}
	if receipt.ReceiptSHA256 != projectedReceiptSHA256 {
		return errors.New("staging publication pair does not bind the projected evidence receipt digest")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	manifestBody, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		return err
	}
	receiptBody, err := lyricsstaging.MarshalPrivateEvidenceReceipt(receipt)
	if err != nil {
		return err
	}
	if receipt.ReceiptSHA256 != projectedReceiptSHA256 {
		return errors.New("projected private evidence receipt digest changed before publication")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFilePairNoReplaceWithPublisherContext(
		ctx,
		manifestPath,
		manifestBody,
		receiptPath,
		receiptBody,
		os.Link,
		syncParentDirectories,
		nil,
	)
}

func writeEvidenceReceiptContext(ctx context.Context, outputPath string, receipt lyricsstaging.PrivateEvidenceReceipt) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if outputPath == "-" {
		return errors.New("private evidence receipt requires a private file path; stdout is not supported")
	}
	receiptDigest := receipt.ReceiptSHA256
	body, err := lyricsstaging.MarshalPrivateEvidenceReceipt(receipt)
	if err != nil {
		return err
	}
	if receipt.ReceiptSHA256 != receiptDigest {
		return errors.New("canonical private evidence receipt digest changed before publication")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFileNoReplaceContext(ctx, outputPath, body)
}

func writeFileNoReplace(outputPath string, body []byte) error {
	return writeFileNoReplaceContext(context.Background(), outputPath, body)
}

func writeFileNoReplaceContext(ctx context.Context, outputPath string, body []byte) error {
	return writeFileNoReplaceWithPublisherContext(ctx, outputPath, body, os.Link, nil)
}

func writeFileNoReplaceWithPublisher(outputPath string, body []byte, publish func(string, string) error) error {
	return writeFileNoReplaceWithPublisherContext(context.Background(), outputPath, body, publish, nil)
}

// writeFileNoReplaceWithPublisherContext exposes a single pre-publication hook
// after the private temporary file has been synced and closed. Production uses
// an atomic no-overwrite hard link, removes only a final path still bound to its
// owned temporary inode on rollback, and fsyncs the parent directory.
func writeFileNoReplaceWithPublisherContext(ctx context.Context, outputPath string, body []byte,
	publish func(string, string) error, beforePublish func(),
) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if publish == nil {
		return errors.New("staging manifest publisher is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared, err := preparePrivatePublication(ctx, outputPath, body, lyricsstaging.MaxManifestBytes, "staging manifest")
	if err != nil {
		return err
	}
	defer func() { _ = prepared.removeTemporary() }()
	if beforePublish != nil {
		beforePublish()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publish(prepared.temporaryPath, prepared.outputPath); err != nil {
		return rollbackOwnedPublications(
			fmt.Errorf("create new staging manifest: %w", err),
			nil,
			[]*preparedPrivatePublication{prepared},
			syncParentDirectories,
		)
	}
	published := []*preparedPrivatePublication{prepared}
	if err := ctx.Err(); err != nil {
		return rollbackOwnedPublications(err, published, []*preparedPrivatePublication{prepared}, syncParentDirectories)
	}
	if err := prepared.removeTemporary(); err != nil {
		return rollbackOwnedPublications(err, published, []*preparedPrivatePublication{prepared}, syncParentDirectories)
	}
	if err := syncParentDirectories([]string{prepared.outputPath}); err != nil {
		return rollbackOwnedPublications(err, published, []*preparedPrivatePublication{prepared}, syncParentDirectories)
	}
	return nil
}

// writeFilePairNoReplaceWithPublisherContext publishes both artifacts from
// separately synced mode-0600 temporary inodes. Any observed failure rolls back
// only final paths still bound to those owned inodes and fsyncs every affected
// parent. A crash may leave zero, one, or both names, so validation recognizes a
// one-name state as an incomplete pair and refuses to overwrite or delete it.
func writeFilePairNoReplaceWithPublisherContext(
	ctx context.Context,
	manifestPath string,
	manifestBody []byte,
	receiptPath string,
	receiptBody []byte,
	publish func(string, string) error,
	syncDirectories func([]string) error,
	afterPublish func(int),
) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if publish == nil || syncDirectories == nil {
		return errors.New("staging publication pair operations are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	absoluteManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return fmt.Errorf("resolve staging manifest output path: %w", err)
	}
	absoluteReceipt, err := filepath.Abs(receiptPath)
	if err != nil {
		return fmt.Errorf("resolve private evidence receipt output path: %w", err)
	}
	if err := requireAbsentPublicationPair(absoluteManifest, absoluteReceipt); err != nil {
		return err
	}
	preparedManifest, err := preparePrivatePublication(
		ctx,
		absoluteManifest,
		manifestBody,
		lyricsstaging.MaxManifestBytes,
		"staging manifest",
	)
	if err != nil {
		return err
	}
	prepared := []*preparedPrivatePublication{preparedManifest}
	defer func() {
		for _, file := range prepared {
			_ = file.removeTemporary()
		}
	}()
	preparedReceipt, err := preparePrivatePublication(
		ctx,
		absoluteReceipt,
		receiptBody,
		lyricsstaging.MaxPrivateEvidenceReceiptBytes,
		"private evidence receipt",
	)
	if err != nil {
		return rollbackOwnedPublications(err, nil, prepared, syncDirectories)
	}
	prepared = append(prepared, preparedReceipt)
	if err := requireAbsentPublicationPair(absoluteManifest, absoluteReceipt); err != nil {
		return rollbackOwnedPublications(err, nil, prepared, syncDirectories)
	}
	published := make([]*preparedPrivatePublication, 0, len(prepared))
	for index, file := range prepared {
		if err := ctx.Err(); err != nil {
			return rollbackOwnedPublications(err, published, prepared, syncDirectories)
		}
		if err := publish(file.temporaryPath, file.outputPath); err != nil {
			return rollbackOwnedPublications(
				fmt.Errorf("create new %s: %w", file.label, err),
				published,
				prepared,
				syncDirectories,
			)
		}
		published = append(published, file)
		if afterPublish != nil {
			afterPublish(index)
		}
		if err := ctx.Err(); err != nil {
			return rollbackOwnedPublications(err, published, prepared, syncDirectories)
		}
	}
	for _, file := range published {
		if err := verifyPublishedFileOwned(file); err != nil {
			return rollbackOwnedPublications(err, published, prepared, syncDirectories)
		}
	}
	for _, file := range prepared {
		if err := file.removeTemporary(); err != nil {
			return rollbackOwnedPublications(err, published, prepared, syncDirectories)
		}
	}
	paths := []string{absoluteManifest, absoluteReceipt}
	if err := syncDirectories(paths); err != nil {
		return rollbackOwnedPublications(err, published, prepared, syncDirectories)
	}
	for _, file := range published {
		if err := verifyPublishedFileOwned(file); err != nil {
			return rollbackOwnedPublications(err, published, prepared, syncDirectories)
		}
	}
	return nil
}

type preparedPrivatePublication struct {
	label         string
	outputPath    string
	temporaryPath string
	ownerInfo     os.FileInfo
}

func preparePrivatePublication(
	ctx context.Context,
	outputPath string,
	body []byte,
	maximum int,
	label string,
) (_ *preparedPrivatePublication, err error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > maximum {
		return nil, fmt.Errorf("%s must be between 1 and %d bytes", label, maximum)
	}
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s output path: %w", label, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolute), "."+filepath.Base(absolute)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create private %s: %w", label, err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("close private %s after failure: %w", label, closeErr)
			}
		}
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("set private %s permissions: %w", label, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for offset := 0; offset < len(body); {
		written, writeErr := temporary.Write(body[offset:])
		if writeErr != nil {
			return nil, fmt.Errorf("write %s: %w", label, writeErr)
		}
		if written <= 0 {
			return nil, fmt.Errorf("write %s: %w", label, io.ErrShortWrite)
		}
		offset += written
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		return nil, fmt.Errorf("sync %s: %w", label, err)
	}
	ownerInfo, err := temporary.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect private %s inode: %w", label, err)
	}
	if !ownerInfo.Mode().IsRegular() || ownerInfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("private %s temporary inode is not a mode-0600 regular file", label)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", label, err)
	}
	closed = true
	return &preparedPrivatePublication{
		label: label, outputPath: absolute, temporaryPath: temporaryPath, ownerInfo: ownerInfo,
	}, nil
}

func (file *preparedPrivatePublication) removeTemporary() error {
	if file == nil || file.temporaryPath == "" {
		return nil
	}
	if err := os.Remove(file.temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove private %s temporary: %w", file.label, err)
	}
	file.temporaryPath = ""
	return nil
}

func rollbackOwnedPublications(
	cause error,
	published []*preparedPrivatePublication,
	prepared []*preparedPrivatePublication,
	syncDirectories func([]string) error,
) error {
	errorsToJoin := []error{cause}
	for index := len(published) - 1; index >= 0; index-- {
		file := published[index]
		if err := removePublishedFileIfOwned(file.ownerInfo, file.outputPath, file.label); err != nil {
			errorsToJoin = append(errorsToJoin, err)
		}
	}
	paths := make([]string, 0, len(prepared))
	for _, file := range prepared {
		paths = append(paths, file.outputPath)
		if err := file.removeTemporary(); err != nil {
			errorsToJoin = append(errorsToJoin, err)
		}
	}
	if syncDirectories != nil && len(paths) > 0 {
		if err := syncDirectories(paths); err != nil {
			errorsToJoin = append(errorsToJoin, fmt.Errorf("sync publication parents after rollback: %w", err))
		}
	}
	return errors.Join(errorsToJoin...)
}

func verifyPublishedFileOwned(file *preparedPrivatePublication) error {
	if file == nil || file.ownerInfo == nil {
		return errors.New("published private file ownership is unavailable")
	}
	outputInfo, err := os.Lstat(file.outputPath)
	if err != nil {
		return fmt.Errorf("inspect published %s output: %w", file.label, err)
	}
	if !outputInfo.Mode().IsRegular() || outputInfo.Mode().Perm() != 0o600 || !os.SameFile(file.ownerInfo, outputInfo) {
		return fmt.Errorf("published %s output no longer matches its owned mode-0600 private inode", file.label)
	}
	return nil
}

func removePublishedFileIfOwned(ownerInfo os.FileInfo, outputPath, label string) error {
	outputInfo, err := os.Lstat(outputPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect rollback %s output: %w", label, err)
	}
	if ownerInfo == nil || !os.SameFile(ownerInfo, outputInfo) {
		return fmt.Errorf("rollback %s output no longer matches its owned private inode", label)
	}
	if err := os.Remove(outputPath); err != nil {
		return fmt.Errorf("remove rollback %s output: %w", label, err)
	}
	return nil
}

func syncParentDirectories(paths []string) error {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		parent := filepath.Dir(path)
		if _, exists := seen[parent]; exists {
			continue
		}
		seen[parent] = struct{}{}
		directory, err := os.Open(parent)
		if err != nil {
			return fmt.Errorf("open publication parent directory %s: %w", parent, err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return fmt.Errorf("sync publication parent directory %s: %w", parent, syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close publication parent directory %s: %w", parent, closeErr)
		}
	}
	return nil
}
