package main

import (
	"context"

	"errors"

	"fmt"

	"net"
	"net/url"

	"sort"
	"strings"

	"time"

	"moesekai/server/internal/lyricssource"

	"moesekai/server/internal/model"

	_ "modernc.org/sqlite"
)

func retryOperation[T any](ctx context.Context, opts options, operation func(context.Context) (T, error)) attemptResult[T] {
	var result attemptResult[T]
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
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
		result.attempts = attempt
		switch {
		case attemptErr != nil:
			var zero T
			result.value = zero
			result.err = attemptErr
		case parentErr != nil:
			var zero T
			result.value = zero
			result.err = parentErr
		default:
			result.value = value
			result.err = operationErr
		}
		if result.err == nil || !retryableSourceError(result.err) || attempt == opts.MaxAttempts {
			return result
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
			result.err = ctx.Err()
			return result
		case <-timer.C:
		}
	}
	return result
}

func retryDelay(base time.Duration, completedAttempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base
	for step := 1; step < completedAttempt; step++ {
		if delay >= 30*time.Second/2 {
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
		errors.Is(err, lyricssource.ErrMissingLyrics), errors.Is(err, lyricssource.ErrLyricsUnpublished),
		errors.Is(err, lyricssource.ErrRevisionChanged), errors.Is(err, lyricssource.ErrUnsupportedTable),
		errors.Is(err, lyricssource.ErrLyricsTooLarge), errors.Is(err, lyricssource.ErrMalformedResponse):
		return false
	}
	var httpError *lyricssource.HTTPError
	if errors.As(err, &httpError) {
		return httpError.StatusCode == 429 || httpError.StatusCode >= 500
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return true
	}
	return true
}

func incompleteSourceError(err error) bool {
	return errors.Is(err, lyricssource.ErrRestrictedReprint) || errors.Is(err, lyricssource.ErrAmbiguous) ||
		errors.Is(err, lyricssource.ErrMissingLyrics) || errors.Is(err, lyricssource.ErrLyricsUnpublished) ||
		errors.Is(err, lyricssource.ErrRevisionChanged) || errors.Is(err, lyricssource.ErrUnsupportedTable) ||
		errors.Is(err, lyricssource.ErrLyricsTooLarge)
}

func safeErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, lyricssource.ErrRestrictedReprint):
		return "restricted_reprint"
	case errors.Is(err, lyricssource.ErrAmbiguous):
		return "ambiguous_source"
	case errors.Is(err, lyricssource.ErrMissingLyrics):
		return "missing_lyrics"
	case errors.Is(err, lyricssource.ErrLyricsUnpublished):
		return "lyrics_unpublished"
	case errors.Is(err, lyricssource.ErrRevisionChanged):
		return "source_drift"
	case errors.Is(err, lyricssource.ErrUnsupportedTable):
		return "unsupported_format"
	case errors.Is(err, lyricssource.ErrLyricsTooLarge):
		return "source_too_large"
	case errors.Is(err, lyricssource.ErrMalformedResponse):
		return "malformed_response"
	}
	var httpError *lyricssource.HTTPError
	if errors.As(err, &httpError) {
		if httpError.StatusCode == 429 {
			return "rate_limited"
		}
		if httpError.StatusCode >= 500 {
			return "source_unavailable"
		}
		return "source_http_error"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "timeout"
		}
		return "source_unavailable"
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return "source_unavailable"
	}
	return "source_unavailable"
}

func sourceIdentity(item catalogItem) lyricssource.MusicIdentity {
	return lyricssource.MusicIdentity{
		MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, ProducerMetadata: item.ProducerMetadata,
		Lyricist: item.Lyricist, Composer: item.Composer, Arranger: item.Arranger,
		PerformerSegmentationPolicy: lyricssource.PerformerSegmentationPolicyFromCatalogVocals(item.Evidence.Vocals),
		Instrumental:                model.CatalogVocalSignalsAreInstrumental(item.Evidence.Vocals),
	}
}

func baseReportItem(item catalogItem, target model.CatalogLyricsTarget) reportItem {
	associations := append([]int(nil), target.AssociationMusicIDs...)
	if associations == nil {
		associations = []int{}
	}
	return reportItem{
		MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, CatalogFingerprint: item.CatalogFingerprint,
		TargetMusicID: target.TargetMusicID, AssociationMusicIDs: associations, ReasonCode: target.ReasonCode,
	}
}

func summarizeCandidate(candidate lyricssource.Candidate) (candidateSummary, error) {
	if !model.IsValidLyricsSourceProvider(candidate.Provider) || candidate.PageID <= 0 || candidate.RevisionID <= 0 ||
		!lyricssource.HasCanonicalSHA1(candidate.SHA1) || candidate.Title == "" || candidate.Title != strings.TrimSpace(candidate.Title) ||
		len(candidate.Title) > maxCandidateTitle || strings.ContainsAny(candidate.Title, "\r\n") || candidate.CanonicalURL == "" ||
		candidate.CanonicalURL != strings.TrimSpace(candidate.CanonicalURL) || len(candidate.CanonicalURL) > maxCandidateURL ||
		candidate.Section == "" || candidate.Section != strings.TrimSpace(candidate.Section) || len(candidate.Section) > 512 ||
		!validRenditionKey(candidate.RenditionKey) ||
		!model.IsValidLyricsSourceCandidateVersionReasonCode(candidate.VersionReason) || len(candidate.IndexEvidenceRefs) == 0 ||
		len(candidate.IndexEvidenceRefs) > 64 {
		return candidateSummary{}, errors.New("invalid provider-aware candidate identity")
	}
	wantOrigin := model.LyricsSourceOriginVocaloidFandom
	switch candidate.Provider {
	case model.LyricsSourceProviderMoegirl:
		wantOrigin = model.LyricsSourceOriginMoegirl
	case model.LyricsSourceProviderSekaipedia:
		wantOrigin = model.LyricsSourceOriginSekaipedia
	}
	if candidate.Origin != wantOrigin || !canonicalCandidateRevisionURL(candidate) ||
		(candidate.Provider == model.LyricsSourceProviderSekaipedia && candidate.RevisionTimestamp == "") ||
		(candidate.RevisionTimestamp != "" && !canonicalCandidateRevisionTimestamp(candidate.RevisionTimestamp)) {
		return candidateSummary{}, errors.New("invalid provider-aware candidate URL or revision timestamp")
	}
	categories, err := canonicalCandidateCategories(candidate.Categories)
	if err != nil || !equalStrings(categories, candidate.Categories) {
		return candidateSummary{}, errors.New("candidate categories are not canonical")
	}
	references := append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
	seenEvidence := make(map[string]struct{}, len(references))
	for _, reference := range references {
		if !validCompactIdentifier(reference.EvidenceID, 256, "._:/-") || !canonicalHex(reference.SHA256, 64) {
			return candidateSummary{}, errors.New("invalid candidate index evidence")
		}
		if _, exists := seenEvidence[reference.EvidenceID]; exists {
			return candidateSummary{}, errors.New("duplicate candidate index evidence")
		}
		seenEvidence[reference.EvidenceID] = struct{}{}
	}
	return candidateSummary{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
		RevisionTimestamp: candidate.RevisionTimestamp,
		SHA1:              candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL, Categories: categories,
		Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: references,
	}, nil
}

func canonicalCandidateRevisionURL(candidate lyricssource.Candidate) bool {
	parsed, err := url.Parse(candidate.CanonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.ForceQuery {
		return false
	}
	switch candidate.Provider {
	case model.LyricsSourceProviderVocaloidFandom:
		if parsed.Host != "vocaloid.fandom.com" || !strings.HasPrefix(parsed.EscapedPath(), "/wiki/") || parsed.EscapedPath() == "/wiki/" {
			return false
		}
		query := parsed.Query()
		return len(query) == 1 && len(query["oldid"]) == 1 && query.Get("oldid") == fmt.Sprintf("%d", candidate.RevisionID) &&
			parsed.RawQuery == query.Encode()
	case model.LyricsSourceProviderMoegirl:
		canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
		query := canonical.Query()
		query.Set("oldid", fmt.Sprintf("%d", candidate.RevisionID))
		query.Set("title", candidate.Title)
		canonical.RawQuery = query.Encode()
		return candidate.CanonicalURL == canonical.String()
	case model.LyricsSourceProviderSekaipedia:
		if parsed.Host != "www.sekaipedia.org" || !strings.HasPrefix(parsed.EscapedPath(), "/wiki/") ||
			parsed.EscapedPath() == "/wiki/" {
			return false
		}
		query := parsed.Query()
		return len(query) == 1 && len(query["oldid"]) == 1 &&
			query.Get("oldid") == fmt.Sprintf("%d", candidate.RevisionID) && parsed.RawQuery == query.Encode()
	default:
		return false
	}
}

func canonicalCandidateRevisionTimestamp(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && value != "" && strings.HasSuffix(value, "Z") && parsed.Unix() > 0 &&
		parsed.UTC().Format(time.RFC3339Nano) == value
}

func validRenditionKey(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || index > 0 && strings.ContainsRune("._-", char) {
			continue
		}
		return false
	}
	return true
}

func validCompactIdentifier(value string, maximum int, punctuation string) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for index, char := range value {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' ||
			index > 0 && strings.ContainsRune(punctuation, char) {
			continue
		}
		return false
	}
	return true
}

func canonicalHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}

func equalIndexEvidenceRefs(left, right []model.LyricsSourceIndexEvidenceRef) bool {
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

func equalStrings(left, right []string) bool {
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

func canonicalCandidateCategories(input []string) ([]string, error) {
	categories := make([]string, 0, len(input))
	for _, category := range input {
		category = strings.TrimSpace(category)
		if category == "" || len(category) > maxCandidateCategory || strings.ContainsAny(category, "\r\n") {
			return nil, errors.New("invalid candidate category")
		}
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for index := 1; index < len(categories); index++ {
		if categories[index] == categories[index-1] {
			return nil, errors.New("duplicate candidate category")
		}
	}
	if categories == nil {
		categories = []string{}
	}
	return categories, nil
}

func equalCanonicalCategories(left, right []string) bool {
	leftCategories, leftErr := canonicalCandidateCategories(left)
	rightCategories, rightErr := canonicalCandidateCategories(right)
	if leftErr != nil || rightErr != nil || len(leftCategories) != len(rightCategories) {
		return false
	}
	for index := range leftCategories {
		if leftCategories[index] != rightCategories[index] {
			return false
		}
	}
	return true
}
