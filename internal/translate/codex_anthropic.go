package translate

import (
	"net/http"
	"strings"
)

// ─── SPEC 8.4 (правая таблица): Responses SSE → anthropic SSE ───────────────

// anthropicSink — эмиттер anthropic-семантики для движка codexToAnthropic:
// стрим шлёт SSE-события, агрегатор накапливает блоки полного ответа.
type anthropicSink interface {
	messageStart(inputTokens int)
	textDelta(text string)
	thinkingDelta(text string)
	toolStart(callID, name string)
	toolArgsDelta(fragment string)
	blockStop()
	messageFinish(stopReason string, u Usage)
	fail(message, code string)
}

// codexToAnthropic — общий движок SPEC 8.4 (правая таблица) поверх общего же
// декодера: state-машина одна, sink'ов две (стрим и агрегатор).
type codexToAnthropic struct {
	sink          anthropicSink
	textSeen      map[string]bool     // TR-5: text-дельты item уже приходили
	reasoningSeen map[string]bool     // TR-5: reasoning-дельты item уже приходили
	argsFrag      map[string][]string // куски аргументов function_call по item_id
	started       bool
	finished      bool
	usage         Usage
	inputTokens   int // usage на message_start, если codex сообщил его в response.created
	errMsg        string
	errCode       string
}

func newCodexToAnthropic() codexToAnthropic {
	return codexToAnthropic{
		textSeen:      map[string]bool{},
		reasoningSeen: map[string]bool{},
		argsFrag:      map[string][]string{},
	}
}

func (c *codexToAnthropic) feed(event string, data []byte) error {
	if c.finished {
		return nil
	}
	ev, ok := decodeCodexEvent(event, data)
	if !ok {
		return nil
	}
	switch ev.kind {
	case codexStart:
		if !c.started {
			c.started = true
			c.inputTokens = ev.inputTokens
			c.sink.messageStart(ev.inputTokens) // usage input_tokens, если уже известен (SPEC 8.4)
		}

	case codexTextDelta:
		c.textSeen[ev.itemID] = true
		c.sink.textDelta(ev.delta)

	case codexReasoningDelta:
		c.reasoningSeen[ev.itemID] = true
		c.sink.thinkingDelta(ev.delta)

	case codexArgsDelta:
		// Аргументы копим по item_id: content_block_start(tool_use) возможен
		// только на output_item.done, когда известны call_id и name (SPEC 8.4),
		// поэтому куски ждут открытия блока.
		c.argsFrag[ev.itemID] = append(c.argsFrag[ev.itemID], ev.delta)

	case codexItemDone:
		switch ev.item.Type {
		case "message":
			if !c.textSeen[ev.item.ID] { // TR-5: без дельт — контент одним куском на done
				if text := ev.item.messageText(); text != "" {
					c.sink.textDelta(text)
				}
			}
			c.sink.blockStop()
		case "reasoning":
			if !c.reasoningSeen[ev.item.ID] { // TR-5 — то же для summary
				for _, s := range ev.item.Summary {
					if s.Text != "" {
						c.sink.thinkingDelta(s.Text)
					}
				}
			}
			c.sink.blockStop()
		case "function_call":
			// content_block_start(tool_use: id,name) → input_json_delta частями
			// → content_block_stop (SPEC 8.4). Куски копились из дельт или
			// пришли целиком на done — sink получает их одной последовательностью.
			c.sink.toolStart(ev.item.CallID, ev.item.Name)
			if frags := c.argsFrag[ev.item.ID]; len(frags) > 0 {
				for _, f := range frags {
					c.sink.toolArgsDelta(f)
				}
			} else if ev.item.Arguments != "" {
				c.sink.toolArgsDelta(ev.item.Arguments) // одним куском
			}
			c.sink.blockStop()
		}

	case codexCompleted:
		c.usage = usageFromResponses(ev.usage)
		c.sink.blockStop() // закрыть открытые блоки перед финалом (SPEC 8.4)
		c.sink.messageFinish(codexStopReason(ev.output), c.usage)
		c.finished = true

	case codexIncomplete:
		c.usage = usageFromResponses(ev.usage)
		c.sink.blockStop()
		c.sink.messageFinish("max_tokens", c.usage)
		c.finished = true

	case codexFailed:
		c.errMsg, c.errCode = ev.errMsg, ev.errCode
		c.sink.fail(ev.errMsg, ev.errCode)
		c.finished = true
	}
	return nil
}

// Usage — накопленный normalized usage (tee).
func (c *codexToAnthropic) Usage() Usage { return c.usage }

// Err — ошибка апстрима из response.failed/error, если была.
func (c *codexToAnthropic) Err() error {
	if c.errMsg == "" && c.errCode == "" {
		return nil
	}
	return &StreamError{Code: c.errCode, Message: orDefault(c.errMsg, c.errCode)}
}

// Finished — пришёл ли финал (completed/incomplete/failed).
func (c *codexToAnthropic) Finished() bool { return c.finished }

// ─── Стрим ──────────────────────────────────────────────────────────────────

// CodexToAnthropicStream транслирует SSE codex в SSE anthropic messages
// (SPEC 8.4, правая таблица).
type CodexToAnthropicStream struct {
	codexToAnthropic
	anthropicEmitter
}

var _ Feeder = (*CodexToAnthropicStream)(nil)

// NewCodexToAnthropic создаёт транслятор поверх ответа клиенту.
func NewCodexToAnthropic(w http.ResponseWriter) *CodexToAnthropicStream {
	t := &CodexToAnthropicStream{anthropicEmitter: newAnthropicEmitter(w)}
	t.codexToAnthropic = newCodexToAnthropic()
	t.sink = t
	return t
}

// Feed — реализация Feeder; возвращает ошибку записи клиенту, если она была.
func (t *CodexToAnthropicStream) Feed(event string, data []byte) error {
	if t.writeErr != nil {
		return t.writeErr
	}
	return t.feed(event, data)
}

func (t *CodexToAnthropicStream) messageStart(inputTokens int) { t.emitMessageStart(inputTokens) }
func (t *CodexToAnthropicStream) textDelta(text string)        { t.emitTextDelta(text) }
func (t *CodexToAnthropicStream) thinkingDelta(text string)    { t.emitThinkingDelta(text) }
func (t *CodexToAnthropicStream) toolStart(callID, name string) {
	t.emitToolStart(callID, name)
}
func (t *CodexToAnthropicStream) toolArgsDelta(fragment string) { t.emitToolArgsDelta(fragment) }
func (t *CodexToAnthropicStream) blockStop()                    { t.emitBlockStop() }

func (t *CodexToAnthropicStream) messageFinish(stopReason string, u Usage) {
	// usage в message_delta — output_tokens (SPEC 8.4); полный normalized usage
	// транслятор отдаёт методом Usage() для accounting.
	t.emitMessageDelta(stopReason, map[string]any{"output_tokens": u.CompletionTokens})
	t.emitMessageStop()
}

func (t *CodexToAnthropicStream) fail(message, code string) { t.emitError(message, code) }

// ─── Non-stream агрегатор ───────────────────────────────────────────────────

// CodexToAnthropicResponse копит события Responses API в ПОЛНЫЙ anthropic-ответ
// (non-stream клиент к codex-апстриму, который всегда stream:true — SPEC 7.1).
// Feed-интерфейс общий со стримом, блоки и TR-5 решаются тем же движком.
type CodexToAnthropicResponse struct {
	codexToAnthropic
	blocks     []anthropicOutBlock
	curKind    string          // тип последнего блока: "" | "text" | "thinking" | "tool"
	toolArgs   strings.Builder // аргументы текущего tool-блока
	stopReason string
}

var _ Feeder = (*CodexToAnthropicResponse)(nil)

// NewCodexToAnthropicResponse создаёт агрегатор.
func NewCodexToAnthropicResponse() *CodexToAnthropicResponse {
	a := &CodexToAnthropicResponse{}
	a.codexToAnthropic = newCodexToAnthropic()
	a.sink = a
	return a
}

// Feed — реализация Feeder.
func (a *CodexToAnthropicResponse) Feed(event string, data []byte) error {
	return a.feed(event, data)
}

func (a *CodexToAnthropicResponse) messageStart(inputTokens int) {} // inputTokens уже хранит движок

func (a *CodexToAnthropicResponse) textDelta(text string) {
	if a.curKind != "text" {
		a.blocks = append(a.blocks, anthropicOutBlock{Type: "text"})
		a.curKind = "text"
	}
	a.blocks[len(a.blocks)-1].Text += text
}

func (a *CodexToAnthropicResponse) thinkingDelta(text string) {
	if a.curKind != "thinking" {
		a.blocks = append(a.blocks, anthropicOutBlock{Type: "thinking"})
		a.curKind = "thinking"
	}
	a.blocks[len(a.blocks)-1].Thinking += text
}

func (a *CodexToAnthropicResponse) toolStart(callID, name string) {
	a.blocks = append(a.blocks, anthropicOutBlock{Type: "tool_use", ID: callID, Name: name})
	a.curKind = "tool"
	a.toolArgs.Reset()
}

func (a *CodexToAnthropicResponse) toolArgsDelta(fragment string) {
	a.toolArgs.WriteString(fragment) // куски приходят только внутри открытого tool-блока
}

func (a *CodexToAnthropicResponse) blockStop() {
	if a.curKind == "tool" && len(a.blocks) > 0 {
		// anthropic требует объект в input; пустые/битые аргументы → "{}"
		// (иначе Marshal всего ответа упадёт на невалидном RawMessage).
		a.blocks[len(a.blocks)-1].Input = rawJSONObject(a.toolArgs.String())
	}
	a.curKind = ""
}

func (a *CodexToAnthropicResponse) messageFinish(stopReason string, u Usage) {
	a.stopReason = stopReason
}

func (a *CodexToAnthropicResponse) fail(message, code string) {} // движок уже записал errMsg/errCode

// Result возвращает накопленный ответ (после прокорма всех событий). Стрим без
// финала даёт пустой stop_reason — валидность проверяет провайдер через
// Finished()/Err().
func (a *CodexToAnthropicResponse) Result() *AnthropicResponse {
	content := a.blocks
	if content == nil {
		content = []anthropicOutBlock{{Type: "text", Text: ""}} // как в ChatToAnthropic (few-shot)
	}
	u := a.usage
	return &AnthropicResponse{
		ID:         "msg_proxy",
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		StopReason: a.stopReason,
		Usage: anthropicOutUsage{
			InputTokens:              u.PromptTokens,
			OutputTokens:             u.CompletionTokens,
			CacheReadInputTokens:     u.CachedInputTokens,
			CacheCreationInputTokens: u.CacheCreationTokens,
		},
	}
}
