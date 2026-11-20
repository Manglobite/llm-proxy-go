package translate

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Клетка responses → chat (ParseResponses → BuildChat, SPEC 8.3) ─────────

// TR-1: golden покрывает instructions → system, message-итемы (строковый и
// массив-контент), function_call → tool_calls примыканием к assistant,
// function_call_output → role:"tool", отбрасывание reasoning-итема, flattened
// tools, tool_choice строкой, max_output_tokens/temperature/reasoning.effort/
// prompt_cache_key.
func TestParseResponsesBuildChatGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "resp_full_request.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	norm, err := ParseResponses(in)
	if err != nil {
		t.Fatalf("ParseResponses: %v", err)
	}
	// Промежуточные инварианты, невидимые в golden-выходе:
	if norm.System != "Системный промпт." {
		t.Fatalf("instructions → system: %q", norm.System)
	}
	// reasoning-итем отброшен: 4 сообщения (user, assistant+tool_calls, tool, user).
	if len(norm.Messages) != 4 {
		t.Fatalf("сообщений %d, ожидалось 4 (reasoning отброшен): %+v", len(norm.Messages), norm.Messages)
	}
	if norm.Messages[1].Role != "assistant" || len(norm.Messages[1].ToolCalls) != 1 || norm.Messages[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("function_call должен примкнуть к assistant-сообщению: %+v", norm.Messages[1])
	}
	if norm.Messages[2].Role != "tool" || norm.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("function_call_output → role:\"tool\": %+v", norm.Messages[2])
	}
	if norm.MaxTokens != 512 || norm.CacheKey != "conv-42" || norm.ReasoningEffort != "high" || !norm.Stream {
		t.Fatalf("скалярные поля: max=%d key=%q effort=%q stream=%v",
			norm.MaxTokens, norm.CacheKey, norm.ReasoningEffort, norm.Stream)
	}
	out := BuildChat(norm, "glm-5.3", false)
	goldenJSON(t, "resp_full_request.chat.json", out)
}

// SPEC 8.3: строковый input — одно user-сообщение.
func TestParseResponsesStringInput(t *testing.T) {
	norm, err := ParseResponses([]byte(`{"model":"m","input":"Привет"}`))
	if err != nil {
		t.Fatalf("ParseResponses: %v", err)
	}
	if len(norm.Messages) != 1 || norm.Messages[0].Role != "user" || joinText(norm.Messages[0].Content) != "Привет" {
		t.Fatalf("строковый input должен стать user-сообщением: %+v", norm.Messages)
	}
}

// Ошибки разбора: 400 на stage translate с внятным сообщением (SPEC 4/10).
func TestParseResponsesErrors(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"previous_response_id", `{"input":"x","previous_response_id":"resp_1"}`, "previous_response_id"},
		{"битый arguments у function_call", `{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{битый"}]}`, "JSON"},
		{"неизвестный type item", `{"input":[{"type":"item_reference","id":"x"}]}`, "item_reference"},
		{"неизвестная роль message", `{"input":[{"type":"message","role":"bot","content":"x"}]}`, "роль"},
		{"неизвестная контент-часть", `{"input":[{"type":"message","role":"user","content":[{"type":"image_url"}]}]}`, "image_url"},
		{"неизвестный tool", `{"input":"x","tools":[{"type":"web_search"}]}`, "web_search"},
		{"неизвестный tool_choice", `{"input":"x","tool_choice":"может"}`, "tool_choice"},
		{"невалидный JSON", `{битый`, "responses request"},
		{"пустое тело", ``, "responses request"},
		{"нет input", `{"model":"m"}`, "input обязателен"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseResponses([]byte(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tt.want, err)
			}
		})
	}
}

// Смежность function_call: без предыдущего assistant — своё сообщение; после
// function_call_output — новое; подряд — то же assistant-сообщение.
func TestParseResponsesFunctionCallAdjacency(t *testing.T) {
	norm, err := ParseResponses([]byte(`{"input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ок"},
		{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"}]}`))
	if err != nil {
		t.Fatalf("ParseResponses: %v", err)
	}
	if len(norm.Messages) != 3 {
		t.Fatalf("сообщений %d, ожидалось 3: %+v", len(norm.Messages), norm.Messages)
	}
	for i, want := range []struct{ role, callID string }{{"assistant", "c1"}, {"tool", "c1"}, {"assistant", "c2"}} {
		m := norm.Messages[i]
		if m.Role != want.role {
			t.Fatalf("сообщение %d: роль %q, ожидалась %q", i, m.Role, want.role)
		}
		if want.role == "assistant" && (len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != want.callID) {
			t.Fatalf("сообщение %d: tool_calls %+v, ожидался %q", i, m.ToolCalls, want.callID)
		}
		if want.role == "tool" && m.ToolCallID != want.callID {
			t.Fatalf("сообщение %d: tool_call_id %q, ожидался %q", i, m.ToolCallID, want.callID)
		}
	}

	norm, err = ParseResponses([]byte(`{"input":[
		{"type":"message","role":"assistant","content":"начало"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"}]}`))
	if err != nil {
		t.Fatalf("ParseResponses: %v", err)
	}
	if len(norm.Messages) != 1 || norm.Messages[0].Role != "assistant" || len(norm.Messages[0].ToolCalls) != 2 {
		t.Fatalf("смежные function_call должны слиться в одно assistant-сообщение: %+v", norm.Messages)
	}
}

// SPEC 8.3: tool_choice объектом {"type":"function","name":...}.
func TestParseResponsesToolChoiceObject(t *testing.T) {
	norm, err := ParseResponses([]byte(`{"input":"x","tool_choice":{"type":"function","name":"get_weather"}}`))
	if err != nil {
		t.Fatalf("ParseResponses: %v", err)
	}
	if norm.ToolChoice == nil || norm.ToolChoice.Mode != "tool" || norm.ToolChoice.Name != "get_weather" {
		t.Fatalf("tool_choice: %+v", norm.ToolChoice)
	}
}

// Пустые arguments у function_call → "{}" (вызов без аргументов остаётся
// валидным объектом и в chat-wire, и в responses-wire).
func TestParseResponsesEmptyArguments(t *testing.T) {
	norm, err := ParseResponses([]byte(`{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":""}]}`))
	if err != nil {
		t.Fatalf("ParseResponses: %v", err)
	}
	if string(norm.Messages[0].ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("пустые arguments должны стать {}: %q", norm.Messages[0].ToolCalls[0].Arguments)
	}
}

// ─── Клетка chat → responses (стрим, SPEC 8.7) ──────────────────────────────

// Вход — те же chat-фикстуры, что у клетки chat→anthropic (8.5); выход —
// синтезируемые события Responses API. respID/createdAt фиксируются для
// детерминизма golden (в рантайме их даёт crypto/rand и time.Now).
func TestChatToResponsesStreamGolden(t *testing.T) {
	for _, tc := range []struct{ in, out string }{
		{"chat_text", "chat_stream_text.responses.sse"},
		{"chat_tools", "chat_stream_tools.responses.sse"},
		{"chat_reasoning", "chat_stream_reasoning.responses.sse"},
		{"chat_length", "chat_stream_length.responses.sse"}, // finish_reason length → response.incomplete
	} {
		t.Run(tc.in, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tr := NewChatToResponses(rec)
			tr.respID, tr.createdAt = "resp_test", 0
			tr.SetModel("glm-5.3")
			feedAll(t, tr, loadSSE(t, tc.in+".sse")) // фикстуры кончаются [DONE] → финал внутри
			if !tr.done {
				t.Fatal("[DONE] должен финализировать стрим")
			}
			goldenSSE(t, tc.out, rec.Body.String())
		})
	}
}

// F4: обрыв апстрима — [DONE] не приходит; финал даёт явный Fail: событие
// error и конец, response.completed отсутствует.
func TestChatToResponsesStreamFailGolden(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewChatToResponses(rec)
	tr.respID, tr.createdAt = "resp_test", 0
	tr.SetModel("glm-5.3")
	feedAll(t, tr, loadSSE(t, "chat_failed.sse")) // без [DONE]
	tr.Fail("upstream reset", "network")
	body := rec.Body.String()
	goldenSSE(t, "chat_stream_failed.responses.sse", body)
	if strings.Contains(body, "response.completed") {
		t.Fatal("после Fail response.completed быть не должно")
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatal("Fail обязан эмитить error-событие")
	}
}

// TR-4 + идемпотентность: [DONE] не пришёл — Finish даёт тот же финал, что и
// сценарий с [DONE]; повторный Finish ничего не добавляет (ровно один
// response.completed).
func TestChatToResponsesStreamFinishIdempotent(t *testing.T) {
	events := loadSSE(t, "chat_text.sse")
	rec := httptest.NewRecorder()
	tr := NewChatToResponses(rec)
	tr.respID, tr.createdAt = "resp_test", 0
	tr.SetModel("glm-5.3")
	feedAll(t, tr, events[:len(events)-1]) // без [DONE]
	if err := tr.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := tr.Finish(); err != nil {
		t.Fatalf("повторный Finish: %v", err)
	}
	goldenSSE(t, "chat_stream_text.responses.sse", rec.Body.String())
	if n := strings.Count(rec.Body.String(), "event: response.completed"); n != 1 {
		t.Fatalf("response.completed %d раз, ожидался один", n)
	}
}

// F2: tee-usage (ARCHITECTURE §5) — usage-чанк (choices:[]) приходит ПОСЛЕ
// finish_reason, Feed копит его, Usage() — единственный источник для БД.
func TestChatToResponsesStreamUsageFromUsageChunk(t *testing.T) {
	tr := NewChatToResponses(httptest.NewRecorder())
	feedAll(t, tr, loadSSE(t, "chat_text.sse"))
	want := Usage{PromptTokens: 120, CompletionTokens: 30, CachedInputTokens: 80, ReasoningTokens: 12}
	if got := tr.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// SPEC 8.7: model в response.created — из SetModel, а если не звался — из
// поля model чанка апстрима.
func TestChatToResponsesStreamModelSource(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewChatToResponses(rec)
	tr.respID, tr.createdAt = "resp_test", 0
	if err := tr.Feed("", []byte(`{"id":"c1","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"model":"glm-5.3-flash"`) {
		t.Fatalf("model из чанка должен попасть в response.created:\n%s", rec.Body.String())
	}
	// SetModel перекрывает чанк, если вызван до первого Feed.
	rec = httptest.NewRecorder()
	tr = NewChatToResponses(rec)
	tr.respID, tr.createdAt = "resp_test", 0
	tr.SetModel("work/terra")
	if err := tr.Feed("", []byte(`{"id":"c1","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"model":"work/terra"`) {
		t.Fatalf("SetModel должен приоритетно задать model:\n%s", rec.Body.String())
	}
}

// ─── Non-stream: ChatToResponses (SPEC 8.7 non-stream) ──────────────────────

// TR-1/TR-8: message + function_call items, usage input/output_tokens;
// reasoning_content апстрима не переносится (как в ChatToAnthropic, SPEC 8.3).
func TestChatToResponsesNonStreamGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "chat_response.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	var resp ChatResponse
	if err := json.Unmarshal(in, &resp); err != nil {
		t.Fatalf("фикстура не парсится: %v", err)
	}
	out := ChatToResponses(&resp, "glm-5.3")
	out.ID, out.CreatedAt = "resp_test", 0 // детерминизм golden
	goldenJSON(t, "chat_response.responses.json", out)
	if out.Status != "completed" {
		t.Fatalf("status %q, ожидался completed (finish_reason tool_calls)", out.Status)
	}
	if len(out.Output) != 2 || out.Output[0].Type != "message" || out.Output[1].Type != "function_call" {
		t.Fatalf("ожидались message + function_call items: %+v", out.Output)
	}
	if out.Output[1].Arguments != `{"city":"Москва"}` {
		t.Fatalf("arguments function_call: %q", out.Output[1].Arguments)
	}
	if out.Usage == nil || out.Usage.InputTokens != 120 || out.Usage.OutputTokens != 30 {
		t.Fatalf("usage: %+v", out.Usage)
	}
	if out.Usage.InputTokensDetails == nil || out.Usage.InputTokensDetails.CachedTokens != 80 {
		t.Fatalf("cached_tokens из prompt_tokens_details: %+v", out.Usage)
	}
}

// SPEC 8.7 non-stream: finish_reason length → status incomplete + incomplete_details.
func TestChatToResponsesNonStreamLength(t *testing.T) {
	resp := &ChatResponse{Choices: []ChatChoice{{
		Index: 0, FinishReason: "length",
		Message: ChatResponseMessage{Role: "assistant", Content: "Обрезанный"},
	}}}
	out := ChatToResponses(resp, "m")
	if out.Status != "incomplete" {
		t.Fatalf("status %q, ожидался incomplete", out.Status)
	}
	if out.IncompleteDetails == nil || out.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details: %+v", out.IncompleteDetails)
	}
}

// ─── Агрегатор codex → Responses-объект (SPEC 8.7 «codex-апстрим») ──────────

// Passthrough-подобная агрегация полной wire-формы: id/model из событий,
// output из output_item.done, usage из response.completed.
func TestCodexToResponsesResponseGolden(t *testing.T) {
	a := NewCodexToResponsesResponse()
	feedAll(t, a, loadSSE(t, "codex_tools.responses.sse"))
	if !a.Finished() || a.Err() != nil {
		t.Fatalf("ожидался успешный финал: finished=%v err=%v", a.Finished(), a.Err())
	}
	goldenJSON(t, "codex_tools.responses.json", a.Result())
}

// Минимальная форма событий (codex_text.sse): item с полным контентом должен
// прийти из output_item.done, а не из заглушки output в response.completed.
func TestCodexToResponsesResponsePrefersDoneItems(t *testing.T) {
	a := NewCodexToResponsesResponse()
	feedAll(t, a, loadSSE(t, "codex_text.sse"))
	r := a.Result()
	if r.ID != "resp_1" {
		t.Fatalf("id %q, ожидался resp_1", r.ID)
	}
	if len(r.Output) != 1 || r.Output[0].Content == nil || r.Output[0].Content[0].Text != "Привет, мир!" {
		t.Fatalf("message-итем обязан нести контент из output_item.done: %+v", r.Output)
	}
	if r.Usage == nil || r.Usage.InputTokens != 120 || r.Usage.OutputTokens != 30 {
		t.Fatalf("usage: %+v", r.Usage)
	}
	if r.Usage.InputTokensDetails == nil || r.Usage.InputTokensDetails.CachedTokens != 80 {
		t.Fatalf("cached_tokens: %+v", r.Usage)
	}
	if r.Usage.OutputTokensDetails == nil || r.Usage.OutputTokensDetails.ReasoningTokens != 12 {
		t.Fatalf("reasoning_tokens: %+v", r.Usage)
	}
}

// SPEC 8.7: response.incomplete → status incomplete + incomplete_details.
func TestCodexToResponsesResponseIncomplete(t *testing.T) {
	a := NewCodexToResponsesResponse()
	feedAll(t, a, loadSSE(t, "codex_incomplete.sse"))
	r := a.Result()
	if r.Status != "incomplete" {
		t.Fatalf("status %q, ожидался incomplete", r.Status)
	}
	if r.IncompleteDetails == nil || r.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details: %+v", r.IncompleteDetails)
	}
}

// response.failed → агрегатор сообщает ошибку типом StreamError (errors.As),
// статус объекта failed.
func TestCodexToResponsesResponseStreamError(t *testing.T) {
	a := NewCodexToResponsesResponse()
	feedAll(t, a, loadSSE(t, "codex_failed.sse"))
	var se *StreamError
	if !errors.As(a.Err(), &se) {
		t.Fatalf("ожидали *StreamError, получили %v", a.Err())
	}
	if se.Code != "server_error" || se.Message != "внутренняя ошибка codex" {
		t.Fatalf("неожиданная ошибка: %+v", se)
	}
	if got := a.Result().Status; got != "failed" {
		t.Fatalf("status %q, ожидался failed", got)
	}
}
