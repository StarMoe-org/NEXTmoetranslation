package lyricscompose

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func testSource(key string) Source {
	return Source{
		SourceKey: key,
		Identity: Identity{
			CatalogSongKey: "music:42", RenditionKey: "sekai-original", FixedIdentityKey: "vocaloid-fandom:12:34:sha1",
		},
		SequenceKind: SequenceFull,
		VisibleJapanese: []VisibleLine{
			{Text: "か\u3099く", StanzaBreakBefore: true},
			{Text: "未来へ"},
		},
	}
}

func testSegmentation() Segmentation {
	return Segmentation{
		Performers: []Performer{{ID: "miku", Name: "初音ミク", Color: "#33CCBB"}},
		Lines: []SegmentedLine{
			{
				Segments: []Segment{
					{Text: "か\u3099", PerformerIDs: []string{"miku"}},
					{Text: "く", PerformerIDs: []string{}},
				},
				TrailingPerformerIDs: []string{"miku"},
			},
			{
				Segments:             []Segment{{Text: "未来へ", PerformerIDs: []string{"miku"}}},
				TrailingPerformerIDs: []string{},
			},
		},
	}
}

func testRuby() Ruby {
	return Ruby{
		GeneratorVersion: "fixed-ruby-v1",
		Lines: [][]RubySpan{
			{{Text: "か\u3099", Reading: "が"}, {Text: "く"}},
			{{Text: "未来", Reading: "みらい"}, {Text: "へ"}},
		},
	}
}

func TestComposeSupplementsAcrossFixedProvidersWithAuditableProvenance(t *testing.T) {
	base := testSource("fandom-full-text")
	segmentationSource := testSource("moegirl-segmentation")
	segmentationSource.Identity.FixedIdentityKey = "moegirl:56:78:sha1"
	segmentation := testSegmentation()
	segmentationSource.Segmentation = &segmentation
	rubySource := testSource("moegirl-ruby")
	rubySource.Identity.FixedIdentityKey = "moegirl:56:78:sha1"
	ruby := testRuby()
	rubySource.Ruby = &ruby

	result, err := Compose(base, segmentationSource, rubySource)
	if err != nil {
		t.Fatal(err)
	}
	if result.Provenance.FullText.SourceKey != "fandom-full-text" ||
		result.Provenance.FullText.FixedIdentityKey != "vocaloid-fandom:12:34:sha1" ||
		result.Provenance.Segmentation == nil || result.Provenance.Segmentation.SourceKey != "moegirl-segmentation" ||
		result.Provenance.Segmentation.FixedIdentityKey != "moegirl:56:78:sha1" ||
		result.Provenance.Ruby == nil || result.Provenance.Ruby.SourceKey != "moegirl-ruby" ||
		result.Provenance.Ruby.FixedIdentityKey != "moegirl:56:78:sha1" {
		t.Fatalf("provenance = %+v", result.Provenance)
	}
	if !result.Lines[0].StanzaBreakBefore || len(result.Lines[0].Segments) != 2 ||
		result.Lines[0].Segments[0].Text != "か\u3099" ||
		!reflect.DeepEqual(result.Lines[0].Segments[0].Ruby, []RubySpan{{Text: "か\u3099", Reading: "が"}}) ||
		!reflect.DeepEqual(result.Lines[0].Segments[1].Ruby, []RubySpan{{Text: "く"}}) {
		t.Fatalf("composed first line = %+v", result.Lines[0])
	}
	if result.RubyGeneratorVersion != "fixed-ruby-v1" || len(result.Performers) != 1 ||
		result.Performers[0] != (Performer{ID: "歌唱者-21", Name: "初音ミク", Color: "#33CCBB"}) ||
		!reflect.DeepEqual(result.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21"}) ||
		!reflect.DeepEqual(result.Lines[0].TrailingPerformerIDs, []string{"歌唱者-21"}) {
		t.Fatalf("composed metadata = performers=%+v line=%+v ruby=%q", result.Performers, result.Lines[0], result.RubyGeneratorVersion)
	}

	segmentation.Performers[0].Name = "changed"
	ruby.Lines[0][0].Reading = "changed"
	if result.Performers[0].Name != "初音ミク" || result.Lines[0].Segments[0].Ruby[0].Reading != "が" {
		t.Fatal("composition result aliases an input component")
	}
}

func TestComposeUsesLosslessDefaultsWithoutOptionalComponents(t *testing.T) {
	result, err := Compose(testSource("base"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Provenance.Segmentation != nil || result.Provenance.Ruby != nil || len(result.Lines) != 2 {
		t.Fatalf("default result = %+v", result)
	}
	for _, line := range result.Lines {
		if len(line.Segments) != 1 || line.Segments[0].Text != line.Text || len(line.Segments[0].Ruby) != 1 ||
			line.Segments[0].Ruby[0] != (RubySpan{Text: line.Text}) {
			t.Fatalf("lossless default line = %+v", line)
		}
	}
}

func TestComposeRequiresExactIdentityKindAndCompleteVisibleSequence(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*Source)
		target error
	}{
		"catalog song": {
			mutate: func(source *Source) { source.Identity.CatalogSongKey = "music:43" },
			target: ErrIdentityMismatch,
		},
		"original and cover rendition": {
			mutate: func(source *Source) { source.Identity.RenditionKey = "sekai-cover" },
			target: ErrIdentityMismatch,
		},
		"Full and Game": {
			mutate: func(source *Source) { source.SequenceKind = SequenceGame },
			target: ErrSequenceKindMismatch,
		},
		"line changed": {
			mutate: func(source *Source) { source.VisibleJapanese[1].Text = "未来に" },
			target: ErrVisibleTextMismatch,
		},
		"line order changed": {
			mutate: func(source *Source) {
				source.VisibleJapanese[0], source.VisibleJapanese[1] = source.VisibleJapanese[1], source.VisibleJapanese[0]
			},
			target: ErrVisibleTextMismatch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			supplement := testSource("supplement")
			test.mutate(&supplement)
			if _, err := Compose(testSource("base"), supplement); !errors.Is(err, test.target) {
				t.Fatalf("error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestValidateSourceRequiresGraphemeExactSegmentAndRubyRestoration(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*Source)
		target error
	}{
		"segment splits combining cluster": {
			mutate: func(source *Source) {
				segmentation := testSegmentation()
				segmentation.Lines[0].Segments = []Segment{{Text: "か"}, {Text: "\u3099く"}}
				source.Segmentation = &segmentation
			},
			target: ErrInvalidSegmentation,
		},
		"ruby splits combining cluster": {
			mutate: func(source *Source) {
				ruby := testRuby()
				ruby.Lines[0] = []RubySpan{{Text: "か"}, {Text: "\u3099く"}}
				source.Ruby = &ruby
			},
			target: ErrInvalidRuby,
		},
		"romanized reading": {
			mutate: func(source *Source) {
				ruby := testRuby()
				ruby.Lines[0][0].Reading = "gaku"
				source.Ruby = &ruby
			},
			target: ErrInvalidRuby,
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := testSource("source")
			test.mutate(&source)
			if err := ValidateSource(source); !errors.Is(err, test.target) {
				t.Fatalf("error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestPerformerValidationErrorsDoNotEchoSourceValues(t *testing.T) {
	const prohibited = "Mikito-P"

	validSource := testSource("source")
	segmentation := testSegmentation()
	validSource.Segmentation = &segmentation
	validResult, err := Compose(validSource)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func() error{
		"source unknown reference": func() error {
			source := validSource
			unsafe := testSegmentation()
			unsafe.Lines[0].Segments[0].PerformerIDs = []string{prohibited}
			source.Segmentation = &unsafe
			return ValidateSource(source)
		},
		"source duplicate declaration": func() error {
			source := validSource
			unsafe := testSegmentation()
			unsafe.Performers = []Performer{{ID: prohibited, Name: prohibited}, {ID: prohibited, Name: prohibited}}
			source.Segmentation = &unsafe
			return ValidateSource(source)
		},
		"result unknown reference": func() error {
			unsafe := validResult
			unsafe.Lines = append([]ComposedLine{}, validResult.Lines...)
			unsafe.Lines[0].Segments = append([]ComposedSegment{}, validResult.Lines[0].Segments...)
			unsafe.Lines[0].Segments[0].PerformerIDs = []string{prohibited}
			return ValidateResult(unsafe)
		},
		"result duplicate declaration": func() error {
			unsafe := validResult
			unsafe.Performers = []Performer{{ID: prohibited, Name: prohibited}, {ID: prohibited, Name: prohibited}}
			return ValidateResult(unsafe)
		},
	}
	for name, validate := range tests {
		t.Run(name, func(t *testing.T) {
			err := validate()
			if !errors.Is(err, ErrInvalidSegmentation) {
				t.Fatalf("error = %v, want invalid performer segmentation", err)
			}
			if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(prohibited)) {
				t.Fatal("performer validation error echoed prohibited source metadata")
			}
		})
	}
}

func TestComposeOmitsUnknownLatinPerformerLabelAndPreservesExactEnglishLyrics(t *testing.T) {
	const prohibited = "Mikito-P"
	source := testSource("unknown-performer")
	source.VisibleJapanese = []VisibleLine{{Text: "ROCK 'N' ROLL"}, {Text: "VOX AC30w"}}
	source.Segmentation = &Segmentation{
		Performers: []Performer{{ID: "miku", Name: prohibited, Color: "#33CCBB"}},
		Lines: []SegmentedLine{
			{Segments: []Segment{
				{Text: "ROCK ", PerformerIDs: []string{"miku"}},
				{Text: "'N' ROLL", PerformerIDs: []string{"miku"}},
			}, TrailingPerformerIDs: []string{"miku"}},
			{Segments: []Segment{{Text: "VOX AC30w", PerformerIDs: []string{"miku"}}}, TrailingPerformerIDs: []string{"miku"}},
		},
	}
	source.Ruby = &Ruby{
		GeneratorVersion: "fixed-ruby-v1",
		Lines: [][]RubySpan{
			{{Text: "ROCK "}, {Text: "'N' ROLL"}},
			{{Text: "VOX AC30w"}},
		},
	}

	result, err := Compose(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Performers) != 0 || result.Provenance.Segmentation != nil || result.Provenance.Ruby == nil {
		t.Fatalf("unknown performer segmentation survived composition: %+v", result)
	}
	for _, line := range result.Lines {
		if len(line.Segments) != 1 || line.Segments[0].Text != line.Text ||
			len(line.Segments[0].PerformerIDs) != 0 || len(line.TrailingPerformerIDs) != 0 {
			t.Fatalf("unknown performer segmentation was not omitted: %+v", line)
		}
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(body)), strings.ToLower(prohibited)) ||
		!strings.Contains(string(body), "ROCK 'N' ROLL") || !strings.Contains(string(body), "VOX AC30w") {
		t.Fatal("composition leaked an unknown performer or changed legitimate English lyric text")
	}
}

func TestComposeRejectsConflictingAuditedPerformerIdentityWithoutEcho(t *testing.T) {
	source := testSource("conflicting-performer")
	segmentation := testSegmentation()
	segmentation.Performers[0] = Performer{ID: "miku", Name: "Hoshino Ichika", Color: "#33CCBB"}
	source.Segmentation = &segmentation

	_, err := Compose(source)
	if !errors.Is(err, lyricscontract.ErrUnsafePerformerMetadata) {
		t.Fatalf("conflicting performer error=%v", err)
	}
	lower := strings.ToLower(err.Error())
	for _, prohibited := range []string{"miku", "hoshino", "ichika"} {
		if strings.Contains(lower, prohibited) {
			t.Fatal("composition error echoed source performer metadata")
		}
	}
}

func TestComposeRejectsAnnotatedRubyCrossingSegmentationBoundary(t *testing.T) {
	base := testSource("base")
	segmentation := testSegmentation()
	base.Segmentation = &segmentation
	rubySource := testSource("ruby")
	ruby := testRuby()
	ruby.Lines[0] = []RubySpan{{Text: "か\u3099く", Reading: "がく"}}
	rubySource.Ruby = &ruby

	if _, err := Compose(base, rubySource); !errors.Is(err, ErrComponentConflict) {
		t.Fatalf("error = %v, want component conflict", err)
	}
}

func TestComposeSplitsOnlyUnannotatedRubyAtSegmentBoundaries(t *testing.T) {
	base := testSource("base")
	segmentation := testSegmentation()
	base.Segmentation = &segmentation
	rubySource := testSource("ruby")
	ruby := testRuby()
	ruby.Lines[0] = []RubySpan{{Text: "か\u3099く"}}
	rubySource.Ruby = &ruby

	result, err := Compose(base, rubySource)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Lines[0].Segments[0].Ruby; !reflect.DeepEqual(got, []RubySpan{{Text: "か\u3099"}}) {
		t.Fatalf("first split ruby = %+v", got)
	}
	if got := result.Lines[0].Segments[1].Ruby; !reflect.DeepEqual(got, []RubySpan{{Text: "く"}}) {
		t.Fatalf("second split ruby = %+v", got)
	}
}

func TestComposeRejectsDisagreeingComponentDonors(t *testing.T) {
	left := testSource("left")
	leftSegmentation := testSegmentation()
	left.Segmentation = &leftSegmentation
	right := testSource("right")
	rightSegmentation := testSegmentation()
	rightSegmentation.Lines[1].Segments[0].PerformerIDs = []string{}
	right.Segmentation = &rightSegmentation

	if _, err := Compose(testSource("base"), left, right); !errors.Is(err, ErrComponentConflict) {
		t.Fatalf("error = %v, want component conflict", err)
	}
}

func TestComposeKeepsBaseComponentProvenance(t *testing.T) {
	base := testSource("z-base")
	segmentation := testSegmentation()
	base.Segmentation = &segmentation
	supplement := testSource("a-supplement")
	sameSegmentation := testSegmentation()
	supplement.Segmentation = &sameSegmentation

	result, err := Compose(base, supplement)
	if err != nil {
		t.Fatal(err)
	}
	if result.Provenance.Segmentation == nil || result.Provenance.Segmentation.SourceKey != "z-base" {
		t.Fatalf("segmentation provenance = %+v", result.Provenance.Segmentation)
	}
}

func fixedCompositionRevision(versionKind string, texts []string, annotated bool) lyricssource.FixedRevision {
	extraction := lyricssource.Extraction{
		Version: lyricssource.LyricsVersion{Kind: versionKind, Label: versionKind},
		Lines:   make([]lyricssource.StructuredLine, len(texts)),
	}
	if annotated {
		extraction.Performers = []lyricssource.Performer{{PerformerID: "miku", Name: "初音ミク", Color: "#33CCBB"}}
		extraction.RubyGeneratorVersion = "fixed-compose-test-v1"
	}
	for index, text := range texts {
		performerIDs := []string{}
		ruby := []lyricssource.RubySpan{{Text: text}}
		if annotated {
			performerIDs = []string{"miku"}
			ruby[0].Reading = "よみ"
		}
		extraction.Lines[index] = lyricssource.StructuredLine{
			Japanese: text,
			Segments: []lyricssource.LyricsSegment{{
				Text: text, PerformerIDs: performerIDs, Ruby: ruby,
			}},
			TrailingPerformerIDs: append([]string{}, performerIDs...),
		}
	}
	return lyricssource.FixedRevision{
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		Extraction:    extraction,
	}
}

func TestComposeFixedArtifactsKeepsLogicalAndUniqueArtifactKeysSeparate(t *testing.T) {
	full := fixedCompositionRevision("sekai", []string{"歌", "未来"}, false)
	annotations := fixedCompositionRevision("sekai", []string{"歌", "未来"}, true)
	redundant := fixedCompositionRevision("sekai", []string{"歌", "未来"}, false)

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{
		{SourceKey: "z-redundant", LogicalRenditionKey: "full-sekai", Fixed: redundant},
		{SourceKey: "b-annotations", LogicalRenditionKey: "full-sekai", Fixed: annotations},
		{SourceKey: "a-full", LogicalRenditionKey: "full-sekai", Fixed: full},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composition.ReasonCode != model.LyricsSourceVersionReasonUntaggedFullOnly ||
		composition.Components.FullText != "a-full" || composition.Components.VersionEvidence != "a-full" ||
		composition.Components.PerformerSegmentation != "b-annotations" || composition.Components.Ruby != "b-annotations" ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{"a-full", "b-annotations"}) {
		t.Fatalf("fixed artifact composition=%+v", composition)
	}
	if len(composition.Full.Performers) != 1 || composition.Full.RubyGeneratorVersion != "fixed-compose-test-v1" ||
		composition.Full.Lines[0].Segments[0].Ruby[0].Reading != "よみ" {
		t.Fatalf("composed Full=%+v", composition.Full)
	}
}

func TestComposeFixedArtifactsResolvesTaggedGameAgainstVocaloidFull(t *testing.T) {
	vocaloid := fixedCompositionRevision("vocaloid", []string{"一", "二", "三"}, false)
	game := fixedCompositionRevision("sekai", []string{"二"}, false)
	game.VersionReason = model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{
		{SourceKey: "game-evidence", LogicalRenditionKey: "game-sekai", Fixed: game},
		{SourceKey: "vocaloid-full", LogicalRenditionKey: "full-vocaloid", Fixed: vocaloid},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composition.ReasonCode != model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid ||
		composition.Components.FullText != "vocaloid-full" || composition.Components.VersionEvidence != "game-evidence" ||
		composition.Components.GameProjection != "" || composition.GameProjection != nil ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{"game-evidence", "vocaloid-full"}) {
		t.Fatalf("tagged game composition=%+v", composition)
	}
	if got := []string{composition.Full.Lines[0].Text, composition.Full.Lines[1].Text, composition.Full.Lines[2].Text}; !reflect.DeepEqual(got, []string{"一", "二", "三"}) {
		t.Fatalf("resolved Full=%v", got)
	}
}

func TestComposeFixedArtifactsPreservesExplicitGameOnlyWithoutPromotingItToFull(t *testing.T) {
	game := fixedCompositionRevision("sekai", []string{"二"}, false)
	game.VersionReason = model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid
	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{{
		SourceKey: "game-evidence", LogicalRenditionKey: "game-sekai", Fixed: game,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if composition.ReasonCode != model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid ||
		composition.Game == nil || len(composition.Game.Lines) != 1 || composition.Game.Lines[0].ID != "game-000001" ||
		composition.Game.Lines[0].Text != "二" || len(composition.Full.Lines) != 0 || composition.GameProjection != nil ||
		composition.Components.FullText != "" || composition.Components.GameText != "game-evidence" ||
		composition.Components.VersionEvidence != "game-evidence" ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{"game-evidence"}) {
		t.Fatalf("game-only composition=%+v", composition)
	}
}

func TestComposeFixedArtifactsAcceptsAuthoritativeVocaloidFullAndGame(t *testing.T) {
	input := newFixedDocumentInput(fixedDocumentFixture{
		provider:          model.LyricsSourceProviderSekaipedia,
		sourceKey:         "full-and-game-authoritative-vocaloid",
		logicalRendition:  "full-vocaloid",
		revisionTimestamp: "2026-08-01T00:00:00Z",
		version:           model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"},
		reason:            model.LyricsSourceVersionReasonTaggedFullAndGame,
		texts:             []string{"主歌", "副歌", "尾声"},
		gamePositions:     []int{0, 1},
		performerID:       "歌唱者-22",
		performerName:     "鏡音リン",
		reading:           "しゅか",
		privateReview:     true,
	})
	document := *input.Fixed.Document
	game := document.Full
	game.Lines = append([]model.LyricsSourceFullLine(nil), document.Full.Lines[:2]...)
	for index := range game.Lines {
		game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	component := model.LyricsSourceComponentRef{RenditionKey: input.SourceKey}
	document.Game = &game
	document.Provenance.GameText = &component
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("validate authoritative vocaloid Full+Game fixture: %v", err)
	}
	input.Fixed.Document = &document

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if len(composition.Full.Lines) != 3 || composition.Game == nil || len(composition.Game.Lines) != 2 ||
		composition.GameProjection == nil || len(composition.GameProjection.LineIDs) != 2 ||
		composition.Components.FullText != input.SourceKey || composition.Components.GameText != input.SourceKey {
		t.Fatalf("authoritative vocaloid Full+Game composition=%+v", composition)
	}
}

func TestComposeFixedArtifactsAcceptsAuthoritativeVocaloidGameOnly(t *testing.T) {
	input := newFixedDocumentInput(fixedDocumentFixture{
		provider:          model.LyricsSourceProviderSekaipedia,
		sourceKey:         "game-authoritative-vocaloid",
		logicalRendition:  "game-vocaloid",
		revisionTimestamp: "2026-08-01T00:00:00Z",
		version:           model.LyricsSourceVersion{Kind: "vocaloid", Label: "Game Version"},
		texts:             []string{"主歌", "副歌"},
		performerID:       "歌唱者-22",
		performerName:     "鏡音リン",
		reading:           "しゅか",
		privateReview:     true,
	})
	document := *input.Fixed.Document
	document.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnly
	input.Fixed.VersionReason = model.LyricsSourceVersionReasonTaggedGameOnly
	game := document.Full
	for index := range game.Lines {
		game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	component := model.LyricsSourceComponentRef{RenditionKey: input.SourceKey}
	document.Full = model.LyricsSourceFull{}
	document.Game = &game
	document.Provenance.FullText = model.LyricsSourceComponentRef{}
	document.Provenance.GameText = &component
	document.Provenance.PerformerSegmentation = &component
	document.Provenance.Ruby = &component
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("validate authoritative vocaloid Game-only fixture: %v", err)
	}
	input.Fixed.Document = &document

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if composition.Full.Lines != nil || composition.Game == nil || len(composition.Game.Lines) != 2 ||
		composition.Components.FullText != "" || composition.Components.GameText != input.SourceKey ||
		composition.Components.PerformerSegmentation != input.SourceKey || composition.Components.Ruby != input.SourceKey {
		t.Fatalf("authoritative vocaloid Game-only composition=%+v", composition)
	}
}

func TestComposeFixedArtifactsReturnsPluralVersionConflict(t *testing.T) {
	left := fixedCompositionRevision("sekai", []string{"左"}, false)
	right := fixedCompositionRevision("sekai", []string{"右"}, false)
	_, err := ComposeFixedArtifacts([]FixedArtifactInput{
		{SourceKey: "provider-a.full-sekai", LogicalRenditionKey: "full-sekai", Fixed: left},
		{SourceKey: "provider-b.full-sekai", LogicalRenditionKey: "full-sekai", Fixed: right},
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("error=%v, want version conflict", err)
	}
}

type fixedDocumentFixture struct {
	provider          model.LyricsSourceProvider
	sourceKey         string
	logicalRendition  string
	revisionTimestamp string
	fetchedAt         string
	version           model.LyricsSourceVersion
	reason            model.LyricsSourceVersionReasonCode
	texts             []string
	lineIDs           []string
	gamePositions     []int
	performerID       string
	performerName     string
	reading           string
	privateReview     bool
}

func newFixedDocumentInput(fixture fixedDocumentFixture) FixedArtifactInput {
	if fixture.fetchedAt == "" {
		fixture.fetchedAt = "2026-08-20T00:00:00Z"
	}
	if fixture.reason == "" {
		fixture.reason = model.LyricsSourceVersionReasonUntaggedFullOnly
	}
	lineIDs := append([]string{}, fixture.lineIDs...)
	if len(lineIDs) == 0 {
		lineIDs = make([]string, len(fixture.texts))
		for index := range lineIDs {
			lineIDs[index] = fmt.Sprintf("full-%06d", index+1)
		}
	}
	performers := []model.LyricsSourcePerformer{}
	if fixture.performerID != "" {
		name := fixture.performerName
		if name == "" {
			name = fixture.performerID
		}
		performers = []model.LyricsSourcePerformer{{
			PerformerID: fixture.performerID, Name: name, Color: "#33CCBB",
		}}
	}
	lines := make([]model.LyricsSourceFullLine, len(fixture.texts))
	for index, text := range fixture.texts {
		performerIDs := []string{}
		if fixture.performerID != "" {
			performerIDs = []string{fixture.performerID}
		}
		span := model.LyricsSourceRubySpan{Text: text, Reading: fixture.reading}
		lines[index] = model.LyricsSourceFullLine{
			ID: lineIDs[index], Text: text,
			Segments: []model.LyricsSourceSegment{{
				Text: text, PerformerIDs: append([]string{}, performerIDs...),
				Ruby: []model.LyricsSourceRubySpan{span},
			}},
			TrailingPerformerIDs: append([]string{}, performerIDs...),
		}
	}
	full := model.LyricsSourceFull{Version: fixture.version, Performers: performers, Lines: lines}
	if fixture.reading != "" {
		full.RubyGeneratorVersion = "fixed-document-test-v1"
	}
	identity := fixedDocumentIdentity(fixture)
	component := model.LyricsSourceComponentRef{RenditionKey: fixture.sourceKey}
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersion,
		ReasonCode:      fixture.reason,
		FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
		Provenance: model.LyricsSourceComponentProvenance{
			FullText: component, VersionEvidence: component,
		},
		Full: full,
	}
	if fixture.performerID != "" {
		document.Provenance.PerformerSegmentation = &component
	}
	if fixture.reading != "" {
		document.Provenance.Ruby = &component
	}
	if fixture.gamePositions != nil {
		lineIDs := make([]string, len(fixture.gamePositions))
		for index, position := range fixture.gamePositions {
			lineIDs[index] = full.Lines[position].ID
		}
		document.GameProjection = &model.LyricsSourceGameProjection{LineIDs: lineIDs}
		document.Provenance.GameProjection = &component
	}
	if fixture.privateReview {
		document.PrivateReview = &model.LyricsSourcePrivateReview{
			PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
		}
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		panic(fmt.Sprintf("invalid fixed document fixture %q: %v", fixture.sourceKey, err))
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, fixture.fetchedAt)
	if err != nil {
		panic(err)
	}
	return FixedArtifactInput{
		SourceKey: fixture.sourceKey, LogicalRenditionKey: fixture.logicalRendition,
		Fixed: lyricssource.FixedRevision{
			Provider: identity.Provider, Origin: identity.Origin, CanonicalURL: identity.CanonicalURL,
			PageID: identity.PageID, PageTitle: identity.Title, RevisionID: identity.RevisionID,
			RevisionTimestamp: mustParseFixtureTime(fixture.revisionTimestamp), SHA1: identity.SHA1,
			Categories: append([]string{}, identity.Categories...), FetchedAt: fetchedAt,
			Section: identity.Section, RenditionKey: fixture.logicalRendition, VersionReason: fixture.reason,
			IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, identity.IndexEvidenceRefs...),
			FixedIdentities:   []model.LyricsSourceFixedIdentity{identity}, Document: &document,
		},
	}
}

func mustParseFixtureTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func fixedDocumentIdentity(fixture fixedDocumentFixture) model.LyricsSourceFixedIdentity {
	pageID := 100 + len(fixture.sourceKey)
	revisionID := 1000 + len(fixture.sourceKey)
	origin := model.LyricsSourceOriginVocaloidFandom
	switch fixture.provider {
	case model.LyricsSourceProviderMoegirl:
		origin = model.LyricsSourceOriginMoegirl
	case model.LyricsSourceProviderSekaipedia:
		origin = model.LyricsSourceOriginSekaipedia
	}
	return model.LyricsSourceFixedIdentity{
		Provider: fixture.provider, Origin: origin, PageID: pageID, RevisionID: revisionID,
		SHA1: strings.Repeat("a", 40), Title: "Composition fixture",
		CanonicalURL:      origin + "/wiki/Composition_fixture?oldid=" + fmt.Sprint(revisionID),
		RevisionTimestamp: fixture.revisionTimestamp, FetchedAt: fixture.fetchedAt,
		Categories: []string{"Lyrics"}, Section: "Lyrics",
		RenditionKey: fixture.sourceKey, CompositionRenditionKey: fixture.logicalRendition,
		VersionReason: fixture.reason,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:" + fixture.sourceKey, SHA256: strings.Repeat("b", 64),
		}},
	}
}

func TestComposeFixedArtifactsUsesAlignedSekaipediaComponentsByAuthority(t *testing.T) {
	fallback := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-aligned", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fandom SEKAI Version"},
		texts:   []string{"歌う", "未来へ"}, performerID: "miku", reading: "よみ",
	})
	authority := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-aligned", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-03T00:00:00Z", fetchedAt: "2026-08-04T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"歌う", "未来へ"}, performerID: "ichika", reading: "うた",
	})

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{fallback, authority})
	if err != nil {
		t.Fatal(err)
	}
	if composition.Components.FullText != fallback.SourceKey ||
		composition.Components.VersionEvidence != authority.SourceKey ||
		composition.Components.PerformerSegmentation != authority.SourceKey ||
		composition.Components.Ruby != authority.SourceKey || composition.Full.Version.Label != "SEKAI Version" ||
		len(composition.Full.Performers) != 1 || lyricscontract.ValidatePersistedPerformerMetadata(composition.Full) != nil ||
		len(composition.Full.Lines) == 0 || len(composition.Full.Lines[0].Segments) == 0 ||
		len(composition.Full.Lines[0].Segments[0].Ruby) == 0 ||
		composition.Full.Lines[0].Segments[0].Ruby[0].Reading != "うた" {
		t.Fatalf("aligned authority composition=%+v Full=%+v", composition, composition.Full)
	}
}

func TestComposeFixedArtifactsFallsBackWhenSekaipediaIsStaleMissingOrHasNoRendition(t *testing.T) {
	fallback := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-fallback", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-10T00:00:00Z", fetchedAt: "2026-08-11T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fresh fallback"},
		texts:   []string{"新しい歌", "続く歌"}, performerID: "miku", reading: "あたらしい",
	})
	stale := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-stale", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-20T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Stale authority"},
		texts:   []string{"新しい歌", "続く歌"}, performerID: "ichika", reading: "ふるい",
	})
	absentRendition := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-vocaloid-only", logicalRendition: "full-vocaloid",
		revisionTimestamp: "2026-08-12T00:00:00Z", fetchedAt: "2026-08-13T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"},
		texts:   []string{"新しい歌", "続く歌"},
	})
	absentRendition.LogicalRenditionKey = "full-sekai"

	for name, inputs := range map[string][]FixedArtifactInput{
		"missing":          {fallback},
		"stale":            {stale, fallback},
		"absent rendition": {fallback, absentRendition},
	} {
		t.Run(name, func(t *testing.T) {
			composition, err := ComposeFixedArtifacts(inputs)
			if err != nil {
				t.Fatal(err)
			}
			if composition.Components.FullText != fallback.SourceKey ||
				composition.Components.VersionEvidence != fallback.SourceKey ||
				composition.Components.PerformerSegmentation != fallback.SourceKey ||
				composition.Components.Ruby != fallback.SourceKey || composition.Full.Version.Label != "Fresh fallback" ||
				len(composition.Full.Performers) != 1 || lyricscontract.ValidatePersistedPerformerMetadata(composition.Full) != nil {
				t.Fatalf("fallback composition=%+v Full=%+v", composition, composition.Full)
			}
		})
	}
}

func TestComposeFixedArtifactsDoesNotGraftOlderSekaipediaComponentsOntoNewerSekaipediaFull(t *testing.T) {
	newer := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-newer-full", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-10T00:00:00Z", fetchedAt: "2026-08-11T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"新しい歌", "続く歌"},
	})
	older := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-older-components", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"新しい歌", "続く歌"}, performerID: "ichika", reading: "ふるい",
	})
	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{older, newer})
	if err != nil {
		t.Fatal(err)
	}
	if composition.Components.FullText != newer.SourceKey || composition.Components.VersionEvidence != newer.SourceKey ||
		composition.Components.PerformerSegmentation != "" || composition.Components.Ruby != "" ||
		len(composition.Full.Performers) != 0 || composition.Full.RubyGeneratorVersion != "" ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{newer.SourceKey}) {
		t.Fatalf("older Sekaipedia components were grafted onto newer Full: composition=%+v Full=%+v", composition, composition.Full)
	}
}

func TestComposeFixedArtifactsRejectsMismatchedSekaipediaEnrichmentAndUsesAlignedFallback(t *testing.T) {
	text := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-text", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fallback SEKAI"}, texts: []string{"新しい日本語"},
	})
	components := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderMoegirl, sourceKey: "moegirl-components", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-03T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fallback SEKAI"},
		texts:   []string{"新しい日本語"}, performerID: "miku", reading: "あたらしい",
	})
	mismatched := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-old-text", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-04T00:00:00Z", fetchedAt: "2026-08-05T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Wrong text structure"},
		texts:   []string{"古い日本語"}, performerID: "rin", reading: "ふるい",
	})

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{mismatched, components, text})
	if err != nil {
		t.Fatal(err)
	}
	if composition.Components.FullText != text.SourceKey || composition.Components.VersionEvidence != text.SourceKey ||
		composition.Components.PerformerSegmentation != components.SourceKey || composition.Components.Ruby != components.SourceKey ||
		len(composition.Full.Performers) != 1 || lyricscontract.ValidatePersistedPerformerMetadata(composition.Full) != nil ||
		composition.Full.Version.Label != "Fallback SEKAI" ||
		reflect.DeepEqual(composition.SelectedSourceKeys, []string{mismatched.SourceKey}) {
		t.Fatalf("mismatched enrichment was grafted: composition=%+v Full=%+v", composition, composition.Full)
	}
	for _, key := range composition.SelectedSourceKeys {
		if key == mismatched.SourceKey {
			t.Fatalf("mismatched Sekaipedia artifact was selected: %+v", composition.SelectedSourceKeys)
		}
	}
}

func TestComposeFixedArtifactsFailsClosedOnSameTierComponentConflict(t *testing.T) {
	left := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-left", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"同じ歌"}, performerID: "miku",
	})
	right := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderMoegirl, sourceKey: "moegirl-right", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-03T00:00:00Z", fetchedAt: "2026-08-04T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"同じ歌"}, performerID: "rin",
	})

	if _, err := ComposeFixedArtifacts([]FixedArtifactInput{right, left}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same-tier conflict error=%v", err)
	}
}

func TestComposeFixedArtifactsProjectsExplicitGameIDsOntoAuthoritativeFullIDs(t *testing.T) {
	fallback := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-projection-full", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fallback SEKAI"},
		texts:   []string{"同じ", "同じ", "終わり"}, lineIDs: []string{"fandom-1", "fandom-2", "fandom-3"},
	})
	authority := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-explicit-game", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-03T00:00:00Z", fetchedAt: "2026-08-04T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		reason:  model.LyricsSourceVersionReasonTaggedFullAndGame,
		texts:   []string{"同じ", "同じ", "終わり"}, lineIDs: []string{"sekai-1", "sekai-2", "sekai-3"},
		gamePositions: []int{1, 2},
	})

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{authority, fallback})
	if err != nil {
		t.Fatal(err)
	}
	if composition.ReasonCode != model.LyricsSourceVersionReasonTaggedFullAndGame ||
		composition.Components.FullText != fallback.SourceKey || composition.Components.GameProjection != authority.SourceKey ||
		composition.GameProjection == nil ||
		!reflect.DeepEqual(composition.GameProjection.LineIDs, []string{"fandom-2", "fandom-3"}) {
		t.Fatalf("explicit projection composition=%+v Full=%+v", composition, composition.Full)
	}
}

func TestComposeFixedArtifactsPreservesAuthoritativeVirtualSingerMarkerAndFlattensUnprovenVocaloid(t *testing.T) {
	fallback := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-vocaloid-text", logicalRendition: "full-vocaloid",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "vocaloid", Label: "VOCALOID Version"},
		texts:   []string{"初音歌う", "鏡音歌う"},
	})
	authority := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-virtual-singer", logicalRendition: "full-vocaloid",
		revisionTimestamp: "2026-08-03T00:00:00Z", fetchedAt: "2026-08-04T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"},
		texts:   []string{"初音歌う", "鏡音歌う"}, performerID: "miku", reading: "うた", privateReview: true,
	})

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{authority, fallback})
	if err != nil {
		t.Fatal(err)
	}
	if composition.PrivateReview == nil ||
		composition.PrivateReview.PerformerSegmentationEvidence != model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured ||
		composition.Components.PerformerSegmentation != authority.SourceKey || len(composition.Full.Performers) != 1 ||
		composition.Full.Version.Label != "VIRTUAL SINGER Version" {
		t.Fatalf("authoritative VIRTUAL SINGER composition=%+v Full=%+v", composition, composition.Full)
	}
	bound, err := BindFixedArtifactComposition(fallback, composition)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Document == nil || bound.Document.PrivateReview == nil ||
		bound.Document.PrivateReview.PerformerSegmentationEvidence != model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured {
		t.Fatalf("bound private marker=%+v", bound.Document)
	}

	unproven := fixedCompositionRevision("vocaloid", []string{"初音歌う", "鏡音歌う"}, true)
	flattened, err := ComposeFixedArtifacts([]FixedArtifactInput{{
		SourceKey: "unproven-vocaloid", LogicalRenditionKey: "full-vocaloid", Fixed: unproven,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if flattened.PrivateReview != nil || len(flattened.Full.Performers) != 0 {
		t.Fatalf("unproven Vocaloid performers survived: %+v", flattened)
	}
	for _, line := range flattened.Full.Lines {
		if len(line.Segments) != 1 || line.Segments[0].Text != line.Text || len(line.Segments[0].PerformerIDs) != 0 {
			t.Fatalf("unproven Vocaloid line was not flattened: %+v", line)
		}
	}
	if err := model.ValidateLyricsSourceFull(flattened.Full); err != nil {
		t.Fatalf("flattened Vocaloid Full is invalid: %v", err)
	}
}

func TestComposeAndBindPreservesAlternateVocalArtifacts(t *testing.T) {
	input := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "alternate-primary", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-19T00:00:00Z",
		version:           model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"}, texts: []string{"主歌"},
	})
	document := *input.Fixed.Document
	alternateIdentity := document.FixedIdentities[0]
	alternateIdentity.RenditionKey = "alternate-game-vocal"
	alternateIdentity.CompositionRenditionKey = "alternate-vocal"
	alternateIdentity.Section = "Lyrics/Archive/歌唱者-21"
	alternate := model.LyricsSourceAlternateVocal{
		TabLabel: "VBS Archive", SingerLabel: "初音ミク", SingerIDs: []string{"歌唱者-21"},
	}
	alternateGame := model.CloneLyricsSourceFull(&document.Full)
	alternateGame.Version = model.LyricsSourceVersion{Kind: "alternate", Label: "VBS Archive — 初音ミク"}
	for index := range alternateGame.Lines {
		alternateGame.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	alternate.Game = alternateGame
	alternateRef := model.LyricsSourceComponentRef{RenditionKey: alternateIdentity.RenditionKey}
	alternate.Provenance.GameText = &alternateRef
	alternate.Provenance.VersionEvidence = alternateRef
	document.FixedIdentities = append(document.FixedIdentities, alternateIdentity)
	document.AlternateVocals = []model.LyricsSourceAlternateVocal{alternate}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatal(err)
	}
	input.Fixed.Document = &document
	input.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if len(composition.AlternateVocals) != 1 || composition.AlternateVocals[0].Game == nil ||
		composition.Components.AlternateVocals != input.SourceKey {
		t.Fatalf("composition dropped alternate vocal=%+v", composition)
	}
	bound, err := BindFixedArtifactComposition(input, composition)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Document == nil || len(bound.Document.AlternateVocals) != 1 || bound.Document.AlternateVocals[0].Game == nil ||
		bound.Document.AlternateVocals[0].Game.Lines[0].Text != "主歌" {
		t.Fatalf("bound document dropped alternate vocal=%+v", bound.Document)
	}
}

func TestBindFixedArtifactCompositionPropagatesDirectFixedRevisionTimestamp(t *testing.T) {
	primary := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-direct-timestamp", logicalRendition: "full-sekai",
		fetchedAt: "2026-08-10T00:00:00Z", version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts: []string{"時刻を結ぶ"},
	})
	directTimestamp := mustParseFixtureTime("2026-08-09T12:34:56.123456789Z")
	primary.Fixed.RevisionTimestamp = directTimestamp
	primary.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, primary.Fixed.FixedIdentities...)
	primary.Fixed.FixedIdentities[0].RevisionTimestamp = ""
	document := *primary.Fixed.Document
	document.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)
	document.FixedIdentities[0].RevisionTimestamp = ""
	primary.Fixed.Document = &document

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{primary})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindFixedArtifactComposition(primary, composition)
	if err != nil {
		t.Fatal(err)
	}
	want := directTimestamp.UTC().Format(time.RFC3339Nano)
	if bound.Document == nil || len(bound.Document.FixedIdentities) != 1 ||
		bound.Document.FixedIdentities[0].RevisionTimestamp != want {
		t.Fatalf("direct FixedRevision.RevisionTimestamp was not propagated: %+v", bound.Document)
	}
}

func TestComposeFixedArtifactsPersistsAuditedPerformerValuesWithoutChangingLyricsOrAuthority(t *testing.T) {
	input := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-no-romaji", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"歌う", "Jo-jo-jo-journey", "VOX AC30w"}, performerID: "ichika",
		performerName: "Hoshino Ichika", reading: "うた",
	})

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if composition.Components.FullText != input.SourceKey ||
		composition.Components.PerformerSegmentation != input.SourceKey ||
		composition.Components.Ruby != input.SourceKey ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{input.SourceKey}) {
		t.Fatal("performer persistence changed component authority")
	}
	if len(composition.Full.Performers) != 1 ||
		composition.Full.Performers[0] != (model.LyricsSourcePerformer{
			PerformerID: "歌唱者-01", Name: "星乃一歌", Color: "#33CCBB",
		}) {
		t.Fatalf("persisted performer=%+v", composition.Full.Performers)
	}
	if got := []string{
		composition.Full.Lines[0].Text,
		composition.Full.Lines[1].Text,
		composition.Full.Lines[2].Text,
	}; !reflect.DeepEqual(got, []string{"歌う", "Jo-jo-jo-journey", "VOX AC30w"}) {
		t.Fatalf("authoritative lyrics changed: %v", got)
	}
	if composition.Full.Lines[0].Segments[0].Ruby[0].Reading != "うた" {
		t.Fatal("Japanese ruby changed at the performer persistence boundary")
	}
	for _, line := range composition.Full.Lines {
		if !reflect.DeepEqual(line.Segments[0].PerformerIDs, []string{"歌唱者-01"}) ||
			!reflect.DeepEqual(line.TrailingPerformerIDs, []string{"歌唱者-01"}) {
			t.Fatal("performer references were not remapped to the persisted identity")
		}
	}
	if input.Fixed.Document == nil || input.Fixed.Document.Full.Performers[0].PerformerID != "ichika" ||
		input.Fixed.Document.Full.Performers[0].Name != "Hoshino Ichika" {
		t.Fatal("composition mutated the fixed source authority")
	}
	if err := lyricscontract.ValidatePersistedPerformerMetadata(composition.Full); err != nil {
		t.Fatalf("persisted performer metadata rejected: %v", err)
	}
	bound, err := BindFixedArtifactComposition(input, composition)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Document == nil || len(bound.Document.Full.Performers) != 1 ||
		bound.Document.Full.Performers[0].PerformerID != "歌唱者-01" ||
		len(bound.Extraction.Performers) != 1 || bound.Extraction.Performers[0].PerformerID != "歌唱者-01" {
		t.Fatal("bound fixed document or legacy extraction lost the persisted performer identity")
	}
}

func TestComposeFixedArtifactsUsesStableNonLatinIDsAndKeepsAuditedOfficialBrandNames(t *testing.T) {
	for _, test := range []struct {
		name       string
		sourceID   string
		sourceName string
		wantID     string
		wantName   string
	}{
		{name: "romanized audited Japanese identity", sourceID: "miku", sourceName: "Hatsune Miku", wantID: "歌唱者-21", wantName: "初音ミク"},
		{name: "provider-local ID with audited Japanese identity", sourceID: "source-local", sourceName: "Ichika Hoshino", wantID: "歌唱者-01", wantName: "星乃一歌"},
		{name: "simplified Chinese audited identity", sourceID: "镜音铃", sourceName: "镜音铃", wantID: "歌唱者-22", wantName: "鏡音リン"},
		{name: "explicit MEIKO brand", sourceID: "meiko", sourceName: "MEIKO", wantID: "歌唱者-25", wantName: "MEIKO"},
		{name: "explicit KAITO brand", sourceID: "kaito", sourceName: "KAITO", wantID: "歌唱者-26", wantName: "KAITO"},
		{name: "audited external GUMI", sourceID: "gumi", sourceName: "GUMI", wantID: "外部歌唱者-01", wantName: "GUMI"},
		{name: "audited external KAFU source ID", sourceID: "外部歌唱者-07", sourceName: "KAFU", wantID: "外部歌唱者-07", wantName: "KAFU"},
		{name: "audited external Kotonoha Aoi", sourceID: "provider-aoi", sourceName: "琴葉葵", wantID: "外部歌唱者-18", wantName: "Kotonoha Aoi"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := newFixedDocumentInput(fixedDocumentFixture{
				provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-stable-performer", logicalRendition: "full-sekai",
				revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
				version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"}, texts: []string{"歌う"},
				performerID: test.sourceID, performerName: test.sourceName,
			})
			composition, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
			if err != nil {
				t.Fatal(err)
			}
			if len(composition.Full.Performers) != 1 || composition.Full.Performers[0].PerformerID != test.wantID ||
				composition.Full.Performers[0].Name != test.wantName {
				t.Fatalf("audited performer=%+v", composition.Full.Performers)
			}
		})
	}
}

func TestComposeFixedArtifactsOmitsUnknownLatinLabelBeforeCompositionWithoutEcho(t *testing.T) {
	input := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-unknown-performer", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"Jo-jo-jo-journey", "VOX AC30w"}, performerID: "miku", performerName: "Mikito-P",
	})

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if composition.Components.PerformerSegmentation != "" || len(composition.Full.Performers) != 0 ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{input.SourceKey}) {
		t.Fatalf("unknown performer segmentation survived fixed composition: %+v", composition)
	}
	body, err := json.Marshal(composition.Full)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(body)), "mikito") ||
		!strings.Contains(string(body), "Jo-jo-jo-journey") || !strings.Contains(string(body), "VOX AC30w") {
		t.Fatal("fixed composition leaked an unknown performer or changed legitimate English lyrics")
	}
}

func TestComposeFixedArtifactsRejectsConflictingRomanizedPerformerValuesWithoutEcho(t *testing.T) {
	input := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-conflicting-performer", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"}, texts: []string{"歌う"},
		performerID: "miku", performerName: "Hoshino Ichika",
	})
	_, err := ComposeFixedArtifacts([]FixedArtifactInput{input})
	if !errors.Is(err, lyricscontract.ErrUnsafePerformerMetadata) {
		t.Fatalf("conflicting performer error=%v", err)
	}
	lower := strings.ToLower(err.Error())
	for _, prohibited := range []string{"miku", "hoshino", "ichika"} {
		if strings.Contains(lower, prohibited) {
			t.Fatal("performer boundary error echoed prohibited source metadata")
		}
	}
}

func TestComposeFixedArtifactsRedactsRomanizedMetadataFromLegacySegmentationErrors(t *testing.T) {
	legacy := fixedCompositionRevision("sekai", []string{"歌う"}, true)
	legacy.Extraction.Performers = []lyricssource.Performer{
		{PerformerID: "miku", Name: "Hatsune Miku", Color: "#33CCBB"},
		{PerformerID: "miku", Name: "Hatsune Miku", Color: "#33CCBB"},
	}
	_, err := ComposeFixedArtifacts([]FixedArtifactInput{{
		SourceKey: "legacy-duplicate-performer", LogicalRenditionKey: "full-sekai", Fixed: legacy,
	}})
	if !errors.Is(err, ErrInvalidSegmentation) {
		t.Fatalf("legacy segmentation error=%v", err)
	}
	lower := strings.ToLower(err.Error())
	for _, prohibited := range []string{"miku", "hatsune"} {
		if strings.Contains(lower, prohibited) {
			t.Fatal("legacy segmentation error echoed prohibited performer metadata")
		}
	}
}

func TestComposeFixedArtifactsIsIndependentOfInputOrder(t *testing.T) {
	base := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderVocaloidFandom, sourceKey: "fandom-order-text", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-02T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fallback SEKAI"}, texts: []string{"歌う", "続く"},
	})
	fallback := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderMoegirl, sourceKey: "moegirl-order-components", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-01T00:00:00Z", fetchedAt: "2026-08-03T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "Fallback SEKAI"},
		texts:   []string{"歌う", "続く"}, performerID: "miku", reading: "よみ",
	})
	authority := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "sekaipedia-order-authority", logicalRendition: "full-sekai",
		revisionTimestamp: "2026-08-04T00:00:00Z", fetchedAt: "2026-08-05T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		texts:   []string{"歌う", "続く"}, performerID: "ichika", reading: "うた",
	})
	orders := [][]FixedArtifactInput{
		{base, fallback, authority},
		{authority, base, fallback},
		{fallback, authority, base},
	}
	want, err := ComposeFixedArtifacts(orders[0])
	if err != nil {
		t.Fatal(err)
	}
	for index, order := range orders[1:] {
		got, err := ComposeFixedArtifacts(order)
		if err != nil {
			t.Fatalf("order %d: %v", index+2, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("order %d changed composition\nwant=%+v\ngot=%+v", index+2, want, got)
		}
	}
}
