package translate

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// ─── SPEC 8.5: anthropic-вход → openai-like апстрим (чат-чанки → anthropic) ──

// chatChunk — чанк openai-chat стрима (то, что отдаёт glm/openai-like апстрим).
type chatChunk struct {
	Model   string `json:"model"` // имя модели апстрима; ChatToResponsesStream подставляет его, если SetModel не звался
	Choices []struct {
		Index        int            `json:"index"`
		Delta        chatChunkDelta `json:"delta"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *ChatUsage `json:"usage"`
}

type chatChunkDelta struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"` // z.ai-стиль reasoning (SPEC 8.5)
	ToolCalls        []chatToolFragment `json:"tool_calls"`
}

// chatToolFragment — фрагмент tool_calls: первый несёт id/name, остальные —
// только куски arguments.
type chatToolFragment struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// ChatToAnthropicStream транслирует SSE openai-chat апстрима в SSE anthropic
// (SPEC 8.5): message_start → блоки → message_delta + message_stop (TR-4:
// финальные события обязательны даже при обрыве без [DONE] — см. Finish).
type ChatToAnthropicStream struct {
	anthropicEmitter
	started      bool
	done         bool
	tools        map[int]int // index tool_call из chat → index anthropic-блока
	finishReason string
	usage        Usage
}

var _ Feeder = (*ChatToAnthropicStream)(nil)

// NewChatToAnthropic создаёт транслятор поверх ответа клиенту.
func NewChatToAnthropic(w http.ResponseWriter) *ChatToAnthropicStream {
	return &ChatToAnthropicStream{
		anthropicEmitter: newAnthropicEmitter(w),
		tools:            map[int]int{},
	}
}

// Feed — реализация Feeder.
func (t *ChatToAnthropicStream) Feed(event string, data []byte) error {
	if t.done {
		return nil
	}
	if t.writeErr != nil {
		return t.writeErr
	}
	// [DONE] — терминатор openai-стрима. usage-чанк приходит ПОСЛЕ чанка с
	// finish_reason, поэтому финал (message_delta с usage) откладывается до
	// [DONE]/Finish — иначе usage не попал бы в message_delta (SPEC 8.5).
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return t.Finish()
	}
	var ch chatChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil // неизвестную форму чанка молча пропускаем — стрим не рвём
	}
	if ch.Usage != nil {
		t.usage = usageFromChat(ch.Usage)
	}
	if len(ch.Choices) == 0 {
		return nil // usage-чанк (choices:[], поле usage) — usage уже скоплен
	}
	c := ch.Choices[0]
	if !t.started {
		t.started = true
		t.emitMessageStart(0) // input_tokens чанк openai не несёт
	}
	if c.Delta.ReasoningContent != "" {
		t.emitThinkingDelta(c.Delta.ReasoningContent)
	}
	if c.Delta.Content != "" {
		t.emitTextDelta(c.Delta.Content)
	}
	for _, fr := range c.Delta.ToolCalls {
		idx, ok := t.tools[fr.Index]
		if !ok {
			// content_block_start на первом фрагменте по index (SPEC 8.5).
			// Текстовый блок открывается лениво (на первом content), поэтому
			// ответ «только tool_calls» не получает пустого text-блока впереди —
			// так ведёт себя и настоящий anthropic.
			idx = t.emitToolStart(orDefault(fr.ID, "tool_call"), fr.Function.Name)
			t.tools[fr.Index] = idx
		}
		if fr.Function.Arguments != "" {
			t.emitToolArgsDelta(fr.Function.Arguments)
		}
	}
	if c.FinishReason != "" {
		t.finishReason = anthropicStopReason(c.FinishReason) // length→max_tokens, tool_calls→tool_use (TR-8)
	}
	return nil
}

// Finish закрывает стрим: message_delta (stop_reason + usage) + message_stop.
// Идемпотентна. Вызывается на [DONE]; провайдер обязан вызвать её и после
// окончания чтения апстрима, если [DONE] не пришёл (обрыв), чтобы финальные
// события anthropic-стрима существовали всегда (TR-4).
func (t *ChatToAnthropicStream) Finish() error {
	if t.done {
		return nil
	}
	t.done = true
	if t.writeErr != nil {
		return t.writeErr
	}
	if !t.started {
		t.emitMessageStart(0) // стрим без единого чанка: обязательные финальные события всё равно уходят
	}
	t.emitBlockStop()
	t.emitMessageDelta(orDefault(t.finishReason, "end_turn"), map[string]any{
		// input_tokens в message_delta нестандартен для anthropic, но openai
		// несёт usage один раз и без него tee-usage клиента терял бы prompt-часть.
		"input_tokens": t.usage.PromptTokens, "output_tokens": t.usage.CompletionTokens,
	})
	t.emitMessageStop()
	return nil
}

// Fail — обрыв/ошибка апстрима: SSE-событие error и конец; message_stop НЕ
// шлётся — ошибка не должна выглядеть корректно завершённым сообщением (SPEC 4).
// Контракт пары: нормальный конец стрима — Finish, обрыв апстрима — Fail.
func (t *ChatToAnthropicStream) Fail(message, code string) {
	if t.done {
		return
	}
	t.done = true
	t.emitError(message, code)
}

// Usage — накопленный normalized usage (tee, ARCHITECTURE §5): openai несёт
// usage отдельным чанком (choices:[]) ПОСЛЕ чанка с finish_reason, Feed копит
// его, Finish кладёт в message_delta, а для БД источник один — этот метод.
func (t *ChatToAnthropicStream) Usage() Usage { return t.usage }
