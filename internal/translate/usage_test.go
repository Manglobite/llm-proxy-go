package translate

import (
	"encoding/json"
	"testing"
)

// TR-3: построчная сверка полей таблицы SPEC 8.6 для всех трёх форм апстримов.
// Отдельно ловушка из REVIEW: cache_write_tokens — это СОЗДАНИЕ кеша, а не
// попадание; перепутать — кеш-статистика дашборда врёт.
func TestUsageNormalizationFromThreeFormats(t *testing.T) {
	t.Run("codex responses", func(t *testing.T) {
		var u responsesUsage
		if err := json.Unmarshal([]byte(`{
			"input_tokens": 120,
			"input_tokens_details": {"cached_tokens": 80, "cache_write_tokens": 5},
			"output_tokens": 30,
			"output_tokens_details": {"reasoning_tokens": 12}
		}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		want := Usage{
			PromptTokens: 120, CompletionTokens: 30,
			CachedInputTokens: 80, CacheCreationTokens: 5, ReasoningTokens: 12,
		}
		if got := usageFromResponses(u); got != want {
			t.Fatalf("usage %+v, ожидалось %+v", got, want)
		}
	})
	t.Run("codex responses без деталей", func(t *testing.T) {
		var u responsesUsage
		if err := json.Unmarshal([]byte(`{"input_tokens": 10, "output_tokens": 3}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := usageFromResponses(u); got != (Usage{PromptTokens: 10, CompletionTokens: 3}) {
			t.Fatalf("usage %+v", got)
		}
	})
	t.Run("glm openai chat", func(t *testing.T) {
		var u ChatUsage
		if err := json.Unmarshal([]byte(`{
			"prompt_tokens": 64,
			"completion_tokens": 18,
			"prompt_tokens_details": {"cached_tokens": 40},
			"completion_tokens_details": {"reasoning_tokens": 7}
		}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		want := Usage{PromptTokens: 64, CompletionTokens: 18, CachedInputTokens: 40, ReasoningTokens: 7}
		if got := usageFromChat(&u); got != want {
			t.Fatalf("usage %+v, ожидалось %+v", got, want)
		}
	})
	t.Run("openai chat без деталей", func(t *testing.T) {
		var u ChatUsage
		if err := json.Unmarshal([]byte(`{"prompt_tokens": 5, "completion_tokens": 2}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := usageFromChat(&u); got != (Usage{PromptTokens: 5, CompletionTokens: 2}) {
			t.Fatalf("usage %+v", got)
		}
		if got := usageFromChat(nil); got != (Usage{}) {
			t.Fatalf("usageFromChat(nil) должен быть нулевым: %+v", got)
		}
	})
	t.Run("glm anthropic", func(t *testing.T) {
		var u AnthropicUsage
		if err := json.Unmarshal([]byte(`{
			"input_tokens": 200,
			"output_tokens": 45,
			"cache_read_input_tokens": 150,
			"cache_creation_input_tokens": 20
		}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		want := Usage{PromptTokens: 200, CompletionTokens: 45, CachedInputTokens: 150, CacheCreationTokens: 20}
		if got := usageFromAnthropic(&u); got != want {
			t.Fatalf("usage %+v, ожидалось %+v", got, want)
		}
	})
	t.Run("anthropic nil", func(t *testing.T) {
		if got := usageFromAnthropic(nil); got != (Usage{}) {
			t.Fatalf("usageFromAnthropic(nil) должен быть нулевым: %+v", got)
		}
	})
}
