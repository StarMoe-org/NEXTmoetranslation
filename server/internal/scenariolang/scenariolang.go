// Package scenariolang tells whether a game scenario's text is written in the
// language of the server it was fetched for.
//
// The asset mirrors have served other servers' scenarios under a server's own
// path: JP events 168, 169 and 172–174 were the Taiwan server's Traditional
// Chinese, some JP cards and area talks Korean or Chinese, and the CN mirrors
// serve the Japanese text of scenarios CN has listed but not yet translated.
// A scenario's ScenarioId gives none of this away, so the text is counted:
// kana, hangul, CJK ideographs and ASCII letters in its TalkData bodies. The
// rules are those SekaiStoryRipper 0.11.2 uses. Text with none of the four
// (symbols only) passes every rule.
package scenariolang

import "fmt"

// Counts are the characters of each script in a scenario's text.
type Counts struct {
	Kana, Hangul, Han, Latin int
}

// Count counts the scripts of the given texts.
func Count(texts ...string) Counts {
	var c Counts
	for _, text := range texts {
		for _, r := range text {
			switch {
			case r >= 0x3041 && r <= 0x30ff:
				c.Kana++
			case (r >= 0x1100 && r <= 0x11ff) || (r >= 0x3130 && r <= 0x318f) || (r >= 0xac00 && r <= 0xd7a3):
				c.Hangul++
			case (r >= 0x3400 && r <= 0x4dbf) || (r >= 0x4e00 && r <= 0x9fff):
				c.Han++
			case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
				c.Latin++
			}
		}
	}
	return c
}

// CountScenario counts the TalkData bodies of a decoded scenario JSON.
func CountScenario(value any) Counts {
	object, _ := value.(map[string]any)
	talks, _ := object["TalkData"].([]any)
	bodies := make([]string, 0, len(talks))
	for _, talk := range talks {
		line, _ := talk.(map[string]any)
		if body, ok := line["Body"].(string); ok {
			bodies = append(bodies, body)
		}
	}
	return Count(bodies...)
}

func (c Counts) empty() bool { return c.Kana+c.Hangul+c.Han+c.Latin == 0 }

// Japanese: more than a quarter as much kana as kanji.
func (c Counts) Japanese() bool { return c.empty() || c.Kana*4 > c.Han }

// Chinese: more than ten times as much kanji as kana and as hangul, and more
// kanji than Latin letters.
func (c Counts) Chinese() bool {
	return c.empty() || (c.Kana*10 < c.Han && c.Hangul*10 < c.Han && c.Han > c.Latin)
}

// English: more Latin letters than kana, hangul and kanji together.
func (c Counts) English() bool { return c.empty() || c.Latin > c.Kana+c.Hangul+c.Han }

func (c Counts) String() string {
	return fmt.Sprintf("kana %d, hangul %d, kanji %d, latin %d", c.Kana, c.Hangul, c.Han, c.Latin)
}
