package translate

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ─── SPEC 8.7: chat-апстрим → Responses-выход (вход /v1/responses) ──────────

// ResponsesAPIResponse — полный объект Responses API: форма data.response из
// событий response.completed/incomplete живых codex-фикстур и non-stream-форма
// SPEC 8.7. Используется и как non-stream ответ (ChatToResponses,
// CodexToResponsesResponse), и внутри синтезируемых событий стрима.
type ResponsesAPIResponse struct {
	ID                string                      `json:"id"`
	Object            string                      `json:"object"`     // "response"
	CreatedAt         int64                       `json:"created_at"` // unix
	Status            string                      `json:"status"`     // in_progress | completed | incomplete | failed
	Model             string                      `json:"model"`
	Output            []ResponsesOutputItem       `json:"output"`
	Usage             *ResponsesOutUsage          `json:"usage"`
	IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *ResponsesOutError          `json:"error,omitempty"`
}

// ResponsesOutputItem — элемент output (message | reasoning | function_call);
// набор значимых полей зависит от Type, omitempty отсекает чужие поля каждой
// формы. Arguments с omitempty: пустая строка в typed-форме не сериализуется —
// канон события output_item.added (arguments:"") даёт map-эмиссия стрима
// (responsesItemMap).
type ResponsesOutputItem struct {
	Type      string                `json:"type"`
	ID        string                `json:"id"`
	Role      string                `json:"role,omitempty"`      // message
	Content   []ResponsesOutContent `json:"content,omitempty"`   // message
	Summary   []ResponsesOutContent `json:"summary,omitempty"`   // reasoning
	CallID    string                `json:"call_id,omitempty"`   // function_call
	Name      string                `json:"name,omitempty"`      // function_call
	Arguments string                `json:"arguments,omitempty"` // function_call, JSON-строка
}

// ResponsesOutContent — часть content/summary: output_text у message,
// summary_text у reasoning (типы — из живых codex-фикстур).
type ResponsesOutContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesOutUsage — usage в wire-форме Responses API (SPEC 8.6, строка «codex
// responses»); форма совпадает с responsesUsage, но тип именованный: он — поле
// экспортируемого ResponsesAPIResponse.
type ResponsesOutUsage struct {
	InputTokens         int                             `json:"input_tokens"`
	OutputTokens        int                             `json:"output_tokens"`
	InputTokensDetails  *ResponsesOutUsageInputDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutUsageOutputDetails `json:"output_tokens_details,omitempty"`
}

// ResponsesOutUsageInputDetails — input_tokens_details; оба ключа присутствуют
// всегда (форма живых codex-ответов).
type ResponsesOutUsageInputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// ResponsesOutUsageOutputDetails — output_tokens_details.
type ResponsesOutUsageOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesIncompleteDetails — причина incomplete (SPEC 8.7: max_output_tokens).
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponsesOutError — ошибка в объекте response (response.failed).
type ResponsesOutError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// responsesOutUsageFor — normalized Usage в wire-форме Responses API (SPEC 8.6
// → 8.7: cached/reasoning уходят в details). Нулевые детали не сериализуем,
// чтобы не шуметь в JSON.
func responsesOutUsageFor(u Usage) *ResponsesOutUsage {
	out := &ResponsesOutUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.CachedInputTokens != 0 || u.CacheCreationTokens != 0 {
		out.InputTokensDetails = &ResponsesOutUsageInputDetails{
			CachedTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheCreationTokens,
		}
	}
	if u.ReasoningTokens != 0 {
		out.OutputTokensDetails = &ResponsesOutUsageOutputDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// newResponseID — идентификатор синтезируемого response (SPEC 8.7: resp_<hex>).
// crypto/rand, а не счётчик: id уходит клиенту как настоящий ответ Responses API.
func newResponseID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "resp_proxy" // Read по документации не ошибается; ветка для формальной корректности
	}
	return "resp_" + hex.EncodeToString(b[:])
}

// itemIDByIndex — синтетический идентификатор item (SPEC 8.7: item_0, item_1, …).
func itemIDByIndex(i int) string { return "item_" + strconv.Itoa(i) }

// responsesEmitter — SSE-механика Responses-выхода: фрейминг
// "event: <name>\ndata: <json>\n\n" (type в data обязателен — канон SPEC 8.7)
// и Flush после каждого события (ARCHITECTURE §5). Отдельно от
// anthropicEmitter: тому нужна книга последовательных блоков, здесь она не
// нужна, а общая часть — пятнадцать строк emit.
type responsesEmitter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	writeErr error // первая ошибка записи; доедет до провайдера через Feed
}

func newResponsesEmitter(w http.ResponseWriter) responsesEmitter {
	e := responsesEmitter{w: w}
	if f, ok := w.(http.Flusher); ok {
		e.flusher = f
	}
	return e
}

func (e *responsesEmitter) emit(event string, v any) {
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

// responsesOutItem — накапливаемый item стрима; на output_item.done
// сериализуется целиком (TR-5: без дельт контент одним куском на done).
type responsesOutItem struct {
	kind   string          // "message" | "reasoning" | "function_call"
	index  int             // output_index — он же суффикс синтетического id item_<n>
	text   strings.Builder // текст message / reasoning
	callID string          // function_call
	name   string          // function_call
	args   strings.Builder // аргументы function_call (JSON-строка, кусками)
}

func (it *responsesOutItem) itemID() string { return itemIDByIndex(it.index) }

// responsesItemMap — item в wire-форме событий (канон SPEC 8.7 по живым
// codex-фикстурам). added: content/summary — пустые массивы, arguments — ""
// (ключ обязателен на output_item.added); map, а не ResponsesOutputItem,
// именно ради этого: omitempty typed-формы снял бы пустой arguments.
func responsesItemMap(it *responsesOutItem, added bool) map[string]any {
	m := map[string]any{"type": it.kind, "id": it.itemID()}
	switch it.kind {
	case "message":
		m["role"] = "assistant"
		if added {
			m["content"] = []any{}
		} else {
			m["content"] = []map[string]any{{"type": "output_text", "text": it.text.String()}}
		}
	case "reasoning":
		if added {
			m["summary"] = []any{}
		} else {
			m["summary"] = []map[string]any{{"type": "summary_text", "text": it.text.String()}}
		}
	case "function_call":
		m["call_id"] = it.callID
		m["name"] = it.name
		m["arguments"] = it.args.String() // и на added (""), и на done — ключ обязателен
	}
	return m
}

// ChatToResponsesStream транслирует SSE openai-chat апстрима в события
// Responses API (SPEC 8.7): response.created → output_item.added → дельты →
// output_item.done → response.completed/incomplete. [DONE] апстрима только
// ЗАВЕРШАЕТ стрим: протокол Responses терминатора не имеет и не эмитится.
type ChatToResponsesStream struct {
	responsesEmitter
	respID       string
	createdAt    int64
	model        string
	started      bool
	done         bool
	items        []*responsesOutItem       // открытые items в порядке output
	tools        map[int]*responsesOutItem // chat-индекс tool_call → function_call item
	itemsDone    bool
	finishReason string
	usage        Usage
}

var _ Feeder = (*ChatToResponsesStream)(nil)

// NewChatToResponses создаёт транслятор поверх ответа клиенту. model — алиас
// из запроса клиента; поле model чанка апстрима подставляется, только если
// SetModel не вызван: клиент должен видеть имя, которое сам и просил.
func NewChatToResponses(w http.ResponseWriter) *ChatToResponsesStream {
	return &ChatToResponsesStream{
		responsesEmitter: newResponsesEmitter(w),
		respID:           newResponseID(),
		createdAt:        time.Now().Unix(),
		tools:            map[int]*responsesOutItem{},
	}
}

// SetModel — имя модели для response.created/completed, если оно известно
// заранее (алиас запроса); вызывается до первого Feed.
func (t *ChatToResponsesStream) SetModel(model string) { t.model = model }

// Feed — реализация Feeder.
func (t *ChatToResponsesStream) Feed(event string, data []byte) error {
	if t.done {
		return nil
	}
	if t.writeErr != nil {
		return t.writeErr // клиент уже отвалился: провайдер обязан прекратить чтение
	}
	// [DONE] — терминатор openai-стрима. usage-чанк приходит ПОСЛЕ чанка с
	// finish_reason, поэтому финальное событие откладывается до [DONE]/Finish —
	// иначе usage не попал бы в response (SPEC 8.5/8.7).
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return t.Finish()
	}
	var ch chatChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil // неизвестную форму чанка молча пропускаем — стрим не рвём
	}
	if t.model == "" && ch.Model != "" {
		t.model = ch.Model
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
		t.emitCreated()
	}
	if c.Delta.ReasoningContent != "" {
		it := t.openItem("reasoning")
		it.text.WriteString(c.Delta.ReasoningContent)
		t.emitDelta("response.reasoning_text.delta", it, c.Delta.ReasoningContent)
	}
	if c.Delta.Content != "" {
		it := t.openItem("message")
		it.text.WriteString(c.Delta.Content)
		t.emitDelta("response.output_text.delta", it, c.Delta.Content)
	}
	for _, fr := range c.Delta.ToolCalls {
		// Дельты аргументов в события не дробятся: SPEC 8.7 требует
		// output_item.added на первый фрагмент и полные аргументы на done.
		it := t.toolItem(fr.Index, fr.ID, fr.Function.Name)
		it.args.WriteString(fr.Function.Arguments)
	}
	if c.FinishReason != "" {
		t.finishReason = c.FinishReason
		t.closeItems()
	}
	return nil
}

// openItem возвращает последний item того же kind либо открывает новый
// (output_item.added): перемежение reasoning/content даёт разные items в
// порядке следования — как в настоящем output Responses API.
func (t *ChatToResponsesStream) openItem(kind string) *responsesOutItem {
	if n := len(t.items); n > 0 && t.items[n-1].kind == kind {
		return t.items[n-1]
	}
	it := &responsesOutItem{kind: kind, index: len(t.items)}
	t.items = append(t.items, it)
	t.emitItemAdded(it)
	return it
}

// toolItem — function_call item по chat-индексу tool_call (фрагменты одного
// вызова приходят с одним index, id/name — на первом).
func (t *ChatToResponsesStream) toolItem(index int, id, name string) *responsesOutItem {
	if it, ok := t.tools[index]; ok {
		return it
	}
	it := &responsesOutItem{kind: "function_call", index: len(t.items), callID: orDefault(id, "tool_call"), name: name}
	t.items = append(t.items, it)
	t.tools[index] = it
	t.emitItemAdded(it)
	return it
}

// closeItems — output_item.done для всех открытых items (SPEC 8.7: на
// finish_reason; идемпотентно — Finish зовёт её, если finish_reason не пришёл).
func (t *ChatToResponsesStream) closeItems() {
	if t.itemsDone {
		return
	}
	t.itemsDone = true
	for _, it := range t.items {
		t.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": it.index, "item": responsesItemMap(it, false),
		})
	}
}

func (t *ChatToResponsesStream) emitItemAdded(it *responsesOutItem) {
	t.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": it.index, "item": responsesItemMap(it, true),
	})
}

// emitDelta — дельта текста/reasoning: поля по codex-фикстурам
// (type, item_id, output_index, delta).
func (t *ChatToResponsesStream) emitDelta(event string, it *responsesOutItem, text string) {
	t.emit(event, map[string]any{
		"type": event, "item_id": it.itemID(), "output_index": it.index, "delta": text,
	})
}

// emitCreated — первый chunk: полный response-объект со статусом in_progress
// (SPEC 8.7); usage:null — форма живых codex-событий response.created.
func (t *ChatToResponsesStream) emitCreated() {
	t.emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": t.respID, "object": "response", "created_at": t.createdAt,
			"status": "in_progress", "model": t.model, "output": []any{}, "usage": nil,
		},
	})
}

// outputSoFar — output финального события: wire-форма всех items (пустой
// массив, а не null, — форма живых codex-ответов).
func (t *ChatToResponsesStream) outputSoFar() []any {
	out := make([]any, 0, len(t.items))
	for _, it := range t.items {
		out = append(out, responsesItemMap(it, false))
	}
	return out
}

// Finish закрывает стрим: response.incomplete при finish_reason length
// (incomplete_details{reason:"max_output_tokens"}), иначе response.completed —
// оба с полным usage из usage-чанка. Идемпотентна. Вызывается на [DONE];
// провайдер обязан вызвать её и после окончания чтения апстрима при обрыве
// (без [DONE]): стрим Responses обязан завершиться финальным событием — лучше
// completed с нулевым usage, чем битый поток.
func (t *ChatToResponsesStream) Finish() error {
	if t.done {
		return nil
	}
	t.done = true
	if t.writeErr != nil {
		return t.writeErr
	}
	if !t.started {
		t.started = true
		t.emitCreated() // стрим без единого чанка: финальные события всё равно уходят
	}
	t.closeItems()
	event := "response.completed"
	resp := map[string]any{
		"id": t.respID, "object": "response", "created_at": t.createdAt,
		"status": "completed", "model": t.model,
		"output": t.outputSoFar(),
		"usage":  responsesOutUsageFor(t.usage),
	}
	if t.finishReason == "length" {
		event = "response.incomplete"
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	t.emit(event, map[string]any{"type": event, "response": resp})
	return nil
}

// Fail — обрыв/ошибка апстрима: событие error и конец; response.completed НЕ
// шлётся — стрим не должен выглядеть завершённым (SPEC 8.7). Контракт пары:
// нормальный конец — Finish, обрыв апстрима — Fail.
func (t *ChatToResponsesStream) Fail(message, code string) {
	if t.done {
		return
	}
	t.done = true
	t.emit("error", map[string]any{
		"type": "error", "code": code,
		"message": orDefault(message, "upstream stream failed"),
	})
}

// Usage — накопленный normalized usage (tee, ARCHITECTURE §5): openai несёт
// usage одним чанком (choices:[]) после finish_reason, Feed копит его, финальное
// событие кладёт в response.usage, а для БД источник один — этот метод.
func (t *ChatToResponsesStream) Usage() Usage { return t.usage }

// ChatToResponses конвертирует non-stream ответ chat-апстрима в объект
// Responses API (SPEC 8.7 non-stream). model — алиас, который видел клиент.
// reasoning_content апстрима не переносится — как в ChatToAnthropic
// (SPEC 8.3); стрим-путь при этом reasoning-итемы отдаёт.
func ChatToResponses(resp *ChatResponse, model string) *ResponsesAPIResponse {
	out := &ResponsesAPIResponse{
		ID: newResponseID(), Object: "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed", Model: model,
		Output: []ResponsesOutputItem{},
		Usage:  responsesOutUsageFor(Usage{}),
	}
	if len(resp.Choices) == 0 {
		return out
	}
	ch := resp.Choices[0]
	if ch.Message.Content != "" {
		out.Output = append(out.Output, ResponsesOutputItem{
			Type: "message", ID: itemIDByIndex(len(out.Output)), Role: "assistant",
			Content: []ResponsesOutContent{{Type: "output_text", Text: ch.Message.Content}},
		})
	}
	for _, tc := range ch.Message.ToolCalls {
		out.Output = append(out.Output, ResponsesOutputItem{
			Type: "function_call", ID: itemIDByIndex(len(out.Output)),
			CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	if ch.FinishReason == "length" {
		out.Status = "incomplete"
		out.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	}
	if resp.Usage != nil {
		out.Usage = responsesOutUsageFor(usageFromChat(resp.Usage))
	}
	return out
}
