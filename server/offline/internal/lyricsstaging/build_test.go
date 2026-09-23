package lyricsstaging

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func validPreflightAndFixed(t *testing.T) (PreflightReport, lyricscontract.CatalogIdentity, lyricssource.FixedRevision) {
	t.Helper()
	wikitext := []byte("== Lyrics ==\n初音歌う")
	wikitextDigest := sha1.Sum(wikitext)
	candidate := CandidateIdentity{
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: 12, RevisionID: 34, SHA1: hex.EncodeToString(wikitextDigest[:]), Title: "合成試験曲",
		CanonicalURL: "https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34",
		Categories:   []string{"Lyrics", "Songs"}, Section: "Lyrics/Project SEKAI Version", RenditionKey: "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "search:vocaloid-fandom:12", SHA256: strings.Repeat("a", 64),
		}},
	}
	item := PreflightItem{
		MusicID: 10, JapaneseTitle: "合成試験曲", CatalogFingerprint: strings.Repeat("b", 64), TargetMusicID: 10,
		AssociationMusicIDs: []int{}, Candidate: &candidate, LineCount: 1, SearchAttempts: 1, FetchAttempts: 1,
	}
	report := PreflightReport{
		SchemaVersion: PreflightSchemaVersion, GeneratedAt: time.Unix(123, 0).UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: CatalogSchemaVersion, CatalogCount: 1,
		Summary:       PreflightSummary{UniqueComplete: 1},
		CatalogReview: []PreflightItem{}, GameSizeEvidence: []PreflightItem{}, UniqueComplete: []PreflightItem{item},
		Ambiguous: []PreflightItem{}, Missing: []PreflightItem{}, Incomplete: []PreflightItem{}, Error: []PreflightItem{},
	}
	identity := lyricscontract.CatalogIdentity{
		MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, CatalogFingerprint: item.CatalogFingerprint,
		ProducerMetadata: "制作者", Lyricist: "制作者", Composer: "制作者", Arranger: "制作者",
		Vocals: []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai"}},
	}
	fixed := lyricssource.FixedRevision{
		Provider: candidate.Provider, Origin: candidate.Origin,
		PageID: candidate.PageID, RevisionID: candidate.RevisionID, SHA1: candidate.SHA1, PageTitle: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
		FetchedAt:         time.Unix(456, 0).UTC(), Wikitext: append([]byte{}, wikitext...),
		Lines: []lyricssource.ExtractedLine{{Japanese: "初音歌う"}},
		Extraction: lyricssource.Extraction{
			Version:              lyricssource.LyricsVersion{Kind: "sekai", Label: "Project SEKAI Version"},
			Performers:           []lyricssource.Performer{{PerformerID: "miku", Name: "Miku", Color: "#33CCBB"}},
			RubyGeneratorVersion: "kagome-ipadic-v1",
			Lines: []lyricssource.StructuredLine{{
				Japanese: "初音歌う", Segments: []lyricssource.LyricsSegment{{
					Text: "初音", PerformerIDs: []string{"miku"},
					Ruby: []lyricssource.RubySpan{{Text: "初音", Reading: "はつね"}},
				}, {
					Text: "歌う", PerformerIDs: []string{"miku"},
					Ruby: []lyricssource.RubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}},
				}}, TrailingPerformerIDs: []string{"miku"},
			}},
		},
	}
	return report, identity, fixed
}

func TestBuildDraftRejectsOneByteWikitextDrift(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	fixed.Wikitext[0] = '-'

	assertFixedRevisionHashMismatchFailsClosed(t, report, identity, fixed)
}

func TestBuildDraftRejectsAdvertisedSHA1UnrelatedToWikitext(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	unrelatedSHA1 := strings.Repeat("0", 40)
	if unrelatedSHA1 == fixed.SHA1 {
		t.Fatal("test SHA1 unexpectedly matches the fixture content")
	}
	fixed.SHA1 = unrelatedSHA1
	report.UniqueComplete[0].Candidate.SHA1 = unrelatedSHA1

	assertFixedRevisionHashMismatchFailsClosed(t, report, identity, fixed)
}

func assertFixedRevisionHashMismatchFailsClosed(t *testing.T, report PreflightReport, identity lyricscontract.CatalogIdentity, fixed lyricssource.FixedRevision) {
	t.Helper()
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err == nil || !strings.Contains(err.Error(), "exact wikitext bytes") {
		t.Fatalf("BuildDraft hash mismatch error = %v", err)
	}
	if !reflect.DeepEqual(draft, Draft{}) {
		t.Fatalf("BuildDraft returned a draft on hash mismatch: %+v", draft)
	}

	reportDigest := sha256.Sum256([]byte("audited preflight report bytes"))
	manifest, err := NewManifest(report, hex.EncodeToString(reportDigest[:]), []Draft{draft})
	if err == nil {
		t.Fatal("NewManifest accepted the failed fixed revision")
	}
	if !reflect.DeepEqual(manifest, Manifest{}) {
		t.Fatalf("NewManifest returned a manifest for the failed fixed revision: %+v", manifest)
	}
}

func TestBuildManifestIsDeterministicAndRoundTrips(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	if err := ValidatePreflight(report); err != nil {
		t.Fatal(err)
	}
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Lines[0].Segments[0].Text+draft.Lines[0].Segments[1].Text != draft.Lines[0].Japanese {
		t.Fatalf("segments=%+v line=%q", draft.Lines[0].Segments, draft.Lines[0].Japanese)
	}
	if draft.Source.FetchedAt != fixed.FetchedAt.UTC().Format(time.RFC3339Nano) || draft.Source.FetchedAt == report.GeneratedAt {
		t.Fatalf("source fetchedAt=%q fixed=%q preflight=%q", draft.Source.FetchedAt,
			fixed.FetchedAt.UTC().Format(time.RFC3339Nano), report.GeneratedAt)
	}
	reportBody := []byte("audited preflight report bytes")
	digest := sha256.Sum256(reportBody)
	manifest, err := NewManifest(report, hex.EncodeToString(digest[:]), []Draft{draft})
	if err != nil {
		t.Fatal(err)
	}
	first, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("manifest serialization is not deterministic")
	}
	decoded, err := DecodeManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.BatchSHA256 != manifest.BatchSHA256 || decoded.Items[0].DraftSHA256 != draft.DraftSHA256 ||
		decoded.CatalogReference[0].LineCount != 1 {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestBuildDraftCanonicalizesHistoricalPerformerAndRubyVocabularyBeforePersistence(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	fixed.Extraction.RubyGeneratorVersion = "sekaipedia-romaji-kana-v1"
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Performers) != 1 || draft.Performers[0].PerformerID != "歌唱者-21" ||
		draft.Performers[0].Name != "初音ミク" || draft.RubyGeneratorVersion != "sekaipedia-ruby-kana-v1" {
		t.Fatalf("canonical staged metadata=%+v ruby=%q", draft.Performers, draft.RubyGeneratorVersion)
	}
	body, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte(`"miku"`), []byte(`"Miku"`), []byte("sekaipedia-romaji-kana-v1"),
	} {
		if bytes.Contains(body, forbidden) {
			t.Fatalf("historical source-local vocabulary escaped into a staged draft: %s", body)
		}
	}

	historical := draft
	historical.RubyGeneratorVersion = "sekaipedia-romaji-kana-v1"
	historical.Document.Full.RubyGeneratorVersion = historical.RubyGeneratorVersion
	historical.DocumentSHA256, _ = lyricsSourceDocumentDigest(historical.Document)
	historical.DraftSHA256 = ""
	historical.DraftSHA256, _ = draftDigest(historical)
	if err := ValidateDraft(historical); err == nil {
		t.Fatal("new staged draft validation accepted retired ruby vocabulary")
	}
}

func TestBuildDraftOmitsUnknownPerformerSegmentationWithoutEcho(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	fixed.Extraction.Performers = []lyricssource.Performer{{
		PerformerID: "mikito-p", Name: "Mikito-P", Color: "#33CCBB",
	}}
	for lineIndex := range fixed.Extraction.Lines {
		for segmentIndex := range fixed.Extraction.Lines[lineIndex].Segments {
			fixed.Extraction.Lines[lineIndex].Segments[segmentIndex].PerformerIDs = []string{"mikito-p"}
		}
		fixed.Extraction.Lines[lineIndex].TrailingPerformerIDs = []string{"mikito-p"}
	}
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Performers) != 0 || draft.Document.Provenance.PerformerSegmentation != nil ||
		len(draft.Lines) != 1 || len(draft.Lines[0].Segments) != 1 ||
		draft.Lines[0].Segments[0].Text != draft.Lines[0].Japanese ||
		len(draft.Lines[0].Segments[0].PerformerIDs) != 0 || len(draft.Lines[0].TrailingPerformerIDs) != 0 {
		t.Fatalf("unsafe staged performer segmentation was not omitted: %+v", draft)
	}
	body, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("mikito")) || !bytes.Contains(body, []byte("初音歌う")) {
		t.Fatal("staged draft leaked an unknown performer or removed valid lyric text")
	}
}

func TestManifestSerializedSizeBudgetMatchesCanonicalOutput(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := sha256.Sum256([]byte("audited preflight report bytes"))
	manifest, err := NewManifest(report, hex.EncodeToString(reportDigest[:]), []Draft{draft})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	measured, err := manifestSerializedSize(manifest, len(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if measured != len(encoded) {
		t.Fatalf("measured serialized bytes=%d, canonical bytes=%d", measured, len(encoded))
	}
	if err := validateManifestSerializedSize(manifest, len(encoded)); err != nil {
		t.Fatalf("exact serialized-size budget: %v", err)
	}
	if err := validateManifestSerializedSize(manifest, len(encoded)-1); err == nil {
		t.Fatal("manifest exceeded a smaller serialized-size budget")
	}
}

func TestValidateDraftRejectsMissingOrNoncanonicalSourceFetchedAt(t *testing.T) {
	for _, fetchedAt := range []string{"", "1970-01-01T00:00:00Z", "2026-07-30T20:34:57+08:00"} {
		t.Run(fetchedAt, func(t *testing.T) {
			draft := validSemanticDraft(t)
			draft.Source.FetchedAt = fetchedAt
			draft.DraftSHA256, _ = draftDigest(draft)
			if err := ValidateDraft(draft); err == nil || !strings.Contains(err.Error(), "fixed source metadata") {
				t.Fatalf("fetchedAt=%q error=%v", fetchedAt, err)
			}
		})
	}
}

func TestValidateDraftRejectsSegmentAndRubyConcatenationDrift(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}

	segmentDrift := draft
	segmentDrift.Lines = cloneLines(draft.Lines)
	segmentDrift.Lines[0].Segments[1].Text = "踊る"
	segmentDrift.ExtractedLinesSHA256 = model.LyricsSourceExtractedLinesSHA256(segmentDrift.Lines)
	segmentDrift.DraftSHA256, _ = draftDigest(segmentDrift)
	if err := ValidateDraft(segmentDrift); err == nil || !strings.Contains(err.Error(), "ruby spans") {
		t.Fatalf("segment drift error=%v", err)
	}

	rubyDrift := draft
	rubyDrift.Lines = cloneLines(draft.Lines)
	rubyDrift.Lines[0].Segments[1].Ruby[0].Text = "踊"
	rubyDrift.ExtractedLinesSHA256 = model.LyricsSourceExtractedLinesSHA256(rubyDrift.Lines)
	rubyDrift.DraftSHA256, _ = draftDigest(rubyDrift)
	if err := ValidateDraft(rubyDrift); err == nil || !strings.Contains(err.Error(), "ruby spans") {
		t.Fatalf("ruby drift error=%v", err)
	}

	lineDrift := draft
	lineDrift.Lines = cloneLines(draft.Lines)
	lineDrift.Lines[0].Japanese = "別の行"
	lineDrift.ExtractedLinesSHA256 = model.LyricsSourceExtractedLinesSHA256(lineDrift.Lines)
	lineDrift.DraftSHA256, _ = draftDigest(lineDrift)
	if err := ValidateDraft(lineDrift); err == nil || !strings.Contains(err.Error(), "segments do not concatenate") {
		t.Fatalf("line drift error=%v", err)
	}
}

func TestPreflightReportSizeLimitLeavesReviewedEnvelopeAroundReceipt(t *testing.T) {
	if MaxPrivateEvidenceReceiptRawBytes != 32<<20 || MaxPrivateEvidenceReceiptBytes != 64<<20 {
		t.Fatalf("receipt limits changed: raw=%d encoded=%d", MaxPrivateEvidenceReceiptRawBytes, MaxPrivateEvidenceReceiptBytes)
	}
	if MaxPreflightReportEnvelopeBytes != 32<<20 || MaxPreflightReportBytes != 96<<20 {
		t.Fatalf(
			"reviewed report transport limit changed: receipt=%d envelope=%d report=%d",
			MaxPrivateEvidenceReceiptBytes,
			MaxPreflightReportEnvelopeBytes,
			MaxPreflightReportBytes,
		)
	}
	if MaxPreflightReportBytes != MaxPrivateEvidenceReceiptBytes+MaxPreflightReportEnvelopeBytes {
		t.Fatal("preflight report limit is not derived from the unchanged receipt limit plus the reviewed envelope allowance")
	}
	for _, size := range []int{0, MaxPrivateEvidenceReceiptBytes, MaxPreflightReportBytes} {
		if err := validatePreflightReportSize(size); err != nil {
			t.Fatalf("size %d rejected: %v", size, err)
		}
	}
	if err := validatePreflightReportSize(MaxPreflightReportBytes + 1); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("%d bytes", MaxPreflightReportBytes)) {
		t.Fatalf("over-limit report error=%v", err)
	}
}

func TestDecodePreflightRequiresCompleteClosedUniqueReport(t *testing.T) {
	report, _, _ := validPreflightAndFixed(t)
	body, err := jsonMarshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePreflight(body); err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte{}, body[:len(body)-1]...)
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	if _, err := DecodePreflight(unknown); err == nil {
		t.Fatal("preflight decoder accepted an unknown field")
	}
	duplicate := []byte(strings.Replace(string(body), `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1))
	if _, err := DecodePreflight(duplicate); err == nil {
		t.Fatal("preflight decoder accepted a duplicate key")
	}
	partial := report
	partial.Missing = nil
	partialBody, _ := jsonMarshal(partial)
	if _, err := DecodePreflight(partialBody); err == nil {
		t.Fatal("preflight decoder accepted a partial report")
	}
}

func TestValidateCandidateAcceptsOnlyCanonicalSekaipediaRevisionIdentity(t *testing.T) {
	report, _, _ := sekaipediaPreflightAndFixed(t)
	candidate := *report.UniqueComplete[0].Candidate
	if err := validateCandidate(candidate); err != nil {
		t.Fatalf("canonical Sekaipedia candidate: %v", err)
	}
	sourceCandidate := candidate.SourceCandidate()
	if sourceCandidate.Provider != model.LyricsSourceProviderSekaipedia ||
		sourceCandidate.Origin != model.LyricsSourceOriginSekaipedia ||
		sourceCandidate.RevisionTimestamp != candidate.RevisionTimestamp {
		t.Fatalf("Sekaipedia source candidate drifted: %+v", sourceCandidate)
	}

	for name, mutate := range map[string]func(*CandidateIdentity){
		"wrong origin": func(value *CandidateIdentity) {
			value.Origin = "https://sekaipedia.org"
		},
		"host without www": func(value *CandidateIdentity) {
			value.CanonicalURL = "https://sekaipedia.org/wiki/List_of_songs?oldid=335193"
		},
		"index path": func(value *CandidateIdentity) {
			value.CanonicalURL = "https://www.sekaipedia.org/index.php?oldid=335193&title=List_of_songs"
		},
		"bare wiki URL": func(value *CandidateIdentity) {
			value.CanonicalURL = "https://www.sekaipedia.org/wiki/List_of_songs"
		},
		"extra query": func(value *CandidateIdentity) {
			value.CanonicalURL = "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193&printable=yes"
		},
		"wrong oldid": func(value *CandidateIdentity) {
			value.CanonicalURL = "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335194"
		},
		"missing revision timestamp": func(value *CandidateIdentity) {
			value.RevisionTimestamp = ""
		},
		"noncanonical revision timestamp": func(value *CandidateIdentity) {
			value.RevisionTimestamp = "2026-07-27T16:29:13.000Z"
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := candidate
			mutate(&invalid)
			if err := validateCandidate(invalid); err == nil {
				t.Fatal("invalid Sekaipedia candidate was accepted")
			}
		})
	}
}

func TestBuildDraftBindsSekaipediaSelectedJapaneseBytesThroughExactRevisionEvidence(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	candidate := report.UniqueComplete[0].Candidate
	candidate.Provider = model.LyricsSourceProviderSekaipedia
	candidate.Origin = model.LyricsSourceOriginSekaipedia
	candidate.RevisionTimestamp = "2026-07-30T12:34:56.123Z"
	candidate.CanonicalURL = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34"

	fullWikitext := append([]byte{}, fixed.Wikitext...)
	fullDigest := sha256.Sum256(fullWikitext)
	fixed.Provider = candidate.Provider
	fixed.Origin = candidate.Origin
	fixed.CanonicalURL = candidate.CanonicalURL
	fixed.RevisionTimestamp = time.Date(2026, time.July, 30, 12, 34, 56, 123000000, time.UTC)
	fixed.FetchedAt = time.Date(2026, time.July, 30, 12, 35, 0, 0, time.UTC)
	fixed.RawSHA256 = hex.EncodeToString(fullDigest[:])
	fixed.Wikitext = []byte("初音歌う")

	listRaw, err := os.ReadFile("../../../internal/lyricssource/testdata/sekaipedia-list-335193.json")
	if err != nil {
		t.Fatal(err)
	}
	listRawDigest := sha256.Sum256(listRaw)
	listRawSHA256 := hex.EncodeToString(listRawDigest[:])
	const evidenceFetchedAt = "2026-07-30T12:34:57Z"
	listEvidence := lyricssource.IndexEvidence{
		EvidenceID: sekaipediaAcquisitionEvidenceIDForTest(
			"authority:sekaipedia:list-of-songs:335193", evidenceFetchedAt, listRawSHA256,
		),
		SHA256: listRawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
		PageID: 268, RevisionID: 335193, RevisionTimestamp: "2026-07-27T16:29:13Z",
		MediaWikiSHA1: "b216a827f88c59f5e954a120027832fe9cd74413", Title: "List of songs",
		CanonicalURL: "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193",
		Categories:   []string{"Lists", "Project SEKAI"}, FetchedAt: evidenceFetchedAt,
		Raw: listRaw, RawSHA256: listRawSHA256,
	}
	songRaw, err := json.Marshal(map[string]any{
		"query": map[string]any{"pages": map[string]any{"12": map[string]any{
			"pageid": 12, "title": candidate.Title,
			"categories": []map[string]string{{"title": "Category:Lyrics"}, {"title": "Category:Songs"}},
			"revisions": []map[string]any{{
				"revid": 34, "timestamp": candidate.RevisionTimestamp, "sha1": candidate.SHA1,
				"slots": map[string]any{"main": map[string]string{"content": string(fullWikitext)}},
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	songRawDigest := sha256.Sum256(songRaw)
	songRawSHA256 := hex.EncodeToString(songRawDigest[:])
	songEvidence := lyricssource.IndexEvidence{
		EvidenceID: sekaipediaAcquisitionEvidenceIDForTest(
			"revision:sekaipedia:12:34", evidenceFetchedAt, songRawSHA256,
		),
		SHA256: songRawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
		RevisionTimestamp: candidate.RevisionTimestamp, MediaWikiSHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		FetchedAt: evidenceFetchedAt, Raw: songRaw, RawSHA256: songRawSHA256,
	}
	candidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{
		{EvidenceID: listEvidence.EvidenceID, SHA256: listEvidence.SHA256},
		{EvidenceID: songEvidence.EvidenceID, SHA256: songEvidence.SHA256},
	}
	fixed.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
	fixed.IndexEvidence = []lyricssource.IndexEvidence{listEvidence, songEvidence}

	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatalf("Sekaipedia selected-Japanese draft: %v", err)
	}
	selectedDigest := sha256.Sum256(fixed.Wikitext)
	if draft.Artifacts[0].Identity.SHA1 != candidate.SHA1 ||
		draft.Artifacts[0].Identity.RevisionTimestamp != candidate.RevisionTimestamp ||
		draft.Artifacts[0].RawWikitextSHA256 != hex.EncodeToString(selectedDigest[:]) {
		t.Fatalf("Sekaipedia selected-Japanese binding drifted: %+v", draft.Artifacts[0])
	}

	driftedJapanese := fixed
	driftedJapanese.Wikitext = []byte("別の歌詞")
	if driftedDraft, err := BuildDraft(report.UniqueComplete[0], identity, driftedJapanese); err == nil ||
		!reflect.DeepEqual(driftedDraft, Draft{}) {
		t.Fatalf("drifted selected-Japanese bytes produced draft=%+v err=%v", driftedDraft, err)
	}
	fullPage := fixed
	fullPage.Wikitext = fullWikitext
	if fullPageDraft, err := BuildDraft(report.UniqueComplete[0], identity, fullPage); err == nil ||
		!reflect.DeepEqual(fullPageDraft, Draft{}) {
		t.Fatalf("complete Sekaipedia page bypassed selected-Japanese boundary: draft=%+v err=%v", fullPageDraft, err)
	}

	drifted := fixed
	drifted.IndexEvidence = append([]lyricssource.IndexEvidence{}, fixed.IndexEvidence...)
	drifted.IndexEvidence[1].RevisionTimestamp = "2026-07-30T12:34:56.124Z"
	if driftedDraft, err := BuildDraft(report.UniqueComplete[0], identity, drifted); err == nil ||
		!reflect.DeepEqual(driftedDraft, Draft{}) {
		t.Fatalf("drifted Sekaipedia evidence produced draft=%+v err=%v", driftedDraft, err)
	}
}

func sekaipediaAcquisitionEvidenceIDForTest(baseID, fetchedAt, rawSHA256 string) string {
	identity := strings.Join([]string{
		"lyrics-source-index-evidence-v1",
		string(lyricssource.IndexEvidenceKindMediaWikiRevision),
		string(model.LyricsSourceProviderSekaipedia),
		model.LyricsSourceOriginSekaipedia,
		baseID,
		fetchedAt,
		rawSHA256,
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s:%x", baseID, digest)
}

func TestBuildDraftPreservesExactSekaipediaRevisionTimestamp(t *testing.T) {
	report, identity, fixed := sekaipediaPreflightAndFixed(t)
	if err := ValidatePreflight(report); err != nil {
		t.Fatalf("Sekaipedia preflight: %v", err)
	}
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatalf("Sekaipedia draft: %v", err)
	}
	want := report.UniqueComplete[0].Candidate.RevisionTimestamp
	if len(draft.Document.FixedIdentities) != 1 || len(draft.Artifacts) != 1 ||
		draft.Document.FixedIdentities[0].RevisionTimestamp != want ||
		draft.Artifacts[0].Identity.RevisionTimestamp != want {
		t.Fatalf("revisionTimestamp was not preserved exactly: document=%+v artifacts=%+v",
			draft.Document.FixedIdentities, draft.Artifacts)
	}

	drifted := fixed
	drifted.RevisionTimestamp = drifted.RevisionTimestamp.Add(time.Second)
	if driftedDraft, err := BuildDraft(report.UniqueComplete[0], identity, drifted); err == nil || !reflect.DeepEqual(driftedDraft, Draft{}) {
		t.Fatalf("fixed revisionTimestamp drift produced draft=%+v err=%v", driftedDraft, err)
	}
}

func sekaipediaPreflightAndFixed(t *testing.T) (PreflightReport, lyricscontract.CatalogIdentity, lyricssource.FixedRevision) {
	t.Helper()
	report, identity, fixed := validPreflightAndFixed(t)
	candidate := report.UniqueComplete[0].Candidate
	candidate.Provider = model.LyricsSourceProviderSekaipedia
	candidate.Origin = model.LyricsSourceOriginSekaipedia
	candidate.RevisionTimestamp = "2026-07-27T16:29:13Z"
	candidate.CanonicalURL = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34"

	fullWikitext := append([]byte{}, fixed.Wikitext...)
	fullDigest := sha256.Sum256(fullWikitext)
	fixed.Provider = candidate.Provider
	fixed.Origin = candidate.Origin
	fixed.RevisionTimestamp = time.Date(2026, time.July, 27, 16, 29, 13, 0, time.UTC)
	fixed.CanonicalURL = candidate.CanonicalURL
	fixed.FetchedAt = time.Date(2026, time.July, 27, 16, 29, 14, 123000000, time.UTC)
	fixed.RawSHA256 = hex.EncodeToString(fullDigest[:])
	fixed.Wikitext = lyricssource.SekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)
	if len(fixed.Wikitext) == 0 {
		t.Fatal("Sekaipedia timestamp fixture produced no selected-Japanese bytes")
	}

	listRaw, err := os.ReadFile("../../../internal/lyricssource/testdata/sekaipedia-list-335193.json")
	if err != nil {
		t.Fatal(err)
	}
	listRawDigest := sha256.Sum256(listRaw)
	listRawSHA256 := hex.EncodeToString(listRawDigest[:])
	const evidenceFetchedAt = "2026-07-27T16:29:14.123Z"
	listEvidence := lyricssource.IndexEvidence{
		EvidenceID: sekaipediaAcquisitionEvidenceIDForTest(
			"authority:sekaipedia:list-of-songs:335193", evidenceFetchedAt, listRawSHA256,
		),
		SHA256: listRawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
		PageID: 268, RevisionID: 335193, RevisionTimestamp: "2026-07-27T16:29:13Z",
		MediaWikiSHA1: "b216a827f88c59f5e954a120027832fe9cd74413", Title: "List of songs",
		CanonicalURL: "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193",
		Categories:   []string{"Lists", "Project SEKAI"}, FetchedAt: evidenceFetchedAt,
		Raw: listRaw, RawSHA256: listRawSHA256,
	}
	songRaw, err := json.Marshal(map[string]any{
		"query": map[string]any{"pages": map[string]any{"12": map[string]any{
			"pageid": 12, "title": candidate.Title,
			"categories": []map[string]string{{"title": "Category:Lyrics"}, {"title": "Category:Songs"}},
			"revisions": []map[string]any{{
				"revid": 34, "timestamp": candidate.RevisionTimestamp, "sha1": candidate.SHA1,
				"slots": map[string]any{"main": map[string]string{"content": string(fullWikitext)}},
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	songRawDigest := sha256.Sum256(songRaw)
	songRawSHA256 := hex.EncodeToString(songRawDigest[:])
	songEvidence := lyricssource.IndexEvidence{
		EvidenceID: sekaipediaAcquisitionEvidenceIDForTest(
			"revision:sekaipedia:12:34", evidenceFetchedAt, songRawSHA256,
		),
		SHA256: songRawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
		RevisionTimestamp: candidate.RevisionTimestamp, MediaWikiSHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		FetchedAt: evidenceFetchedAt, Raw: songRaw, RawSHA256: songRawSHA256,
	}
	candidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{
		{EvidenceID: listEvidence.EvidenceID, SHA256: listEvidence.SHA256},
		{EvidenceID: songEvidence.EvidenceID, SHA256: songEvidence.SHA256},
	}
	fixed.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
	fixed.IndexEvidence = []lyricssource.IndexEvidence{listEvidence, songEvidence}
	return report, identity, fixed
}

func cloneLines(lines []model.LyricsSourceExtractedLine) []model.LyricsSourceExtractedLine {
	result := make([]model.LyricsSourceExtractedLine, len(lines))
	for lineIndex, line := range lines {
		result[lineIndex] = line
		result[lineIndex].TrailingPerformerIDs = append([]string{}, line.TrailingPerformerIDs...)
		result[lineIndex].Segments = make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			result[lineIndex].Segments[segmentIndex] = segment
			result[lineIndex].Segments[segmentIndex].PerformerIDs = append([]string{}, segment.PerformerIDs...)
			result[lineIndex].Segments[segmentIndex].Ruby = append([]model.LyricsSourceRubySpan{}, segment.Ruby...)
		}
	}
	return result
}

func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}
