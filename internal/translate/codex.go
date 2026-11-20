package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ─── Общий декодер событий Responses API (SPEC 8.4) ─────────────────────────
//
// Таблица полей апстрима существует в ОДНОМ экземпляре: её используют все
// четыре потребителя (стрим и агрегатор × openai/anthropic выход), поэтому
// дублирование маппингов исключено по построению.

// codexEventKind — тип декодированного события.
type codexEventKind int

const (
	codexStart          codexEventKind = iota // response.created
	codexTextDelta                            // response.output_text.delta
	codexReasoningDelta                       // response.reasoning_summary_text.delta | response.reasoning_text.delta
	codexArgsDelta                            // response.function_call_arguments.delta
	codexItemDone                             // response.output_item.done
	codexCompleted                            // response.completed
	codexIncomplete                           // response.incomplete
	codexFailed                               // response.failed | error
)

// codexEvent — декодированное событие; значимы поля, соответствующие kind.
type codexEvent struct {
	kind             codexEventKind
	itemID           string         // для дельт
	delta            string         // текст/кусок аргументов
	item             codexItem      // output_item.done
	usage            responsesUsage // completed/incomplete
	output           []codexItem    // completed: финальный output (stop-причина, fallback reasoning)
	inputTokens      int            // created: input_tokens, если апстрим сообщил usage сразу
	respID           string         // created/completed: id response-объекта (агрегатор responses)
	model            string         // created/completed: модель апстрима
	incompleteReason string         // incomplete: incomplete_details.reason
	errMsg           string
	errCode          string
}

// codexItem — элемент output апстрима (message | reasoning | function_call).
type codexItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"` // плоское поле — упрощение few-shot
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content,omitempty"` // реальная форма codex: текст в content[].text
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// messageText — полный текст message-item: плоское text, иначе склейка
// content[].text (настоящая форма codex; few-shot упрощал до плоского поля).
func (it codexItem) messageText() string {
	if it.Text != "" {
		return it.Text
	}
	var sb strings.Builder
	for _, c := range it.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// decodeCodexEvent разбирает (event, data) SSE апстрима. ok == false для
// неизвестных событий и форм (output_item.added, content_part.*, ping, ...) —
// потребители молча пропускают их, стрим не рвётся.
func decodeCodexEvent(event string, data []byte) (codexEvent, bool) {
	var ev codexEvent
	switch event {
	case "response.created":
		var d struct {
			Response struct {
				ID    string          `json:"id"`
				Model string          `json:"model"`
				Usage *responsesUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false
		}
		ev.kind = codexStart
		ev.respID, ev.model = d.Response.ID, d.Response.Model
		if d.Response.Usage != nil {
			ev.inputTokens = d.Response.Usage.InputTokens
		}
		return ev, true

	case "response.output_text.delta":
		var d struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false
		}
		ev.kind, ev.itemID, ev.delta = codexTextDelta, d.ItemID, d.Delta
		return ev, true

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		var d struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false
		}
		ev.kind, ev.itemID, ev.delta = codexReasoningDelta, d.ItemID, d.Delta
		return ev, true

	case "response.function_call_arguments.delta":
		var d struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false
		}
		ev.kind, ev.itemID, ev.delta = codexArgsDelta, d.ItemID, d.Delta
		return ev, true

	case "response.output_item.done":
		var d struct {
			Item codexItem `json:"item"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false // неизвестную форму item молча пропускаем, стрим не рвём
		}
		ev.kind, ev.item = codexItemDone, d.Item
		return ev, true

	case "response.completed", "response.incomplete":
		var d struct {
			Response struct {
				ID                string         `json:"id"`
				Model             string         `json:"model"`
				Usage             responsesUsage `json:"usage"`
				Output            []codexItem    `json:"output"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false
		}
		ev.kind = codexCompleted
		if event == "response.incomplete" {
			ev.kind = codexIncomplete
		}
		ev.respID, ev.model = d.Response.ID, d.Response.Model
		ev.usage, ev.output = d.Response.Usage, d.Response.Output
		if d.Response.IncompleteDetails != nil {
			ev.incompleteReason = d.Response.IncompleteDetails.Reason
		}
		return ev, true

	case "response.failed", "error":
		// Ошибка приходит в трёх формах: response.failed вкладывает её в
		// response.error; событие error несёт её объектом "error" (форма
		// few-shot) или плоскими полями code/message верхнего уровня (реальная
		// форма OpenAI). Читаем все три места, приоритет — непустое.
		var d struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Error   struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
			Response struct {
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return ev, false
		}
		ev.kind = codexFailed
		ev.errMsg, ev.errCode = d.Error.Message, d.Error.Code
		if ev.errMsg == "" {
			ev.errMsg = d.Message
		}
		if ev.errCode == "" {
			ev.errCode = d.Code
		}
		if ev.errMsg == "" {
			ev.errMsg = d.Response.Error.Message
		}
		if ev.errCode == "" {
			ev.errCode = d.Response.Error.Code
		}
		return ev, true
	}
	return ev, false
}

// codexFinishReason — finish_reason для openai-выхода из финального output:
// наличие function_call → tool_calls, иначе stop (обязательство few-shot).
func codexFinishReason(output []codexItem) string {
	for _, it := range output {
		if it.Type == "function_call" {
			return "tool_calls"
		}
	}
	return "stop"
}

// codexStopReason — stop_reason для anthropic-выхода (SPEC 8.4): end_turn/tool_use.
func codexStopReason(output []codexItem) string {
	for _, it := range output {
		if it.Type == "function_call" {
			return "tool_use"
		}
	}
	return "end_turn"
}

// StreamError — ошибка апстрима, пришедшая событием response.failed/error;
// наверху ветвятся через errors.As (CODING 1.2).
type StreamError struct {
	Code    string
	Message string
}

func (e *StreamError) Error() string {
	if e.Code == "" {
		return "upstream stream failed: " + e.Message
	}
	return fmt.Sprintf("upstream stream failed (code %q): %s", e.Code, e.Message)
}

// orDefault — подстановка дефолта для пустой строки (few-shot sse).
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Feeder — общий контракт приёмника SSE-событий: и стримы, и агрегаторы
// получают события апстрима через один и тот же Feed (провайдер гонит их
// из ScanSSE, ARCHITECTURE §5).
type Feeder interface {
	Feed(event string, data []byte) error
}
