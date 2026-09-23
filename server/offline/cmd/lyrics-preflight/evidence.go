package main

import (
	"bytes"

	"crypto/sha256"

	"encoding/json"
	"errors"

	"fmt"

	"reflect"

	"strings"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

var defaultPreflightEvidenceCapacityLimits = preflightEvidenceCapacityLimits{
	maxItems:           lyricsstaging.MaxPrivateEvidenceReceiptItems,
	maxPerItemRawBytes: int64(lyricssource.MaxIndexEvidenceRawBytes),
	maxRawBytes:        int64(lyricsstaging.MaxPrivateEvidenceReceiptRawBytes),
	maxEncodedBytes:    int64(lyricsstaging.MaxPrivateEvidenceReceiptBytes),
}

var preflightEvidenceReceiptEncodedOverhead = func() int64 {
	item := lyricssource.IndexEvidence{}
	itemBytes, err := preflightEvidenceEncodedItemBytes(item)
	if err != nil {
		panic(err)
	}
	body, err := json.MarshalIndent(lyricsstaging.PrivateEvidenceReceipt{
		SchemaVersion: lyricsstaging.PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: []lyricssource.IndexEvidence{item},
		ReceiptSHA256: strings.Repeat("0", sha256.Size*2),
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	return int64(len(body)+1) - itemBytes
}()

func newPreflightEvidenceAggregator() *preflightEvidenceAggregator {
	return newPreflightEvidenceAggregatorWithLimits(defaultPreflightEvidenceCapacityLimits)
}

func newPreflightEvidenceAggregatorWithLimits(limits preflightEvidenceCapacityLimits) *preflightEvidenceAggregator {
	return &preflightEvidenceAggregator{
		byID: make(map[string]lyricssource.IndexEvidence), limits: limits, capacityInitialized: true,
	}
}

// add admits one completed result at a time. It checks conflicts before all
// capacity errors, charges only canonical-unique acquisitions, and performs no
// raw clone until the complete batch is known to fit the reviewed receipt.
func (aggregator *preflightEvidenceAggregator) add(evidence []lyricssource.IndexEvidence) error {
	if aggregator == nil {
		return errors.New("preflight evidence aggregator is required")
	}
	if err := aggregator.initializeCapacity(); err != nil {
		return err
	}
	pending := make(map[string]lyricssource.IndexEvidence, len(evidence))
	for _, item := range evidence {
		if existing, found := aggregator.byID[item.EvidenceID]; found {
			if !reflect.DeepEqual(existing, item) {
				return errPreflightEvidenceConflict
			}
			continue
		}
		if existing, found := pending[item.EvidenceID]; found {
			if !reflect.DeepEqual(existing, item) {
				return errPreflightEvidenceConflict
			}
			continue
		}
		pending[item.EvidenceID] = item
	}
	if len(pending) == 0 {
		return nil
	}

	limits := aggregator.capacityLimits()
	nextItems := len(aggregator.byID) + len(pending)
	if nextItems > limits.maxItems {
		return preflightEvidenceCapacityError(nextItems, 0, aggregator.rawBytes, aggregator.encodedReceiptBytes())
	}
	nextRawBytes := aggregator.rawBytes
	nextEncodedEvidenceBytes := aggregator.encodedEvidenceBytes
	for _, item := range pending {
		itemRawBytes := int64(len(item.Raw))
		if itemRawBytes > limits.maxPerItemRawBytes {
			return preflightEvidenceCapacityError(nextItems, itemRawBytes, nextRawBytes, aggregator.encodedReceiptBytes())
		}
		if itemRawBytes > limits.maxRawBytes-nextRawBytes {
			return preflightEvidenceCapacityError(nextItems, itemRawBytes, nextRawBytes+itemRawBytes, aggregator.encodedReceiptBytes())
		}
		nextRawBytes += itemRawBytes
		encodedItemBytes, err := preflightEvidenceEncodedItemBytes(item)
		if err != nil {
			return fmt.Errorf("encode preflight evidence capacity probe: %w", err)
		}
		if encodedItemBytes > limits.maxEncodedBytes-nextEncodedEvidenceBytes {
			return preflightEvidenceCapacityError(nextItems, itemRawBytes, nextRawBytes, limits.maxEncodedBytes+1)
		}
		nextEncodedEvidenceBytes += encodedItemBytes
	}
	nextEncodedBytes := preflightEvidenceEncodedReceiptBytes(nextItems, nextEncodedEvidenceBytes)
	if nextEncodedBytes > limits.maxEncodedBytes {
		return preflightEvidenceCapacityError(nextItems, 0, nextRawBytes, nextEncodedBytes)
	}

	clone := aggregator.cloneEvidenceEnvelope
	if clone == nil {
		clone = clonePreflightIndexEvidence
	}
	for evidenceID, item := range pending {
		aggregator.byID[evidenceID] = clone(item)
	}
	aggregator.rawBytes = nextRawBytes
	aggregator.encodedEvidenceBytes = nextEncodedEvidenceBytes
	return nil
}

func (aggregator *preflightEvidenceAggregator) initializeCapacity() error {
	if aggregator.capacityInitialized {
		if aggregator.byID == nil {
			aggregator.byID = make(map[string]lyricssource.IndexEvidence)
		}
		return nil
	}
	if aggregator.byID == nil {
		aggregator.byID = make(map[string]lyricssource.IndexEvidence)
	}
	limits := aggregator.capacityLimits()
	if len(aggregator.byID) > limits.maxItems {
		return preflightEvidenceCapacityError(len(aggregator.byID), 0, 0, 0)
	}
	rawBytes := int64(0)
	encodedEvidenceBytes := int64(0)
	for _, item := range aggregator.byID {
		itemRawBytes := int64(len(item.Raw))
		if itemRawBytes > limits.maxPerItemRawBytes || itemRawBytes > limits.maxRawBytes-rawBytes {
			return preflightEvidenceCapacityError(len(aggregator.byID), itemRawBytes, rawBytes+itemRawBytes, 0)
		}
		rawBytes += itemRawBytes
		encodedItemBytes, err := preflightEvidenceEncodedItemBytes(item)
		if err != nil {
			return fmt.Errorf("encode retained preflight evidence capacity probe: %w", err)
		}
		if encodedItemBytes > limits.maxEncodedBytes-encodedEvidenceBytes {
			return preflightEvidenceCapacityError(len(aggregator.byID), itemRawBytes, rawBytes, limits.maxEncodedBytes+1)
		}
		encodedEvidenceBytes += encodedItemBytes
	}
	encodedBytes := preflightEvidenceEncodedReceiptBytes(len(aggregator.byID), encodedEvidenceBytes)
	if encodedBytes > limits.maxEncodedBytes {
		return preflightEvidenceCapacityError(len(aggregator.byID), 0, rawBytes, encodedBytes)
	}
	aggregator.rawBytes = rawBytes
	aggregator.encodedEvidenceBytes = encodedEvidenceBytes
	aggregator.capacityInitialized = true
	return nil
}

func (aggregator *preflightEvidenceAggregator) capacityLimits() preflightEvidenceCapacityLimits {
	limits := aggregator.limits
	if limits.maxItems <= 0 {
		limits.maxItems = defaultPreflightEvidenceCapacityLimits.maxItems
	}
	if limits.maxPerItemRawBytes <= 0 {
		limits.maxPerItemRawBytes = defaultPreflightEvidenceCapacityLimits.maxPerItemRawBytes
	}
	if limits.maxRawBytes <= 0 {
		limits.maxRawBytes = defaultPreflightEvidenceCapacityLimits.maxRawBytes
	}
	if limits.maxEncodedBytes <= 0 {
		limits.maxEncodedBytes = defaultPreflightEvidenceCapacityLimits.maxEncodedBytes
	}
	return limits
}

func (aggregator *preflightEvidenceAggregator) encodedReceiptBytes() int64 {
	if aggregator == nil {
		return 0
	}
	return preflightEvidenceEncodedReceiptBytes(len(aggregator.byID), aggregator.encodedEvidenceBytes)
}

func preflightEvidenceEncodedReceiptBytes(itemCount int, encodedEvidenceBytes int64) int64 {
	if itemCount <= 0 {
		return 0
	}
	return preflightEvidenceReceiptEncodedOverhead + encodedEvidenceBytes + int64(2*(itemCount-1))
}

type preflightEvidenceJSONSizeCounter struct {
	bytes    int64
	newlines int64
}

func (counter *preflightEvidenceJSONSizeCounter) Write(data []byte) (int, error) {
	counter.bytes += int64(len(data))
	counter.newlines += int64(bytes.Count(data, []byte{'\n'}))
	return len(data), nil
}

func preflightEvidenceEncodedItemBytes(item lyricssource.IndexEvidence) (int64, error) {
	var counter preflightEvidenceJSONSizeCounter
	encoder := json.NewEncoder(&counter)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(item); err != nil {
		return 0, err
	}
	if counter.bytes <= 0 || counter.newlines <= 0 {
		return 0, errors.New("preflight evidence capacity probe produced no JSON value")
	}
	// Encoder.Encode adds one final newline. The remaining newline count is also
	// the number of item lines that receive the receipt's four-space array indent.
	return counter.bytes - 1 + 4*counter.newlines, nil
}

func preflightEvidenceCapacityError(itemCount int, perItemRawBytes, aggregateRawBytes, encodedBytes int64) error {
	return fmt.Errorf(
		"%w: item count %d (limit %d), per-item raw bytes %d (limit %d), aggregate raw bytes %d (limit %d), encoded bytes %d (limit %d)",
		errPreflightEvidenceCapacity,
		itemCount, lyricsstaging.MaxPrivateEvidenceReceiptItems,
		perItemRawBytes, lyricssource.MaxIndexEvidenceRawBytes,
		aggregateRawBytes, lyricsstaging.MaxPrivateEvidenceReceiptRawBytes,
		encodedBytes, lyricsstaging.MaxPrivateEvidenceReceiptBytes,
	)
}

func (aggregator *preflightEvidenceAggregator) lookup(evidenceID string) (lyricssource.IndexEvidence, bool) {
	if aggregator == nil {
		return lyricssource.IndexEvidence{}, false
	}
	evidence, found := aggregator.byID[evidenceID]
	return evidence, found
}

func (aggregator *preflightEvidenceAggregator) resolve(evidenceID string) (lyricssource.IndexEvidence, bool) {
	evidence, found := aggregator.lookup(evidenceID)
	if !found {
		return lyricssource.IndexEvidence{}, false
	}
	return clonePreflightIndexEvidence(evidence), true
}

func clonePreflightIndexEvidence(evidence lyricssource.IndexEvidence) lyricssource.IndexEvidence {
	evidence.Categories = append([]string(nil), evidence.Categories...)
	evidence.Raw = append([]byte(nil), evidence.Raw...)
	return evidence
}

func hydrateReportItemCandidates(resolver *lyricsstaging.PrivateEvidenceResolver, item reportItem) ([]lyricssource.Candidate, error) {
	identities := append([]candidateSummary{}, item.FixedArtifactCandidates...)
	if len(identities) == 0 && item.Candidate != nil {
		identities = []candidateSummary{*item.Candidate}
	}
	if len(identities) == 0 || resolver == nil {
		return nil, errors.New("private evidence resolver and fixed candidates are required")
	}
	stagingIdentities := make([]lyricsstaging.CandidateIdentity, len(identities))
	for index, identity := range identities {
		stagingIdentities[index] = stagingCandidate(identity)
	}
	return resolver.HydrateCandidates(stagingIdentities)
}

func evidenceReceiptForReport(generated report, available *preflightEvidenceAggregator) (*lyricsstaging.PrivateEvidenceReceipt, error) {
	identities := reportCandidateIdentities(generated)
	if len(identities) == 0 {
		return nil, nil
	}
	if available == nil {
		return nil, errors.New("preflight evidence aggregator is required")
	}
	if err := available.initializeCapacity(); err != nil {
		return nil, err
	}
	used := make(map[string]struct{})
	selected := []lyricssource.IndexEvidence{}
	for _, identity := range identities {
		for _, reference := range identity.IndexEvidenceRefs {
			if _, duplicate := used[reference.EvidenceID]; duplicate {
				continue
			}
			evidence, found := available.lookup(reference.EvidenceID)
			if !found || evidence.SHA256 != reference.SHA256 || evidence.RawSHA256 != reference.SHA256 {
				return nil, errors.New("preflight candidate exact evidence is unavailable")
			}
			used[reference.EvidenceID] = struct{}{}
			selected = append(selected, evidence)
		}
	}

	// Bind every compact candidate to the canonical selected evidence through a
	// borrowed resolver before receipt construction. The receipt constructor then
	// performs the only retained raw clone for the final private report.
	sourceCandidates := make([]lyricssource.Candidate, len(identities))
	for index, identity := range identities {
		sourceCandidates[index] = identity.SourceCandidate()
	}
	if err := lyricssource.ValidateCandidatesAgainstIndexEvidence(selected, sourceCandidates); err != nil {
		return nil, err
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(selected)
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

func reportCandidateIdentities(generated report) []lyricsstaging.CandidateIdentity {
	result := []lyricsstaging.CandidateIdentity{}
	for _, items := range [][]reportItem{
		generated.CatalogReview, generated.GameSizeEvidence, generated.UniqueComplete,
		generated.Ambiguous, generated.Missing, generated.Incomplete, generated.Error,
	} {
		for _, item := range items {
			if len(item.FixedArtifactCandidates) > 0 {
				for _, candidate := range item.FixedArtifactCandidates {
					result = append(result, stagingCandidate(candidate))
				}
				continue
			}
			if item.Candidate != nil {
				result = append(result, stagingCandidate(*item.Candidate))
			}
			for _, candidate := range item.Candidates {
				result = append(result, stagingCandidate(candidate))
			}
		}
	}
	return result
}

func reportSearchDiagnostics(input lyricssource.SearchDiagnostics) *searchDiagnostics {
	if input == (lyricssource.SearchDiagnostics{}) {
		return &searchDiagnostics{}
	}
	return &searchDiagnostics{
		SearchHits: input.SearchHits, Restricted: input.Restricted, RestrictedTitleMatch: input.RestrictedTitleMatch,
		TitleMismatch: input.TitleMismatch, CreditMismatch: input.CreditMismatch,
		LyricistCreditMissing: input.LyricistCreditMissing, LyricistCreditMismatch: input.LyricistCreditMismatch,
		ComposerCreditMissing: input.ComposerCreditMissing, ComposerCreditMismatch: input.ComposerCreditMismatch,
		ArrangerCreditMissing: input.ArrangerCreditMissing, ArrangerCreditMismatch: input.ArrangerCreditMismatch,
		SignalMismatch: input.SignalMismatch, Verified: input.Verified,
	}
}

func missingSearchReason(diagnostics *searchDiagnostics) string {
	if diagnostics == nil {
		return ""
	}
	reason, ok := (lyricssource.SearchDiagnostics{
		SearchHits: diagnostics.SearchHits, Restricted: diagnostics.Restricted,
		RestrictedTitleMatch: diagnostics.RestrictedTitleMatch, TitleMismatch: diagnostics.TitleMismatch,
		CreditMismatch: diagnostics.CreditMismatch, LyricistCreditMissing: diagnostics.LyricistCreditMissing,
		LyricistCreditMismatch: diagnostics.LyricistCreditMismatch, ComposerCreditMissing: diagnostics.ComposerCreditMissing,
		ComposerCreditMismatch: diagnostics.ComposerCreditMismatch, ArrangerCreditMissing: diagnostics.ArrangerCreditMissing,
		ArrangerCreditMismatch: diagnostics.ArrangerCreditMismatch, SignalMismatch: diagnostics.SignalMismatch,
		Verified: diagnostics.Verified,
	}).ZeroCandidateReason()
	if !ok {
		return "malformed_search_diagnostics"
	}
	return string(reason)
}
