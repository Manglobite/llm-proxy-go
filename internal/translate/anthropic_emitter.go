package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// anthropicEmitter — общая SSE-механика anthropic-выхода: фрейминг
// "event: <name>\ndata: <json>\n\n" и книга последовательных блоков
// (content_block_start → дельты → content_block_stop, TR-6). Общий для
// кодекс- и chat-трансляторов, чтобы фрейминг не дублировался; Flush после
// каждого события (ARCHITECTURE §5).
type anthropicEmitter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	blockIdx int    // index следующего блока
	openIdx  int    // index открытого блока, -1 — открытых нет
	openKind string // "" | "text" | "thinking" | "tool"
	writeErr error  // первая ошибка записи; доедет до провайдера через Feed
}

func newAnthropicEmitter(w http.ResponseWriter) anthropicEmitter {
	e := anthropicEmitter{w: w, openIdx: -1}
	if f, ok := w.(http.Flusher); ok {
		e.flusher = f
	}
	return e
}

func (e *anthropicEmitter) emit(event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return // Marshal структур под нашим контролем не падает
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b); err != nil && e.writeErr == nil {
		e.writeErr = err // клиент ушёл: дальше пишем в никуда, но ошибку вернёт Feed
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *anthropicEmitter) closeOpenBlock() {
	if e.openIdx < 0 {
		return
	}
	e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.openIdx})
	e.openIdx, e.openKind = -1, ""
}

// openBlock закрывает предыдущий блок (anthropic требует последовательные
// блоки: start → дельты → stop, TR-6) и открывает новый со следующим index.
func (e *anthropicEmitter) openBlock(kind string, contentBlock map[string]any) int {
	e.closeOpenBlock()
	idx := e.blockIdx
	e.blockIdx++
	e.openIdx, e.openKind = idx, kind
	e.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": contentBlock,
	})
	return idx
}

func (e *anthropicEmitter) emitMessageStart(inputTokens int) {
	e.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_proxy", "type": "message", "role": "assistant", "model": "",
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
}

func (e *anthropicEmitter) emitTextDelta(text string) {
	if e.openKind != "text" {
		e.openBlock("text", map[string]any{"type": "text", "text": ""})
	}
	e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.openIdx,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (e *anthropicEmitter) emitThinkingDelta(text string) {
	if e.openKind != "thinking" {
		e.openBlock("thinking", map[string]any{"type": "thinking", "thinking": ""})
	}
	e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.openIdx,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (e *anthropicEmitter) emitToolStart(callID, name string) int {
	return e.openBlock("tool", map[string]any{
		"type": "tool_use", "id": callID, "name": name, "input": map[string]any{},
	})
}

func (e *anthropicEmitter) emitToolArgsDelta(fragment string) {
	if e.openKind != "tool" {
		return // дельты без открытого tool-блока не бывают: порядок гарантирует движок
	}
	e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.openIdx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": fragment},
	})
}

func (e *anthropicEmitter) emitBlockStop() { e.closeOpenBlock() }

// emitMessageDelta — stop_reason + usage; отдельно от message_stop, потому что
// chat-путь присылает usage-чанк ПОСЛЕ finish_reason и message_delta копит
// usage до самого конца стрима (SPEC 8.5).
func (e *anthropicEmitter) emitMessageDelta(stopReason string, usage map[string]any) {
	e.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	})
}

func (e *anthropicEmitter) emitMessageStop() {
	e.emit("message_stop", map[string]any{"type": "message_stop"})
}

// emitError — SSE-событие error (SPEC 8.4: response.failed/error). После него
// message_stop не шлём: сообщение не завершилось, притворяться иначе нельзя.
func (e *anthropicEmitter) emitError(message, code string) {
	e.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "upstream_error", "message": orDefault(message, "upstream stream failed"), "code": code},
	})
}
