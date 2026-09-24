package store

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

// lyricsDocumentSourceRevision is one wiki revision named by a lyrics document
// source URL, normalized to the canonical /wiki/<page>?oldid=<id> form.
type lyricsDocumentSourceRevision struct {
	provider     model.LyricsSourceProvider
	origin       string
	title        string
	revisionID   int
	canonicalURL string
}

var errLyricsDocumentSourceRevision = errors.New("source revision is invalid")

// parseLyricsDocumentSourceURL accepts /wiki/<page>?oldid=<id> and
// index.php?title=<page>&oldid=<id> revision URLs on the provider origins
// known to the source contract. The display title comes from the page name
// unless titleOverride is set.
func parseLyricsDocumentSourceURL(raw, titleOverride string) (lyricsDocumentSourceRevision, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Port() != "" {
		return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: source.url must be an http(s) wiki revision URL", errLyricsDocumentSourceRevision)
	}
	host := strings.ToLower(parsed.Hostname())
	var result lyricsDocumentSourceRevision
	for _, provider := range []model.LyricsSourceProvider{
		model.LyricsSourceProviderVocaloidFandom, model.LyricsSourceProviderSekaipedia, model.LyricsSourceProviderMoegirl,
	} {
		for _, origin := range model.LyricsSourceProviderOrigins(provider) {
			if host == strings.TrimPrefix(origin, "https://") {
				result.provider, result.origin = provider, origin
			}
		}
	}
	if result.provider == "" {
		return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: source host %q is not a supported lyrics wiki", errLyricsDocumentSourceRevision, host)
	}
	query := parsed.Query()
	oldids := query["oldid"]
	if len(oldids) != 1 {
		return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: source.url must pin one revision with ?oldid=<revision id>", errLyricsDocumentSourceRevision)
	}
	revisionID, err := strconv.Atoi(oldids[0])
	if err != nil || revisionID <= 0 || strconv.Itoa(revisionID) != oldids[0] {
		return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: oldid must be a positive revision id", errLyricsDocumentSourceRevision)
	}
	var page string
	switch path := parsed.EscapedPath(); {
	case strings.HasPrefix(path, "/wiki/") && len(path) > len("/wiki/"):
		page, err = url.PathUnescape(strings.TrimPrefix(path, "/wiki/"))
		if err != nil {
			return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: source page name is not valid percent-encoding", errLyricsDocumentSourceRevision)
		}
	case path == "/index.php" || path == "/w/index.php":
		page = query.Get("title")
	default:
		return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: source.url must be a /wiki/<page> or index.php?title=<page> URL", errLyricsDocumentSourceRevision)
	}
	page = strings.Trim(strings.ReplaceAll(strings.TrimSpace(page), " ", "_"), "_")
	// MediaWiki titles may contain '?' (served as %3F); '#' starts a section.
	if page == "" || !utf8.ValidString(page) || strings.ContainsAny(page, "\r\n\x00#") {
		return lyricsDocumentSourceRevision{}, fmt.Errorf("%w: source page name is empty or invalid", errLyricsDocumentSourceRevision)
	}
	segments := strings.Split(page, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	result.revisionID = revisionID
	result.canonicalURL = result.origin + "/wiki/" + strings.Join(segments, "/") + "?oldid=" + strconv.Itoa(revisionID)
	result.title = strings.TrimSpace(titleOverride)
	if result.title == "" {
		result.title = strings.ReplaceAll(page, "_", " ")
	}
	return result, nil
}

// parseLyricsDocumentRuby splits {base|reading} markup into ruby spans; {{ and
// }} are a literal { and }, which markup valid before the escape rejected. It
// returns every problem it finds so a caller can report them all at once;
// each names the zero-based character index in the markup.
func parseLyricsDocumentRuby(markup string) (string, []model.LyricsSourceRubySpan, []string) {
	return parseLyricsDocumentRubyAt(markup, 0)
}

// parseLyricsDocumentRubyAt parses markup that starts at character offset of
// the line's ja, so a segment's problems name positions in the whole line.
func parseLyricsDocumentRubyAt(markup string, offset int) (string, []model.LyricsSourceRubySpan, []string) {
	var text, plain strings.Builder
	spans := []model.LyricsSourceRubySpan{}
	var problems, missing []string
	var kanjiRun []rune
	kanjiStart := 0
	flushKanji := func() {
		if len(kanjiRun) > 0 {
			missing = append(missing, fmt.Sprintf("「%s」 at character %d", string(kanjiRun), kanjiStart))
			kanjiRun = nil
		}
	}
	flushPlain := func() {
		flushKanji()
		if plain.Len() > 0 {
			spans = append(spans, model.LyricsSourceRubySpan{Text: plain.String()})
			plain.Reset()
		}
	}
	runes := []rune(markup)
	for index := 0; index < len(runes); index++ {
		at := offset + index
		if (runes[index] == '{' || runes[index] == '}') && index+1 < len(runes) && runes[index+1] == runes[index] {
			flushKanji()
			plain.WriteRune(runes[index])
			text.WriteRune(runes[index])
			index++
			continue
		}
		switch runes[index] {
		case '}':
			flushKanji()
			problems = append(problems, fmt.Sprintf("unbalanced '}' at character %d; write }} for a literal }", at))
		case '{':
			flushKanji()
			end, separator := -1, -1
			for cursor := index + 1; cursor < len(runes); cursor++ {
				if runes[cursor] == '}' {
					end = cursor
					break
				}
				if runes[cursor] == '{' {
					break
				}
				if runes[cursor] == '|' && separator < 0 {
					separator = cursor
				}
			}
			if end < 0 {
				problems = append(problems, fmt.Sprintf("unclosed '{' at character %d; write {{ for a literal {", at))
				continue
			}
			written := string(runes[index : end+1])
			if separator < 0 {
				problems = append(problems, fmt.Sprintf("ruby %s at character %d must be written {kanji|reading}", written, at))
				index = end
				continue
			}
			base, reading := string(runes[index+1:separator]), string(runes[separator+1:end])
			index = end
			switch {
			case base == "" || reading == "":
				problems = append(problems, fmt.Sprintf("ruby %s at character %d has an empty base or reading", written, at))
				continue
			case strings.Contains(reading, "|"):
				problems = append(problems, fmt.Sprintf("ruby %s at character %d has more than one '|'", written, at))
				continue
			case !publicV3ReadingBaseSpan(base):
				problems = append(problems, fmt.Sprintf("ruby base %q in %s at character %d must contain only kanji", base, written, at))
				continue
			case !lyricsDocumentKanaReading(reading):
				problems = append(problems, fmt.Sprintf("ruby reading %q in %s at character %d must be kana matching ^[ぁ-ゖァ-ヺー・゙゚]+$ and start with kana", reading, written, at))
				continue
			}
			flushPlain()
			spans = append(spans, model.LyricsSourceRubySpan{Text: base, Reading: reading})
			text.WriteString(base)
		default:
			if model.LyricsSourceRubyBaseRune(runes[index]) {
				if len(kanjiRun) == 0 {
					kanjiStart = at
				}
				kanjiRun = append(kanjiRun, runes[index])
			} else {
				flushKanji()
			}
			plain.WriteRune(runes[index])
			text.WriteRune(runes[index])
		}
	}
	flushPlain()
	if len(missing) > 0 {
		problems = append(problems, lyricsDocumentMissingRubyProblem+strings.Join(missing, ", "))
	}
	return text.String(), spans, problems
}

// escapeLyricsDocumentMarkup writes plain lyrics text as markup.
func escapeLyricsDocumentMarkup(text string) string {
	return strings.NewReplacer("{", "{{", "}", "}}").Replace(text)
}

type lyricsDocumentParsedSegment struct {
	text  string
	spans []model.LyricsSourceRubySpan
}

// parseLyricsDocumentSegments parses each segment's markup on its own, so a
// ruby cannot span two segments; the markups must concatenate to the line's.
// The segment markups are parsed only when the line markup parsed cleanly.
// Problems name character indices in the line's ja.
func parseLyricsDocumentSegments(line string, segments []LyricsDocumentSegment, lineParsed bool) ([]lyricsDocumentParsedSegment, []string) {
	if len(segments) == 0 || len(segments) > maxLyricsSegmentsPerLine {
		return nil, []string{fmt.Sprintf("segments, when present, must list 1 to %d segments; it lists %d", maxLyricsSegmentsPerLine, len(segments))}
	}
	var joined strings.Builder
	for _, segment := range segments {
		joined.WriteString(segment.Japanese)
	}
	if joined.String() != line {
		joinedRunes, lineRunes := []rune(joined.String()), []rune(line)
		index := 0
		for index < len(joinedRunes) && index < len(lineRunes) && joinedRunes[index] == lineRunes[index] {
			index++
		}
		excerpt := func(runes []rune) string {
			rest := runes[index:]
			if len(rest) > 16 {
				return string(rest[:16]) + "…"
			}
			return string(rest)
		}
		return nil, []string{fmt.Sprintf("the segment ja values must concatenate to the line's ja; they first differ at character %d: the line has %q, the segments give %q",
			index, excerpt(lineRunes), excerpt(joinedRunes))}
	}
	if !lineParsed {
		return nil, nil
	}
	var problems []string
	parsed := make([]lyricsDocumentParsedSegment, len(segments))
	offset := 0
	for index, segment := range segments {
		text, spans, segmentProblems := parseLyricsDocumentRubyAt(segment.Japanese, offset)
		for _, problem := range segmentProblems {
			problems = append(problems, fmt.Sprintf("segment %d: %s", index, problem))
		}
		if strings.TrimSpace(text) == "" {
			problems = append(problems, fmt.Sprintf("segment %d has no text (it starts at character %d)", index, offset))
		}
		parsed[index] = lyricsDocumentParsedSegment{text: text, spans: spans}
		offset += utf8.RuneCountInString(segment.Japanese)
	}
	return parsed, problems
}

// lyricsDocumentKanaReading is the main-site reading pattern
// ^[ぁ-ゖァ-ヺー・゙゚]+$ narrowed to readings that start with kana, which the
// source and public validators also require.
func lyricsDocumentKanaReading(value string) bool {
	for index, current := range value {
		kana := current >= 'ぁ' && current <= 'ゖ' || current >= 'ァ' && current <= 'ヺ'
		if index == 0 && !kana {
			return false
		}
		if !kana && current != 'ー' && current != '・' && current != '\u3099' && current != '\u309A' {
			return false
		}
	}
	return value != "" && publicV3KanaReading(value)
}
