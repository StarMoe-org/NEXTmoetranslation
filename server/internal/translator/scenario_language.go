package translator

import "fmt"

// scenarioScriptCounts counts the kana, hangul, CJK ideographs and ASCII
// letters of a scenario's TalkData bodies.
type scenarioScriptCounts struct {
	kana, hangul, han, latin int
}

func countScenarioScripts(value any) scenarioScriptCounts {
	var counts scenarioScriptCounts
	talks, _ := asMap(value)["TalkData"].([]any)
	for _, talk := range talks {
		body, _ := asMap(talk)["Body"].(string)
		for _, r := range body {
			switch {
			case r >= 0x3041 && r <= 0x30ff:
				counts.kana++
			case (r >= 0x1100 && r <= 0x11ff) || (r >= 0x3130 && r <= 0x318f) || (r >= 0xac00 && r <= 0xd7a3):
				counts.hangul++
			case (r >= 0x3400 && r <= 0x4dbf) || (r >= 0x4e00 && r <= 0x9fff):
				counts.han++
			case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
				counts.latin++
			}
		}
	}
	return counts
}

// errNotJapaneseScenario tells why a scenario fetched as JP is not Japanese,
// or is nil. The asset mirror has served other servers' files under JP paths:
// events 168, 169 and 172–174 were the Taiwan server's Traditional Chinese,
// and some card and special stories Korean. Their ScenarioId gives nothing
// away (the genuine JP files of those events say one event lower too), so the
// text is read: Japanese has more than a quarter as much kana as kanji, the
// rule SekaiStoryRipper 0.11.2 uses. Text of symbols only passes.
func errNotJapaneseScenario(value any) error {
	c := countScenarioScripts(value)
	if c.kana+c.hangul+c.han+c.latin == 0 || c.kana*4 > c.han {
		return nil
	}
	return fmt.Errorf("not Japanese text (kana %d, hangul %d, kanji %d, latin %d)", c.kana, c.hangul, c.han, c.latin)
}
