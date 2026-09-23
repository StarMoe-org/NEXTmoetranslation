package main

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"
)

func TestPreflightEvidenceAggregationReusesExactDuplicate(t *testing.T) {
	candidate := candidateFor(41, "one")
	aggregator := newPreflightEvidenceAggregator()
	if err := aggregator.add(candidate.IndexEvidence); err != nil {
		t.Fatal(err)
	}
	duplicateClones := 0
	aggregator.cloneEvidenceEnvelope = func(evidence lyricssource.IndexEvidence) lyricssource.IndexEvidence {
		duplicateClones++
		return clonePreflightIndexEvidence(evidence)
	}
	exactDuplicate := clonePreflightIndexEvidence(candidate.IndexEvidence[0])
	if err := aggregator.add([]lyricssource.IndexEvidence{exactDuplicate}); err != nil {
		t.Fatalf("exact duplicate evidence was rejected: %v", err)
	}
	if len(aggregator.byID) != 1 || duplicateClones != 0 {
		t.Fatalf("exact duplicate evidence produced entries=%d clones=%d, want 1 entry and no clone", len(aggregator.byID), duplicateClones)
	}

	summary, err := summarizeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	generated := report{UniqueComplete: []reportItem{{Candidate: &summary}}}
	receipt, err := evidenceReceiptForReport(generated, aggregator)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || len(receipt.IndexEvidence) != 1 {
		t.Fatalf("exact duplicate receipt=%+v", receipt)
	}
	hydrated, err := receipt.HydrateCandidate(stagingCandidate(summary))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hydrated, candidate) {
		t.Fatalf("hydrated exact candidate=%+v, want %+v", hydrated, candidate)
	}
}

func TestPreflightEvidenceAggregationAcceptsIndependentRevisionReacquisitions(t *testing.T) {
	candidate := candidateFor(42, "one")
	if err := lyricssource.ValidateCandidateIndexEvidence(candidate); err != nil {
		t.Fatal(err)
	}
	laterEvidence := clonePreflightIndexEvidence(candidate.IndexEvidence[0])
	laterEvidence.FetchedAt = time.Unix(101, 0).UTC().Format(time.RFC3339Nano)
	laterEvidence.EvidenceID = lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		laterEvidence.Provider,
		"fetch:vocaloid-fandom:"+strconv.Itoa(laterEvidence.PageID),
		laterEvidence.FetchedAt,
		laterEvidence.RawSHA256,
	)
	laterCandidate := candidate
	laterCandidate.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
	laterCandidate.IndexEvidenceRefs[0].EvidenceID = laterEvidence.EvidenceID
	laterCandidate.IndexEvidence = []lyricssource.IndexEvidence{laterEvidence}
	if err := lyricssource.ValidateCandidateIndexEvidence(laterCandidate); err != nil {
		t.Fatalf("independent reacquisition is invalid: %v", err)
	}

	aggregator := newPreflightEvidenceAggregator()
	if err := aggregator.add(candidate.IndexEvidence); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.add(laterCandidate.IndexEvidence); err != nil {
		t.Fatalf("independent reacquisition conflicted: %v", err)
	}
	if len(aggregator.byID) != 2 || candidate.IndexEvidence[0].EvidenceID == laterEvidence.EvidenceID {
		t.Fatalf("reacquisition aggregate=%+v", aggregator.byID)
	}
}

func TestPreflightEvidenceAggregationRejectsUnvalidatedSameIDEnvelopeConflict(t *testing.T) {
	candidate := candidateFor(45, "one")
	conflictingEvidence := clonePreflightIndexEvidence(candidate.IndexEvidence[0])
	conflictingEvidence.FetchedAt = time.Unix(101, 0).UTC().Format(time.RFC3339Nano)
	conflictingCandidate := candidate
	conflictingCandidate.IndexEvidence = []lyricssource.IndexEvidence{conflictingEvidence}
	if err := lyricssource.ValidateCandidateIndexEvidence(conflictingCandidate); err == nil {
		t.Fatal("same-ID envelope drift passed strict candidate validation")
	}

	aggregator := newPreflightEvidenceAggregator()
	if err := aggregator.add(candidate.IndexEvidence); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.add(conflictingCandidate.IndexEvidence); !errors.Is(err, errPreflightEvidenceConflict) {
		t.Fatalf("same-ID/different-envelope conflict error=%v", err)
	}
	if len(aggregator.byID) != 1 || !reflect.DeepEqual(aggregator.byID[candidate.IndexEvidence[0].EvidenceID], candidate.IndexEvidence[0]) {
		t.Fatalf("conflicting arrival changed the accepted aggregate: %+v", aggregator.byID)
	}
}

func TestPreflightEvidenceAggregationEnforcesIncrementalCapacityBoundaries(t *testing.T) {
	first := candidateFor(46, "capacity-first").IndexEvidence[0]
	second := candidateFor(47, "capacity-second").IndexEvidence[0]
	third := candidateFor(49, "capacity-third").IndexEvidence[0]

	itemLimits := defaultPreflightEvidenceCapacityLimits
	itemLimits.maxItems = 2
	itemAggregator := newPreflightEvidenceAggregatorWithLimits(itemLimits)
	if err := itemAggregator.add([]lyricssource.IndexEvidence{first, second}); err != nil {
		t.Fatalf("exact item boundary was rejected: %v", err)
	}
	if err := itemAggregator.add([]lyricssource.IndexEvidence{third}); !errors.Is(err, errPreflightEvidenceCapacity) {
		t.Fatalf("item overflow error=%v", err)
	}
	if len(itemAggregator.byID) != 2 {
		t.Fatalf("item overflow changed retained evidence: %d", len(itemAggregator.byID))
	}

	perItemLimits := defaultPreflightEvidenceCapacityLimits
	perItemLimits.maxPerItemRawBytes = int64(len(first.Raw))
	if err := newPreflightEvidenceAggregatorWithLimits(perItemLimits).add([]lyricssource.IndexEvidence{first}); err != nil {
		t.Fatalf("exact per-item raw boundary was rejected: %v", err)
	}
	perItemLimits.maxPerItemRawBytes--
	if err := newPreflightEvidenceAggregatorWithLimits(perItemLimits).add([]lyricssource.IndexEvidence{first}); !errors.Is(err, errPreflightEvidenceCapacity) {
		t.Fatalf("per-item raw overflow error=%v", err)
	}

	rawLimits := defaultPreflightEvidenceCapacityLimits
	rawLimits.maxRawBytes = int64(len(first.Raw) + len(second.Raw))
	rawAggregator := newPreflightEvidenceAggregatorWithLimits(rawLimits)
	if err := rawAggregator.add([]lyricssource.IndexEvidence{first}); err != nil {
		t.Fatal(err)
	}
	if err := rawAggregator.add([]lyricssource.IndexEvidence{second}); err != nil {
		t.Fatalf("exact aggregate raw boundary was rejected: %v", err)
	}
	rawLimits.maxRawBytes--
	rawOverflow := newPreflightEvidenceAggregatorWithLimits(rawLimits)
	if err := rawOverflow.add([]lyricssource.IndexEvidence{first}); err != nil {
		t.Fatal(err)
	}
	if err := rawOverflow.add([]lyricssource.IndexEvidence{second}); !errors.Is(err, errPreflightEvidenceCapacity) {
		t.Fatalf("aggregate raw overflow error=%v", err)
	}
	if len(rawOverflow.byID) != 1 {
		t.Fatalf("aggregate raw overflow changed retained evidence: %d", len(rawOverflow.byID))
	}

	firstEncoded, err := preflightEvidenceEncodedItemBytes(first)
	if err != nil {
		t.Fatal(err)
	}
	secondEncoded, err := preflightEvidenceEncodedItemBytes(second)
	if err != nil {
		t.Fatal(err)
	}
	exactEncoded := preflightEvidenceEncodedReceiptBytes(2, firstEncoded+secondEncoded)
	encodedLimits := defaultPreflightEvidenceCapacityLimits
	encodedLimits.maxEncodedBytes = exactEncoded
	encodedAggregator := newPreflightEvidenceAggregatorWithLimits(encodedLimits)
	if err := encodedAggregator.add([]lyricssource.IndexEvidence{first, second}); err != nil {
		t.Fatalf("exact encoded boundary was rejected: %v", err)
	}
	encodedLimits.maxEncodedBytes--
	encodedOverflow := newPreflightEvidenceAggregatorWithLimits(encodedLimits)
	clones := 0
	encodedOverflow.cloneEvidenceEnvelope = func(evidence lyricssource.IndexEvidence) lyricssource.IndexEvidence {
		clones++
		return clonePreflightIndexEvidence(evidence)
	}
	if err := encodedOverflow.add([]lyricssource.IndexEvidence{first, second}); !errors.Is(err, errPreflightEvidenceCapacity) {
		t.Fatalf("encoded overflow error=%v", err)
	}
	if len(encodedOverflow.byID) != 0 || clones != 0 {
		t.Fatalf("encoded overflow retained=%d clones=%d", len(encodedOverflow.byID), clones)
	}
}

func TestPreflightEvidenceReceiptStillRejectsCandidateEvidenceIdentityDrift(t *testing.T) {
	candidate := candidateFor(52, "identity-drift")
	summary, err := summarizeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	summary.Provider = lyricssource.ProviderMoegirl
	summary.Origin = lyricssource.OriginMoegirl
	aggregator := newPreflightEvidenceAggregator()
	if err := aggregator.add(candidate.IndexEvidence); err != nil {
		t.Fatal(err)
	}
	generated := report{UniqueComplete: []reportItem{{Candidate: &summary}}}
	if receipt, err := evidenceReceiptForReport(generated, aggregator); err == nil || receipt != nil {
		t.Fatalf("identity-drift receipt=%+v err=%v", receipt, err)
	}
}

func TestPreflightEvidenceAggregationEncodedAccountingMatchesCanonicalReceipt(t *testing.T) {
	evidence := []lyricssource.IndexEvidence{
		candidateFor(50, "encoded-<first>").IndexEvidence[0],
		candidateFor(51, "encoded-第二").IndexEvidence[0],
	}
	aggregator := newPreflightEvidenceAggregator()
	if err := aggregator.add(evidence); err != nil {
		t.Fatal(err)
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(evidence)
	if err != nil {
		t.Fatal(err)
	}
	body, err := lyricsstaging.MarshalPrivateEvidenceReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := aggregator.encodedReceiptBytes(), int64(len(body)); got != want {
		t.Fatalf("incremental encoded accounting=%d, canonical receipt=%d", got, want)
	}
}

func TestPreflightAcceptsSelectedJapaneseSekaipediaEvidenceAndPreservesRevisionTimestamp(t *testing.T) {
	candidate := candidateFor(48, "one")
	candidate.Provider = lyricssource.ProviderSekaipedia
	candidate.Origin = lyricssource.OriginSekaipedia
	candidate.CanonicalURL = strings.Replace(candidate.CanonicalURL, "vocaloid.fandom.com", "www.sekaipedia.org", 1)
	candidate.RevisionTimestamp = time.Unix(150, 0).UTC().Format(time.RFC3339Nano)
	candidate.RawSHA256 = strings.Repeat("a", 64)

	summary, err := summarizeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if summary.RevisionTimestamp != candidate.RevisionTimestamp ||
		summary.sourceCandidate().RevisionTimestamp != candidate.RevisionTimestamp ||
		stagingCandidate(summary).RevisionTimestamp != candidate.RevisionTimestamp {
		t.Fatalf("Sekaipedia revision timestamp was not preserved: summary=%+v", summary)
	}

	fixed := fixedFor(candidate, "固定歌詞")
	fixed.Wikitext = lyricssource.SekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)
	fixed.RevisionTimestamp = time.Unix(150, 0).UTC()
	fixed.RawSHA256 = candidate.RawSHA256
	if code := fixedRevisionErrorCode(candidate, fixed); code != "" {
		t.Fatalf("selected-Japanese Sekaipedia fixed revision code = %q", code)
	}
	withoutWikitext := fixed
	withoutWikitext.Wikitext = nil
	if code := fixedRevisionErrorCode(candidate, withoutWikitext); code != "source_identity_drift" {
		t.Fatalf("wikitextless Sekaipedia boundary code = %q", code)
	}
	withRomanization := fixed
	withRomanization.Wikitext = []byte("romaji must remain transient")
	if code := fixedRevisionErrorCode(candidate, withRomanization); code != "source_identity_drift" {
		t.Fatalf("Sekaipedia romanization boundary code = %q", code)
	}
	driftedTimestamp := fixed
	driftedTimestamp.RevisionTimestamp = driftedTimestamp.RevisionTimestamp.Add(time.Second)
	if code := fixedRevisionErrorCode(candidate, driftedTimestamp); code != "source_identity_drift" {
		t.Fatalf("Sekaipedia revision timestamp drift code = %q", code)
	}
}

func TestPreflightEvidenceAggregationReceiptDeterminism(t *testing.T) {
	first := candidateFor(43, "one")
	second := candidateFor(44, "one")
	firstSummary, err := summarizeCandidate(first)
	if err != nil {
		t.Fatal(err)
	}
	secondSummary, err := summarizeCandidate(second)
	if err != nil {
		t.Fatal(err)
	}
	generated := report{Ambiguous: []reportItem{{Candidates: []candidateSummary{firstSummary, secondSummary}}}}

	forward := newPreflightEvidenceAggregator()
	if err := forward.add(append(candidateEvidence([]lyricssource.Candidate{first}), candidateEvidence([]lyricssource.Candidate{second})...)); err != nil {
		t.Fatal(err)
	}
	reverse := newPreflightEvidenceAggregator()
	if err := reverse.add(append(candidateEvidence([]lyricssource.Candidate{second}), candidateEvidence([]lyricssource.Candidate{first})...)); err != nil {
		t.Fatal(err)
	}

	forwardReceipt, err := evidenceReceiptForReport(generated, forward)
	if err != nil {
		t.Fatal(err)
	}
	reverseReceipt, err := evidenceReceiptForReport(generated, reverse)
	if err != nil {
		t.Fatal(err)
	}
	if forwardReceipt == nil || reverseReceipt == nil || !reflect.DeepEqual(*forwardReceipt, *reverseReceipt) {
		t.Fatalf("receipt changed with aggregation arrival order:\nforward=%+v\nreverse=%+v", forwardReceipt, reverseReceipt)
	}
	for _, summary := range []candidateSummary{firstSummary, secondSummary} {
		forwardCandidate, forwardErr := forwardReceipt.HydrateCandidate(stagingCandidate(summary))
		reverseCandidate, reverseErr := reverseReceipt.HydrateCandidate(stagingCandidate(summary))
		if forwardErr != nil || reverseErr != nil || !reflect.DeepEqual(forwardCandidate, reverseCandidate) {
			t.Fatalf("deterministic receipt hydration for %+v: forward=%+v err=%v reverse=%+v err=%v",
				summary, forwardCandidate, forwardErr, reverseCandidate, reverseErr)
		}
	}
}
