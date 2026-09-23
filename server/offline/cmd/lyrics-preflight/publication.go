package main

import (
	"context"
	cryptorand "crypto/rand"

	"encoding/hex"
	"encoding/json"
	"errors"

	"fmt"
	"io"

	"os"

	"path/filepath"

	"strings"

	"time"

	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

func validateStagingReportContract(generated report) error {
	return lyricsstaging.ValidatePreflight(stagingReportContract(generated))
}

func stagingReportContract(generated report) lyricsstaging.PreflightReport {
	return lyricsstaging.PreflightReport{
		SchemaVersion: generated.SchemaVersion, GeneratedAt: generated.GeneratedAt,
		CatalogSchemaVersion: generated.CatalogSchemaVersion, CatalogCount: generated.CatalogCount,
		Summary: lyricsstaging.PreflightSummary{
			CatalogReview: generated.Summary.CatalogReview, GameSizeEvidence: generated.Summary.GameSizeEvidence,
			UniqueComplete: generated.Summary.UniqueComplete, Ambiguous: generated.Summary.Ambiguous,
			Missing: generated.Summary.Missing, Incomplete: generated.Summary.Incomplete, Error: generated.Summary.Error,
		},
		EvidenceReceipt:  generated.EvidenceReceipt,
		CatalogReview:    stagingReportItems(generated.CatalogReview),
		GameSizeEvidence: stagingReportItems(generated.GameSizeEvidence),
		UniqueComplete:   stagingReportItems(generated.UniqueComplete),
		Ambiguous:        stagingReportItems(generated.Ambiguous),
		Missing:          stagingReportItems(generated.Missing),
		Incomplete:       stagingReportItems(generated.Incomplete),
		Error:            stagingReportItems(generated.Error),
	}
}

func stagingReportItems(items []reportItem) []lyricsstaging.PreflightItem {
	if items == nil {
		return nil
	}
	result := make([]lyricsstaging.PreflightItem, len(items))
	for index, item := range items {
		result[index] = lyricsstaging.PreflightItem{
			MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, CatalogFingerprint: item.CatalogFingerprint,
			TargetMusicID: item.TargetMusicID, AssociationMusicIDs: append([]int(nil), item.AssociationMusicIDs...),
			ReasonCode: item.ReasonCode, PostFetchState: item.PostFetchState, CompositionReason: item.CompositionReason,
			Candidates: stagingCandidates(item.Candidates), FixedArtifactCandidates: stagingCandidates(item.FixedArtifactCandidates),
			LineCount: item.LineCount, SearchAttempts: item.SearchAttempts, FetchAttempts: item.FetchAttempts,
			ErrorCode: item.ErrorCode, SearchDiagnostics: stagingSearchDiagnostics(item.SearchDiagnostics),
		}
		if item.Candidate != nil {
			candidate := stagingCandidate(*item.Candidate)
			result[index].Candidate = &candidate
		}
	}
	return result
}

func stagingCandidates(candidates []candidateSummary) []lyricsstaging.CandidateIdentity {
	if candidates == nil {
		return nil
	}
	result := make([]lyricsstaging.CandidateIdentity, len(candidates))
	for index, candidate := range candidates {
		result[index] = stagingCandidate(candidate)
	}
	return result
}

func stagingSearchDiagnostics(diagnostics *searchDiagnostics) *lyricsstaging.SearchDiagnostics {
	if diagnostics == nil {
		return nil
	}
	return &lyricsstaging.SearchDiagnostics{
		SearchHits: diagnostics.SearchHits, Restricted: diagnostics.Restricted,
		RestrictedTitleMatch: diagnostics.RestrictedTitleMatch, TitleMismatch: diagnostics.TitleMismatch,
		CreditMismatch: diagnostics.CreditMismatch, LyricistCreditMissing: diagnostics.LyricistCreditMissing,
		LyricistCreditMismatch: diagnostics.LyricistCreditMismatch, ComposerCreditMissing: diagnostics.ComposerCreditMissing,
		ComposerCreditMismatch: diagnostics.ComposerCreditMismatch, ArrangerCreditMissing: diagnostics.ArrangerCreditMissing,
		ArrangerCreditMismatch: diagnostics.ArrangerCreditMismatch, SignalMismatch: diagnostics.SignalMismatch,
		Verified: diagnostics.Verified,
	}
}

// writeReport writes the complete provenance-bearing report only to a new
// private file. It never emits the report to stdout.
func writeReport(outputPath, databasePath string, generated report, stdout io.Writer) error {
	return writeReportContext(context.Background(), outputPath, databasePath, generated, stdout)
}

func writeReportContext(ctx context.Context, outputPath, databasePath string, generated report, _ io.Writer) error {
	return writeReportWithPublisherContext(ctx, outputPath, databasePath, generated, os.Link, nil)
}

// writeReportWithPublisherContext validates and serializes the entire final
// report before creating a private temporary. Publication is one atomic,
// no-overwrite hard link; any cancellation or sync failure removes only the
// inode this invocation owns.
func writeReportWithPublisherContext(ctx context.Context, outputPath, databasePath string, generated report,
	publish func(string, string) error, beforePublish func(),
) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if publish == nil {
		return errors.New("preflight report publisher is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if outputPath == "-" {
		return errors.New("complete preflight report requires a private file path; stdout is not supported")
	}
	if err := validateDistinctOutputPath(outputPath, databasePath); err != nil {
		return err
	}
	parsedGeneratedAt, err := time.Parse(time.RFC3339Nano, generated.GeneratedAt)
	if err != nil || parsedGeneratedAt.Format(time.RFC3339Nano) != generated.GeneratedAt || !strings.HasSuffix(generated.GeneratedAt, "Z") {
		return errors.New("invalid preflight report timestamp")
	}
	if generated.CatalogReview == nil || generated.GameSizeEvidence == nil || generated.UniqueComplete == nil ||
		generated.Ambiguous == nil || generated.Missing == nil || generated.Incomplete == nil || generated.Error == nil {
		return errors.New("invalid preflight report arrays")
	}
	classifiedCount := len(generated.CatalogReview) + len(generated.GameSizeEvidence) + len(generated.UniqueComplete) +
		len(generated.Ambiguous) + len(generated.Missing) + len(generated.Incomplete) + len(generated.Error)
	if generated.SchemaVersion != reportSchemaVersion || generated.CatalogSchemaVersion != catalogSchemaVersion ||
		generated.GeneratedAt == "" || generated.CatalogCount < 0 || classifiedCount != generated.CatalogCount ||
		generated.Summary.CatalogReview != len(generated.CatalogReview) ||
		generated.Summary.GameSizeEvidence != len(generated.GameSizeEvidence) ||
		generated.Summary.UniqueComplete != len(generated.UniqueComplete) || generated.Summary.Ambiguous != len(generated.Ambiguous) ||
		generated.Summary.Missing != len(generated.Missing) || generated.Summary.Incomplete != len(generated.Incomplete) ||
		generated.Summary.Error != len(generated.Error) {
		return errors.New("invalid preflight report envelope")
	}
	if err := validateClassifiedReportItems(generated); err != nil {
		return fmt.Errorf("invalid preflight report item: %w", err)
	}
	if err := validateStagingReportContract(generated); err != nil {
		return fmt.Errorf("invalid preflight staging contract: %w", err)
	}
	body, err := json.MarshalIndent(generated, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON report: %w", err)
	}
	body = append(body, '\n')
	if !completeReportSizeWithinReviewedContract(int64(len(body))) {
		return fmt.Errorf("complete preflight report exceeds %d bytes", maxCompleteReportBytes)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	outputAbsolute, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve report path: %w", err)
	}
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return fmt.Errorf("create private report temporary name: %w", err)
	}
	temporaryPath := filepath.Join(filepath.Dir(outputAbsolute), "."+filepath.Base(outputAbsolute)+".tmp-"+hex.EncodeToString(random[:]))
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private JSON report temporary: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set private JSON report permissions: %w", err)
	}
	if _, err := temporary.Write(body); err != nil {
		return fmt.Errorf("write private JSON report temporary: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync private JSON report temporary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close private JSON report temporary: %w", err)
	}
	closed = true
	temporaryInfo, err := os.Stat(temporaryPath)
	if err != nil {
		return fmt.Errorf("inspect private JSON report temporary: %w", err)
	}
	if beforePublish != nil {
		beforePublish()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publish(temporaryPath, outputAbsolute); err != nil {
		return fmt.Errorf("create new JSON report atomically: %w", err)
	}
	cleanupPublished := func(cause error) error {
		outputInfo, outputErr := os.Stat(outputAbsolute)
		if outputErr == nil && os.SameFile(temporaryInfo, outputInfo) {
			if removeErr := os.Remove(outputAbsolute); removeErr != nil {
				return errors.Join(cause, fmt.Errorf("remove unpublished JSON report: %w", removeErr))
			}
		}
		return cause
	}
	if err := ctx.Err(); err != nil {
		return cleanupPublished(err)
	}
	directory, err := os.Open(filepath.Dir(outputAbsolute))
	if err != nil {
		return cleanupPublished(fmt.Errorf("open JSON report directory for sync: %w", err))
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return cleanupPublished(errors.Join(
			func() error {
				if syncErr != nil {
					return fmt.Errorf("sync JSON report directory: %w", syncErr)
				}
				return nil
			}(),
			func() error {
				if closeErr != nil {
					return fmt.Errorf("close JSON report directory: %w", closeErr)
				}
				return nil
			}(),
		))
	}
	if err := os.Remove(temporaryPath); err != nil {
		return cleanupPublished(fmt.Errorf("remove published JSON report temporary: %w", err))
	}
	directory, err = os.Open(filepath.Dir(outputAbsolute))
	if err != nil {
		return cleanupPublished(fmt.Errorf("reopen JSON report directory for final sync: %w", err))
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil || closeErr != nil {
		return cleanupPublished(errors.Join(syncErr, closeErr))
	}
	if err := ctx.Err(); err != nil {
		return cleanupPublished(err)
	}
	return nil
}
