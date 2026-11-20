package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ─── SPEC 8.4 (левая таблица): Responses SSE → openai chat chunks ───────────

// openaiSink — эмиттер openai-семантики. Две реализации: стрим (чанки в
// http.ResponseWriter) и агрегатор (накопление полного chat.completion).
type openaiSink interface {
	roleChunk()
	contentDelta(text string)
	reasoningDelta(text string)
	toolCall(index int, call ChatCall)
	finish(reason string, u Usage)
	fail(message, code string)
}

// codexToOpenAI — общий движок SPEC 8.4 (левая таблица): state-машина поверх
// общего декодера, эмитящая в sink. Стрим и агрегатор различаются только
// sink'ом — таблица маппинга событий существует в одном экземпляре.
type codexToOpenAI struct {
	sink             openaiSink
	itemDelta        map[string]bool // TR-5: по item_id — приходили ли текстовые дельты
	openTool         int             // index следующего tool_call
	started          bool
	done             bool // финал отправлен; события после него игнорируются
	reasoningEmitted bool // summary reasoning эмитится один раз (few-shot)
	usage            Usage
	errMsg, errCode  string
}

func newCodexToOpenAI() codexToOpenAI {
	return codexToOpenAI{itemDelta: map[string]bool{}}
}

// feed обрабатывает одно событие апстрима. Ошибкой является только проблема
// записи клиенту (стрим); неизвестные события ошибкой не считаются.
func (c *codexToOpenAI) feed(event string, data []byte) error {
	if c.done {
		return nil
	}
	ev, ok := decodeCodexEvent(event, data)
	if !ok {
		return nil // неизвестные события Responses API молча пропускаем
	}
	switch ev.kind {
	case codexStart:
		if !c.started {
			c.started = true
			c.sink.roleChunk()
		}

	case codexTextDelta:
		c.itemDelta[ev.itemID] = true
		c.sink.contentDelta(ev.delta)

	case codexItemDone:
		switch ev.item.Type {
		case "message":
			if !c.itemDelta[ev.item.ID] { // TR-5: item без дельт — контент одним куском
				if text := ev.item.messageText(); text != "" {
					c.sink.contentDelta(text)
				}
			}
		case "reasoning":
			// z.ai-стиль reasoning_content (SPEC 8.4). Дельты reasoning в чанки
			// не дробим: summary на done несёт полный текст, дробление дало бы
			// клиенту рваную простыню без пользы.
			if len(ev.item.Summary) > 0 {
				for _, s := range ev.item.Summary {
					if s.Text != "" {
						c.sink.reasoningDelta(s.Text)
					}
				}
				c.reasoningEmitted = true
			}
		case "function_call":
			c.sink.toolCall(c.openTool, ChatCall{
				ID: ev.item.CallID, Type: "function",
				Function: ChatFunction{Name: ev.item.Name, Arguments: ev.item.Arguments},
			})
			c.openTool++
		}

	case codexCompleted:
		c.usage = usageFromResponses(ev.usage)
		if !c.reasoningEmitted {
			// few-shot: reasoning без дельт и без done — эмитим из финального output
			for _, it := range ev.output {
				if it.Type != "reasoning" {
					continue
				}
				for _, s := range it.Summary {
					if s.Text != "" {
						c.sink.reasoningDelta(s.Text)
					}
				}
			}
		}
		c.sink.finish(codexFinishReason(ev.output), c.usage)
		c.done = true

	case codexIncomplete:
		c.usage = usageFromResponses(ev.usage)
		c.sink.finish("length", c.usage)
		c.done = true

	case codexFailed:
		c.errMsg, c.errCode = ev.errMsg, ev.errCode
		c.sink.fail(ev.errMsg, ev.errCode)
		c.done = true
	}
	return nil
}

// Usage — накопленный normalized usage (tee, ARCHITECTURE §5).
func (c *codexToOpenAI) Usage() Usage { return c.usage }

// Err — ошибка апстрима, если пришло response.failed/error; иначе nil.
func (c *codexToOpenAI) Err() error {
	if c.errMsg == "" && c.errCode == "" {
		return nil
	}
	return &StreamError{Code: c.errCode, Message: orDefault(c.errMsg, c.errCode)}
}

// Finished — пришёл ли финал (completed/incomplete/failed). Стрим без финала
// провайдер обязан обработать как ошибку, а не отдавать усечённый ответ.
func (c *codexToOpenAI) Finished() bool { return c.done }

// ─── Стрим ──────────────────────────────────────────────────────────────────

// CodexToOpenAIStream транслирует SSE codex в openai-chat chunks, пишет их в w
// (SSE-фрейминг "data: {...}\n\n") и копит usage (few-shot sse-codex-to-openai).
type CodexToOpenAIStream struct {
	codexToOpenAI
	w        http.ResponseWriter
	flusher  http.Flusher
	writeErr error // первая ошибка записи; Feed возвращает её, провайдер прекращает чтение апстрима
}

var _ Feeder = (*CodexToOpenAIStream)(nil)

// NewCodexToOpenAI создаёт транслятор поверх ответа клиенту.
func NewCodexToOpenAI(w http.ResponseWriter) *CodexToOpenAIStream {
	t := &CodexToOpenAIStream{w: w}
	t.codexToOpenAI = newCodexToOpenAI()
	// Flusher есть у штатного net/http-ответа; отсутствие не фатально —
	// события уйдут буферизованно, стрим просто станет «рваным» на клиенте.
	if f, ok := w.(http.Flusher); ok {
		t.flusher = f
	}
	t.sink = t // события движка идут в методы-эмиттеры ниже
	return t
}

// Feed скармливает транслятору одно SSE-событие апстрима (из ScanSSE).
// Возвращает ошибку только при проблемах записи клиенту.
func (t *CodexToOpenAIStream) Feed(event string, data []byte) error {
	if t.writeErr != nil {
		return t.writeErr // клиент уже отвалился: провайдер обязан прекратить чтение
	}
	return t.feed(event, data)
}

func (t *CodexToOpenAIStream) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return // Marshal структур под нашим контролем не падает
	}
	if _, err := fmt.Fprintf(t.w, "data: %s\n\n", b); err != nil && t.writeErr == nil {
		t.writeErr = err // пишем дальше в никуда, но первая ошибка доедет до провайдера
	}
	t.flush()
}

func (t *CodexToOpenAIStream) flush() {
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

func (t *CodexToOpenAIStream) chunk(delta map[string]any) {
	choices := []map[string]any{{"index": 0, "delta": delta}}
	t.emit(map[string]any{"id": "chatcmpl-proxy", "object": "chat.completion.chunk", "choices": choices})
}

func (t *CodexToOpenAIStream) roleChunk() {
	t.chunk(map[string]any{"role": "assistant", "content": ""})
}

func (t *CodexToOpenAIStream) contentDelta(text string) {
	t.chunk(map[string]any{"content": text})
}

func (t *CodexToOpenAIStream) reasoningDelta(text string) {
	t.chunk(map[string]any{"reasoning_content": text}) // z.ai-стиль, opencode понимает (SPEC 8.4)
}

func (t *CodexToOpenAIStream) toolCall(index int, call ChatCall) {
	t.chunk(map[string]any{"tool_calls": []map[string]any{{
		"index": index, "id": call.ID, "type": "function",
		"function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments},
	}}})
}

func (t *CodexToOpenAIStream) finish(reason string, u Usage) {
	// finish_reason — на уровне choice, рядом с delta (OpenAI-wire, SPEC 8.4), НЕ внутри delta
	t.emit(map[string]any{
		"id": "chatcmpl-proxy", "object": "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	})
	// usage-чанк: choices пустой (семантика stream_options.include_usage)
	t.emit(map[string]any{
		"id": "chatcmpl-proxy", "object": "chat.completion.chunk", "choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CachedInputTokens},
		},
	})
	t.writeRaw("data: [DONE]\n\n") // [DONE] всегда после финиша (TR-4)
	t.flush()
}

func (t *CodexToOpenAIStream) fail(message, code string) {
	t.emit(map[string]any{"error": map[string]any{
		"message": orDefault(message, "upstream stream failed"),
		"type":    "upstream_error", "code": code,
	}})
	t.writeRaw("data: [DONE]\n\n") // ошибка апстрима → chunk с error → [DONE] (TR-4)
	t.flush()
}

func (t *CodexToOpenAIStream) writeRaw(s string) {
	if _, err := fmt.Fprint(t.w, s); err != nil && t.writeErr == nil {
		t.writeErr = err
	}
}

// ─── Non-stream агрегатор ───────────────────────────────────────────────────

// ChatCompletion — полный (non-stream) ответ chat.completion.
type ChatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"` // "chat.completion"
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *ChatUsage             `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type ChatCompletionMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"` // z.ai-стиль
	ToolCalls        []ChatCall `json:"tool_calls,omitempty"`
}

// CodexToOpenAIResponse — non-stream агрегатор: копит события Responses API в
// ПОЛНЫЙ chat.completion для клиента, который не умеет стрим, при апстриме,
// который всегда стримит (store:false, stream:true — SPEC 7.1). Feed-интерфейс
// тот же, что у стрима; маппинг событий общий (codexToOpenAI).
type CodexToOpenAIResponse struct {
	codexToOpenAI
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    []ChatCall
	finishReason string
}

var _ Feeder = (*CodexToOpenAIResponse)(nil)

// NewCodexToOpenAIResponse создаёт агрегатор.
func NewCodexToOpenAIResponse() *CodexToOpenAIResponse {
	a := &CodexToOpenAIResponse{}
	a.codexToOpenAI = newCodexToOpenAI()
	a.sink = a
	return a
}

// Feed — реализация Feeder.
func (a *CodexToOpenAIResponse) Feed(event string, data []byte) error {
	return a.feed(event, data)
}

func (a *CodexToOpenAIResponse) roleChunk() {} // роль агрегатору известна: собирается в Result

func (a *CodexToOpenAIResponse) contentDelta(text string) {
	a.content.WriteString(text)
}

func (a *CodexToOpenAIResponse) reasoningDelta(text string) {
	a.reasoning.WriteString(text)
}

func (a *CodexToOpenAIResponse) toolCall(index int, call ChatCall) {
	// index движка монотонен и совпадает с порядком добавления; условие ловит
	// рассинхрон индексов (пропуск/сдвиг), а не дубли: повторный done того же
	// вызова от codex не наблюдается, дедуп по CallID не оправдан.
	if index == len(a.toolCalls) {
		a.toolCalls = append(a.toolCalls, call)
	}
}

func (a *CodexToOpenAIResponse) finish(reason string, u Usage) {
	a.finishReason = reason
}

func (a *CodexToOpenAIResponse) fail(message, code string) {} // ошибку уже записал движок (Err/Finished)

// Result возвращает накопленный ответ. Для стрима без финала finish_reason
// подменяется на stop: усечённость видна по Finished()/Err(), а клиенту нужен
// валидный объект.
func (a *CodexToOpenAIResponse) Result() *ChatCompletion {
	msg := ChatCompletionMessage{Role: "assistant", Content: a.content.String()}
	if a.reasoning.Len() > 0 {
		msg.ReasoningContent = a.reasoning.String()
	}
	if len(a.toolCalls) > 0 {
		msg.ToolCalls = a.toolCalls
	}
	return &ChatCompletion{
		ID:     "chatcmpl-proxy",
		Object: "chat.completion",
		Choices: []ChatCompletionChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: orDefault(a.finishReason, "stop"),
		}},
		Usage: chatUsageForCompletion(a.usage),
	}
}

// chatUsageForCompletion — normalized Usage в wire-форме chat.completion;
// нулевые детали не сериализуем, чтобы не шуметь в JSON.
func chatUsageForCompletion(u Usage) *ChatUsage {
	out := &ChatUsage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}
	if u.CachedInputTokens != 0 {
		out.PromptTokensDetails = &ChatUsageDetail{CachedTokens: u.CachedInputTokens}
	}
	if u.ReasoningTokens != 0 {
		out.CompletionTokensDetails = &ChatUsageDetail{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}
