package translator

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
)

func TestParseXMLTranslationsKeepsLineBreaksAndDecodesEntities(t *testing.T) {
	content := "<translations><t id=\"1\">第一行\n第二行 &lt;b&gt; &amp;lt; &quot;引用&quot; &apos;x&apos;&#10;第三行&#x41;</t>" +
		"<t id=\"2\">\n  前后空白\n</t><t id=\"3\">&#0; &bogus; &amp</t></translations>"
	got := parseXMLTranslations(content, 3)
	want := []string{"第一行\n第二行 <b> &lt; \"引用\" 'x'\n第三行A", "前后空白", "&#0; &bogus; &amp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %q, want %q", got, want)
	}
}

func TestLLMBatchesBoundCountAndBytes(t *testing.T) {
	keys := []string{"aaaa", "bbbb", "cc", "dddddddddddd", "e", "f", "g", "h"}
	got := llmBatches(keys, 3, 10)
	want := [][]string{{"aaaa", "bbbb", "cc"}, {"dddddddddddd"}, {"e", "f", "g"}, {"h"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %q, want %q", got, want)
	}
}

var strictXMLText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// fakeXMLTranslator is an OpenAI-compatible endpoint that parses the prompt
// with a real XML decoder and answers every item as "译:"+text, re-escaped as
// strict XML with the line breaks left raw.
type fakeXMLTranslator struct {
	mu       sync.Mutex
	requests [][]string
}

func (f *fakeXMLTranslator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 1 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var items struct {
		Items []struct {
			ID   string `xml:"id,attr"`
			Text string `xml:",chardata"`
		} `xml:"item"`
	}
	input := strings.TrimPrefix(request.Messages[0].Content, gameContextPrompt)
	if err := xml.Unmarshal([]byte("<root>"+input+"</root>"), &items); err != nil {
		http.Error(w, "prompt is not well-formed XML: "+err.Error(), http.StatusBadRequest)
		return
	}
	texts := make([]string, 0, len(items.Items))
	var reply strings.Builder
	reply.WriteString("<translations>\n")
	for _, item := range items.Items {
		texts = append(texts, item.Text)
		reply.WriteString(`<t id="` + item.ID + `">` + strictXMLText.Replace("译:"+item.Text) + "</t>\n")
	}
	reply.WriteString("</translations>")
	f.mu.Lock()
	f.requests = append(f.requests, texts)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": reply.String()}}},
	})
}

func TestManualAITranslateGachaInfoKeepsLongMultilineText(t *testing.T) {
	fake := &fakeXMLTranslator{}
	server := httptest.NewServer(fake)
	defer server.Close()
	tr, _, cfg := openTestTranslator(t)
	for key, value := range map[string]string{
		config.KeyOpenAIAPIKey: "test", config.KeyOpenAIBaseURL: server.URL, config.KeyOpenAIModel: "test-model",
		config.KeyBatchSize: "20", config.KeyRateDelayMS: "0", config.KeyLLMRequestTimeoutMS: "5000", config.KeyLLMMaxRetries: "0",
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	descriptions := []string{
		syntheticGachaDescription(201), syntheticGachaDescription(202), syntheticGachaDescription(203),
		"短い説明 <b>A&B</b>\n二行目\n", "もう一つの短い説明\n\n三行目",
	}
	entries := map[string]model.Entry{}
	for _, description := range descriptions {
		entries[description] = model.Entry{Source: model.SourceUnknown}
	}
	if _, err := tr.store.ImportCategory("gachaInfo", model.Category{"description": entries}); err != nil {
		t.Fatal(err)
	}

	result, err := tr.ManualAITranslate(AITranslateRequest{Category: "gachaInfo", Field: "description", Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidates != len(descriptions) || result.Translated != len(descriptions) {
		t.Fatalf("result = %+v", result)
	}
	stored, err := tr.store.CategoryData("gachaInfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, description := range descriptions {
		want := model.Entry{Text: strings.TrimSpace("译:" + description), Source: model.SourceLLM}
		if got := stored["description"][description]; !reflect.DeepEqual(got, want) {
			t.Fatalf("stored translation for %.40q = %q, want %q", description, got.Text, want.Text)
		}
	}

	var sent []string
	for _, batch := range fake.requests {
		size := 0
		for _, text := range batch {
			size += len(text)
		}
		if len(batch) > 1 && size > maxLLMBatchTextBytes {
			t.Fatalf("request carried %d items / %d bytes, over the %d-byte budget", len(batch), size, maxLLMBatchTextBytes)
		}
		sent = append(sent, batch...)
	}
	if len(fake.requests) < 3 {
		t.Fatalf("%d requests for three several-KB descriptions, want them split", len(fake.requests))
	}
	sortedSent, sortedWant := append([]string(nil), sent...), append([]string(nil), descriptions...)
	sort.Strings(sortedSent)
	sort.Strings(sortedWant)
	if !reflect.DeepEqual(sortedSent, sortedWant) {
		t.Fatal("texts decoded from the prompt differ from the stored Japanese keys")
	}
}
