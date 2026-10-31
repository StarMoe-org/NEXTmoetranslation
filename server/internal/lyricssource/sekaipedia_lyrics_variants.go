package lyricssource

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSekaipediaIgnoredCitationBytes = 64 << 10

func parseSekaipediaLyricsLayout(section string) (map[string]string, bool, error) {
	value := strings.TrimSpace(section)
	sameLyrics := false
	if remainder, matched := trimKnownSekaipediaSameLyricsNote(value); matched {
		trimmedRemainder := strings.TrimSpace(remainder)
		if trimmedRemainder != "" && (len(remainder) != len(trimmedRemainder) ||
			strings.HasPrefix(trimmedRemainder, "{{") || strings.HasPrefix(trimmedRemainder, "<tabber>") ||
			strings.HasPrefix(trimmedRemainder, "===")) {
			sameLyrics = true
			value = trimmedRemainder
		}
	}
	if trimmed, matched, err := trimSekaipediaLeadingLyricStub(value); err != nil {
		return nil, sameLyrics, err
	} else if matched {
		value = trimmed
	}
	if strings.HasPrefix(value, "===") {
		tabs, err := parseSekaipediaSubheadingLyricsLayout(value)
		return tabs, sameLyrics, err
	}
	if len(value) >= len("<tabber>") && strings.EqualFold(value[:len("<tabber>")], "<tabber>") {
		if trimmed, ok := trimSekaipediaTabberCitationSuffix(value); ok {
			value = trimmed
		}
	}
	if len(value) > len("<tabber>")+len("</tabber>") &&
		strings.EqualFold(value[:len("<tabber>")], "<tabber>") &&
		strings.EqualFold(value[len(value)-len("</tabber>"):], "</tabber>") {
		inner := strings.TrimSpace(value[len("<tabber>") : len(value)-len("</tabber>")])
		tabs, err := parseSekaipediaTabberEntries(inner)
		return tabs, sameLyrics, err
	}
	if strings.HasPrefix(value, "{{") {
		if _, _, inner, ok := balancedSekaipediaTemplateAt(value, 0); ok {
			if fields, fieldsOK := splitTopLevelSekaipediaFields(inner, "|"); fieldsOK && len(fields) > 0 &&
				strings.EqualFold(strings.TrimSpace(fields[0]), "Lyrics head") {
				return map[string]string{"Full Version": value}, sameLyrics, nil
			}
		}
	}
	if templates, err := parseSekaipediaTemplateSequence(value); err == nil {
		stripped, stripErr := stripSekaipediaLeadingLyricStubs(templates)
		if stripErr == nil && len(stripped) > 0 && strings.EqualFold(strings.TrimSpace(stripped[0].name), "Lyrics head") {
			return map[string]string{"Full Version": value}, sameLyrics, nil
		}
	}
	return nil, false, ErrUnsupportedTable
}

func trimSekaipediaLeadingLyricStub(value string) (string, bool, error) {
	value = strings.TrimSpace(value)
	matched := false
	for strings.HasPrefix(value, "{{") {
		_, end, inner, ok := balancedSekaipediaTemplateAt(value, 0)
		if !ok {
			return "", false, ErrUnsupportedTable
		}
		fields, fieldsOK := splitTopLevelSekaipediaFields(inner, "|")
		if !fieldsOK || len(fields) == 0 || !strings.EqualFold(strings.TrimSpace(fields[0]), "Lyric stub") {
			break
		}
		if !sekaipediaLyricStubMarkersKnown(fields[1:]) {
			return "", false, ErrUnsupportedTable
		}
		matched = true
		value = strings.TrimSpace(value[end:])
	}
	if !matched {
		return value, false, nil
	}
	if value == "" {
		return "", true, ErrUnsupportedTable
	}
	return value, true, nil
}

func parseSekaipediaSubheadingLyricsLayout(value string) (map[string]string, error) {
	matches := sekaipediaLevelThreeHeadingPattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return nil, ErrUnsupportedTable
	}
	tabs := make(map[string]string, len(matches))
	for index, match := range matches {
		label := strings.TrimSpace(value[match[2]:match[3]])
		switch label {
		case "Game Version", "Full Version", "APPEND/Full Version", "SEKAI", "VIRTUAL SINGER":
		default:
			return nil, ErrUnsupportedTable
		}
		if _, duplicate := tabs[label]; duplicate {
			return nil, ErrAmbiguous
		}
		end := len(value)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		body := strings.TrimSpace(value[match[1]:end])
		if body == "" {
			return nil, ErrMissingLyrics
		}
		tabs[label] = body
	}
	return tabs, nil
}

func trimKnownSekaipediaSameLyricsNote(value string) (string, bool) {
	for _, note := range [...]string{
		sekaipediaSameLyricsNote,
		sekaipediaSameLyricsHyphenatedNote,
		sekaipediaSameDurationAndLyricsNote,
	} {
		if strings.HasPrefix(value, note) {
			return strings.TrimPrefix(value, note), true
		}
	}
	return value, false
}

func trimSekaipediaTabberCitationSuffix(value string) (string, bool) {
	lowerValue := strings.ToLower(value)
	closeAt := strings.LastIndex(lowerValue, "</tabber>")
	if closeAt < 0 {
		return "", false
	}
	coreEnd := closeAt + len("</tabber>")
	suffix := value[coreEnd:]
	if strings.TrimSpace(suffix) == "" {
		return strings.TrimSpace(value[:coreEnd]), true
	}
	for len(suffix) > 0 {
		suffix = strings.TrimLeftFunc(suffix, unicode.IsSpace)
		if suffix == "" {
			break
		}
		consumed, ok := consumeSekaipediaIgnoredCitationPrefix(suffix)
		if !ok || consumed <= 0 {
			return "", false
		}
		suffix = suffix[consumed:]
	}
	return strings.TrimSpace(value[:coreEnd]), true
}

func parseSekaipediaTabberEntries(inner string) (map[string]string, error) {
	parts, ok := splitTopLevelSekaipediaFields(inner, "\n|-|\n")
	if !ok || len(parts) == 1 && strings.Contains(inner, "|-|") {
		parts, ok = splitTopLevelSekaipediaTabberFields(inner)
		if !ok {
			return nil, ErrUnsupportedTable
		}
	}
	return parseSekaipediaTabberParts(parts)
}

func splitTopLevelSekaipediaTabberFields(value string) ([]string, bool) {
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
		case templateDepth == 0 && linkDepth == 0 && strings.HasPrefix(value[index:], "|-|"):
			beforeOK := index == 0 || unicode.IsSpace(rune(value[index-1])) || value[index-1] == '}'
			afterIndex := index + len("|-|")
			afterOK := afterIndex >= len(value) || unicode.IsSpace(rune(value[afterIndex]))
			if !beforeOK || !afterOK {
				continue
			}
			result = append(result, value[start:index])
			index += len("|-|") - 1
			start = index + 1
		}
	}
	if templateDepth != 0 || linkDepth != 0 || inTag || tagQuote != 0 {
		return nil, false
	}
	return append(result, value[start:]), true
}

func parseSekaipediaTabberParts(parts []string) (map[string]string, error) {
	if len(parts) == 0 || len(parts) > 32 {
		return nil, ErrUnsupportedTable
	}
	tabs := make(map[string]string, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		lineEnd := strings.IndexByte(part, '\n')
		firstLine := part
		remainder := ""
		if lineEnd >= 0 {
			firstLine = part[:lineEnd]
			remainder = part[lineEnd+1:]
		}
		separator := strings.IndexByte(firstLine, '=')
		if separator < 0 {
			return nil, ErrUnsupportedTable
		}
		label, ok := normalizeSekaipediaLyricsTabLabel(strings.TrimSpace(firstLine[:separator]))
		if !ok {
			return nil, ErrUnsupportedTable
		}
		if _, duplicate := tabs[label]; duplicate {
			return nil, ErrAmbiguous
		}
		inlineBody := strings.TrimSpace(firstLine[separator+1:])
		body := strings.TrimSpace(remainder)
		if inlineBody != "" {
			if body == "" {
				body = inlineBody
			} else {
				body = inlineBody + "\n" + body
			}
		}
		if body == "" {
			return nil, ErrMissingLyrics
		}
		tabs[label] = body
	}
	return tabs, nil
}

func normalizeSekaipediaLyricsTabLabel(value string) (string, bool) {
	switch value {
	case "Game version":
		return "Game Version", true
	case "Full version":
		return "Full Version", true
	case "Game Version", "Full Version", "SEKAI", "VIRTUAL SINGER", "Another Vocal":
		return value, true
	}
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\r\n\x00{}[]<>|=") {
		return "", false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return "", false
		}
	}
	return value, true
}

func unwrapSekaipediaNestedRendition(body, kind string) (string, error) {
	templates, err := parseSekaipediaTemplateSequence(body)
	if err != nil || len(templates) != 1 || templates[0].name != "#tag:tabber" {
		return body, nil
	}
	if len(templates[0].fields) != 2 {
		return "", ErrUnsupportedTable
	}
	inner := templates[0].fields[1]
	const escapedSeparator = "\n{{!}}-{{!}}\n"
	const literalSeparator = "\n|-|\n"
	var tabs map[string]string
	if strings.Contains(inner, escapedSeparator) {
		if strings.Contains(inner, literalSeparator) {
			return "", ErrUnsupportedTable
		}
		tabs, err = parseSekaipediaTabberParts(strings.Split(inner, escapedSeparator))
	} else {
		tabs, err = parseSekaipediaTabberEntries(inner)
	}
	if err != nil {
		return "", err
	}
	wanted := "sekai"
	if kind == "vocaloid" {
		wanted = "virtual singer"
	}
	for label, body := range tabs {
		if sekaipediaPrimaryNestedLabelKey(label) == wanted {
			return body, nil
		}
	}
	return "", ErrMissingLyrics
}

func stripSekaipediaLeadingLyricStubs(templates []sekaipediaTemplate) ([]sekaipediaTemplate, error) {
	for len(templates) > 0 && strings.EqualFold(strings.TrimSpace(templates[0].name), "lyric stub") {
		if !sekaipediaLyricStubMarkersKnown(templates[0].fields[1:]) {
			return nil, ErrUnsupportedTable
		}
		templates = templates[1:]
	}
	return templates, nil
}

// sekaipediaLyricStubMarkersKnown reports whether every marker of a {{Lyric stub}} is one the
// parser knows to be metadata only. Sekaipedia combines them, e.g. {{lyric stub|translation|full}}
// on Emerald (song 730), so each marker is checked on its own. Any other marker still rejects
// the page.
func sekaipediaLyricStubMarkersKnown(markers []string) bool {
	for _, marker := range markers {
		switch strings.ToLower(strings.TrimSpace(marker)) {
		case "full", "translation", "lyrics":
		default:
			return false
		}
	}
	return true
}

func parseSekaipediaLyricColumnVariant(
	value string,
	set sekaipediaSingerSet,
) ([]sekaipediaColumnLine, bool, error) {
	lines, err := parseSekaipediaLyricColumn(value, set)
	if err == nil {
		return lines, sekaipediaLinesHavePerformerIDs(lines), nil
	}
	if _, _, _, found := findBalancedSekaipediaNamedTemplate(value, "Lyric"); found {
		return nil, false, err
	}
	lines, plainErr := parseSekaipediaPlainLyricColumn(value)
	if plainErr != nil {
		return nil, false, plainErr
	}
	return lines, false, nil
}

func nextSekaipediaTemplateStart(value string, start int) int {
	for cursor := start; cursor < len(value); {
		if end, recognized, ok := sekaipediaOpaqueMarkupEnd(value, cursor); recognized {
			if !ok {
				return -2
			}
			cursor = end
			continue
		}
		if strings.HasPrefix(value[cursor:], "{{") {
			return cursor
		}
		cursor++
	}
	return -1
}

func expandSekaipediaMultilineColorTemplates(value string, depth int) (string, error) {
	if depth > 8 || value == "" || len(value) > maxExtractedTextBytes || !utf8.ValidString(value) {
		return "", ErrUnsupportedTable
	}
	var result strings.Builder
	for cursor := 0; cursor < len(value); {
		next := nextSekaipediaTemplateStart(value, cursor)
		if next == -2 {
			return "", ErrUnsupportedTable
		}
		if next < 0 {
			result.WriteString(value[cursor:])
			break
		}
		result.WriteString(value[cursor:next])
		_, end, inner, ok := balancedSekaipediaTemplateAt(value, next)
		if !ok {
			return "", ErrUnsupportedTable
		}
		fields, fieldsOK := splitTopLevelSekaipediaFields(inner, "|")
		if !fieldsOK || len(fields) == 0 {
			return "", ErrUnsupportedTable
		}
		if strings.TrimSpace(fields[0]) != "Color" {
			result.WriteString(value[next:end])
			cursor = end
			continue
		}
		if len(fields) != 3 || !validSekaipediaIgnoredFormattingSelector(fields[1]) || strings.TrimSpace(fields[2]) == "" {
			return "", ErrUnsupportedTable
		}
		expanded, err := expandSekaipediaMultilineColorTemplates(fields[2], depth+1)
		if err != nil {
			return "", err
		}
		result.WriteString(expanded)
		cursor = end
		if result.Len() > maxExtractedTextBytes {
			return "", ErrLyricsTooLarge
		}
	}
	return result.String(), nil
}

func validSekaipediaColorSpanOpening(value string) bool {
	const prefix = "<span style=\"color:#"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, "\">") {
		return false
	}
	hex := value[len(prefix) : len(value)-len("\">")]
	if len(hex) != 6 {
		return false
	}
	for _, current := range hex {
		if !((current >= '0' && current <= '9') || (current >= 'a' && current <= 'f') || (current >= 'A' && current <= 'F')) {
			return false
		}
	}
	return true
}

func validSekaipediaMarginSpanOpening(value string) bool {
	const prefix = "<span style=\"margin-left: "
	const suffix = "px\">"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
		return false
	}
	number := value[len(prefix) : len(value)-len(suffix)]
	if len(number) == 0 || len(number) > 4 {
		return false
	}
	for _, current := range number {
		if current < '0' || current > '9' {
			return false
		}
	}
	pixels, err := strconv.Atoi(number)
	return err == nil && pixels >= 0 && pixels <= 4096
}

func parseSekaipediaLyricFormattingTag(value string) (string, bool, int, bool) {
	lower := strings.ToLower(value)
	for _, tag := range []string{"big", "small", "s"} {
		opening := "<" + tag + ">"
		closing := "</" + tag + ">"
		switch {
		case strings.HasPrefix(lower, opening):
			return tag, false, len(opening), true
		case strings.HasPrefix(lower, closing):
			return tag, true, len(closing), true
		}
	}
	if strings.HasPrefix(lower, "</span>") {
		return "span", true, len("</span>"), true
	}
	end := strings.IndexByte(value, '>')
	if end < 0 {
		return "", false, 0, false
	}
	opening := value[:end+1]
	if validSekaipediaColorSpanOpening(opening) || validSekaipediaMarginSpanOpening(opening) {
		return "span", false, len(opening), true
	}
	return "", false, 0, false
}

func unwrapSekaipediaPlainColumnFormatting(value string) (string, error) {
	for depth := 0; depth < 4; depth++ {
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, "<span style=\"color:#") && strings.HasSuffix(value, "</span>") &&
			strings.Count(strings.ToLower(value), "<span") == 1 && strings.Count(strings.ToLower(value), "</span>") == 1:
			openingEnd := strings.IndexByte(value, '>')
			if openingEnd < 0 || !validSekaipediaColorSpanOpening(value[:openingEnd+1]) {
				return "", ErrUnsupportedTable
			}
			value = strings.TrimSpace(value[openingEnd+1 : len(value)-len("</span>")])
			continue
		case strings.HasPrefix(value, "<span style=\"margin-left: ") && strings.HasSuffix(value, "</span>") &&
			strings.Count(strings.ToLower(value), "<span") == 1 && strings.Count(strings.ToLower(value), "</span>") == 1:
			openingEnd := strings.IndexByte(value, '>')
			if openingEnd < 0 || !validSekaipediaMarginSpanOpening(value[:openingEnd+1]) {
				return "", ErrUnsupportedTable
			}
			value = strings.TrimSpace(value[openingEnd+1 : len(value)-len("</span>")])
			continue
		case strings.HasPrefix(value, "'''") && strings.HasSuffix(value, "'''") &&
			len(value) > 6 && strings.Count(value, "'''") == 2:
			value = strings.TrimSpace(value[3 : len(value)-3])
			continue
		case strings.HasPrefix(value, "<s>") && strings.HasSuffix(value, "</s>") &&
			strings.Count(strings.ToLower(value), "<s>") == 1 && strings.Count(strings.ToLower(value), "</s>") == 1:
			value = strings.TrimSpace(value[len("<s>") : len(value)-len("</s>")])
			continue
		case strings.HasPrefix(value, "<big>") && strings.HasSuffix(value, "</big>") &&
			strings.Count(strings.ToLower(value), "<big>") == 1 && strings.Count(strings.ToLower(value), "</big>") == 1:
			value = strings.TrimSpace(value[len("<big>") : len(value)-len("</big>")])
			continue
		case strings.HasPrefix(value, "<small>") && strings.HasSuffix(value, "</small>") &&
			strings.Count(strings.ToLower(value), "<small>") == 1 && strings.Count(strings.ToLower(value), "</small>") == 1:
			value = strings.TrimSpace(value[len("<small>") : len(value)-len("</small>")])
			continue
		case strings.HasPrefix(value, "''") && strings.HasSuffix(value, "''") &&
			len(value) > 4 && strings.Count(value, "''") == 2:
			value = strings.TrimSpace(value[2 : len(value)-2])
			continue
		case strings.HasPrefix(value, "{{"):
			_, end, inner, ok := balancedSekaipediaTemplateAt(value, 0)
			if !ok || end != len(value) {
				return value, nil
			}
			fields, ok := splitTopLevelSekaipediaFields(inner, "|")
			if !ok || len(fields) == 0 || strings.TrimSpace(fields[0]) != "Color" {
				return value, nil
			}
			if len(fields) != 3 || !validSekaipediaIgnoredFormattingSelector(fields[1]) || strings.TrimSpace(fields[2]) == "" {
				return "", ErrUnsupportedTable
			}
			value = strings.TrimSpace(fields[2])
			continue
		default:
			return value, nil
		}
	}
	return "", ErrUnsupportedTable
}
