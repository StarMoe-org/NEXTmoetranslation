package scenariolang

import "testing"

// First lines of files the asset mirrors served: event 168/1 in Japanese, in
// the Taiwan server's Traditional Chinese and in the CN server's Chinese, card
// 1116/1 in Korean, card 1/1 in English.
const (
	textJP = "——皆さん、お待たせしました！\n受付が終わったので、中に入りましょう"
	textTW = "──讓大家久等了！報到手續完成了，我們進去吧。"
	textCN = "——大家久等了！\n手续已经办完了，我们进去吧。"
	textKR = "……그럼 컬래버레이션 스테이지에서 할 쇼 대본도\n완성됐으니, 당장 연습을 시작해 볼까? 아오야기 군."
	textEN = "What a nice breeze..."
)

func TestEachLanguageRuleTakesItsOwnTextOnly(t *testing.T) {
	for _, test := range []struct {
		text                       string
		japanese, chinese, english bool
	}{
		{textJP, true, false, false},
		{textTW, false, true, false},
		{textCN, false, true, false},
		{textKR, false, false, false},
		{textEN, false, false, true},
		{"……！♪——", true, true, true},
		{"", true, true, true},
	} {
		c := Count(test.text)
		if c.Japanese() != test.japanese || c.Chinese() != test.chinese || c.English() != test.english {
			t.Errorf("%q (%v): japanese=%v chinese=%v english=%v", test.text, c, c.Japanese(), c.Chinese(), c.English())
		}
	}
}

func TestLanguageRuleThresholds(t *testing.T) {
	for _, test := range []struct {
		name string
		got  bool
		want bool
	}{
		{"kana x4 above kanji", Count("あ漢字字").Japanese(), true},
		{"kana x4 equal to kanji", Count("あ漢字字字").Japanese(), false},
		{"kanji above kana x10", Count("あ一二三四五六七八九十十").Chinese(), true},
		{"kanji equal to kana x10", Count("あ一二三四五六七八九十").Chinese(), false},
		{"kanji above hangul x10", Count("가一二三四五六七八九十十").Chinese(), true},
		{"kanji not above Latin", Count("Leo/need一二三").Chinese(), false},
		{"Latin above the rest", Count("OK, sure、了解").English(), true},
		{"Latin not above the rest", Count("OK、了解です").English(), false},
	} {
		if test.got != test.want {
			t.Errorf("%s = %v, want %v", test.name, test.got, test.want)
		}
	}
	if got := Count("あア가ㄱ漢㐀Ab1！"); got != (Counts{Kana: 2, Hangul: 2, Han: 2, Latin: 2}) || got.String() != "kana 2, hangul 2, kanji 2, latin 2" {
		t.Fatalf("counts = %+v %q", got, got.String())
	}
}

func TestCountScenarioReadsTheBodiesOnly(t *testing.T) {
	scenario := map[string]any{"TalkData": []any{
		map[string]any{"WindowDisplayName": "みのり", "Body": textTW},
		map[string]any{"WindowDisplayName": "遥", "Body": 7},
		"not a talk",
	}}
	if got := CountScenario(scenario); got != Count(textTW) {
		t.Fatalf("counts = %+v, want %+v", got, Count(textTW))
	}
	for _, value := range []any{nil, []any{}, map[string]any{"TalkData": "x"}} {
		if got := CountScenario(value); got != (Counts{}) {
			t.Fatalf("%#v counts = %+v", value, got)
		}
	}
}
