package lyricssource

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

func assertExactHanOnlyRuby(t *testing.T, text string, spans []RubySpan) {
	t.Helper()
	if !rubySpansValidForText(text, spans) || rubySpansText(spans) != text {
		t.Fatalf("ruby contract failed: text=%q spans=%+v", text, spans)
	}
	for _, span := range spans {
		if span.Reading == "" {
			continue
		}
		if !validGeneratedRubyReading(span.Reading) {
			t.Fatalf("non-kana reading: %+v", span)
		}
		for _, current := range span.Text {
			if !model.LyricsSourceRubyBaseRune(current) {
				t.Fatalf("non-Han or numeric text received ruby: %+v", span)
			}
		}
	}
}

func TestDeterministicRubyExactHanCoverage(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		wantText    string
		wantReading string
	}{
		{name: "standalone ideographic motion stroke", text: "o彡゜MEIKO!", wantText: "彡", wantReading: "さん"},
		{name: "ordinary Han beside kana", text: "歌う", wantText: "歌", wantReading: "うた"},
		{name: "non-Han remains plain", text: "MEIKO! ゜"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spans, err := generateRubySpans(test.text)
			if err != nil {
				t.Fatal(err)
			}
			assertExactHanOnlyRuby(t, test.text, spans)
			for _, span := range spans {
				if span.Text == test.wantText && span.Reading == test.wantReading {
					return
				}
			}
			if test.wantText != "" || test.wantReading != "" {
				t.Fatalf("missing exact reading %q/%q in %+v", test.wantText, test.wantReading, spans)
			}
		})
	}
}

func TestRubyTreatsIdeographicZeroAsPlainNumericText(t *testing.T) {
	tests := []struct {
		name       string
		japanese   string
		romanized  string
		wantSource bool
	}{
		{
			name:       "redacted phrase",
			japanese:   "「本当は〇〇なんでしょ？」",
			romanized:  `"hontou wa 〇〇 nan desho?"`,
			wantSource: true,
		},
		{
			name:       "single redaction marker",
			japanese:   "T氏の言う通り〇しましょう",
			romanized:  "T-shi no iutoori 〇 shimashou",
			wantSource: true,
		},
		{
			name:     "only redaction markers",
			japanese: "〇〇〇〇〇！！",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var spans []RubySpan
			var ok bool
			if test.wantSource {
				spans, ok = deriveSekaipediaRuby(test.japanese, test.romanized)
			} else {
				var err error
				spans, err = generateRubySpans(test.japanese)
				ok = err == nil
			}
			if !ok || !rubySpansValidForText(test.japanese, spans) || rubySpansText(spans) != test.japanese {
				t.Fatalf("numeric redaction ruby failed: ok=%t spans=%+v", ok, spans)
			}
			foundReading := false
			for _, span := range spans {
				if span.Reading != "" {
					foundReading = true
				}
				if span.Reading != "" && containsRune(span.Text, '〇') {
					t.Fatalf("ideographic zero received ruby: %+v", span)
				}
			}
			if test.wantSource && !foundReading {
				t.Fatalf("source alignment lost all non-numeric Han readings: %+v", spans)
			}
		})
	}
}

func containsRune(value string, target rune) bool {
	for _, current := range value {
		if current == target {
			return true
		}
	}
	return false
}

func TestDeterministicRubyUniqueStandaloneDictionaryProbeFailsClosedOnAmbiguity(t *testing.T) {
	if broad, err := generateRubySpans("煌"); err == nil {
		t.Fatalf("ordinary deterministic generation accepted a probe-only Han token: %+v", broad)
	}
	spans, err := generateSekaipediaMismatchedColumnRubySpans("煌")
	if err != nil || !rubySpansValidForText("煌", spans) || len(spans) != 1 ||
		spans[0].ReadingEvidenceKind != model.LyricsSourceReadingEvidenceDeterministicDictionary ||
		spans[0].GeneratorVersion != rubyGeneratorVersion {
		t.Fatalf("bounded unique standalone dictionary probe failed: err=%v spans=%+v", err, spans)
	}
	if ambiguous, accepted := kagomeUniqueStandaloneHanRubySpans("生"); accepted {
		t.Fatalf("ambiguous standalone dictionary probe was accepted: %+v", ambiguous)
	}
	for _, value := range []string{"煌び", "A", ""} {
		if broad, accepted := kagomeUniqueStandaloneHanRubySpans(value); accepted {
			t.Fatalf("non-standalone Han probe %q was accepted: %+v", value, broad)
		}
	}
}

func TestSekaipediaFixedReviewedOrthographyUsesExactLocalTuple(t *testing.T) {
	spans, ok := deriveSekaipediaRuby("呼び続け", "yobi tsuzuke")
	if !ok {
		t.Fatal("fixed reviewed orthography source alignment failed")
	}
	for _, span := range spans {
		if span.Text != "続" {
			continue
		}
		if span.Reading != "つづ" ||
			span.ReadingEvidenceKind != model.LyricsSourceReadingEvidenceFixedReviewedToken ||
			span.GeneratorVersion != rubyGeneratorVersion || span.SourceRowOrdinal != 0 ||
			span.SourceSegmentOrdinal != 0 {
			t.Fatalf("fixed reviewed orthography span=%+v", span)
		}
		return
	}
	t.Fatalf("fixed reviewed orthography span missing: %+v", spans)
}

func TestSekaipediaHybridRubyBoundsLeadingDictionaryRepair(t *testing.T) {
	if converted, ok := romanizeSekaipediaWordToKana("ttzangi"); ok {
		t.Fatalf("malformed source word gained a broad conversion: %q", converted)
	}
	tests := []struct {
		name        string
		japanese    string
		romanized   string
		wantOK      bool
		wantText    string
		wantReading string
	}{
		{
			name:        "repeated leading consonants are bounded by a Kagome-covered prefix",
			japanese:    "残機は疾うに",
			romanized:   "ttzangi wa tou ni",
			wantOK:      true,
			wantText:    "疾",
			wantReading: "と",
		},
		{
			name:     "single leading consonant remains rejected",
			japanese: "残機は疾うに", romanized: "tzangi wa tou ni", wantOK: false,
		},
		{
			name:     "arbitrary leading source word remains rejected",
			japanese: "残機は疾うに", romanized: "qxzangi wa tou ni", wantOK: false,
		},
		{
			name:     "opaque source word cannot stand in for unresolved Han",
			japanese: "疾うに", romanized: "ttzangi tou ni", wantOK: false,
		},
		{
			name:     "opaque source word after the dictionary prefix is rejected",
			japanese: "残機は疾うに", romanized: "zanki wa ttzangi tou ni", wantOK: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spans, ok := deriveSekaipediaRuby(test.japanese, test.romanized)
			if ok != test.wantOK {
				t.Fatalf("derive ok=%t want=%t spans=%+v", ok, test.wantOK, spans)
			}
			if !ok {
				return
			}
			assertExactHanOnlyRuby(t, test.japanese, spans)
			dictionaryCount := 0
			foundSource := false
			for _, span := range spans {
				switch {
				case span.Text == "残" || span.Text == "機":
					if span.ReadingEvidenceKind != model.LyricsSourceReadingEvidenceDeterministicDictionary ||
						span.GeneratorVersion != rubyGeneratorVersion {
						t.Fatalf("Kagome prefix provenance=%+v", span)
					}
					dictionaryCount++
				case span.Text == test.wantText && span.Reading == test.wantReading:
					if span.ReadingEvidenceKind != model.LyricsSourceReadingEvidenceSourceTransliteration ||
						span.GeneratorVersion != "" {
						t.Fatalf("source fallback provenance=%+v", span)
					}
					foundSource = true
				}
			}
			if dictionaryCount != 2 || !foundSource {
				t.Fatalf("mixed dictionary/source provenance missing in %+v", spans)
			}
		})
	}
}

func TestSekaipediaSourceRubyUsesOneExactWordBoundaryPartition(t *testing.T) {
	spans, ok := deriveSekaipediaSourceRuby("一ヶ月二ヶ月", "ikkagetsu nikkagetsu")
	if !ok {
		t.Fatal("exact source partition was rejected")
	}
	assertExactHanOnlyRuby(t, "一ヶ月二ヶ月", spans)
	if got := sekaipediaTestRubyMarkup(spans); got != "{一|いっ}ヶ{月二|げつにっ}ヶ{月|げつ}" {
		t.Fatalf("source partition=%s", got)
	}

	words, ok := deriveSekaipediaSourceWordRuby(
		"一ヶ月二ヶ月", [][]rune{[]rune("いっかげつ"), []rune("にっかげつ")}, sekaipediaSourceAlignmentHints{},
	)
	if !ok {
		t.Fatal("exact source-word partition was rejected")
	}
	assertExactHanOnlyRuby(t, "一ヶ月二ヶ月", words)
	if got := sekaipediaTestRubyMarkup(words); got != "{一|いっ}ヶ{月|げつ}{二|にっ}ヶ{月|げつ}" {
		t.Fatalf("source-word partition=%s", got)
	}

	if ambiguous, accepted := deriveSekaipediaSourceWordRuby(
		"一二三", [][]rune{[]rune("いち"), []rune("に")}, sekaipediaSourceAlignmentHints{},
	); accepted {
		t.Fatalf("ambiguous source-word partition accepted: %+v", ambiguous)
	}
}

func sekaipediaTestRubyMarkup(spans []RubySpan) string {
	var markup string
	for _, span := range spans {
		if span.Reading == "" {
			markup += span.Text
			continue
		}
		markup += "{" + span.Text + "|" + span.Reading + "}"
	}
	return markup
}

func TestSekaipediaSourceReadingPlausibility(t *testing.T) {
	tests := []struct {
		hanCount int
		reading  string
		want     bool
	}{
		{hanCount: 1, reading: "なぐ", want: true},
		{hanCount: 1, reading: "いっ", want: true},
		{hanCount: 3, reading: "きみ", want: true},
		{hanCount: 2, reading: "ふゆう", want: true},
		{hanCount: 1, reading: "っかげつ"},
		{hanCount: 1, reading: "んしか"},
		{hanCount: 1, reading: "ゅう"},
		{hanCount: 1, reading: "ーる"},
		{hanCount: 1, reading: "ッた"},
		{hanCount: 1, reading: "ンだ"},
		{hanCount: 2, reading: "な"},
		{hanCount: 2, reading: "きょ"},
		{hanCount: 1, reading: "ちみもうりょう"},
	}
	for _, test := range tests {
		if got := sekaipediaPlausibleSourceReading(test.hanCount, []rune(test.reading)); got != test.want {
			t.Errorf("plausible(%d Han, %q)=%t want %t", test.hanCount, test.reading, got, test.want)
		}
	}
	if !sekaipediaPlausibleSourceReadingMode(2, []rune("な"), true) ||
		sekaipediaPlausibleSourceReadingMode(2, []rune("っな"), true) {
		t.Fatal("single-mora allowance must still reject a reading that cannot start a word")
	}
}

// 共鳴らして read "narashite" deliberately gives 共鳴 the single mora な. The
// exact alignment keeps it because the visible kana leave no other split, な
// starts a source word, and the word goes on in the okurigana; the rest of the
// line (帷帳 read とばり) stays on the source too. Rows from Sekaipedia
// revisions 337394, 337175, 328002 and 337220.
func TestSekaipediaExactSourceKeepsSingleMoraWordplay(t *testing.T) {
	tests := []struct {
		japanese   string
		romanized  string
		want       string
		dictionary bool
	}{
		{japanese: "共鳴らして", romanized: "narashite", want: "{共鳴|な}らして"},
		{
			japanese: "共鳴らせるさ帷帳の奥　きっと", romanized: "naraserusa tobari no oku kitto",
			want: "{共鳴|な}らせるさ{帷帳|とばり}の{奥|おく}　きっと",
		},
		{japanese: "共鳴らせたなら", romanized: "naraseta nara", want: "{共鳴|な}らせたなら"},
		{
			japanese: "なんでも背負い込んで笑顔で", romanized: "nan demo shoikonde egao de",
			want: "なんでも{背負|しょ}い{込|こ}んで{笑顔|えがお}で",
		},
		// The word goes on in the next base, so せ|かい is no wordplay.
		{
			japanese: "みんなみんな笑っちゃえの正解(世界)は", romanized: "minna minna waracchae no sekai wa",
			want: "みんなみんな{笑|わら}っちゃえの{正解|せいかい}({世界|せかい})は", dictionary: true,
		},
	}
	for _, test := range tests {
		spans, ok := deriveSekaipediaRuby(test.japanese, test.romanized)
		if !ok {
			t.Fatalf("derive failed for %q", test.japanese)
		}
		assertExactHanOnlyRuby(t, test.japanese, spans)
		if got := sekaipediaTestRubyMarkup(spans); got != test.want {
			t.Fatalf("%q ruby=%s want %s", test.japanese, got, test.want)
		}
		if test.dictionary {
			continue
		}
		for _, span := range spans {
			if span.Reading != "" && span.ReadingEvidenceKind != model.LyricsSourceReadingEvidenceSourceTransliteration {
				t.Fatalf("%q span %+v is not source evidence", test.japanese, span)
			}
		}
	}
}

// The lines below are real Sekaipedia rows whose source romanization the old
// alignment accepted with readings shifted onto the wrong Han ({本日|ほ},
// {列島上々|れ}, 25{時|にじゅうごじ}, {君|わら}, ...), plus the review's numeral,
// single-mora and Latin-text cases. Each must align correctly or fail closed
// to the dictionary; every reading must carry the expected evidence.
func TestSekaipediaRubyRejectsMisalignedSourceReadings(t *testing.T) {
	source := model.LyricsSourceReadingEvidenceSourceTransliteration
	dictionary := model.LyricsSourceReadingEvidenceDeterministicDictionary
	tests := []struct {
		name      string
		japanese  string
		romanized string
		want      string
		evidence  model.LyricsSourceReadingEvidenceKind
	}{
		{
			name:     "missing leading source word",
			japanese: "なんて素敵なことでしょう。", romanized: "suteki na koto deshou.",
			want: "なんて{素敵|すてき}なことでしょう。", evidence: dictionary,
		},
		{
			name:     "ambiguous separator inside a Han run",
			japanese: "列島上々 不眠日本", romanized: "rettou joujou fumin nippon",
			want: "{列島|れっとう}{上々|じょうじょう} {不眠|ふみん}{日本|にっぽん}", evidence: dictionary,
		},
		{
			name:     "source word spelled in Latin letters",
			japanese: "ビバップな浮遊感　夜が次第に乱反射して", romanized: "BEBOP na fuyuu kan yoru ga shidai ni ranhansha shite",
			want:     "ビバップな{浮遊|ふゆう}{感|かん}　{夜|よる}が{次第|しだい}に{乱反射|らんはんしゃ}して",
			evidence: dictionary,
		},
		{
			name:     "source particle typo",
			japanese: "本日の宿題は　無個性な僕のこと", romanized: "honjitsu wa sukudai wa mukosei na boku no koto",
			want: "{本日|ほんじつ}の{宿題|しゅくだい}は　{無|む}{個性|こせい}な{僕|ぼく}のこと", evidence: dictionary,
		},
		{
			name:     "source drops the kana between two bases",
			japanese: "君が笑った", romanized: "waratta",
			want: "{君|きみ}が{笑|わら}った", evidence: dictionary,
		},
		{
			name:     "spelled-out number stays on its digits",
			japanese: "眠らない25時", romanized: "nemuranai nijuugoji",
			want: "{眠|ねむ}らない25{時|じ}", evidence: source,
		},
		{
			name:     "full-width digit",
			japanese: "午前６時も鮮やかに", romanized: "gozen rokuji mo azayaka ni",
			want: "{午前|ごぜん}６{時|じ}も{鮮|あざ}やかに", evidence: source,
		},
		{
			name:     "numeral with sound change",
			japanese: "目を覚ました8月14日のベッドの上", romanized: "me o samashita hachigatsu juuyokka no beddo no ue",
			want: "{目|め}を{覚|さ}ました8{月|がつ}14{日|か}のベッドの{上|うえ}", evidence: source,
		},
		{name: "numeral 6 read むい", japanese: "6日", romanized: "muika", want: "6{日|か}", evidence: source},
		{
			name:     "numeral 6 read むい before more Han",
			japanese: "6日目の朝", romanized: "muika me no asa",
			want: "6{日目|かめ}の{朝|あさ}", evidence: source,
		},
		{name: "numeral 10 read とお", japanese: "10日", romanized: "tooka", want: "10{日|か}", evidence: source},
		{name: "numeral 10 before a year", japanese: "10年", romanized: "toonen", want: "10{年|ねん}", evidence: source},
		{
			// The typo leaves む|いか and むい|か equally good; the tie fails closed.
			name:     "numeral split tie in the edit alignment",
			japanese: "6日の朝", romanized: "muika ni asa",
			want: "6{日|にち}の{朝|あさ}", evidence: dictionary,
		},
		{
			name:     "digits the source also spells as digits",
			japanese: "10884 夜でも", romanized: "10884 yoru demo",
			want: "10884 {夜|よる}でも", evidence: source,
		},
		{
			name:     "source omits a visible sokuon",
			japanese: "殴ったって蹴っ飛ばしても砕けない", romanized: "nagutatte kettobashite mo kudakenai",
			want: "{殴|なぐ}ったって{蹴|け}っ{飛|と}ばしても{砕|くだ}けない", evidence: source,
		},
		{
			name:     "okurigana kana that matches twice",
			japanese: "時代柄暗い話題が街行けど、", romanized: "jidaigara kurai wadai ga machi yukedo,",
			want: "{時代柄暗|じだいがらくら}い{話題|わだい}が{街行|まちゆ}けど、", evidence: source,
		},
		{
			name:     "special reading supported by the source",
			japanese: "祈っても　王子様は　まだ来ない (aa)", romanized: "inotte mo kimi wa mada konai (aa)",
			want: "{祈|いの}っても　{王子様|きみ}は　まだ{来|こ}ない (aa)", evidence: source,
		},
		{
			name:     "single mora whose word ends with the base",
			japanese: "今日も", romanized: "kyo mo",
			want: "{今日|きょう}も", evidence: dictionary,
		},
		{
			name:     "single mora followed by a particle",
			japanese: "大人で", romanized: "o de",
			want: "{大人|おとな}で", evidence: dictionary,
		},
		{
			name:     "unmatched Latin text sung as other words",
			japanese: "「殴るよ？15cm切った髪に気づけ」", romanized: `"naguru yo? juugo senchi kitta kami ni kidzuke"`,
			want: "「{殴|なぐ}るよ？15cm{切|き}った{髪|かみ}に{気|き}づけ」", evidence: dictionary,
		},
		{
			name:      "unmatched Latin letters absorb a source word",
			japanese:  "どうしてですか？！ちょっと！？HPすり減らすのやめませんか？！",
			romanized: "doushite desu ka?! chotto!? tairyoku suriherasu no yamemasen ka?!",
			want:      "どうしてですか？！ちょっと！？HPすり{減|へ}らすのやめませんか？！", evidence: source,
		},
		{
			name:     "degree sign read with its number",
			japanese: "君の体温はたった36℃程しかないから", romanized: "kimi no taion wa tatta sanjuurokudo hodoshikanai kara",
			want: "{君|きみ}の{体温|たいおん}はたった36℃{程|ほど}しかないから", evidence: dictionary,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spans, ok := deriveSekaipediaRuby(test.japanese, test.romanized)
			if !ok {
				t.Fatalf("derive failed for %q", test.japanese)
			}
			assertExactHanOnlyRuby(t, test.japanese, spans)
			if got := sekaipediaTestRubyMarkup(spans); got != test.want {
				t.Fatalf("ruby=%s want %s", got, test.want)
			}
			for _, span := range spans {
				if span.Reading != "" && span.ReadingEvidenceKind != test.evidence {
					t.Fatalf("span %+v evidence want %q", span, test.evidence)
				}
			}
		})
	}
}

// The lenient last resort runs only when the dictionary cannot read the line
// either, and only for callers that pass lastResort. It differs from the strict
// alignment only in searching plausible readings alone and in reading a source
// with a word that is not romaji ("oh"). It still rejects any tie that reads a
// base differently, any edit that substitutes a kana, a dropped visible kana
// run, and a letter-name word as a reading, so 彁 (no dictionary reading)
// never reads わ or べぼぴー. 色欲是空　愛憎交々 is a Sekaipedia row (revision
// 326668) whose space falls after either "shikiyoku" or "zekuu"; the rest are
// synthetic.
func TestSekaipediaLenientLastResortStaysAnchored(t *testing.T) {
	const japanese, romanized = "彁が笑ったね", "kimi ga waratta ne oh"
	if _, err := generateRubySpans(japanese); err == nil {
		t.Fatal("the dictionary reads the line; the lenient branch is not exercised")
	}
	if spans, ok := deriveSekaipediaRubyMode(japanese, romanized, false); ok {
		t.Fatalf("lastResort=false used the lenient alignment: %s", sekaipediaTestRubyMarkup(spans))
	}
	spans, ok := deriveSekaipediaRubyMode(japanese, romanized, true)
	if got := sekaipediaTestRubyMarkup(spans); !ok || got != "{彁|きみ}が{笑|わら}ったね" {
		t.Fatalf("lenient last resort=%s ok=%t", got, ok)
	}
	for _, span := range spans {
		if span.Reading != "" && span.ReadingEvidenceKind != model.LyricsSourceReadingEvidenceSourceTransliteration {
			t.Fatalf("span %+v is not a source reading", span)
		}
	}

	for _, test := range [][2]string{
		{"色欲是空　愛憎交々", "shikiyoku zekuu aizou komogomo"},
		{"本日の彁は　無個性な僕のこと", "honjitsu wa sukudai wa mukosei na boku no koto"},
		{"彁が笑った", "waratta"},
		{"彁が笑った", "BEBOP ga waratta"},
		{"列島上々不眠日本", "rettou jouj fumin nipon"},
	} {
		if _, err := generateRubySpans(test[0]); err == nil {
			t.Fatalf("the dictionary reads %q; the lenient branch is not exercised", test[0])
		}
		if spans, ok := deriveSekaipediaRubyMode(test[0], test[1], true); ok {
			t.Errorf("%q %q accepted %s", test[0], test[1], sekaipediaTestRubyMarkup(spans))
		}
	}
	if spans, ok := deriveSekaipediaRuby("彁が笑った", "kimi ga waratta"); !ok ||
		sekaipediaTestRubyMarkup(spans) != "{彁|きみ}が{笑|わら}った" {
		t.Fatalf("anchored source reading for an unread Han=%s ok=%t", sekaipediaTestRubyMarkup(spans), ok)
	}
}

// The hybrid alignment reads dictionary tokens from the dictionary and the
// others from the source, but a source word that is not romaji (BEBOP, the
// truncated "jouj") never becomes such a token's reading ({彁|べぼぴー},
// {々|じぇー}).
func TestSekaipediaHybridRubyRejectsLetterNameReadings(t *testing.T) {
	for _, test := range [][2]string{
		{"彁が笑った", "BEBOP ga waratta"},
		{"列島上々不眠日本", "rettou jouj fumin nipon"},
	} {
		if spans, ok := deriveSekaipediaHybridRuby(test[0], test[1]); ok {
			t.Errorf("hybrid %q %q accepted %s", test[0], test[1], sekaipediaTestRubyMarkup(spans))
		}
	}
	if spans, ok := deriveSekaipediaHybridRuby("彁の宿題は", "ka no sukudai wa"); !ok ||
		sekaipediaTestRubyMarkup(spans) != "{彁|か}の{宿題|しゅくだい}は" {
		t.Fatalf("hybrid with romaji source=%s ok=%t", sekaipediaTestRubyMarkup(spans), ok)
	}
}

// Alignment cost is bounded: a long line returns quickly, and a line beyond
// sekaipediaAlignmentBudget fails closed to the dictionary.
func TestSekaipediaRubyAlignmentTimeIsBounded(t *testing.T) {
	// Load the dictionary first so its one-time setup is not timed.
	if err := initializeFuriganaTokenizer(); err != nil {
		t.Fatal(err)
	}
	type line struct {
		japanese, romanized string
	}
	repeat := func(text string, count int, separator string) string {
		return strings.TrimSpace(strings.Repeat(text+separator, count))
	}
	var tests []line
	for _, test := range [][2]string{
		{"眠らない25時の街で10884回目の夜", "nemuranai nijuugoji no machi de ichiman hassen happyaku hachijuu yonkaime no yoru"},
		{"列島上々不眠日本東京大都会摩天楼群", "rettou jouj fumin nipon toukyo daitokai matenro gun"},
		{"時代柄暗い話題が街行けど、", "jidaigara kurai wadei ga mach yukedo,"},
	} {
		for _, count := range []int{8, 60} {
			tests = append(tests, line{repeat(test[0], count, ""), repeat(test[1], count, " ")})
		}
	}
	// 2101 characters that fail the whole-line alignment and reach the
	// word-by-word one; it took 38s before that pass had a budget.
	for _, tokyo := range []string{"toukyou oosaka", "toukyo oosaka"} {
		tests = append(tests, line{
			repeat("東京大阪", 300, "") + "の" + repeat("名古屋", 300, ""),
			repeat(tokyo, 300, " ") + " no " + repeat("nagoya", 300, " "),
		})
	}
	for _, test := range tests {
		start := time.Now()
		spans, ok := deriveSekaipediaRuby(test.japanese, test.romanized)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("%d characters took %v", utf8.RuneCountInString(test.japanese), elapsed)
		}
		if ok {
			assertExactHanOnlyRuby(t, test.japanese, spans)
		}
	}
	if _, ok := deriveSekaipediaSourceWordRuby(
		repeat("東京大阪", 300, "")+"の"+repeat("名古屋", 300, ""),
		make([][]rune, 601), sekaipediaSourceAlignmentHints{},
	); ok {
		t.Fatal("word alignment beyond the budget did not fail closed")
	}
	long := strings.Repeat("時代柄暗い話題が街行けど、", 80)
	if sekaipediaAlignmentWithinBudget(len([]rune(strings.Repeat("じだいがらくらいわだいがまちゆけど", 80))), 80*7) {
		t.Fatal("an 80-line paragraph is within the alignment budget")
	}
	if _, ok := sekaipediaSourceRubySpans(long, []rune(strings.Repeat("じだいがらくらいわだいがまちゆけど", 80)),
		sekaipediaSourceAlignmentHints{}); ok {
		t.Fatal("source alignment beyond the budget did not fail closed")
	}
}

func TestGeneratedRubyPreservesCallerSegmentation(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
	}{
		{name: "two kaomoji performer segments", texts: []string{"o彡゜MEIKO! ", "（o彡゜MEIKO!）"}},
		{name: "Han and plain segments", texts: []string{"歌う", " MEIKO!"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generated, err := generateRubySpansForTexts(test.texts)
			if err != nil {
				t.Fatal(err)
			}
			if len(generated) != len(test.texts) {
				t.Fatalf("generated segments=%d want=%d", len(generated), len(test.texts))
			}
			for index, text := range test.texts {
				assertExactHanOnlyRuby(t, text, generated[index])
			}
		})
	}
}

// A whole-line derivation reads the line without its {{ruby}} templates, so
// it must keep each explicit reading rather than overwrite it with the
// dictionary or source reading. These rows come from Sekaipedia; the old
// parser emitted {物語|ものがたり} and the romanization's ざつおん for 雑音.
func TestSekaipediaWholeLineRubyKeepsExplicitSegmentReadings(t *testing.T) {
	content := `== Versions ==
{{Song versions head}}
{{Song versions line
| version = SEKAI
| singers = Ichika,Miku
| audio = sample
}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head
| columns = japanese,romaji,english
| japanese = Japanese lyrics
| romaji = Romanized lyrics
| english = English translation
}}
{{Lyrics line
| japanese = 別れが {{ruby|綴|と}}じる {{ruby|物語|ストーリィ}}は　-BAD END－ じゃない
| romaji   = wakare ga tojiru sutooryi wa　-BAD END- janai
| english  = ignored translation
}}
{{Lyrics line
| japanese = {{ruby|雑音|そうおん}}なんて消えてしまえ
| romaji = zatsuon nante kiete shimae
| english = ignored translation
}}
{{Lyrics tail}}
`
	parsed, err := parseSekaipediaSong(content, PerformerSegmentationSekaiEligible)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Full.Lines) != 2 {
		t.Fatalf("explicit ruby extraction=%+v", parsed)
	}
	want := map[string]string{"綴": "と", "物語": "すとおりぃ", "雑音": "そうおん", "別": "わか", "消": "き"}
	found := map[string]bool{}
	for _, line := range parsed.Full.Lines {
		for _, segment := range line.Segments {
			assertExactHanOnlyRuby(t, segment.Text, segment.Ruby)
			for _, span := range segment.Ruby {
				if reading, exists := want[span.Text]; exists {
					if span.Reading != reading {
						t.Fatalf("%s read %q want %q in %s", span.Text, span.Reading, reading,
							sekaipediaTestRubyMarkup(segment.Ruby))
					}
					found[span.Text] = true
				}
			}
		}
	}
	if len(found) != len(want) {
		t.Fatalf("missing explicit readings %v in %+v", want, parsed.Full.Lines)
	}
}

// An explicit {{ruby}} reading that has no plausible per-base split keeps only
// its visible base, so the row derivation reads it, instead of inventing a
// split such as {怒|ら}れる{人|んぐらあ}. A whole-phrase group span
// {怒れる人|らんぐらあ} cannot be emitted: the ruby contract
// (rubySpansValidForText, model.LyricsSourceRubyBaseRune, the store's public v3
// and document validators) only allows Han in a ruby base. A reading that does
// split per base (愛と愛 read め め, と unvoiced) keeps its explicit ruby. The
// fragments are Sekaipedia rows from revisions 329789, 328680 and 337014.
func TestSekaipediaExplicitRubyWithoutPlausibleSplitKeepsOnlyBase(t *testing.T) {
	roster := sekaipediaAllSingerSet()
	tests := []struct {
		singers  string
		value    string
		base     string
		wantRuby string
	}{
		{
			singers: "Kanade, Ena, KAITO", value: "痛みと{{ruby|怒れる人|{{Lyric|Kanade, Ena|ラングラー}}}}を喰らったったらった",
			base: "怒れる人",
		},
		{
			singers: "Tsukasa, Emu, KAITO", value: "{{Ruby|犯人|{{Lyric|Tsukasa, Emu|答え}}}}教えて",
			base: "犯人",
		},
		{
			singers: "Honami", value: "{{ruby|愛と愛|{{Lyric|Honami|め め}}}}が苦手です隠れたいよ",
			base: "愛と愛", wantRuby: "{愛|め}と{愛|め}",
		},
	}
	for _, test := range tests {
		performers, err := resolveSekaipediaSingerList(test.singers, roster, true)
		if err != nil {
			t.Fatal(err)
		}
		events, err := parseSekaipediaInlineLyricEvents(test.value, performers, roster, 0)
		if err != nil {
			t.Fatalf("%q: %v", test.value, err)
		}
		found := false
		for _, event := range events {
			if !strings.Contains(event.text, test.base) {
				continue
			}
			found = true
			got := sekaipediaTestRubyMarkup(event.ruby)
			if test.wantRuby == "" && len(event.ruby) != 0 || test.wantRuby != "" && !strings.Contains(got, test.wantRuby) {
				t.Fatalf("%q explicit ruby=%s want %q", test.base, got, test.wantRuby)
			}
		}
		if !found {
			t.Fatalf("%q base event missing: %+v", test.base, events)
		}
	}
}

// An explicit {{ruby}} is the page's own reading, so a deliberate single mora
// (共鳴 read な, revision 337394) is kept even nested in {{Lyric}}. When the
// base spans nested performer segments and the reading does not split at
// their edge, the bases stay without ruby rather than failing the table.
func TestSekaipediaExplicitRubyKeepsSingleMoraAndSegmentSplitFallback(t *testing.T) {
	roster := sekaipediaAllSingerSet()
	performers, err := resolveSekaipediaSingerList("Ichika, Miku", roster, true)
	if err != nil {
		t.Fatal(err)
	}
	events, err := parseSekaipediaInlineLyricEvents(
		"{{ruby|共鳴|{{Lyric|Ichika|な}}}}らして", performers, roster, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, event := range events {
		got += sekaipediaTestRubyMarkup(event.ruby)
	}
	if !strings.Contains(got, "{共鳴|な}") {
		t.Fatalf("nested explicit single mora=%s events=%+v", got, events)
	}

	events, err = parseSekaipediaInlineLyricEvents(
		"{{ruby|{{Lyric|Ichika|一番}}{{Lyric|Miku|星}}|いちばんぼし}}", performers, roster, 0,
	)
	if err != nil {
		t.Fatalf("segment split failure: %v", err)
	}
	for _, event := range events {
		if containsKanji(event.text) && len(event.ruby) != 0 {
			t.Fatalf("event %q kept a reading that crosses segments: %s", event.text, sekaipediaTestRubyMarkup(event.ruby))
		}
	}
}

// A line that gets no trusted ruby fails the song, so the error must say which
// line and why, while errors.Is still matches ErrUnsupportedTable. Like other
// source diagnostics, Error() carries no lyric text: the line text stays in
// the *sekaipediaLineError for errors.As. Under the disabled policy the failure
// is still a catalog rendition conflict, with the line named in its text. The
// tied row is from Sekaipedia revision 326668; 彁 is synthetic.
func TestSekaipediaLineRubyErrorNamesLineAndReason(t *testing.T) {
	song := func(version, singers string, lines ...[2]string) string {
		var body strings.Builder
		for _, line := range lines {
			body.WriteString("{{Lyrics line\n| japanese = " + line[0] + "\n| romaji = " + line[1] +
				"\n| english = ignored translation\n}}\n")
		}
		return `== Versions ==
{{Song versions head}}
{{Song versions line
| version = ` + version + `
| singers = ` + singers + `
| audio = sample
}}
{{Song versions tail}}
== Lyrics ==
{{Lyrics head
| columns = japanese,romaji,english
| japanese = Japanese lyrics
| romaji = Romanized lyrics
| english = English translation
}}
` + body.String() + "{{Lyrics tail}}\n"
	}
	tests := []struct {
		lines  [][2]string
		line   int
		failed string
		want   []string
	}{
		{
			lines: [][2]string{{"君が笑った", "kimi ga waratta"}, {"彁が笑った", "waratta"}},
			line:  2, failed: "彁が笑った",
			want: []string{"line 2: ", "the source drops visible kana", "the dictionary cannot read 彁"},
		},
		{
			lines: [][2]string{{"色欲是空　愛憎交々", "shikiyoku zekuu aizou komogomo"}},
			line:  1, failed: "色欲是空　愛憎交々",
			want: []string{"line 1: ", "tied source alignments", "the dictionary cannot read 交, 々"},
		},
	}
	for _, test := range tests {
		_, err := parseSekaipediaSong(song("SEKAI", "Ichika,Miku", test.lines...), PerformerSegmentationSekaiEligible)
		if !errors.Is(err, ErrUnsupportedTable) {
			t.Fatalf("%v: err=%v want ErrUnsupportedTable", test.lines, err)
		}
		var lineErr *sekaipediaLineError
		if !errors.As(err, &lineErr) || lineErr.line != test.line || lineErr.text != test.failed {
			t.Fatalf("%v: err=%v is not a line error for line %d %q", test.lines, err, test.line, test.failed)
		}
		for _, want := range test.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
		for _, line := range test.lines {
			if strings.Contains(err.Error(), line[0]) || strings.Contains(err.Error(), strconv.Quote(line[0])) {
				t.Errorf("error %q leaks the lyric line %q", err, line[0])
			}
		}

		_, err = parseSekaipediaSong(song("VIRTUAL SINGER", "Hatsune Miku", test.lines...), PerformerSegmentationDisabled)
		if !errors.Is(err, ErrCatalogRenditionConflict) || errors.Is(err, ErrUnsupportedTable) {
			t.Fatalf("%v disabled: err=%v want only ErrCatalogRenditionConflict", test.lines, err)
		}
		if !strings.Contains(err.Error(), test.want[0]) || strings.Contains(err.Error(), test.failed) {
			t.Errorf("disabled error %q lacks %q or leaks the line", err, test.want[0])
		}
	}
}
