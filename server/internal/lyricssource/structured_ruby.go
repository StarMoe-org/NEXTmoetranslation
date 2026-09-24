package lyricssource

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"

	"moesekai/server/internal/model"
)

func containsStructuredJapanese(value string) bool {
	for _, current := range value {
		if (current >= 0x3040 && current <= 0x30ff) || (current >= 0x4e00 && current <= 0x9fff) ||
			(current >= 0xff66 && current <= 0xff9f) {
			return true
		}
	}
	return false
}

func normalizePerformerID(value string) string {
	var builder strings.Builder
	for _, current := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(current) || unicode.IsNumber(current) || current == '-' || current == '_' {
			builder.WriteRune(current)
		}
	}
	return builder.String()
}

func validGeneratedRubyReading(value string) bool {
	if value == "" {
		return false
	}
	hasKana := false
	for _, current := range value {
		switch {
		case unicode.In(current, unicode.Hiragana, unicode.Katakana):
			hasKana = true
		case current == 'ー' || current == '・':
			if !hasKana {
				return false
			}
		case unicode.Is(unicode.Mn, current) || unicode.Is(unicode.Mc, current):
			if !hasKana {
				return false
			}
		default:
			return false
		}
	}
	return hasKana
}

// GenerateDeterministicRubySpans applies the current audited Kagome/IPADIC
// algorithm. It returns exact text-covering spans, annotates only Han bases with
// kana, and fails closed when any Han character cannot be resolved.
func GenerateDeterministicRubySpans(text string) ([]RubySpan, error) {
	return generateRubySpans(text)
}

func markRubyReadingEvidence(
	spans []RubySpan,
	kind model.LyricsSourceReadingEvidenceKind,
	generatorVersion string,
) []RubySpan {
	result := append([]RubySpan(nil), spans...)
	for index := range result {
		if result[index].Reading == "" {
			continue
		}
		result[index].ReadingEvidenceKind = kind
		result[index].GeneratorVersion = generatorVersion
	}
	return result
}

// DeterministicRubyGeneratorVersion identifies the algorithm used by
// GenerateDeterministicRubySpans for immutable plan and artifact bindings.
func DeterministicRubyGeneratorVersion() string {
	return rubyGeneratorVersion
}

func generateRubySpans(text string) ([]RubySpan, error) {
	return generateRubySpansWithUniqueStandaloneDictionary(text, false)
}

func generateSekaipediaMismatchedColumnRubySpans(text string) ([]RubySpan, error) {
	return generateRubySpansWithUniqueStandaloneDictionary(text, true)
}

func generateRubySpansWithUniqueStandaloneDictionary(
	text string, allowUniqueStandaloneDictionary bool,
) ([]RubySpan, error) {
	if err := initializeFuriganaTokenizer(); err != nil {
		return nil, err
	}
	result := make([]RubySpan, 0, len(text))
	for _, token := range furiganaTokenizer.Tokenize(text) {
		surface := token.Surface
		if surface == "" {
			continue
		}
		spans := []RubySpan{{Text: surface}}
		if containsKanji(surface) {
			aligned, ok := tokenRubySpans(token, allowUniqueStandaloneDictionary)
			if !ok {
				return nil, unresolvedGeneratedRubyError()
			}
			spans = aligned
		}
		result = appendRubySpans(result, spans...)
	}
	if len(result) == 0 && text != "" {
		result = []RubySpan{{Text: text}}
	}
	if !rubySpansValidForText(text, result) {
		return nil, unresolvedGeneratedRubyError()
	}
	return result, nil
}

// tokenRubySpans resolves the ruby of one Kagome token that contains Han.
func tokenRubySpans(token tokenizer.Token, allowUniqueStandaloneDictionary bool) ([]RubySpan, bool) {
	surface := token.Surface
	features := token.Features()
	candidate := ""
	if len(features) >= 8 && features[7] != "*" {
		candidate = katakanaToHiragana(features[7])
	}
	aligned, ok := rubySpansFromKanaReading(surface, []rune(candidate))
	if validGeneratedRubyReading(candidate) && ok {
		return markRubyReadingEvidence(
			aligned, model.LyricsSourceReadingEvidenceDeterministicDictionary, rubyGeneratorVersion,
		), true
	}
	if aligned, ok = kagomeNormalizedRubySpans(surface); ok {
		return markRubyReadingEvidence(
			aligned, model.LyricsSourceReadingEvidenceDeterministicDictionary, rubyGeneratorVersion,
		), true
	}
	if aligned, ok = deterministicStandaloneHanRubySpans(surface); ok {
		return aligned, true
	}
	if allowUniqueStandaloneDictionary {
		return kagomeUniqueStandaloneHanRubySpans(surface)
	}
	return nil, false
}

// SuggestRubySpans returns the dictionary ruby of text token by token, as
// GenerateDeterministicRubySpans would with the unique standalone-Han
// fallback, but a token it cannot resolve stays plain text instead of failing
// the whole text. The spans always cover text exactly. Its readings are
// dictionary suggestions for an editor to review, not source evidence.
func SuggestRubySpans(text string) ([]RubySpan, error) {
	if err := initializeFuriganaTokenizer(); err != nil {
		return nil, err
	}
	result := make([]RubySpan, 0, len(text))
	for _, token := range furiganaTokenizer.Tokenize(text) {
		if token.Surface == "" {
			continue
		}
		spans := []RubySpan{{Text: token.Surface}}
		if containsKanji(token.Surface) {
			if aligned, ok := tokenRubySpans(token, true); ok {
				spans = aligned
			}
		}
		result = appendRubySpans(result, spans...)
	}
	if rubySpansText(result) != text {
		return []RubySpan{{Text: text}}, nil
	}
	return result, nil
}

func initializeFuriganaTokenizer() error {
	furiganaTokenizerOnce.Do(func() {
		furiganaTokenizer, furiganaTokenizerErr = tokenizer.New(ipa.Dict(), tokenizer.OmitBosEos())
	})
	return furiganaTokenizerErr
}

func unresolvedGeneratedRubyError() error {
	return fmt.Errorf("%w: deterministic ruby did not cover every Han character", ErrUnsupportedTable)
}

func generateRubySpansForTexts(texts []string) ([][]RubySpan, error) {
	result := make([][]RubySpan, len(texts))
	for index, text := range texts {
		spans, err := generateRubySpans(text)
		if err != nil {
			return nil, err
		}
		result[index] = spans
	}
	return result, nil
}

func splitRubySpansByTexts(spans []RubySpan, texts []string) ([][]RubySpan, bool) {
	remaining := append([]RubySpan(nil), spans...)
	result := make([][]RubySpan, len(texts))
	for textIndex, text := range texts {
		unconsumed := text
		for unconsumed != "" {
			if len(remaining) == 0 || remaining[0].Text == "" {
				return nil, false
			}
			span := remaining[0]
			switch {
			case strings.HasPrefix(unconsumed, span.Text):
				result[textIndex] = appendRubySpans(result[textIndex], span)
				unconsumed = strings.TrimPrefix(unconsumed, span.Text)
				remaining = remaining[1:]
			case strings.HasPrefix(span.Text, unconsumed):
				if span.Reading != "" {
					return nil, false
				}
				result[textIndex] = appendRubySpans(result[textIndex], RubySpan{Text: unconsumed})
				remaining[0].Text = strings.TrimPrefix(span.Text, unconsumed)
				unconsumed = ""
			default:
				return nil, false
			}
		}
		if text == "" {
			continue
		}
		if !rubySpansValidForText(text, result[textIndex]) {
			return nil, false
		}
	}
	return result, len(remaining) == 0
}

// kagomeKanaReading returns the deterministic IPADIC pronunciation for an
// exact Han surface. Dictionary-covered text keeps this orthographic reading;
// fixed-source romaji is used only to align or fill dictionary-missing regions.
func kagomeKanaReading(surface string) ([]rune, bool) {
	if surface == "" || !containsKanji(surface) || initializeFuriganaTokenizer() != nil {
		return nil, false
	}
	var reading strings.Builder
	for _, token := range furiganaTokenizer.Tokenize(surface) {
		if token.Surface == "" {
			continue
		}
		if !containsKanji(token.Surface) {
			return nil, false
		}
		features := token.Features()
		if len(features) < 8 || features[7] == "*" {
			return nil, false
		}
		candidate := katakanaToHiragana(features[7])
		if !validGeneratedRubyReading(candidate) {
			return nil, false
		}
		reading.WriteString(candidate)
	}
	result := []rune(reading.String())
	return result, len(result) > 0
}

// deterministicStandaloneHanRubySpans covers exact standalone Han tokens whose
// standard Japanese reading is absent from IPADIC. Keep this list token-exact:
// surrounding kana, punctuation, digits, and Latin text must never inherit the
// reading, and compounds must continue through Kagome or fixed-source alignment.
func deterministicStandaloneHanRubySpans(surface string) ([]RubySpan, bool) {
	reading, exists := map[string]string{
		"彡": "さん",
	}[surface]
	if !exists || !validGeneratedRubyReading(reading) {
		return nil, false
	}
	spans := markRubyReadingEvidence(
		[]RubySpan{{Text: surface, Reading: reading}},
		model.LyricsSourceReadingEvidenceFixedReviewedToken,
		rubyGeneratorVersion,
	)
	return spans, rubySpansValidForText(surface, spans)
}

// kagomeUniqueStandaloneHanRubySpans recovers an exact one-Han token only when
// the embedded IPADIC dictionary yields one unique reading across a closed set
// of bounded morphological probes. No reading is stored in code, ambiguous Han
// remain unsupported, and compounds or tokens containing non-Han text never use
// this fallback.
func kagomeUniqueStandaloneHanRubySpans(surface string) ([]RubySpan, bool) {
	runes := []rune(surface)
	if len(runes) != 1 || !model.LyricsSourceRubyBaseRune(runes[0]) || initializeFuriganaTokenizer() != nil {
		return nil, false
	}
	readings := map[string]struct{}{}
	probes := []string{"お" + surface, "ご" + surface}
	for _, suffix := range []string{
		"する", "した", "して", "る", "う", "む", "ぶ", "く", "ぐ", "す", "つ", "ぬ", "ふ",
		"い", "しい", "か", "き", "け", "こ", "さ", "し", "せ", "そ", "た", "て", "と",
		"な", "に", "ね", "の", "ま", "み", "め", "も", "や", "ゆ", "よ",
	} {
		probes = append(probes, surface+suffix)
	}
	for _, probe := range probes {
		for _, token := range furiganaTokenizer.Tokenize(probe) {
			if !strings.Contains(token.Surface, surface) || !containsKanji(token.Surface) {
				continue
			}
			features := token.Features()
			if len(features) < 8 || features[7] == "*" {
				continue
			}
			candidate := katakanaToHiragana(features[7])
			spans, ok := rubySpansFromKanaReading(token.Surface, []rune(candidate))
			if !ok {
				continue
			}
			for _, span := range spans {
				if span.Text == surface && validGeneratedRubyReading(span.Reading) {
					readings[span.Reading] = struct{}{}
				}
			}
		}
	}
	if len(readings) != 1 {
		return nil, false
	}
	for reading := range readings {
		spans := markRubyReadingEvidence(
			[]RubySpan{{Text: surface, Reading: reading}},
			model.LyricsSourceReadingEvidenceDeterministicDictionary,
			rubyGeneratorVersion,
		)
		return spans, rubySpansValidForText(surface, spans)
	}
	return nil, false
}

func containsKanji(value string) bool {
	for _, current := range value {
		if model.LyricsSourceRubyBaseRune(current) {
			return true
		}
	}
	return false
}

// rubySpansCoverKanji enforces the positive half of the public furigana
// contract: every span containing Han carries a valid kana pronunciation.
func rubySpansCoverKanji(spans []RubySpan) bool {
	for _, span := range spans {
		if containsKanji(span.Text) && !validGeneratedRubyReading(span.Reading) {
			return false
		}
	}
	return true
}

// rubySpansValidForText enforces both sides of the contract. Every visible Han
// character is inside a ruby base with a kana reading, while kana, punctuation,
// digits, and Latin text never receive a redundant reading.
func rubySpansValidForText(text string, spans []RubySpan) bool {
	if text == "" {
		return len(spans) == 0
	}
	if len(spans) == 0 || rubySpansText(spans) != text {
		return false
	}
	for _, span := range spans {
		if span.Text == "" {
			return false
		}
		if span.Reading == "" {
			if containsKanji(span.Text) {
				return false
			}
			continue
		}
		if !validGeneratedRubyReading(span.Reading) {
			return false
		}
		for _, current := range span.Text {
			if !model.LyricsSourceRubyBaseRune(current) {
				return false
			}
		}
	}
	return true
}

func katakanaToHiragana(value string) string {
	var builder strings.Builder
	for _, current := range value {
		if current >= 'ァ' && current <= 'ヶ' {
			current -= 'ァ' - 'ぁ'
		}
		builder.WriteRune(current)
	}
	return builder.String()
}

func structuredLinesFromLegacy(lines []ExtractedLine) ([]StructuredLine, error) {
	result := make([]StructuredLine, len(lines))
	for index, line := range lines {
		ruby, err := generateRubySpans(line.Japanese)
		if err != nil {
			return nil, err
		}
		result[index] = StructuredLine{
			Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments:             []LyricsSegment{{Text: line.Japanese, PerformerIDs: []string{}, Ruby: ruby}},
			TrailingPerformerIDs: []string{},
		}
	}
	return result, nil
}

func legacyExtractedLines(lines []StructuredLine) []ExtractedLine {
	result := make([]ExtractedLine, len(lines))
	for index, line := range lines {
		result[index] = ExtractedLine{Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore}
	}
	return result
}

func canonicalPerformers(performers []Performer) []Performer {
	result := append([]Performer(nil), performers...)
	sort.SliceStable(result, func(left, right int) bool { return result[left].PerformerID < result[right].PerformerID })
	return result
}

var _ = canonicalPerformers
