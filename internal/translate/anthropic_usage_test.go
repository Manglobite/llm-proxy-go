// Юнит-тесты tee-коллектора anthropic-SSE (SPEC 8.6, строка «glm anthropic»):
// кеш-поля cache_read/cache_creation обязательны — это главный радиатор
// дашборда кеш-хит рейта для glm.
package translate

import "testing"

// Полный стрим: message_start (input + оба кеш-поля) → message_delta (output).
func TestAnthropicStreamUsageFullStream(t *testing.T) {
	c := &AnthropicStreamUsage{}
	c.Feed("message_start", []byte(`{
		"type":"message_start",
		"message":{"id":"msg_1","role":"assistant",
			"usage":{"input_tokens":25,"output_tokens":1,
				"cache_read_input_tokens":80,"cache_creation_input_tokens":7}}
	}`))
	c.Feed("content_block_delta", []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`))
	c.Feed("message_delta", []byte(`{
		"type":"message_delta",
		"delta":{"stop_reason":"end_turn"},
		"usage":{"output_tokens":42}
	}`))

	want := Usage{
		PromptTokens: 25, CompletionTokens: 42,
		CachedInputTokens: 80, CacheCreationTokens: 7,
	}
	if got := c.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// Событие без event:-строки: тип берётся из поля type внутри data.
func TestAnthropicStreamUsageEventNameFromData(t *testing.T) {
	c := &AnthropicStreamUsage{}
	c.Feed("", []byte(`{"type":"message_start","message":{"usage":{"input_tokens":9,"cache_read_input_tokens":3}}}`))
	c.Feed("", []byte(`{"type":"message_delta","usage":{"output_tokens":4}}`))
	want := Usage{PromptTokens: 9, CompletionTokens: 4, CachedInputTokens: 3}
	if got := c.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// Битый JSON и посторонние события игнорируются: tee не ломает passthrough.
func TestAnthropicStreamUsageIgnoresGarbage(t *testing.T) {
	c := &AnthropicStreamUsage{}
	c.Feed("message_start", []byte(`{"message":{"usage":`))
	c.Feed("ping", []byte(`{"type":"ping"}`))
	c.Feed("message_start", []byte(`{"type":"message_start","message":{"usage":{"input_tokens":11,"cache_creation_input_tokens":2}}}`))
	c.Feed("message_delta", []byte(`not json at all`))
	c.Feed("message_delta", []byte(`{"type":"message_delta","usage":{"output_tokens":5}}`))
	want := Usage{PromptTokens: 11, CompletionTokens: 5, CacheCreationTokens: 2}
	if got := c.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// message_start с usage без кеш-полей (апстрим их не прислал) — нули, не паника.
func TestAnthropicStreamUsageNoCacheFields(t *testing.T) {
	c := &AnthropicStreamUsage{}
	c.Feed("message_start", []byte(`{"type":"message_start","message":{"usage":{"input_tokens":13,"output_tokens":1}}}`))
	c.Feed("message_delta", []byte(`{"type":"message_delta","usage":{"output_tokens":6}}`))
	want := Usage{PromptTokens: 13, CompletionTokens: 6}
	if got := c.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// Только message_delta (start потерялся): входная сторона из delta, не нули.
func TestAnthropicStreamUsageDeltaOnly(t *testing.T) {
	c := &AnthropicStreamUsage{}
	c.Feed("message_delta", []byte(`{"type":"message_delta","usage":{"input_tokens":20,"output_tokens":8,"cache_read_input_tokens":4}}`))
	want := Usage{PromptTokens: 20, CompletionTokens: 8, CachedInputTokens: 4}
	if got := c.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// Пустой коллектор — нулевой Usage (учёт 0 при обрыве до message_start).
func TestAnthropicStreamUsageEmpty(t *testing.T) {
	c := &AnthropicStreamUsage{}
	if got := c.Usage(); got != (Usage{}) {
		t.Fatalf("usage %+v, ожидался нулевой", got)
	}
}
