package translator

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// pinnedChinesePrompt is the zh-CN instruction text every existing caller has
// always sent; changing it changes every stored AI translation's provenance.
const pinnedChinesePrompt = "你是一个专业的游戏翻译器，专门翻译《世界计划 彩色舞台 feat. 初音未来》(Project SEKAI) 游戏内容。\n" +
	"请将以下XML格式的日文文本翻译成简体中文。\n" +
	"请只返回<translations>...</translations>，每条使用 <t id=\"N\">文本</t>。\n" +
	"每条原文中的换行须在译文对应位置原样保留。\n"

var testLLMItem = regexp.MustCompile(`(?s)<item id="(\d+)">(.*?)</item>`)

// recordingLLM is an OpenAI-compatible endpoint that records each prompt and
// answers every item as "测试译文:" + the item text.
type recordingLLM struct {
	mu      sync.Mutex
	prompts []string
}

func (f *recordingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 1 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	prompt := request.Messages[0].Content
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.mu.Unlock()
	var reply strings.Builder
	reply.WriteString("<translations>")
	for _, item := range testLLMItem.FindAllStringSubmatch(prompt, -1) {
		reply.WriteString(`<t id="` + item[1] + `">测试译文:` + item[2] + `</t>`)
	}
	reply.WriteString("</translations>")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": reply.String()}}},
	})
}

func (f *recordingLLM) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

func configureRecordingLLM(t *testing.T, cfg *config.Config, batchSize string) *recordingLLM {
	t.Helper()
	fake := &recordingLLM{}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	if _, err := cfg.SetMany(map[string]string{
		config.KeyOpenAIAPIKey: "test", config.KeyOpenAIBaseURL: server.URL, config.KeyOpenAIModel: "test-model",
		config.KeyBatchSize: batchSize, config.KeyRateDelayMS: "0", config.KeyLLMRequestTimeoutMS: "5000", config.KeyLLMMaxRetries: "0",
	}); err != nil {
		t.Fatal(err)
	}
	return fake
}

func TestExistingLLMCallersKeepTheChinesePromptByteForByte(t *testing.T) {
	if gameContextPrompt != pinnedChinesePrompt {
		t.Fatalf("gameContextPrompt changed:\n%q", gameContextPrompt)
	}
	if llmPromptForLocale(model.LocaleChinese) != pinnedChinesePrompt || llmPromptForLocale("") != pinnedChinesePrompt {
		t.Fatal("zh-CN no longer uses the pinned prompt")
	}
	tr, _, cfg := openTestTranslator(t)
	fake := configureRecordingLLM(t, cfg, "20")
	texts := []string{"テスト文一", "テスト文二\n二行目"}
	if _, err := tr.callLLM("openai", texts); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.callAutomaticLLM("openai", texts[:1], nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.translateBatch("openai", texts[1:]); err != nil {
		t.Fatal(err)
	}
	want := []string{
		pinnedChinesePrompt + "<item id=\"1\">テスト文一</item>\n<item id=\"2\">テスト文二\n二行目</item>\n",
		pinnedChinesePrompt + "<item id=\"1\">テスト文一</item>\n",
		pinnedChinesePrompt + "<item id=\"1\">テスト文二\n二行目</item>\n",
	}
	if got := fake.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts = %q, want %q", got, want)
	}
}

func TestSideStoryAIRunsOnlyOnDemandWithTheLocalePrompt(t *testing.T) {
	h := newSideStoryHarness(t)
	fake := configureRecordingLLM(t, h.cfg, "2")
	h.upstream.remove(testCNAreaPath)
	h.upstream.remove(testENAreaPath)
	h.round(t)
	h.round(t)
	if _, err := h.tr.RefreshSideStoryContext(t.Context(), store.SideStoryKindArea, testAreaScenario); err != nil {
		t.Fatal(err)
	}
	if calls := len(fake.recorded()); calls != 0 {
		t.Fatalf("backfill and refresh made %d LLM calls", calls)
	}

	for _, test := range []struct {
		locale, prompt, other string
	}{
		{model.LocaleChinese, pinnedChinesePrompt, gameContextPromptEnglish},
		{model.LocaleEnglish, gameContextPromptEnglish, pinnedChinesePrompt},
	} {
		before := len(fake.recorded())
		result, err := h.tr.AITranslateSideStoryContext(t.Context(), store.SideStoryKindArea, testAreaScenario, test.locale, "", "openai")
		if err != nil {
			t.Fatal(err)
		}
		if result != (store.SideStoryAIResult{Translated: 3, Remaining: 0}) || h.tr.Status().LastMode != "ai-side-story" {
			t.Fatalf("%s result = %+v, status %+v", test.locale, result, h.tr.Status())
		}
		prompts := fake.recorded()[before:]
		want := []string{
			test.prompt + "<item id=\"1\">" + testJPLine1 + "</item>\n<item id=\"2\">" + testJPSpeaker + "</item>\n",
			test.prompt + "<item id=\"1\">" + testJPLine2 + "</item>\n",
		}
		if !reflect.DeepEqual(prompts, want) {
			t.Fatalf("%s prompts = %q, want %q", test.locale, prompts, want)
		}
		if strings.Contains(strings.Join(prompts, ""), test.other) {
			t.Fatalf("%s prompt carries the other locale's instructions", test.locale)
		}
		if got := lineTexts(h.detail(t, store.SideStoryKindArea, testAreaScenario, test.locale), "1"); !reflect.DeepEqual(got, map[string]string{
			testJPLine1: "llm:测试译文:" + testJPLine1, testJPSpeaker: "llm:测试译文:" + testJPSpeaker, testJPLine2: "llm:测试译文:" + testJPLine2,
		}) {
			t.Fatalf("%s lines = %v", test.locale, got)
		}
	}
	if result, err := h.tr.AITranslateSideStoryContext(t.Context(), store.SideStoryKindArea, testAreaScenario, model.LocaleChinese, "", ""); err != nil ||
		result != (store.SideStoryAIResult{}) {
		t.Fatalf("second AI run = %+v, %v", result, err)
	}
	if _, err := h.tr.AITranslateSideStoryContext(t.Context(), store.SideStoryKindArea, testAreaScenario, model.LocaleJapanese, "", ""); !errors.Is(err, store.ErrSideStoryInvalid) {
		t.Fatalf("ja-JP AI err = %v", err)
	}
	h.round(t)
	if calls := len(fake.recorded()); calls != 4 {
		t.Fatalf("%d LLM calls after another round, want the 4 on-demand batches", calls)
	}
}
