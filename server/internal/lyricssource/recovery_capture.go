package lyricssource

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/model"
)

type RecoveryRequestKind string

const (
	RecoveryRequestSearch     RecoveryRequestKind = "search"
	RecoveryRequestRevision   RecoveryRequestKind = "revision"
	RecoveryRequestFixedIndex RecoveryRequestKind = "fixed_index"
)

type RecoveryObservedRevision struct {
	Selector   string
	RevisionID int64
	Timestamp  string
	SHA1       string
}

// RecoveryCapture is the exact ledger-ready projection of one independently
// validated provider HTTP response. It deliberately contains no ledger type so
// lyricssource remains below lyricsacquisition in the dependency graph.
type RecoveryCapture struct {
	Provider               model.LyricsSourceProvider
	CanonicalRequestURL    string
	RequestKind            RecoveryRequestKind
	RevisionSelector       string
	FetchedAt              string
	RawResponse            []byte
	RawResponseSHA256      string
	Evidence               IndexEvidence
	EvidenceEnvelope       []byte
	EvidenceEnvelopeSHA256 string
	ObservedRevisions      []RecoveryObservedRevision
}

// VerifySekaipediaRevisionContent binds revision authority to the immutable
// MediaWiki revision tuple and exact main-slot content. Envelope bytes and their
// digest remain evidence, but page-info fields outside this tuple cannot decide
// revision freshness.
func VerifySekaipediaRevisionContent(raw []byte, expected FixedIndex) error {
	if expected.PageID <= 0 || expected.RevisionID <= 0 || expected.RevisionTimestamp == "" ||
		!HasCanonicalSHA1(expected.SHA1) || !canonicalIndexEvidenceSHA256.MatchString(expected.ContentSHA256) {
		return errors.New("Sekaipedia revision-content authority is invalid")
	}
	page, err := parsePageResponse(raw)
	if err != nil || page.pageID != expected.PageID || page.revisionID != expected.RevisionID ||
		canonicalFetchedAt(page.revisionTimestamp) != expected.RevisionTimestamp || page.sha1 != expected.SHA1 {
		return ErrRevisionChanged
	}
	content := []byte(page.content)
	contentSHA1 := sha1.Sum(content)
	contentSHA256 := sha256.Sum256(content)
	if hex.EncodeToString(contentSHA1[:]) != expected.SHA1 ||
		hex.EncodeToString(contentSHA256[:]) != expected.ContentSHA256 {
		return ErrRevisionChanged
	}
	return nil
}

// CaptureRecoveryHTTPResponse validates one exact provider response and builds
// its canonical evidence envelope before provider parsing can observe the
// bytes. Fixed-index classification is allowed only for an exact plan-derived
// authority supplied by the caller.
func CaptureRecoveryHTTPResponse(
	provider model.LyricsSourceProvider,
	authorities []FixedIndex,
	response RecoveryHTTPResponse,
) (RecoveryCapture, error) {
	if !model.IsValidLyricsSourceProvider(provider) || response.FetchedAt.IsZero() ||
		response.FetchedAt.Location() != time.UTC || len(response.Raw) == 0 ||
		len(response.Raw) > MaxIndexEvidenceRawBytes {
		return RecoveryCapture{}, errors.New("recovery HTTP response boundary is invalid")
	}
	requestURL, query, err := validateRecoveryRequestURL(provider, response.CanonicalRequestURL)
	if err != nil {
		return RecoveryCapture{}, err
	}
	if response.Action != "search" && response.Action != "creator-alias" && response.Action != "page" {
		return RecoveryCapture{}, errors.New("recovery HTTP response action is unsupported")
	}

	capture := RecoveryCapture{
		Provider: provider, CanonicalRequestURL: requestURL,
		FetchedAt:   canonicalFetchedAt(response.FetchedAt),
		RawResponse: append([]byte(nil), response.Raw...),
	}
	rawDigest := sha256.Sum256(capture.RawResponse)
	capture.RawResponseSHA256 = hex.EncodeToString(rawDigest[:])

	if query.Get("generator") == "search" {
		if provider != ProviderVocaloidFandom || response.Action == "page" {
			return RecoveryCapture{}, errors.New("recovery search response provider or action is invalid")
		}
		pages, parseErr := parseSearchResponse(response.Raw)
		if parseErr != nil {
			return RecoveryCapture{}, parseErr
		}
		evidence, evidenceErr := newFandomSearchIndexEvidence(requestURL, response.FetchedAt, response.Raw)
		if evidenceErr != nil {
			return RecoveryCapture{}, evidenceErr
		}
		capture.RequestKind = RecoveryRequestSearch
		capture.Evidence = evidence
		capture.ObservedRevisions = recoveryObservedSearchRevisions(pages)
	} else {
		if response.Action != "page" {
			return RecoveryCapture{}, errors.New("recovery revision response action is invalid")
		}
		page, parseErr := parseAcquiredPageResponse(response.Raw, response.FetchedAt)
		if parseErr != nil {
			return RecoveryCapture{}, parseErr
		}
		baseID, fixed, baseErr := recoveryRevisionBaseID(provider, authorities, page)
		if baseErr != nil {
			return RecoveryCapture{}, baseErr
		}
		projection := []byte(page.content)
		if provider == ProviderSekaipedia {
			projection = response.Raw
		}
		evidence, evidenceErr := newMediaWikiRevisionIndexEvidence(provider, baseID, page, projection)
		if evidenceErr != nil {
			return RecoveryCapture{}, evidenceErr
		}
		capture.RequestKind = RecoveryRequestRevision
		if fixed {
			capture.RequestKind = RecoveryRequestFixedIndex
		}
		capture.RevisionSelector = "oldid:" + strconv.Itoa(page.revisionID)
		capture.Evidence = evidence
		capture.ObservedRevisions = []RecoveryObservedRevision{recoveryObservedRevision(page)}
	}

	capture.EvidenceEnvelope, err = json.Marshal(capture.Evidence)
	if err != nil {
		return RecoveryCapture{}, fmt.Errorf("encode recovery evidence envelope: %w", err)
	}
	envelopeDigest := sha256.Sum256(capture.EvidenceEnvelope)
	capture.EvidenceEnvelopeSHA256 = hex.EncodeToString(envelopeDigest[:])
	return cloneRecoveryCapture(capture), nil
}

func validateRecoveryRequestURL(
	provider model.LyricsSourceProvider,
	value string,
) (string, url.Values, error) {
	endpoint, ok := recoveryCanonicalEndpoint(provider)
	if !ok {
		return "", nil, errors.New("recovery request provider is unsupported")
	}
	parsed, err := url.Parse(value)
	endpointURL, endpointErr := url.Parse(endpoint)
	if err != nil || endpointErr != nil || parsed == nil || endpointURL == nil || parsed.Scheme != "https" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || parsed.ForceQuery ||
		parsed.Scheme != endpointURL.Scheme || !strings.EqualFold(parsed.Host, endpointURL.Host) ||
		parsed.EscapedPath() != endpointURL.EscapedPath() {
		return "", nil, errors.New("recovery request URL is outside the provider-owned endpoint")
	}
	query := parsed.Query()
	if parsed.RawQuery == "" || parsed.RawQuery != query.Encode() || query.Get("action") != "query" ||
		query.Get("format") != "json" || query.Get("maxlag") != mediaWikiMaxLag {
		return "", nil, errors.New("recovery request URL is not canonical MediaWiki maxlag=5 JSON")
	}
	return parsed.String(), query, nil
}

func recoveryRevisionBaseID(
	provider model.LyricsSourceProvider,
	authorities []FixedIndex,
	page wikiPage,
) (string, bool, error) {
	matches := 0
	var matched FixedIndex
	for _, authority := range authorities {
		if recoveryAuthorityMatchesPage(provider, authority, page) {
			matches++
			matched = authority
		}
	}
	if matches > 1 {
		return "", false, errors.New("recovery response matches multiple configured authorities")
	}
	if matches == 1 {
		switch provider {
		case ProviderSekaipedia:
			return sekaipediaAuthorityEvidenceID(matched), true, nil
		case ProviderMoegirl:
			return fmt.Sprintf("search:moegirl:%d", matched.PageID), true, nil
		default:
			return "", false, errors.New("recovery provider does not support fixed authorities")
		}
	}
	switch provider {
	case ProviderSekaipedia:
		return sekaipediaSongEvidenceID(page.pageID, page.revisionID), false, nil
	case ProviderMoegirl:
		return fmt.Sprintf("search:moegirl:%d", page.pageID), false, nil
	case ProviderVocaloidFandom:
		return fmt.Sprintf("fetch:vocaloid-fandom:%d", page.pageID), false, nil
	default:
		return "", false, errors.New("recovery revision provider is unsupported")
	}
}

func recoveryAuthorityMatchesPage(provider model.LyricsSourceProvider, authority FixedIndex, page wikiPage) bool {
	if authority.PageID != page.pageID || authority.RevisionID != page.revisionID || authority.SHA1 != page.sha1 {
		return false
	}
	content := []byte(page.content)
	contentSHA1 := sha1.Sum(content)
	if authority.SHA1 != hex.EncodeToString(contentSHA1[:]) {
		return false
	}
	switch provider {
	case ProviderSekaipedia:
		contentSHA256 := sha256.Sum256(content)
		return sekaipediaAuthorityEvidenceID(authority) != "" &&
			authority.RevisionTimestamp == canonicalFetchedAt(page.revisionTimestamp) &&
			authority.ContentSHA256 == hex.EncodeToString(contentSHA256[:])
	case ProviderMoegirl:
		return authority.RevisionTimestamp == "" && authority.ContentSHA256 == "" && authority.RawSHA256 == ""
	default:
		return false
	}
}

func recoveryObservedSearchRevisions(pages []wikiPage) []RecoveryObservedRevision {
	observed := make([]RecoveryObservedRevision, len(pages))
	for index, page := range pages {
		observed[index] = recoveryObservedRevision(page)
		observed[index].Selector = fmt.Sprintf("pageid:%d", page.pageID)
	}
	sort.Slice(observed, func(left, right int) bool {
		if observed[left].Selector != observed[right].Selector {
			return observed[left].Selector < observed[right].Selector
		}
		return observed[left].RevisionID < observed[right].RevisionID
	})
	return observed
}

func recoveryObservedRevision(page wikiPage) RecoveryObservedRevision {
	result := RecoveryObservedRevision{
		Selector:   "oldid:" + strconv.Itoa(page.revisionID),
		RevisionID: int64(page.revisionID), SHA1: page.sha1,
	}
	if !page.revisionTimestamp.IsZero() {
		result.Timestamp = canonicalFetchedAt(page.revisionTimestamp)
	}
	return result
}

func cloneRecoveryCapture(capture RecoveryCapture) RecoveryCapture {
	capture.RawResponse = append([]byte(nil), capture.RawResponse...)
	if capture.Evidence.Categories == nil {
		capture.Evidence.Categories = nil
	} else {
		capture.Evidence.Categories = append([]string{}, capture.Evidence.Categories...)
	}
	capture.Evidence.Raw = append([]byte(nil), capture.Evidence.Raw...)
	capture.EvidenceEnvelope = append([]byte(nil), capture.EvidenceEnvelope...)
	capture.ObservedRevisions = append([]RecoveryObservedRevision(nil), capture.ObservedRevisions...)
	return capture
}
