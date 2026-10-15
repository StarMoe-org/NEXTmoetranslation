package lyricssource

import (
	"fmt"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

func stripSekaipediaCitationOnlyInterstitial(value string) (string, bool) {
	if value == "" || !utf8.ValidString(value) || len(value) > maxSekaipediaIgnoredCitationBytes {
		return "", false
	}
	var whitespace strings.Builder
	matched := false
	for value != "" {
		leadingEnd := len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
		whitespace.WriteString(value[:leadingEnd])
		value = value[leadingEnd:]
		if value == "" {
			break
		}
		consumed, ok := consumeSekaipediaIgnoredCitationPrefix(value)
		if !ok || consumed <= 0 {
			return "", false
		}
		matched = true
		value = value[consumed:]
	}
	return whitespace.String(), matched
}

func parseSekaipediaLyricsHead(template sekaipediaTemplate) (sekaipediaLyricsHead, error) {
	params, err := sekaipediaNamedParameters(template.fields, map[string]bool{
		"columns": true, "japanese": true, "romaji": true, "english": true, "english 2": true,
	})
	if err != nil || params["columns"] == "" {
		return sekaipediaLyricsHead{}, ErrUnsupportedTable
	}
	romajiDeclared := params["romaji"] != ""
	japaneseHeader := params["japanese"]
	if (japaneseHeader != "" && !equalFoldSekaipediaHeader(
		japaneseHeader, "Japanese lyrics", "Japanese lyric", "Japanese/German lyrics",
	)) || romajiDeclared && !equalFoldSekaipediaHeader(params["romaji"], "Romanized lyrics") {
		return sekaipediaLyricsHead{}, ErrUnsupportedTable
	}
	columns := strings.Split(params["columns"], ",")
	if len(columns) < 1 || len(columns) > 4 {
		return sekaipediaLyricsHead{}, ErrUnsupportedTable
	}
	seen := map[string]bool{}
	for _, column := range columns {
		column = strings.TrimSpace(column)
		if column != "japanese" && column != "romaji" && column != "english" && column != "english 2" || seen[column] {
			return sekaipediaLyricsHead{}, ErrUnsupportedTable
		}
		seen[column] = true
	}
	englishDeclared := params["english"] != ""
	englishSource := englishDeclared && equalFoldSekaipediaHeader(params["english"], "English lyrics")
	if englishDeclared && !englishSource && !equalFoldSekaipediaHeader(
		params["english"],
		"English translation", "EnglishTranslation", "English translation 1", "Official English translation",
	) {
		return sekaipediaLyricsHead{}, ErrUnsupportedTable
	}
	englishTwoDeclared := params["english 2"] != ""
	if englishTwoDeclared && !equalFoldSekaipediaHeader(params["english 2"], "English translation 2") {
		return sekaipediaLyricsHead{}, ErrUnsupportedTable
	}
	if seen["romaji"] != romajiDeclared || seen["english"] != englishDeclared ||
		seen["english 2"] != englishTwoDeclared || englishTwoDeclared && !englishDeclared {
		return sekaipediaLyricsHead{}, ErrUnsupportedTable
	}
	if seen["japanese"] {
		return sekaipediaLyricsHead{sourceColumn: "japanese", hasRomaji: romajiDeclared, declared: cloneDeclaredSekaipediaColumns(seen)}, nil
	}
	if len(columns) == 1 && seen["english"] && englishSource && !romajiDeclared && !englishTwoDeclared {
		return sekaipediaLyricsHead{sourceColumn: "english", englishSource: true, declared: cloneDeclaredSekaipediaColumns(seen)}, nil
	}
	return sekaipediaLyricsHead{}, ErrUnsupportedTable
}

func cloneDeclaredSekaipediaColumns(seen map[string]bool) map[string]bool {
	result := make(map[string]bool, len(seen))
	for column, declared := range seen {
		result[column] = declared
	}
	return result
}

func parseSekaipediaLyricColumn(value string, set sekaipediaSingerSet) ([]sekaipediaColumnLine, error) {
	value = strings.Trim(value, " \t\r\n")
	if value == "" || len(value) > maxExtractedTextBytes || !utf8.ValidString(value) || strings.ContainsRune(value, '\r') {
		return nil, ErrUnsupportedTable
	}
	lines := []sekaipediaColumnLine{}
	current := sekaipediaColumnLine{}
	flush := func() error {
		if len(current.segments) == 0 {
			return nil
		}
		for _, segment := range current.segments {
			if segment.text == "" || len(segment.text) > maxExtractedLineBytes || strings.ContainsRune(segment.text, '\n') {
				return ErrUnsupportedTable
			}
		}
		lines = append(lines, current)
		current = sekaipediaColumnLine{}
		return nil
	}
	formatStack := []string{}
	pendingPunctuation := ""
	consumeWhitespace := func(value string) error {
		for cursor := 0; cursor < len(value); {
			lowerRemaining := strings.ToLower(value[cursor:])
			formatMarker := ""
			formatMarkerBytes := 0
			switch {
			case strings.HasPrefix(lowerRemaining, "'''"):
				formatMarker, formatMarkerBytes = "bold", 3
			case strings.HasPrefix(lowerRemaining, "''"):
				formatMarker, formatMarkerBytes = "italic", 2
			}
			if formatMarker != "" {
				if len(formatStack) > 0 && formatStack[len(formatStack)-1] == formatMarker {
					formatStack = formatStack[:len(formatStack)-1]
				} else {
					for _, active := range formatStack {
						if active == formatMarker {
							return ErrUnsupportedTable
						}
					}
					if len(formatStack) >= 8 {
						return ErrUnsupportedTable
					}
					formatStack = append(formatStack, formatMarker)
				}
				cursor += formatMarkerBytes
				continue
			}
			if tag, closing, consumed, matched := parseSekaipediaLyricFormattingTag(value[cursor:]); matched {
				if closing {
					if len(formatStack) == 0 || formatStack[len(formatStack)-1] != tag {
						return ErrUnsupportedTable
					}
					formatStack = formatStack[:len(formatStack)-1]
				} else {
					if len(formatStack) >= 8 {
						return ErrUnsupportedTable
					}
					formatStack = append(formatStack, tag)
				}
				cursor += consumed
				continue
			}
			currentRune, size := utf8.DecodeRuneInString(value[cursor:])
			if currentRune == utf8.RuneError && size == 1 {
				return ErrUnsupportedTable
			}
			cursor += size
			switch currentRune {
			case '\n':
				if err := flush(); err != nil {
					return err
				}
			default:
				if !unicode.IsSpace(currentRune) {
					return ErrUnsupportedTable
				}
				if len(current.segments) > 0 {
					current.segments[len(current.segments)-1].text += string(currentRune)
				}
			}
		}
		return nil
	}
	consumeLyricInterstitial := func(value string) error {
		lines, leadingBreak, trailingBreak, err := parseSekaipediaLyricInterstitial(value)
		if err != nil {
			return err
		}
		if leadingBreak {
			if err := flush(); err != nil {
				return err
			}
		}
		for index, rendered := range lines {
			if pendingPunctuation != "" {
				rendered = pendingPunctuation + rendered
				pendingPunctuation = ""
			}
			current.segments = append(current.segments, sekaipediaColumnSegment{text: rendered})
			if index+1 < len(lines) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if trailingBreak {
			if err := flush(); err != nil {
				return err
			}
		}
		return nil
	}
	consumeInterstitial := func(value string) error {
		if whitespace, ok := stripSekaipediaCitationOnlyInterstitial(value); ok {
			return consumeWhitespace(whitespace)
		}
		leading, punctuation, trailing, ok := splitSekaipediaInterstitialPunctuation(value)
		if ok {
			if err := consumeWhitespace(leading); err != nil {
				return err
			}
			if len(current.segments) > 0 {
				current.segments[len(current.segments)-1].text += punctuation
			} else {
				pendingPunctuation += punctuation
			}
			return consumeWhitespace(trailing)
		}
		linesBefore := len(lines)
		currentBefore := current
		currentBefore.segments = append([]sekaipediaColumnSegment(nil), current.segments...)
		for index := range currentBefore.segments {
			currentBefore.segments[index].performerIDs = append([]string(nil), currentBefore.segments[index].performerIDs...)
		}
		formatStackBefore := append([]string(nil), formatStack...)
		if err := consumeWhitespace(value); err == nil {
			return nil
		}
		lines = lines[:linesBefore]
		current = currentBefore
		formatStack = formatStackBefore
		return consumeLyricInterstitial(value)
	}

	cursor := 0
	templateOrdinal := 0
	for cursor < len(value) {
		next := nextSekaipediaTemplateStart(value, cursor)
		if next == -2 {
			return nil, ErrUnsupportedTable
		}
		if next < 0 {
			if err := consumeInterstitial(value[cursor:]); err != nil {
				return nil, err
			}
			cursor = len(value)
			break
		}
		if err := consumeInterstitial(value[cursor:next]); err != nil {
			return nil, err
		}
		_, end, inner, ok := balancedSekaipediaTemplateAt(value, next)
		if !ok {
			return nil, ErrUnsupportedTable
		}
		templateOrdinal++
		fields, ok := splitTopLevelSekaipediaFields(inner, "|")
		if !ok || len(fields) == 0 {
			return nil, ErrUnsupportedTable
		}
		fields, ok = unwrapSekaipediaSingleNestedTemplate(fields)
		if !ok || len(fields) == 0 {
			return nil, ErrUnsupportedTable
		}
		name := strings.TrimSpace(fields[0])
		performerIDs := []string{}
		text := ""
		var explicitReading []rune
		switch {
		case strings.EqualFold(name, "Lyric"):
			var err error
			performerIDs, text, err = parseSekaipediaLyricTemplateFields(fields, set)
			if err != nil {
				return nil, err
			}
			if len(fields) == 5 && strings.EqualFold(strings.TrimSpace(fields[2]), "ruby") {
				candidate := canonicalizeSekaipediaSourceKana(fields[4])
				if validGeneratedRubyReading(string(candidate)) {
					explicitReading = candidate
				}
			}
		case strings.EqualFold(name, "ruby"):
			if len(fields) != 3 || strings.TrimSpace(fields[2]) == "" || len(fields[2]) > maxExtractedLineBytes {
				return nil, ErrUnsupportedTable
			}
			if strings.TrimSpace(fields[1]) == "" {
				if _, err := parseSekaipediaInlineLyricEvents(strings.TrimSpace(fields[2]), nil, set, 0); err != nil {
					return nil, err
				}
				reading := canonicalizeSekaipediaSourceKana(fields[2])
				if len(current.segments) > 0 && validGeneratedRubyReading(string(reading)) {
					previous := &current.segments[len(current.segments)-1]
					if len(previous.ruby) == 0 {
						if spans, aligned := rubySpansFromKanaReading(previous.text, reading); aligned {
							previous.ruby = markRubyReadingEvidence(
								spans, model.LyricsSourceReadingEvidenceExplicitSourceKana, "",
							)
						}
					}
				}
				cursor = end
				continue
			}
			text = fields[1]
			candidate := canonicalizeSekaipediaSourceKana(fields[2])
			if validGeneratedRubyReading(string(candidate)) {
				explicitReading = candidate
			}
		default:
			if len(fields) != 2 {
				return nil, ErrUnsupportedTable
			}
			var err error
			performerIDs, err = resolveSekaipediaSingerList(name, set, true)
			if err != nil {
				return nil, err
			}
			text = fields[1]
		}
		trailingFormatting := ""
		events, err := parseSekaipediaInlineLyricEvents(text, performerIDs, set, 0)
		if err != nil {
			leading, stripped, trailing, ok := splitSekaipediaCrossTemplateItalicBoundary(text)
			if !ok {
				return nil, err
			}
			if leading != "" {
				if formattingErr := consumeWhitespace(leading); formattingErr != nil {
					return nil, formattingErr
				}
			}
			events, err = parseSekaipediaInlineLyricEvents(stripped, performerIDs, set, 0)
			if err != nil {
				return nil, err
			}
			trailingFormatting = trailing
		}
		explicitEventRuby := map[int][]RubySpan{}
		if len(explicitReading) > 0 {
			texts := []string{}
			eventIndexes := []int{}
			prefix := pendingPunctuation
			for eventIndex, event := range events {
				if event.lineBreak {
					continue
				}
				eventText := event.text
				if prefix != "" {
					eventText = prefix + eventText
					prefix = ""
				}
				texts = append(texts, eventText)
				eventIndexes = append(eventIndexes, eventIndex)
			}
			var combined strings.Builder
			for _, eventText := range texts {
				combined.WriteString(eventText)
			}
			spans, aligned := rubySpansFromKanaReading(combined.String(), explicitReading)
			if !aligned {
				return nil, ErrUnsupportedTable
			}
			spans = markRubyReadingEvidence(
				spans, model.LyricsSourceReadingEvidenceExplicitSourceKana, "",
			)
			split, splitOK := splitRubySpansByTexts(spans, texts)
			if !splitOK || len(split) != len(eventIndexes) {
				return nil, ErrUnsupportedTable
			}
			for index, eventIndex := range eventIndexes {
				explicitEventRuby[eventIndex] = split[index]
			}
		}
		templateSegmentOrdinal := 0
		for eventIndex, event := range events {
			if event.lineBreak {
				if len(current.segments) == 0 {
					return nil, ErrUnsupportedTable
				}
				if err := flush(); err != nil {
					return nil, err
				}
				continue
			}
			text := event.text
			prefix := pendingPunctuation
			if prefix != "" {
				text = prefix + text
				pendingPunctuation = ""
			}
			if text == "" || len(text) > maxExtractedLineBytes || strings.ContainsRune(text, '\n') {
				return nil, ErrUnsupportedTable
			}
			explicitRuby := append([]RubySpan(nil), explicitEventRuby[eventIndex]...)
			if len(event.ruby) != 0 {
				if len(explicitRuby) != 0 {
					return nil, ErrUnsupportedTable
				}
				explicitRuby = append([]RubySpan(nil), event.ruby...)
			}
			templateSegmentOrdinal++
			current.segments = append(current.segments, sekaipediaColumnSegment{
				text: text, performerIDs: append([]string(nil), event.performerIDs...),
				ruby: explicitRuby, sourceGroup: templateOrdinal,
				sourceSegmentOrdinal: templateSegmentOrdinal,
			})
		}
		if trailingFormatting != "" {
			if formattingErr := consumeWhitespace(trailingFormatting); formattingErr != nil {
				return nil, formattingErr
			}
		}
		cursor = end
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(formatStack) != 0 || pendingPunctuation != "" {
		return nil, ErrUnsupportedTable
	}
	if len(lines) == 0 || len(lines) > maxExtractedLines {
		return nil, ErrUnsupportedTable
	}
	return lines, nil
}

func sekaipediaLinesHavePerformerIDs(lines []sekaipediaColumnLine) bool {
	for _, line := range lines {
		for _, segment := range line.segments {
			if len(segment.performerIDs) > 0 {
				return true
			}
		}
	}
	return false
}

func sekaipediaLinesHaveUnperformedSegments(lines []sekaipediaColumnLine) bool {
	for _, line := range lines {
		for _, segment := range line.segments {
			if len(segment.performerIDs) == 0 {
				return true
			}
		}
	}
	return false
}

func sekaipediaColumnLineText(line sekaipediaColumnLine) string {
	var text strings.Builder
	for _, segment := range line.segments {
		text.WriteString(segment.text)
	}
	return text.String()
}

func appendSekaipediaMissingReadingLines(
	lines []sekaipediaColumnLine, count int,
) []sekaipediaColumnLine {
	for index := 0; index < count; index++ {
		lines = append(lines, sekaipediaColumnLine{})
	}
	return lines
}

func deriveSekaipediaLocalLineRuby(
	japaneseLines, romajiLines []sekaipediaColumnLine, lineIndex int,
) ([]RubySpan, bool) {
	if lineIndex < 0 || lineIndex >= len(japaneseLines) {
		return nil, false
	}
	if fallback := japaneseLines[lineIndex].rubyFallback; rubySpansValidForText(
		sekaipediaColumnLineText(japaneseLines[lineIndex]), fallback,
	) {
		return append([]RubySpan(nil), fallback...), true
	}
	if len(japaneseLines) != len(romajiLines) {
		return nil, false
	}
	var japanese, romanized strings.Builder
	for _, segment := range japaneseLines[lineIndex].segments {
		japanese.WriteString(segment.text)
	}
	for _, segment := range romajiLines[lineIndex].segments {
		romanized.WriteString(segment.text)
	}
	spans, ok := deriveSekaipediaRuby(japanese.String(), romanized.String())
	if !ok {
		return nil, false
	}
	return keepSekaipediaSegmentRuby(japaneseLines[lineIndex].segments, spans)
}

// keepSekaipediaSegmentRuby keeps a segment's own ruby (an explicit {{ruby}}
// reading) inside a whole-line derivation, which reads the line without it.
// When the line ruby cannot be split at that segment's edges it is rejected,
// so the segment-local derivation that honours the explicit reading is used.
func keepSekaipediaSegmentRuby(segments []sekaipediaColumnSegment, spans []RubySpan) ([]RubySpan, bool) {
	texts := make([]string, len(segments))
	explicit := false
	for index, segment := range segments {
		texts[index] = segment.text
		explicit = explicit || len(segment.ruby) > 0 && rubySpansValidForText(segment.text, segment.ruby)
	}
	if !explicit {
		return spans, true
	}
	split, ok := splitRubySpansByTexts(spans, texts)
	if !ok || len(split) != len(segments) {
		return nil, false
	}
	result := make([]RubySpan, 0, len(spans))
	for index, segment := range segments {
		if len(segment.ruby) > 0 && rubySpansValidForText(segment.text, segment.ruby) {
			result = appendRubySpans(result, segment.ruby...)
			continue
		}
		result = appendRubySpans(result, split[index]...)
	}
	return result, true
}

type sekaipediaSourceGroupSegments struct {
	texts        []string
	performerIDs []string
	locations    [][2]int
}

func sekaipediaColumnSourceGroups(lines []sekaipediaColumnLine) (map[int]sekaipediaSourceGroupSegments, bool) {
	groups := map[int]sekaipediaSourceGroupSegments{}
	for lineIndex, line := range lines {
		for segmentIndex, segment := range line.segments {
			if segment.sourceGroup <= 0 {
				continue
			}
			group := groups[segment.sourceGroup]
			group.texts = append(group.texts, segment.text)
			group.locations = append(group.locations, [2]int{lineIndex, segmentIndex})
			for _, performerID := range segment.performerIDs {
				found := false
				for _, existing := range group.performerIDs {
					found = found || existing == performerID
				}
				if !found {
					group.performerIDs = append(group.performerIDs, performerID)
				}
			}
			groups[segment.sourceGroup] = group
		}
	}
	return groups, len(groups) != 0
}

// applySekaipediaExactSourceGroupRuby accepts a source-reading count mismatch
// only when both columns retain the same ordered top-level template groups.
// Reading text may cross line breaks inside one exact group, but it never moves
// to another source row, template group, performer segment, side, or rendition.
func applySekaipediaExactSourceGroupRuby(
	japaneseLines, romajiLines []sekaipediaColumnLine,
) bool {
	japaneseGroups, japaneseOK := sekaipediaColumnSourceGroups(japaneseLines)
	romajiGroups, romajiOK := sekaipediaColumnSourceGroups(romajiLines)
	if !japaneseOK || !romajiOK || len(japaneseGroups) != len(romajiGroups) {
		return false
	}
	for groupID := 1; groupID <= len(japaneseGroups); groupID++ {
		japanese, foundJapanese := japaneseGroups[groupID]
		romanized, foundRomaji := romajiGroups[groupID]
		if !foundJapanese || !foundRomaji ||
			!stringSlicesEqual(japanese.performerIDs, romanized.performerIDs) {
			return false
		}
		var japaneseText strings.Builder
		var romajiText strings.Builder
		for _, text := range japanese.texts {
			japaneseText.WriteString(text)
		}
		for _, text := range romanized.texts {
			romajiText.WriteString(text)
		}
		spans, ok := deriveSekaipediaSourceRuby(japaneseText.String(), romajiText.String())
		if !ok {
			return false
		}
		split, ok := splitRubySpansByTexts(spans, japanese.texts)
		if !ok || len(split) != len(japanese.locations) {
			return false
		}
		for index, location := range japanese.locations {
			segment := &japaneseLines[location[0]].segments[location[1]]
			if len(segment.ruby) != 0 {
				if !rubySpansValidForText(segment.text, segment.ruby) {
					return false
				}
				continue
			}
			segment.ruby = split[index]
		}
	}
	for _, line := range japaneseLines {
		for _, segment := range line.segments {
			if containsKanji(segment.text) && !rubySpansValidForText(segment.text, segment.ruby) {
				return false
			}
		}
	}
	return true
}

func deriveSekaipediaColumnRubyFallback(
	japaneseLines, romajiLines []sekaipediaColumnLine,
) ([][]RubySpan, bool) {
	if len(japaneseLines) == 0 || len(romajiLines) == 0 {
		return nil, false
	}
	texts := make([]string, len(japaneseLines))
	var japanese, romanized strings.Builder
	for lineIndex, line := range japaneseLines {
		texts[lineIndex] = sekaipediaColumnLineText(line)
		japanese.WriteString(texts[lineIndex])
	}
	for _, line := range romajiLines {
		romanized.WriteString(sekaipediaColumnLineText(line))
	}
	// The lines are mismatched, so a line the source cannot read is left to
	// its own dictionary reading rather than a lenient alignment of the block.
	spans, ok := deriveSekaipediaRubyMode(japanese.String(), romanized.String(), false)
	if !ok {
		return nil, false
	}
	result, splitOK := splitRubySpansByTexts(spans, texts)
	return result, splitOK
}

func deriveSekaipediaLocalSegmentRuby(
	japaneseLines, romajiLines []sekaipediaColumnLine, lineIndex, segmentIndex int,
) ([]RubySpan, bool) {
	if lineIndex < 0 || lineIndex >= len(japaneseLines) ||
		segmentIndex < 0 || segmentIndex >= len(japaneseLines[lineIndex].segments) {
		return nil, false
	}
	segment := japaneseLines[lineIndex].segments[segmentIndex]
	if rubySpansValidForText(segment.text, segment.ruby) {
		return append([]RubySpan(nil), segment.ruby...), true
	}
	if len(japaneseLines) == len(romajiLines) &&
		len(japaneseLines[lineIndex].segments) == len(romajiLines[lineIndex].segments) {
		if source, ok := deriveSekaipediaRuby(
			segment.text, romajiLines[lineIndex].segments[segmentIndex].text,
		); ok {
			return source, true
		}
	}
	if dictionary, err := generateRubySpans(segment.text); err == nil {
		return dictionary, true
	}
	if japaneseLines[lineIndex].allowUniqueDictionaryProbe {
		if dictionary, err := generateSekaipediaMismatchedColumnRubySpans(segment.text); err == nil {
			return dictionary, true
		}
	}
	return nil, false
}

func sekaipediaCompleteRubyKana(text string, spans []RubySpan) ([]rune, bool) {
	if !rubySpansValidForText(text, spans) {
		return nil, false
	}
	var reading strings.Builder
	for _, span := range spans {
		if span.Reading != "" {
			reading.WriteString(span.Reading)
			continue
		}
		for _, current := range span.Text {
			if sekaipediaIsKana(current) {
				reading.WriteRune(current)
			} else if model.LyricsSourceRubyBaseRune(current) {
				return nil, false
			}
		}
	}
	return canonicalizeSekaipediaKana([]rune(reading.String())), true
}

// deriveSekaipediaExactLineSegmentRubies maps one source-reading template to
// multiple nested visible segments only when one unique ordered word partition
// satisfies every source-group, performer, explicit-ruby, and native-kana
// boundary. Plain non-Japanese segments may consume otherwise unclaimed words,
// but ambiguity fails closed and no reading is emitted for those segments.
func deriveSekaipediaExactLineSegmentRubies(
	japaneseLine, romajiLine sekaipediaColumnLine,
) ([][]RubySpan, bool) {
	if len(japaneseLine.segments) < 2 || len(romajiLine.segments) != 1 {
		return nil, false
	}
	readingSegment := romajiLine.segments[0]
	group := readingSegment.sourceGroup
	if group <= 0 {
		return nil, false
	}
	var japaneseText strings.Builder
	for _, segment := range japaneseLine.segments {
		if segment.sourceGroup != group || !stringSlicesEqual(segment.performerIDs, readingSegment.performerIDs) {
			return nil, false
		}
		japaneseText.WriteString(segment.text)
	}
	words, ok := sekaipediaTransientRomanizedWords(japaneseText.String(), readingSegment.text)
	if !ok || len(words) == 0 {
		return nil, false
	}
	wordKana := make([][]rune, len(words))
	for index, word := range words {
		kana, converted := romanizeSekaipediaWordToKana(word)
		if !converted {
			return nil, false
		}
		wordKana[index] = []rune(kana)
	}
	type state struct {
		segment int
		word    int
	}
	type result struct {
		count int
		ruby  [][]RubySpan
	}
	memo := map[state]result{}
	var solve func(int, int) result
	solve = func(segmentIndex, wordIndex int) result {
		key := state{segment: segmentIndex, word: wordIndex}
		if cached, found := memo[key]; found {
			return cached
		}
		if segmentIndex == len(japaneseLine.segments) {
			if wordIndex == len(words) {
				return result{count: 1}
			}
			return result{}
		}
		segment := japaneseLine.segments[segmentIndex]
		minimumWords := 1
		if !sekaipediaHasJapaneseScript(segment.text) {
			minimumWords = 0
		}
		remainingSegments := len(japaneseLine.segments) - segmentIndex - 1
		lastEnd := len(words) - remainingSegments
		if lastEnd < wordIndex+minimumWords {
			return result{}
		}
		candidate := result{}
		for end := wordIndex + minimumWords; end <= lastEnd; end++ {
			var target strings.Builder
			for _, kana := range wordKana[wordIndex:end] {
				target.WriteString(string(kana))
			}
			targetKana := []rune(target.String())
			var spans []RubySpan
			valid := false
			switch {
			case len(segment.ruby) != 0:
				expected, complete := sekaipediaCompleteRubyKana(segment.text, segment.ruby)
				valid = complete && sekaipediaKanaSlicesEqual(expected, targetKana)
				spans = append([]RubySpan(nil), segment.ruby...)
			case containsKanji(segment.text):
				spans, valid = deriveSekaipediaSourceRuby(segment.text, strings.Join(words[wordIndex:end], " "))
			case sekaipediaHasJapaneseScript(segment.text):
				expected := canonicalizeSekaipediaSourceKana(segment.text)
				valid = len(expected) > 0 && sekaipediaKanaSlicesEqual(expected, targetKana)
				spans = []RubySpan{{Text: segment.text}}
			default:
				valid = true
				spans = []RubySpan{{Text: segment.text}}
			}
			if !valid || !rubySpansValidForText(segment.text, spans) {
				continue
			}
			child := solve(segmentIndex+1, end)
			if child.count == 0 {
				continue
			}
			if candidate.count == 0 {
				candidate.ruby = append([][]RubySpan{spans}, child.ruby...)
			}
			candidate.count += child.count
			if candidate.count > 1 {
				candidate.count = 2
				break
			}
		}
		memo[key] = candidate
		return candidate
	}
	mapped := solve(0, 0)
	return mapped.ruby, mapped.count == 1 && len(mapped.ruby) == len(japaneseLine.segments)
}

func deriveSekaipediaLocalSegmentRubies(
	japaneseLines, romajiLines []sekaipediaColumnLine, lineIndex int,
) ([][]RubySpan, bool) {
	if lineIndex < 0 || lineIndex >= len(japaneseLines) {
		return nil, false
	}
	if len(japaneseLines) == len(romajiLines) {
		if exact, ok := deriveSekaipediaExactLineSegmentRubies(japaneseLines[lineIndex], romajiLines[lineIndex]); ok {
			return exact, true
		}
	}
	result := make([][]RubySpan, len(japaneseLines[lineIndex].segments))
	for segmentIndex := range japaneseLines[lineIndex].segments {
		spans, ok := deriveSekaipediaLocalSegmentRuby(japaneseLines, romajiLines, lineIndex, segmentIndex)
		if !ok {
			return nil, false
		}
		result[segmentIndex] = spans
	}
	return result, true
}

func applySekaipediaRepeatedLineRubyFallback(
	japaneseLines, romajiLines []sekaipediaColumnLine,
) {
	type candidate struct {
		spans     []RubySpan
		ambiguous bool
	}
	candidates := map[string]candidate{}
	for lineIndex := range japaneseLines {
		text := sekaipediaColumnLineText(japaneseLines[lineIndex])
		if text == "" {
			continue
		}
		spans, ok := deriveSekaipediaLocalLineRuby(japaneseLines, romajiLines, lineIndex)
		if !ok || !rubySpansValidForText(text, spans) {
			continue
		}
		current, exists := candidates[text]
		if !exists {
			candidates[text] = candidate{spans: append([]RubySpan(nil), spans...)}
			continue
		}
		if !reflect.DeepEqual(current.spans, spans) {
			current.ambiguous = true
			candidates[text] = current
		}
	}
	for lineIndex := range japaneseLines {
		if len(japaneseLines[lineIndex].rubyFallback) > 0 {
			continue
		}
		text := sekaipediaColumnLineText(japaneseLines[lineIndex])
		candidate, exists := candidates[text]
		if !exists || candidate.ambiguous {
			continue
		}
		japaneseLines[lineIndex].rubyFallback = append([]RubySpan(nil), candidate.spans...)
	}
}

func bindSekaipediaRubySourceLocation(
	spans []RubySpan,
	source sekaipediaColumnSegment,
) []RubySpan {
	result := append([]RubySpan(nil), spans...)
	for index := range result {
		if result[index].Reading == "" {
			continue
		}
		switch result[index].ReadingEvidenceKind {
		case model.LyricsSourceReadingEvidenceExplicitSourceKana,
			model.LyricsSourceReadingEvidenceSourceTransliteration:
			if source.sourceGroup <= 0 || source.sourceSegmentOrdinal <= 0 {
				continue
			}
			result[index].SourceRowOrdinal = source.sourceGroup
			result[index].SourceSegmentOrdinal = source.sourceSegmentOrdinal
		}
	}
	return result
}

func bindSekaipediaRubyToSourceSegments(
	spans []RubySpan,
	source []sekaipediaColumnSegment,
) ([]RubySpan, bool) {
	texts := make([]string, len(source))
	for index := range source {
		texts[index] = source[index].text
	}
	split, ok := splitRubySpansByTexts(spans, texts)
	if !ok || len(split) != len(source) {
		return nil, false
	}
	result := make([]RubySpan, 0, len(spans))
	for index := range split {
		result = append(result, bindSekaipediaRubySourceLocation(split[index], source[index])...)
	}
	return result, true
}

func compactSekaipediaLyricsSegments(
	segments []LyricsSegment, source []sekaipediaColumnSegment,
) ([]LyricsSegment, bool) {
	if len(segments) != len(source) {
		return nil, false
	}
	compacted := make([]LyricsSegment, 0, len(segments))
	compactedGroups := make([]int, 0, len(segments))
	for index, segment := range segments {
		group := source[index].sourceGroup
		if len(compacted) == 0 || compactedGroups[len(compactedGroups)-1] != group ||
			!stringSlicesEqual(compacted[len(compacted)-1].PerformerIDs, segment.PerformerIDs) {
			compacted = append(compacted, segment)
			compactedGroups = append(compactedGroups, group)
			continue
		}
		previous := &compacted[len(compacted)-1]
		if len(previous.Text)+len(segment.Text) > maxExtractedLineBytes {
			return nil, false
		}
		previous.Text += segment.Text
		previous.Ruby = appendRubySpans(previous.Ruby, segment.Ruby...)
		if !rubySpansValidForText(previous.Text, previous.Ruby) {
			return nil, false
		}
	}
	return compacted, true
}

func buildSekaipediaStructuredLines(
	japaneseLines, romajiLines []sekaipediaColumnLine,
	aligned, japaneseOnly, generatedRuby bool,
) ([]StructuredLine, int, error) {
	preserveGeneratedSegments := generatedRuby && sekaipediaLinesHavePerformerIDs(japaneseLines)
	var singersByID map[string]sekaipediaSinger
	if aligned || japaneseOnly || preserveGeneratedSegments {
		var err error
		_, singersByID, err = buildSekaipediaSingerAliases(sekaipediaSingers)
		if err != nil {
			return nil, 0, err
		}
	}
	lineRubyError := func(lineIndex int, text, romanized string, err error) error {
		if romanized == "" && len(romajiLines) == len(japaneseLines) {
			romanized = sekaipediaColumnLineText(romajiLines[lineIndex])
		}
		return &sekaipediaLineError{
			line: lineIndex + 1, text: text, reason: sekaipediaRubyFailureReason(text, romanized), err: err,
		}
	}
	result := make([]StructuredLine, len(japaneseLines))
	totalSegments := 0
	for lineIndex, line := range japaneseLines {
		var japanese strings.Builder
		for _, segment := range line.segments {
			japanese.WriteString(segment.text)
		}
		text := japanese.String()
		if text == "" || len(text) > maxExtractedLineBytes {
			return nil, 0, ErrLyricsTooLarge
		}
		if !aligned && !japaneseOnly && !preserveGeneratedSegments {
			ruby := []RubySpan{{Text: text}}
			if generatedRuby {
				switch {
				case len(line.rubyFallback) > 0 && rubySpansValidForText(text, line.rubyFallback):
					if bound, ok := bindSekaipediaRubyToSourceSegments(line.rubyFallback, line.segments); ok {
						ruby = bound
						break
					}
					fallthrough
				case len(line.segments) > 0:
					local, localOK := deriveSekaipediaLocalSegmentRubies(japaneseLines, romajiLines, lineIndex)
					if localOK {
						ruby = nil
						for segmentIndex, spans := range local {
							spans = bindSekaipediaRubySourceLocation(spans, line.segments[segmentIndex])
							ruby = appendRubySpans(ruby, spans...)
						}
					} else if lineRuby, lineOK := deriveSekaipediaLocalLineRuby(japaneseLines, romajiLines, lineIndex); lineOK {
						if bound, boundOK := bindSekaipediaRubyToSourceSegments(lineRuby, line.segments); boundOK {
							ruby = bound
						} else if dictionary, dictionaryErr := generateRubySpans(text); dictionaryErr == nil {
							ruby = dictionary
						} else {
							return nil, 0, lineRubyError(lineIndex, text, "", dictionaryErr)
						}
					} else if dictionary, dictionaryErr := generateRubySpans(text); dictionaryErr == nil {
						ruby = dictionary
					} else {
						return nil, 0, lineRubyError(lineIndex, text, "", dictionaryErr)
					}
				default:
					dictionary, dictionaryErr := generateRubySpans(text)
					if dictionaryErr != nil {
						return nil, 0, lineRubyError(lineIndex, text, "", dictionaryErr)
					}
					ruby = dictionary
				}
			}
			result[lineIndex] = StructuredLine{
				Japanese: text, StanzaBreakBefore: line.stanzaBreakBefore,
				Segments: []LyricsSegment{{
					Text: text, PerformerIDs: []string{}, Ruby: ruby,
				}},
				TrailingPerformerIDs: []string{},
			}
			if generatedRuby {
				totalSegments++
			}
			continue
		}
		var generatedSegmentRuby [][]RubySpan
		if generatedRuby && !aligned {
			texts := make([]string, len(line.segments))
			for segmentIndex := range line.segments {
				texts[segmentIndex] = line.segments[segmentIndex].text
			}
			if localRuby, ok := deriveSekaipediaLocalSegmentRubies(japaneseLines, romajiLines, lineIndex); ok {
				generatedSegmentRuby = localRuby
			} else if lineRuby, lineOK := deriveSekaipediaLocalLineRuby(japaneseLines, romajiLines, lineIndex); lineOK {
				var splitOK bool
				generatedSegmentRuby, splitOK = splitRubySpansByTexts(lineRuby, texts)
				if !splitOK {
					var dictionaryErr error
					generatedSegmentRuby, dictionaryErr = generateRubySpansForTexts(texts)
					if dictionaryErr != nil {
						return nil, 0, lineRubyError(lineIndex, text, "", dictionaryErr)
					}
				}
			} else if dictionaryRuby, dictionaryErr := generateRubySpansForTexts(texts); dictionaryErr == nil {
				generatedSegmentRuby = dictionaryRuby
			} else {
				return nil, 0, lineRubyError(lineIndex, text, "", dictionaryErr)
			}
		}
		segments := make([]LyricsSegment, len(line.segments))
		for segmentIndex, segment := range line.segments {
			ruby := []RubySpan{{Text: segment.text}}
			if aligned {
				var ok bool
				romanized := romajiLines[lineIndex].segments[segmentIndex].text
				ruby, ok = deriveSekaipediaRuby(segment.text, romanized)
				if !ok {
					return nil, 0, lineRubyError(lineIndex, segment.text, romanized, ErrUnsupportedTable)
				}
			} else if generatedRuby {
				ruby = generatedSegmentRuby[segmentIndex]
			}
			performerIDs, ok := sekaipediaPersistedPerformerIDs(segment.performerIDs, singersByID)
			if !ok {
				return nil, 0, ErrMalformedResponse
			}
			ruby = bindSekaipediaRubySourceLocation(ruby, segment)
			segments[segmentIndex] = LyricsSegment{
				Text: segment.text, PerformerIDs: performerIDs, Ruby: ruby,
			}
			totalSegments++
		}
		compactedSegments, compacted := compactSekaipediaLyricsSegments(segments, line.segments)
		if !compacted {
			return nil, 0, &sekaipediaLineError{
				line: lineIndex + 1, text: text, reason: "performer segments do not merge", err: ErrUnsupportedTable,
			}
		}
		result[lineIndex] = StructuredLine{
			Japanese: text, StanzaBreakBefore: line.stanzaBreakBefore,
			Segments: compactedSegments, TrailingPerformerIDs: []string{},
		}
		totalSegments -= len(segments) - len(compactedSegments)
	}
	return result, totalSegments, nil
}

// sekaipediaLineError names the lyric line that failed a table and why; err
// (ErrUnsupportedTable) stays matchable with errors.Is. Like the other source
// diagnostics, Error() carries no lyric text, since it reaches server logs;
// the line text stays in text for a caller that takes it with errors.As.
type sekaipediaLineError struct {
	line   int
	text   string
	reason string
	err    error
}

func (failure *sekaipediaLineError) Error() string {
	return fmt.Sprintf("line %d: %s: %v", failure.line, failure.reason, failure.err)
}

func (failure *sekaipediaLineError) Unwrap() error {
	return failure.err
}

func sekaipediaColumnsAligned(japanese, romaji []sekaipediaColumnLine) bool {
	if len(japanese) != len(romaji) {
		return false
	}
	for lineIndex := range japanese {
		if len(japanese[lineIndex].segments) != len(romaji[lineIndex].segments) {
			return false
		}
		for segmentIndex := range japanese[lineIndex].segments {
			japaneseSegment := japanese[lineIndex].segments[segmentIndex]
			romajiSegment := romaji[lineIndex].segments[segmentIndex]
			left := japaneseSegment.performerIDs
			right := romajiSegment.performerIDs
			if len(left) != len(right) {
				return false
			}
			for index := range left {
				if left[index] != right[index] {
					return false
				}
			}
			if _, ok := deriveSekaipediaSourceRuby(japaneseSegment.text, romajiSegment.text); !ok {
				return false
			}
		}
	}
	return true
}
