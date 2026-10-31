package lyricssource

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/model"
)

func TestSekaipediaDirectPlainLyricsUseJapaneseOnlyAndGeneratedRuby(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line
| version = SEKAI
| singers = Ichika,Miku
| audio = sample
}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head
| columns = japanese,english
| japanese = Japanese lyrics
| english = English translation
}}
{{Lyrics line
| japanese = {{Color|red|{{ruby|歌|うた}}う}}
| english = ignored translation
}}
{{Lyrics line
| japanese = <big>未来へ</big>
| english = ignored translation
}}
{{Lyrics tail}}
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Section != "Lyrics/Full Version" || parsed.ReasonCode != "untagged_full_only" ||
		len(parsed.Full.Lines) != 2 || len(parsed.Full.Performers) != 0 ||
		parsed.Full.RubyGeneratorVersion != rubyGeneratorVersion || parsed.AuthoritativeStructured {
		t.Fatalf("direct plain extraction=%+v", parsed)
	}
	if parsed.Full.Lines[0].Japanese != "歌う" || parsed.Full.Lines[1].Japanese != "未来へ" ||
		len(parsed.Full.Lines[0].Segments) != 1 || len(parsed.Full.Lines[0].Segments[0].Ruby) == 0 {
		t.Fatalf("direct Japanese-only lines=%+v", parsed.Full.Lines)
	}
}

func TestSekaipediaTaggedJapanesePreservesPerformerSegmentationWhenRomajiCannotAlign(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line
| version = SEKAI
| singers = Yoisaki Kanade, MEIKO
| audio = sample
}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head
| columns = japanese,romaji
| japanese = Japanese lyrics
| romaji = Romanized lyrics
}}
{{Lyrics line
| japanese = {{Lyric|Kanade|歌う}}{{Lyric|MEIKO|未来へ}}
| romaji = {{Lyric|Kanade|not a reading}}{{Lyric|MEIKO|still not a reading}}
}}
{{Lyrics tail}}
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.AuthoritativeStructured || len(parsed.Full.Performers) != 2 || len(parsed.Full.Lines) != 1 ||
		len(parsed.Full.Lines[0].Segments) != 2 || parsed.Full.RubyGeneratorVersion != sekaipediaRubyGeneratorVersion {
		t.Fatalf("tagged Japanese fallback=%+v", parsed)
	}
	segments := parsed.Full.Lines[0].Segments
	if segments[0].Text != "歌う" || !equalStrings(segments[0].PerformerIDs, []string{"歌唱者-17"}) ||
		segments[1].Text != "未来へ" || !equalStrings(segments[1].PerformerIDs, []string{"歌唱者-25"}) ||
		len(segments[0].Ruby) != 2 || segments[0].Ruby[0].Text != "歌" || segments[0].Ruby[0].Reading != "うた" ||
		segments[0].Ruby[1].Text != "う" || segments[0].Ruby[1].Reading != "" ||
		len(segments[1].Ruby) != 2 || segments[1].Ruby[0].Text != "未来" || segments[1].Ruby[0].Reading != "みらい" ||
		segments[1].Ruby[1].Text != "へ" || segments[1].Ruby[1].Reading != "" {
		t.Fatalf("tagged Japanese segments=%+v", segments)
	}
}

func TestSekaipediaSoleInstrumentalVersionWithLyricsBecomesUnsegmentedOriginal(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line
| version = Instrumental
| singers =
| audio = Song707 (Instrumental).flac
| date = 2026/02/21
}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head|columns=japanese,romaji,english|japanese=Japanese lyrics|romaji=Romanized lyrics|english=English translation}}
{{Lyrics line|japanese=歌う|romaji=utau|english=ignored}}
{{Lyrics line|japanese=未来へ|romaji=mirai e|english=ignored}}
{{Lyrics tail}}
`
	for _, policy := range []PerformerSegmentationPolicy{
		PerformerSegmentationDisabled,
		PerformerSegmentationSekaiEligible,
	} {
		parsed, err := parseSekaipediaSong(content, policy)
		if err != nil {
			t.Fatalf("policy %q: %v", policy, err)
		}
		if parsed.Section != "Lyrics/Full Version" || parsed.RenditionKey != "full-original" ||
			parsed.ReasonCode != model.LyricsSourceVersionReasonUntaggedFullOnly ||
			parsed.Full.Version != (LyricsVersion{Kind: "original", Label: "Original Version"}) ||
			parsed.Full.Performers == nil || len(parsed.Full.Performers) != 0 || parsed.AuthoritativeStructured ||
			len(parsed.Full.Lines) != 2 {
			t.Fatalf("policy %q extraction=%+v", policy, parsed)
		}
		for _, line := range parsed.Full.Lines {
			if len(line.Segments) != 1 || len(line.Segments[0].PerformerIDs) != 0 {
				t.Fatalf("policy %q invented performer segmentation: %+v", policy, line)
			}
		}
	}
}

func TestSekaipediaInstrumentalVersionLyricsExceptionFailsClosed(t *testing.T) {
	base := `== Versions ==
{{Song versions head}}
{{Song versions line|version=Instrumental|singers=|audio=sample|date=2026/02/21}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese=歌う}}
{{Lyrics tail}}
`
	for name, content := range map[string]string{
		"nonempty singers": strings.Replace(base, "singers=|audio", "singers=Miku|audio", 1),
		"missing audio":    strings.Replace(base, "audio=sample", "audio=", 1),
		"not sole version": strings.Replace(base, "{{Song versions tail}}",
			"{{Song versions line|version=Connect Live|singers=Miku|audio=sample}}\n{{Song versions tail}}", 1),
		"source singer tag without roster": strings.Replace(base, "japanese=歌う", "japanese={{Lyric|Miku|歌う}}", 1),
		"game tab without original authority": strings.Replace(base,
			"{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}",
			"<tabber>\nFull Version =\n{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}\n|-|\nGame Version =\n{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}\n</tabber>", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSekaipediaSong(content, PerformerSegmentationDisabled); err == nil {
				t.Fatal("unsafe Instrumental-version exception was accepted")
			}
		})
	}
}

func TestSekaipediaPlainVocaloidLyricsAreNotMarkedAuthoritativelySegmented(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=VIRTUAL SINGER|singers=Miku|audio=sample}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head|columns=japanese,romaji,english|japanese=Japanese lyrics|romaji=Romanized lyrics|english=English translation}}
{{Lyrics line|japanese=歌う|romaji=utau|english=ignored}}
{{Lyrics line|japanese=未来へ|romaji=mirai e|english=ignored}}
{{Lyrics tail}}
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationDisabled)
	if err != nil || parsed.Full.Version.Kind != "vocaloid" || parsed.AuthoritativeStructured ||
		len(parsed.Full.Performers) != 0 || len(parsed.Full.Lines) != 2 {
		t.Fatalf("plain Vocaloid extraction=%+v err=%v", parsed, err)
	}
	for _, line := range parsed.Full.Lines {
		if len(line.Segments) != 1 || len(line.Segments[0].PerformerIDs) != 0 {
			t.Fatalf("plain Vocaloid line retained performer segmentation: %+v", line)
		}
	}
}

func TestSekaipediaTaggedVocaloidLyricsPreserveSourceSegmentationForCatalogDisabledPolicy(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=VIRTUAL SINGER|singers=Miku,Rin|audio=sample}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku|歌}}{{Lyric|Rin|う}}}}
{{Lyrics tail}}
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationDisabled)
	if err != nil || parsed.Full.Version.Kind != "vocaloid" || !parsed.AuthoritativeStructured ||
		len(parsed.Full.Performers) != 2 || len(parsed.Full.Lines) != 1 || len(parsed.Full.Lines[0].Segments) != 2 ||
		parsed.Full.Lines[0].Segments[0].Text != "歌" ||
		!equalStrings(parsed.Full.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21"}) ||
		parsed.Full.Lines[0].Segments[1].Text != "う" ||
		!equalStrings(parsed.Full.Lines[0].Segments[1].PerformerIDs, []string{"歌唱者-22"}) {
		t.Fatalf("tagged Vocaloid source segmentation=%+v err=%v", parsed, err)
	}
}

func TestSekaipediaMixedTaggedVocaloidLyricsCarryAuthoritativeStructuredEvidence(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=VIRTUAL SINGER|singers=Miku,Rin|audio=sample}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku|歌}}間{{Lyric|Rin|う}}}}
{{Lyrics tail}}
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Full.Version.Kind != "vocaloid" || parsed.AuthoritativeStructured ||
		parsed.Full.RubyGeneratorVersion != rubyGeneratorVersion || len(parsed.Full.Performers) != 2 ||
		len(parsed.Full.Lines) != 1 || len(parsed.Full.Lines[0].Segments) != 3 ||
		len(parsed.Renditions) != 1 ||
		parsed.Renditions[0].FullStructuredEvidence != sekaipediaPerformerEvidencePartial {
		t.Fatalf("mixed tagged/plain Vocaloid extraction=%+v", parsed)
	}
	segments := parsed.Full.Lines[0].Segments
	if segments[0].Text != "歌" || !equalStrings(segments[0].PerformerIDs, []string{"歌唱者-21"}) ||
		segments[1].Text != "間" || len(segments[1].PerformerIDs) != 0 ||
		segments[2].Text != "う" || !equalStrings(segments[2].PerformerIDs, []string{"歌唱者-22"}) {
		t.Fatalf("mixed tagged/plain Vocaloid segments=%+v", segments)
	}

	const pageID, revisionID = 42, 420
	const title = "Mixed Vocaloid Fixture"
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("a", 40), Title: title,
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, title, revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "authority:sekaipedia:list-of-songs:420", SHA256: strings.Repeat("b", 64),
		}},
	}
	_, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("build mixed tagged/plain Vocaloid document: %v", err)
	}
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 || len(document.Renditions) != 1 {
		t.Fatalf("partial source segmentation document shape=%+v", document)
	}
	rendition := document.Renditions[0]
	if rendition.RenditionKey != "vocaloid" || rendition.ReasonCode != model.LyricsSourceVersionReasonUntaggedFullOnly ||
		rendition.Full == nil || rendition.Game != nil || rendition.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		rendition.PrivateReview != nil || rendition.Provenance.FullPerformerSegmentation == nil ||
		rendition.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceSourcePartial ||
		rendition.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceNone || len(rendition.SourcePerformerIDs) != 0 ||
		len(rendition.Full.Performers) != 2 || len(rendition.Full.Lines) != 1 || rendition.Full.Lines[0].Text != "歌間う" ||
		len(rendition.Full.Lines[0].Segments) != 3 ||
		!equalStrings(rendition.Full.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21"}) ||
		len(rendition.Full.Lines[0].Segments[1].PerformerIDs) != 0 ||
		!equalStrings(rendition.Full.Lines[0].Segments[2].PerformerIDs, []string{"歌唱者-22"}) {
		t.Fatalf("partial source segmentation was not preserved: %+v", document)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("validate mixed tagged/plain Vocaloid document: %v", err)
	}
}

func TestGeneratedRubyFailsClosedWhenHanDictionaryReadingIsUnavailable(t *testing.T) {
	if spans, err := generateRubySpans("々"); !errors.Is(err, ErrUnsupportedTable) || spans != nil {
		t.Fatalf("iteration-mark ruby=%+v err=%v", spans, err)
	}
}

func TestSekaipediaRubyPrefersCompatibleSourceReadingOverDictionary(t *testing.T) {
	spans, ok := deriveSekaipediaRuby("生命へ", "inochi e")
	if !ok || len(spans) != 2 || spans[0].Text != "生命" || spans[0].Reading != "いのち" ||
		spans[0].ReadingEvidenceKind != model.LyricsSourceReadingEvidenceSourceTransliteration ||
		spans[0].GeneratorVersion != "" || spans[1].Text != "へ" || spans[1].Reading != "" {
		t.Fatalf("source-first ruby=%+v ok=%t", spans, ok)
	}
}

func TestRubyContractRejectsReadingThatCoversAnyNonHanCharacter(t *testing.T) {
	for _, test := range []struct {
		text  string
		spans []RubySpan
	}{
		{text: "歌う", spans: []RubySpan{{Text: "歌う", Reading: "うたう"}}},
		{text: "歌!", spans: []RubySpan{{Text: "歌!", Reading: "うた"}}},
		{text: "歌A", spans: []RubySpan{{Text: "歌A", Reading: "うた"}}},
		{text: "歌1", spans: []RubySpan{{Text: "歌1", Reading: "うた"}}},
	} {
		if rubySpansValidForText(test.text, test.spans) {
			t.Fatalf("non-Han received ruby: text=%q spans=%+v", test.text, test.spans)
		}
	}
	if !rubySpansValidForText("歌う", []RubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}}) {
		t.Fatal("exact Han-only ruby plus unannotated kana was rejected")
	}
}

func TestGeneratedRubyAnnotatesKanjiWithoutRepeatingKana(t *testing.T) {
	const text = "正答無くなっちゃって わーんわーんわーん 待って！"
	spans, err := generateRubySpans(text)
	if err != nil {
		t.Fatal(err)
	}
	if rubySpansText(spans) != text || !rubySpansCoverKanji(spans) {
		t.Fatalf("incomplete generated ruby=%+v", spans)
	}
	for _, span := range spans {
		if span.Reading == "" {
			continue
		}
		for _, current := range span.Text {
			if sekaipediaIsKana(current) {
				t.Fatalf("kana received redundant ruby: %+v", spans)
			}
		}
	}
}

func TestSekaipediaTaggedColumnAcceptsClosedSingerAndRubyTemplateForms(t *testing.T) {
	lines, err := parseSekaipediaLyricColumn(
		"{{An|歌う}}{{ruby|空|そら}}{{ruby|{{Lyric|An|心}}|こころ}}{{Lyric|Miku|未来}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || len(lines[0].segments) != 4 || lines[0].segments[0].text != "歌う" ||
		!equalStrings(lines[0].segments[0].performerIDs, []string{"an"}) ||
		lines[0].segments[1].text != "空" || len(lines[0].segments[1].performerIDs) != 0 ||
		lines[0].segments[2].text != "心" || !equalStrings(lines[0].segments[2].performerIDs, []string{"an"}) ||
		lines[0].segments[3].text != "未来" || !equalStrings(lines[0].segments[3].performerIDs, []string{"miku"}) {
		t.Fatalf("closed tagged template forms=%+v", lines)
	}
	compositeRuby, err := parseSekaipediaLyricColumn(
		"{{{{Lyric|Ichika, Shiho, Miku|ruby|心臓|ココロ}}}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "shiho"}},
	)
	if err != nil || len(compositeRuby) != 1 || len(compositeRuby[0].segments) != 1 ||
		compositeRuby[0].segments[0].text != "心臓" ||
		!equalStrings(compositeRuby[0].segments[0].performerIDs, []string{"ichika", "shiho", "miku"}) {
		t.Fatalf("composite ruby Lyric form=%+v err=%v", compositeRuby, err)
	}
	blankRuby, err := parseSekaipediaLyricColumn(
		"前{{ruby|　　|{{Lyric|Kanade|カラ}}}}後",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"kanade"}},
	)
	if err != nil || len(blankRuby) != 1 || len(blankRuby[0].segments) != 2 ||
		blankRuby[0].segments[0].text != "前" || len(blankRuby[0].segments[0].performerIDs) != 0 ||
		blankRuby[0].segments[1].text != "後" || len(blankRuby[0].segments[1].performerIDs) != 0 {
		t.Fatalf("blank-base ruby form=%+v err=%v", blankRuby, err)
	}

	group, err := parseSekaipediaLyricColumn(
		"{{WxS|歌う}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"tsukasa", "emu", "nene", "rui"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(group) != 1 || len(group[0].segments) != 1 ||
		!equalStrings(group[0].segments[0].performerIDs, []string{"tsukasa", "emu", "nene", "rui"}) {
		t.Fatalf("closed group template form=%+v", group)
	}

	for _, source := range []string{
		"<small>{{Lyric|An|歌う}}</small>",
		"<span style=\"margin-left: 160px\">{{Lyric|An|歌う}}</span>",
		"'''<s><big>{{Lyric|An|歌う}}</big></s>'''",
		"''{{Lyric|An|歌う}}''",
		"{{Lyric|An|''歌う''}}",
	} {
		wrapped, err := parseSekaipediaLyricColumn(
			source,
			sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
		)
		if err != nil || len(wrapped) != 1 || len(wrapped[0].segments) != 1 || wrapped[0].segments[0].text != "歌う" {
			t.Fatalf("bounded formatting source=%q wrapper=%+v err=%v", source, wrapped, err)
		}
	}
	crossTemplate, err := parseSekaipediaLyricColumn(
		"{{Lyric|An|''一}}{{Lyric|An|二}}{{Lyric|An|三}}{{Lyric|An|四}}{{Lyric|An|五''}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	)
	if err != nil || len(crossTemplate) != 1 || len(crossTemplate[0].segments) != 5 {
		t.Fatalf("cross-template formatting lines=%+v err=%v", crossTemplate, err)
	}
	for index, want := range []string{"一", "二", "三", "四", "五"} {
		if crossTemplate[0].segments[index].text != want ||
			!equalStrings(crossTemplate[0].segments[index].performerIDs, []string{"an"}) {
			t.Fatalf("cross-template segment %d=%+v", index, crossTemplate[0].segments[index])
		}
	}
	for _, source := range []string{
		"{{Lyric|An|''歌う}}{{Lyric|An|踊る}}",
		"{{Lyric|An|歌''う}}{{Lyric|An|踊る''}}",
	} {
		if _, err := parseSekaipediaLyricColumn(
			source,
			sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
		); !errors.Is(err, ErrUnsupportedTable) {
			t.Fatalf("invalid cross-template formatting source=%q error=%v", source, err)
		}
	}
	if _, err := parseSekaipediaLyricColumn(
		"<small>{{Lyric|An|歌う}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unclosed formatting wrapper error=%v", err)
	}

	for _, source := range []string{
		"{{Lyric|An|前{{Lyric|Miku|中}}後}}",
		"{{Lyric|An|前{{ruby|{{Lyric|Miku|中}}|なか}}後}}",
	} {
		nested, err := parseSekaipediaLyricColumn(
			source,
			sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}},
		)
		if err != nil || len(nested) != 1 || len(nested[0].segments) != 3 ||
			nested[0].segments[0].text != "前" || !equalStrings(nested[0].segments[0].performerIDs, []string{"an"}) ||
			nested[0].segments[1].text != "中" || !equalStrings(nested[0].segments[1].performerIDs, []string{"miku"}) ||
			nested[0].segments[2].text != "後" || !equalStrings(nested[0].segments[2].performerIDs, []string{"an"}) {
			t.Fatalf("nested tagged source=%q lines=%+v err=%v", source, nested, err)
		}
	}
	glossSet := sekaipediaSingerSet{kind: "alternate", ids: []string{"airi"}}
	gloss, err := parseSekaipediaLyricColumn(
		"{{Lyric|Airi|{{ruby|失敗|{{Lyric|Airi|Fail}}}}}}", glossSet,
	)
	if err != nil || len(gloss) != 1 || len(gloss[0].segments) != 1 ||
		gloss[0].segments[0].text != "失敗" || !equalStrings(gloss[0].segments[0].performerIDs, []string{"airi"}) ||
		len(gloss[0].segments[0].ruby) != 0 {
		t.Fatalf("non-kana display gloss=%+v err=%v", gloss, err)
	}
	glossReading, err := parseSekaipediaReadingColumn("{{Lyric|Airi|shippai}}", glossSet)
	if err != nil {
		t.Fatal(err)
	}
	if spans, ok := deriveSekaipediaLocalSegmentRuby(gloss, glossReading, 0, 0); !ok ||
		!rubySpansValidForText("失敗", spans) {
		t.Fatalf("display gloss did not retain local source reading: %+v ok=%t", spans, ok)
	}
	segmentedJapanese := sekaipediaColumnLine{segments: []sekaipediaColumnSegment{
		{text: "14", performerIDs: []string{"airi"}, sourceGroup: 1},
		{text: "听", performerIDs: []string{"airi"}, ruby: []RubySpan{{Text: "听", Reading: "ぽんど"}}, sourceGroup: 1},
		{text: "を嗤う", performerIDs: []string{"airi"}, sourceGroup: 1},
		{text: "蔑奴", performerIDs: []string{"airi"}, ruby: []RubySpan{{Text: "蔑奴", Reading: "べっど"}}, sourceGroup: 1},
	}}
	segmentedReading, err := parseSekaipediaReadingColumn(
		"{{Lyric|Airi|juuyon pondo o warau beddo}}", glossSet,
	)
	if err != nil {
		t.Fatal(err)
	}
	mappedRuby, ok := deriveSekaipediaExactLineSegmentRubies(segmentedJapanese, segmentedReading[0])
	if !ok || len(mappedRuby) != 4 {
		t.Fatalf("exact source-template segment mapping=%+v ok=%t", mappedRuby, ok)
	}
	for index, spans := range mappedRuby {
		if !rubySpansValidForText(segmentedJapanese.segments[index].text, spans) {
			t.Fatalf("exact source-template segment %d=%+v", index+1, spans)
		}
	}
	punctuated, err := parseSekaipediaLyricColumn(
		"♪{{Lyric|An|歌う}}・{{Lyric|Miku|踊る}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}},
	)
	if err != nil || len(punctuated) != 1 || len(punctuated[0].segments) != 2 ||
		punctuated[0].segments[0].text != "♪歌う・" || punctuated[0].segments[1].text != "踊る" {
		t.Fatalf("punctuated tagged lines=%+v err=%v", punctuated, err)
	}
	mixedInline, err := parseSekaipediaLyricColumn(
		"{{Lyric|An|歌う}}間奏{{Lyric|Miku|踊る}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}},
	)
	if err != nil || len(mixedInline) != 1 || len(mixedInline[0].segments) != 3 ||
		mixedInline[0].segments[1].text != "間奏" || len(mixedInline[0].segments[1].performerIDs) != 0 {
		t.Fatalf("mixed inline Japanese lines=%+v err=%v", mixedInline, err)
	}
	mixedMultiline, err := parseSekaipediaLyricColumn(
		"始まり\n続き{{Lyric|An|歌う}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	)
	if err != nil || len(mixedMultiline) != 2 || len(mixedMultiline[0].segments) != 1 ||
		mixedMultiline[0].segments[0].text != "始まり" || len(mixedMultiline[1].segments) != 2 ||
		mixedMultiline[1].segments[0].text != "続き" || len(mixedMultiline[1].segments[0].performerIDs) != 0 ||
		mixedMultiline[1].segments[1].text != "歌う" {
		t.Fatalf("mixed multiline Japanese lines=%+v err=%v", mixedMultiline, err)
	}
	boundedLines, err := parseSekaipediaLyricColumn(
		"{{Lyric|An|一}}\n間{{Lyric|An|二}}\n独立\n{{Lyric|An|三}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	)
	if err != nil || len(boundedLines) != 4 || boundedLines[0].segments[0].text != "一" ||
		len(boundedLines[1].segments) != 2 || boundedLines[1].segments[0].text != "間" ||
		boundedLines[1].segments[1].text != "二" || boundedLines[2].segments[0].text != "独立" ||
		boundedLines[3].segments[0].text != "三" {
		t.Fatalf("bounded Japanese interstitial lines=%+v err=%v", boundedLines, err)
	}
	mixedLatin, err := parseSekaipediaLyricColumn(
		"note {{Lyric|An|歌う}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	)
	if err != nil || len(mixedLatin) != 1 || len(mixedLatin[0].segments) != 2 ||
		mixedLatin[0].segments[0].text != "note" || len(mixedLatin[0].segments[0].performerIDs) != 0 ||
		mixedLatin[0].segments[1].text != "歌う" ||
		!equalStrings(mixedLatin[0].segments[1].performerIDs, []string{"an"}) {
		t.Fatalf("mixed Latin interstitial lines=%+v err=%v", mixedLatin, err)
	}
	mixedMultiline, err = parseSekaipediaLyricColumn(
		"始まり\ninstrumental{{Lyric|An|歌う}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	)
	if err != nil || len(mixedMultiline) != 2 || len(mixedMultiline[0].segments) != 1 ||
		mixedMultiline[0].segments[0].text != "始まり" || len(mixedMultiline[1].segments) != 2 ||
		mixedMultiline[1].segments[0].text != "instrumental" ||
		len(mixedMultiline[1].segments[0].performerIDs) != 0 ||
		mixedMultiline[1].segments[1].text != "歌う" ||
		!equalStrings(mixedMultiline[1].segments[1].performerIDs, []string{"an"}) {
		t.Fatalf("mixed multiline interstitial lines=%+v err=%v", mixedMultiline, err)
	}
	emoticon, err := parseSekaipediaLyricColumn(
		"(ﾉ◕ヮ◕)ﾉ*:･ﾟ✧ <3 {{Lyric|An|歌う}}",
		sekaipediaSingerSet{kind: "sekai", ids: []string{"an"}},
	)
	if err != nil || len(emoticon) != 1 || len(emoticon[0].segments) != 2 ||
		emoticon[0].segments[0].text != "(ﾉ◕ヮ◕)ﾉ*:･ﾟ✧ <3" ||
		len(emoticon[0].segments[0].performerIDs) != 0 || emoticon[0].segments[1].text != "歌う" ||
		!equalStrings(emoticon[0].segments[1].performerIDs, []string{"an"}) {
		t.Fatalf("emoticon interstitial lines=%+v err=%v", emoticon, err)
	}
}

func TestSekaipediaLyricTemplateAcceptsClosedOptionalPerformerForms(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}}
	for _, test := range []struct {
		source string
		ids    []string
	}{
		{source: "{{Lyric|An||歌う}}", ids: []string{"an"}},
		{source: "{{Lyric|An|Miku|歌う}}", ids: []string{"an", "miku"}},
		{source: "{{Lyric|An|backing=Miku|歌う}}", ids: []string{"an", "miku"}},
		{source: "{{Lyric|An|backup=Miku|歌う}}", ids: []string{"an", "miku"}},
		{source: "{{Lyric|歌う}}", ids: []string{}},
	} {
		lines, err := parseSekaipediaLyricColumn(test.source, set)
		if err != nil || len(lines) != 1 || len(lines[0].segments) != 1 || lines[0].segments[0].text != "歌う" ||
			!equalStrings(lines[0].segments[0].performerIDs, test.ids) {
			t.Fatalf("closed Lyric form %q lines=%+v err=%v", test.source, lines, err)
		}
	}
	for _, source := range []string{
		"{{Lyric|An}}",
		"{{Lyric|An|unknown=Miku|歌う}}",
		"{{Lyric|An|Unknown Singer|歌う}}",
		"{{Lyric|An|Miku|Miku|歌う}}",
	} {
		if _, err := parseSekaipediaLyricColumn(source, set); !errors.Is(err, ErrUnsupportedTable) {
			t.Fatalf("invalid Lyric form %q error=%v", source, err)
		}
	}
}

func TestSekaipediaMixedTaggedAndPlainLinesPreserveOnlyWitnessedPerformers(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|An|歌う}}}}\n" +
		"{{Lyrics line|japanese=踊る}}\n" +
		"{{Lyrics tail}}"
	for _, set := range []sekaipediaSingerSet{
		{kind: "sekai", ids: []string{"an"}},
		{kind: "sekai", ids: []string{"an", "miku"}},
	} {
		parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true)
		if err != nil || len(parsed.extraction.Lines) != 2 || len(parsed.extraction.Performers) != 1 ||
			parsed.extraction.Performers[0].Name != "白石杏" ||
			len(parsed.extraction.Lines[0].Segments) != 1 || len(parsed.extraction.Lines[1].Segments) != 1 ||
			len(parsed.extraction.Lines[0].Segments[0].PerformerIDs) != 1 ||
			len(parsed.extraction.Lines[1].Segments[0].PerformerIDs) != 0 ||
			parsed.extraction.RubyGeneratorVersion != rubyGeneratorVersion || parsed.aligned || parsed.japaneseOnly {
			t.Fatalf("mixed tagged/plain rendition set=%v parsed=%+v err=%v", set.ids, parsed, err)
		}
	}
	allTagged := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|An|歌う}}}}\n" +
		"{{Lyrics tail}}"
	parsed, err := parseSekaipediaRenditionWithSet(
		allTagged, "sekai", sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}}, true,
	)
	if err != nil || len(parsed.extraction.Performers) != 1 ||
		parsed.extraction.Performers[0].PerformerID != "歌唱者-10" ||
		len(parsed.extraction.Lines) != 1 || len(parsed.extraction.Lines[0].Segments) != 1 ||
		!equalStrings(parsed.extraction.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-10"}) {
		t.Fatalf("fully tagged partial performer set parsed=%+v err=%v", parsed, err)
	}

	externalDirect := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|Miku|電子の歌}}}}\n" +
		"{{Lyrics tail}}"
	externalParsed, err := parseSekaipediaRenditionWithSet(
		externalDirect, "sekai", sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "shiho"}}, true,
	)
	if err != nil || len(externalParsed.extraction.Performers) != 1 ||
		externalParsed.extraction.Performers[0].PerformerID != "歌唱者-21" ||
		len(externalParsed.extraction.Lines) != 1 || len(externalParsed.extraction.Lines[0].Segments) != 1 ||
		!equalStrings(externalParsed.extraction.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21"}) {
		t.Fatalf("direct singer outside version roster parsed=%+v err=%v", externalParsed, err)
	}

	for _, source := range []string{
		"{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
			"{{Lyrics line|japanese={{Lyric|An|歌う}}}}\n" +
			"{{Lyrics line|japanese={{Lyric|Miku|踊る}}}}\n" +
			"{{Lyrics tail}}",
		"{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
			"{{Lyrics line|japanese={{An|歌う}}}}\n" +
			"{{Lyrics tail}}",
	} {
		parsed, err := parseSekaipediaRenditionWithSet(
			source, "sekai", sekaipediaSingerSet{kind: "sekai", ids: []string{"an", "miku"}}, true,
		)
		if err != nil || len(parsed.extraction.Performers) == 0 {
			t.Fatalf("tagged performer evidence source=%q parsed=%+v err=%v", source, parsed, err)
		}
	}
}

func TestSekaipediaLeadingLyricStubAcceptsFullMarker(t *testing.T) {
	for _, source := range []string{
		"{{Lyric stub|full}}\n{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}",
		"{{Lyric stub|translation}}\n{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}",
		"{{lyric stub|translation|full}}\n{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}",
	} {
		templates, err := parseSekaipediaTemplateSequence(source)
		if err != nil {
			t.Fatalf("source=%q parse templates: %v", source, err)
		}
		stripped, err := stripSekaipediaLeadingLyricStubs(templates)
		if err != nil || len(stripped) != 3 || !strings.EqualFold(stripped[0].name, "Lyrics head") {
			t.Fatalf("source=%q stripped=%+v err=%v", source, stripped, err)
		}
	}
	unknown, err := parseSekaipediaTemplateSequence("{{Lyric stub|translation|unexpected}}\n{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	if _, err := stripSekaipediaLeadingLyricStubs(unknown); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("stub with an unknown marker error=%v", err)
	}
}

// Emerald (song 730) opens its tabbed lyrics with {{lyric stub|translation|full}}; the layout
// parser must skip the combined stub and still reach both tabs.
func TestSekaipediaTabberLayoutSkipsCombinedLyricStub(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}"
	section := "{{lyric stub|translation|full}}\n<tabber>\nGame Version =\n" + body + "\n|-|\nFull Version =\n" + body + "\n</tabber>"
	tabs, _, err := parseSekaipediaLyricsLayout(section)
	if err != nil || tabs["Game Version"] == "" || tabs["Full Version"] == "" {
		t.Fatalf("tabs=%v err=%v", tabs, err)
	}
	unknown := strings.Replace(section, "translation|full", "translation|unexpected", 1)
	if _, _, err := parseSekaipediaLyricsLayout(unknown); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("stub with an unknown marker error=%v", err)
	}
}

func TestSekaipediaEquivalentVersionSetsCollapseOnlyWhenOutputsMatch(t *testing.T) {
	full := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese=始まり}}\n" +
		"{{Lyrics line|japanese=終わり}}\n" +
		"{{Lyrics tail}}"
	game := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese=終わり}}\n" +
		"{{Lyrics tail}}"
	sets := []sekaipediaSingerSet{
		{kind: "sekai", ids: []string{"an"}},
		{kind: "sekai", ids: []string{"miku"}},
	}
	joint, err := parseSekaipediaFullGameAgainstSets(full, game, "sekai", sets)
	if err != nil || len(joint.full.extraction.Performers) != 0 || fmt.Sprint(joint.projection) != "[1]" {
		t.Fatalf("equivalent multi-set Full/Game selection=%+v err=%v", joint, err)
	}

	structured := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|VS|歌う}}}}\n" +
		"{{Lyrics tail}}"
	if _, err := parseSekaipediaRenditionAgainstSets(
		structured,
		"vocaloid",
		[]sekaipediaSingerSet{
			{kind: "vocaloid", ids: []string{"miku"}},
			{kind: "vocaloid", ids: []string{"rin"}},
		},
		true,
	); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("divergent multi-set rendition error=%v", err)
	}
}

func TestSekaipediaMultipleVersionSetsPreferUniqueExplicitSingerCoverage(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|Rin|歌}}{{Lyric|VS|う}}}}\n" +
		"{{Lyrics tail}}"
	parsed, err := parseSekaipediaRenditionAgainstSets(
		body,
		"vocaloid",
		[]sekaipediaSingerSet{
			{kind: "vocaloid", ids: []string{"miku"}},
			{kind: "vocaloid", ids: []string{"miku", "rin", "len", "luka", "meiko", "kaito"}},
		},
		true,
	)
	if err != nil || len(parsed.set.ids) != 6 || len(parsed.extraction.Performers) != 6 ||
		len(parsed.extraction.Lines) != 1 || len(parsed.extraction.Lines[0].Segments) != 2 {
		t.Fatalf("unique explicit singer coverage rendition=%+v err=%v", parsed, err)
	}
}

func TestSekaipediaIgnoresBoundedAlternateTabsAndCitationSuffixes(t *testing.T) {
	full := sekaipediaSyntheticTaggedLyricsBody("Ichika,Miku", "歌う", "utau")
	alternate := sekaipediaSyntheticTaggedLyricsBody("Ichika,Miku", "別歌", "betsu uta")
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=SEKAI|singers=Ichika,Miku|audio=sample|date=2026-01-01}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Full version =
` + full + `<ref name="source">ignored citation</ref>
|-|
Alternate Vocal =
` + alternate + `
</tabber><references />
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Section != "Lyrics/Full Version" || len(parsed.Full.Lines) != 1 || parsed.Full.Lines[0].Japanese != "歌う" {
		t.Fatalf("selected primary tab=%+v", parsed)
	}
	for _, section := range []string{
		"<TABBER>\nFull Version =\n" + full + "\n|-|\nGame Version =\n" + full + "\n</TABBER>",
		"<tabber>Game Version =\n" + full + "\n<references/>\n|-|\nFull Version =\n" + full + "\n<references/></tabber>",
		"<tabber>Game Version =\n" + full + "\n<references>\n<ref name=source>citation</ref>\n</references>\n|-|\nFull Version =\n" + full + "\n<references>\n<ref name=source>citation</ref>\n</references></tabber>",
		"<tabber>\nGame Version =\n" + full + "<!-- editor's note -->\n|-|\nFull Version =\n" + full + "\n</tabber>",
	} {
		tabs, _, err := parseSekaipediaLyricsLayout(section)
		if err != nil || tabs["Full Version"] == "" || tabs["Game Version"] == "" {
			t.Fatalf("tabber boundary variant tabs=%+v err=%v", tabs, err)
		}
	}
	if _, _, err := parseSekaipediaLyricsLayout("metadata<tabber>Full Version =\n" + full + "</tabber>"); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("prefixed tabber error=%v", err)
	}
}

func TestSekaipediaRetainsIndependentFullAndGameWhenProjectionCannotBeProven(t *testing.T) {
	full := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Ichika|完全版}}}}
{{Lyrics line|japanese={{Lyric|Miku|続き}}}}
{{Lyrics tail}}`
	game := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Ichika,Miku|ゲーム版固有}}}}
{{Lyrics tail}}`
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=SEKAI|singers=Ichika,Miku|audio=sekai}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Full Version =
` + full + `
|-|
Game Version =
` + game + `
</tabber>`

	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ReasonCode != model.LyricsSourceVersionReasonTaggedFullAndGame ||
		len(parsed.Full.Lines) != 2 || parsed.Game == nil || len(parsed.Game.Lines) != 1 ||
		len(parsed.GameLineIndexes) != 0 || parsed.Full.Lines[0].Japanese != "完全版" ||
		parsed.Game.Lines[0].Japanese != "ゲーム版固有" {
		t.Fatalf("independent Full/Game extraction=%+v", parsed)
	}

	const pageID, revisionID = 41, 410
	const title = "Independent Full Game Fixture"
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("a", 40), Title: title,
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, title, revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:41:410", SHA256: strings.Repeat("b", 64),
		}},
	}
	identities, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
		len(document.Renditions) != 1 {
		t.Fatalf("independent Full/Game document identities=%+v document=%+v", identities, document)
	}
	rendition := document.Renditions[0]
	if rendition.RenditionKey != "sekai" || rendition.Full == nil || rendition.Game == nil ||
		rendition.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		len(rendition.Full.Lines) != 2 || len(rendition.Game.Lines) != 1 ||
		rendition.Full.Lines[0].Text != "完全版" || rendition.Game.Lines[0].Text != "ゲーム版固有" ||
		rendition.Provenance.FullText == nil || rendition.Provenance.GameText == nil ||
		rendition.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceSourceComplete ||
		rendition.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceSourceComplete {
		t.Fatalf("independent Full/Game rendition=%+v", rendition)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("independent Full/Game document: %v", err)
	}
}

func TestSekaipediaExactProjectionDoesNotTransferGamePerformersToPartialFull(t *testing.T) {
	full := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese=一番}}
{{Lyrics line|japanese={{Lyric|Ichika|二番}}}}
{{Lyrics line|japanese=続き}}
{{Lyrics tail}}`
	game := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku|一番}}}}
{{Lyrics line|japanese={{Lyric|Ichika|二番}}}}
{{Lyrics tail}}`
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=SEKAI|singers=Ichika,Miku|audio=sekai}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Full Version =
` + full + `
|-|
Game Version =
` + game + `
</tabber>`

	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Game == nil || len(parsed.GameLineIndexes) != 2 ||
		len(parsed.Full.Lines) != 3 || len(parsed.Full.Performers) != 1 ||
		len(parsed.Full.Lines[0].Segments[0].PerformerIDs) != 0 ||
		!equalStrings(parsed.Full.Lines[1].Segments[0].PerformerIDs, []string{"歌唱者-01"}) {
		t.Fatalf("partial Full parser boundary=%+v", parsed)
	}

	const pageID, revisionID = 44, 440
	const title = "Projected Partial Full Fixture"
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("a", 40), Title: title,
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, title, revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:44:440", SHA256: strings.Repeat("b", 64),
		}},
	}
	_, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 || len(document.Renditions) != 1 {
		t.Fatalf("projected partial Full document shape=%+v", document)
	}
	rendition := document.Renditions[0]
	if rendition.Full == nil || rendition.Game == nil ||
		rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
		!equalStrings(rendition.Relation.LineIDs, []string{"full-000001", "full-000002"}) ||
		len(rendition.Full.Lines) != 3 || len(rendition.Full.Performers) != 1 ||
		len(rendition.Game.Performers) != 2 || rendition.Full.Lines[0].Text != "一番" ||
		rendition.Full.Lines[1].Text != "二番" || rendition.Full.Lines[2].Text != "続き" ||
		len(rendition.Full.Lines[0].Segments[0].PerformerIDs) != 0 ||
		!equalStrings(rendition.Full.Lines[1].Segments[0].PerformerIDs, []string{"歌唱者-01"}) ||
		len(rendition.Full.Lines[2].Segments[0].PerformerIDs) != 0 ||
		!equalStrings(rendition.Game.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21"}) ||
		rendition.Provenance.FullPerformerSegmentation == nil || rendition.Provenance.GamePerformerSegmentation == nil ||
		rendition.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceSourcePartial ||
		rendition.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceSourcePartial {
		t.Fatalf("projected partial Full rendition=%+v", rendition)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("projected partial Full document: %v", err)
	}
}

func TestSekaipediaExactProjectionKeepsVocaloidFullAnonymousAndGameAttributed(t *testing.T) {
	full := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese=一番}}
{{Lyrics line|japanese=続き}}
{{Lyrics tail}}`
	game := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku|一番}}}}
{{Lyrics tail}}`
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=VIRTUAL SINGER|singers=Miku|audio=virtual}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Full Version =
` + full + `
|-|
Game Version =
` + game + `
</tabber>`

	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AuthoritativeStructured || parsed.Full.Version.Kind != "vocaloid" ||
		len(parsed.Full.Performers) != 0 || parsed.Game == nil || len(parsed.Game.Performers) != 1 ||
		len(parsed.GameLineIndexes) != 1 || parsed.GameLineIndexes[0] != 0 {
		t.Fatalf("projected Vocaloid parser boundary=%+v", parsed)
	}

	const pageID, revisionID = 45, 450
	const title = "Projected Vocaloid Game Fixture"
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("a", 40), Title: title,
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, title, revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:45:450", SHA256: strings.Repeat("b", 64),
		}},
	}
	_, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 || len(document.Renditions) != 1 {
		t.Fatalf("projected Vocaloid document shape=%+v", document)
	}
	rendition := document.Renditions[0]
	if rendition.Full == nil || rendition.Game == nil ||
		rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
		!equalStrings(rendition.Relation.LineIDs, []string{"full-000001"}) ||
		len(rendition.Full.Performers) != 0 || len(rendition.Full.Lines) != 2 ||
		len(rendition.Full.Lines[0].Segments[0].PerformerIDs) != 0 ||
		len(rendition.Full.Lines[1].Segments[0].PerformerIDs) != 0 ||
		len(rendition.Game.Performers) != 1 ||
		!equalStrings(rendition.Game.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-21"}) ||
		rendition.Provenance.FullPerformerSegmentation != nil || rendition.Provenance.GamePerformerSegmentation == nil ||
		rendition.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceNone ||
		rendition.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceSourcePartial {
		t.Fatalf("projected Vocaloid source segmentation=%+v", rendition)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("projected Vocaloid document: %v", err)
	}
}

func TestSekaipediaRetainsAlternateVocalWhenPrimaryGameProjectionIsAmbiguous(t *testing.T) {
	full := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Ichika|同じ}}}}
{{Lyrics line|japanese={{Lyric|Miku|同じ}}}}
{{Lyrics tail}}`
	game := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Ichika,Miku|同じ}}}}
{{Lyrics tail}}`
	alternateGame := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku,Luka|同じ}}}}
{{Lyrics tail}}`
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=SEKAI|singers=Ichika,Miku|audio=sekai}}
{{Song versions line|version=Another Vocal|singers=Miku,Luka|audio=alternate}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Full Version =
` + full + `
|-|
Game Version =
` + game + `
|-|
Alternate Vocal =
{{#tag:tabber|
Miku, Luka =
` + alternateGame + `
}}
</tabber>`

	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ReasonCode != model.LyricsSourceVersionReasonTaggedFullAndGame || parsed.Game == nil ||
		len(parsed.Game.Lines) != 1 || parsed.Game.Lines[0].Japanese != "同じ" || len(parsed.GameLineIndexes) != 0 ||
		len(parsed.AlternateVocals) != 1 || parsed.AlternateVocals[0].Full != nil || parsed.AlternateVocals[0].Game == nil ||
		parsed.AlternateVocals[0].Game.Version.Kind != "alternate" || len(parsed.AlternateVocals[0].Game.Performers) != 2 ||
		!equalStrings(parsed.AlternateVocals[0].SingerIDs, []string{"歌唱者-21", "歌唱者-24"}) {
		t.Fatalf("alternate-vocal extraction=%+v", parsed)
	}

	const pageID, revisionID = 42, 420
	const title = "Alternate Vocal Fixture"
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("a", 40), Title: title,
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, title, revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:42:420", SHA256: strings.Repeat("b", 64),
		}},
	}
	identities, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
		len(document.Renditions) != 2 {
		t.Fatalf("alternate-vocal document identities=%+v document=%+v", identities, document)
	}
	var primary, alternate *model.LyricsSourceRendition
	for index := range document.Renditions {
		rendition := &document.Renditions[index]
		switch rendition.SourceKind {
		case model.LyricsSourceRenditionSekai:
			primary = rendition
		case model.LyricsSourceRenditionAlternate:
			alternate = rendition
		}
	}
	if primary == nil || primary.Full == nil || primary.Game == nil ||
		len(primary.Game.Lines) != 1 || primary.Game.Lines[0].Text != "同じ" ||
		primary.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		alternate == nil || alternate.Full != nil || alternate.Game == nil ||
		alternate.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		alternate.Provenance.GameText == nil || alternate.Provenance.GamePerformerSegmentation == nil ||
		alternate.Provenance.VersionEvidence.RenditionKey != alternate.Provenance.GameText.RenditionKey ||
		alternate.SourceTabPaths[0][0] != "Alternate Vocal" || alternate.SourceTabPaths[0][1] != "Miku, Luka" {
		t.Fatalf("alternate-vocal native renditions=%+v", document.Renditions)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("alternate-vocal document: %v", err)
	}
}

func TestSekaipediaRetainsPairedAlternateVocalFullAndGameRenditions(t *testing.T) {
	full := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Ichika|同じ}}}}
{{Lyrics line|japanese={{Lyric|Miku|同じ}}}}
{{Lyrics tail}}`
	game := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Ichika,Miku|同じ}}}}
{{Lyrics tail}}`
	alternateGame := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku,Luka|同じ}}}}
{{Lyrics tail}}`
	alternateFull := `{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}
{{Lyrics line|japanese={{Lyric|Miku|同じ}}}}
{{Lyrics line|japanese={{Lyric|Luka|同じ}}}}
{{Lyrics tail}}`
	content := `== Versions ==
{{Song versions head}}
{{Song versions line|version=SEKAI|singers=Ichika,Miku|audio=sekai}}
{{Song versions line|version=Another Vocal|singers=Miku,Luka|audio=alternate}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Full Version =
` + full + `
|-|
Game Version =
` + game + `
|-|
Alternate Vocal =
{{#tag:tabber|
Miku, Luka =
` + alternateGame + `
}}
|-|
Alternate Vocal (Full) =
{{#tag:tabber|
Miku, Luka =
` + alternateFull + `
}}
</tabber>`

	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ReasonCode != model.LyricsSourceVersionReasonTaggedFullAndGame || parsed.Game == nil ||
		len(parsed.Game.Lines) != 1 || parsed.Game.Lines[0].Japanese != "同じ" || len(parsed.GameLineIndexes) != 0 ||
		len(parsed.AlternateVocals) != 1 || parsed.AlternateVocals[0].Full == nil || parsed.AlternateVocals[0].Game == nil ||
		len(parsed.AlternateVocals[0].fullProjectionLines) != 2 || len(parsed.AlternateVocals[0].gameProjectionLines) != 1 ||
		parsed.AlternateVocals[0].Full.Version.Kind != "alternate" || parsed.AlternateVocals[0].Game.Version.Kind != "alternate" {
		t.Fatalf("paired alternate-vocal extraction=%+v", parsed)
	}

	const pageID, revisionID = 43, 430
	const title = "Paired Alternate Vocal Fixture"
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("c", 40), Title: title,
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, title, revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:43:430", SHA256: strings.Repeat("d", 64),
		}},
	}
	identities, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
		len(document.Renditions) != 2 {
		t.Fatalf("paired alternate-vocal document identities=%+v document=%+v", identities, document)
	}
	var primary, alternate *model.LyricsSourceRendition
	for index := range document.Renditions {
		rendition := &document.Renditions[index]
		if rendition.SourceKind == model.LyricsSourceRenditionSekai {
			primary = rendition
		} else if rendition.SourceKind == model.LyricsSourceRenditionAlternate {
			alternate = rendition
		}
	}
	if primary == nil || primary.Full == nil || primary.Game == nil ||
		primary.Relation.Kind != model.LyricsSourceRenditionRelationNone || len(primary.Game.Lines) != 1 ||
		alternate == nil || alternate.Full == nil || alternate.Game == nil ||
		alternate.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		alternate.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceSourceComplete ||
		alternate.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceSourceComplete ||
		alternate.Provenance.FullText == nil || alternate.Provenance.GameText == nil ||
		alternate.Provenance.FullText.RenditionKey != alternate.Provenance.GameText.RenditionKey ||
		len(alternate.SourceTabPaths) != 2 {
		t.Fatalf("paired alternate-vocal native renditions=%+v", document.Renditions)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("paired alternate-vocal document: %v", err)
	}

	mutatedAlternateGame := strings.Replace(content, "{{Lyric|Miku,Luka|同じ}}", "{{Lyric|Miku,Luka|不相关}}", 1)
	if mutated, err := parseSekaipediaSong(mutatedAlternateGame, PerformerSegmentationSekaiEligible); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("declared Alternate Game with unresolved Han was not rejected: parsed=%+v err=%v", mutated, err)
	}
}

func TestSekaipediaLyricsHeadAcceptsClosedHeaderVariants(t *testing.T) {
	for _, test := range []struct {
		fields     []string
		wantRomaji bool
	}{
		{fields: []string{"Lyrics head", "columns=japanese,romaji,english", "japanese=Japanese lyric", "romaji=Romanized lyrics", "english=Official English translation"}, wantRomaji: true},
		{fields: []string{"Lyrics head", "columns=japanese,romaji,english", "japanese=Japanese lyrics", "romaji=Romanized lyrics", "english=English lyrics"}, wantRomaji: true},
		{fields: []string{"Lyrics head", "columns=japanese,romaji,english", "japanese=Japanese/German lyrics", "romaji=Romanized lyrics", "english=English translation"}, wantRomaji: true},
		{fields: []string{"Lyrics head", "columns=japanese,english", "english=English translation"}},
		{fields: []string{"Lyrics head", "columns=japanese,english", "english=EnglishTranslation"}},
		{fields: []string{"Lyrics head", "columns=japanese,english", "english=Englishtranslation"}},
		{fields: []string{"Lyrics head", "columns=japanese,romaji,english,english 2", "japanese=Japanese lyrics", "romaji=Romanized lyrics", "english=English translation 1", "english 2=English translation 2"}, wantRomaji: true},
	} {
		head, err := parseSekaipediaLyricsHead(sekaipediaTemplate{name: "Lyrics head", fields: test.fields})
		if err != nil || head.sourceColumn != "japanese" || head.hasRomaji != test.wantRomaji || head.englishSource {
			t.Fatalf("header fields=%v head=%+v wantRomaji=%t err=%v", test.fields, head, test.wantRomaji, err)
		}
	}
	englishOnly := sekaipediaTemplate{name: "Lyrics head", fields: []string{
		"Lyrics head", "columns=english", "english=English lyrics",
	}}
	head, err := parseSekaipediaLyricsHead(englishOnly)
	if err != nil || head.sourceColumn != "english" || head.hasRomaji || !head.englishSource {
		t.Fatalf("English source header=%+v err=%v", head, err)
	}
	translationOnly := sekaipediaTemplate{name: "Lyrics head", fields: []string{
		"Lyrics head", "columns=english", "english=English translation",
	}}
	if _, err := parseSekaipediaLyricsHead(translationOnly); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("translation-only header error=%v", err)
	}
}

func TestSekaipediaEnglishOriginalUsesEnglishAsSourceWithoutRubyReadings(t *testing.T) {
	body := strings.Join([]string{
		"{{Lyrics head|columns=english|english=English lyrics}}",
		"{{Lyrics line|english=ROCK 'N' ROLL}}",
		"{{Lyrics tail}}",
	}, "\n")
	parsed, err := parseSekaipediaRenditionWithSet(
		body, "sekai", sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika"}}, true,
	)
	if err != nil || len(parsed.extraction.Lines) != 1 || parsed.extraction.Lines[0].Japanese != "ROCK 'N' ROLL" {
		t.Fatalf("English source extraction=%+v err=%v", parsed, err)
	}
	line := parsed.extraction.Lines[0]
	if len(line.Segments) != 1 || len(line.Segments[0].Ruby) != 1 ||
		line.Segments[0].Ruby[0].Text != line.Japanese || line.Segments[0].Ruby[0].Reading != "" ||
		parsed.extraction.RubyGeneratorVersion != "" {
		t.Fatalf("English source ruby=%+v version=%q", line.Segments, parsed.extraction.RubyGeneratorVersion)
	}
}

func TestSekaipediaVersionsRetainClosedAuxiliaryLabels(t *testing.T) {
	section := `
{{Song versions head}}
{{Song versions line|version=SEKAI|singers=Ichika,Miku|audio=sample}}
{{Song versions line|version=Connect Live|singers=Ichika,Miku|audio=}}
{{Song versions line|version=April Fools' 2026|singers=Ichika,Miku|audio=sample}}
{{Song versions tail}}
`
	records, err := parseSekaipediaVersions(section)
	if err != nil || len(records) != 3 || records[0].kind != "sekai" ||
		records[1].kind != "alternate" || records[1].label != "Connect Live" ||
		records[2].kind != "alternate" || records[2].label != "April Fools' 2026" {
		t.Fatalf("auxiliary versions records=%+v err=%v", records, err)
	}
	unknown := strings.Replace(section, "Connect Live", "Unreviewed Event Version", 1)
	if _, err := parseSekaipediaVersions(unknown); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unknown auxiliary version error=%v", err)
	}
}

func TestSekaipediaVersionsAcceptExactRenditionTabber(t *testing.T) {
	table := func(version, singers string) string {
		return "{{Song versions head}}\n" +
			"{{Song versions line|version=" + version + "|singers=" + singers + "|audio=sample}}\n" +
			"{{Song versions tail}}"
	}
	section := "<tabber>\nVIRTUAL SINGER=\n" + table("VIRTUAL SINGER", "Miku") +
		"\n|-|\nSEKAI=\n" + table("SEKAI", "Ichika,Miku") +
		"\n|-|\nAnother Vocal=\n" + table("Another Vocal", "Ichika") + "\n</tabber>"
	records, err := parseSekaipediaVersions(section)
	if err != nil || len(records) != 3 || records[0].kind != "vocaloid" ||
		records[1].kind != "sekai" || records[2].kind != "another" {
		t.Fatalf("version tabber records=%+v err=%v", records, err)
	}
	mismatch := strings.Replace(section, "version=VIRTUAL SINGER", "version=SEKAI", 1)
	if _, err := parseSekaipediaVersions(mismatch); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("version tab mismatch error=%v", err)
	}
	missing := strings.Replace(section, "\n|-|\nAnother Vocal=\n"+table("Another Vocal", "Ichika"), "", 1)
	if _, err := parseSekaipediaVersions(missing); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("missing version tab error=%v", err)
	}
}

func TestSekaipediaClosedUnitAggregatesRequireExactVersionSet(t *testing.T) {
	if resolved, err := resolveSekaipediaSingerList(
		"Haurka", sekaipediaSingerSet{kind: "sekai", ids: []string{"haruka"}}, false,
	); err != nil || strings.Join(resolved, ",") != "haruka" {
		t.Fatalf("reviewed Haruka transposition alias=%v err=%v", resolved, err)
	}
	if resolved, err := resolveSekaipediaSingerList(
		"Rui,Rui", sekaipediaSingerSet{kind: "sekai", ids: []string{"rui"}}, true,
	); err != nil || strings.Join(resolved, ",") != "rui" {
		t.Fatalf("exact repeated singer alias=%v err=%v", resolved, err)
	}
	if _, err := resolveSekaipediaSingerList(
		"Miku,Hatsune Miku", sekaipediaSingerSet{kind: "sekai", ids: []string{"miku"}}, true,
	); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("equivalent repeated singer alias error=%v", err)
	}
	set := sekaipediaSingerSet{
		kind: "sekai", ids: []string{"tsukasa", "emu", "nene", "rui", "miku"},
	}
	resolved, err := resolveSekaipediaSingerList("WxS,Miku", set, true)
	if err != nil || strings.Join(resolved, ",") != "tsukasa,emu,nene,rui,miku" {
		t.Fatalf("WxS aggregate=%v err=%v", resolved, err)
	}
	all, err := resolveSekaipediaSingerList("All", set, true)
	if err != nil || strings.Join(all, ",") != "tsukasa,emu,nene,rui,miku" {
		t.Fatalf("All aggregate=%v err=%v", all, err)
	}
	wrongSet := sekaipediaSingerSet{
		kind: "sekai", ids: []string{"ichika", "saki", "honami", "shiho", "miku"},
	}
	if _, err := resolveSekaipediaSingerList("WxS,Miku", wrongSet, true); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("cross-unit aggregate error=%v", err)
	}
	if _, err := resolveSekaipediaSingerList("SEKAI", sekaipediaSingerSet{kind: "vocaloid", ids: []string{"miku"}}, true); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unlisted SEKAI voice error=%v", err)
	}
	externalSet := sekaipediaSingerSet{kind: "vocaloid", ids: []string{"gumi", "teto", "sekai_voice"}}
	external, err := resolveSekaipediaSingerList("GUMI,Teto,SEKAI", externalSet, true)
	if err != nil || strings.Join(external, ",") != "gumi,teto,sekai_voice" {
		t.Fatalf("external source legend=%v err=%v", external, err)
	}
	performers := sekaipediaPerformers(external)
	if len(performers) != 3 || performers[0].PerformerID != "外部歌唱者-01" || performers[1].PerformerID != "外部歌唱者-02" {
		t.Fatalf("external persisted legend=%+v", performers)
	}
	mixedVSSet := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "miku", "rin"}}
	if resolved, err := resolveSekaipediaSingerList("VS", mixedVSSet, true); err != nil || strings.Join(resolved, ",") != "miku,rin" {
		t.Fatalf("mixed-set VS aggregate=%v err=%v", resolved, err)
	}
	maleSet := sekaipediaSingerSet{
		kind: "sekai", ids: []string{"akito", "toya", "tsukasa", "rui", "len", "kaito"},
	}
	if resolved, err := resolveSekaipediaSingerList("MaleOCs,Len,KAITO", maleSet, true); err != nil || len(resolved) != 6 {
		t.Fatalf("MaleOCs aggregate=%v err=%v", resolved, err)
	}
	enstarsSet := sekaipediaSingerSet{
		kind: "sekai", ids: []string{"akito", "toya", "tsukasa", "rui", "sazanami_jun", "sena_izumi", "morisawa_chiaki", "sakasaki_natsume"},
	}
	if resolved, err := resolveSekaipediaSingerList("Enstars", enstarsSet, true); err != nil || len(resolved) != 4 {
		t.Fatalf("Enstars aggregate=%v err=%v", resolved, err)
	}
	combinedSet := sekaipediaSingerSet{
		kind: "sekai",
		ids: []string{
			"akito", "toya", "tsukasa", "rui",
			"sazanami_jun", "sena_izumi", "morisawa_chiaki", "sakasaki_natsume",
		},
	}
	if resolved, err := resolveSekaipediaSingerList("MaleOCs, Enstars", combinedSet, true); err != nil || len(resolved) != 8 {
		t.Fatalf("combined closed aggregates=%v err=%v", resolved, err)
	}
}

func TestSekaipediaTaggedLyricsRenderOnlyWhitelistedInlineMarkup(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika"}}
	parsed, err := parseSekaipediaLyricColumn(
		"{{Lyric|Ichika|{{ruby|歌|うた}}う}}{{Lyric|Ichika|<big>未来</big>}}",
		set,
	)
	if err != nil || len(parsed) != 1 || len(parsed[0].segments) != 3 ||
		parsed[0].segments[0].text != "歌" || parsed[0].segments[1].text != "う" ||
		parsed[0].segments[2].text != "未来" ||
		parsed[0].segments[0].sourceGroup != 1 || parsed[0].segments[0].sourceSegmentOrdinal != 1 ||
		parsed[0].segments[1].sourceGroup != 1 || parsed[0].segments[1].sourceSegmentOrdinal != 2 ||
		len(parsed[0].segments[0].ruby) != 1 || parsed[0].segments[0].ruby[0].Reading != "うた" ||
		parsed[0].segments[0].ruby[0].ReadingEvidenceKind != model.LyricsSourceReadingEvidenceExplicitSourceKana {
		t.Fatalf("whitelisted tagged markup=%+v err=%v", parsed, err)
	}
	if _, err := parseSekaipediaLyricColumn("{{Lyric|Ichika|{{Unknown|歌う}}}}", set); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unknown tagged markup error=%v", err)
	}
	if _, err := parseSekaipediaLyricColumn("{{Lyric|Ichika|[[歌う]]}}", set); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("tagged wikilink error=%v", err)
	}
}

func TestSekaipediaPlainLyricsRejectUnknownMarkup(t *testing.T) {
	for _, value := range []string{
		"{{Unknown|歌う}}",
		"<span>歌う</span>",
		"[[歌う]]",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseSekaipediaPlainLyricColumn(value); !errors.Is(err, ErrUnsupportedTable) {
				t.Fatalf("unknown plain markup error=%v", err)
			}
		})
	}
}

func TestSekaipediaPlainLyricRendererIgnoresBoundedInlineCitations(t *testing.T) {
	for _, test := range []struct {
		source string
		want   string
	}{
		{source: "歌<ref name=source>citation</ref>う", want: "歌う"},
		{source: "歌<!-- reviewed -->う", want: "歌う"},
		{source: "歌<br>う", want: "歌う"},
		{source: "歌<nowiki>{{literal}}</nowiki>う", want: "歌{{literal}}う"},
	} {
		rendered, err := renderSekaipediaPlainLyricText(test.source, 0)
		if err != nil || rendered != test.want {
			t.Fatalf("inline metadata source=%q rendered=%q want=%q err=%v", test.source, rendered, test.want, err)
		}
	}
	if _, err := renderSekaipediaPlainLyricText("歌<unknown>metadata</unknown>う", 0); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unknown inline HTML error=%v", err)
	}
}

func TestSekaipediaTaggedColumnIgnoresCitationOnlyInterstitials(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "miku"}}
	lines, err := parseSekaipediaLyricColumn(
		"{{Lyric|Ichika|歌}}<ref name=source>ignored {{Ruby|注|ちゅう}}</ref>{{Lyric|Miku|う}}",
		set,
	)
	if err != nil || len(lines) != 1 || len(lines[0].segments) != 2 ||
		lines[0].segments[0].text != "歌" || lines[0].segments[1].text != "う" ||
		!equalStrings(lines[0].segments[0].performerIDs, []string{"ichika"}) ||
		!equalStrings(lines[0].segments[1].performerIDs, []string{"miku"}) {
		t.Fatalf("citation-only tagged interstitial lines=%+v err=%v", lines, err)
	}
	if _, err := parseSekaipediaLyricColumn(
		"{{Lyric|Ichika|歌}}<unknown>metadata</unknown>{{Lyric|Miku|う}}", set,
	); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unknown tagged interstitial HTML error=%v", err)
	}
}

func TestSekaipediaPlainColumnAcceptsClosedMultilineFormatting(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   []string
	}{
		{name: "italic wrapper", source: "''歌う\n踊る''", want: []string{"歌う", "踊る"}},
		{name: "combined bold italic across lines", source: "'''''歌う\n踊る'''''\n終わり", want: []string{"歌う", "踊る", "終わり"}},
		{name: "Color wrapper", source: "{{Color|#fff|\n歌う\n踊る\n}}", want: []string{"歌う", "踊る"}},
		{name: "literal less-than", source: "3 < 4", want: []string{"3 < 4"}},
		{name: "single ruby template", source: "{{ruby|歌|うた}}", want: []string{"歌"}},
		{name: "nowiki literal template", source: "<nowiki>{{literal}}</nowiki>", want: []string{"{{literal}}"}},
		{name: "multiline nowiki wrapper", source: "<nowiki>{{literal}}\n<3</nowiki>", want: []string{"{{literal}}", "<3"}},
		{name: "partial multiline nowiki", source: "前<nowiki>{{literal}}\n<3</nowiki>後", want: []string{"前{{literal}}", "<3後"}},
		{name: "color span wrapper", source: "<span style=\"color:#a83529\">歌う\n踊る</span>", want: []string{"歌う", "踊る"}},
		{name: "margin span wrapper", source: "<span style=\"margin-left: 160px\">歌う\n踊る</span>", want: []string{"歌う", "踊る"}},
		{name: "strike big bold wrapper", source: "'''<s><big>歌う\n踊る</big></s>'''", want: []string{"歌う", "踊る"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines, err := parseSekaipediaPlainLyricColumn(test.source)
			if err != nil || len(lines) != len(test.want) {
				t.Fatalf("plain lines=%+v err=%v", lines, err)
			}
			for index, want := range test.want {
				if len(lines[index].segments) != 1 || lines[index].segments[0].text != want {
					t.Fatalf("line %d=%+v want=%q", index+1, lines[index], want)
				}
			}
		})
	}
	for _, invalid := range []string{
		"''歌う\n踊る",
		"{{Color||\n歌う\n}}",
		"{{Color|#fff|\n歌う\n}",
		"<span class=lyrics>歌う</span>",
		"<span style=\"margin-left: 5000px\">歌う</span>",
		"<s>歌う",
	} {
		if _, err := parseSekaipediaPlainLyricColumn(invalid); !errors.Is(err, ErrUnsupportedTable) {
			t.Fatalf("invalid multiline formatting %q error=%v", invalid, err)
		}
	}
}

func TestSekaipediaTemplateParsingIgnoresOpaqueMetadataBraces(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika"}}
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|Ichika|歌<!-- ignored }} {{Lyric|Miku|example}} -->う}}}}\n" +
		"{{Lyrics tail}}"
	parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true)
	if err != nil || len(parsed.extraction.Lines) != 1 || parsed.extraction.Lines[0].Japanese != "歌う" {
		t.Fatalf("opaque metadata rendition=%+v err=%v", parsed, err)
	}
	for _, value := range []string{
		"<!-- {{Lyric|Miku|example}} -->",
		"<nowiki>{{Lyric|Miku|literal}}</nowiki>",
	} {
		if _, _, _, found := findBalancedSekaipediaNamedTemplate(value, "Lyric"); found {
			t.Fatalf("opaque metadata exposed a nested Lyric template: %q", value)
		}
	}
	if _, _, _, ok := balancedSekaipediaTemplateAt("{{Lyric|Ichika|歌<!-- unclosed}}", 0); ok {
		t.Fatal("unclosed metadata comment was accepted")
	}
}

func TestSekaipediaTopLevelSectionsIgnoreHeadingsInsideMetadataComments(t *testing.T) {
	content := `== Lyrics ==
{{Lyric stub|translation}}
{{Lyrics head|columns=japanese,romaji,english|japanese=Japanese lyrics|romaji=Romanized lyrics|english=English translation}}
{{Lyrics line|japanese=歌う|romaji=utau|english=}}
{{Lyrics tail}}
== Versions ==
{{Song versions head}}
{{Song versions line|version=VIRTUAL SINGER|singers=Kasane Teto|audio=sample}}
{{Song versions tail}}
<!--
== Charts ==
<gallery>commented metadata</gallery>
-->
== Update history ==
* Added.
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationDisabled)
	if err != nil || parsed.Full.Version.Kind != "vocaloid" || len(parsed.Full.Lines) != 1 ||
		parsed.Full.Lines[0].Japanese != "歌う" || len(parsed.Full.Lines[0].Segments) != 1 ||
		len(parsed.Full.Lines[0].Segments[0].Ruby) == 0 {
		t.Fatalf("commented heading extraction=%+v err=%v", parsed, err)
	}
	if section, err := sekaipediaTopLevelSection(content, "Versions"); err != nil ||
		!strings.Contains(section, "commented metadata") || strings.Contains(section, "Update history") {
		t.Fatalf("active Versions section=%q err=%v", section, err)
	}
	if _, err := sekaipediaTopLevelSection("== Lyrics ==\n歌う\n<!-- unclosed", "Lyrics"); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("unclosed metadata comment error=%v", err)
	}
}

func TestSekaipediaDirectLayoutAcceptsCaseVariantsAndLeadingStub(t *testing.T) {
	body := "{{LYRICS HEAD|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese=歌う}}\n{{Lyrics tail}}"
	for _, section := range []string{body, "{{Lyric stub}}\n" + body, "{{Lyric stub|translation}}\n" + body} {
		tabs, _, err := parseSekaipediaLyricsLayout(section)
		if err != nil || tabs["Full Version"] == "" {
			t.Fatalf("direct layout tabs=%+v err=%v", tabs, err)
		}
	}
	for _, note := range []string{
		sekaipediaSameLyricsNote,
		sekaipediaSameLyricsHyphenatedNote,
		sekaipediaSameDurationAndLyricsNote,
	} {
		tabs, sameLyrics, err := parseSekaipediaLyricsLayout(note + body)
		if err != nil || !sameLyrics || tabs["Full Version"] == "" {
			t.Fatalf("same-lyrics note=%q tabs=%+v same=%t err=%v", note, tabs, sameLyrics, err)
		}
	}
	if _, _, err := parseSekaipediaLyricsLayout(
		"''The game-cut and full versions contains the same lyrics.''\n" + body,
	); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unknown same-lyrics note error=%v", err)
	}
	if _, _, err := parseSekaipediaLyricsLayout("{{Lyric stub}}"); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("stub-only layout error=%v", err)
	}
	fullStubTabs, sameLyrics, err := parseSekaipediaLyricsLayout("{{Lyric stub|full}}\n" + body)
	if err != nil || sameLyrics || fullStubTabs["Full Version"] == "" {
		t.Fatalf("full-stub layout tabs=%+v same=%t err=%v", fullStubTabs, sameLyrics, err)
	}
}

func TestSekaipediaTemplateSequenceRejectsNonCitationSuffix(t *testing.T) {
	head := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}"
	line := "{{Lyrics line|japanese={{Lyric|Ichika|歌う}}}}"
	tail := "{{Lyrics tail}}"
	valid := head + "\n" + line + "\n" + tail
	if _, err := parseSekaipediaTemplateSequence(valid + "<ref>citation</ref>"); err != nil {
		t.Fatalf("citation suffix rejected: %v", err)
	}
	if _, err := parseSekaipediaTemplateSequence(valid + "}"); err != nil {
		t.Fatalf("single stray brace after Lyrics tail rejected: %v", err)
	}
	if _, err := parseSekaipediaTemplateSequence(head + "\n" + line + "}\n" + tail); err != nil {
		t.Fatalf("single stray brace after Lyrics line rejected: %v", err)
	}
	if _, err := parseSekaipediaTemplateSequence(valid + "}}"); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("multiple stray braces error=%v", err)
	}
	interstitial := head + "<ref name=source>citation</ref>\n<references />\n" + line + "<!-- reviewed -->\n" + tail
	if templates, err := parseSekaipediaTemplateSequence(interstitial); err != nil || len(templates) != 3 {
		t.Fatalf("interstitial citation templates=%+v err=%v", templates, err)
	}
	unicodeWhitespace := head + "\u3000" + line + "\u00a0" + tail
	if templates, err := parseSekaipediaTemplateSequence(unicodeWhitespace); err != nil || len(templates) != 3 {
		t.Fatalf("Unicode whitespace templates=%+v err=%v", templates, err)
	}
	if _, err := parseSekaipediaTemplateSequence(valid + "unreviewed trailing text"); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("arbitrary suffix error=%v", err)
	}
}

func TestSekaipediaKnownTemplateNamesAreCaseInsensitive(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "miku"}}
	body := sekaipediaSyntheticTaggedLyricsBody("Ichika,Miku", "歌う", "utau")
	body = strings.NewReplacer(
		"Lyrics head", "lyrics head",
		"Lyrics line", "lyrics line",
		"Lyrics tail", "lyrics tail",
	).Replace(body)
	if parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true); err != nil || len(parsed.extraction.Lines) != 1 {
		t.Fatalf("case-insensitive known templates=%+v err=%v", parsed, err)
	}
}

func TestSekaipediaNestedTagTabberUsesEscapedPipeSeparator(t *testing.T) {
	sekai := sekaipediaSyntheticTaggedLyricsBody("Ichika,Miku", "歌う", "utau")
	virtual := sekaipediaSyntheticTaggedLyricsBody("Miku", "歌う", "utau")
	body := "{{#tag:tabber|\nSEKAI =\n" + sekai + "\n{{!}}-{{!}}\nVIRTUAL SINGER =\n" + virtual + "\n}}"
	selected, err := unwrapSekaipediaNestedRendition(body, "sekai")
	if err != nil || strings.TrimSpace(selected) != strings.TrimSpace(sekai) {
		t.Fatalf("escaped nested SEKAI selection error=%v", err)
	}
	selected, err = unwrapSekaipediaNestedRendition(body, "vocaloid")
	if err != nil || strings.TrimSpace(selected) != strings.TrimSpace(virtual) {
		t.Fatalf("escaped nested VIRTUAL SINGER selection error=%v", err)
	}
	inlineSecondLabel := strings.Replace(body, "VIRTUAL SINGER =\n", "VIRTUAL SINGER =", 1)
	selected, err = unwrapSekaipediaNestedRendition(inlineSecondLabel, "vocaloid")
	if err != nil || strings.TrimSpace(selected) != strings.TrimSpace(virtual) {
		t.Fatalf("inline escaped nested VIRTUAL SINGER selection error=%v", err)
	}
	mixed := strings.Replace(body, "{{!}}-{{!}}", "|-|\nSEKAI =\n"+sekai+"\n{{!}}-{{!}}", 1)
	if _, err := unwrapSekaipediaNestedRendition(mixed, "sekai"); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("mixed nested separators error=%v", err)
	}
}

func TestSekaipediaRetainsVBSArchiveAndGameOnlyRenditions(t *testing.T) {
	game := sekaipediaSyntheticTaggedLyricsBody("Miku", "主歌", "shuka")
	archive := sekaipediaSyntheticTaggedLyricsBody("An,Akito", "主歌", "shuka")
	content := `== Versions ==
{{Song versions head}}
{{Song versions line
| version = VIRTUAL SINGER
| singers = Hatsune Miku
| audio = sample
}}
{{Song versions tail}}
== Lyrics ==
<tabber>
Game Version=
` + game + `
|-|
VBS Archive (An, Akito) Version=
` + archive + `
</tabber>`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatalf("game-only VBS Archive parse error: %v", err)
	}
	if parsed.ReasonCode != model.LyricsSourceVersionReasonTaggedGameOnly ||
		parsed.Section != "Lyrics/Game Version" || parsed.Game == nil || len(parsed.Game.Lines) != 1 ||
		parsed.Game.Lines[0].Japanese != "主歌" {
		t.Fatalf("game-only extraction=%+v", parsed)
	}
	if len(parsed.AlternateVocals) != 1 {
		t.Fatalf("alternate count=%d", len(parsed.AlternateVocals))
	}
	alternate := parsed.AlternateVocals[0]
	if alternate.TabLabel != "VBS Archive (An, Akito) Version" || alternate.SingerLabel != "An, Akito" || alternate.Game == nil || len(alternate.SingerIDs) != 2 {
		t.Fatalf("archive alternate=%+v", alternate)
	}

	const pageID, revisionID = 44, 440
	candidate := Candidate{
		Provider: ProviderSekaipedia, Origin: OriginSekaipedia,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: "2026-08-04T08:00:00Z",
		SHA1: strings.Repeat("e", 40), Title: "VBS Archive Game-only Fixture",
		CanonicalURL: canonicalRevisionURL(ProviderSekaipedia, "VBS Archive Game-only Fixture", revisionID),
		Categories:   []string{"Lyrics"},
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:44:440", SHA256: strings.Repeat("f", 64),
		}},
	}
	identities, document, err := buildSekaipediaDocument(
		candidate, parsed, time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("build game-only VBS Archive document: %v", err)
	}
	if len(identities) != 1 || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
		len(document.Renditions) != 2 {
		t.Fatalf("game-only VBS Archive document identities=%+v document=%+v", identities, document)
	}
	var primary, archiveRendition *model.LyricsSourceRendition
	for index := range document.Renditions {
		rendition := &document.Renditions[index]
		if rendition.SourceKind == model.LyricsSourceRenditionVocaloid {
			primary = rendition
		} else if rendition.SourceKind == model.LyricsSourceRenditionAlternate {
			archiveRendition = rendition
		}
	}
	if primary == nil || primary.Full != nil || primary.Game == nil || len(primary.Game.Lines) != 1 ||
		primary.Game.Lines[0].Text != "主歌" || primary.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		primary.Provenance.FullText != nil || primary.Provenance.GameText == nil ||
		archiveRendition == nil || archiveRendition.Full != nil || archiveRendition.Game == nil ||
		archiveRendition.SourceTabPaths[0][0] != "VBS Archive (An, Akito) Version" ||
		archiveRendition.Relation.Kind != model.LyricsSourceRenditionRelationNone {
		t.Fatalf("game-only VBS Archive native renditions=%+v", document.Renditions)
	}
	body, err := document.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"full":`) || !strings.Contains(string(body), `"game":`) {
		t.Fatalf("game-only document JSON boundary synthesized Full: %s", body)
	}
	if err := model.ValidateLyricsSourceDocument(*document); err != nil {
		t.Fatalf("game-only VBS Archive document: %v", err)
	}
}

func TestSekaipediaRetainsAuxiliaryRenditionInsideGameTab(t *testing.T) {
	primary := sekaipediaSyntheticTaggedLyricsBody("Miku", "主歌", "shuka")
	auxiliary := sekaipediaSyntheticTaggedLyricsBody("Rin,Len", "副歌", "fuka")
	nested := "{{#tag:tabber|\nVIRTUAL SINGER =\n" + primary + "\n{{!}}-{{!}}\nCOLORFUL LIVE =\n" + auxiliary + "\n}}"
	tabs, _, err := parseSekaipediaLyricsLayout("<tabber>\nGame Version=\n" + nested + "\n</tabber>")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := parseSekaipediaVersions(`{{Song versions head}}
{{Song versions line|version=VIRTUAL SINGER|singers=Hatsune Miku|audio=sample}}
{{Song versions line|version=COLORFUL LIVE|singers=Rin,Len|audio=sample}}
{{Song versions tail}}`)
	if err != nil {
		t.Fatal(err)
	}
	alternates, err := parseSekaipediaAlternateVocals(tabs, versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(alternates) != 1 || alternates[0].TabLabel != "COLORFUL LIVE" || alternates[0].Game == nil || len(alternates[0].SingerIDs) != 2 {
		t.Fatalf("auxiliary alternates=%+v", alternates)
	}
}

func TestSekaipediaLyricsLayoutAcceptsInlineTabberSeparatorVariants(t *testing.T) {
	game := sekaipediaSyntheticTaggedLyricsBody("Ichika", "歌う", "utau")
	full := sekaipediaSyntheticTaggedLyricsBody("Ichika", "歌う", "utau")
	for _, source := range []string{
		"<tabber>\nGame Version=\n" + game + "|-|\nFull Version =\n" + full + "\n</tabber>",
		"<tabber>\nGame Version =\n" + game + "\n|-| \nFull Version=\n" + full + "\n</tabber>",
	} {
		tabs, _, err := parseSekaipediaLyricsLayout(source)
		if err != nil || tabs["Game Version"] == "" || tabs["Full Version"] == "" {
			t.Fatalf("tabber separator source=%q tabs=%v err=%v", source, tabs, err)
		}
	}
}

func TestSekaipediaLyricsLayoutAcceptsLeadingStubAndSubheading(t *testing.T) {
	body := "{{Lyric stub|translation}}\n===Game Version===\n" +
		sekaipediaSyntheticTaggedLyricsBody("Rin", "歌う", "utau")
	tabs, _, err := parseSekaipediaLyricsLayout(body)
	if err != nil || tabs["Game Version"] == "" {
		t.Fatalf("leading stub/subheading tabs=%v err=%v", tabs, err)
	}
}

func TestSekaipediaPlainLyricRendererAcceptsCombinedBoldItalic(t *testing.T) {
	for _, test := range []struct {
		source string
		want   string
	}{
		{source: "'''''歌う'''''", want: "歌う"},
		{source: "'''あっ！''あーっ！'''''", want: "あっ！あーっ！"},
		{source: "''あっ！'''あーっ！'''''", want: "あっ！あーっ！"},
	} {
		rendered, err := renderSekaipediaPlainLyricText(test.source, 0)
		if err != nil || rendered != test.want {
			t.Fatalf("combined bold italic source=%q rendered=%q want=%q err=%v", test.source, rendered, test.want, err)
		}
	}
}

func TestSekaipediaPlainLyricRendererAcceptsMusic487Markup(t *testing.T) {
	for index, source := range []string{
		"'''''ほら忽然と姿どこかへ消してる'''''",
		"'''''大丈夫ですか？ねえ大丈夫ですか？どうしてですか？！ちょっと！？{{Ruby|HP|たいりょく}}すり減らすのやめませんか？！'''''",
		"'''''隠れていないで出てきて下さい！すべては万全です！'''''",
		"<big>'''そして！'''</big>",
		"'''''ヘロヘロ・フラフラ'''''なのですか？",
		"'''''そして[無茶]が１つ消えて'''''",
		"'''''そして[無理]が２つ消えて'''''",
		"'''''[無謀]それは暗転消えた'''''",
		"''だ、大丈夫ですかーーーー？！''",
		"もう！'''''強制的ドクターストップ！！！！'''''",
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			if _, err := renderSekaipediaPlainLyricText(source, 0); err != nil {
				t.Fatalf("Music 487 source=%q error=%v", source, err)
			}
		})
	}
}

func TestSekaipediaVocaloidLyricsAllowKnownVirtualSingersOutsideVersionSet(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese={{Lyric|Rin, Len|歌う}}}}\n" +
		"{{Lyrics tail}}"
	if _, err := parseSekaipediaRenditionWithSet(body, "vocaloid", sekaipediaSingerSet{kind: "vocaloid", ids: []string{"meiko"}}, true); err != nil {
		t.Fatalf("known virtual singer outside version set error=%v", err)
	}
	invalid := strings.Replace(body, "Rin, Len", "An, Len", 1)
	if _, err := parseSekaipediaRenditionWithSet(invalid, "vocaloid", sekaipediaSingerSet{kind: "vocaloid", ids: []string{"meiko"}}, true); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("non-virtual singer outside version set error=%v", err)
	}
}

func TestSekaipediaEvidencedRenditionRosterExpandsDeclaredRoster(t *testing.T) {
	peer := sekaipediaPeerRenditionExtraction{
		Kind:               "sekai",
		SourcePerformerIDs: []string{"歌唱者-01", "歌唱者-05"},
		Full: &Extraction{Performers: []Performer{
			{PerformerID: "歌唱者-05"}, {PerformerID: "歌唱者-21"}, {PerformerID: "歌唱者-01"},
		}},
	}
	roster := sekaipediaEvidencedRenditionRoster(peer)
	want := []string{"歌唱者-01", "歌唱者-05", "歌唱者-21"}
	if !stringSlicesEqual(roster, want) {
		t.Fatalf("evidenced roster=%v want %v", roster, want)
	}
	untouched := sekaipediaEvidencedRenditionRoster(sekaipediaPeerRenditionExtraction{
		SourcePerformerIDs: []string{"歌唱者-21"},
	})
	if !stringSlicesEqual(untouched, []string{"歌唱者-21"}) {
		t.Fatalf("evidenced roster without sides=%v", untouched)
	}
	emptyDeclared := sekaipediaEvidencedRenditionRoster(sekaipediaPeerRenditionExtraction{
		Full: &Extraction{Performers: []Performer{{PerformerID: "歌唱者-21"}}},
	})
	if emptyDeclared != nil {
		t.Fatalf("empty declared roster was expanded: %v", emptyDeclared)
	}
}

func TestSekaipediaEmptyJapaneseLineTemplateIsOnlyASeparator(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese=始まり}}\n" +
		"{{Lyrics line|japanese=|romaji=|english=}}\n" +
		"{{Lyrics line|japanese=終わり}}\n" +
		"{{Lyrics tail}}"
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika"}}
	parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true)
	if err != nil || len(parsed.extraction.Lines) != 2 || !parsed.extraction.Lines[1].StanzaBreakBefore {
		t.Fatalf("empty Japanese separator rendition=%+v err=%v", parsed, err)
	}
	invalid := strings.Replace(body, "{{Lyrics line|japanese=|romaji=|english=}}", "{{Lyrics line|japanese=|romaji=missing source|english=}}", 1)
	if _, err := parseSekaipediaRenditionWithSet(invalid, "sekai", set, true); !errors.Is(err, ErrMissingLyrics) {
		t.Fatalf("empty Japanese with reading error=%v", err)
	}
	// A rendition whose Lyrics head declares the romanized column may carry
	// romaji-only rows (e.g. repeated interjections with no Japanese source
	// text). Those rows act as stanza separators: no Japanese text is lost
	// and the following line keeps its stanza break.
	declaredRomaji := "{{Lyrics head|columns=japanese,romaji|japanese=Japanese lyrics|romaji=Romanized lyrics}}\n" +
		"{{Lyrics line|japanese=始まり|romaji=hajimari}}\n" +
		"{{Lyrics line|japanese=|romaji=yu!}}\n" +
		"{{Lyrics line|japanese=終わり|romaji=owari}}\n" +
		"{{Lyrics tail}}"
	parsed, err = parseSekaipediaRenditionWithSet(declaredRomaji, "sekai", set, true)
	if err != nil || len(parsed.extraction.Lines) != 2 || !parsed.extraction.Lines[1].StanzaBreakBefore {
		t.Fatalf("declared-romaji separator rendition=%+v err=%v", parsed, err)
	}
}

func TestSekaipediaGameRenditionAcceptsExactTabBoundaryWithoutLyricsTail(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese=歌う}}"
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika"}}
	parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, false)
	if err != nil || len(parsed.extraction.Lines) != 1 || parsed.extraction.Lines[0].Japanese != "歌う" {
		t.Fatalf("tail-less bounded Game rendition=%+v err=%v", parsed, err)
	}
	if _, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("tail-less Full rendition error=%v", err)
	}
}

func TestSekaipediaLyricsTailAcceptsEmptyAttribution(t *testing.T) {
	body := "{{Lyrics head|columns=japanese|japanese=Japanese lyrics}}\n" +
		"{{Lyrics line|japanese=歌う}}\n{{Lyrics tail|}}"
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika"}}
	parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true)
	if err != nil || len(parsed.extraction.Lines) != 1 || parsed.extraction.Lines[0].Japanese != "歌う" {
		t.Fatalf("empty-attribution Lyrics tail rendition=%+v err=%v", parsed, err)
	}
}

func TestSekaipediaLeadingLyricStubIsMetadataOnly(t *testing.T) {
	set := sekaipediaSingerSet{kind: "sekai", ids: []string{"ichika", "miku"}}
	body := "{{Lyric stub}}\n" + sekaipediaSyntheticTaggedLyricsBody("Ichika,Miku", "歌う", "utau")
	parsed, err := parseSekaipediaRenditionWithSet(body, "sekai", set, true)
	if err != nil || len(parsed.extraction.Lines) != 1 {
		t.Fatalf("leading lyric stub parse=%+v err=%v", parsed, err)
	}
	invalid := strings.Replace(body, "{{Lyric stub}}", "{{Lyric stub|unexpected}}", 1)
	if _, err := parseSekaipediaRenditionWithSet(invalid, "sekai", set, true); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("parameterized lyric stub error=%v", err)
	}
}

func TestSekaipediaReadingColumnRetainsOnlyExactValidPrefix(t *testing.T) {
	japanese := "{{Lyric|Miku|歌う\n踊る}}\n{{Lyric|Miku|終わる\n始まる\n続ける}}"
	reading := "{{Lyric|Miku|utau\nodoru}}\n{{Lyric|Miku|owaru\nhajimaru|malformed sibling}}"
	set := sekaipediaSingerSet{kind: "vocaloid", ids: []string{"miku"}}
	if _, err := parseSekaipediaReadingColumn(reading, set); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("complete malformed reading column error=%v", err)
	}
	prefix, ok := parseSekaipediaReadingColumnPrefix(reading, set)
	if !ok || len(prefix) != 4 {
		t.Fatalf("reading prefix lines=%d ok=%t", len(prefix), ok)
	}
	source, _, err := parseSekaipediaSourceColumn(japanese, set)
	if err != nil || !sekaipediaColumnsAligned(source[:len(prefix)], prefix) {
		t.Fatalf("reading prefix did not align exact source groups: err=%v", err)
	}

	body := "{{Lyrics head|columns=japanese,romaji|japanese=Japanese lyrics|romaji=Romanized lyrics}}\n" +
		"{{Lyrics line|japanese=" + japanese + "|romaji=" + reading + "}}\n" +
		"{{Lyrics tail}}"
	parsed, err := parseSekaipediaRenditionWithSet(body, "vocaloid", set, false)
	if err != nil || len(parsed.extraction.Lines) != 5 {
		t.Fatalf("partial reading rendition lines=%d err=%v", len(parsed.extraction.Lines), err)
	}
	for index, line := range parsed.extraction.Lines {
		if len(line.Segments) != 1 || !rubySpansValidForText(line.Japanese, line.Segments[0].Ruby) {
			t.Fatalf("partial reading line %d=%+v", index+1, line)
		}
	}
}

func sekaipediaSyntheticTaggedLyricsBody(singers, japanese, reading string) string {
	return strings.Join([]string{
		"{{Lyrics head|columns=japanese,romaji,english|japanese=Japanese lyrics|romaji=Romanized lyrics|english=English translation}}",
		"{{Lyrics line|japanese={{Lyric|" + singers + "|" + japanese + "}}|romaji={{Lyric|" + singers + "|" + reading + "}}|english=ignored translation}}",
		"{{Lyrics tail|ignored attribution}}",
	}, "\n")
}
