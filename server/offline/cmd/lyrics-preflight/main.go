package main

import (
	"context"

	"errors"
	"flag"
	"fmt"
	"io"

	"os"
	"os/signal"

	"strings"

	"syscall"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

const (
	catalogSchemaVersion   = 18
	reportSchemaVersion    = 1
	maxConcurrency         = 16
	maxAttempts            = 5
	maxCatalogRecords      = 100_000
	maxCatalogJSONBytes    = 1 << 20
	maxCompleteReportBytes = lyricsstaging.MaxPreflightReportBytes
	maxReportCandidates    = 16
	maxCandidateTitle      = 2048
	maxCandidateURL        = 4096
	maxCandidateCategory   = 1024
	defaultRequestTimeout  = 8 * time.Minute
	maxRequestTimeout      = 10 * time.Minute
)

var sqliteSidecarSuffixes = [...]string{"-wal", "-shm", "-journal"}

var (
	errPreflightEvidenceConflict = errors.New("preflight evidence ID has conflicting exact resolutions")
	errPreflightEvidenceCapacity = errors.New("preflight evidence receipt capacity exceeded")
)

var safeResumeErrorCodes = map[string]struct{}{
	"malformed_response": {}, // search phase only; mergeResumeReport enforces the item-level guard
	"rate_limited":       {},
	"source_unavailable": {},
	"timeout":            {},
}

// Missing records are searched again only for deterministic zero-candidate
// diagnostics that can legitimately change after general search/identity rule
// fixes. Restricted evidence is intentionally excluded.
var safeResumeMissingReasons = map[string]struct{}{
	string(lyricssource.ZeroCandidateNoSearchHits):      {},
	string(lyricssource.ZeroCandidateTitleMismatch):     {},
	string(lyricssource.ZeroCandidateCreditMismatch):    {},
	string(lyricssource.ZeroCandidateMissingSongSignal): {},
}

// Incomplete records are retried only when re-inspecting the recorded fixed
// page/revision/SHA can legitimately change deterministic verification or
// extraction. ambiguous_source here is fixed-revision ambiguity after Search
// selected one candidate; multi-candidate identity ambiguity remains in the
// Ambiguous bucket and is never admitted by this flag. Source drift,
// restrictions, and unpublished lyrics remain excluded.
var safeResumeIncompleteCodes = map[string]struct{}{
	"ambiguous_source":   {},
	"missing_lyrics":     {},
	"unsupported_format": {},
}

type options struct {
	DatabasePath          string
	OutputPath            string
	CheckpointPath        string
	ResumeCheckpointPath  string
	ResumeReportPath      string
	ResumeErrorCodes      string
	ResumeMissingReasons  string
	ResumeIncompleteCodes string
	ResumeUniqueComplete  bool
	Concurrency           int
	MaxAttempts           int
	RequestTimeout        time.Duration
	RetryDelay            time.Duration
}

type sourceClient interface {
	Search(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error)
	FetchFixedCandidateRevision(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error)
}

type diagnosticSourceClient interface {
	SearchWithDiagnostics(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error)
}

type sourceSearchResult struct {
	Candidates  []lyricssource.Candidate
	Diagnostics *searchDiagnostics
}

type catalogLoader func(context.Context, string) ([]catalogItem, error)

type sourceClientFactory func() (sourceClient, error)

type commandDependencies struct {
	NewSourceClient sourceClientFactory
	LoadCatalog     catalogLoader
	Now             func() time.Time
}

func defaultCommandDependencies() commandDependencies {
	return commandDependencies{
		NewSourceClient: func() (sourceClient, error) {
			return lyricssource.DefaultRegistry()
		},
		LoadCatalog: loadCatalog,
		Now:         time.Now,
	}
}

type catalogItem struct {
	MusicID            int
	JapaneseTitle      string
	ProducerMetadata   string
	Lyricist           string
	Composer           string
	Arranger           string
	Evidence           model.CatalogLyricsEvidence
	CatalogFingerprint string
}

// candidateSummary retains the complete compact provider-aware candidate
// identity while excluding private index-response bytes, wikitext, and
// extracted lines.
type candidateSummary struct {
	Provider             model.LyricsSourceProvider           `json:"provider"`
	Origin               string                               `json:"origin"`
	PageID               int                                  `json:"pageId"`
	RevisionID           int                                  `json:"revisionId"`
	RevisionTimestamp    string                               `json:"revisionTimestamp,omitempty"`
	SHA1                 string                               `json:"sha1"`
	Title                string                               `json:"title"`
	CanonicalURL         string                               `json:"canonicalUrl"`
	Categories           []string                             `json:"categories"`
	Section              string                               `json:"section"`
	RenditionKey         string                               `json:"renditionKey"`
	ArtifactRenditionKey string                               `json:"artifactRenditionKey,omitempty"`
	VersionReason        model.LyricsSourceVersionReasonCode  `json:"versionReason"`
	IndexEvidenceRefs    []model.LyricsSourceIndexEvidenceRef `json:"indexEvidenceRefs"`
}

type candidateIdentityKey struct {
	Provider     model.LyricsSourceProvider
	PageID       int
	RevisionID   int
	Section      string
	RenditionKey string
}

func (candidate candidateSummary) identityKey() candidateIdentityKey {
	return candidateIdentityKey{
		Provider: candidate.Provider, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
		Section: candidate.Section, RenditionKey: candidate.RenditionKey,
	}
}

func (candidate candidateSummary) sourceCandidate() lyricssource.Candidate {
	return lyricssource.Candidate{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, RevisionTimestamp: candidate.RevisionTimestamp,
		SHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
	}
}

// reportItem is the complete per-music JSON surface. Completeness is conveyed
// as a line count only, never source or lyric text.
type reportItem struct {
	MusicID                 int                                 `json:"musicId"`
	JapaneseTitle           string                              `json:"japaneseTitle"`
	CatalogFingerprint      string                              `json:"catalogFingerprint"`
	TargetMusicID           int                                 `json:"targetMusicId,omitempty"`
	AssociationMusicIDs     []int                               `json:"associationMusicIds,omitempty"`
	ReasonCode              string                              `json:"reasonCode,omitempty"`
	PostFetchState          lyricsstaging.PostFetchState        `json:"postFetchState,omitempty"`
	CompositionReason       model.LyricsSourceVersionReasonCode `json:"compositionReason,omitempty"`
	Candidate               *candidateSummary                   `json:"candidate,omitempty"`
	Candidates              []candidateSummary                  `json:"candidates,omitempty"`
	FixedArtifactCandidates []candidateSummary                  `json:"fixedArtifactCandidates,omitempty"`
	LineCount               int                                 `json:"lineCount,omitempty"`
	SearchAttempts          int                                 `json:"searchAttempts,omitempty"`
	FetchAttempts           int                                 `json:"fetchAttempts,omitempty"`
	ErrorCode               string                              `json:"errorCode,omitempty"`
	SearchDiagnostics       *searchDiagnostics                  `json:"searchDiagnostics,omitempty"`
}

type searchDiagnostics struct {
	SearchHits             int `json:"searchHits"`
	Restricted             int `json:"restricted"`
	RestrictedTitleMatch   int `json:"restrictedTitleMatch"`
	TitleMismatch          int `json:"titleMismatch"`
	CreditMismatch         int `json:"creditMismatch"`
	LyricistCreditMissing  int `json:"lyricistCreditMissing"`
	LyricistCreditMismatch int `json:"lyricistCreditMismatch"`
	ComposerCreditMissing  int `json:"composerCreditMissing"`
	ComposerCreditMismatch int `json:"composerCreditMismatch"`
	ArrangerCreditMissing  int `json:"arrangerCreditMissing"`
	ArrangerCreditMismatch int `json:"arrangerCreditMismatch"`
	SignalMismatch         int `json:"signalMismatch"`
	Verified               int `json:"verified"`
}

type reportSummary struct {
	CatalogReview    int `json:"catalog_review"`
	GameSizeEvidence int `json:"game_size_evidence"`
	UniqueComplete   int `json:"unique_complete"`
	Ambiguous        int `json:"ambiguous"`
	Missing          int `json:"missing"`
	Incomplete       int `json:"incomplete"`
	Error            int `json:"error"`
}

type report struct {
	SchemaVersion        int                                   `json:"schemaVersion"`
	GeneratedAt          string                                `json:"generatedAt"`
	CatalogSchemaVersion int                                   `json:"catalogSchemaVersion"`
	CatalogCount         int                                   `json:"catalogCount"`
	Summary              reportSummary                         `json:"summary"`
	EvidenceReceipt      *lyricsstaging.PrivateEvidenceReceipt `json:"evidenceReceipt,omitempty"`
	CatalogReview        []reportItem                          `json:"catalog_review"`
	GameSizeEvidence     []reportItem                          `json:"game_size_evidence"`
	UniqueComplete       []reportItem                          `json:"unique_complete"`
	Ambiguous            []reportItem                          `json:"ambiguous"`
	Missing              []reportItem                          `json:"missing"`
	Incomplete           []reportItem                          `json:"incomplete"`
	Error                []reportItem                          `json:"error"`
}

type classifiedResult struct {
	class    string
	item     reportItem
	evidence []lyricssource.IndexEvidence
}

type preflightEvidenceCapacityLimits struct {
	maxItems           int
	maxPerItemRawBytes int64
	maxRawBytes        int64
	maxEncodedBytes    int64
}

type preflightEvidenceAggregator struct {
	byID                  map[string]lyricssource.IndexEvidence
	rawBytes              int64
	encodedEvidenceBytes  int64
	capacityInitialized   bool
	limits                preflightEvidenceCapacityLimits
	cloneEvidenceEnvelope func(lyricssource.IndexEvidence) lyricssource.IndexEvidence
}

type fixedResumeWork struct {
	item       reportItem
	candidates []lyricssource.Candidate
}

type attemptResult[T any] struct {
	value    T
	err      error
	attempts int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lyrics preflight: %v\n", err)
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
	if dependencies.NewSourceClient == nil || dependencies.LoadCatalog == nil || dependencies.Now == nil {
		return errors.New("lyrics preflight command dependencies are required")
	}
	flags := flag.NewFlagSet("lyrics-preflight", flag.ContinueOnError)
	if stdout == nil {
		flags.SetOutput(io.Discard)
	} else {
		flags.SetOutput(stdout)
	}
	databasePath := flags.String("db", "", "path to an existing schema-v18 SQLite database")
	outputPath := flags.String("output", "", "new private JSON report path for atomic final publication")
	checkpointPath := flags.String("checkpoint", "", "create a new exclusive mode-0600 private SQLite checkpoint; this is not a final report or stage/import input")
	resumeCheckpointPath := flags.String("resume-checkpoint", "", "validate and continue the same private SQLite checkpoint, processing only missing work; this is not a report")
	resumeReportPath := flags.String("resume-report", "", "prior complete JSON report whose selected safe items should be retried")
	resumeErrorCodes := flags.String("resume-error-codes", "rate_limited", "comma-separated safe error codes to retry from -resume-report (malformed_response is search-phase only), or none")
	resumeMissingReasons := flags.String("resume-missing-reasons", "", "comma-separated missing reasons to search again from -resume-report: no_search_hits,title_mismatch,credit_mismatch,missing_song_signal; use none to disable (default off)")
	resumeIncompleteCodes := flags.String("resume-incomplete-codes", "", "comma-separated fixed-revision incomplete codes to retry without Search after selector/parser changes: ambiguous_source,missing_lyrics,unsupported_format")
	resumeUniqueComplete := flags.Bool("resume-unique-complete", false, "revalidate every recorded unique_complete fixed revision without Search after selector/parser changes")
	concurrency := flags.Int("concurrency", 4, "bounded source request concurrency")
	attempts := flags.Int("max-attempts", 3, "maximum attempts per source operation")
	requestTimeout := flags.Duration("request-timeout", defaultRequestTimeout, "timeout for each source operation, including client-side limiter waits")
	retryDelay := flags.Duration("retry-delay", 250*time.Millisecond, "initial retry delay")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	opts := options{
		DatabasePath: *databasePath, OutputPath: *outputPath,
		CheckpointPath: *checkpointPath, ResumeCheckpointPath: *resumeCheckpointPath,
		ResumeReportPath: *resumeReportPath, ResumeErrorCodes: *resumeErrorCodes,
		ResumeMissingReasons: *resumeMissingReasons, ResumeIncompleteCodes: *resumeIncompleteCodes,
		ResumeUniqueComplete: *resumeUniqueComplete, Concurrency: *concurrency, MaxAttempts: *attempts,
		RequestTimeout: *requestTimeout, RetryDelay: *retryDelay,
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
	generated, err := executeWithCatalogLoaderAndProgress(ctx, opts, source, dependencies.Now, dependencies.LoadCatalog, stdout)
	if err != nil {
		return err
	}
	return writeReportContext(ctx, opts.OutputPath, opts.DatabasePath, generated, stdout)
}

func validateOptions(opts options) error {
	if err := validateExecutionOptions(opts); err != nil {
		return err
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return errors.New("-output is required")
	}
	if opts.OutputPath != strings.TrimSpace(opts.OutputPath) {
		return errors.New("-output must not have surrounding whitespace")
	}
	if opts.OutputPath == "-" {
		return errors.New("complete preflight report requires a private file path; stdout is not supported")
	}
	if err := validateDistinctOutputPath(opts.OutputPath, opts.DatabasePath); err != nil {
		return err
	}
	if err := validateCheckpointOptions(opts); err != nil {
		return err
	}
	if opts.ResumeReportPath != strings.TrimSpace(opts.ResumeReportPath) {
		return errors.New("-resume-report must not have surrounding whitespace")
	}
	if opts.ResumeReportPath != "" {
		if opts.ResumeReportPath == "-" {
			return errors.New("-resume-report must identify an existing JSON file")
		}
		if _, err := selectedResumeErrorCodes(opts); err != nil {
			return err
		}
		if _, err := selectedResumeMissingReasons(opts); err != nil {
			return err
		}
		if _, err := selectedResumeIncompleteCodes(opts); err != nil {
			return err
		}
		if err := validateResumeReportPath(opts.ResumeReportPath, opts.DatabasePath, opts.OutputPath); err != nil {
			return err
		}
	} else {
		if opts.ResumeErrorCodes != "" && opts.ResumeErrorCodes != "rate_limited" {
			return errors.New("-resume-error-codes requires -resume-report")
		}
		if opts.ResumeMissingReasons != "" {
			return errors.New("-resume-missing-reasons requires -resume-report")
		}
		if opts.ResumeIncompleteCodes != "" {
			return errors.New("-resume-incomplete-codes requires -resume-report")
		}
		if opts.ResumeUniqueComplete {
			return errors.New("-resume-unique-complete requires -resume-report")
		}
	}
	return nil
}

func validateExecutionOptions(opts options) error {
	if strings.TrimSpace(opts.DatabasePath) == "" {
		return errors.New("-db is required")
	}
	if opts.DatabasePath != strings.TrimSpace(opts.DatabasePath) {
		return errors.New("-db must not have surrounding whitespace")
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
	return nil
}
