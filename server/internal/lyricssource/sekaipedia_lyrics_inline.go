package lyricssource

import (
	"errors"

	"strings"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

type sekaipediaInlineLyricEvent struct {
	text         string
	performerIDs []string
	ruby         []RubySpan
	lineBreak    bool
}

func unwrapSekaipediaSingleNestedTemplate(fields []string) ([]string, bool) {
	if len(fields) != 1 {
		return fields, true
	}
	nested := strings.TrimSpace(fields[0])
	if !strings.HasPrefix(nested, "{{") {
		return fields, true
	}
	_, end, inner, ok := balancedSekaipediaTemplateAt(nested, 0)
	if !ok || end != len(nested) {
		return nil, false
	}
	nestedFields, ok := splitTopLevelSekaipediaFields(inner, "|")
	if !ok || len(nestedFields) == 0 {
		return nil, false
	}
	return nestedFields, true
}

func parseSekaipediaLyricTemplateFields(
	fields []string,
	set sekaipediaSingerSet,
) ([]string, string, error) {
	if len(fields) < 2 || !strings.EqualFold(strings.TrimSpace(fields[0]), "Lyric") {
		return nil, "", ErrUnsupportedTable
	}
	if len(fields) == 2 {
		text := strings.TrimSpace(fields[1])
		if text == "" {
			return nil, "", ErrUnsupportedTable
		}
		if _, err := resolveSekaipediaSingerList(text, set, true); err == nil {
			return nil, "", ErrUnsupportedTable
		}
		return []string{}, text, nil
	}
	if len(fields) == 5 && strings.EqualFold(strings.TrimSpace(fields[2]), "ruby") {
		primary := strings.TrimSpace(fields[1])
		base := strings.TrimSpace(fields[3])
		reading := strings.TrimSpace(fields[4])
		if primary == "" || base == "" || reading == "" || len(base) > maxExtractedLineBytes || len(reading) > maxExtractedLineBytes ||
			strings.ContainsAny(base, "\r\n\x00") || strings.ContainsAny(reading, "\r\n\x00") {
			return nil, "", ErrUnsupportedTable
		}
		performerIDs, err := resolveSekaipediaSingerList(primary, set, true)
		if err != nil {
			return nil, "", err
		}
		return performerIDs, base, nil
	}
	if len(fields) != 3 && len(fields) != 4 {
		return nil, "", ErrUnsupportedTable
	}
	primary := strings.TrimSpace(fields[1])
	textIndex := 2
	combined := primary
	if len(fields) == 4 {
		textIndex = 3
		middle := strings.TrimSpace(fields[2])
		if middle != "" {
			secondary := middle
			if separator := strings.IndexByte(middle, '='); separator >= 0 {
				name := strings.ToLower(strings.TrimSpace(middle[:separator]))
				if name != "backing" && name != "backup" {
					return nil, "", ErrUnsupportedTable
				}
				secondary = strings.TrimSpace(middle[separator+1:])
			}
			if secondary == "" {
				return nil, "", ErrUnsupportedTable
			}
			combined += "," + secondary
		}
	}
	performerIDs, err := resolveSekaipediaSingerList(combined, set, true)
	if err != nil {
		return nil, "", err
	}
	text := strings.TrimSpace(fields[textIndex])
	if text == "" {
		return nil, "", ErrUnsupportedTable
	}
	return performerIDs, text, nil
}

func parseSekaipediaExplicitRubyEvents(
	base, reading string,
	performerIDs []string,
	set sekaipediaSingerSet,
	depth int,
) ([]sekaipediaInlineLyricEvent, error) {
	base = strings.TrimSpace(base)
	reading = strings.TrimSpace(reading)
	if base == "" || reading == "" || depth > 8 {
		return nil, ErrUnsupportedTable
	}
	baseEvents, err := parseSekaipediaInlineLyricEvents(base, performerIDs, set, depth)
	if err != nil {
		return nil, err
	}
	readingEvents, err := parseSekaipediaInlineLyricEvents(reading, performerIDs, set, depth)
	if err != nil {
		return nil, err
	}
	for _, event := range append(append([]sekaipediaInlineLyricEvent(nil), baseEvents...), readingEvents...) {
		if event.lineBreak {
			return nil, ErrUnsupportedTable
		}
	}

	// Sekaipedia also uses Ruby for display glosses that the kana-only source
	// contract cannot represent: Latin bases with a kana pronunciation, and
	// annotations with no kana reading at all. Validate both nested sides, retain
	// only the exact visible base and its performer attribution, and leave ruby
	// empty for row/segment-local source transliteration or generation. Annotation
	// performers never replace or widen the visible base performers.
	if len(baseEvents) == len(readingEvents) && len(baseEvents) > 0 {
		allBaseNonHan := true
		allReadingNonKana := true
		for index := range baseEvents {
			allBaseNonHan = allBaseNonHan && !containsKanji(baseEvents[index].text)
			allReadingNonKana = allReadingNonKana && len(canonicalizeSekaipediaSourceKana(readingEvents[index].text)) == 0
			if len(readingEvents[index].ruby) != 0 {
				allBaseNonHan = false
				allReadingNonKana = false
			}
		}
		if allBaseNonHan || allReadingNonKana {
			return baseEvents, nil
		}
	}

	// Prefer an exact event-to-event source binding. This keeps an explicit
	// reading inside the same nested performer segment instead of using a
	// combined line as evidence for neighboring segments.
	if len(baseEvents) == len(readingEvents) {
		result := append([]sekaipediaInlineLyricEvent(nil), baseEvents...)
		complete := true
		for index := range result {
			if !stringSlicesEqual(baseEvents[index].performerIDs, readingEvents[index].performerIDs) {
				complete = false
				break
			}
			spans, ok := sekaipediaExplicitRubySpans(baseEvents[index].text, readingEvents[index].text)
			if !ok {
				complete = false
				break
			}
			result[index].ruby = markRubyReadingEvidence(
				spans, model.LyricsSourceReadingEvidenceExplicitSourceKana, "",
			)
		}
		if complete {
			return result, nil
		}
	}

	texts := make([]string, len(baseEvents))
	var combinedBase strings.Builder
	var combinedReading strings.Builder
	for index, event := range baseEvents {
		texts[index] = event.text
		combinedBase.WriteString(event.text)
	}
	for _, event := range readingEvents {
		combinedReading.WriteString(event.text)
	}
	spans, ok := sekaipediaExplicitRubySpans(combinedBase.String(), combinedReading.String())
	var split [][]RubySpan
	if ok {
		split, ok = splitRubySpansByTexts(spans, texts)
		ok = ok && len(split) == len(baseEvents)
	}
	if !ok {
		// A phrase gloss such as 怒れる人 read ラングラー, or a reading that is
		// itself Han (犯人 glossed 答え), has no plausible per-base split. Like the
		// display glosses above, keep only the visible base and leave its ruby to
		// the row's source transliteration or generation instead of inventing one.
		// One group span over the whole base ({怒れる人|らんぐらあ}) is not an
		// option: the ruby contract (rubySpansValidForText and the store's
		// public and document validators) only allows Han in a ruby base. A
		// reading that splits but not at the nested segment edges is kept out
		// the same way.
		return baseEvents, nil
	}
	result := append([]sekaipediaInlineLyricEvent(nil), baseEvents...)
	for index := range result {
		result[index].ruby = markRubyReadingEvidence(
			split[index], model.LyricsSourceReadingEvidenceExplicitSourceKana, "",
		)
	}
	return result, nil
}

func parseSekaipediaInlineLyricEvents(
	value string,
	performerIDs []string,
	set sekaipediaSingerSet,
	depth int,
) ([]sekaipediaInlineLyricEvent, error) {
	if depth > 8 || value == "" || len(value) > maxExtractedTextBytes || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\r\x00") {
		return nil, ErrUnsupportedTable
	}
	events := []sekaipediaInlineLyricEvent{}
	appendPlain := func(chunk string) error {
		for index, fragment := range strings.Split(chunk, "\n") {
			if fragment != "" {
				rendered := fragment
				if strings.ContainsAny(fragment, "{}[]<>") || strings.Contains(fragment, "''") {
					leading := len(fragment) - len(strings.TrimLeftFunc(fragment, unicode.IsSpace))
					trailingStart := len(strings.TrimRightFunc(fragment, unicode.IsSpace))
					core := fragment[leading:trailingStart]
					if core == "" {
						rendered = fragment
					} else {
						plain, err := renderSekaipediaPlainLyricText(core, depth)
						if err != nil || plain == "" {
							return ErrUnsupportedTable
						}
						rendered = fragment[:leading] + plain + fragment[trailingStart:]
					}
				}
				events = append(events, sekaipediaInlineLyricEvent{
					text: rendered, performerIDs: append([]string(nil), performerIDs...),
				})
			}
			if index+1 < len(strings.Split(chunk, "\n")) {
				events = append(events, sekaipediaInlineLyricEvent{lineBreak: true})
			}
		}
		return nil
	}
	for cursor := 0; cursor < len(value); {
		next := nextSekaipediaTemplateStart(value, cursor)
		if next == -2 {
			return nil, ErrUnsupportedTable
		}
		if next < 0 {
			if err := appendPlain(value[cursor:]); err != nil {
				return nil, err
			}
			break
		}
		if err := appendPlain(value[cursor:next]); err != nil {
			return nil, err
		}
		_, end, inner, ok := balancedSekaipediaTemplateAt(value, next)
		if !ok {
			return nil, ErrUnsupportedTable
		}
		fields, ok := splitTopLevelSekaipediaFields(inner, "|")
		if !ok || len(fields) == 0 {
			return nil, ErrUnsupportedTable
		}
		fields, ok = unwrapSekaipediaSingleNestedTemplate(fields)
		if !ok || len(fields) == 0 {
			return nil, ErrUnsupportedTable
		}
		name := strings.TrimSpace(fields[0])
		nestedIDs := performerIDs
		nestedText := ""
		switch {
		case strings.EqualFold(name, "Lyric"):
			var err error
			nestedIDs, nestedText, err = parseSekaipediaLyricTemplateFields(fields, set)
			if err != nil {
				return nil, err
			}
		case strings.EqualFold(name, "ruby"):
			if len(fields) != 3 || strings.TrimSpace(fields[2]) == "" || len(fields[2]) > maxExtractedLineBytes {
				return nil, ErrUnsupportedTable
			}
			if strings.TrimSpace(fields[1]) == "" {
				if _, err := parseSekaipediaInlineLyricEvents(strings.TrimSpace(fields[2]), nestedIDs, set, depth+1); err != nil {
					return nil, err
				}
				cursor = end
				continue
			}
			nested, err := parseSekaipediaExplicitRubyEvents(
				fields[1], fields[2], nestedIDs, set, depth+1,
			)
			if err != nil {
				return nil, err
			}
			events = append(events, nested...)
			cursor = end
			continue
		case name == "Color":
			if len(fields) != 3 || !validSekaipediaIgnoredFormattingSelector(fields[1]) || strings.TrimSpace(fields[2]) == "" {
				return nil, ErrUnsupportedTable
			}
			nestedText = fields[2]
		default:
			if len(fields) != 2 {
				return nil, ErrUnsupportedTable
			}
			var err error
			nestedIDs, err = resolveSekaipediaSingerList(name, set, true)
			if err != nil {
				return nil, err
			}
			nestedText = fields[1]
		}
		nested, err := parseSekaipediaInlineLyricEvents(nestedText, nestedIDs, set, depth+1)
		if err != nil {
			return nil, err
		}
		events = append(events, nested...)
		cursor = end
	}
	if len(events) == 0 {
		return nil, ErrUnsupportedTable
	}
	compacted := make([]sekaipediaInlineLyricEvent, 0, len(events))
	for _, event := range events {
		if !event.lineBreak && len(compacted) > 0 && !compacted[len(compacted)-1].lineBreak &&
			len(compacted[len(compacted)-1].ruby) == 0 && len(event.ruby) == 0 &&
			stringSlicesEqual(compacted[len(compacted)-1].performerIDs, event.performerIDs) {
			if len(compacted[len(compacted)-1].text)+len(event.text) > maxExtractedLineBytes {
				return nil, ErrUnsupportedTable
			}
			compacted[len(compacted)-1].text += event.text
			continue
		}
		compacted = append(compacted, event)
	}
	return compacted, nil
}

func parseSekaipediaLyricInterstitial(value string) ([]string, bool, bool, error) {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\x00") {
		return nil, false, false, ErrUnsupportedTable
	}
	leadingEnd := len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
	trailingStart := len(strings.TrimRightFunc(value, unicode.IsSpace))
	if leadingEnd > trailingStart {
		return nil, false, false, ErrUnsupportedTable
	}
	leadingWhitespace := value[:leadingEnd]
	trailingWhitespace := value[trailingStart:]
	if strings.Count(leadingWhitespace, "\n") > 1 || strings.Count(trailingWhitespace, "\n") > 1 {
		return nil, false, false, ErrUnsupportedTable
	}
	leadingBreak := strings.ContainsRune(leadingWhitespace, '\n')
	trailingBreak := strings.ContainsRune(trailingWhitespace, '\n')
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false, false, ErrUnsupportedTable
	}
	parts := strings.Split(value, "\n")
	if len(parts) == 0 || len(parts) > maxExtractedLines {
		return nil, false, false, ErrUnsupportedTable
	}
	result := make([]string, 0, len(parts))
	for _, sourceLine := range parts {
		line := strings.TrimSpace(sourceLine)
		if line == "" {
			return nil, false, false, ErrUnsupportedTable
		}
		rendered, err := renderSekaipediaPlainLyricText(line, 0)
		if err != nil || rendered == "" || len(rendered) > maxExtractedLineBytes || strings.ContainsRune(rendered, '\n') {
			return nil, false, false, ErrUnsupportedTable
		}
		result = append(result, rendered)
	}
	return result, leadingBreak, trailingBreak, nil
}

func splitSekaipediaCrossTemplateItalicBoundary(value string) (string, string, string, bool) {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\x00") || strings.Count(value, "''")%2 == 0 {
		return "", "", "", false
	}
	leadingEnd := len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
	trailingStart := len(strings.TrimRightFunc(value, unicode.IsSpace))
	if leadingEnd > trailingStart {
		return "", "", "", false
	}
	leading := strings.HasPrefix(value[leadingEnd:], "''")
	trailing := strings.HasSuffix(value[:trailingStart], "''")
	if leading == trailing {
		return "", "", "", false
	}
	stripped := value
	leadingMarker := ""
	trailingMarker := ""
	if leading {
		stripped = value[:leadingEnd] + value[leadingEnd+2:]
		leadingMarker = "''"
	} else {
		stripped = value[:trailingStart-2] + value[trailingStart:]
		trailingMarker = "''"
	}
	if strings.TrimSpace(stripped) == "" || strings.Count(stripped, "''")%2 != 0 {
		return "", "", "", false
	}
	return leadingMarker, stripped, trailingMarker, true
}

func splitSekaipediaInterstitialPunctuation(value string) (string, string, string, bool) {
	if value == "" || len(value) > 64 || !utf8.ValidString(value) || strings.ContainsAny(value, "{}[]<>\r\n\x00'\"") {
		return "", "", "", false
	}
	leadingEnd := len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
	trailingStart := len(strings.TrimRightFunc(value, unicode.IsSpace))
	if leadingEnd > trailingStart {
		return "", "", "", false
	}
	punctuation := value[leadingEnd:trailingStart]
	if punctuation == "" {
		return "", "", "", false
	}
	for _, current := range punctuation {
		if !unicode.IsPunct(current) && !unicode.IsSymbol(current) {
			return "", "", "", false
		}
	}
	return value[:leadingEnd], punctuation, value[trailingStart:], true
}

func validSekaipediaIgnoredFormattingSelector(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00{}[]<>|") {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func sekaipediaOpaqueMarkupEnd(value string, start int) (int, bool, bool) {
	if start < 0 || start >= len(value) || value[start] != '<' {
		return 0, false, false
	}
	remaining := value[start:]
	lowerRemaining := strings.ToLower(remaining)
	if strings.HasPrefix(lowerRemaining, "<nowiki>") {
		closeAt := strings.Index(lowerRemaining[len("<nowiki>"):], "</nowiki>")
		if closeAt < 0 {
			return 0, true, false
		}
		return start + len("<nowiki>") + closeAt + len("</nowiki>"), true, true
	}
	if strings.HasPrefix(remaining, "<!--") || strings.HasPrefix(lowerRemaining, "<ref") {
		consumed, ok := consumeSekaipediaIgnoredCitationPrefix(remaining)
		if !ok {
			return 0, true, false
		}
		return start + consumed, true, true
	}
	return 0, false, false
}

func balancedSekaipediaTemplateAt(value string, start int) (int, int, string, bool) {
	if start < 0 || start+2 > len(value) || value[start:start+2] != "{{" {
		return 0, 0, "", false
	}
	depth := 0
	for index := start; index+1 < len(value); index++ {
		if end, recognized, ok := sekaipediaOpaqueMarkupEnd(value, index); recognized {
			if !ok {
				return 0, 0, "", false
			}
			index = end - 1
			continue
		}
		switch value[index : index+2] {
		case "{{":
			depth++
			index++
		case "}}":
			depth--
			index++
			if depth == 0 {
				end := index + 1
				return start, end, value[start+2 : end-2], true
			}
			if depth < 0 {
				return 0, 0, "", false
			}
		}
	}
	return 0, 0, "", false
}

func splitTopLevelSekaipediaFields(value, separator string) ([]string, bool) {
	if separator == "" {
		return nil, false
	}
	result := make([]string, 0, 2)
	start := 0
	templateDepth := 0
	linkDepth := 0
	inTag := false
	var tagQuote byte
	for index := 0; index < len(value); index++ {
		switch {
		case inTag:
			if tagQuote != 0 {
				if value[index] == tagQuote {
					tagQuote = 0
				}
				continue
			}
			if value[index] == '\'' || value[index] == '"' {
				tagQuote = value[index]
				continue
			}
			if value[index] == '>' {
				inTag = false
			}
		case value[index] == '<':
			if end, recognized, ok := sekaipediaOpaqueMarkupEnd(value, index); recognized {
				if !ok {
					return nil, false
				}
				index = end - 1
				continue
			}
			if sekaipediaHTMLTagStartsAt(value, index) {
				inTag = true
			}
		case index+1 < len(value) && value[index:index+2] == "{{":
			templateDepth++
			index++
		case index+1 < len(value) && value[index:index+2] == "}}":
			if templateDepth == 0 {
				return nil, false
			}
			templateDepth--
			index++
		case templateDepth == 0 && index+1 < len(value) && value[index:index+2] == "[[":
			linkDepth++
			index++
		case templateDepth == 0 && index+1 < len(value) && value[index:index+2] == "]]":
			if linkDepth == 0 {
				return nil, false
			}
			linkDepth--
			index++
		case templateDepth == 0 && linkDepth == 0 && strings.HasPrefix(value[index:], separator):
			result = append(result, value[start:index])
			index += len(separator) - 1
			start = index + 1
		}
	}
	if templateDepth != 0 || linkDepth != 0 || inTag || tagQuote != 0 {
		return nil, false
	}
	return append(result, value[start:]), true
}

func sekaipediaHTMLTagStartsAt(value string, start int) bool {
	if start < 0 || start+1 >= len(value) || value[start] != '<' {
		return false
	}
	cursor := start + 1
	if value[cursor] == '/' {
		cursor++
	}
	if cursor >= len(value) {
		return false
	}
	current := value[cursor]
	return current == '!' || current == '?' || current >= 'A' && current <= 'Z' || current >= 'a' && current <= 'z'
}

func findBalancedSekaipediaNamedTemplate(value, name string) (int, int, string, bool) {
	needle := "{{" + strings.ToLower(name)
	lowerValue := strings.ToLower(value)
	for cursor := 0; cursor < len(value); {
		if end, recognized, ok := sekaipediaOpaqueMarkupEnd(value, cursor); recognized {
			if !ok {
				return 0, 0, "", false
			}
			cursor = end
			continue
		}
		if strings.HasPrefix(lowerValue[cursor:], needle) {
			start, end, inner, ok := balancedSekaipediaTemplateAt(value, cursor)
			if ok {
				fields, fieldsOK := splitTopLevelSekaipediaFields(inner, "|")
				if fieldsOK && len(fields) > 0 && strings.EqualFold(strings.TrimSpace(fields[0]), name) {
					return start, end, inner, true
				}
			}
		}
		cursor++
	}
	return 0, 0, "", false
}

func consumeSekaipediaIgnoredCitationPrefix(value string) (int, bool) {
	if value == "" || !utf8.ValidString(value) {
		return 0, false
	}
	bounded := value
	if len(bounded) > maxSekaipediaIgnoredCitationBytes {
		bounded = bounded[:maxSekaipediaIgnoredCitationBytes]
	}
	lowerBounded := strings.ToLower(bounded)
	switch {
	case strings.HasPrefix(bounded, "<!--"):
		end := strings.Index(bounded[4:], "-->")
		if end < 0 {
			return 0, false
		}
		return 4 + end + 3, true
	case strings.HasPrefix(lowerBounded, "<references"):
		openEnd := strings.IndexByte(bounded, '>')
		if openEnd < 0 {
			return 0, false
		}
		opening := strings.TrimSpace(bounded[:openEnd+1])
		if strings.HasSuffix(opening, "/>") {
			return openEnd + 1, true
		}
		closeAt := strings.Index(lowerBounded[openEnd+1:], "</references>")
		if closeAt < 0 {
			return 0, false
		}
		return openEnd + 1 + closeAt + len("</references>"), true
	case strings.HasPrefix(lowerBounded, "<ref"):
		openEnd := strings.IndexByte(bounded, '>')
		if openEnd < 0 {
			return 0, false
		}
		opening := strings.TrimSpace(bounded[:openEnd+1])
		if strings.HasSuffix(opening, "/>") {
			return openEnd + 1, true
		}
		closeAt := strings.Index(lowerBounded[openEnd+1:], "</ref>")
		if closeAt < 0 {
			return 0, false
		}
		return openEnd + 1 + closeAt + len("</ref>"), true
	default:
		return 0, false
	}
}

func validSekaipediaTemplateSequenceSuffix(value string) bool {
	if len(value) > maxSekaipediaIgnoredCitationBytes || !utf8.ValidString(value) {
		return false
	}
	for {
		value = strings.TrimSpace(value)
		if value == "" {
			return true
		}
		consumed, ok := consumeSekaipediaIgnoredCitationPrefix(value)
		if !ok {
			return false
		}
		value = value[consumed:]
	}
}

func isSekaipediaRecoverableReadingError(err error) bool {
	return errors.Is(err, ErrUnsupportedTable) || errors.Is(err, ErrMissingLyrics) || errors.Is(err, ErrAmbiguous)
}
