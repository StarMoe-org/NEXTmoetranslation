package lyricssource

import (
	"errors"

	"regexp"

	"strings"
	"sync"

	"github.com/ikawaha/kagome/v2/tokenizer"
)

const (
	historicalRubyGeneratorVersion = "kagome-ipadic-v1"
	rubyGeneratorVersion           = "kagome-ipadic-han-kana-v2"
)

var (
	structuredTabberStartPattern        = regexp.MustCompile(`(?i)<tabber\b[^>]*>`)
	structuredTabberEndPattern          = regexp.MustCompile(`(?i)</tabber\s*>`)
	structuredTabberSuffixPattern       = regexp.MustCompile(`(?i)^\s*(?:\{\{clr\}\})?\s*$`)
	structuredTabSeparator              = regexp.MustCompile(`(?m)^\|-\|\s*`)
	structuredVersionPattern            = regexp.MustCompile(`(?m)^\s*([^=\n][^\n]*?)\s*=\s*\n`)
	structuredInlineTableVersionPattern = regexp.MustCompile(`(?m)^[ \t\n]*([^=\n][^\n]*?)[ \t]*=[ \t]*(\{\|[^\n]*?)[ \t]*(?:\n|$)`)
	structuredLegendPattern             = regexp.MustCompile(`(?is)\{\{\s*lrc\s+legend\s*\|(.*?)\}\}`)
	structuredSharedCellPattern         = regexp.MustCompile(`(?is)^\s*\{\{\s*shared(?:\s*\|\s*([1-9][0-9]?))?\s*\}\}\s*(.*)$`)
	structuredColorPattern              = regexp.MustCompile(`(?is)\{\{\s*lrc\s+color\s*\|\s*([^|{}]+?)\s*\|\s*([^|{}]*?)\s*\}\}`)
	structuredRubyPattern               = regexp.MustCompile(`(?is)\{\{\s*ruby\s*\|\s*([^|{}]+?)\s*\|\s*([^|{}]+?)\s*\}\}`)
	structuredNowikiPattern             = regexp.MustCompile(`(?is)<nowiki>([^\r\n]*?)</nowiki>`)
	structuredLyricsTemplatePattern     = regexp.MustCompile(`(?is)^\s*\{\{\s*lyrics\s*\|(.*)\}\}\s*$`)
	structuredDisplayTemplatePattern    = regexp.MustCompile(`(?is)\{\{\s*(vlw|wp|color)\s*\|([^{}]*)\}\}`)
	structuredInterwikiPattern          = regexp.MustCompile(`(?is)\{\{\s*iw\s*\|([^{}]*)\}\}`)
	structuredCombinedTabSeparator      = regexp.MustCompile(`(?m)^\s*\|}\s*\|-\|`)
	structuredLegendOptionPattern       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*\s*=`)
	structuredAttributeName             = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_.:-]*`)
	structuredDisplayAttributeName      = regexp.MustCompile(`(?i)^(?:class|style|align|valign|bgcolor|border|width|height)$`)
	structuredBreakPattern              = regexp.MustCompile(`(?i)^\s*<br\s*/?>\s*$`)
	structuredColspanPattern            = regexp.MustCompile(`(?i)^\s*colspan\s*=\s*(?:"([2-9])"|'([2-9])'|([2-9]))\s*$`)
	structuredSupportedColor            = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	structuredModelCodePattern          = regexp.MustCompile(`^[A-Z0-9]+[a-z]?(?: [A-Z0-9]+[a-z]?){0,3}$`)
	structuredUnclosedColorCellPattern  = regexp.MustCompile(`(?i)^\|\{\{\s*lrc\s+color\s*\|\s*([^|{}\s]+)\s*\|\s*([^|{}\[\]<>]+?)\s*$`)

	furiganaTokenizerOnce  sync.Once
	furiganaTokenizer      *tokenizer.Tokenizer
	furiganaTokenizerErr   error
	furiganaReadingsBlob   string
	furiganaReadingOffsets []uint32
)

// Named colors are normalized to hex before they can reach editor swatches.
// Keep this as a finite CSS-color whitelist rather than accepting arbitrary CSS.
var structuredNamedColors = map[string]string{
	"aqua":          "#00FFFF",
	"black":         "#000000",
	"blue":          "#0000FF",
	"fuchsia":       "#FF00FF",
	"gray":          "#808080",
	"green":         "#008000",
	"grey":          "#808080",
	"lime":          "#00FF00",
	"maroon":        "#800000",
	"navy":          "#000080",
	"olive":         "#808000",
	"orange":        "#FFA500",
	"palevioletred": "#DB7093",
	"purple":        "#800080",
	"red":           "#FF0000",
	"silver":        "#C0C0C0",
	"teal":          "#008080",
	"violet":        "#EE82EE",
	"white":         "#FFFFFF",
	"yellow":        "#FFFF00",
}

type structuredVersionBlock struct {
	label        string
	body         string
	kind         string
	languageRole string
}

type structuredRawSegment struct {
	text         string
	performerIDs []string
}

func extractStructuredLyrics(content string) (Extraction, error) {
	if hasLyricsTextRestriction(content, nil) {
		return Extraction{}, ErrRestrictedReprint
	}
	section, err := structuredLyricsSection(content)
	if err != nil {
		return Extraction{}, err
	}
	blocks, err := structuredLyricsVersionBlocks(section)
	if err != nil {
		return Extraction{}, err
	}
	selected, err := selectStructuredLyricsVersion(blocks)
	if err != nil {
		return Extraction{}, err
	}
	performers, err := extractStructuredLegend(selected.body)
	legendFallback := errors.Is(err, ErrUnsupportedTable)
	if err != nil && !legendFallback {
		return Extraction{}, err
	}
	if legendFallback {
		performers = []Performer{}
	} else {
		selected.body = repairBoundedStructuredSourceTable(selected.body, performers)
	}
	var lines []StructuredLine
	tableCount := strings.Count(selected.body, "{|")
	if tableCount == 0 {
		plainBody, wrappedTemplate, templateErr := unwrapStructuredLyricsTemplate(selected.body)
		if templateErr != nil {
			return Extraction{}, templateErr
		}
		var legacy []ExtractedLine
		if wrappedTemplate {
			legacy, err = extractPlainSourceLyrics(plainBody)
		} else {
			legacy, err = extractPlainLyrics(plainBody)
		}
		if err != nil {
			return Extraction{}, err
		}
		lines, err = structuredLinesFromLegacy(legacy)
	} else if tableCount == 1 && strings.Count(selected.body, "|}") == 1 {
		if !legendFallback {
			lines, err = extractStructuredLyricsTable(selected.body, performers)
		} else {
			err = ErrUnsupportedTable
		}
		if errors.Is(err, ErrUnsupportedTable) {
			var legacy []ExtractedLine
			legacy, err = extractPlaintextLyricsTable(selected.body)
			if err == nil {
				lines, err = structuredLinesFromLegacy(legacy)
			}
		}
	} else {
		return Extraction{}, ErrUnsupportedTable
	}
	if err != nil {
		return Extraction{}, err
	}
	return Extraction{
		Version:              LyricsVersion{Kind: selected.kind, Label: selected.label},
		Performers:           performers,
		RubyGeneratorVersion: rubyGeneratorVersion,
		Lines:                lines,
	}, nil
}

func structuredLyricsSection(content string) (string, error) {
	location := headingPattern.FindStringIndex(content)
	if location == nil {
		return "", ErrMissingLyrics
	}
	section := strings.ReplaceAll(content[location[1]:], "\r", "")
	depth := 0
	lineStart := 0
	for lineStart < len(section) {
		lineEnd := strings.IndexByte(section[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(section)
		} else {
			lineEnd += lineStart
		}
		line := section[lineStart:lineEnd]
		if depth == 0 && topLevelHeadingPattern.MatchString(line) {
			return section[:lineStart], nil
		}
		starts := len(structuredTabberStartPattern.FindAllStringIndex(line, -1))
		ends := len(structuredTabberEndPattern.FindAllStringIndex(line, -1))
		depth += starts - ends
		if depth < 0 || depth > 1 {
			return "", ErrUnsupportedTable
		}
		if lineEnd == len(section) {
			break
		}
		lineStart = lineEnd + 1
	}
	return section, nil
}

func unwrapStructuredLyricsTemplate(body string) (string, bool, error) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(strings.ToLower(trimmed), "{{lyrics") {
		return body, false, nil
	}
	match := structuredLyricsTemplatePattern.FindStringSubmatch(trimmed)
	if match == nil || strings.Contains(match[1], "{{") || strings.Contains(match[1], "}}") || strings.Contains(match[1], "|") {
		return "", false, ErrUnsupportedTable
	}
	return match[1], true, nil
}

func extractPlainSourceLyrics(source string) ([]ExtractedLine, error) {
	raw := strings.Split(strings.ReplaceAll(source, "\r", ""), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		sanitized, err := sanitizeLyricText(line)
		if err != nil {
			return nil, err
		}
		lines = append(lines, sanitized)
	}
	return lyricLines(lines, false)
}

func structuredLyricsVersionBlocks(section string) ([]structuredVersionBlock, error) {
	section = strings.ReplaceAll(section, "\r", "")
	section = structuredCombinedTabSeparator.ReplaceAllString(section, "|}\n|-|")
	start := structuredTabberStartPattern.FindStringIndex(section)
	if start == nil {
		return []structuredVersionBlock{{label: "Original Version", body: section, kind: "original", languageRole: "source"}}, nil
	}
	prefix, err := structuredTabberPrefix(section[:start[0]])
	if err != nil {
		return nil, err
	}
	rest := section[start[1]:]
	end := structuredTabberEndPattern.FindStringIndex(rest)
	if end == nil || !structuredTabberSuffixPattern.MatchString(rest[end[1]:]) {
		return nil, ErrUnsupportedTable
	}
	tabber := rest[:end[0]]
	parts := structuredTabSeparator.Split(tabber, -1)
	blocks := make([]structuredVersionBlock, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		match := structuredVersionPattern.FindStringSubmatch(part)
		labelRaw := ""
		body := ""
		if match == nil {
			match = structuredInlineTableVersionPattern.FindStringSubmatch(part)
			if match == nil {
				return nil, ErrUnsupportedTable
			}
			labelRaw = match[1]
			body = match[2] + "\n" + part[len(match[0]):]
		} else {
			labelRaw = match[1]
			body = part[len(match[0]):]
		}
		label := strings.Join(strings.Fields(strings.TrimSpace(labelRaw)), " ")
		body = repairStructuredVersionBoundaryTableClose(body)
		blocks = append(blocks, structuredVersionBlock{
			label: label, body: prefix + body, kind: structuredVersionKind(label),
		})
	}
	if len(blocks) == 0 {
		return nil, ErrMissingLyrics
	}
	assignStructuredVersionLanguageRoles(blocks)
	return blocks, nil
}

func structuredTabberPrefix(prefix string) (string, error) {
	if strings.TrimSpace(prefix) == "" {
		return "", nil
	}
	matches := structuredLegendPattern.FindAllStringIndex(prefix, -1)
	if len(matches) != 1 {
		return "", ErrUnsupportedTable
	}
	match := matches[0]
	if strings.TrimSpace(prefix[:match[0]]+prefix[match[1]:]) != "" {
		return "", ErrUnsupportedTable
	}
	return prefix[match[0]:match[1]] + "\n", nil
}

// repairStructuredVersionBoundaryTableClose recognizes one exact tab-boundary
// typo: a single Japanese/Romaji table whose final non-empty line is a row
// separator because its closing `|}` was omitted immediately before the next
// `|-|` tab. It never repairs arbitrary unclosed tables or table contents.
func repairStructuredVersionBoundaryTableClose(body string) string {
	if strings.Count(body, "{|") != 1 || strings.Count(body, "|}") != 0 {
		return body
	}
	lines := strings.Split(body, "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last < 0 || strings.TrimSpace(lines[last]) != "|-" {
		return body
	}
	lines[last] = "|}"
	repaired := strings.Join(lines, "\n")
	_, _, table, ok := selectedStructuredSingleTable(repaired)
	if !ok || !strictMixedEnglishJapaneseRomajiTable(table) {
		return body
	}
	return repaired
}

func structuredVersionKind(label string) string {
	value := strings.ToLower(label)
	switch {
	case strings.Contains(value, "vocaloid"), strings.Contains(value, "virtual singer"):
		return "vocaloid"
	case strings.Contains(value, "project sekai"), strings.Contains(value, "pjsk"), strings.Contains(value, "sekai"),
		strings.Contains(value, "version") && (strings.Contains(value, "nightcord") || strings.Contains(value, "leo/need") ||
			strings.Contains(value, "more more jump") || strings.Contains(value, "vivid bad squad") ||
			strings.Contains(value, "wonderlands") || strings.Contains(value, "25-ji")):
		return "sekai"
	default:
		return "original"
	}
}

func selectStructuredLyricsVersion(blocks []structuredVersionBlock) (structuredVersionBlock, error) {
	source := make([]structuredVersionBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.languageRole == "source" {
			source = append(source, block)
		}
	}
	if len(source) == 0 {
		return structuredVersionBlock{}, ErrMissingLyrics
	}
	source = preferExplicitCompleteStructuredVersions(source)
	sekai := make([]structuredVersionBlock, 0, 1)
	for _, block := range source {
		if block.kind == "sekai" {
			sekai = append(sekai, block)
		}
	}
	if len(sekai) == 1 {
		return sekai[0], nil
	}
	if len(sekai) > 1 {
		return structuredVersionBlock{}, ErrAmbiguous
	}
	if len(source) == 1 {
		return source[0], nil
	}
	return structuredVersionBlock{}, ErrAmbiguous
}

func extractStructuredLegend(body string) ([]Performer, error) {
	match := structuredLegendPattern.FindStringSubmatch(body)
	if match == nil {
		return []Performer{}, nil
	}
	parts := strings.Split(match[1], "|")
	if len(parts) < 2 {
		return nil, ErrUnsupportedTable
	}
	performers := make([]Performer, 0, len(parts)-1)
	seen := map[string]struct{}{}
	for _, raw := range parts[1:] {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, ErrUnsupportedTable
		}
		if structuredLegendOptionPattern.MatchString(raw) {
			continue
		}
		name, color, found := strings.Cut(raw, ":")
		name = strings.Join(strings.Fields(strings.TrimSpace(name)), " ")
		color = strings.TrimSpace(color)
		id := normalizePerformerID(name)
		if id == "" {
			return nil, ErrUnsupportedTable
		}
		if found {
			var colorOK bool
			color, colorOK = normalizeStructuredLegendColor(color)
			if !colorOK {
				return nil, ErrUnsupportedTable
			}
		}
		if _, exists := seen[id]; exists {
			return nil, ErrUnsupportedTable
		}
		seen[id] = struct{}{}
		performers = append(performers, Performer{PerformerID: id, Name: name, Color: color})
	}
	return performers, nil
}

func normalizeStructuredLegendColor(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if structuredSupportedColor.MatchString(value) {
		return strings.ToUpper(value), true
	}
	color, ok := structuredNamedColors[strings.ToLower(value)]
	return color, ok
}
