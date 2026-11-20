package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Клетки матрицы SPEC 8.2 (конвертации): Parse* → Build* → golden ────────

// TR-1: клетка anthropic → openai chat. Golden покрывает multi-block system
// (ловушка REVIEW §14: конкатенация частей), tool_use↔tool_calls, tool_result →
// role:"tool" с сохранением порядка, cache_control → CacheMark (в chat-выход
// отбрасывается), tool_choice any→required, thinking → не переносится.
func TestParseAnthropicBuildChatGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "anth_full_request.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	norm, err := ParseAnthropic(in)
	if err != nil {
		t.Fatalf("ParseAnthropic: %v", err)
	}
	// Промежуточные инварианты, невидимые в golden-выходе:
	if len(norm.System) == 0 || !strings.Contains(norm.System, "по-русски") {
		t.Fatalf("multi-block system должен конкатенироваться: %q", norm.System)
	}
	if norm.Thinking == nil || !norm.Thinking.Enabled || norm.Thinking.BudgetTokens != 2048 {
		t.Fatalf("thinking должен парситься (не терять молча): %+v", norm.Thinking)
	}
	// cache_control на text-блоке user-сообщения даёт CacheMark в нормформе.
	if !norm.Messages[0].Content[0].CacheMark {
		t.Fatal("cache_control на пользовательской части должен дать CacheMark")
	}
	out := BuildChat(norm, "glm-5.3", false)
	goldenJSON(t, "anth_full_request.chat.json", out)
}

// TR-1: порядок сообщений при tool_result (обязательство few-shot):
// [text, tool_result, tool_result, text] → user, tool, tool, user.
func TestParseAnthropicToolResultBecomesSeparateToolMessages(t *testing.T) {
	norm, err := ParseAnthropic([]byte(`{"max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"text","text":"до"},
		{"type":"tool_result","tool_use_id":"t1","content":"результат 1"},
		{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"результат 2"}]},
		{"type":"text","text":"после"}
	]}]}`))
	if err != nil {
		t.Fatalf("ParseAnthropic: %v", err)
	}
	if len(norm.Messages) != 4 {
		t.Fatalf("ожидались 4 сообщения (порядок сохранён), получили %d: %+v", len(norm.Messages), norm.Messages)
	}
	want := []struct{ role, text, id string }{
		{"user", "до", ""},
		{"tool", "результат 1", "t1"},
		{"tool", "результат 2", "t2"},
		{"user", "после", ""},
	}
	for i, w := range want {
		m := norm.Messages[i]
		if m.Role != w.role {
			t.Fatalf("сообщение %d: роль %q, ожидалась %q", i, m.Role, w.role)
		}
		if w.id != "" && m.ToolCallID != w.id {
			t.Fatalf("сообщение %d: tool_call_id %q, ожидался %q", i, m.ToolCallID, w.id)
		}
		if got := joinText(m.Content); got != w.text {
			t.Fatalf("сообщение %d: текст %q, ожидался %q", i, got, w.text)
		}
	}
}

// TR-7: anthropic-вход без max_tokens → ошибка (400 на stage translate).
func TestParseAnthropicWithoutMaxTokensErrors(t *testing.T) {
	_, err := ParseAnthropic([]byte(`{"model":"m","messages":[{"role":"user","content":"привет"}]}`))
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("ожидали ошибку про max_tokens, получили: %v", err)
	}
}

// cache_control на блоках → CacheMark у NormPart; thinking-блоки → NormPart
// thinking (не теряются).
func TestParseAnthropicCacheMarksAndThinking(t *testing.T) {
	norm, err := ParseAnthropic([]byte(`{"max_tokens":16,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"thinking","text":"думаю"}]}]}`))
	if err != nil {
		t.Fatalf("ParseAnthropic: %v", err)
	}
	if !norm.Messages[0].Content[0].CacheMark {
		t.Fatal("cache_control на text-блоке должен дать CacheMark")
	}
	if norm.Messages[0].Content[1].Type != "thinking" || norm.Messages[0].Content[1].Text != "думаю" {
		t.Fatalf("thinking-блок должен сохраниться в нормформе: %+v", norm.Messages[0].Content)
	}
	// CacheMark на system-блоке отброшен — нормформа system это строка,
	// а апстримы openai-семейства кешируют префикс сами (SPEC 8.3).
	if !strings.Contains(norm.System, "s") {
		t.Fatalf("system потерян: %q", norm.System)
	}
}

// TR-1: клетка openai chat → responses (codex). Golden: instructions из system,
// input[] (message/function_call/function_call_output), tools flattened
// (strict:false), tool_choice объектом, reasoning из reasoning_effort,
// prompt_cache_key, store:false stream:true.
func TestParseOpenAIChatBuildResponsesGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "openai_full_request.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	norm, err := ParseOpenAIChat(in)
	if err != nil {
		t.Fatalf("ParseOpenAIChat: %v", err)
	}
	if !norm.Messages[0].Content[0].CacheMark {
		t.Fatal("z.ai-маркер cache_control внутри части должен дать CacheMark")
	}
	out := BuildResponses(norm, "gpt-6.1-terra")
	goldenJSON(t, "openai_full_request.responses.json", out)
}

// TR-1: клетка anthropic → responses (двухступенчато через одну нормформу,
// SPEC 8.3).
func TestParseAnthropicBuildResponsesGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "anth_full_request.json"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	norm, err := ParseAnthropic(in)
	if err != nil {
		t.Fatalf("ParseAnthropic: %v", err)
	}
	out := BuildResponses(norm, "gpt-6.1-terra")
	goldenJSON(t, "anth_full_request.responses.json", out)
}

// ─── Разбор openai-входа: детали, не видные в golden ────────────────────────

func TestParseOpenAIChatVariants(t *testing.T) {
	t.Run("tool_choice строкой", func(t *testing.T) {
		for raw, mode := range map[string]string{
			`"auto"`:     "auto",
			`"required"`: "required",
			`"none"`:     "none",
		} {
			norm, err := ParseOpenAIChat([]byte(`{"messages":[],"tool_choice":` + raw + `}`))
			if err != nil {
				t.Fatalf("tool_choice %s: %v", raw, err)
			}
			if norm.ToolChoice == nil || norm.ToolChoice.Mode != mode {
				t.Fatalf("tool_choice %s: ожидался Mode %q, получили %+v", raw, mode, norm.ToolChoice)
			}
		}
	})
	t.Run("tool_choice объектом", func(t *testing.T) {
		norm, err := ParseOpenAIChat([]byte(`{"messages":[],"tool_choice":{"type":"function","function":{"name":"f"}}}`))
		if err != nil {
			t.Fatalf("ParseOpenAIChat: %v", err)
		}
		if norm.ToolChoice == nil || norm.ToolChoice.Mode != "tool" || norm.ToolChoice.Name != "f" {
			t.Fatalf("ожидался Mode tool/name f, получили %+v", norm.ToolChoice)
		}
	})
	t.Run("stop строкой и массивом", func(t *testing.T) {
		norm, err := ParseOpenAIChat([]byte(`{"messages":[],"stop":"END"}`))
		if err != nil || len(norm.Stop) != 1 || norm.Stop[0] != "END" {
			t.Fatalf("stop строкой: %v %+v", err, norm.Stop)
		}
		norm, err = ParseOpenAIChat([]byte(`{"messages":[],"stop":["A","B"]}`))
		if err != nil || len(norm.Stop) != 2 {
			t.Fatalf("stop массивом: %v %+v", err, norm.Stop)
		}
	})
	t.Run("max_completion_tokens как fallback", func(t *testing.T) {
		norm, err := ParseOpenAIChat([]byte(`{"messages":[],"max_completion_tokens":77}`))
		if err != nil || norm.MaxTokens != 77 {
			t.Fatalf("ожидался MaxTokens 77: %v %d", err, norm.MaxTokens)
		}
	})
	t.Run("developer роль = system", func(t *testing.T) {
		norm, err := ParseOpenAIChat([]byte(`{"messages":[{"role":"developer","content":"d"},{"role":"system","content":"s"}]}`))
		if err != nil || norm.System != "d\n\ns" {
			t.Fatalf("system-конкатенация: %v %q", err, norm.System)
		}
	})
	t.Run("невалидные arguments — ошибка", func(t *testing.T) {
		_, err := ParseOpenAIChat([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{битый"}}]}]}`))
		if err == nil || !strings.Contains(err.Error(), "JSON") {
			t.Fatalf("ожидали ошибку валидности arguments: %v", err)
		}
	})
	t.Run("пустые arguments → {}", func(t *testing.T) {
		norm, err := ParseOpenAIChat([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":""}}]}]}`))
		if err != nil {
			t.Fatalf("ParseOpenAIChat: %v", err)
		}
		if string(norm.Messages[0].ToolCalls[0].Arguments) != "{}" {
			t.Fatalf("пустые arguments должны стать {}: %q", norm.Messages[0].ToolCalls[0].Arguments)
		}
	})
	t.Run("неизвестная контент-часть — ошибка", func(t *testing.T) {
		_, err := ParseOpenAIChat([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`))
		if err == nil || !strings.Contains(err.Error(), "image_url") {
			t.Fatalf("ожидали ошибку о неподдерживаемой части: %v", err)
		}
	})
	t.Run("content null у ассистента с tool_calls", func(t *testing.T) {
		norm, err := ParseOpenAIChat([]byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`))
		if err != nil {
			t.Fatalf("ParseOpenAIChat: %v", err)
		}
		if len(norm.Messages[0].ToolCalls) != 1 {
			t.Fatalf("tool_calls должны сохраниться: %+v", norm.Messages[0])
		}
	})
}

// BuildChat: stream=true добавляет stream_options.include_usage (SPEC 7.3),
// stream=false — не добавляет; ToolChoice none проходит в оба wire-формата.
func TestBuildChatStreamAndNoneToolChoice(t *testing.T) {
	norm := &NormalizedRequest{ToolChoice: &NormToolChoice{Mode: "none"}}
	req := BuildChat(norm, "m", true)
	if req.StreamOpts == nil || !req.StreamOpts.IncludeUsage {
		t.Fatalf("stream=true должен включать include_usage: %+v", req.StreamOpts)
	}
	if req.ToolChoice != "none" {
		t.Fatalf("tool_choice none: %+v", req.ToolChoice)
	}
	req = BuildChat(norm, "m", false)
	if req.StreamOpts != nil {
		t.Fatalf("stream=false не должен добавлять stream_options: %+v", req.StreamOpts)
	}
}

// ─── Passthrough-подмена модели (TR-9) ──────────────────────────────────────

// passthrough-клетки SPEC 8.2: заменяется ровно ключ model, всё остальное —
// байт-в-байт; z.ai-маркеры cache_control внутри content-частей и cache_control
// на anthropic-блоках переживают подмену нетронутыми.
func TestRewriteModelChangesOnlyModelField(t *testing.T) {
	openai := `{"model":"work/terra","messages":[
		{"role":"user","content":[{"type":"text","text":"привет","cache_control":{"type":"ephemeral"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]}],
		"tool_choice":"auto","prompt_cache_key":"conv-1"}`
	anthropic := `{"model":"glm/5.3","max_tokens":64,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"r","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"вопрос"}]}]}`

	for name, tc := range map[string]struct{ body, want string }{
		"openai c z.ai-маркерами":   {openai, "glm-5.3"},
		"anthropic c cache_control": {anthropic, "glm-5.3"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := RewriteModel([]byte(tc.body), tc.want)
			if err != nil {
				t.Fatalf("RewriteModel: %v", err)
			}
			var before, after map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.body), &before); err != nil {
				t.Fatalf("исходник не парсится: %v", err)
			}
			if err := json.Unmarshal(out, &after); err != nil {
				t.Fatalf("результат не парсится: %v", err)
			}
			if len(before) != len(after) {
				t.Fatalf("число полей изменилось: %d → %d", len(before), len(after))
			}
			changed := 0
			for k, v := range before {
				if k == "model" {
					var got string
					if err := json.Unmarshal(after[k], &got); err != nil || got != tc.want {
						t.Fatalf("model: %s (ожидалось %q)", after[k], tc.want)
					}
					changed++
					continue
				}
				// Значения сравниваем в компактированном виде: Marshal компактирует
				// RawMessage (убирает insignificant whitespace вне строк), семантика
				// «байт-в-байт» — про отсутствие перекодирования содержимого.
				var compacted bytes.Buffer
				if err := json.Compact(&compacted, v); err != nil {
					t.Fatalf("компакт исходного %q: %v", k, err)
				}
				if compacted.String() != string(after[k]) {
					t.Fatalf("поле %q изменилось:\nбыло  %s\nстало %s", k, compacted.String(), after[k])
				}
			}
			if changed != 1 {
				t.Fatalf("должно меняться ровно одно поле, изменилось %d", changed)
			}
			// и сами маркеры кеша физически присутствуют в выводе
			if !strings.Contains(string(out), "cache_control") {
				t.Fatal("cache_control должен пережить подмену байт-в-байт")
			}
		})
	}
}

func TestRewriteModelInvalidBodyErrors(t *testing.T) {
	if _, err := RewriteModel([]byte("{битый"), "m"); err == nil {
		t.Fatal("ожидали ошибку на невалидном теле")
	}
}

// F1 (SPEC 7.3/8.2): passthrough chat→chat — в стрим-запрос инжектится
// stream_options.include_usage; non-stream и уже включённый вариант —
// байт-в-байт нетронутыми; не-объектный stream_options — ошибка.
func TestInjectIncludeUsageVariants(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string // ожидаемый компактный JSON; "" — байт-в-байт тот же
		wantErr bool
	}{
		{
			name: "нет stream_options — вставлен",
			body: `{"model":"m","stream":true}`,
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "пустой stream_options — влит include_usage",
			body: `{"stream":true,"stream_options":{}}`,
			want: `{"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "stream_options null — заменён",
			body: `{"stream":true,"stream_options":null}`,
			want: `{"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "уже включён — тело не изменено",
			body: `{"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "non-stream — тело не изменено",
			body: `{"model":"m","stream":false}`,
		},
		{
			name: "stream отсутствует — тело не изменено",
			body: `{"model":"m"}`,
		},
		{
			name:    "не-объектный stream_options — ошибка",
			body:    `{"stream":true,"stream_options":"да"}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InjectIncludeUsage([]byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ожидали ошибку, получили %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("InjectIncludeUsage: %v", err)
			}
			if tt.want == "" {
				if string(got) != tt.body {
					t.Fatalf("тело должно остаться байт-в-байт:\nбыло  %s\nстало %s", tt.body, got)
				}
				return
			}
			if string(got) != tt.want {
				t.Fatalf("результат %s, ожидалось %s", got, tt.want)
			}
		})
	}
}

// F3: симметрия с openai-входом — неизвестный тип контент-блока сообщения —
// ошибка, а не молчаливая потеря контента.
func TestParseAnthropicUnknownBlockTypeErrors(t *testing.T) {
	_, err := ParseAnthropic([]byte(`{"max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"text","text":"до"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported block type") {
		t.Fatalf("ожидали ошибку unsupported block type: %v", err)
	}
}

// F3: не-text блоки внутри tool_result.content — тоже ошибка (текст результата
// иначе потерялся бы молча).
func TestParseAnthropicToolResultNonTextContentErrors(t *testing.T) {
	_, err := ParseAnthropic([]byte(`{"max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported block type") {
		t.Fatalf("ожидали ошибку unsupported block type: %v", err)
	}
}
