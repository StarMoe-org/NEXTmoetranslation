package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"moesekai/server/internal/db"
	"moesekai/server/internal/store"
	"moesekai/server/offline/internal/lyricsrecoveryimport"
	"moesekai/server/offline/internal/lyricsrecoverypublic"
)

const recoveryPublicCandidateMaximumCompatibleRuntimeSchema = 39

type options struct {
	databasePath            string
	batchSHA256             string
	outputDirectory         string
	v2CompatOutputDirectory string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	if ctx == nil || output == nil {
		return errors.New("recovery Public candidate requires context and output")
	}
	var opts options
	flags := flag.NewFlagSet("lyrics-recovery-public-candidate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.databasePath, "database", "", "existing standalone recovery SQLite database with contiguous schema v27 through v39")
	flags.StringVar(&opts.batchSHA256, "batch-sha256", "", "exact lowercase recovery batch SHA-256")
	flags.StringVar(&opts.outputDirectory, "output-directory", "", "new immutable local strict Public v3 candidate directory")
	flags.StringVar(&opts.v2CompatOutputDirectory, "v2-compat-output-directory", "", "optional separate immutable lossless Public v2 compatibility directory")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("recovery Public candidate requires only explicit named flags")
	}
	for _, path := range []string{opts.databasePath, opts.outputDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("recovery Public candidate paths must be canonical absolute paths")
		}
	}
	outputs := []string{opts.outputDirectory}
	if opts.v2CompatOutputDirectory != "" {
		if !filepath.IsAbs(opts.v2CompatOutputDirectory) || filepath.Clean(opts.v2CompatOutputDirectory) != opts.v2CompatOutputDirectory {
			return errors.New("recovery Public v2 compatibility output must be a canonical absolute path")
		}
		outputs = append(outputs, opts.v2CompatOutputDirectory)
		if publicCandidatePathsOverlap(opts.outputDirectory, opts.v2CompatOutputDirectory) {
			return errors.New("recovery Public v3 and v2 compatibility outputs must be separately addressed")
		}
	}
	for _, outputDirectory := range outputs {
		if publicCandidatePathsOverlap(outputDirectory, opts.databasePath) {
			return errors.New("recovery Public candidate database must remain outside every output directory")
		}
	}
	if err := validatePublicCandidateOutputTargets(outputs); err != nil {
		return err
	}

	snapshot, err := db.OpenImmutableSnapshot(ctx, opts.databasePath)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = snapshot.Close()
		}
	}()
	runtimeSchema, err := snapshot.Database.ValidateKnownMigrationPrefix(
		ctx, lyricsrecoveryimport.ImportReceiptRuntimeSchemaVersion, recoveryPublicCandidateMaximumCompatibleRuntimeSchema,
	)
	if err != nil {
		return fmt.Errorf("recovery Public candidate database must have an exact known schema v%d through v%d prefix: %w",
			lyricsrecoveryimport.ImportReceiptRuntimeSchemaVersion, recoveryPublicCandidateMaximumCompatibleRuntimeSchema, err)
	}
	if runtimeSchema >= db.LyricsPeerTranslationSchemaVersion {
		if err := db.ValidateLyricsPeerTranslationSchema(ctx, snapshot.Database, true, "recovery Public candidate"); err != nil {
			return err
		}
	}
	if runtimeSchema >= db.LyricsTranslationEditionSchemaVersion {
		if err := db.ValidateLyricsTranslationEditionSchema(ctx, snapshot.Database, true, "recovery Public candidate"); err != nil {
			return err
		}
	}
	if err := snapshot.Database.ValidateLyricsStorageOwnership(ctx); err != nil {
		return fmt.Errorf("validate recovery Public candidate lyrics storage ownership: %w", err)
	}
	storeInstance := store.New(snapshot.Database)
	databaseSHA256 := snapshot.SHA256()
	databaseBytes := snapshot.Size()
	v3Candidate, err := storeInstance.RecoveryPublicLyricsV3Context(ctx, opts.batchSHA256)
	if err != nil {
		return err
	}
	v3Bundle, err := lyricsrecoverypublic.BuildV3Bundle(v3Candidate, databaseSHA256, databaseBytes)
	if err != nil {
		return err
	}
	var v2Bundle lyricsrecoverypublic.Bundle
	hasV2Compatibility := opts.v2CompatOutputDirectory != ""
	if hasV2Compatibility {
		v2Candidate, err := storeInstance.RecoveryPublicLyricsV2CompatibilityContext(ctx, opts.batchSHA256)
		if err != nil {
			return fmt.Errorf("build fail-closed Public v2 compatibility candidate: %w", err)
		}
		v2Bundle, err = lyricsrecoverypublic.BuildBundle(v2Candidate, databaseSHA256, databaseBytes)
		if err != nil {
			return err
		}
	}
	if err := snapshot.Close(); err != nil {
		return fmt.Errorf("close immutable recovery Public candidate database: %w", err)
	}
	closed = true
	if err := lyricsrecoverypublic.PublishExactV3(opts.outputDirectory, v3Bundle); err != nil {
		return err
	}
	if hasV2Compatibility {
		if err := lyricsrecoverypublic.PublishExact(opts.v2CompatOutputDirectory, v2Bundle); err != nil {
			return err
		}
	}
	counts := publicCandidateStateCounts(v3Bundle)
	if _, err := fmt.Fprintf(output,
		"PASS recoveryPublicCandidate version=3 catalog=%d details=%d complete=%d gameOnly=%d noLyrics=%d ambiguous=%d missing=%d incomplete=%d failed=%d assets=%d batchSha256=%s rootSha256=%s databaseSha256=%s contentSha256=%s manifestSha256=%s receiptSha256=%s output=%s\n",
		v3Bundle.Manifest.CatalogCount, v3Bundle.Manifest.DetailCount, counts[store.PublicLyricsStateComplete],
		counts[store.PublicLyricsStateGameOnly], counts[store.PublicLyricsStateSatisfiedNoLyrics],
		counts[store.PublicLyricsStateAmbiguous], counts[store.PublicLyricsStateMissing],
		counts[store.PublicLyricsStateIncomplete], counts[store.PublicLyricsStateFailed],
		v3Bundle.Receipt.AssetCount, v3Bundle.Receipt.BatchSHA256, v3Bundle.Receipt.RootSHA256,
		v3Bundle.Receipt.DatabaseSHA256, v3Bundle.Receipt.ContentSHA256, v3Bundle.Receipt.ManifestSHA256,
		v3Bundle.Receipt.ReceiptSHA256, opts.outputDirectory); err != nil {
		return err
	}
	if hasV2Compatibility {
		counts = publicCandidateStateCounts(v2Bundle)
		_, err = fmt.Fprintf(output,
			"PASS recoveryPublicCandidate version=2 compatibility=lossless-one-rendition catalog=%d details=%d complete=%d gameOnly=%d noLyrics=%d ambiguous=%d missing=%d incomplete=%d failed=%d assets=%d batchSha256=%s rootSha256=%s databaseSha256=%s contentSha256=%s manifestSha256=%s receiptSha256=%s output=%s\n",
			v2Bundle.Manifest.CatalogCount, v2Bundle.Manifest.DetailCount, counts[store.PublicLyricsStateComplete],
			counts[store.PublicLyricsStateGameOnly], counts[store.PublicLyricsStateSatisfiedNoLyrics],
			counts[store.PublicLyricsStateAmbiguous], counts[store.PublicLyricsStateMissing],
			counts[store.PublicLyricsStateIncomplete], counts[store.PublicLyricsStateFailed],
			v2Bundle.Receipt.AssetCount, v2Bundle.Receipt.BatchSHA256, v2Bundle.Receipt.RootSHA256,
			v2Bundle.Receipt.DatabaseSHA256, v2Bundle.Receipt.ContentSHA256, v2Bundle.Receipt.ManifestSHA256,
			v2Bundle.Receipt.ReceiptSHA256, opts.v2CompatOutputDirectory)
		return err
	}
	return nil
}

func validatePublicCandidateOutputTargets(outputDirectories []string) error {
	for _, outputDirectory := range outputDirectories {
		parent := filepath.Dir(outputDirectory)
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("recovery Public candidate output parent must be an existing direct directory")
		}
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil || resolvedParent != parent {
			return errors.New("recovery Public candidate output parent must not traverse a filesystem alias")
		}
		if _, err := os.Lstat(outputDirectory); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("recovery Public candidate output directory already exists")
			}
			return fmt.Errorf("inspect recovery Public candidate output: %w", err)
		}
	}
	return nil
}

func publicCandidatePathsOverlap(left, right string) bool {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func publicCandidateStateCounts(bundle lyricsrecoverypublic.Bundle) map[store.PublicLyricsAvailabilityState]int {
	counts := make(map[store.PublicLyricsAvailabilityState]int, len(bundle.Manifest.States))
	for _, state := range bundle.Manifest.States {
		counts[state.State] = state.Count
	}
	return counts
}
