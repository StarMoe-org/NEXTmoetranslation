package translator

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"moesekai/server/internal/config"
)

// A call that used up its attempts wraps ErrLLMFailed, one made without the
// API key also wraps ErrLLMKeyMissing, and the messages read as before.
func TestCallLLMWrapsTypedErrors(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "test rate limit", http.StatusTooManyRequests)
	}))
	defer llm.Close()
	tr, _, cfg := openTestTranslator(t)
	for key, value := range map[string]string{config.KeyOpenAIBaseURL: llm.URL, config.KeyLLMMaxRetries: "0"} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	check := func(provider string, keyMissing bool, message string) {
		t.Helper()
		_, err := tr.callLLM(provider, []string{"テスト"})
		if err == nil || !errors.Is(err, ErrLLMFailed) || errors.Is(err, ErrLLMKeyMissing) != keyMissing || err.Error() != message {
			t.Fatalf("%s error = %v (llm failed %t, key missing %t), want %q", provider, err,
				errors.Is(err, ErrLLMFailed), errors.Is(err, ErrLLMKeyMissing), message)
		}
	}
	check("openai", true, "llm failed after 1 attempts (provider=openai, texts=1): OPENAI_API_KEY is not configured")
	check("gemini", true, "llm failed after 1 attempts (provider=gemini, texts=1): GEMINI_API_KEY is not configured")
	if err := cfg.Set(config.KeyOpenAIAPIKey, "test-key"); err != nil {
		t.Fatal(err)
	}
	check("openai", false, "llm failed after 1 attempts (provider=openai, texts=1): openai http 429: test rate limit")
}
