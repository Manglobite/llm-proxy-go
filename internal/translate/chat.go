package translate

import (
	"encoding/json"
	"strings"
)

// ─── Выход: нормформа → OpenAI Chat Completions (SPEC 8.3) ──────────────────

// ChatRequest — тело апстрима openai-chat (glm, openai-like).
type ChatRequest struct {
	Model      string        `json:"model"`
	Messages   []ChatMessage `json:"messages"`
	Tools      []ChatTool    `json:"tools,omitempty"`
	ToolChoice any           `json:"tool_choice,omitempty"`
	MaxTokens  int           `json:"max_tokens,omitempty"`
	Temp       *float64      `json:"temperature,omitempty"`
	Stop       []string      `json:"stop,omitempty"`
	Stream     bool          `json:"stream,omitempty"`
	StreamOpts *StreamOpts   `json:"stream_options,omitempty"`
}

// StreamOpts — include_usage: просим usage-чанк в конце стрима (SPEC 7.3).
type StreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatMessage struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	ToolCalls  []ChatCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ChatFunction вынесен из ChatCall в именованный тип: он нужен и сборщику
// запроса, и стрим-агрегатору; wire-формат тот же, что в few-shot (там функция
// была анонимной структурой — без имени её не собрать литералом).
type ChatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-строка
}

type ChatCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function ChatFunction `json:"function"`
}

type ChatTool struct {
	Type     string   `json:"type"` // "function"
	Function NormTool `json:"function"`
}

// BuildChat собирает openai-chat-запрос. model — модель АПСТРИМА (подмена уже
// сделана роутером). CacheMark сознательно НЕ переносится: z.ai и openai-like
// кешируют префикс автоматически (SPEC 7.2). Thinking — anthropic-настройка,
// в openai-chat ей соответствия нет (SPEC 8.3 «thinking → отбрасывать»).
func BuildChat(norm *NormalizedRequest, model string, stream bool) *ChatRequest {
	req := &ChatRequest{
		Model: model, MaxTokens: norm.MaxTokens, Temp: norm.Temp,
		Stop: norm.Stop, Stream: stream,
	}
	if stream {
		req.StreamOpts = &StreamOpts{IncludeUsage: true} // usage-чанк в конце стрима
	}
	if norm.System != "" {
		req.Messages = append(req.Messages, ChatMessage{Role: "system", Content: norm.System})
	}
	for _, m := range norm.Messages {
		switch m.Role {
		case "tool":
			req.Messages = append(req.Messages, ChatMessage{
				Role: "tool", ToolCallID: m.ToolCallID, Content: joinText(m.Content),
			})
		case "assistant":
			cm := ChatMessage{Role: "assistant", Content: joinText(m.Content)}
			for _, tc := range m.ToolCalls {
				cm.ToolCalls = append(cm.ToolCalls, ChatCall{
					ID: tc.ID, Type: "function",
					Function: ChatFunction{Name: tc.Name, Arguments: string(tc.Arguments)},
				})
			}
			req.Messages = append(req.Messages, cm)
		default: // user
			req.Messages = append(req.Messages, ChatMessage{Role: m.Role, Content: joinText(m.Content)})
		}
	}
	for _, t := range norm.Tools {
		req.Tools = append(req.Tools, ChatTool{Type: "function", Function: t})
	}
	if norm.ToolChoice != nil {
		switch norm.ToolChoice.Mode {
		case "auto":
			req.ToolChoice = "auto"
		case "required":
			req.ToolChoice = "required"
		case "none":
			req.ToolChoice = "none"
		case "tool":
			req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": norm.ToolChoice.Name}}
		}
	}
	return req
}

// joinText склеивает текстовые части; thinking не переносится в openai-chat
// апстрим (few-shot translate). Разделитель ставится только между реальными
// частями, чтобы thinking-блок в начале не оставлял пустой перевод строки.
func joinText(parts []NormPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// ─── Ответ: OpenAI Chat → Anthropic Messages (non-stream, SPEC 8.5) ─────────

// ChatResponse — полный ответ openai-chat апстрима (интересуют choice[0] и usage).
type ChatResponse struct {
	ID      string       `json:"id"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage"`
}

type ChatChoice struct {
	Index        int                 `json:"index"`
	Message      ChatResponseMessage `json:"message"`
	FinishReason string              `json:"finish_reason"` // stop | length | tool_calls | content_filter
}

type ChatResponseMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"` // z.ai-стиль; в anthropic-выход v1 не переносится
	ToolCalls        []ChatCall `json:"tool_calls,omitempty"`
}

// ─── Форма ответа anthropic messages (выход и агрегаторы) ───────────────────

// AnthropicResponse — полный ответ /v1/messages.
type AnthropicResponse struct {
	ID         string              `json:"id"`
	Type       string              `json:"type"` // "message"
	Role       string              `json:"role"` // "assistant"
	Content    []anthropicOutBlock `json:"content"`
	StopReason string              `json:"stop_reason"` // end_turn | max_tokens | tool_use
	Usage      anthropicOutUsage   `json:"usage"`
}

// anthropicOutBlock — блок content ответа. Thinking добавлен к few-shot:
// стрим-агрегаторы отдают reasoning (z.ai-стиль) thinking-блоком.
type anthropicOutBlock struct {
	Type     string          `json:"type"` // text | tool_use | thinking
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"` // для type == "thinking"
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type anthropicOutUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// rawJSONObject приводит строку аргументов к валидному JSON-объекту: пустая
// или битая строка иначе сломала бы Marshal всего ответа целиком
// (json.RawMessage валидируется при маршалинге), а anthropic требует в input
// объект.
func rawJSONObject(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// ChatToAnthropic конвертирует non-stream ответ openai-chat апстрима в
// anthropic-ответ (few-shot translate). reasoning_content апстрима не
// переносится — так же, как thinking-блоки входа (SPEC 8.3 «thinking →
// отбрасывать»); стрим-путь при этом thinking отдаёт (SPEC 8.5).
func ChatToAnthropic(resp *ChatResponse) *AnthropicResponse {
	out := &AnthropicResponse{ID: "msg_proxy", Type: "message", Role: "assistant"}
	if len(resp.Choices) == 0 {
		out.Content = []anthropicOutBlock{{Type: "text", Text: ""}}
		return out
	}
	ch := resp.Choices[0]
	if ch.Message.Content != "" {
		out.Content = append(out.Content, anthropicOutBlock{Type: "text", Text: ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		out.Content = append(out.Content, anthropicOutBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: rawJSONObject(tc.Function.Arguments),
		})
	}
	out.StopReason = anthropicStopReason(ch.FinishReason)
	if u := resp.Usage; u != nil {
		nu := usageFromChat(u)
		out.Usage = anthropicOutUsage{
			InputTokens: nu.PromptTokens, OutputTokens: nu.CompletionTokens,
			CacheReadInputTokens: nu.CachedInputTokens,
		}
	}
	return out
}

// anthropicStopReason — маппинг стоп-причин (TR-8): length→max_tokens,
// tool_calls→tool_use, иначе end_turn.
func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}
