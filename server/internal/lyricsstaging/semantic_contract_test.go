package lyricsstaging

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func validSemanticDraft(t *testing.T) Draft {
	t.Helper()
	report, identity, fixed := validPreflightAndFixed(t)
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func resignSemanticDraft(t *testing.T, draft *Draft) {
	t.Helper()
	draft.ExtractedLinesSHA256 = model.LyricsSourceExtractedLinesSHA256(draft.Lines)
	var err error
	draft.DraftSHA256, err = draftDigest(*draft)
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateDraftAcceptsMetadataFreeLinesAndKnownPerformers(t *testing.T) {
	known := validSemanticDraft(t)
	if err := ValidateDraft(known); err != nil {
		t.Fatalf("known performers: %v", err)
	}

	metadataFree := validSemanticDraft(t)
	metadataFree.Performers = []model.LyricsSourcePerformer{}
	metadataFree.Lines = []model.LyricsSourceExtractedLine{{
		Japanese: "歌う",
		Segments: []model.LyricsSourceSegment{{
			Text:         "歌う",
			PerformerIDs: []string{},
			Ruby:         []model.LyricsSourceRubySpan{{Text: "歌", Reading: ""}, {Text: "う"}},
		}},
		TrailingPerformerIDs: []string{},
	}}
	metadataFree.Document.Full = model.NewLyricsSourceFullFromLegacy(metadataFree.SelectedVersion, metadataFree.Performers,
		metadataFree.RubyGeneratorVersion, metadataFree.Lines)
	metadataFree.Document.Provenance.PerformerSegmentation = nil
	metadataFree.Document.Provenance.Ruby = nil
	metadataFree.DocumentSHA256, _ = lyricsSourceDocumentDigest(metadataFree.Document)
	resignSemanticDraft(t, &metadataFree)
	if err := ValidateDraft(metadataFree); err != nil {
		t.Fatalf("metadata-free line: %v", err)
	}
}

func TestValidateDraftEnforcesVocaloidFullWithoutPerformerSegmentation(t *testing.T) {
	invalid := validSemanticDraft(t)
	invalid.SelectedVersion.Kind = "vocaloid"
	invalid.Document.Full.Version.Kind = "vocaloid"
	invalid.DocumentSHA256, _ = lyricsSourceDocumentDigest(invalid.Document)
	resignSemanticDraft(t, &invalid)
	if err := ValidateDraft(invalid); err == nil || !strings.Contains(strings.ToLower(err.Error()), "vocaloid") {
		t.Fatalf("segmented Vocaloid Full error=%v", err)
	}

	valid := validSemanticDraft(t)
	valid.SelectedVersion.Kind = "vocaloid"
	valid.Performers = []model.LyricsSourcePerformer{}
	valid.Document.Full.Version.Kind = "vocaloid"
	valid.Document.Full.Performers = []model.LyricsSourcePerformer{}
	valid.Document.Provenance.PerformerSegmentation = nil
	for index := range valid.Lines {
		ruby := []model.LyricsSourceRubySpan{}
		for _, segment := range valid.Lines[index].Segments {
			ruby = append(ruby, segment.Ruby...)
		}
		valid.Lines[index].Segments = []model.LyricsSourceSegment{{
			Text: valid.Lines[index].Japanese, PerformerIDs: []string{}, Ruby: ruby,
		}}
		valid.Lines[index].TrailingPerformerIDs = []string{}
		valid.Document.Full.Lines[index].Segments = append([]model.LyricsSourceSegment{}, valid.Lines[index].Segments...)
		valid.Document.Full.Lines[index].TrailingPerformerIDs = []string{}
	}
	valid.DocumentSHA256, _ = lyricsSourceDocumentDigest(valid.Document)
	resignSemanticDraft(t, &valid)
	if err := ValidateDraft(valid); err != nil {
		t.Fatalf("unsegmented Vocaloid Full: %v", err)
	}
}

func TestBuildDraftTransportsMarkedAuthoritativeVirtualSingerSegmentation(t *testing.T) {
	report, identity, fixed := authoritativeVirtualSingerFixture(t)
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatalf("marked VIRTUAL SINGER draft: %v", err)
	}
	if draft.SelectedVersion.Kind != "vocaloid" || len(draft.Performers) == 0 ||
		len(draft.Lines[0].Segments) < 2 || draft.Document.Provenance.PerformerSegmentation == nil ||
		draft.Document.PrivateReview == nil ||
		draft.Document.PrivateReview.PerformerSegmentationEvidence !=
			model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured {
		t.Fatalf("authoritative VIRTUAL SINGER transport drifted: %+v", draft.Document)
	}

	manifest, err := NewManifest(report, strings.Repeat("d", 64), []Draft{draft})
	if err != nil {
		t.Fatalf("marked VIRTUAL SINGER manifest: %v", err)
	}
	body, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(body)
	for _, forbidden := range [][]byte{[]byte(`"romaji"`), []byte(`"romanization"`), []byte(`"romanisation"`), []byte(`"romanized"`)} {
		if bytes.Contains(lower, forbidden) {
			t.Fatalf("staged manifest persisted forbidden romanization: %s", body)
		}
	}
	if bytes.Count(body, []byte(string(model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured))) != 1 {
		t.Fatalf("private segmentation marker drifted in manifest: %s", body)
	}
	decoded, err := DecodeManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	privateReview := decoded.Items[0].Document.PrivateReview
	if privateReview == nil || privateReview.PerformerSegmentationEvidence !=
		model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured {
		t.Fatalf("round-trip private marker drifted: %+v", privateReview)
	}
}

func TestBuildDraftRejectsMissingOrWrongVirtualSingerSegmentationMarker(t *testing.T) {
	for name, mutate := range map[string]func(*model.LyricsSourceDocument){
		"missing marker": func(document *model.LyricsSourceDocument) {
			document.PrivateReview = nil
		},
		"wrong marker": func(document *model.LyricsSourceDocument) {
			document.PrivateReview.PerformerSegmentationEvidence = "structured"
		},
	} {
		t.Run(name, func(t *testing.T) {
			report, identity, fixed := authoritativeVirtualSingerFixture(t)
			mutate(fixed.Document)
			if draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed); err == nil || draft.DraftSHA256 != "" {
				t.Fatalf("invalid marker produced draft=%+v err=%v", draft, err)
			}
		})
	}
}

func TestBuildDraftRequiresUnmarkedLegacyVocaloidToBeFlattened(t *testing.T) {
	report, identity, fixed := legacyVocaloidFixture(t)
	if draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed); err == nil || draft.DraftSHA256 != "" {
		t.Fatalf("unmarked segmented legacy Vocaloid produced draft=%+v err=%v", draft, err)
	}

	flattenStructuredExtraction(&fixed.Extraction)
	draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatalf("flattened legacy Vocaloid: %v", err)
	}
	if len(draft.Performers) != 0 || draft.Document.PrivateReview != nil ||
		draft.Document.Provenance.PerformerSegmentation != nil || len(draft.Lines[0].Segments) != 1 ||
		draft.Lines[0].Segments[0].Text != draft.Lines[0].Japanese ||
		len(draft.Lines[0].Segments[0].PerformerIDs) != 0 || len(draft.Lines[0].TrailingPerformerIDs) != 0 {
		t.Fatalf("flattened legacy Vocaloid drifted: %+v", draft)
	}
}

func TestBuildDraftKeepsOriginalPerformerFree(t *testing.T) {
	report, identity, fixed := validPreflightAndFixed(t)
	report.UniqueComplete[0].Candidate.RenditionKey = "full-original"
	fixed.RenditionKey = "full-original"
	fixed.Extraction.Version = lyricssource.LyricsVersion{Kind: "original", Label: "Original Version"}
	if draft, err := BuildDraft(report.UniqueComplete[0], identity, fixed); err == nil || draft.DraftSHA256 != "" {
		t.Fatalf("segmented Original produced draft=%+v err=%v", draft, err)
	}
	flattenStructuredExtraction(&fixed.Extraction)
	if _, err := BuildDraft(report.UniqueComplete[0], identity, fixed); err != nil {
		t.Fatalf("performer-free Original: %v", err)
	}
}

func authoritativeVirtualSingerFixture(t *testing.T) (PreflightReport, lyricscontract.CatalogIdentity, lyricssource.FixedRevision) {
	t.Helper()
	report, identity, fixed := validPreflightAndFixed(t)
	base, err := BuildDraft(report.UniqueComplete[0], identity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	candidate := report.UniqueComplete[0].Candidate
	candidate.RenditionKey = "full-vocaloid"
	fixed.RenditionKey = candidate.RenditionKey
	fixed.Extraction.Version = lyricssource.LyricsVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
	identity.Vocals = []model.CatalogVocalSignal{{VocalID: 1, VocalType: "virtual_singer"}}

	document := base.Document
	document.Full.Version = model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
	document.FixedIdentities[0].RenditionKey = candidate.RenditionKey
	document.FixedIdentities[0].CompositionRenditionKey = candidate.RenditionKey
	component := model.LyricsSourceComponentRef{RenditionKey: candidate.RenditionKey}
	document.Provenance.FullText = component
	document.Provenance.PerformerSegmentation = &component
	document.Provenance.Ruby = &component
	document.Provenance.VersionEvidence = component
	document.PrivateReview = &model.LyricsSourcePrivateReview{
		PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("authoritative VIRTUAL SINGER fixture: %v", err)
	}
	fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)
	fixed.Document = &document
	return report, identity, fixed
}

func legacyVocaloidFixture(t *testing.T) (PreflightReport, lyricscontract.CatalogIdentity, lyricssource.FixedRevision) {
	t.Helper()
	report, identity, fixed := validPreflightAndFixed(t)
	report.UniqueComplete[0].Candidate.RenditionKey = "full-vocaloid"
	fixed.RenditionKey = "full-vocaloid"
	fixed.Extraction.Version = lyricssource.LyricsVersion{Kind: "vocaloid", Label: "Vocaloid Version"}
	identity.Vocals = []model.CatalogVocalSignal{{VocalID: 1, VocalType: "virtual_singer"}}
	return report, identity, fixed
}

func flattenStructuredExtraction(extraction *lyricssource.Extraction) {
	extraction.Performers = []lyricssource.Performer{}
	for lineIndex := range extraction.Lines {
		line := &extraction.Lines[lineIndex]
		ruby := []lyricssource.RubySpan{}
		for _, segment := range line.Segments {
			ruby = append(ruby, segment.Ruby...)
		}
		line.Segments = []lyricssource.LyricsSegment{{
			Text: line.Japanese, PerformerIDs: []string{}, Ruby: ruby,
		}}
		line.TrailingPerformerIDs = []string{}
	}
}

func TestValidateDraftRejectsInvalidPerformerReferences(t *testing.T) {
	for name, mutate := range map[string]func(*Draft){
		"unknown segment performer": func(draft *Draft) {
			draft.Lines[0].Segments[0].PerformerIDs = []string{"unknown"}
		},
		"unknown trailing performer": func(draft *Draft) {
			draft.Lines[0].TrailingPerformerIDs = []string{"unknown"}
		},
		"duplicate segment performer": func(draft *Draft) {
			draft.Lines[0].Segments[0].PerformerIDs = []string{"miku", "miku"}
		},
		"duplicate trailing performer": func(draft *Draft) {
			draft.Lines[0].TrailingPerformerIDs = []string{"miku", "miku"}
		},
		"malformed segment performer": func(draft *Draft) {
			draft.Lines[0].Segments[0].PerformerIDs = []string{"miku!"}
		},
		"malformed trailing performer": func(draft *Draft) {
			draft.Lines[0].TrailingPerformerIDs = []string{"miku!"}
		},
		"malformed reference without legend": func(draft *Draft) {
			draft.Performers = []model.LyricsSourcePerformer{}
			draft.Lines[0].Segments[0].PerformerIDs = []string{"miku!"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			draft := validSemanticDraft(t)
			draft.Lines = cloneLines(draft.Lines)
			mutate(&draft)
			resignSemanticDraft(t, &draft)
			if err := ValidateDraft(draft); err == nil {
				t.Fatal("invalid performer reference was accepted")
			}
		})
	}
}

func TestValidateDraftRejectsExcessiveCounts(t *testing.T) {
	for name, mutate := range map[string]func(*Draft){
		"performers": func(draft *Draft) {
			draft.Performers = make([]model.LyricsSourcePerformer, maxStagedPerformers+1)
			for index := range draft.Performers {
				draft.Performers[index] = model.LyricsSourcePerformer{
					PerformerID: "performer_" + strconv.Itoa(index),
					Name:        "Performer",
				}
			}
		},
		"segments per line": func(draft *Draft) {
			segments := make([]model.LyricsSourceSegment, maxStagedSegmentsPerLine+1)
			for index := range segments {
				segments[index] = model.LyricsSourceSegment{
					Text:         "a",
					PerformerIDs: []string{},
					Ruby:         []model.LyricsSourceRubySpan{{Text: "a"}},
				}
			}
			draft.Performers = []model.LyricsSourcePerformer{}
			draft.Lines = []model.LyricsSourceExtractedLine{{
				Japanese:             strings.Repeat("a", len(segments)),
				Segments:             segments,
				TrailingPerformerIDs: []string{},
			}}
		},
		"ruby spans per segment": func(draft *Draft) {
			ruby := make([]model.LyricsSourceRubySpan, maxStagedRubySpansPerSegment+1)
			for index := range ruby {
				ruby[index] = model.LyricsSourceRubySpan{Text: "a"}
			}
			draft.Performers = []model.LyricsSourcePerformer{}
			draft.Lines = []model.LyricsSourceExtractedLine{{
				Japanese: "歌",
				Segments: []model.LyricsSourceSegment{{
					Text:         "歌",
					PerformerIDs: []string{},
					Ruby:         ruby,
				}},
				TrailingPerformerIDs: []string{},
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			draft := validSemanticDraft(t)
			mutate(&draft)
			resignSemanticDraft(t, &draft)
			if err := ValidateDraft(draft); err == nil {
				t.Fatal("excessive count was accepted")
			}
		})
	}
}

func TestValidatePreflightRequiresConsistentMissingReasonDiagnostics(t *testing.T) {
	missingReport := func() PreflightReport {
		report, _, _ := validPreflightAndFixed(t)
		item := report.UniqueComplete[0]
		item.Candidate = nil
		item.LineCount = 0
		item.FetchAttempts = 0
		item.SearchDiagnostics = &SearchDiagnostics{}
		item.ReasonCode = string(lyricssource.ZeroCandidateNoSearchHits)
		report.UniqueComplete = []PreflightItem{}
		report.Missing = []PreflightItem{item}
		report.Summary.UniqueComplete = 0
		report.Summary.Missing = 1
		return report
	}

	if err := ValidatePreflight(missingReport()); err != nil {
		t.Fatalf("valid missing diagnostics: %v", err)
	}
	for name, mutate := range map[string]func(*PreflightItem){
		"missing diagnostics": func(item *PreflightItem) {
			item.SearchDiagnostics = nil
			item.ReasonCode = ""
		},
		"missing reason": func(item *PreflightItem) {
			item.ReasonCode = ""
		},
		"mismatched reason": func(item *PreflightItem) {
			item.ReasonCode = string(lyricssource.ZeroCandidateTitleMismatch)
		},
		"incomplete aggregate partition": func(item *PreflightItem) {
			item.SearchDiagnostics = &SearchDiagnostics{SearchHits: 1}
			item.ReasonCode = string(lyricssource.ZeroCandidateNoSearchHits)
		},
		"role counts exceed credit mismatches": func(item *PreflightItem) {
			item.SearchDiagnostics = &SearchDiagnostics{
				SearchHits: 1, CreditMismatch: 1, LyricistCreditMissing: 1, LyricistCreditMismatch: 1,
			}
			item.ReasonCode = string(lyricssource.ZeroCandidateCreditMismatch)
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := missingReport()
			mutate(&report.Missing[0])
			if err := ValidatePreflight(report); err == nil {
				t.Fatal("invalid missing diagnostics were accepted")
			}
		})
	}
}

func TestValidateDraftRejectsCumulativeRubyReadingOverflow(t *testing.T) {
	draft := validSemanticDraft(t)
	spanCount := maxStagedRubyReadingTotalBytes/maxStagedRubyReadingBytes + 1
	ruby := make([]model.LyricsSourceRubySpan, spanCount)
	for index := range ruby {
		ruby[index] = model.LyricsSourceRubySpan{Text: "a", Reading: strings.Repeat("r", maxStagedRubyReadingBytes)}
	}
	draft.Performers = []model.LyricsSourcePerformer{}
	draft.Lines = []model.LyricsSourceExtractedLine{{
		Japanese: strings.Repeat("a", spanCount),
		Segments: []model.LyricsSourceSegment{{
			Text: strings.Repeat("a", spanCount), PerformerIDs: []string{}, Ruby: ruby,
		}},
		TrailingPerformerIDs: []string{},
	}}
	resignSemanticDraft(t, &draft)
	if err := ValidateDraft(draft); err == nil || !strings.Contains(err.Error(), "ruby readings exceed") {
		t.Fatalf("cumulative ruby-reading error=%v", err)
	}
}

func TestValidateDraftRejectsExcessiveRubyAndMetadata(t *testing.T) {
	for name, mutate := range map[string]func(*Draft){
		"ruby text": func(draft *Draft) {
			draft.Performers = []model.LyricsSourcePerformer{}
			draft.Lines = []model.LyricsSourceExtractedLine{{
				Japanese: "歌",
				Segments: []model.LyricsSourceSegment{{
					Text:         "歌",
					PerformerIDs: []string{},
					Ruby:         []model.LyricsSourceRubySpan{{Text: strings.Repeat("a", maxStagedRubyTextBytes+1)}},
				}},
				TrailingPerformerIDs: []string{},
			}}
		},
		"ruby reading": func(draft *Draft) {
			draft.Lines = cloneLines(draft.Lines)
			draft.Lines[0].Segments[0].Ruby[0].Reading = strings.Repeat("a", maxStagedRubyReadingBytes+1)
		},
		"performer ID": func(draft *Draft) {
			draft.Performers[0].PerformerID = strings.Repeat("a", maxStagedPerformerIDBytes+1)
		},
		"performer name": func(draft *Draft) {
			draft.Performers[0].Name = strings.Repeat("a", maxStagedPerformerNameBytes+1)
		},
		"performer color": func(draft *Draft) {
			draft.Performers[0].Color = "#33CCBBA"
		},
		"version label": func(draft *Draft) {
			draft.SelectedVersion.Label = strings.Repeat("a", maxStagedVersionLabelBytes+1)
		},
		"ruby generator version": func(draft *Draft) {
			draft.RubyGeneratorVersion = strings.Repeat("a", maxStagedRubyGeneratorVersionBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			draft := validSemanticDraft(t)
			mutate(&draft)
			resignSemanticDraft(t, &draft)
			if err := ValidateDraft(draft); err == nil {
				t.Fatal("excessive semantic metadata was accepted")
			}
		})
	}
}
