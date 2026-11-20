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

// ─── ScanSSE (few-shot sse-codex-to-openai) ─────────────────────────────────

func TestScanSSEParsesEventsMultilineDataAndIgnoresComments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []sseFixture
	}{
		{
			name: "простые события",
			in:   "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n",
			want: []sseFixture{{"a", "1"}, {"b", "2"}},
		},
		{
			name: "мультистрочный data склеивается через \\n",
			in:   "event: x\ndata: один\ndata: два\n\n",
			want: []sseFixture{{"x", "один\nдва"}},
		},
		{
			name: "комментарии и пустые строки игнорируются",
			in:   ": ping keepalive\n\nevent: y\n\n: ещё комментарий\n\n",
			want: []sseFixture{{"y", ""}},
		},
		{
			name: "событие без финальной пустой строки",
			in:   "event: z\ndata: хвост",
			want: []sseFixture{{"z", "хвост"}},
		},
		{
			name: "data без пробела после двоеточия",
			in:   "data:{\"a\":1}\n\n",
			want: []sseFixture{{"", `{"a":1}`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []sseFixture
			if err := ScanSSE(strings.NewReader(tt.in), func(ev string, data []byte) error {
				got = append(got, sseFixture{event: ev, data: string(data)})
				return nil
			}); err != nil {
				t.Fatalf("ScanSSE: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("событий %d, ожидалось %d: %+v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("событие %d: %+v, ожидалось %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestScanSSEPropagatesHandlerError(t *testing.T) {
	sentinel := errors.New("стоп")
	err := ScanSSE(strings.NewReader("event: a\ndata: 1\n\n"), func(string, []byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка обработчика должна возвращаться: %v", err)
	}
}

// ─── Клетка codex → openai (стрим, SPEC 8.4 левая таблица) ──────────────────

// Каждая фикстура → golden открытого SSE-тела: чанки, финал finish_reason +
// usage-чанк + [DONE] (TR-4), error → error-чанк + [DONE], TR-5 (done без
// дельт), length при incomplete.
func TestCodexToOpenAIStreamGolden(t *testing.T) {
	for _, name := range []string{
		"codex_text", "codex_tools", "codex_reasoning",
		"codex_done_no_deltas", "codex_incomplete", "codex_failed", "codex_error",
		"codex_error_nested",
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tr := NewCodexToOpenAI(rec)
			feedAll(t, tr, loadSSE(t, name+".sse"))
			if !tr.Finished() {
				t.Fatal("стрим не дошёл до финала")
			}
			goldenSSE(t, name+".openai.sse", rec.Body.String())
		})
	}
}

// Tee-usage (SPEC 8.6): из response.completed с деталями — все пять полей.
func TestCodexToOpenAIStreamUsageFromCompleted(t *testing.T) {
	tr := NewCodexToOpenAI(httptest.NewRecorder())
	feedAll(t, tr, loadSSE(t, "codex_text.sse"))
	want := Usage{PromptTokens: 120, CompletionTokens: 30, CachedInputTokens: 80, CacheCreationTokens: 5, ReasoningTokens: 12}
	if got := tr.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
	if tr.Err() != nil {
		t.Fatalf("Err должен быть nil: %v", tr.Err())
	}
}

// Клетка codex → anthropic (стрим, SPEC 8.4 правая таблица): message_start →
// блоки → message_delta → message_stop (TR-6), error → SSE-событие error.
func TestCodexToAnthropicStreamGolden(t *testing.T) {
	for _, name := range []string{
		"codex_text", "codex_tools", "codex_reasoning",
		"codex_done_no_deltas", "codex_incomplete", "codex_failed", "codex_error",
		"codex_error_nested",
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tr := NewCodexToAnthropic(rec)
			feedAll(t, tr, loadSSE(t, name+".sse"))
			if !tr.Finished() {
				t.Fatal("стрим не дошёл до финала")
			}
			goldenSSE(t, name+".anthropic.sse", rec.Body.String())
		})
	}
}

// ─── Non-stream агрегаторы (тот же Feed, полный объект) ─────────────────────

func TestCodexToOpenAIResponseGolden(t *testing.T) {
	for _, name := range []string{"codex_text", "codex_tools"} {
		t.Run(name, func(t *testing.T) {
			a := NewCodexToOpenAIResponse()
			feedAll(t, a, loadSSE(t, name+".sse"))
			if !a.Finished() || a.Err() != nil {
				t.Fatalf("ожидался успешный финал: finished=%v err=%v", a.Finished(), a.Err())
			}
			goldenJSON(t, name+".chat.json", a.Result())
		})
	}
}

func TestCodexToAnthropicResponseGolden(t *testing.T) {
	for _, name := range []string{"codex_text", "codex_tools"} {
		t.Run(name, func(t *testing.T) {
			a := NewCodexToAnthropicResponse()
			feedAll(t, a, loadSSE(t, name+".sse"))
			if !a.Finished() || a.Err() != nil {
				t.Fatalf("ожидался успешный финал: finished=%v err=%v", a.Finished(), a.Err())
			}
			goldenJSON(t, name+".anthropic.json", a.Result())
		})
	}
}

// response.failed → агрегатор сообщает ошибку типом StreamError (errors.As).
func TestCodexAggregatorReportsStreamError(t *testing.T) {
	a := NewCodexToOpenAIResponse()
	feedAll(t, a, loadSSE(t, "codex_failed.sse"))
	var se *StreamError
	if !errors.As(a.Err(), &se) {
		t.Fatalf("ожидали *StreamError, получили %v", a.Err())
	}
	if se.Code != "server_error" || se.Message != "внутренняя ошибка codex" {
		t.Fatalf("неожиданная ошибка: %+v", se)
	}
	if a.Result() == nil {
		t.Fatal("Result обязан возвращать объект даже при ошибке апстрима")
	}
}

// response.incomplete → stop_reason max_tokens у anthropic-агрегатора.
func TestCodexToAnthropicResponseIncompleteStopsWithMaxTokens(t *testing.T) {
	a := NewCodexToAnthropicResponse()
	feedAll(t, a, loadSSE(t, "codex_incomplete.sse"))
	if got := a.Result().StopReason; got != "max_tokens" {
		t.Fatalf("stop_reason %q, ожидался max_tokens", got)
	}
}

// ─── Клетка openai chat → anthropic (стрим, SPEC 8.5) ───────────────────────

func TestChatToAnthropicStreamGolden(t *testing.T) {
	for _, name := range []string{"chat_text", "chat_tools", "chat_reasoning", "chat_length"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tr := NewChatToAnthropic(rec)
			feedAll(t, tr, loadSSE(t, name+".sse")) // фикстуры кончаются [DONE] → финал внутри
			if !tr.done {
				t.Fatal("[DONE] должен финализировать стрим")
			}
			goldenSSE(t, name+".anthropic.sse", rec.Body.String())
		})
	}
}

// SPEC 8.5 + TR-4: usage-чанк приходит ПОСЛЕ finish_reason и должен попасть в
// message_delta; если апстрим оборвался без [DONE], финал даёт явный Finish().
// Золотой файл тот же, что у сценария с [DONE]: последовательности совпадают.
func TestChatToAnthropicStreamFinishWithoutDoneMarker(t *testing.T) {
	events := loadSSE(t, "chat_text.sse")
	rec := httptest.NewRecorder()
	tr := NewChatToAnthropic(rec)
	feedAll(t, tr, events[:len(events)-1]) // без [DONE]
	if err := tr.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := tr.Finish(); err != nil { // идемпотентность
		t.Fatalf("повторный Finish: %v", err)
	}
	goldenSSE(t, "chat_text.anthropic.sse", rec.Body.String())
}

// TR-8: маппинг стоп-причин finish_reason → stop_reason.
func TestChatToAnthropicStreamFinishReasonMapping(t *testing.T) {
	tests := []struct{ finish, want string }{
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"stop", "end_turn"},
		{"content_filter", "end_turn"}, // неизвестная → end_turn (TR-8)
	}
	for _, tt := range tests {
		t.Run(tt.finish+"→"+tt.want, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tr := NewChatToAnthropic(rec)
			chunk := `{"choices":[{"index":0,"delta":{},"finish_reason":"` + tt.finish + `"}]}`
			if err := tr.Feed("", []byte(chunk)); err != nil {
				t.Fatalf("Feed: %v", err)
			}
			if err := tr.Finish(); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if !strings.Contains(rec.Body.String(), `"stop_reason":"`+tt.want+`"`) {
				t.Fatalf("в выводе нет stop_reason %q:\n%s", tt.want, rec.Body.String())
			}
		})
	}
}

// F2: tee-usage chat→anthropic (ARCHITECTURE §5): usage-чанк приходит ПОСЛЕ
// finish_reason, Feed копит его, Usage() — единственный источник для БД.
func TestChatToAnthropicStreamUsageFromUsageChunk(t *testing.T) {
	tr := NewChatToAnthropic(httptest.NewRecorder())
	feedAll(t, tr, loadSSE(t, "chat_text.sse"))
	want := Usage{PromptTokens: 120, CompletionTokens: 30, CachedInputTokens: 80, ReasoningTokens: 12}
	if got := tr.Usage(); got != want {
		t.Fatalf("usage %+v, ожидалось %+v", got, want)
	}
}

// F4: обрыв апстрима → Fail: SSE-событие error, message_stop отсутствует —
// ошибка не должна выглядеть завершённым сообщением (SPEC 4).
func TestChatToAnthropicStreamFailEmitsErrorWithoutMessageStop(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewChatToAnthropic(rec)
	feedAll(t, tr, loadSSE(t, "chat_failed.sse")) // без [DONE] — апстрим оборвался
	tr.Fail("upstream reset", "network")
	body := rec.Body.String()
	goldenSSE(t, "chat_failed.anthropic.sse", body)
	if strings.Contains(body, "message_stop") {
		t.Fatal("после Fail message_stop быть не должно")
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatal("Fail обязан эмитить error-событие")
	}
}

// ─── Non-stream ответ chat → anthropic (few-shot translate) ─────────────────

// TR-1/TR-8: text + tool_use блоки, stop-причины, usage с cache_read;
// reasoning_content апстрима не переносится (как thinking-блоки входа, SPEC 8.3).
func TestChatToAnthropicNonStreamGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "chat_response.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	var resp ChatResponse
	if err := json.Unmarshal(in, &resp); err != nil {
		t.Fatalf("фикстура не парсится: %v", err)
	}
	out := ChatToAnthropic(&resp)
	goldenJSON(t, "chat_response.anthropic.json", out)
	if out.Content[0].Type != "text" || out.Content[1].Type != "tool_use" {
		t.Fatalf("ожидались text + tool_use блоки: %+v", out.Content)
	}
}
