package store

import (
	"reflect"
	"testing"
)

func TestSuggestLyricsDocumentRubyFillsOnlyKanjiWithoutRuby(t *testing.T) {
	for _, test := range []struct {
		markup, want string
		suggested    []LyricsDocumentSuggestedRuby
		problems     []string
	}{
		{
			markup: "試験の歌", want: "{試験|しけん}の{歌|うた}",
			suggested: []LyricsDocumentSuggestedRuby{{Character: 0, Text: "試験", Reading: "しけん"}, {Character: 9, Text: "歌", Reading: "うた"}},
		},
		{
			// Word-level ruby with okurigana outside the braces.
			markup: "今日も見上げる空", want: "{今日|きょう}も{見上|みあ}げる{空|そら}",
			suggested: []LyricsDocumentSuggestedRuby{
				{Character: 0, Text: "今日", Reading: "きょう"}, {Character: 9, Text: "見上", Reading: "みあ"}, {Character: 18, Text: "空", Reading: "そら"},
			},
		},
		{
			// A ruby the markup writes is kept, katakana reading included.
			markup: "{試験|シケン}の歌", want: "{試験|シケン}の{歌|うた}",
			suggested: []LyricsDocumentSuggestedRuby{{Character: 9, Text: "歌", Reading: "うた"}},
		},
		{
			markup: "{今日|きょう}も明日も", want: "{今日|きょう}も{明日|あした}も",
			suggested: []LyricsDocumentSuggestedRuby{{Character: 9, Text: "明日", Reading: "あした"}},
		},
		{
			// The dictionary word 試験 overlaps the written {試|し}, so 験 stays
			// without ruby and is reported as PUT would report it.
			markup: "{試|し}験の歌", want: "{試|し}験の{歌|うた}",
			suggested: []LyricsDocumentSuggestedRuby{{Character: 7, Text: "歌", Reading: "うた"}},
			problems:  []string{"kanji without a {kanji|reading}: 「験」 at character 5"},
		},
		{
			// Literal braces stay escaped around the added ruby.
			markup: "{{合成}}の歌", want: "{{{合成|ごうせい}}}の{歌|うた}",
			suggested: []LyricsDocumentSuggestedRuby{{Character: 2, Text: "合成", Reading: "ごうせい"}, {Character: 14, Text: "歌", Reading: "うた"}},
		},
		{markup: "ルルル", want: "ルルル"},
		{markup: "{試験|しけん}の{歌|うた}", want: "{試験|しけん}の{歌|うた}"},
		{
			// Any other ja problem returns the markup unchanged.
			markup: "{試験の歌", want: "{試験の歌",
			problems: []string{
				"unclosed '{' at character 0; write {{ for a literal {",
				"kanji without a {kanji|reading}: 「試験」 at character 1, 「歌」 at character 4",
			},
		},
	} {
		got, err := SuggestLyricsDocumentRuby(test.markup)
		if err != nil {
			t.Fatalf("%s: %v", test.markup, err)
		}
		if test.suggested == nil {
			test.suggested = []LyricsDocumentSuggestedRuby{}
		}
		if test.problems == nil {
			test.problems = []string{}
		}
		want := LyricsDocumentRubySuggestion{Japanese: test.want, Suggested: test.suggested, Problems: test.problems}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %+v\nwant %+v", test.markup, got, want)
		}
		_, _, problems := parseLyricsDocumentRuby(got.Japanese)
		if len(problems) == 0 {
			problems = []string{}
		}
		if !reflect.DeepEqual(problems, got.Problems) {
			t.Fatalf("%s: PUT would report %q, the suggestion reports %q", test.markup, problems, got.Problems)
		}
		for _, suggested := range got.Suggested {
			written := string([]rune(got.Japanese)[suggested.Character:][:len([]rune(suggested.Text))+len([]rune(suggested.Reading))+3])
			if written != "{"+suggested.Text+"|"+suggested.Reading+"}" {
				t.Fatalf("%s: character %d reads %q", test.markup, suggested.Character, written)
			}
		}
	}
}
