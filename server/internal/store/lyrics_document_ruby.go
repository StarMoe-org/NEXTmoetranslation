package store

import (
	"sort"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/lyricssource"
)

// lyricsDocumentMissingRubyProblem starts the ja problem that lists the kanji
// written without a {kanji|reading}.
const lyricsDocumentMissingRubyProblem = "kanji without a {kanji|reading}: "

// LyricsDocumentRubySuggestion is one ja markup with dictionary ruby written
// for the kanji that had none.
type LyricsDocumentRubySuggestion struct {
	// Japanese is the markup with the suggested ruby added; every ruby the
	// input wrote, and every other character, is kept as written.
	Japanese string `json:"ja"`
	// Suggested lists the added ruby in order.
	Suggested []LyricsDocumentSuggestedRuby `json:"suggested"`
	// Problems are the ja problems PUT would still report for Japanese.
	Problems []string `json:"problems"`
}

// LyricsDocumentSuggestedRuby is one added ruby; Character is the zero-based
// character index of its '{' in the returned markup.
type LyricsDocumentSuggestedRuby struct {
	Character int    `json:"character"`
	Text      string `json:"text"`
	Reading   string `json:"reading"`
}

// SuggestLyricsDocumentRuby writes the Kagome/IPADIC dictionary reading of
// every word whose kanji carry no ruby in markup. A word that overlaps a ruby
// the markup already writes is left alone, as is a word the dictionary cannot
// read; both stay in Problems. Markup with any other ja problem is returned
// unchanged with its problems. The readings are dictionary guesses: lyrics
// often read a word differently, so a caller checks each one.
func SuggestLyricsDocumentRuby(markup string) (LyricsDocumentRubySuggestion, error) {
	result := LyricsDocumentRubySuggestion{Japanese: markup, Suggested: []LyricsDocumentSuggestedRuby{}, Problems: []string{}}
	text, spans, problems := parseLyricsDocumentRuby(markup)
	if len(problems) == 0 {
		return result, nil
	}
	if len(problems) > 1 || !strings.HasPrefix(problems[0], lyricsDocumentMissingRubyProblem) {
		result.Problems = problems
		return result, nil
	}
	type ruby struct {
		start, end int
		reading    string
		suggested  bool
	}
	runes := []rune(text)
	annotated := make([]bool, len(runes))
	var rubies []ruby
	at := 0
	for _, span := range spans {
		length := utf8.RuneCountInString(span.Text)
		if span.Reading != "" {
			rubies = append(rubies, ruby{start: at, end: at + length, reading: span.Reading})
			for index := at; index < at+length; index++ {
				annotated[index] = true
			}
		}
		at += length
	}
	generated, err := lyricssource.SuggestRubySpans(text)
	if err != nil {
		return result, err
	}
	at = 0
	for _, span := range generated {
		length := utf8.RuneCountInString(span.Text)
		free := span.Reading != "" && publicV3ReadingBaseSpan(span.Text) && lyricsDocumentKanaReading(span.Reading)
		for index := at; free && index < at+length; index++ {
			free = !annotated[index]
		}
		if free {
			rubies = append(rubies, ruby{start: at, end: at + length, reading: span.Reading, suggested: true})
		}
		at += length
	}
	sort.Slice(rubies, func(left, right int) bool { return rubies[left].start < rubies[right].start })
	var out strings.Builder
	written, cursor := 0, 0
	write := func(value string) {
		out.WriteString(value)
		written += utf8.RuneCountInString(value)
	}
	for _, current := range rubies {
		write(escapeLyricsDocumentMarkup(string(runes[cursor:current.start])))
		base := string(runes[current.start:current.end])
		if current.suggested {
			result.Suggested = append(result.Suggested, LyricsDocumentSuggestedRuby{Character: written, Text: base, Reading: current.reading})
		}
		write("{" + base + "|" + current.reading + "}")
		cursor = current.end
	}
	write(escapeLyricsDocumentMarkup(string(runes[cursor:])))
	result.Japanese = out.String()
	if _, _, remaining := parseLyricsDocumentRuby(result.Japanese); len(remaining) > 0 {
		result.Problems = remaining
	}
	return result, nil
}
