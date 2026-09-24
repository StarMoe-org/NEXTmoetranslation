package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/translator"
)

// The real translator's provider failures answer 502 upstream_unavailable; a
// missing API key and a cancelled run stay 500 internal_error.
func TestSideStoryAIMapsLLMFailuresToUpstreamUnavailable(t *testing.T) {
	h := setupSideStoryAPI(t)
	var mu sync.Mutex
	var replies []int // statuses for the next requests; afterwards 200 without translations
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		status := http.StatusOK
		content := "<translations></translations>"
		if len(replies) > 0 {
			status, replies = replies[0], replies[1:]
			content = `<translations><t id=\"1\">测试译文</t></translations>`
		}
		mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "test rate limit", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+content+`"}}]}`)
	}))
	t.Cleanup(llm.Close)
	for key, value := range map[string]string{
		config.KeyOpenAIBaseURL: llm.URL, config.KeyBatchSize: "1", config.KeyRateDelayMS: "0", config.KeyLLMMaxRetries: "0",
	} {
		if err := h.api.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	h.api.SetSideStoryRunner(translator.NewSideStoryBackfill(h.api.translator, translator.SideStoryBackfillOptions{}))
	path, body := "/api/editor/v1/story/card/501/ai", map[string]any{"episode": "1", "provider": "openai"}
	details := func(failed sideStoryAPIError) string { return strings.Join(failed.Details, "\n") }

	failed := h.expectError(t, http.MethodPost, path, h.token, body, http.StatusInternalServerError, "internal_error")
	if !strings.Contains(details(failed), "OPENAI_API_KEY is not configured") {
		t.Fatalf("missing key details = %q", failed.Details)
	}

	if err := h.api.cfg.Set(config.KeyOpenAIAPIKey, "test-key"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	replies = []int{http.StatusOK, http.StatusTooManyRequests}
	mu.Unlock()
	failed = h.expectError(t, http.MethodPost, path, h.token, body, http.StatusBadGateway, "upstream_unavailable")
	if got := details(failed); !strings.Contains(got, "openai http 429") || !strings.Contains(got, "translated lines saved before the failure: 1") {
		t.Fatalf("rate-limited details = %q", failed.Details)
	}
	if rebuilds := h.files.taken(); !reflect.DeepEqual(rebuilds, []string{"card/501"}) {
		t.Fatalf("rate-limited rebuilds = %v", rebuilds)
	}

	failed = h.expectError(t, http.MethodPost, path, h.token, body, http.StatusBadGateway, "upstream_unavailable")
	if got := details(failed); !strings.Contains(got, "parse incomplete") || !strings.Contains(got, "translated lines saved before the failure: 0") {
		t.Fatalf("unparsable details = %q", failed.Details)
	}

	runner := &fakeSideStoryRunner{aiErr: fmt.Errorf("card 501 batch 1/1 failed after saving 0/1: llm failed after 1 attempts (provider=openai, texts=1): %w", context.Canceled)}
	h.api.SetSideStoryRunner(runner)
	h.expectError(t, http.MethodPost, path, h.token, body, http.StatusInternalServerError, "internal_error")
}
