package translator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

// syntheticGachaDescription mirrors the layout of a real
// gachaInformation.description (period, pickup list, details, notes) with
// synthetic wording. Real descriptions run to several KB.
func syntheticGachaDescription(id int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "期間限定ガチャ「テスト用ガチャ%d」開催！\n\n", id)
	b.WriteString("【開催期間】\n20XX/01/01 15:00 ～ 20XX/01/09 14:59\n\n【ピックアップメンバー】\n")
	for member := 1; member <= 12; member++ {
		fmt.Fprintf(&b, "★4 テスト用メンバー%02d「合成テキストの見出し%02d」\n", member, member)
	}
	b.WriteString("\n【ガチャ詳細】\n")
	b.WriteString("・ピックアップメンバーの出現確率がアップしています。\n")
	b.WriteString("・10回ガチャでは★3以上のメンバーが1人以上確定で出現します。\n")
	b.WriteString("・ガチャを引くと「テスト用ポイント」を獲得でき、交換所でメンバーと交換できます。\n\n【注意事項】\n")
	for note := 1; note <= 24; note++ {
		fmt.Fprintf(&b, "※合成された注意事項その%02dです。期間終了後は内容が変更される場合があります。\n", note)
	}
	b.WriteString("※表示確率は小数点以下を四捨五入 & 端数 < 0.001% を含みます。\n")
	b.WriteString("※\"期間限定\"表記のメンバーは、期間終了後に入手できなくなります。\n")
	return b.String()
}

func gachaFixture(t *testing.T, gachas []map[string]any) string {
	t.Helper()
	body, err := json.Marshal(gachas)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestExtractGachaInfoPairsByIDWithExactKeys(t *testing.T) {
	description := syntheticGachaDescription(101)
	if len(description) < 4<<10 {
		t.Fatalf("fixture description is %d bytes, want a several-KB text", len(description))
	}
	cnDescription := "限定扭蛋「测试用扭蛋101」开启！\n\n【开放时间】\n……"
	summary := "期間限定メンバーが登場！\n対象メンバーの出現確率アップ！"
	cnSummary := "限定成员登场！\n对象成员出现概率提升！"
	bubble := "限定メンバー\n登場中！"
	jpOnlySummary := "  前後に空白がある概要\n"

	jp := gachaFixture(t, []map[string]any{
		{"id": 101, "name": "テストガチャA", "gachaInformation": map[string]any{
			"gachaId": 101, "summary": summary, "bubbleText": bubble, "description": description,
		}},
		{"id": 102, "name": "テストガチャB", "gachaInformation": map[string]any{
			"gachaId": 102, "summary": summary, "bubbleText": "", "description": " \n",
		}},
		{"id": 103, "name": "テストガチャC"},
		{"id": 104, "name": "テストガチャD", "gachaInformation": map[string]any{
			"gachaId": 104, "summary": jpOnlySummary,
		}},
	})
	cn := gachaFixture(t, []map[string]any{
		{"id": 102, "name": "测试扭蛋B", "gachaInformation": map[string]any{"summary": cnSummary}},
		{"id": 101, "name": "测试扭蛋A", "gachaInformation": map[string]any{
			"summary": cnSummary, "bubbleText": bubble, "description": cnDescription + "\n",
		}},
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jp-master/gachas.json":
			fmt.Fprint(w, jp)
		case "/cn-master/gachas.json":
			fmt.Fprint(w, cn)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	tr, _, cfg := openTestTranslator(t)
	configureLocalSources(t, cfg, upstream.URL)

	fields, err := tr.extractGachaInfo()
	if err != nil {
		t.Fatal(err)
	}
	wantPairs := map[string]map[string]string{
		"summary":     {summary: cnSummary, jpOnlySummary: ""},
		"bubbleText":  {bubble: ""},
		"description": {description: cnDescription},
	}
	wantTrace := map[string]map[string][]string{
		"summary":     {summary: {"101", "102"}, jpOnlySummary: {"104"}},
		"bubbleText":  {bubble: {"101"}},
		"description": {description: {"101"}},
	}
	if len(fields) != len(wantPairs) {
		t.Fatalf("fields = %v, want summary/bubbleText/description", reflect.ValueOf(fields).MapKeys())
	}
	for field, want := range wantPairs {
		if got := fields[field].Pairs; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s pairs = %q, want %q", field, got, want)
		}
		if got := fields[field].Trace; !reflect.DeepEqual(got, wantTrace[field]) {
			t.Fatalf("%s trace = %q, want %q", field, got, wantTrace[field])
		}
	}

	if _, err := tr.store.ApplyCNCategory("gachaInfo", fields); err != nil {
		t.Fatal(err)
	}
	stored, err := tr.store.CategoryData("gachaInfo")
	if err != nil {
		t.Fatal(err)
	}
	wantDescription := model.Entry{Text: cnDescription, Source: model.SourceCN, Ids: []string{"101"}}
	if got := stored["description"][description]; !reflect.DeepEqual(got, wantDescription) {
		t.Fatalf("stored description = %+v, want %+v", got, wantDescription)
	}
	wantPending := model.Entry{Text: "", Source: model.SourceUnknown, Ids: []string{"104"}}
	if got := stored["summary"][jpOnlySummary]; !reflect.DeepEqual(got, wantPending) {
		t.Fatalf("stored untranslated summary = %+v, want %+v", got, wantPending)
	}
	gacha, err := tr.store.CategoryData("gacha")
	if err != nil || len(gacha) != 0 {
		t.Fatalf("gachaInfo sync touched gacha: %+v err=%v", gacha, err)
	}
}

func TestCNSyncStepsCoverEverySupportedCategory(t *testing.T) {
	tr, _, _ := openTestTranslator(t)
	var categories []string
	for _, step := range tr.cnSyncSteps() {
		categories = append(categories, step.category)
	}
	seen := map[string]bool{}
	for _, category := range categories {
		if !model.IsValidCategory(category) || seen[category] {
			t.Fatalf("CN sync step %q is unsupported or duplicated: %v", category, categories)
		}
		seen[category] = true
	}
	for _, category := range model.SupportedCategories {
		if !seen[category] {
			t.Fatalf("supported category %q has no CN sync step: %v", category, categories)
		}
	}
}
