package translator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
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

func TestExtractGachaInfoRegistersExactJPKeysWithoutCN(t *testing.T) {
	description := syntheticGachaDescription(101)
	if len(description) < 4<<10 {
		t.Fatalf("fixture description is %d bytes, want a several-KB text", len(description))
	}
	summary := "期間限定メンバーが登場！\n対象メンバーの出現確率アップ！"
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
	// The CN server's own announcement for the same gacha id is not a translation.
	cn := gachaFixture(t, []map[string]any{
		{"id": 101, "name": "测试扭蛋A", "gachaInformation": map[string]any{
			"summary": "测试译文一", "bubbleText": "测试译文二", "description": "测试译文三\n【开放时间】\n……",
		}},
	})
	var cnFetched atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jp-master/gachas.json":
			fmt.Fprint(w, jp)
		case "/cn-master/gachas.json":
			cnFetched.Store(true)
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
		"summary":     {summary: "", jpOnlySummary: ""},
		"bubbleText":  {bubble: ""},
		"description": {description: ""},
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
	if cnFetched.Load() {
		t.Fatal("extractGachaInfo fetched the CN gachas.json")
	}

	if _, err := tr.store.ImportCategory("gachaInfo", model.Category{
		"summary":     {summary: {Text: "测试译文四", Source: model.SourceLLM, Ids: []string{"101"}}},
		"bubbleText":  {bubble: {Text: "测试译文五", Source: model.SourceHuman}},
		"description": {description: {Text: "测试译文六", Source: model.SourcePinned}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.store.ApplyCNCategory("gachaInfo", fields); err != nil {
		t.Fatal(err)
	}
	stored, err := tr.store.CategoryData("gachaInfo")
	if err != nil {
		t.Fatal(err)
	}
	wantStored := model.Category{
		"summary": {
			summary:       {Text: "测试译文四", Source: model.SourceLLM, Ids: []string{"101", "102"}},
			jpOnlySummary: {Text: "", Source: model.SourceUnknown, Ids: []string{"104"}},
		},
		"bubbleText":  {bubble: {Text: "测试译文五", Source: model.SourceHuman, Ids: []string{"101"}}},
		"description": {description: {Text: "测试译文六", Source: model.SourcePinned, Ids: []string{"101"}}},
	}
	if !reflect.DeepEqual(stored, wantStored) {
		t.Fatalf("stored gachaInfo = %+v, want %+v", stored, wantStored)
	}
	candidates, _, err := tr.store.AICandidates("gachaInfo", "summary", 0)
	if err != nil || !reflect.DeepEqual(candidates, []string{jpOnlySummary}) {
		t.Fatalf("AI candidates = %q err=%v, want only the new key", candidates, err)
	}
	gacha, err := tr.store.CategoryData("gacha")
	if err != nil || len(gacha) != 0 {
		t.Fatalf("gachaInfo sync touched gacha: %+v err=%v", gacha, err)
	}
}

func TestExtractGachaKeepsCNNameSharedWithLaterJPOnlyGacha(t *testing.T) {
	jp := gachaFixture(t, []map[string]any{
		{"id": 101, "name": "テストガチャA"},
		{"id": 102, "name": "テストガチャB"},
		{"id": 900, "name": "テストガチャA"},
		{"id": 901, "name": "テストガチャB"},
	})
	cn := gachaFixture(t, []map[string]any{
		{"id": 101, "name": "测试扭蛋A"},
		{"id": 102, "name": "测试扭蛋B"},
		{"id": 901, "name": "テストガチャB"},
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

	fields, err := tr.extractGacha()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"テストガチャA": "测试扭蛋A", "テストガチャB": "测试扭蛋B"}
	if got := fields["name"].Pairs; !reflect.DeepEqual(got, want) {
		t.Fatalf("name pairs = %q, want %q", got, want)
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
