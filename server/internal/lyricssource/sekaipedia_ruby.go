package lyricssource

import (
	"crypto/sha256"
	"encoding/hex"
	"html"

	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"moesekai/server/internal/model"
)

type sekaipediaRubyRegion struct {
	text     string
	kana     []rune
	variable bool
	annotate bool
	// numerals are the candidate pronunciations of a digit run (source
	// alignment only); empty when the run is not read as a number.
	numerals [][]rune
	// free marks visible Latin or digit text that the source sings from
	// other words: it may absorb any kana, including none.
	free bool
}

type sekaipediaRubyAlignment struct {
	count int
	ends  []int
}

type sekaipediaHybridRubyRegion struct {
	text           string
	fixedKana      []rune
	dictionary     []RubySpan
	dictionaryKana []rune
	variable       bool
	annotate       bool
}

type sekaipediaHybridRubyAlignment struct {
	count      int
	cost       int
	ends       []int
	signature  uint64
	equivalent bool
}

// The tuple digest is SHA-256(text NUL source-reading NUL dictionary-reading).
// It is bound by the fixed F50 orthography case and the immutable F-ORTH audit;
// keeping only the digest prevents a page, music-ID, or source-text exception.
var sekaipediaFixedReviewedOrthographyDigests = map[string]struct{}{
	"ad2610f3f834f33d28f0f6070c09a6a38f7f3dc97a276d928e29d22d548a61ca": {},
}

func applySekaipediaFixedReviewedOrthography(japanese string, source []RubySpan) []RubySpan {
	dictionary, err := generateRubySpans(japanese)
	if err != nil || len(dictionary) != len(source) || !rubySpansValidForText(japanese, dictionary) {
		return source
	}
	for index := range source {
		if source[index].Text != dictionary[index].Text {
			return source
		}
	}
	result := append([]RubySpan(nil), source...)
	for index := range result {
		if result[index].Reading == "" || result[index].Reading == dictionary[index].Reading ||
			dictionary[index].Reading == "" {
			continue
		}
		digest := sha256.Sum256([]byte(result[index].Text + "\x00" + result[index].Reading + "\x00" + dictionary[index].Reading))
		if _, reviewed := sekaipediaFixedReviewedOrthographyDigests[hex.EncodeToString(digest[:])]; !reviewed {
			continue
		}
		result[index].Reading = dictionary[index].Reading
		result[index].ReadingEvidenceKind = model.LyricsSourceReadingEvidenceFixedReviewedToken
		result[index].GeneratorVersion = rubyGeneratorVersion
		result[index].SourceRowOrdinal = 0
		result[index].SourceSegmentOrdinal = 0
	}
	return result
}

func deriveSekaipediaRuby(japanese, romanized string) ([]RubySpan, bool) {
	return deriveSekaipediaRubyMode(japanese, romanized, true)
}

// deriveSekaipediaRubyMode tries the source alignment, the hybrid and the
// dictionary in turn. With lastResort, a line none of them can read falls back
// to a lenient source alignment; a caller that has its own fallback (such as
// per-line dictionary readings) passes false.
func deriveSekaipediaRubyMode(japanese, romanized string, lastResort bool) ([]RubySpan, bool) {
	if japanese == "" || !utf8.ValidString(japanese) {
		return nil, false
	}
	if source, ok := deriveSekaipediaSourceRuby(japanese, romanized); ok {
		return applySekaipediaFixedReviewedOrthography(japanese, source), true
	}
	if hybrid, ok := deriveSekaipediaHybridRuby(japanese, romanized); ok {
		return hybrid, true
	}
	if dictionary, err := generateRubySpans(japanese); err == nil && rubySpansValidForText(japanese, dictionary) {
		return dictionary, true
	}
	if !lastResort {
		return nil, false
	}
	return deriveSekaipediaLastResortRuby(japanese, romanized)
}

// deriveSekaipediaLastResortRuby reads a line that neither the source, the
// hybrid nor the dictionary can read in full, with the lenient source
// alignment (see alignSekaipediaSourceRubyRegions): plausible readings only,
// no tie and no substituted kana, every visible kana run between two bases
// matched, and at least as many visible kana kept as edits needed, so
// 彁が笑った romanized "waratta" fails instead of reading 彁 as わ.
func deriveSekaipediaLastResortRuby(japanese, romanized string) ([]RubySpan, bool) {
	source, ok := deriveSekaipediaSourceRubyMode(japanese, romanized, true)
	if !ok {
		return nil, false
	}
	return applySekaipediaFixedReviewedOrthography(japanese, source), true
}

func deriveSekaipediaSourceRuby(japanese, romanized string) ([]RubySpan, bool) {
	return deriveSekaipediaSourceRubyMode(japanese, romanized, false)
}

func deriveSekaipediaSourceRubyMode(japanese, romanized string, lenient bool) ([]RubySpan, bool) {
	return deriveSekaipediaSourceRubyDiagnosed(japanese, romanized, lenient, nil)
}

// deriveSekaipediaSourceRubyDiagnosed is deriveSekaipediaSourceRubyMode that
// records in diagnosis why the lenient alignment failed.
func deriveSekaipediaSourceRubyDiagnosed(
	japanese, romanized string, lenient bool, diagnosis *sekaipediaAlignmentDiagnosis,
) ([]RubySpan, bool) {
	if japanese == "" || romanized == "" || !utf8.ValidString(japanese) || !utf8.ValidString(romanized) {
		return nil, false
	}
	if !sekaipediaHasJapaneseScript(japanese) {
		return []RubySpan{{Text: japanese}}, true
	}
	words, ok := sekaipediaTransientRomanizedWords(japanese, romanized)
	if !ok || len(words) == 0 {
		return nil, false
	}
	var target strings.Builder
	wordReadings := make([][]rune, 0, len(words))
	wordBoundaries := map[int]bool{0: true}
	// An opaque source has words that are not romaji ("party night" for
	// パーリナイ, "BEBOP" for ビバップ); spelled out as letter names they match
	// nothing visible, so only the last-resort lenient pass may align them,
	// and even then never as a base's reading.
	opaque := false
	letterNames := []int{0}
	for _, word := range words {
		kana, ok := romanizeSekaipediaWordToKana(word)
		if !ok {
			if sekaipediaIgnorableRomanizedWord(word) {
				continue
			}
			if diagnosis != nil {
				diagnosis.unconverted = true
			}
			return nil, false
		}
		_, romaji := romanizeSekaipediaWordToKanaStrict(word)
		opaque = opaque || !romaji
		reading := []rune(kana)
		for range reading {
			count := letterNames[len(letterNames)-1]
			if !romaji {
				count++
			}
			letterNames = append(letterNames, count)
		}
		wordReadings = append(wordReadings, reading)
		target.WriteString(kana)
		wordBoundaries[utf8.RuneCountInString(target.String())] = true
	}
	targetRunes := []rune(target.String())
	if len(targetRunes) == 0 {
		return nil, false
	}
	hints := sekaipediaSourceAlignmentHints{
		wordBoundaries: wordBoundaries,
		sourceWords:    sekaipediaASCIIWords(norm.NFKC.String(html.UnescapeString(romanized))),
		letterNames:    letterNames,
		numerals:       true,
	}
	var spans []RubySpan
	aligned := false
	if !opaque {
		spans, aligned = sekaipediaSourceRubySpans(japanese, targetRunes, hints)
		if !aligned {
			wordHints := hints
			wordHints.wordBoundaries = nil
			spans, aligned = deriveSekaipediaSourceWordRuby(japanese, wordReadings, wordHints)
		}
	}
	if !aligned && lenient {
		hints.lenient = true
		hints.diagnosis = diagnosis
		spans, aligned = sekaipediaSourceRubySpans(japanese, targetRunes, hints)
	}
	if !aligned {
		return nil, false
	}
	return markRubyReadingEvidence(
		spans, model.LyricsSourceReadingEvidenceSourceTransliteration, "",
	), true
}

// deriveSekaipediaSourceWordRuby recovers a failed whole-segment alignment only
// when the immutable source word boundaries have one exact partition of the
// same visible performer segment. Local dictionary readings bound the possible
// token boundaries, but the retained ruby comes from the fixed-source words.
// Nothing moves across a segment, source row, side, or rendition, and tied or
// otherwise ambiguous mappings fail closed.
func deriveSekaipediaSourceWordRuby(
	japanese string, wordReadings [][]rune, hints sekaipediaSourceAlignmentHints,
) ([]RubySpan, bool) {
	japaneseRunes := []rune(japanese)
	if len(wordReadings) < 2 || len(wordReadings) > len(japaneseRunes) ||
		!sekaipediaAlignmentWithinBudget(len(japaneseRunes), len(wordReadings)) ||
		initializeFuriganaTokenizer() != nil {
		return nil, false
	}
	tokenBoundaries := map[int]bool{}
	var tokenText strings.Builder
	runeCount := 0
	for _, token := range furiganaTokenizer.Tokenize(japanese) {
		if token.Surface == "" {
			continue
		}
		tokenText.WriteString(token.Surface)
		runeCount += len([]rune(token.Surface))
		tokenBoundaries[runeCount] = true
	}
	if tokenText.String() != japanese || !tokenBoundaries[len(japaneseRunes)] {
		return nil, false
	}
	type state struct {
		word  int
		start int
	}
	type result struct {
		count int
		cost  int
		spans []RubySpan
	}
	type piece struct {
		reading []rune
		ok      bool
	}
	dictionaryReadings := map[[2]int]piece{}
	memo := map[state]result{}
	var solve func(int, int) result
	solve = func(wordIndex, start int) result {
		key := state{word: wordIndex, start: start}
		if cached, found := memo[key]; found {
			return cached
		}
		if wordIndex == len(wordReadings) {
			if start == len(japaneseRunes) {
				return result{count: 1}
			}
			return result{}
		}
		remainingWords := len(wordReadings) - wordIndex - 1
		maxCost := max(1, len(wordReadings[wordIndex])/3)
		// Every visible Han and kana reads at least one kana, so a piece much
		// longer than its word's reading plus the allowed edits cannot match
		// it; the slack covers punctuation and spaces that read nothing.
		lastEnd := min(len(japaneseRunes)-remainingWords, start+len(wordReadings[wordIndex])+maxCost+sekaipediaKanaRegionSlack)
		best := result{}
		for end := start + 1; end <= lastEnd; end++ {
			if !tokenBoundaries[end] {
				continue
			}
			text := string(japaneseRunes[start:end])
			if !containsKanji(text) {
				continue
			}
			cached, found := dictionaryReadings[[2]int{start, end}]
			if !found {
				cached.reading, cached.ok = kagomeKanaReading(text)
				dictionaryReadings[[2]int{start, end}] = cached
			}
			if !cached.ok {
				continue
			}
			cost := sekaipediaKanaEditDistance(wordReadings[wordIndex], cached.reading)
			if cost > maxCost {
				continue
			}
			spans, ok := sekaipediaSourceRubySpans(text, wordReadings[wordIndex], hints)
			if !ok {
				continue
			}
			child := solve(wordIndex+1, end)
			if child.count == 0 {
				continue
			}
			cost += child.cost
			switch {
			case best.count == 0 || cost < best.cost:
				best = result{
					count: child.count, cost: cost,
					spans: append(append([]RubySpan(nil), spans...), child.spans...),
				}
			case cost == best.cost:
				best.count += child.count
				if best.count > 1 {
					best.count = 2
				}
			}
		}
		memo[key] = best
		return best
	}
	aligned := solve(0, 0)
	return aligned.spans, aligned.count == 1 && rubySpansValidForText(japanese, aligned.spans)
}

// sekaipediaSourceRubyPlausible rejects source-aligned readings that can only
// come from a shifted alignment: a reading that starts with a kana no word can
// begin with (it was cut from the previous base's reading), a single mora over
// two or more Han (the rest moved onto a neighbour), or one too long for its
// base (it swallowed a neighbour's or a digit run's reading). A reading shorter
// than its Han count is otherwise allowed, because lyrics deliberately write
// whole-word readings such as 王子様 read きみ.
func sekaipediaSourceRubyPlausible(spans []RubySpan) bool {
	for _, span := range spans {
		if span.Reading == "" {
			continue
		}
		hanCount := 0
		for _, current := range span.Text {
			if model.LyricsSourceRubyBaseRune(current) {
				hanCount++
			}
		}
		if !sekaipediaPlausibleSourceReading(hanCount, []rune(span.Reading)) {
			return false
		}
	}
	return true
}

func sekaipediaPlausibleSourceReading(hanCount int, reading []rune) bool {
	return sekaipediaPlausibleSourceReadingMode(hanCount, reading, false)
}

// sekaipediaPlausibleSourceReadingMode applies sekaipediaPlausibleSourceReading;
// singleMora lets two or more Han share one mora (see
// sekaipediaSingleMoraAllowed).
func sekaipediaPlausibleSourceReadingMode(hanCount int, reading []rune, singleMora bool) bool {
	if len(reading) == 0 {
		return false
	}
	return sekaipediaPlausibleSourceShape(
		hanCount, len(reading), reading[0], sekaipediaKanaMoraCount(reading), singleMora,
	)
}

// sekaipediaPlausibleSourceShape is sekaipediaPlausibleSourceReadingMode for a
// reading given by its length, first kana and mora count, so that an
// alignment can check every candidate end in constant time.
func sekaipediaPlausibleSourceShape(hanCount, length int, first rune, mora int, singleMora bool) bool {
	return hanCount > 0 && length > 0 && length <= 4*hanCount+2 &&
		!sekaipediaKanaCannotStartWord(first) && (hanCount < 2 || singleMora || mora >= 2)
}

// sekaipediaMoraPrefix counts, for each offset of target, the kana before it
// that make a mora of their own.
func sekaipediaMoraPrefix(target []rune) []int {
	prefix := make([]int, len(target)+1)
	for index, current := range target {
		prefix[index+1] = prefix[index]
		if !sekaipediaKanaSmallVowel(current) {
			prefix[index+1]++
		}
	}
	return prefix
}

// sekaipediaSingleMoraAllowed reports whether the exact alignment may read
// regions[index] as target[start:end] when that is a single mora over several
// Han. An explicit {{ruby}} reading is the author's own, so it always may
// (共鳴 read な). A romanized one may only when the reading starts a source
// word and that word goes on in the okurigana right after the base:
// 共鳴らして "narashite" (な|らして) and 背負い込んで "shoikonde" (しょ|い). A mora
// cut from the previous word, a word that ends with the base (今日も "kyo
// mo"), or one that goes on in the next base (正解(世界) "sekai" split せ|かい)
// is still rejected. The exact alignment must also be the only one.
func sekaipediaSingleMoraAllowed(
	regions []sekaipediaRubyRegion, index, start, end int, hints *sekaipediaSourceAlignmentHints,
) bool {
	switch {
	case hints == nil:
		return false
	case hints.explicit:
		return true
	}
	return hints.wordBoundaries[start] && !hints.wordBoundaries[end] &&
		index+1 < len(regions) && len(regions[index+1].kana) > 0
}

// sekaipediaSourceEndsPlausible checks the readings of an exact alignment,
// allowing a single mora over several Han only as sekaipediaSingleMoraAllowed
// does.
func sekaipediaSourceEndsPlausible(
	regions []sekaipediaRubyRegion, target []rune, ends []int, hints *sekaipediaSourceAlignmentHints,
) bool {
	if len(ends) != len(regions) {
		return false
	}
	start := 0
	for index, region := range regions {
		end := ends[index]
		if end < start || end > len(target) {
			return false
		}
		if region.annotate && (!sekaipediaPlausibleSourceReadingMode(
			utf8.RuneCountInString(region.text), target[start:end],
			sekaipediaSingleMoraAllowed(regions, index, start, end, hints),
		) || hints.spellsLetterNames(start, end)) {
			return false
		}
		start = end
	}
	return true
}

// sekaipediaSourceRubySpans aligns one source (romaji or explicit kana)
// pronunciation against visible text. The exact kana-anchored alignment runs
// first and must be unique among plausible readings (see
// sekaipediaSourceEndsPlausible); the edit-distance alignment (see
// alignSekaipediaSourceRubyRegions) covers source typos and fails closed
// rather than keep the first of several alignments, which used to put one
// kana on the first base and the rest on the next. Its result must pass the
// strict sekaipediaSourceRubyPlausible. A line beyond the alignment budget
// fails closed.
func sekaipediaSourceRubySpans(
	japanese string, target []rune, hints sekaipediaSourceAlignmentHints,
) ([]RubySpan, bool) {
	regions := sekaipediaSourceRubyRegions(japanese, hints)
	if !sekaipediaAlignmentWithinBudget(len(target), len(regions)) {
		if hints.diagnosis != nil {
			hints.diagnosis.overBudget = true
		}
		return nil, false
	}
	if ends, ok := alignSekaipediaRubyRegions(regions, target, &hints); ok &&
		sekaipediaSourceEndsPlausible(regions, target, ends, &hints) {
		if spans, built := sekaipediaRubySpansFromEnds(japanese, regions, target, ends, false); built {
			return spans, true
		}
	}
	if ends, ok := alignSekaipediaSourceRubyRegions(regions, target, hints); ok {
		if spans, built := sekaipediaRubySpansFromEnds(japanese, regions, target, ends, true); built &&
			sekaipediaSourceRubyPlausible(spans) {
			return spans, true
		}
	}
	return nil, false
}

// sekaipediaAlignmentBudget bounds the (kana + 1) x (regions + 1) table of a
// source or hybrid alignment. A lyric line may be up to 8 KiB and anyone can
// edit the wiki, so a line beyond the budget fails closed to the dictionary
// instead of spending seconds on its alignment.
const sekaipediaAlignmentBudget = 12000

// sekaipediaKanaRegionSlack is how many more source kana than it shows a
// visible kana run may absorb in the edit-distance alignment.
const sekaipediaKanaRegionSlack = 4

// spellsLetterNames reports whether target[start:end] contains kana
// of a source word spelled out as letter names.
func (hints *sekaipediaSourceAlignmentHints) spellsLetterNames(start, end int) bool {
	return hints != nil && len(hints.letterNames) > end && hints.letterNames[end] > hints.letterNames[start]
}

func sekaipediaAlignmentWithinBudget(targetLength, regionCount int) bool {
	return (targetLength+1)*(regionCount+1) <= sekaipediaAlignmentBudget
}

type sekaipediaSourceAlignmentHints struct {
	// wordBoundaries are the kana offsets between source words, when known.
	wordBoundaries map[int]bool
	// sourceWords are the Latin and digit words of a romanized source; nil
	// for an explicit kana reading.
	sourceWords []string
	// letterNames counts, for each kana offset, the kana before it that spell
	// out a word that is not romaji; no base reading may contain them.
	letterNames []int
	// numerals reads every other digit run as a Japanese number, so its kana
	// stays on the digits instead of moving onto the next base.
	numerals bool
	// explicit marks a kana reading written for exactly this base (a
	// {{ruby}} template) rather than a romanization of the whole line.
	explicit bool
	// lenient searches plausible readings only and reads a source with
	// words that are not romaji; it is the last resort when no dictionary
	// reading exists either (see alignSekaipediaSourceRubyRegions).
	lenient bool
	// diagnosis, when set, records why the edit alignment failed.
	diagnosis *sekaipediaAlignmentDiagnosis
}

// sekaipediaAlignmentDiagnosis records why a source alignment was not
// trusted, for the error of a line that gets no ruby.
type sekaipediaAlignmentDiagnosis struct {
	unconverted bool // a source word does not romanize to kana
	overBudget  bool // the line is beyond sekaipediaAlignmentBudget
	unaligned   bool // no plausible alignment exists
	tied        bool // equally good alignments read a base differently
	substituted bool // the best alignment substitutes a kana
	unanchored  bool // it drops visible kana or needs more edits than it keeps
}

// reasons lists the recorded failures, most specific first.
func (diagnosis sekaipediaAlignmentDiagnosis) reasons() []string {
	var reasons []string
	for _, reason := range []struct {
		failed bool
		text   string
	}{
		{diagnosis.unconverted, "a source word does not romanize"},
		{diagnosis.overBudget, "the line is beyond the alignment budget"},
		{diagnosis.unaligned, "no plausible source alignment"},
		{diagnosis.tied, "tied source alignments"},
		{diagnosis.substituted, "the source alignment substitutes kana"},
		{diagnosis.unanchored, "the source drops visible kana"},
	} {
		if reason.failed {
			reasons = append(reasons, reason.text)
		}
	}
	return reasons
}

// sekaipediaRubyFailureReason explains why japanese got no trusted ruby from
// romanized: what the last-resort source alignment rejected, and which Han
// the dictionary cannot read.
func sekaipediaRubyFailureReason(japanese, romanized string) string {
	var reasons []string
	if strings.TrimSpace(romanized) == "" {
		reasons = append(reasons, "no romanized source")
	} else {
		var diagnosis sekaipediaAlignmentDiagnosis
		if _, ok := deriveSekaipediaSourceRubyDiagnosed(japanese, romanized, true, &diagnosis); !ok {
			reasons = append(reasons, diagnosis.reasons()...)
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "no trusted source alignment")
		}
	}
	if initializeFuriganaTokenizer() == nil {
		var unread []string
		for _, token := range furiganaTokenizer.Tokenize(japanese) {
			if containsKanji(token.Surface) {
				if _, ok := tokenRubySpans(token, false); !ok {
					unread = append(unread, token.Surface)
				}
			}
		}
		if len(unread) > 0 {
			reasons = append(reasons, "the dictionary cannot read "+strings.Join(unread, ", "))
		}
	}
	return strings.Join(reasons, "; ")
}

// sekaipediaSourceRubyRegions classifies the visible Latin and digit text for
// a source alignment. A Latin or digit token that the romanization repeats
// (MEIKO, 1st, the 436 of "kono 436 no reiji") carries none of the source
// kana. One it does not repeat is sung from other words (15cm "juugo senchi",
// HP "tairyoku", ℃ "do"), so it may absorb kana, but how much stays
// unknown; any tie over it fails closed. With numerals enabled, a digit-only
// run gets its numeral pronunciations instead.
func sekaipediaSourceRubyRegions(japanese string, hints sekaipediaSourceAlignmentHints) []sekaipediaRubyRegion {
	regions := sekaipediaRubyRegions(japanese)
	type token struct {
		start, end int
		letter     bool
		matched    bool
	}
	starts := make([]int, len(regions)+1)
	var flat []rune
	for index, region := range regions {
		starts[index] = len(flat)
		if len(region.kana) == 0 && !region.annotate {
			flat = append(flat, []rune(strings.ToLower(norm.NFKC.String(region.text)))...)
		} else {
			flat = append(flat, ' ')
		}
	}
	starts[len(regions)] = len(flat)
	remaining := map[string]int{}
	for _, word := range hints.sourceWords {
		remaining[word]++
	}
	tokens := []token{}
	for index := 0; index < len(flat); {
		if !sekaipediaASCIIAlphanumeric(flat[index]) {
			index++
			continue
		}
		current := token{start: index}
		for index < len(flat) && sekaipediaASCIIAlphanumeric(flat[index]) {
			current.letter = current.letter || flat[index] >= 'a' && flat[index] <= 'z'
			index++
		}
		current.end = index
		if word := string(flat[current.start:current.end]); remaining[word] > 0 {
			remaining[word]--
			current.matched = true
		}
		tokens = append(tokens, current)
	}
	for index := range regions {
		region := &regions[index]
		if len(region.kana) > 0 || region.annotate {
			continue
		}
		matched, unmatchedLetters := false, false
		for _, current := range tokens {
			if current.end <= starts[index] || current.start >= starts[index+1] {
				continue
			}
			matched = matched || current.matched
			unmatchedLetters = unmatchedLetters || !current.matched && current.letter
		}
		romanized := hints.sourceWords != nil
		switch {
		case romanized && matched && region.variable:
			region.variable = false
		case romanized && unmatchedLetters:
			region.variable = true
			region.free = true
		case region.variable && hints.numerals:
			region.numerals = sekaipediaNumeralReadings(norm.NFKC.String(region.text))
		}
	}
	return regions
}

func sekaipediaASCIIAlphanumeric(current rune) bool {
	return current >= 'a' && current <= 'z' || current >= '0' && current <= '9'
}

func sekaipediaDigitWord(value string) bool {
	if value == "" {
		return false
	}
	for _, current := range value {
		if current < '0' || current > '9' {
			return false
		}
	}
	return true
}

// sekaipediaKanaCannotStartWord reports small kana, the moraic nasal, the
// long-vowel mark, and iteration marks, none of which can open a reading.
func sekaipediaKanaCannotStartWord(current rune) bool {
	return strings.ContainsRune("ぁぃぅぇぉっゃゅょゎゕゖんゝゞァィゥェォッャュョヮヵヶンヽヾーｧｨｩｪｫｯｬｭｮﾝｰ", current)
}

func sekaipediaKanaMoraCount(reading []rune) int {
	count := 0
	for _, current := range reading {
		if !sekaipediaKanaSmallVowel(current) {
			count++
		}
	}
	return count
}

// sekaipediaKanaSmallVowel reports small kana that join the previous kana's
// mora instead of making their own (the small っ makes one).
func sekaipediaKanaSmallVowel(current rune) bool {
	return strings.ContainsRune("ぁぃぅぇぉゃゅょゎァィゥェォャュョヮｧｨｩｪｫｬｭｮ", current)
}

// rubySpansFromKanaReading aligns one complete kana pronunciation against the
// visible Japanese text while emitting readings only for Han-like bases. Kana
// still acts as a fixed alignment anchor, but it is never redundantly rendered
// as ruby above itself. Digits may consume pronunciation during alignment yet
// remain plain visible text.
func deriveSekaipediaHybridRuby(japanese, romanized string) ([]RubySpan, bool) {
	if japanese == "" || romanized == "" || !utf8.ValidString(japanese) || !utf8.ValidString(romanized) ||
		initializeFuriganaTokenizer() != nil {
		return nil, false
	}
	words, ok := sekaipediaTransientRomanizedWords(japanese, romanized)
	if !ok || len(words) == 0 {
		return nil, false
	}
	regions := sekaipediaHybridRubyRegions(japanese)
	if len(regions) == 0 {
		return nil, false
	}
	var target strings.Builder
	wordBoundaries := map[int]bool{}
	// opaque marks the kana of source words that are not romaji (BEBOP, a
	// truncated "jouj"): spelled out as letter names, they may stand beside a
	// dictionary reading but never become a base's own reading.
	var opaque []bool
	repairedLeadingDictionaryWord := false
	for wordIndex, word := range words {
		kana, converted := romanizeSekaipediaWordToKana(word)
		if !converted {
			switch {
			case sekaipediaIgnorableRomanizedWord(word):
				continue
			case wordIndex == 0 && !repairedLeadingDictionaryWord &&
				sekaipediaHybridLeadingDictionaryWordMatches(word, regions):
				repairedLeadingDictionaryWord = true
				continue
			default:
				return nil, false
			}
		}
		_, romaji := romanizeSekaipediaWordToKanaStrict(word)
		for range []rune(kana) {
			opaque = append(opaque, !romaji)
		}
		target.WriteString(kana)
		wordBoundaries[len([]rune(target.String()))] = true
	}
	targetRunes := []rune(target.String())
	if len(targetRunes) == 0 || !sekaipediaAlignmentWithinBudget(len(targetRunes), len(regions)) {
		return nil, false
	}
	ends, aligned := alignSekaipediaHybridRubyRegions(regions, targetRunes, wordBoundaries)
	if !aligned {
		return nil, false
	}
	result := make([]RubySpan, 0, len(regions))
	start := 0
	for index, region := range regions {
		end := ends[index]
		switch {
		case region.annotate && len(region.dictionary) > 0:
			result = appendRubySpans(result, region.dictionary...)
		case region.annotate:
			for _, letterName := range opaque[start:end] {
				if letterName {
					return nil, false
				}
			}
			spans, ok := sekaipediaSourceRubySpans(region.text, targetRunes[start:end], sekaipediaSourceAlignmentHints{})
			if !ok {
				return nil, false
			}
			result = appendRubySpans(result, markRubyReadingEvidence(
				spans, model.LyricsSourceReadingEvidenceSourceTransliteration, "",
			)...)
		default:
			result = appendRubySpans(result, RubySpan{Text: region.text})
		}
		start = end
	}
	if start != len(targetRunes) || !rubySpansValidForText(japanese, result) {
		return nil, false
	}
	return result, true
}

func sekaipediaHybridRubyRegions(japanese string) []sekaipediaHybridRubyRegion {
	regions := make([]sekaipediaHybridRubyRegion, 0, len(japanese))
	for _, token := range furiganaTokenizer.Tokenize(japanese) {
		surface := token.Surface
		if surface == "" {
			continue
		}
		if containsKanji(surface) {
			region := sekaipediaHybridRubyRegion{text: surface, variable: true, annotate: true}
			features := token.Features()
			if len(features) >= 8 && features[7] != "*" {
				reading := []rune(katakanaToHiragana(features[7]))
				if validGeneratedRubyReading(string(reading)) {
					if spans, aligned := rubySpansFromKanaReading(surface, reading); aligned {
						region.dictionary = markRubyReadingEvidence(
							spans, model.LyricsSourceReadingEvidenceDeterministicDictionary, rubyGeneratorVersion,
						)
						region.dictionaryKana = reading
					}
				}
			}
			if len(region.dictionary) == 0 {
				if spans, exact := deterministicStandaloneHanRubySpans(surface); exact {
					region.dictionary = spans
					region.dictionaryKana = []rune(spans[0].Reading)
				}
			}
			regions = append(regions, region)
			continue
		}
		for _, sourceRegion := range sekaipediaRubyRegions(surface) {
			regions = append(regions, sekaipediaHybridRubyRegion{
				text: sourceRegion.text, fixedKana: sourceRegion.kana,
				variable: sourceRegion.variable, annotate: sourceRegion.annotate,
			})
		}
	}
	return regions
}

func sekaipediaHybridHasDictionaryPrefixAndFallback(regions []sekaipediaHybridRubyRegion) bool {
	prefixCovered := false
	for _, region := range regions {
		if region.text == "" {
			continue
		}
		if !prefixCovered {
			if !region.annotate || len(region.dictionary) == 0 {
				return false
			}
			prefixCovered = true
			continue
		}
		if region.annotate && len(region.dictionary) == 0 {
			return true
		}
	}
	return false
}

// sekaipediaHybridLeadingDictionaryWordMatches admits one narrowly bounded
// source typo only when Kagome already covers the leading visible Han prefix and
// later Han token still requires exact source alignment. The repair removes at
// most two repeated leading consonants, uses the strict syllable converter (not
// arbitrary letter-name expansion), and accepts only a one-kana near-match to
// the dictionary pronunciation. The source word supplies no persisted reading.
func sekaipediaHybridLeadingDictionaryWordMatches(
	word string, regions []sekaipediaHybridRubyRegion,
) bool {
	if !sekaipediaHybridHasDictionaryPrefixAndFallback(regions) {
		return false
	}
	var dictionary []rune
	for _, region := range regions {
		if region.text == "" {
			continue
		}
		if !region.annotate || len(region.dictionaryKana) == 0 {
			break
		}
		dictionary = append(dictionary, region.dictionaryKana...)
	}
	word = strings.ToLower(strings.TrimSpace(word))
	word = strings.ReplaceAll(word, "l", "r")
	if len(dictionary) == 0 || len(word) < 4 || word[0] != word[1] ||
		strings.ContainsRune("aiueon", rune(word[0])) {
		return false
	}
	for removed := 1; removed <= 2 && removed < len(word); removed++ {
		if word[removed-1] != word[0] {
			break
		}
		candidate, ok := romanizeSekaipediaWordToKanaStrict(word[removed:])
		if !ok {
			continue
		}
		candidateRunes := canonicalizeSekaipediaSourceKana(candidate)
		if len(candidateRunes) == len(dictionary) && sekaipediaKanaEditDistance(candidateRunes, dictionary) <= 1 {
			return true
		}
	}
	return false
}

func alignSekaipediaHybridRubyRegions(
	regions []sekaipediaHybridRubyRegion, target []rune, wordBoundaries map[int]bool,
) ([]int, bool) {
	wordBoundaryCounts := make([]int, len(target)+1)
	for index := 1; index <= len(target); index++ {
		wordBoundaryCounts[index] = wordBoundaryCounts[index-1]
		if wordBoundaries[index] {
			wordBoundaryCounts[index]++
		}
	}
	type state struct {
		region int
		pos    int
	}
	memo := map[state]sekaipediaHybridRubyAlignment{}
	var solve func(int, int) sekaipediaHybridRubyAlignment
	solve = func(regionIndex, pos int) sekaipediaHybridRubyAlignment {
		key := state{region: regionIndex, pos: pos}
		if cached, exists := memo[key]; exists {
			return cached
		}
		if regionIndex == len(regions) {
			if pos == len(target) {
				return sekaipediaHybridRubyAlignment{count: 1, signature: sekaipediaSignatureSeed, equivalent: true}
			}
			return sekaipediaHybridRubyAlignment{}
		}
		region := regions[regionIndex]
		candidateEnds := []int{}
		switch {
		case len(region.fixedKana) > 0:
			if end, matches := sekaipediaKanaPrefix(target, pos, region.fixedKana); matches {
				candidateEnds = append(candidateEnds, end)
			}
		case !region.variable:
			candidateEnds = append(candidateEnds, pos)
		default:
			firstEnd := pos + 1
			if len(region.dictionary) > 0 || !region.annotate {
				firstEnd = pos
			}
			for end := firstEnd; end <= len(target); end++ {
				candidateEnds = append(candidateEnds, end)
			}
		}

		// One pass gives a dictionary reading's distance to every end.
		var dictionaryDistances []int
		if len(region.dictionaryKana) > 0 {
			dictionaryDistances = sekaipediaEditDistanceRows(target[pos:], region.dictionaryKana, false, false)
		}
		best := sekaipediaHybridRubyAlignment{}
		// candidateEnds ascend, so the source reading's hash grows with them.
		readingHash, hashedEnd := uint64(sekaipediaSignatureSeed), pos
		for _, end := range candidateEnds {
			for ; hashedEnd < end; hashedEnd++ {
				readingHash = sekaipediaSignatureMix(readingHash, uint64(target[hashedEnd]))
			}
			if region.annotate && len(region.dictionary) == 0 {
				crossedBoundaries := wordBoundaryCounts[end] - wordBoundaryCounts[pos]
				if crossedBoundaries > 1 || crossedBoundaries == 1 && !wordBoundaries[end] {
					continue
				}
			}
			child := solve(regionIndex+1, end)
			if child.count == 0 {
				continue
			}
			edgeCost := 0
			if region.variable {
				if !wordBoundaries[end] && end != len(target) {
					edgeCost += 10
				}
				wordsConsumed := wordBoundaryCounts[end] - wordBoundaryCounts[pos]
				if wordsConsumed > 1 {
					edgeCost += wordsConsumed - 1
				} else if wordsConsumed == 0 {
					edgeCost++
				}
			}
			switch {
			case len(region.dictionaryKana) > 0:
				edgeCost += 100 * dictionaryDistances[end-pos]
			case len(region.fixedKana) > 0:
				edgeCost += 100 * sekaipediaKanaEditDistance(target[pos:end], region.fixedKana)
			}
			cost := edgeCost + child.cost
			signature := child.signature
			if region.annotate && len(region.dictionary) == 0 {
				signature = sekaipediaSignatureMix(sekaipediaSignatureMix(signature, readingHash), uint64(end-pos))
			}
			switch {
			case best.count == 0 || cost < best.cost:
				best = sekaipediaHybridRubyAlignment{
					count: child.count, cost: cost, ends: append([]int{end}, child.ends...),
					signature: signature, equivalent: child.equivalent,
				}
			case cost == best.cost:
				if !best.equivalent || !child.equivalent || best.signature != signature {
					best.equivalent = false
				}
				best.count += child.count
				if best.count > 1 {
					best.count = 2
				}
			}
		}
		memo[key] = best
		return best
	}
	result := solve(0, 0)
	return result.ends, result.count > 0 && result.equivalent && len(result.ends) == len(regions)
}

func kagomeNormalizedRubySpans(surface string) ([]RubySpan, bool) {
	normalized := norm.NFKC.String(surface)
	if normalized == surface || normalized == "" || !containsKanji(normalized) {
		return nil, false
	}
	reading, ok := kagomeKanaReading(normalized)
	if !ok {
		return nil, false
	}
	return rubySpansFromKanaReading(surface, reading)
}

func preferSekaipediaDictionaryRuby(source []RubySpan) []RubySpan {
	result := append([]RubySpan(nil), source...)
	for index := range result {
		if result[index].Reading == "" {
			continue
		}
		if dictionary, ok := kagomeKanaReading(result[index].Text); ok {
			result[index].Reading = string(dictionary)
		}
	}
	return result
}

func rubySpansFromKanaReading(japanese string, targetRunes []rune) ([]RubySpan, bool) {
	regions := sekaipediaRubyRegions(japanese)
	ends, ok := alignSekaipediaRubyRegions(regions, targetRunes, nil)
	if !ok {
		return nil, false
	}
	return sekaipediaRubySpansFromEnds(japanese, regions, targetRunes, ends, false)
}

// sekaipediaRubySpansFromEnds builds ruby from aligned region ends. The exact
// alignment requires every variable region to consume kana; the edit-distance
// alignment requires it of annotated regions only.
func sekaipediaRubySpansFromEnds(
	japanese string, regions []sekaipediaRubyRegion, targetRunes []rune, ends []int, source bool,
) ([]RubySpan, bool) {
	if len(ends) != len(regions) {
		return nil, false
	}
	result := make([]RubySpan, 0, len(regions))
	start := 0
	for index, region := range regions {
		end := ends[index]
		reading := ""
		if end < start || end <= start && (region.annotate || region.variable && !region.free && !source) {
			return nil, false
		}
		if region.annotate {
			reading = string(targetRunes[start:end])
			if !validGeneratedRubyReading(reading) {
				return nil, false
			}
		}
		span := RubySpan{Text: region.text, Reading: reading}
		if len(result) > 0 && result[len(result)-1].Reading == "" && span.Reading == "" {
			result[len(result)-1].Text += span.Text
		} else {
			result = append(result, span)
		}
		start = end
	}
	if start != len(targetRunes) || !rubySpansValidForText(japanese, result) {
		return nil, false
	}
	return result, true
}

// sekaipediaExplicitRubySpans aligns one explicit source reading against one
// visible base. Whole-string alignment fails when the source page separates
// kana tokens with the same punctuation as the base, because the punctuation
// is stripped from the canonical kana; the punctuated fallback restores those
// token boundaries and keeps the shared punctuation as unannotated text.
func sekaipediaExplicitRubySpans(base, reading string) ([]RubySpan, bool) {
	// A gloss written in Han (犯人 glossed 答え) is no pronunciation; reading
	// only its kana would give 犯人 the reading え.
	if containsKanji(reading) {
		return nil, false
	}
	explicit := sekaipediaSourceAlignmentHints{numerals: true, explicit: true}
	if spans, ok := sekaipediaSourceRubySpans(base, canonicalizeSekaipediaSourceKana(reading), explicit); ok {
		return spans, true
	}
	spans, ok := rubySpansFromPunctuatedSourceReading(base, reading)
	if !ok || !sekaipediaSourceRubyPlausible(spans) {
		return nil, false
	}
	return spans, true
}

func rubySpansFromPunctuatedSourceReading(japanese, reading string) ([]RubySpan, bool) {
	japaneseTokens, japaneseSeparators, ok := splitSekaipediaPunctuatedReading(japanese)
	if !ok || len(japaneseTokens) < 2 {
		return nil, false
	}
	readingTokens, readingSeparators, ok := splitSekaipediaPunctuatedReading(reading)
	if !ok || len(readingTokens) != len(japaneseTokens) || !stringSlicesEqual(japaneseSeparators, readingSeparators) {
		return nil, false
	}
	spans := make([]RubySpan, 0, len(japaneseTokens)*2)
	for index, japaneseToken := range japaneseTokens {
		kana := canonicalizeSekaipediaSourceKana(readingTokens[index])
		tokenSpans, aligned := sekaipediaSourceRubySpans(
			japaneseToken, kana, sekaipediaSourceAlignmentHints{numerals: true, explicit: true},
		)
		if !aligned {
			return nil, false
		}
		spans = append(spans, tokenSpans...)
		if index < len(japaneseSeparators) {
			spans = append(spans, RubySpan{Text: japaneseSeparators[index]})
		}
	}
	if !rubySpansValidForText(japanese, spans) {
		return nil, false
	}
	return spans, true
}

func splitSekaipediaPunctuatedReading(value string) ([]string, []string, bool) {
	tokens := []string{}
	separators := []string{}
	var current strings.Builder
	for _, currentRune := range value {
		if sekaipediaRubySeparator(currentRune) {
			if current.Len() == 0 {
				return nil, nil, false
			}
			tokens = append(tokens, current.String())
			current.Reset()
			separators = append(separators, string(currentRune))
			continue
		}
		current.WriteRune(currentRune)
	}
	if current.Len() == 0 {
		return nil, nil, false
	}
	tokens = append(tokens, current.String())
	return tokens, separators, true
}

func sekaipediaRubySeparator(current rune) bool {
	switch current {
	case '、', '，', ',', '・', '。', '！', '？', '!', '?', '.', '‥', '…':
		return true
	}
	return unicode.Is(unicode.Po, current)
}

// alignSekaipediaSourceRubyRegions aligns a source pronunciation that may
// contain typos or missing and extra sounds. Alignments are ranked by kana edit
// distance, then by the number of implausible readings and of visible kana runs
// between two bases that the source drops entirely, then by the number of
// readings with fewer kana than their base has Han, then by visible separators
// off a source word boundary, then by the number of visible kana kept as
// matched anchors. Implausible readings are ranked rather than excluded, so an
// implausible best alignment is rejected instead of replaced by a costlier,
// plausible-looking one. A short reading is only a tie-break: when okurigana
// kana could match in two places (時代柄暗い話題 read "jidaigara kurai wadai"),
// the split that leaves 時代柄暗 two kana loses, while a lone special reading
// such as 王子様 read "kimi" is still kept.
//
// The best alignment must also be trustworthy. An exact (zero-cost) tie that
// only moves an okurigana kana between bases keeps the first, shortest split,
// as 指し示す (sashishimesu) always has; a tie that moves a visible separator,
// a digit run or unmatched Latin text, or a costly tie that gives a base
// another reading, fails closed. For a romanized source it also fails closed
// when it needs more edits than it keeps visible kana, or drops a visible kana
// run between two bases (君が笑った romanized "waratta"): the source is then a
// different or truncated line, and nothing anchors either base. An explicit
// kana reading legitimately skips plain kana between its bases (愛と愛 read
// め め), so those two checks do not apply to it. hints.lenient, the last
// resort when no dictionary reading exists either, searches plausible readings
// only, so an implausible best split does not hide a plausible one; it keeps
// those two checks and also rejects any tie that reads a base differently
// (色欲是空　愛憎交々 split after shikiyoku or after zekuu) and any edit that
// substitutes a kana (本日の…は read "honjitsu wa …"), leaving such a line to
// the dictionary-only fallback.
//
// Each visible kana run absorbs at most sekaipediaKanaRegionSlack more source
// kana than it shows, its edit distances for every end come from one dynamic
// programming pass, and the caller keeps the table within
// sekaipediaAlignmentBudget.
func alignSekaipediaSourceRubyRegions(
	regions []sekaipediaRubyRegion, target []rune, hints sekaipediaSourceAlignmentHints,
) ([]int, bool) {
	diagnosis := hints.diagnosis
	if diagnosis == nil {
		diagnosis = &sekaipediaAlignmentDiagnosis{}
	}
	if !sekaipediaAlignmentWithinBudget(len(target), len(regions)) {
		diagnosis.overBudget = true
		return nil, false
	}
	interior := make([]bool, len(regions))
	annotatedAfter := false
	for index := len(regions) - 1; index >= 0; index-- {
		interior[index] = annotatedAfter
		annotatedAfter = annotatedAfter || regions[index].annotate
	}
	annotatedBefore := false
	for index, region := range regions {
		interior[index] = interior[index] && annotatedBefore && len(region.kana) > 0
		annotatedBefore = annotatedBefore || region.annotate
	}
	type result struct {
		valid       bool
		cost        int
		implausible int
		dropped     int
		short       int
		misses      int
		matched     int
		substituted int
		end         int
		readings    uint64
		breaks      uint64
		ambiguous   bool
		// readingTie marks any tie that reads a base differently, which the
		// lenient pass rejects even at zero cost.
		readingTie bool
	}
	// compare is negative when left is the better alignment and zero on a tie.
	compare := func(left, right result) int {
		for _, pair := range [][2]int{
			{left.cost, right.cost}, {left.implausible, right.implausible}, {left.dropped, right.dropped},
			{left.short, right.short}, {left.misses, right.misses}, {right.matched, left.matched},
		} {
			if pair[0] != pair[1] {
				if pair[0] < pair[1] {
					return -1
				}
				return 1
			}
		}
		return 0
	}
	width := len(target) + 1
	moraPrefix := sekaipediaMoraPrefix(target)
	memo := make([]result, len(regions)*width)
	solved := make([]bool, len(regions)*width)
	var solve func(int, int) result
	solve = func(regionIndex, pos int) result {
		if regionIndex == len(regions) {
			return result{valid: pos == len(target)}
		}
		key := regionIndex*width + pos
		if solved[key] {
			return memo[key]
		}
		region := regions[regionIndex]
		separator := !region.variable && len(region.kana) == 0
		firstEnd, lastEnd := pos, pos
		var distances, indels, matches []int
		switch {
		case len(region.kana) > 0:
			lastEnd = min(len(target), pos+len(region.kana)+sekaipediaKanaRegionSlack)
			distances = sekaipediaEditDistanceRows(target[pos:lastEnd], region.kana, true, true)
			matches = sekaipediaCommonSubsequenceRows(target[pos:lastEnd], region.kana)
			if hints.lenient {
				indels = sekaipediaEditDistanceRowsCost(target[pos:lastEnd], region.kana, true, true, 2)
			}
		case separator:
		default:
			lastEnd = len(target)
			if region.annotate {
				firstEnd = pos + 1
			}
			if len(region.numerals) > 0 {
				lastEnd = min(lastEnd, pos+sekaipediaLongestReading(region.numerals)+4)
				distances = sekaipediaNumeralDistanceRows(target[pos:lastEnd], region.numerals)
			}
		}
		separatorMiss := 0
		if separator && hints.wordBoundaries != nil && pos != len(target) && !hints.wordBoundaries[pos] {
			separatorMiss = 1
		}
		hanCount := utf8.RuneCountInString(region.text)
		readingHash := uint64(sekaipediaSignatureSeed)
		best := result{}
		for end := pos; end <= lastEnd; end++ {
			if end > pos {
				readingHash = sekaipediaSignatureMix(readingHash, uint64(target[end-1]))
			}
			if end < firstEnd {
				continue
			}
			plausible := true
			if region.annotate {
				plausible = sekaipediaPlausibleSourceShape(
					hanCount, end-pos, target[pos], moraPrefix[end]-moraPrefix[pos], false,
				) && !hints.spellsLetterNames(pos, end)
				if hints.lenient && !plausible {
					continue
				}
			}
			child := solve(regionIndex+1, end)
			if !child.valid {
				continue
			}
			candidate := child
			candidate.end = end
			candidate.misses += separatorMiss
			switch {
			case len(region.kana) > 0:
				candidate.cost += distances[end-pos]
				candidate.matched += matches[end-pos]
				if interior[regionIndex] && end == pos {
					candidate.dropped++
				}
				if indels != nil && indels[end-pos] != distances[end-pos] {
					candidate.substituted++
				}
			case len(region.numerals) > 0:
				candidate.cost += distances[end-pos]
				if distances[end-pos] > 0 {
					candidate.substituted++
				}
			}
			if region.annotate {
				candidate.readings = sekaipediaSignatureMix(
					sekaipediaSignatureMix(child.readings, readingHash), uint64(end-pos),
				)
				if !plausible {
					candidate.implausible++
				}
				if end-pos < hanCount {
					candidate.short++
				}
			}
			switch {
			case separator:
				candidate.breaks = sekaipediaSignatureMix(child.breaks, uint64(pos)<<1)
			case region.variable && !region.annotate:
				// A digit run or Latin text: which kana it takes is a break too, so
				// 6日 read "muika" as む|いか or むい|か is a tie, not a reading.
				candidate.breaks = sekaipediaSignatureMix(
					sekaipediaSignatureMix(child.breaks, uint64(pos)<<1|1), uint64(end),
				)
			}
			switch order := compare(candidate, best); {
			case !best.valid || order < 0:
				best = candidate
			case order == 0:
				best.ambiguous = best.ambiguous || candidate.ambiguous || candidate.breaks != best.breaks ||
					best.cost > 0 && candidate.readings != best.readings
				best.readingTie = best.readingTie || candidate.readingTie || candidate.readings != best.readings
				best.substituted = max(best.substituted, candidate.substituted)
			}
		}
		memo[key], solved[key] = best, true
		return best
	}
	aligned := solve(0, 0)
	if !aligned.valid {
		diagnosis.unaligned = true
		return nil, false
	}
	ends := make([]int, len(regions))
	pos := 0
	for index := range regions {
		pos = memo[index*width+pos].end
		ends[index] = pos
	}
	anchored := aligned.dropped == 0 && aligned.matched >= aligned.cost
	if hints.lenient {
		diagnosis.tied = aligned.ambiguous || aligned.readingTie
		diagnosis.substituted = aligned.substituted > 0
		diagnosis.unanchored = !anchored
		return ends, anchored && !aligned.ambiguous && !aligned.readingTie && aligned.substituted == 0
	}
	return ends, !aligned.ambiguous && aligned.implausible == 0 && (hints.explicit || anchored)
}

const sekaipediaSignatureSeed = 0x6a09e667f3bcc908

// sekaipediaSignatureMix folds value into an alignment signature (splitmix64).
func sekaipediaSignatureMix(seed, value uint64) uint64 {
	mixed := seed ^ (value + 0x9e3779b97f4a7c15)
	mixed = (mixed ^ (mixed >> 30)) * 0xbf58476d1ce4e5b9
	mixed = (mixed ^ (mixed >> 27)) * 0x94d049bb133111eb
	return mixed ^ (mixed >> 31)
}

// sekaipediaEditDistanceRows returns, for every prefix length r of
// pronunciation, the edit distance from pronunciation[:r] to expected, in one
// dynamic programming pass. source selects source-mode kana equivalence; with
// freeSokuon an expected sokuon the pronunciation leaves out costs nothing
// (as in sekaipediaKanaEditDistanceMode).
func sekaipediaEditDistanceRows(pronunciation, expected []rune, source, freeSokuon bool) []int {
	return sekaipediaEditDistanceRowsCost(pronunciation, expected, source, freeSokuon, 1)
}

// sekaipediaEditDistanceRowsCost is sekaipediaEditDistanceRows with the given
// substitution cost; 2 counts only insertions and deletions, so a prefix whose
// two distances agree has an optimal alignment without substitutions.
func sekaipediaEditDistanceRowsCost(pronunciation, expected []rune, source, freeSokuon bool, substitutionCost int) []int {
	skip := func(current rune) int {
		if freeSokuon && current == 'っ' {
			return 0
		}
		return 1
	}
	rows := make([]int, len(pronunciation)+1)
	previous := make([]int, len(expected)+1)
	current := make([]int, len(expected)+1)
	for index, shown := range expected {
		previous[index+1] = previous[index] + skip(shown)
	}
	rows[0] = previous[len(expected)]
	for row, spoken := range pronunciation {
		current[0] = row + 1
		for column, shown := range expected {
			substitution := previous[column]
			if !sekaipediaKanaEquivalentMode(spoken, shown, source) {
				substitution += substitutionCost
			}
			current[column+1] = min(substitution, previous[column+1]+1, current[column]+skip(shown))
		}
		previous, current = current, previous
		rows[row+1] = previous[len(expected)]
	}
	return rows
}

// sekaipediaCommonSubsequenceRows returns, for every prefix length r of
// pronunciation, how many kana of visible pronunciation[:r] keeps in order
// (their longest common subsequence under source-mode kana equivalence).
func sekaipediaCommonSubsequenceRows(pronunciation, visible []rune) []int {
	rows := make([]int, len(pronunciation)+1)
	previous := make([]int, len(visible)+1)
	current := make([]int, len(visible)+1)
	for row, spoken := range pronunciation {
		current[0] = 0
		for column, shown := range visible {
			if sekaipediaKanaEquivalentMode(spoken, shown, true) {
				current[column+1] = previous[column] + 1
			} else {
				current[column+1] = max(previous[column+1], current[column])
			}
		}
		previous, current = current, previous
		rows[row+1] = previous[len(visible)]
	}
	return rows
}

// sekaipediaNumeralDistanceRows returns, for every prefix length of
// pronunciation, its smallest edit distance to one of a digit run's numeral
// readings.
func sekaipediaNumeralDistanceRows(pronunciation []rune, numerals [][]rune) []int {
	var best []int
	for _, reading := range numerals {
		rows := sekaipediaEditDistanceRows(pronunciation, reading, true, false)
		if best == nil {
			best = rows
			continue
		}
		for index := range best {
			best[index] = min(best[index], rows[index])
		}
	}
	return best
}

func sekaipediaHasJapaneseScript(value string) bool {
	for _, current := range value {
		if unicode.In(current, unicode.Han, unicode.Hiragana, unicode.Katakana) {
			return true
		}
	}
	return false
}

func sekaipediaTransientRomanizedWords(japanese, romanized string) ([]string, bool) {
	sourceWords := sekaipediaASCIIWords(norm.NFKC.String(html.UnescapeString(japanese)))
	remaining := make(map[string]int, len(sourceWords))
	for _, word := range sourceWords {
		remaining[word]++
	}
	words := sekaipediaASCIIWords(norm.NFKC.String(html.UnescapeString(romanized)))
	result := make([]string, 0, len(words))
	for _, word := range words {
		if remaining[word] > 0 {
			remaining[word]--
			continue
		}
		result = append(result, word)
	}
	return result, true
}

func sekaipediaASCIIWords(value string) []string {
	words := []string{}
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	for _, character := range strings.ToLower(value) {
		switch character {
		case 'ā', 'â':
			current.WriteString("aa")
			continue
		case 'ī', 'î':
			current.WriteString("ii")
			continue
		case 'ū', 'û':
			current.WriteString("uu")
			continue
		case 'ē', 'ê':
			current.WriteString("ee")
			continue
		case 'ō', 'ô':
			current.WriteString("ou")
			continue
		}
		if character <= unicode.MaxASCII && (unicode.IsLetter(character) || unicode.IsDigit(character)) {
			current.WriteRune(character)
			continue
		}
		if unicode.Is(unicode.Mn, character) || unicode.Is(unicode.Mc, character) {
			continue
		}
		flush()
	}
	flush()
	return words
}

func sekaipediaASCIIWordHasLetter(value string) bool {
	for _, current := range value {
		if current >= 'a' && current <= 'z' {
			return true
		}
	}
	return false
}

func sekaipediaRubyRegions(japanese string) []sekaipediaRubyRegion {
	regions := []sekaipediaRubyRegion{}
	var text strings.Builder
	variable := false
	annotate := false
	flush := func() {
		if text.Len() == 0 {
			return
		}
		regions = append(regions, sekaipediaRubyRegion{
			text: text.String(), variable: variable, annotate: annotate,
		})
		text.Reset()
		variable = false
		annotate = false
	}
	for _, current := range japanese {
		if sekaipediaIsKana(current) {
			flush()
			kana := []rune(katakanaToHiragana(string(current)))
			if len(regions) > 0 && len(regions[len(regions)-1].kana) > 0 {
				regions[len(regions)-1].text += string(current)
				regions[len(regions)-1].kana = append(regions[len(regions)-1].kana, kana...)
			} else {
				regions = append(regions, sekaipediaRubyRegion{text: string(current), kana: kana})
			}
			continue
		}
		nextAnnotate := model.LyricsSourceRubyBaseRune(current)
		nextVariable := nextAnnotate || unicode.IsNumber(current)
		if text.Len() > 0 && (nextVariable != variable || nextAnnotate != annotate) {
			flush()
		}
		text.WriteRune(current)
		variable = nextVariable
		annotate = nextAnnotate
	}
	flush()
	for index := range regions {
		if len(regions[index].kana) > 0 {
			regions[index].kana = canonicalizeSekaipediaKana(regions[index].kana)
		}
	}
	return regions
}

func sekaipediaIsKana(value rune) bool {
	return unicode.In(value, unicode.Hiragana, unicode.Katakana) || value == 'ー'
}

func canonicalizeSekaipediaSourceKana(value string) []rune {
	return canonicalizeSekaipediaKana([]rune(norm.NFKC.String(strings.TrimSpace(value))))
}

func canonicalizeSekaipediaKana(input []rune) []rune {
	result := make([]rune, 0, len(input))
	for _, current := range input {
		if current >= 'ァ' && current <= 'ヶ' {
			current -= 'ァ' - 'ぁ'
		}
		if current == 'ー' {
			if vowel := sekaipediaKanaVowel(result); vowel != 0 {
				result = append(result, vowel)
			}
			continue
		}
		if unicode.In(current, unicode.Hiragana) {
			result = append(result, current)
		}
	}
	return result
}

func sekaipediaKanaVowel(value []rune) rune {
	if len(value) == 0 {
		return 0
	}
	current := value[len(value)-1]
	switch {
	case strings.ContainsRune("ぁあかがさざただなはばぱまゃやらゎわ", current):
		return 'あ'
	case strings.ContainsRune("ぃいきぎしじちぢにひびぴみりゐ", current):
		return 'い'
	case strings.ContainsRune("ぅうくぐすずつづぬふぶぷむゅゆるゔ", current):
		return 'う'
	case strings.ContainsRune("ぇえけげせぜてでねへべぺめれゑ", current):
		return 'え'
	case strings.ContainsRune("ぉおこごそぞとのほぼぽもょよろを", current):
		return 'お'
	default:
		return 0
	}
}

// alignSekaipediaRubyRegions finds the unique exact alignment of target
// against regions. With source hints it considers only plausible readings
// (sekaipediaSingleMoraAllowed decides single-mora ones) and reads a digit run
// as its longest matching numeral, so 6日 read "muika" is むい|か rather than
// む|いか.
func alignSekaipediaRubyRegions(
	regions []sekaipediaRubyRegion, target []rune, source *sekaipediaSourceAlignmentHints,
) ([]int, bool) {
	plausibleOnly := source != nil
	width := len(target) + 1
	memo := make([]sekaipediaRubyAlignment, len(regions)*width)
	solved := make([]bool, len(regions)*width)
	dictionaries := make([][]rune, len(regions))
	dictionaryKnown := make([]bool, len(regions))
	var moraPrefix []int
	if plausibleOnly {
		moraPrefix = sekaipediaMoraPrefix(target)
	}
	var solve func(int, int) sekaipediaRubyAlignment
	solve = func(regionIndex, pos int) sekaipediaRubyAlignment {
		if regionIndex == len(regions) {
			if pos == len(target) {
				return sekaipediaRubyAlignment{count: 1, ends: []int{}}
			}
			return sekaipediaRubyAlignment{}
		}
		key := regionIndex*width + pos
		if solved[key] {
			return memo[key]
		}
		region := regions[regionIndex]
		candidateEnds := []int{}
		switch {
		case len(region.kana) > 0:
			if end, matches := sekaipediaKanaPrefixMode(target, pos, region.kana, plausibleOnly); matches {
				candidateEnds = append(candidateEnds, end)
			}
		case !region.variable:
			candidateEnds = append(candidateEnds, pos)
		default:
			nextFixed := []rune(nil)
			for next := regionIndex + 1; next < len(regions); next++ {
				switch {
				case len(regions[next].kana) > 0:
					nextFixed = regions[next].kana
				case regions[next].variable:
					next = len(regions)
				default:
					continue
				}
				break
			}
			firstEnd := pos + 1
			if region.free {
				firstEnd = pos
			}
			hanCount := utf8.RuneCountInString(region.text)
			for end := firstEnd; end <= len(target); end++ {
				if region.annotate && plausibleOnly && (!sekaipediaPlausibleSourceShape(
					hanCount, end-pos, target[pos], moraPrefix[end]-moraPrefix[pos],
					sekaipediaSingleMoraAllowed(regions, regionIndex, pos, end, source),
				) || source.spellsLetterNames(pos, end)) {
					continue
				}
				if len(nextFixed) == 0 {
					candidateEnds = append(candidateEnds, end)
					continue
				}
				if _, matches := sekaipediaKanaPrefixMode(target, end, nextFixed, plausibleOnly); matches {
					candidateEnds = append(candidateEnds, end)
				}
			}
			if region.annotate {
				if !dictionaryKnown[regionIndex] {
					if reading, ok := kagomeKanaReading(region.text); ok {
						dictionaries[regionIndex] = reading
					}
					dictionaryKnown[regionIndex] = true
				}
				candidateEnds = constrainSekaipediaRubyCandidateEnds(dictionaries[regionIndex], target, pos, candidateEnds)
			} else if len(region.numerals) > 0 {
				candidateEnds = sekaipediaNumeralCandidateEnds(region.numerals, target, pos, candidateEnds)
			}
		}
		result := sekaipediaRubyAlignment{}
		for _, end := range candidateEnds {
			child := solve(regionIndex+1, end)
			if child.count == 0 {
				continue
			}
			if result.count == 0 {
				result.ends = append([]int{end}, child.ends...)
			}
			result.count += child.count
			if result.count > 1 {
				result.count = 2
				break
			}
		}
		memo[key], solved[key] = result, true
		return result
	}
	result := solve(0, 0)
	return result.ends, result.count == 1 && len(result.ends) == len(regions)
}

// constrainSekaipediaRubyCandidateEnds keeps the ends at which a base reads as
// its dictionary reading, when any does; dictionary is nil when the base has
// none.
func constrainSekaipediaRubyCandidateEnds(dictionary []rune, target []rune, start int, candidateEnds []int) []int {
	if len(dictionary) == 0 {
		return candidateEnds
	}
	matched := make([]int, 0, len(candidateEnds))
	for _, end := range candidateEnds {
		if end > start && end <= len(target) && sekaipediaKanaSlicesEqual(target[start:end], dictionary) {
			matched = append(matched, end)
		}
	}
	if len(matched) == 0 {
		// Keep the fixed-source alignment available when its pronunciation differs.
		// Dictionary-first replacement is applied later only to exact covered bases.
		return candidateEnds
	}
	return matched
}

func sekaipediaKanaPrefix(target []rune, start int, expected []rune) (int, bool) {
	return sekaipediaKanaPrefixMode(target, start, expected, false)
}

func sekaipediaKanaPrefixMode(target []rune, start int, expected []rune, source bool) (int, bool) {
	if start < 0 || len(target)-start < len(expected) {
		return start, false
	}
	for index, current := range expected {
		if !sekaipediaKanaEquivalentMode(current, target[start+index], source) {
			return start, false
		}
	}
	return start + len(expected), true
}

func sekaipediaKanaEquivalent(left, right rune) bool {
	return sekaipediaKanaEquivalentMode(left, right, false)
}

// sekaipediaKanaEquivalentMode compares visible kana with a pronunciation.
// Source mode (romaji or explicit kana aligned against visible text) also
// reads the counter ヶ/ヵ (一ヶ月, 三ヶ日, ヶ所) and the archaic ヱ/ヰ as the
// kana that the source spells.
func sekaipediaKanaEquivalentMode(left, right rune, source bool) bool {
	if left == right {
		return true
	}
	if left > right {
		left, right = right, left
	}
	switch left {
	case 'ぁ':
		return right == 'あ'
	case 'ぃ':
		return right == 'い'
	case 'ぅ':
		return right == 'う'
	case 'ぇ':
		return right == 'え'
	case 'ぉ':
		return right == 'お'
	case 'じ':
		return right == 'ぢ'
	case 'ず':
		return right == 'づ'
	case 'は':
		return right == 'わ'
	case 'え':
		return right == 'へ' || source && right == 'ゑ'
	case 'お':
		return right == 'を'
	case 'い':
		return source && right == 'ゐ'
	case 'か', 'が', 'こ':
		return source && (right == 'ゖ' || right == 'ゕ')
	}
	return false
}

func sekaipediaKanaSlicesEqual(left, right []rune) bool {
	return sekaipediaKanaSlicesEqualMode(left, right, false)
}

func sekaipediaKanaSlicesEqualMode(left, right []rune, source bool) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sekaipediaKanaEquivalentMode(left[index], right[index], source) {
			return false
		}
	}
	return true
}

func sekaipediaKanaEditDistance(left, right []rune) int {
	return sekaipediaKanaEditDistanceMode(left, right, false, false)
}

// sekaipediaKanaEditDistanceMode is the edit distance from a pronunciation
// (left) to visible or expected kana (right). With visible set, a visible
// sokuon the source leaves out costs nothing: romaji often drops gemination
// (星ッ read "sei", 殴ったって spelled "nagutatte").
func sekaipediaKanaEditDistanceMode(left, right []rune, source, visible bool) int {
	skip := func(current rune) int {
		if visible && current == 'っ' {
			return 0
		}
		return 1
	}
	previous := make([]int, len(right)+1)
	for index, current := range right {
		previous[index+1] = previous[index] + skip(current)
	}
	for leftIndex, leftRune := range left {
		current := make([]int, len(right)+1)
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range right {
			substitution := previous[rightIndex]
			if !sekaipediaKanaEquivalentMode(leftRune, rightRune, source) {
				substitution++
			}
			deletion := previous[rightIndex+1] + 1
			insertion := current[rightIndex] + skip(rightRune)
			current[rightIndex+1] = min(substitution, deletion, insertion)
		}
		previous = current
	}
	return previous[len(right)]
}

func sekaipediaDictionaryKana(japanese string) ([]rune, bool) {
	spans, err := generateRubySpans(japanese)
	if err != nil {
		return nil, false
	}
	var reading strings.Builder
	for _, span := range spans {
		if span.Reading != "" {
			reading.WriteString(span.Reading)
			continue
		}
		for _, current := range span.Text {
			switch {
			case sekaipediaIsKana(current):
				reading.WriteRune(current)
			case unicode.In(current, unicode.Han) || unicode.IsDigit(current):
				return nil, false
			}
		}
	}
	result := canonicalizeSekaipediaKana([]rune(reading.String()))
	return result, len(result) > 0
}

var sekaipediaRomajiKana = map[string]string{
	"kya": "きゃ", "kyu": "きゅ", "kyo": "きょ", "gya": "ぎゃ", "gyu": "ぎゅ", "gyo": "ぎょ",
	"sha": "しゃ", "shu": "しゅ", "sho": "しょ", "sya": "しゃ", "syu": "しゅ", "syo": "しょ",
	"ja": "じゃ", "ju": "じゅ", "jo": "じょ", "jya": "じゃ", "jyu": "じゅ", "jyo": "じょ",
	"cha": "ちゃ", "chu": "ちゅ", "cho": "ちょ", "cya": "ちゃ", "cyu": "ちゅ", "cyo": "ちょ",
	"nya": "にゃ", "nyu": "にゅ", "nyo": "にょ", "hya": "ひゃ", "hyu": "ひゅ", "hyo": "ひょ",
	"bya": "びゃ", "byu": "びゅ", "byo": "びょ", "pya": "ぴゃ", "pyu": "ぴゅ", "pyo": "ぴょ",
	"mya": "みゃ", "myu": "みゅ", "myo": "みょ", "rya": "りゃ", "ryu": "りゅ", "ryo": "りょ",
	"dya": "ぢゃ", "dyu": "ぢゅ", "dyo": "ぢょ", "she": "しぇ", "je": "じぇ", "che": "ちぇ",
	"tsa": "つぁ", "tsi": "つぃ", "tse": "つぇ", "tso": "つぉ", "thi": "てぃ", "the": "てぇ", "tho": "てょ",
	"dhi": "でぃ", "dhe": "でぇ", "dho": "でょ", "kwa": "くぁ", "kwi": "くぃ", "kwe": "くぇ", "kwo": "くぉ",
	"gwa": "ぐぁ", "gwi": "ぐぃ", "gwe": "ぐぇ", "gwo": "ぐぉ",
	"shi": "し", "chi": "ち", "tsu": "つ", "tzu": "つ", "dzi": "ぢ", "dzu": "づ",
	"fa": "ふぁ", "fi": "ふぃ", "fe": "ふぇ", "fo": "ふぉ", "fyu": "ふゅ",
	"va": "ゔぁ", "vi": "ゔぃ", "vu": "ゔ", "ve": "ゔぇ", "vo": "ゔぉ",
	"ji": "じ", "ti": "てぃ", "tu": "とぅ", "di": "でぃ", "du": "どぅ", "wi": "うぃ", "wu": "う", "we": "うぇ", "ye": "いぇ",
	"a": "あ", "i": "い", "u": "う", "e": "え", "o": "お",
	"ka": "か", "ki": "き", "ku": "く", "ke": "け", "ko": "こ",
	"ga": "が", "gi": "ぎ", "gu": "ぐ", "ge": "げ", "go": "ご",
	"sa": "さ", "si": "し", "su": "す", "se": "せ", "so": "そ",
	"za": "ざ", "zi": "じ", "zu": "ず", "ze": "ぜ", "zo": "ぞ",
	"ta": "た", "te": "て", "to": "と", "da": "だ", "de": "で", "do": "ど",
	"na": "な", "ni": "に", "nu": "ぬ", "ne": "ね", "no": "の",
	"ha": "は", "hi": "ひ", "fu": "ふ", "hu": "ふ", "he": "へ", "ho": "ほ",
	"ba": "ば", "bi": "び", "bu": "ぶ", "be": "べ", "bo": "ぼ",
	"pa": "ぱ", "pi": "ぴ", "pu": "ぷ", "pe": "ぺ", "po": "ぽ",
	"ma": "ま", "mi": "み", "mu": "む", "me": "め", "mo": "も",
	"ya": "や", "yu": "ゆ", "yo": "よ", "ra": "ら", "ri": "り", "ru": "る", "re": "れ", "ro": "ろ",
	"wa": "わ", "wo": "を",
}

func sekaipediaIgnorableRomanizedWord(value string) bool {
	return len(value) == 1 && value[0] >= 'a' && value[0] <= 'z' && !strings.ContainsRune("aiueon", rune(value[0]))
}

func sekaipediaRomanizedLetterNames(value string) (string, bool) {
	letterNames := map[byte]string{
		'b': "びー", 'c': "しー", 'd': "でぃー", 'f': "えふ", 'g': "じー", 'h': "えいち",
		'j': "じぇー", 'k': "けー", 'm': "えむ", 'n': "えぬ", 'p': "ぴー", 'q': "きゅー", 'r': "あーる",
		's': "えす", 't': "てぃー", 'v': "ぶい", 'w': "だぶりゅー", 'x': "えっくす", 'y': "わい", 'z': "ぜっと",
	}
	if value == "" || len(value) > 8 {
		return "", false
	}
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		name, ok := letterNames[value[index]]
		if !ok {
			return "", false
		}
		result.WriteString(name)
	}
	return result.String(), result.Len() > 0
}

func romanizeSekaipediaWordToKana(value string) (string, bool) {
	return romanizeSekaipediaWordToKanaMode(value, true)
}

func romanizeSekaipediaWordToKanaStrict(value string) (string, bool) {
	return romanizeSekaipediaWordToKanaMode(value, false)
}

func romanizeSekaipediaWordToKanaMode(value string, allowLetterNames bool) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "l", "r")
	if value == "" {
		return "", false
	}
	if value == "m" {
		return "ん", true
	}
	if allowLetterNames {
		if letterName, ok := map[string]string{
			"h": "えいち",
			"r": "あーる",
			"w": "だぶりゅー",
			"y": "わい",
		}[value]; ok {
			return letterName, true
		}
	}
	for _, current := range value {
		if current < 'a' || current > 'z' {
			return "", false
		}
	}
	var result strings.Builder
	for index := 0; index < len(value); {
		if index+1 < len(value) && value[index] == value[index+1] && value[index] != 'a' && value[index] != 'i' &&
			value[index] != 'u' && value[index] != 'e' && value[index] != 'o' && value[index] != 'n' {
			result.WriteRune('っ')
			index++
			continue
		}
		if strings.HasPrefix(value[index:], "tch") {
			result.WriteRune('っ')
			index++
			continue
		}
		if value[index] == 'n' {
			if index+1 == len(value) {
				result.WriteRune('ん')
				index++
				continue
			}
			next := value[index+1]
			if next == 'n' || !strings.ContainsRune("aiueoy", rune(next)) {
				result.WriteRune('ん')
				index++
				continue
			}
		}
		matched := false
		for length := 3; length >= 1; length-- {
			if index+length > len(value) {
				continue
			}
			if kana, exists := sekaipediaRomajiKana[value[index:index+length]]; exists {
				result.WriteString(kana)
				index += length
				matched = true
				break
			}
		}
		if !matched {
			if !allowLetterNames {
				return "", false
			}
			letterNames, ok := sekaipediaRomanizedLetterNames(value[index:])
			if !ok {
				return "", false
			}
			result.WriteString(letterNames)
			return result.String(), true
		}
	}
	return result.String(), result.Len() > 0
}

// sekaipediaNumeralCandidateEnds keeps the longest end at which a digit run
// reads as one of its numeral pronunciations; the exact alignment needs one.
// 6 reads む or むい and 10 reads と or とお, so the shortest would move the
// rest onto the next base (6{日|いか}, 10{年|おねん}).
func sekaipediaNumeralCandidateEnds(numerals [][]rune, target []rune, start int, candidateEnds []int) []int {
	longest := -1
	for _, end := range candidateEnds {
		if end < start || end > len(target) || end <= longest {
			continue
		}
		for _, reading := range numerals {
			if sekaipediaKanaSlicesEqualMode(target[start:end], reading, true) {
				longest = end
				break
			}
		}
	}
	if longest < 0 {
		return nil
	}
	return []int{longest}
}

func sekaipediaLongestReading(readings [][]rune) int {
	longest := 0
	for _, reading := range readings {
		longest = max(longest, len(reading))
	}
	return longest
}

var (
	sekaipediaDigitKana = [10]string{"ぜろ", "いち", "に", "さん", "よん", "ご", "ろく", "なな", "はち", "きゅう"}
	// Alternative final readings of a digit: 4時 よじ, 7時 しちじ, 9月 くがつ,
	// 1つ ひとつ, 2人 ふたり, 3日 みっか, 0時 れいじ.
	sekaipediaDigitVariants = [10][]string{
		{"れい", "まる"}, {"ひと", "いっ"}, {"ふた", "ふつ"}, {"み", "みっ"}, {"よ", "し", "よっ"},
		{"いつ"}, {"む", "むっ", "むい", "ろっ"}, {"しち", "なの"}, {"や", "やっ", "よう", "はっ"}, {"く", "ここの"},
	}
)

// sekaipediaNumeralReadings returns candidate pronunciations of a digit run:
// the Japanese number (with the usual sound changes on its last digit or
// unit), native counting for small numbers, and digit by digit. It returns
// nil for anything that is not a plain ASCII digit run.
func sekaipediaNumeralReadings(digits string) [][]rune {
	if !sekaipediaDigitWord(digits) || len(digits) > 12 {
		return nil
	}
	seen := map[string]bool{}
	result := [][]rune{}
	add := func(value string) {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, []rune(value))
		}
	}
	if len(digits) > 1 {
		for _, zero := range []string{"ぜろ", "れい", "まる"} {
			var reading strings.Builder
			for _, current := range digits {
				if current == '0' {
					reading.WriteString(zero)
				} else {
					reading.WriteString(sekaipediaDigitKana[current-'0'])
				}
			}
			add(reading.String())
		}
	}
	if len(digits) > 1 && digits[0] == '0' || len(digits) > 8 {
		return result
	}
	value := 0
	for _, current := range digits {
		value = value*10 + int(current-'0')
	}
	standard := sekaipediaJapaneseNumber(value)
	add(standard)
	switch {
	case value < 10:
		for _, variant := range sekaipediaDigitVariants[value] {
			add(variant)
		}
	case value%10 != 0:
		prefix := sekaipediaJapaneseNumber(value - value%10)
		for _, variant := range sekaipediaDigitVariants[value%10] {
			add(prefix + variant)
		}
	case value == 10:
		add("とお")
		add("と")
	case value == 20:
		add("はた")
		add("はつ")
	}
	// Gemination before a counter: 一回 いっかい, 六本 ろっぽん, 八個 はっこ,
	// 十分 じゅっぷん/じっぷん, 百回 ひゃっかい.
	for _, pair := range [][2]string{
		{"いち", "いっ"}, {"ろく", "ろっ"}, {"はち", "はっ"}, {"じゅう", "じゅっ"}, {"じゅう", "じっ"}, {"ひゃく", "ひゃっ"},
	} {
		if strings.HasSuffix(standard, pair[0]) {
			add(strings.TrimSuffix(standard, pair[0]) + pair[1])
		}
	}
	return result
}

func sekaipediaJapaneseNumber(value int) string {
	if value == 0 {
		return "ぜろ"
	}
	var result strings.Builder
	if value >= 10000 {
		result.WriteString(sekaipediaJapaneseNumber(value / 10000))
		result.WriteString("まん")
		value %= 10000
	}
	thousands := [10]string{"", "せん", "にせん", "さんぜん", "よんせん", "ごせん", "ろくせん", "ななせん", "はっせん", "きゅうせん"}
	hundreds := [10]string{"", "ひゃく", "にひゃく", "さんびゃく", "よんひゃく", "ごひゃく", "ろっぴゃく", "ななひゃく", "はっぴゃく", "きゅうひゃく"}
	result.WriteString(thousands[value/1000])
	result.WriteString(hundreds[value/100%10])
	if tens := value / 10 % 10; tens > 0 {
		if tens > 1 {
			result.WriteString(sekaipediaDigitKana[tens])
		}
		result.WriteString("じゅう")
	}
	if units := value % 10; units > 0 {
		result.WriteString(sekaipediaDigitKana[units])
	}
	return result.String()
}
