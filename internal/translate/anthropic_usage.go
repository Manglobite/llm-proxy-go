package translate

import "encoding/json"

// AnthropicStreamUsage — чистый tee-коллектор usage anthropic-SSE (SPEC 8.6,
// строка «glm anthropic»). Нужен для passthrough-стрима glm-антропика: события
// уходят клиенту как есть, а usage параллельно копится отсюда — без ResponseWriter
// и без транслятора (passthrough не имеет права переписывать события).
//
// Источники полей (реальные события anthropic):
//   - message_start: message.usage.input_tokens / cache_read_input_tokens /
//     cache_creation_input_tokens — входная сторона и кеш;
//   - message_delta: usage.output_tokens — выходная сторона (накопительный).
type AnthropicStreamUsage struct {
	start    AnthropicUsage // из message_start (input + кеш-поля)
	delta    AnthropicUsage // из message_delta (output)
	gotStart bool
	gotDelta bool
}

// Feed разбирает одно событие SSE. События не из перечисленных и битый JSON
// молча игнорируются: tee-парсер не имеет права ломать passthrough — при
// неудаче учёт останется нулевым, что видно в дашборде, а клиентский стрим
// цел.
func (c *AnthropicStreamUsage) Feed(event string, data []byte) {
	ev := event
	if ev == "" {
		// event:-строки может не быть (событие шло голым data:) — тип события
		// у anthropic всегда продублирован полем type внутри data.
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &probe) == nil {
			ev = probe.Type
		}
	}
	switch ev {
	case "message_start":
		var m struct {
			Message *struct {
				Usage *AnthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &m); err == nil && m.Message != nil && m.Message.Usage != nil {
			c.start, c.gotStart = *m.Message.Usage, true
		}
	case "message_delta":
		var m struct {
			Usage *AnthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(data, &m); err == nil && m.Usage != nil {
			c.delta, c.gotDelta = *m.Usage, true
		}
	}
}

// Usage — собранный usage: input/cache из message_start, output из
// message_delta (последний wins: delta-события накопительные). Если
// message_start не дошёл, но delta есть — входная сторона берётся из delta:
// лучше частичный учёт, чем нулевой при живом ответе.
func (c *AnthropicStreamUsage) Usage() Usage {
	var out Usage
	if c.gotStart {
		out.PromptTokens = c.start.InputTokens
		out.CachedInputTokens = c.start.CacheReadInputTokens
		out.CacheCreationTokens = c.start.CacheCreationInputTokens
	}
	if c.gotDelta {
		out.CompletionTokens = c.delta.OutputTokens
		if !c.gotStart {
			out.PromptTokens = c.delta.InputTokens
			out.CachedInputTokens = c.delta.CacheReadInputTokens
			out.CacheCreationTokens = c.delta.CacheCreationInputTokens
		}
	}
	return out
}
