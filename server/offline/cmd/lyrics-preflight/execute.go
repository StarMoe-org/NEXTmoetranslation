package main

import (
	"bytes"
	"context"

	"crypto/sha1"

	"encoding/hex"

	"errors"

	"fmt"
	"io"

	"reflect"

	"strings"
	"sync"

	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricscompose"
	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

func execute(ctx context.Context, opts options, source sourceClient, now func() time.Time) (report, error) {
	return executeWithCatalogLoaderAndProgress(ctx, opts, source, now, loadCatalog, io.Discard)
}

// executeWithCatalogLoader is the test seam used to prove that source requests
// are admitted only after catalog loading has returned.
func executeWithCatalogLoader(ctx context.Context, opts options, source sourceClient, now func() time.Time, loader catalogLoader) (report, error) {
	return executeWithCatalogLoaderAndProgress(ctx, opts, source, now, loader, io.Discard)
}

func executeWithCatalogLoaderAndProgress(ctx context.Context, opts options, source sourceClient, now func() time.Time,
	loader catalogLoader, progress io.Writer,
) (generatedReport report, returnErr error) {
	if ctx == nil {
		return report{}, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return report{}, err
	}
	if err := validateExecutionOptions(opts); err != nil {
		return report{}, err
	}
	if opts.CheckpointPath != "" && opts.ResumeCheckpointPath != "" {
		return report{}, errors.New("-checkpoint and -resume-checkpoint are mutually exclusive")
	}
	if (opts.CheckpointPath != "" || opts.ResumeCheckpointPath != "") && opts.ResumeReportPath != "" {
		return report{}, errors.New("checkpoint execution must not be combined with -resume-report")
	}
	if opts.CheckpointPath != strings.TrimSpace(opts.CheckpointPath) || opts.ResumeCheckpointPath != strings.TrimSpace(opts.ResumeCheckpointPath) {
		return report{}, errors.New("checkpoint paths must not have surrounding whitespace")
	}
	if opts.CheckpointPath == "-" || opts.ResumeCheckpointPath == "-" {
		return report{}, errors.New("checkpoint requires a private SQLite file path")
	}
	if source == nil {
		return report{}, errors.New("lyrics source client is required")
	}
	if now == nil || loader == nil {
		return report{}, errors.New("clock and catalog loader are required")
	}
	// Validate the selected retry set before opening the catalog or admitting
	// any source request. Resume mode must fail closed on unsupported classes.
	if opts.ResumeUniqueComplete && opts.ResumeReportPath == "" {
		return report{}, errors.New("-resume-unique-complete requires -resume-report")
	}
	retryErrorCodes, err := selectedResumeErrorCodes(opts)
	if err != nil {
		return report{}, err
	}
	retryMissingReasons, err := selectedResumeMissingReasons(opts)
	if err != nil {
		return report{}, err
	}
	retryIncompleteCodes, err := selectedResumeIncompleteCodes(opts)
	if err != nil {
		return report{}, err
	}

	var prior *report
	if opts.ResumeReportPath != "" {
		loaded, err := loadResumeReport(opts.ResumeReportPath)
		if err != nil {
			return report{}, err
		}
		prior = &loaded
	}

	// The production loader closes the pinned mode=ro, immutable, query_only
	// SQLite snapshot before it returns and before any external request is admitted.
	catalog, err := loader(ctx, opts.DatabasePath)
	if err != nil {
		return report{}, err
	}
	generated := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: now().UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: catalogSchemaVersion, CatalogCount: len(catalog),
		CatalogReview: []reportItem{}, GameSizeEvidence: []reportItem{}, UniqueComplete: []reportItem{},
		Ambiguous: []reportItem{}, Missing: []reportItem{}, Incomplete: []reportItem{}, Error: []reportItem{},
	}

	records := make([]model.CatalogLyricsGroupingRecord, 0, len(catalog))
	itemsByID := make(map[int]catalogItem, len(catalog))
	for _, item := range catalog {
		if item.MusicID <= 0 || strings.TrimSpace(item.JapaneseTitle) == "" || item.CatalogFingerprint == "" {
			return report{}, errors.New("catalog loader returned an invalid music record")
		}
		if _, exists := itemsByID[item.MusicID]; exists {
			return report{}, errors.New("catalog loader returned duplicate music IDs")
		}
		records = append(records, model.CatalogLyricsGroupingRecord{
			MusicID: item.MusicID, Fingerprint: item.CatalogFingerprint, Evidence: item.Evidence,
		})
		itemsByID[item.MusicID] = item
	}
	targets := model.ClassifyCatalogLyricsTargets(records)
	if len(targets) != len(catalog) {
		return report{}, errors.New("catalog classifier did not return every music record")
	}
	var checkpoint *preflightCheckpoint
	defer func() {
		if checkpoint == nil {
			return
		}
		validationContext, cancelValidation := context.WithTimeout(context.Background(), checkpointValidationTimeout)
		validationErr := checkpoint.validateState(validationContext, returnErr == nil)
		cancelValidation()
		closeErr := checkpoint.Close()
		if validationErr != nil {
			validationErr = fmt.Errorf("validate resumable checkpoint before close: %w", validationErr)
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("close resumable checkpoint: %w", closeErr)
		}
		if validationErr != nil || closeErr != nil {
			generatedReport = report{}
			returnErr = errors.Join(returnErr, validationErr, closeErr)
		}
	}()
	resumeFixedByMusicID := make(map[int]fixedResumeWork)
	availableEvidence := newPreflightEvidenceAggregator()
	if prior != nil {
		if err := mergeResumeReport(&generated, *prior, catalog, targets, retryErrorCodes, retryMissingReasons, retryIncompleteCodes, opts.ResumeUniqueComplete); err != nil {
			return report{}, err
		}
		priorIdentities := reportCandidateIdentities(*prior)
		var priorEvidenceResolver *lyricsstaging.PrivateEvidenceResolver
		if len(priorIdentities) > 0 {
			if prior.EvidenceReceipt == nil {
				return report{}, errors.New("resume report candidates require a private evidence receipt")
			}
			priorEvidenceResolver, err = lyricsstaging.NewPrivateEvidenceResolver(*prior.EvidenceReceipt)
			if err != nil {
				return report{}, fmt.Errorf("validate resume report evidence receipt: %w", err)
			}
			if err := priorEvidenceResolver.ValidateCandidates(priorIdentities); err != nil {
				return report{}, fmt.Errorf("bind resume report exact evidence: %w", err)
			}
		} else if prior.EvidenceReceipt != nil {
			return report{}, errors.New("resume report has private evidence without candidates")
		}
		if prior.EvidenceReceipt != nil {
			if err := availableEvidence.add(prior.EvidenceReceipt.IndexEvidence); err != nil {
				return report{}, err
			}
		}
		addFixedResume := func(item reportItem) error {
			candidates, err := hydrateReportItemCandidates(priorEvidenceResolver, item)
			if err != nil {
				return fmt.Errorf("resume report music %d fixed candidates: %w", item.MusicID, err)
			}
			resumeFixedByMusicID[item.MusicID] = fixedResumeWork{item: item, candidates: candidates}
			return nil
		}
		for _, item := range prior.Error {
			if _, retry := retryErrorCodes[item.ErrorCode]; retry && item.Candidate != nil && safeResumeErrorItem(item) {
				if err := addFixedResume(item); err != nil {
					return report{}, err
				}
			}
		}
		for _, item := range prior.Incomplete {
			if _, retry := retryIncompleteCodes[item.ErrorCode]; retry {
				if err := addFixedResume(item); err != nil {
					return report{}, err
				}
			}
		}
		if opts.ResumeUniqueComplete {
			for _, item := range prior.UniqueComplete {
				if err := addFixedResume(item); err != nil {
					return report{}, err
				}
			}
		}
	}
	checkpointCompleted := make(map[int]struct{})
	switch {
	case opts.CheckpointPath != "":
		checkpoint, err = createPreflightCheckpoint(ctx, opts.CheckpointPath, opts, catalog, targets, generated.GeneratedAt)
		if err != nil {
			return report{}, err
		}
		if err := checkpoint.progress(progress, "created"); err != nil {
			return report{}, err
		}
	case opts.ResumeCheckpointPath != "":
		checkpoint, err = openPreflightCheckpoint(ctx, opts.ResumeCheckpointPath, opts, catalog, targets)
		if err != nil {
			return report{}, err
		}
		generated, availableEvidence, checkpointCompleted, err = checkpoint.reconstruct(ctx)
		if err != nil {
			return report{}, err
		}
		if err := checkpoint.progress(progress, "resumed"); err != nil {
			return report{}, err
		}
	}
	persistResult := func(result classifiedResult) error {
		if checkpoint != nil {
			if err := checkpoint.storeResult(result); err != nil {
				return err
			}
			if err := checkpoint.progress(progress, "stored"); err != nil {
				return err
			}
		}
		return availableEvidence.add(result.evidence)
	}
	work := make([]model.CatalogLyricsTarget, 0, len(targets))
	for _, target := range targets {
		item, exists := itemsByID[target.MusicID]
		if !exists {
			return report{}, errors.New("catalog classifier returned an unknown music ID")
		}
		if _, completed := checkpointCompleted[target.MusicID]; completed {
			continue
		}
		switch target.Disposition {
		case model.LyricsCatalogTargetReview:
			if prior == nil {
				result := classifiedResult{class: "catalog_review", item: baseReportItem(item, target)}
				if err := persistResult(result); err != nil {
					return report{}, err
				}
				generated.CatalogReview = append(generated.CatalogReview, canonicalCheckpointReportItem(result.item))
			}
		case model.LyricsCatalogTargetFullTarget:
			if target.TargetMusicID != target.MusicID {
				return report{}, errors.New("catalog classifier returned an invalid full target")
			}
			if prior == nil || retryResumeMusicID(*prior, target.MusicID, retryErrorCodes, retryMissingReasons, retryIncompleteCodes, opts.ResumeUniqueComplete) {
				work = append(work, target)
			}
		case model.LyricsCatalogTargetGameSizeEvidence:
			if target.TargetMusicID <= 0 || target.TargetMusicID == target.MusicID {
				return report{}, errors.New("catalog classifier returned invalid game-size evidence")
			}
			// A game-size evidence row is classified and reported but never
			// independently searched.
			if prior == nil {
				result := classifiedResult{class: "game_size_evidence", item: baseReportItem(item, target)}
				if err := persistResult(result); err != nil {
					return report{}, err
				}
				generated.GameSizeEvidence = append(generated.GameSizeEvidence, canonicalCheckpointReportItem(result.item))
			}
		default:
			return report{}, fmt.Errorf("unsupported catalog target disposition %q", target.Disposition)
		}
	}

	workContext, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	jobs := make(chan model.CatalogLyricsTarget)
	results := make(chan classifiedResult, opts.Concurrency)
	workers := opts.Concurrency
	if workers > len(work) {
		workers = len(work)
	}
	var workerGroup sync.WaitGroup
	workerGroup.Add(workers)
	for range workers {
		go func() {
			defer workerGroup.Done()
			for target := range jobs {
				item := itemsByID[target.MusicID]
				var result classifiedResult
				if priorWork, fixedResume := resumeFixedByMusicID[target.MusicID]; fixedResume {
					result = inspectFixedResumeTarget(workContext, opts, source, item, target, priorWork)
				} else {
					result = inspectTarget(workContext, opts, source, item, target)
				}
				// The collector always drains results. Sending an already completed
				// result even after cancellation lets its transaction reach the
				// checkpoint; canceled in-flight operations are filtered below.
				results <- result
			}
		}()
	}
	go func() {
		defer close(results)
		for _, target := range work {
			select {
			case <-workContext.Done():
				close(jobs)
				workerGroup.Wait()
				return
			case jobs <- target:
			}
		}
		close(jobs)
		workerGroup.Wait()
	}()
	var resultErr error
	for result := range results {
		if resultErr != nil {
			continue
		}
		if result.item.ErrorCode == "canceled" {
			continue
		}
		result.item = canonicalCheckpointReportItem(result.item)
		if err := persistResult(result); err != nil {
			resultErr = err
			cancelWork()
			continue
		}
		switch result.class {
		case "unique_complete":
			generated.UniqueComplete = append(generated.UniqueComplete, result.item)
		case "ambiguous":
			generated.Ambiguous = append(generated.Ambiguous, result.item)
		case "missing":
			generated.Missing = append(generated.Missing, result.item)
		case "incomplete":
			generated.Incomplete = append(generated.Incomplete, result.item)
		case "error":
			generated.Error = append(generated.Error, result.item)
		default:
			resultErr = fmt.Errorf("unsupported result class %q", result.class)
			cancelWork()
		}
	}
	if resultErr != nil {
		return report{}, resultErr
	}
	if err := ctx.Err(); err != nil {
		return report{}, err
	}
	if checkpoint != nil {
		generated, availableEvidence, checkpointCompleted, err = checkpoint.reconstruct(ctx)
		if err != nil {
			return report{}, err
		}
		if len(checkpointCompleted) != len(catalog) {
			return report{}, errors.New("checkpoint is missing completed catalog work")
		}
	}
	sortReport(&generated)
	generated.Summary = reportSummary{
		CatalogReview: len(generated.CatalogReview), GameSizeEvidence: len(generated.GameSizeEvidence),
		UniqueComplete: len(generated.UniqueComplete), Ambiguous: len(generated.Ambiguous),
		Missing: len(generated.Missing), Incomplete: len(generated.Incomplete), Error: len(generated.Error),
	}
	receipt, err := evidenceReceiptForReport(generated, availableEvidence)
	if err != nil {
		return report{}, err
	}
	generated.EvidenceReceipt = receipt
	if checkpoint != nil {
		if err := checkpoint.progress(progress, "complete"); err != nil {
			return report{}, err
		}
	}
	return generated, nil
}

func inspectFixedResumeTarget(ctx context.Context, opts options, source sourceClient, item catalogItem, target model.CatalogLyricsTarget, prior fixedResumeWork) classifiedResult {
	resultItem := baseReportItem(item, target)
	// No new search is issued, but the original bounded search evidence remains
	// part of the resumable audit trail for every exact fixed candidate.
	resultItem.SearchAttempts = prior.item.SearchAttempts
	if prior.item.SearchDiagnostics != nil {
		copy := *prior.item.SearchDiagnostics
		resultItem.SearchDiagnostics = &copy
	}
	if len(prior.candidates) == 0 {
		resultItem.ErrorCode = "malformed_resume_candidate"
		return classifiedResult{class: "error", item: resultItem}
	}
	return inspectFixedCandidates(ctx, opts, source, sourceIdentity(item), resultItem, prior.candidates)
}

// inspectTarget retains only bounded identity metadata and counts from private
// source responses; raw evidence is returned separately for the private receipt.
func inspectTarget(ctx context.Context, opts options, source sourceClient, item catalogItem, target model.CatalogLyricsTarget) classifiedResult {
	resultItem := baseReportItem(item, target)
	identity := sourceIdentity(item)
	search := retryOperation(ctx, opts, func(attemptCtx context.Context) (sourceSearchResult, error) {
		if diagnostic, ok := source.(diagnosticSourceClient); ok {
			candidates, diagnostics, err := diagnostic.SearchWithDiagnostics(attemptCtx, identity)
			return sourceSearchResult{Candidates: candidates, Diagnostics: reportSearchDiagnostics(diagnostics)}, err
		}
		candidates, err := source.Search(attemptCtx, identity)
		return sourceSearchResult{Candidates: candidates}, err
	})
	resultItem.SearchAttempts = search.attempts
	if search.err != nil {
		resultItem.ErrorCode = safeErrorCode(search.err)
		return classifiedResult{class: "error", item: resultItem}
	}
	resultItem.SearchDiagnostics = search.value.Diagnostics
	if len(search.value.Candidates) == 0 {
		if resultItem.SearchDiagnostics == nil {
			resultItem.SearchDiagnostics = &searchDiagnostics{}
		}
		resultItem.ReasonCode = missingSearchReason(resultItem.SearchDiagnostics)
		return classifiedResult{class: "missing", item: resultItem}
	}
	if len(search.value.Candidates) > maxReportCandidates {
		resultItem.ErrorCode = "candidate_limit_exceeded"
		resultItem.Candidates = []candidateSummary{}
		return classifiedResult{class: "ambiguous", item: resultItem}
	}
	if err := lyricssource.ValidateCandidatesIndexEvidence(search.value.Candidates); err != nil {
		resultItem.ErrorCode = "malformed_candidate_evidence"
		return classifiedResult{class: "error", item: resultItem}
	}
	summaries := make([]candidateSummary, len(search.value.Candidates))
	seenIdentities := make(map[candidateIdentityKey]struct{}, len(summaries))
	providerCounts := make(map[model.LyricsSourceProvider]int, len(summaries))
	for index, candidate := range search.value.Candidates {
		summary, err := summarizeCandidate(candidate)
		if err != nil {
			resultItem.ErrorCode = "malformed_candidate"
			return classifiedResult{class: "error", item: resultItem}
		}
		key := summary.identityKey()
		if _, exists := seenIdentities[key]; exists {
			resultItem.ErrorCode = "duplicate_candidate_identity"
			return classifiedResult{class: "error", item: resultItem}
		}
		seenIdentities[key] = struct{}{}
		providerCounts[summary.Provider]++
		summaries[index] = summary
	}
	for _, count := range providerCounts {
		if count > 1 {
			resultItem.Candidates = summaries
			return classifiedResult{class: "ambiguous", item: resultItem, evidence: candidateEvidence(search.value.Candidates)}
		}
	}
	return inspectFixedCandidates(ctx, opts, source, identity, resultItem, search.value.Candidates)
}

func inspectFixedCandidates(
	ctx context.Context,
	opts options,
	source sourceClient,
	identity lyricssource.MusicIdentity,
	resultItem reportItem,
	candidates []lyricssource.Candidate,
) classifiedResult {
	summaries := make([]candidateSummary, len(candidates))
	for index, candidate := range candidates {
		summary, err := summarizeCandidate(candidate)
		if err != nil || lyricssource.ValidateCandidateIndexEvidence(candidate) != nil {
			resultItem.ErrorCode = "malformed_candidate_evidence"
			return classifiedResult{class: "error", item: resultItem}
		}
		summaries[index] = summary
	}
	fixed := make([]lyricssource.FixedRevision, len(candidates))
	maximumAttempts := 0
	for index, candidate := range candidates {
		fetch := retryOperation(ctx, opts, func(attemptCtx context.Context) (lyricssource.FixedRevision, error) {
			return source.FetchFixedCandidateRevision(attemptCtx, identity, candidate)
		})
		if fetch.attempts > maximumAttempts {
			maximumAttempts = fetch.attempts
		}
		resultItem.FetchAttempts = maximumAttempts
		if fetch.err != nil {
			resultItem.Candidate = &summaries[index]
			resultItem.ErrorCode = safeErrorCode(fetch.err)
			class := "error"
			if incompleteSourceError(fetch.err) {
				class = "incomplete"
			}
			return classifiedResult{class: class, item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
		}
		if errorCode := fixedRevisionErrorCode(candidate, fetch.value); errorCode != "" {
			resultItem.Candidate = &summaries[index]
			resultItem.ErrorCode = errorCode
			return classifiedResult{class: "incomplete", item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
		}
		fixed[index] = fetch.value
	}

	candidateIdentities := make([]lyricsstaging.CandidateIdentity, len(summaries))
	for index, summary := range summaries {
		candidateIdentities[index] = stagingCandidate(summary)
	}
	artifactKeys, err := lyricsstaging.ResolveArtifactRenditionKeys(candidateIdentities)
	if err != nil {
		resultItem.ErrorCode = "artifact_identity_conflict"
		return classifiedResult{class: "error", item: resultItem}
	}
	compositionInputs := make([]lyricscompose.FixedArtifactInput, len(fixed))
	for index := range fixed {
		compositionInputs[index] = lyricscompose.FixedArtifactInput{
			SourceKey: artifactKeys[index], LogicalRenditionKey: candidates[index].RenditionKey, Fixed: fixed[index],
		}
	}
	composition, err := lyricscompose.ComposeFixedArtifacts(compositionInputs)
	if err != nil {
		if len(candidates) > 1 {
			for index := range summaries {
				summaries[index].ArtifactRenditionKey = artifactKeys[index]
			}
			resultItem.Candidate = nil
			resultItem.FixedArtifactCandidates = summaries
			resultItem.PostFetchState = lyricsstaging.PostFetchStateVersionConflict
			resultItem.CompositionReason = model.LyricsSourceVersionReasonVersionConflict
			resultItem.ErrorCode = string(model.LyricsSourceVersionReasonVersionConflict)
			return classifiedResult{class: "incomplete", item: resultItem, evidence: candidateEvidence(candidates)}
		}
		resultItem.Candidate = &summaries[0]
		resultItem.ErrorCode = "composition_conflict"
		return classifiedResult{class: "incomplete", item: resultItem, evidence: candidateEvidence(candidates)}
	}
	selected := make(map[string]struct{}, len(composition.SelectedSourceKeys))
	for _, key := range composition.SelectedSourceKeys {
		selected[key] = struct{}{}
	}
	selectedSummaries := make([]candidateSummary, 0, len(selected))
	selectedCandidates := make([]lyricssource.Candidate, 0, len(selected))
	var primary *candidateSummary
	for index, summary := range summaries {
		if _, keep := selected[artifactKeys[index]]; !keep {
			continue
		}
		summary.ArtifactRenditionKey = artifactKeys[index]
		selectedSummaries = append(selectedSummaries, summary)
		selectedCandidates = append(selectedCandidates, candidates[index])
		if artifactKeys[index] == composition.Components.FullText {
			copy := summary
			primary = &copy
		}
	}
	if primary == nil || len(selectedSummaries) == 0 {
		resultItem.ErrorCode = "composition_conflict"
		return classifiedResult{class: "error", item: resultItem}
	}
	resultItem.Candidate = primary
	resultItem.FixedArtifactCandidates = selectedSummaries
	resultItem.PostFetchState = lyricsstaging.PostFetchStateComplete
	resultItem.CompositionReason = composition.ReasonCode
	resultItem.LineCount = len(composition.Full.Lines)
	return classifiedResult{class: "unique_complete", item: resultItem, evidence: candidateEvidence(selectedCandidates)}
}

func classifyFixedRevision(resultItem reportItem, candidate lyricssource.Candidate, fetch attemptResult[lyricssource.FixedRevision]) classifiedResult {
	if err := lyricssource.ValidateCandidateIndexEvidence(candidate); err != nil {
		resultItem.ErrorCode = "malformed_candidate_evidence"
		return classifiedResult{class: "error", item: resultItem}
	}
	if fetch.err != nil {
		summary, _ := summarizeCandidate(candidate)
		resultItem.Candidate = &summary
		resultItem.FetchAttempts = fetch.attempts
		resultItem.ErrorCode = safeErrorCode(fetch.err)
		if incompleteSourceError(fetch.err) {
			return classifiedResult{class: "incomplete", item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
		}
		return classifiedResult{class: "error", item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
	}
	return inspectComposedFixedForTest(resultItem, candidate, fetch)
}

func inspectComposedFixedForTest(resultItem reportItem, candidate lyricssource.Candidate, fetch attemptResult[lyricssource.FixedRevision]) classifiedResult {
	resultItem.FetchAttempts = fetch.attempts
	if errorCode := fixedRevisionErrorCode(candidate, fetch.value); errorCode != "" {
		summary, _ := summarizeCandidate(candidate)
		resultItem.Candidate = &summary
		resultItem.ErrorCode = errorCode
		return classifiedResult{class: "incomplete", item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
	}
	summary, _ := summarizeCandidate(candidate)
	keyCandidates := []lyricsstaging.CandidateIdentity{stagingCandidate(summary)}
	keys, _ := lyricsstaging.ResolveArtifactRenditionKeys(keyCandidates)
	composition, err := lyricscompose.ComposeFixedArtifacts([]lyricscompose.FixedArtifactInput{{
		SourceKey: keys[0], LogicalRenditionKey: candidate.RenditionKey, Fixed: fetch.value,
	}})
	if err != nil {
		resultItem.Candidate = &summary
		resultItem.ErrorCode = "composition_conflict"
		return classifiedResult{class: "incomplete", item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
	}
	summary.ArtifactRenditionKey = keys[0]
	resultItem.Candidate = &summary
	resultItem.FixedArtifactCandidates = []candidateSummary{summary}
	resultItem.PostFetchState = lyricsstaging.PostFetchStateComplete
	resultItem.CompositionReason = composition.ReasonCode
	resultItem.LineCount = len(composition.Full.Lines)
	return classifiedResult{class: "unique_complete", item: resultItem, evidence: candidateEvidence([]lyricssource.Candidate{candidate})}
}

func fixedRevisionErrorCode(candidate lyricssource.Candidate, fixed lyricssource.FixedRevision) string {
	if fixed.Provider != candidate.Provider || fixed.Origin != candidate.Origin || fixed.PageID != candidate.PageID ||
		fixed.RevisionID != candidate.RevisionID || fixed.SHA1 != candidate.SHA1 || fixed.PageTitle != candidate.Title ||
		fixed.CanonicalURL != candidate.CanonicalURL || !equalCanonicalCategories(fixed.Categories, candidate.Categories) ||
		fixed.Section != candidate.Section || fixed.RenditionKey != candidate.RenditionKey ||
		fixed.VersionReason != candidate.VersionReason || !equalIndexEvidenceRefs(fixed.IndexEvidenceRefs, candidate.IndexEvidenceRefs) ||
		!reflect.DeepEqual(fixed.IndexEvidence, candidate.IndexEvidence) {
		return "source_identity_drift"
	}
	if fixed.FetchedAt.IsZero() || len(fixed.Lines) == 0 || len(fixed.Lines) > 1000 ||
		len(fixed.Extraction.Lines) != len(fixed.Lines) {
		return "invalid_extraction_size"
	}
	if fixed.Provider == model.LyricsSourceProviderSekaipedia {
		if len(fixed.Wikitext) == 0 || len(fixed.Wikitext) > 2<<20 ||
			!bytes.Equal(fixed.Wikitext, lyricssource.SekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)) ||
			fixed.RevisionTimestamp.IsZero() ||
			fixed.RevisionTimestamp.UTC().Format(time.RFC3339Nano) != candidate.RevisionTimestamp ||
			!canonicalHex(fixed.RawSHA256, 64) ||
			(candidate.RawSHA256 != "" && fixed.RawSHA256 != candidate.RawSHA256) {
			return "source_identity_drift"
		}
	} else {
		if len(fixed.Wikitext) == 0 || len(fixed.Wikitext) > 2<<20 {
			return "invalid_extraction_size"
		}
		digest := sha1.Sum(fixed.Wikitext)
		if hex.EncodeToString(digest[:]) != fixed.SHA1 {
			return "source_identity_drift"
		}
	}
	totalExtractedBytes := 0
	for index, line := range fixed.Lines {
		if line.Japanese == "" || strings.TrimSpace(line.Japanese) == "" || len(line.Japanese) > 8<<10 ||
			line.Japanese != fixed.Extraction.Lines[index].Japanese ||
			line.StanzaBreakBefore != fixed.Extraction.Lines[index].StanzaBreakBefore {
			return "invalid_extraction_line"
		}
		totalExtractedBytes += len(line.Japanese)
		if totalExtractedBytes > 1<<20 {
			return "extraction_too_large"
		}
	}
	return ""
}

func stagingCandidate(candidate candidateSummary) lyricsstaging.CandidateIdentity {
	return lyricsstaging.CandidateIdentity{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, RevisionTimestamp: candidate.RevisionTimestamp,
		SHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		Section: candidate.Section, RenditionKey: candidate.RenditionKey,
		ArtifactRenditionKey: candidate.ArtifactRenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
	}
}

func candidateEvidence(candidates []lyricssource.Candidate) []lyricssource.IndexEvidence {
	result := []lyricssource.IndexEvidence{}
	for _, candidate := range candidates {
		result = append(result, candidate.IndexEvidence...)
	}
	return result
}
