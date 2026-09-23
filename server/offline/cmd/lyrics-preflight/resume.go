package main

import (
	"crypto/sha256"

	"errors"

	"fmt"
	"io"

	"os"

	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"time"

	"moesekai/server/internal/legacy"

	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

type resumeReportLoadHook func(stage, absolutePath string) error

const (
	resumeReportLoadAfterInitialStat = "after_initial_stat"
	resumeReportLoadAfterOpen        = "after_open"
	resumeReportLoadAfterRead        = "after_read"
	resumeReportLoadAfterDecode      = "after_decode"
)

func completeReportSizeWithinReviewedContract(size int64) bool {
	return size >= 0 && size <= int64(maxCompleteReportBytes)
}

func loadResumeReport(reportPath string) (report, error) {
	return loadResumeReportWithHook(reportPath, nil)
}

// loadResumeReportWithHook is a deterministic test seam for mutating the path
// between the loader's pinned snapshot checks.
func loadResumeReportWithHook(reportPath string, hook resumeReportLoadHook) (report, error) {
	if strings.TrimSpace(reportPath) == "" || reportPath != strings.TrimSpace(reportPath) || reportPath == "-" {
		return report{}, errors.New("invalid resume report path")
	}
	absolutePath, err := filepath.Abs(reportPath)
	if err != nil {
		return report{}, fmt.Errorf("resolve resume report path: %w", err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return report{}, fmt.Errorf("inspect resume report: %w", err)
	}
	if !info.Mode().IsRegular() {
		return report{}, errors.New("resume report path must identify a regular file")
	}
	if info.Size() <= 0 || !completeReportSizeWithinReviewedContract(info.Size()) {
		return report{}, fmt.Errorf("resume report must be between 1 and %d bytes", maxCompleteReportBytes)
	}
	if err := runResumeReportLoadHook(hook, resumeReportLoadAfterInitialStat, absolutePath); err != nil {
		return report{}, err
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return report{}, fmt.Errorf("open resume report: %w", err)
	}
	defer file.Close()

	digest, err := resumeReportSnapshotDigest(file, absolutePath, info, info.Size(), "after open")
	if err != nil {
		return report{}, err
	}
	if err := runResumeReportLoadHook(hook, resumeReportLoadAfterOpen, absolutePath); err != nil {
		return report{}, err
	}
	if err := verifyResumeReportSnapshot(file, absolutePath, info, info.Size(), digest, "after open hook"); err != nil {
		return report{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return report{}, fmt.Errorf("seek pinned resume report: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxCompleteReportBytes+1))
	if err != nil {
		return report{}, fmt.Errorf("read resume report: %w", err)
	}
	if !completeReportSizeWithinReviewedContract(int64(len(body))) {
		return report{}, fmt.Errorf("resume report exceeds %d bytes", maxCompleteReportBytes)
	}
	if int64(len(body)) != info.Size() || sha256.Sum256(body) != digest {
		return report{}, errors.New("resume report snapshot bytes changed while reading")
	}
	if err := runResumeReportLoadHook(hook, resumeReportLoadAfterRead, absolutePath); err != nil {
		return report{}, err
	}
	if err := verifyResumeReportSnapshot(file, absolutePath, info, info.Size(), digest, "after read"); err != nil {
		return report{}, err
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return report{}, fmt.Errorf("decode resume report: %w", err)
	}
	var prior report
	if err := decodeClosedJSON(body, &prior); err != nil {
		return report{}, fmt.Errorf("decode resume report: %w", err)
	}
	if err := runResumeReportLoadHook(hook, resumeReportLoadAfterDecode, absolutePath); err != nil {
		return report{}, err
	}
	if err := verifyResumeReportSnapshot(file, absolutePath, info, info.Size(), digest, "after decode"); err != nil {
		return report{}, err
	}
	return prior, nil
}

func runResumeReportLoadHook(hook resumeReportLoadHook, stage, absolutePath string) error {
	if hook == nil {
		return nil
	}
	if err := hook(stage, absolutePath); err != nil {
		return fmt.Errorf("resume report load hook %s: %w", stage, err)
	}
	return nil
}

func verifyResumeReportSnapshot(file *os.File, absolutePath string, info os.FileInfo, size int64, expected [sha256.Size]byte, stage string) error {
	digest, err := resumeReportSnapshotDigest(file, absolutePath, info, size, stage)
	if err != nil {
		return err
	}
	if digest != expected {
		return fmt.Errorf("resume report snapshot bytes changed %s", stage)
	}
	return nil
}

func resumeReportSnapshotDigest(file *os.File, absolutePath string, info os.FileInfo, size int64, stage string) ([sha256.Size]byte, error) {
	if err := verifyResumeReportSnapshotIdentity(file, absolutePath, info, size, stage+" before hash"); err != nil {
		return [sha256.Size]byte{}, err
	}
	digest, err := hashResumeReportSnapshot(file, size)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash pinned resume report %s: %w", stage, err)
	}
	if err := verifyResumeReportSnapshotIdentity(file, absolutePath, info, size, stage+" after hash"); err != nil {
		return [sha256.Size]byte{}, err
	}
	return digest, nil
}

func verifyResumeReportSnapshotIdentity(file *os.File, absolutePath string, info os.FileInfo, size int64, stage string) error {
	if file == nil || info == nil {
		return errors.New("resume report snapshot is not active")
	}
	fileInfo, fileErr := file.Stat()
	pathInfo, pathErr := os.Stat(absolutePath)
	if fileErr != nil || pathErr != nil || !fileInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(info, fileInfo) || !os.SameFile(info, pathInfo) || fileInfo.Size() != size || pathInfo.Size() != size {
		return fmt.Errorf("resume report snapshot path, inode, or size changed %s", stage)
	}
	return nil
}

func hashResumeReportSnapshot(file *os.File, size int64) ([sha256.Size]byte, error) {
	if file == nil || size <= 0 || !completeReportSizeWithinReviewedContract(size) {
		return [sha256.Size]byte{}, errors.New("invalid resume report snapshot")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, file, size); err != nil {
		return [sha256.Size]byte{}, err
	}
	var extra [1]byte
	count, err := file.Read(extra[:])
	if count != 0 || !errors.Is(err, io.EOF) {
		if err != nil && !errors.Is(err, io.EOF) {
			return [sha256.Size]byte{}, err
		}
		return [sha256.Size]byte{}, errors.New("resume report snapshot size changed")
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func mergeResumeReport(generated *report, prior report, catalog []catalogItem, targets []model.CatalogLyricsTarget, retryErrorCodes, retryMissingReasons, retryIncompleteCodes map[string]struct{}, retryUniqueComplete bool) error {
	if generated == nil {
		return errors.New("generated report is required")
	}
	if err := validateResumeReportEnvelope(prior); err != nil {
		return err
	}
	if prior.CatalogCount != len(catalog) {
		return fmt.Errorf("resume report catalog count %d does not match current catalog count %d", prior.CatalogCount, len(catalog))
	}
	catalogByID := make(map[int]catalogItem, len(catalog))
	for _, item := range catalog {
		catalogByID[item.MusicID] = item
	}
	targetByID := make(map[int]model.CatalogLyricsTarget, len(targets))
	for _, target := range targets {
		targetByID[target.MusicID] = target
	}

	seen := make(map[int]string, prior.CatalogCount)
	copyClass := func(class string, items []reportItem, destination *[]reportItem) error {
		for _, item := range items {
			if err := validateResumeReportItem(class, item); err != nil {
				return err
			}
			if previousClass, exists := seen[item.MusicID]; exists {
				return fmt.Errorf("resume report music %d appears in both %s and %s", item.MusicID, previousClass, class)
			}
			seen[item.MusicID] = class
			catalogItem, exists := catalogByID[item.MusicID]
			if !exists {
				return fmt.Errorf("resume report music %d is not present in the current catalog", item.MusicID)
			}
			if item.JapaneseTitle != catalogItem.JapaneseTitle {
				return fmt.Errorf("resume report music %d title does not match the current catalog", item.MusicID)
			}
			if item.CatalogFingerprint != catalogItem.CatalogFingerprint {
				return fmt.Errorf("resume report music %d fingerprint does not match the current catalog", item.MusicID)
			}
			target, exists := targetByID[item.MusicID]
			if !exists || !resumeItemMatchesTarget(class, item, target) {
				return fmt.Errorf("resume report music %d classification does not match the current catalog", item.MusicID)
			}
			*destination = append(*destination, item)
		}
		return nil
	}
	for _, class := range []struct {
		name        string
		items       []reportItem
		destination *[]reportItem
	}{
		{name: "catalog_review", items: prior.CatalogReview, destination: &generated.CatalogReview},
		{name: "game_size_evidence", items: prior.GameSizeEvidence, destination: &generated.GameSizeEvidence},
		{name: "unique_complete", items: prior.UniqueComplete, destination: &generated.UniqueComplete},
		{name: "ambiguous", items: prior.Ambiguous, destination: &generated.Ambiguous},
		{name: "missing", items: prior.Missing, destination: &generated.Missing},
		{name: "incomplete", items: prior.Incomplete, destination: &generated.Incomplete},
		{name: "error", items: prior.Error, destination: &generated.Error},
	} {
		if err := copyClass(class.name, class.items, class.destination); err != nil {
			return err
		}
	}
	if len(seen) != len(catalog) {
		return fmt.Errorf("resume report contains %d unique items, want %d", len(seen), len(catalog))
	}
	for errorIndex := 0; errorIndex < len(generated.Error); {
		item := generated.Error[errorIndex]
		if _, retry := retryErrorCodes[item.ErrorCode]; !retry {
			errorIndex++
			continue
		}
		if !safeResumeErrorItem(item) {
			return fmt.Errorf("resume report music %d error %q is not safe to retry from its recorded phase", item.MusicID, item.ErrorCode)
		}
		generated.Error = append(generated.Error[:errorIndex], generated.Error[errorIndex+1:]...)
	}
	for missingIndex := 0; missingIndex < len(generated.Missing); {
		if _, retry := retryMissingReasons[generated.Missing[missingIndex].ReasonCode]; !retry {
			missingIndex++
			continue
		}
		generated.Missing = append(generated.Missing[:missingIndex], generated.Missing[missingIndex+1:]...)
	}
	for incompleteIndex := 0; incompleteIndex < len(generated.Incomplete); {
		if _, retry := retryIncompleteCodes[generated.Incomplete[incompleteIndex].ErrorCode]; !retry {
			incompleteIndex++
			continue
		}
		generated.Incomplete = append(generated.Incomplete[:incompleteIndex], generated.Incomplete[incompleteIndex+1:]...)
	}
	if retryUniqueComplete {
		generated.UniqueComplete = generated.UniqueComplete[:0]
	}
	return nil
}

func validateResumeReportEnvelope(prior report) error {
	parsedGeneratedAt, err := time.Parse(time.RFC3339Nano, prior.GeneratedAt)
	if err != nil || parsedGeneratedAt.Format(time.RFC3339Nano) != prior.GeneratedAt || !strings.HasSuffix(prior.GeneratedAt, "Z") {
		return errors.New("invalid resume report timestamp")
	}
	if prior.CatalogReview == nil || prior.GameSizeEvidence == nil || prior.UniqueComplete == nil ||
		prior.Ambiguous == nil || prior.Missing == nil || prior.Incomplete == nil || prior.Error == nil {
		return errors.New("invalid resume report arrays")
	}
	classifiedCount := len(prior.CatalogReview) + len(prior.GameSizeEvidence) + len(prior.UniqueComplete) +
		len(prior.Ambiguous) + len(prior.Missing) + len(prior.Incomplete) + len(prior.Error)
	if prior.SchemaVersion != reportSchemaVersion || prior.CatalogSchemaVersion != catalogSchemaVersion ||
		prior.CatalogCount < 0 || classifiedCount != prior.CatalogCount ||
		prior.Summary.CatalogReview != len(prior.CatalogReview) ||
		prior.Summary.GameSizeEvidence != len(prior.GameSizeEvidence) ||
		prior.Summary.UniqueComplete != len(prior.UniqueComplete) || prior.Summary.Ambiguous != len(prior.Ambiguous) ||
		prior.Summary.Missing != len(prior.Missing) || prior.Summary.Incomplete != len(prior.Incomplete) ||
		prior.Summary.Error != len(prior.Error) {
		return errors.New("invalid resume report envelope")
	}
	return nil
}

func validateClassifiedReportItems(generated report) error {
	seen := make(map[int]string, generated.CatalogCount)
	for _, classified := range []struct {
		class string
		items []reportItem
	}{
		{class: "catalog_review", items: generated.CatalogReview},
		{class: "game_size_evidence", items: generated.GameSizeEvidence},
		{class: "unique_complete", items: generated.UniqueComplete},
		{class: "ambiguous", items: generated.Ambiguous},
		{class: "missing", items: generated.Missing},
		{class: "incomplete", items: generated.Incomplete},
		{class: "error", items: generated.Error},
	} {
		for _, item := range classified.items {
			if err := validateResumeReportItem(classified.class, item); err != nil {
				return err
			}
			if previousClass, exists := seen[item.MusicID]; exists {
				return fmt.Errorf("preflight report music %d appears in both %s and %s", item.MusicID, previousClass, classified.class)
			}
			seen[item.MusicID] = classified.class
		}
	}
	return nil
}

func validateResumeReportItem(class string, item reportItem) error {
	if item.MusicID <= 0 || item.JapaneseTitle == "" || strings.TrimSpace(item.JapaneseTitle) != item.JapaneseTitle ||
		len(item.JapaneseTitle) > maxCandidateTitle || strings.ContainsAny(item.JapaneseTitle, "\r\n") ||
		item.CatalogFingerprint == "" || item.SearchAttempts < 0 || item.SearchAttempts > maxAttempts ||
		item.FetchAttempts < 0 || item.FetchAttempts > maxAttempts || item.LineCount < 0 ||
		len(item.AssociationMusicIDs) > maxCatalogRecords || len(item.Candidates) > maxReportCandidates ||
		len(item.FixedArtifactCandidates) > lyricsstaging.MaxFixedArtifactBundleArtifacts ||
		(item.PostFetchState != "" && item.PostFetchState != lyricsstaging.PostFetchStateComplete && item.PostFetchState != lyricsstaging.PostFetchStateVersionConflict) ||
		(item.CompositionReason != "" && !model.IsValidLyricsSourceVersionReasonCode(item.CompositionReason)) {
		return fmt.Errorf("resume report %s item has an invalid public surface", class)
	}
	if item.Candidate != nil {
		if err := validateCandidateSummary(*item.Candidate); err != nil {
			return fmt.Errorf("resume report music %d candidate: %w", item.MusicID, err)
		}
	}
	for _, candidate := range item.Candidates {
		if err := validateCandidateSummary(candidate); err != nil {
			return fmt.Errorf("resume report music %d candidate: %w", item.MusicID, err)
		}
	}
	containsSelectedCandidate := false
	fixedIdentities := make([]lyricsstaging.CandidateIdentity, len(item.FixedArtifactCandidates))
	for index, candidate := range item.FixedArtifactCandidates {
		if err := validateCandidateSummary(candidate); err != nil {
			return fmt.Errorf("resume report music %d fixed artifact candidate: %w", item.MusicID, err)
		}
		fixedIdentities[index] = stagingCandidate(candidate)
		containsSelectedCandidate = containsSelectedCandidate || item.Candidate != nil && reflect.DeepEqual(candidate, *item.Candidate)
	}
	if _, err := lyricsstaging.ResolveArtifactRenditionKeys(fixedIdentities); err != nil {
		return fmt.Errorf("resume report music %d fixed artifact candidates: %w", item.MusicID, err)
	}
	if len(item.FixedArtifactCandidates) > 0 && item.PostFetchState != lyricsstaging.PostFetchStateVersionConflict && !containsSelectedCandidate {
		return fmt.Errorf("resume report music %d fixed artifacts omit the selected candidate", item.MusicID)
	}
	seenAssociations := make(map[int]struct{}, len(item.AssociationMusicIDs))
	for _, association := range item.AssociationMusicIDs {
		// The classifier stores the anchor's complete association set on every
		// row in the work group. A game-size evidence row may therefore contain
		// its own music ID while pointing at a different TargetMusicID.
		if association <= 0 || association == item.TargetMusicID ||
			(association == item.MusicID && item.TargetMusicID == item.MusicID) {
			return fmt.Errorf("resume report music %d has an invalid association", item.MusicID)
		}
		if _, exists := seenAssociations[association]; exists {
			return fmt.Errorf("resume report music %d has duplicate associations", item.MusicID)
		}
		seenAssociations[association] = struct{}{}
	}
	seenCandidates := make(map[candidateIdentityKey]struct{}, len(item.Candidates))
	for _, candidate := range item.Candidates {
		key := candidate.identityKey()
		if _, exists := seenCandidates[key]; exists {
			return fmt.Errorf("resume report music %d has duplicate candidate identities", item.MusicID)
		}
		seenCandidates[key] = struct{}{}
	}
	if item.SearchDiagnostics != nil {
		diagnostics := item.SearchDiagnostics
		for _, count := range []int{
			diagnostics.SearchHits, diagnostics.Restricted, diagnostics.RestrictedTitleMatch,
			diagnostics.TitleMismatch, diagnostics.CreditMismatch, diagnostics.LyricistCreditMissing,
			diagnostics.LyricistCreditMismatch, diagnostics.ComposerCreditMissing,
			diagnostics.ComposerCreditMismatch, diagnostics.ArrangerCreditMissing,
			diagnostics.ArrangerCreditMismatch, diagnostics.SignalMismatch, diagnostics.Verified,
		} {
			if count < 0 || count > maxCatalogRecords {
				return fmt.Errorf("resume report music %d has malformed search diagnostics", item.MusicID)
			}
		}
		if diagnostics.RestrictedTitleMatch > diagnostics.Restricted || diagnostics.TitleMismatch > diagnostics.SearchHits ||
			diagnostics.CreditMismatch > diagnostics.SearchHits || diagnostics.SignalMismatch > diagnostics.SearchHits ||
			diagnostics.Verified > diagnostics.SearchHits {
			return fmt.Errorf("resume report music %d has malformed search diagnostics", item.MusicID)
		}
	}
	switch class {
	case "catalog_review", "game_size_evidence":
		if item.Candidate != nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 || item.SearchAttempts != 0 ||
			item.FetchAttempts != 0 || item.ErrorCode != "" || item.SearchDiagnostics != nil || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("resume report music %d has an invalid catalog-only item", item.MusicID)
		}
	case "unique_complete":
		if item.ReasonCode != "" || item.Candidate == nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) == 0 || item.LineCount <= 0 ||
			item.SearchAttempts <= 0 || item.FetchAttempts <= 0 || item.ErrorCode != "" || item.CompositionReason == "" ||
			item.PostFetchState == lyricsstaging.PostFetchStateVersionConflict || item.CompositionReason == model.LyricsSourceVersionReasonVersionConflict {
			return fmt.Errorf("resume report music %d has an invalid unique-complete item", item.MusicID)
		}
	case "ambiguous":
		if item.ReasonCode != "" || item.Candidate != nil || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 || item.SearchAttempts <= 0 ||
			item.FetchAttempts != 0 || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("resume report music %d has an invalid ambiguous item", item.MusicID)
		}
		if item.ErrorCode == "candidate_limit_exceeded" {
			if len(item.Candidates) != 0 {
				return fmt.Errorf("resume report music %d has an invalid candidate-limit item", item.MusicID)
			}
		} else if item.ErrorCode != "" || len(item.Candidates) < 2 {
			return fmt.Errorf("resume report music %d has an invalid ambiguous candidate set", item.MusicID)
		}
	case "missing":
		if item.Candidate != nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 || item.SearchAttempts <= 0 ||
			item.FetchAttempts != 0 || item.ErrorCode != "" || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("resume report music %d has an invalid missing item", item.MusicID)
		}
		if item.SearchDiagnostics == nil {
			return fmt.Errorf("resume report music %d has a missing reason without diagnostics", item.MusicID)
		}
		if item.ReasonCode != missingSearchReason(item.SearchDiagnostics) {
			return fmt.Errorf("resume report music %d has inconsistent missing diagnostics", item.MusicID)
		}
	case "incomplete":
		if item.PostFetchState == lyricsstaging.PostFetchStateVersionConflict {
			if item.ReasonCode != "" || item.Candidate != nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) < 2 ||
				item.LineCount != 0 || item.SearchAttempts <= 0 || item.FetchAttempts <= 0 ||
				item.ErrorCode != string(model.LyricsSourceVersionReasonVersionConflict) ||
				item.CompositionReason != model.LyricsSourceVersionReasonVersionConflict {
				return fmt.Errorf("resume report music %d has an invalid incomplete item", item.MusicID)
			}
			break
		}
		if item.ReasonCode != "" || item.Candidate == nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 ||
			item.SearchAttempts <= 0 || item.FetchAttempts <= 0 || item.ErrorCode == "" || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("resume report music %d has an invalid incomplete item", item.MusicID)
		}
	case "error":
		if item.ErrorCode == "" || item.ReasonCode != "" || item.LineCount != 0 || item.SearchAttempts <= 0 ||
			len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || (item.Candidate == nil && item.FetchAttempts != 0) ||
			(item.Candidate != nil && item.FetchAttempts <= 0) || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("resume report music %d has an invalid error item", item.MusicID)
		}
	default:
		return fmt.Errorf("unsupported resume report class %q", class)
	}
	return nil
}

func validateCandidateSummary(candidate candidateSummary) error {
	_, err := summarizeCandidate(candidate.sourceCandidate())
	return err
}

func resumeItemMatchesTarget(class string, item reportItem, target model.CatalogLyricsTarget) bool {
	if item.TargetMusicID != target.TargetMusicID || !equalMusicIDs(item.AssociationMusicIDs, target.AssociationMusicIDs) ||
		item.CatalogFingerprint != target.CatalogFingerprint {
		return false
	}
	switch target.Disposition {
	case model.LyricsCatalogTargetReview:
		return class == "catalog_review" && item.ReasonCode == target.ReasonCode
	case model.LyricsCatalogTargetGameSizeEvidence:
		return class == "game_size_evidence" && item.ReasonCode == target.ReasonCode
	case model.LyricsCatalogTargetFullTarget:
		return class != "catalog_review" && class != "game_size_evidence"
	default:
		return false
	}
}

func equalMusicIDs(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]int(nil), left...)
	rightCopy := append([]int(nil), right...)
	sort.Ints(leftCopy)
	sort.Ints(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func selectedResumeErrorCodes(opts options) (map[string]struct{}, error) {
	selected := make(map[string]struct{})
	if opts.ResumeReportPath == "" {
		return selected, nil
	}
	value := opts.ResumeErrorCodes
	if value == "" {
		value = "rate_limited"
	}
	for _, code := range strings.Split(value, ",") {
		if code == "" || code != strings.TrimSpace(code) {
			return nil, errors.New("-resume-error-codes must be a comma-separated list without whitespace")
		}
		if code == "none" {
			if value != "none" {
				return nil, errors.New("-resume-error-codes none must not be combined with error codes")
			}
			return selected, nil
		}
		if _, safe := safeResumeErrorCodes[code]; !safe {
			return nil, fmt.Errorf("-resume-error-codes contains unsafe or unsupported code %q", code)
		}
		if _, duplicate := selected[code]; duplicate {
			return nil, fmt.Errorf("-resume-error-codes contains duplicate code %q", code)
		}
		selected[code] = struct{}{}
	}
	return selected, nil
}

func selectedResumeMissingReasons(opts options) (map[string]struct{}, error) {
	selected := make(map[string]struct{})
	if opts.ResumeMissingReasons == "" {
		return selected, nil
	}
	if opts.ResumeReportPath == "" {
		return nil, errors.New("-resume-missing-reasons requires -resume-report")
	}
	value := opts.ResumeMissingReasons
	for _, reason := range strings.Split(value, ",") {
		if reason == "" || reason != strings.TrimSpace(reason) {
			return nil, errors.New("-resume-missing-reasons must be a comma-separated list without whitespace")
		}
		if reason == "none" {
			if value != "none" {
				return nil, errors.New("-resume-missing-reasons none must not be combined with missing reasons")
			}
			return selected, nil
		}
		if _, safe := safeResumeMissingReasons[reason]; !safe {
			return nil, fmt.Errorf("-resume-missing-reasons contains unsafe or unsupported reason %q", reason)
		}
		if _, duplicate := selected[reason]; duplicate {
			return nil, fmt.Errorf("-resume-missing-reasons contains duplicate reason %q", reason)
		}
		selected[reason] = struct{}{}
	}
	return selected, nil
}

func selectedResumeIncompleteCodes(opts options) (map[string]struct{}, error) {
	selected := make(map[string]struct{})
	if opts.ResumeReportPath == "" || opts.ResumeIncompleteCodes == "" {
		return selected, nil
	}
	for _, code := range strings.Split(opts.ResumeIncompleteCodes, ",") {
		if code == "" || code != strings.TrimSpace(code) {
			return nil, errors.New("-resume-incomplete-codes must be a comma-separated list without whitespace")
		}
		if _, safe := safeResumeIncompleteCodes[code]; !safe {
			return nil, fmt.Errorf("-resume-incomplete-codes contains unsafe or unsupported code %q", code)
		}
		if _, duplicate := selected[code]; duplicate {
			return nil, fmt.Errorf("-resume-incomplete-codes contains duplicate code %q", code)
		}
		selected[code] = struct{}{}
	}
	return selected, nil
}

func safeResumeErrorItem(item reportItem) bool {
	if item.ErrorCode != "malformed_response" {
		return true
	}
	// A malformed fixed-revision fetch already has a pinned candidate and must
	// never be converted into a fresh broad Search. Only a one-attempt malformed
	// search result can be explicitly resumed through this error channel.
	return item.Candidate == nil && item.FetchAttempts == 0 && item.SearchAttempts == 1
}

func retryResumeMusicID(prior report, musicID int, retryErrorCodes, retryMissingReasons, retryIncompleteCodes map[string]struct{}, retryUniqueComplete bool) bool {
	if retryUniqueComplete {
		for _, item := range prior.UniqueComplete {
			if item.MusicID == musicID {
				return true
			}
		}
	}
	for _, item := range prior.Error {
		if item.MusicID == musicID {
			_, retry := retryErrorCodes[item.ErrorCode]
			return retry && safeResumeErrorItem(item)
		}
	}
	for _, item := range prior.Missing {
		if item.MusicID == musicID {
			_, retry := retryMissingReasons[item.ReasonCode]
			return retry
		}
	}
	for _, item := range prior.Incomplete {
		if item.MusicID == musicID {
			_, retry := retryIncompleteCodes[item.ErrorCode]
			return retry
		}
	}
	return false
}

func sortReport(generated *report) {
	for _, items := range []*[]reportItem{
		&generated.CatalogReview, &generated.GameSizeEvidence, &generated.UniqueComplete,
		&generated.Ambiguous, &generated.Missing, &generated.Incomplete, &generated.Error,
	} {
		sort.Slice(*items, func(i, j int) bool { return (*items)[i].MusicID < (*items)[j].MusicID })
		for index := range *items {
			sort.Ints((*items)[index].AssociationMusicIDs)
			sort.Slice((*items)[index].Candidates, func(left, right int) bool {
				return candidateSummaryLess((*items)[index].Candidates[left], (*items)[index].Candidates[right])
			})
			sort.Slice((*items)[index].FixedArtifactCandidates, func(left, right int) bool {
				return candidateSummaryLess((*items)[index].FixedArtifactCandidates[left], (*items)[index].FixedArtifactCandidates[right])
			})
		}
	}
}

func candidateSummaryLess(left, right candidateSummary) bool {
	if left.Provider != right.Provider {
		return left.Provider < right.Provider
	}
	if left.PageID != right.PageID {
		return left.PageID < right.PageID
	}
	if left.RevisionID != right.RevisionID {
		return left.RevisionID < right.RevisionID
	}
	if left.Section != right.Section {
		return left.Section < right.Section
	}
	if left.RenditionKey != right.RenditionKey {
		return left.RenditionKey < right.RenditionKey
	}
	if left.Title != right.Title {
		return left.Title < right.Title
	}
	return left.CanonicalURL < right.CanonicalURL
}

func validateResumeReportPath(reportPath, databasePath, outputPath string) error {
	reportAbsolute, err := filepath.Abs(reportPath)
	if err != nil {
		return fmt.Errorf("resolve resume report path: %w", err)
	}
	databaseAbsolute, err := filepath.Abs(databasePath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	if filepath.Clean(reportAbsolute) == filepath.Clean(databaseAbsolute) {
		return errors.New("resume report path must not be the database path")
	}
	reportInfo, err := os.Stat(reportAbsolute)
	if err != nil {
		return fmt.Errorf("inspect resume report: %w", err)
	}
	if !reportInfo.Mode().IsRegular() {
		return errors.New("resume report path must identify a regular file")
	}
	databaseInfo, err := os.Stat(databaseAbsolute)
	if err == nil && os.SameFile(reportInfo, databaseInfo) {
		return errors.New("resume report path must not resolve to the database path")
	}
	if outputPath != "-" {
		outputAbsolute, err := filepath.Abs(outputPath)
		if err != nil {
			return fmt.Errorf("resolve report path: %w", err)
		}
		if filepath.Clean(reportAbsolute) == filepath.Clean(outputAbsolute) {
			return errors.New("output path must not be the resume report path")
		}
		if outputInfo, err := os.Stat(outputAbsolute); err == nil && os.SameFile(reportInfo, outputInfo) {
			return errors.New("output path must not resolve to the resume report path")
		}
	}
	return nil
}

func validateDistinctOutputPath(outputPath, databasePath string) error {
	if strings.TrimSpace(outputPath) == "" {
		return errors.New("report path is required")
	}
	outputAbsolute, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve report path: %w", err)
	}
	databaseAbsolute, err := filepath.Abs(databasePath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	if filepath.Clean(outputAbsolute) == filepath.Clean(databaseAbsolute) {
		return errors.New("report path must not be the database path")
	}
	outputDirectory := filepath.Dir(outputAbsolute)
	outputDirectoryInfo, err := os.Stat(outputDirectory)
	if err != nil {
		return fmt.Errorf("inspect report directory: %w", err)
	}
	if !outputDirectoryInfo.IsDir() {
		return errors.New("report parent must be a directory")
	}
	if _, err := os.Lstat(outputAbsolute); err == nil {
		return errors.New("create new JSON report: path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect report path: %w", err)
	}
	resolvedOutputDirectory, err := filepath.EvalSymlinks(outputDirectory)
	if err != nil {
		return fmt.Errorf("resolve report directory: %w", err)
	}
	resolvedDatabase, err := filepath.EvalSymlinks(databaseAbsolute)
	if err != nil {
		return fmt.Errorf("resolve database file: %w", err)
	}
	if filepath.Join(resolvedOutputDirectory, filepath.Base(outputAbsolute)) == resolvedDatabase {
		return errors.New("report path must not resolve to the database path")
	}
	return nil
}
