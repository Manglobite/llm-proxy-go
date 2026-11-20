package translate

import (
	"encoding/json"
	"fmt"
)

// ─── Вход: OpenAI Chat Completions → нормформа (SPEC 8.1) ───────────────────

// OpenAIChatRequest — тело POST /v1/chat/completions.
type OpenAIChatRequest struct {
	Model               string              `json:"model"`
	Messages            []OpenAIChatMessage `json:"messages"`
	MaxTokens           int                 `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"` // новое имя лимита у OpenAI; fallback, чтобы не терять лимит молча
	Temp                *float64            `json:"temperature,omitempty"`
	Stop                json.RawMessage     `json:"stop,omitempty"` // string | []string
	Stream              bool                `json:"stream"`
	Tools               []OpenAITool        `json:"tools,omitempty"`
	ToolChoice          json.RawMessage     `json:"tool_choice,omitempty"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string              `json:"prompt_cache_key,omitempty"`
}

// OpenAIChatMessage — элемент messages.
type OpenAIChatMessage struct {
	Role       string           `json:"role"`    // system | developer | user | assistant | tool
	Content    json.RawMessage  `json:"content"` // string | []part | null
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// OpenAIToolCall — вызов функции ассистентом во входном запросе.
type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-строка
	} `json:"function"`
}

// openAIContentPart — элемент массива content. Типы кроме text (картинки и
// т.п.) — ошибка: в нормформе v1 места им нет, молча терять контент нельзя.
type openAIContentPart struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"` // z.ai-стиль маркер кеша
}

// OpenAITool — tool-определение openai; Function сразу в нормформ-типе
// (теги NormTool совпадают с wire-формой parameters).
type OpenAITool struct {
	Type     string   `json:"type"` // "function"
	Function NormTool `json:"function"`
}

// ParseOpenAIChat разбирает openai-chat-запрос в нормформу.
func ParseOpenAIChat(body []byte) (*NormalizedRequest, error) {
	var in OpenAIChatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("openai chat request: %w", err)
	}
	norm := &NormalizedRequest{
		MaxTokens:       in.MaxTokens,
		Temp:            in.Temp,
		Stream:          in.Stream,
		ReasoningEffort: in.ReasoningEffort,
		CacheKey:        in.PromptCacheKey,
	}
	if norm.MaxTokens == 0 {
		norm.MaxTokens = in.MaxCompletionTokens
	}
	stop, err := parseStop(in.Stop)
	if err != nil {
		return nil, err
	}
	norm.Stop = stop
	for _, m := range in.Messages {
		if err := parseOpenAIMessage(norm, m); err != nil {
			return nil, err
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("openai chat: tool %q: неподдерживаемый type %q (v1 — только function)", t.Function.Name, t.Type)
		}
		norm.Tools = append(norm.Tools, t.Function)
	}
	if len(in.ToolChoice) > 0 {
		tc, err := parseOpenAIToolChoice(in.ToolChoice)
		if err != nil {
			return nil, err
		}
		norm.ToolChoice = tc
	}
	return norm, nil
}

// parseOpenAIMessage — одно сообщение в нормформу; system/developer копятся
// в norm.System (конкатенация).
func parseOpenAIMessage(norm *NormalizedRequest, m OpenAIChatMessage) error {
	parts, err := openAIContentParts(m.Content)
	if err != nil {
		return err
	}
	switch m.Role {
	case "system", "developer":
		// developer — новое имя system у OpenAI: в нормформе это одна роль.
		// cache_control в system-частях не переносится: апстрим кеширует
		// префикс сам (SPEC 7.2/8.3).
		if norm.System != "" {
			norm.System += "\n\n"
		}
		norm.System += joinText(parts)
		return nil
	case "tool":
		norm.Messages = append(norm.Messages, NormMessage{
			Role:       "tool",
			ToolCallID: m.ToolCallID,
			Content:    parts,
		})
		return nil
	case "user", "assistant":
		msg := NormMessage{Role: m.Role, Content: parts}
		for _, tc := range m.ToolCalls {
			args, err := toolCallArguments(tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				return err
			}
			msg.ToolCalls = append(msg.ToolCalls, NormToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
		}
		norm.Messages = append(norm.Messages, msg)
		return nil
	default:
		return fmt.Errorf("openai chat: неподдерживаемая роль сообщения %q", m.Role)
	}
}

// openAIContentParts разбирает content (string | []part | null) в части,
// сохраняя z.ai-маркеры кеша (TR-9: во входе они бывают внутри частей).
func openAIContentParts(raw json.RawMessage) ([]NormPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if s, ok := unmarshalString(raw); ok {
		return textParts(s, false), nil
	}
	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("openai chat content: %w", err)
	}
	out := make([]NormPart, 0, len(parts))
	for _, p := range parts {
		if p.Type != "" && p.Type != "text" {
			return nil, fmt.Errorf("openai chat: неподдерживаемый тип контент-части %q (v1 — только text)", p.Type)
		}
		out = append(out, NormPart{Type: "text", Text: p.Text, CacheMark: len(p.CacheControl) > 0})
	}
	return out, nil
}

// toolCallArguments нормализует arguments (JSON-строка) в RawMessage «как есть».
// Пустая строка → "{}": вызов без аргументов должен остаться валидным объектом
// и в chat-wire, и в responses-wire. Невалидный JSON — ошибка парсинга: апстрим
// отклонил бы её непонятной ошибкой, а здесь это честные 400 на stage translate.
func toolCallArguments(name, args string) (json.RawMessage, error) {
	if args == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(args)) {
		return nil, fmt.Errorf("openai chat: tool_call %q: arguments не являются валидным JSON", name)
	}
	return json.RawMessage(args), nil
}

// parseOpenAIToolChoice — строка (auto/required/none) или объект
// {"type":"function","function":{"name":...}}.
func parseOpenAIToolChoice(raw json.RawMessage) (*NormToolChoice, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "required", "none":
			return &NormToolChoice{Mode: s}, nil
		default:
			return nil, fmt.Errorf("openai chat: tool_choice %q не поддерживается", s)
		}
	}
	var tc struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("openai chat: tool_choice: %w", err)
	}
	if tc.Type == "function" && tc.Function.Name != "" {
		return &NormToolChoice{Mode: "tool", Name: tc.Function.Name}, nil
	}
	return nil, fmt.Errorf(`openai chat: tool_choice: ожидается {"type":"function","function":{"name":...}}`)
}

// parseStop — stop бывает строкой или массивом строк.
func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if s, ok := unmarshalString(raw); ok {
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("openai chat: stop: %w", err)
	}
	return list, nil
}
