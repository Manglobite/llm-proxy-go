package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ─── Вход: Anthropic Messages → нормформа (SPEC 8.3) ────────────────────────

// AnthropicRequest — тело POST /v1/messages.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"` // string | []block
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temp          *float64           `json:"temperature,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
}

// AnthropicMessage — элемент messages: content бывает string или массив блоков.
type AnthropicMessage struct {
	Role    string          `json:"role"` // user | assistant
	Content json.RawMessage `json:"content"`
}

// AnthropicTool — tool-определение anthropic (input_schema вместо parameters).
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // enabled | disabled
	BudgetTokens int    `json:"budget_tokens"`
}

// anthropicBlock — блок content; набор значимых полей зависит от Type.
type anthropicBlock struct {
	Type  string          `json:"type"` // text | tool_use | tool_result | thinking
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result:
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string | []block
	IsError   bool            `json:"is_error,omitempty"`
	Cache     json.RawMessage `json:"cache_control,omitempty"`
}

// ParseAnthropic разбирает anthropic-запрос в нормформу.
// max_tokens <= 0 — ошибка: требование протокола Anthropic (SPEC 4, TR-7),
// молчаливый ноль превратился бы в неограниченный ответ на апстриме.
func ParseAnthropic(body []byte) (*NormalizedRequest, error) {
	var in AnthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if in.MaxTokens <= 0 {
		return nil, errors.New("anthropic request: max_tokens обязателен и должен быть положительным")
	}
	norm := &NormalizedRequest{
		MaxTokens: in.MaxTokens, Temp: in.Temp,
		Stop: in.StopSequences, Stream: in.Stream,
	}
	if in.Thinking != nil && in.Thinking.Type == "enabled" {
		norm.Thinking = &NormThinking{Enabled: true, BudgetTokens: in.Thinking.BudgetTokens}
	}
	if len(in.System) > 0 {
		if s, ok := unmarshalString(in.System); ok {
			norm.System = s
		} else {
			var blocks []anthropicBlock
			if err := json.Unmarshal(in.System, &blocks); err != nil {
				return nil, fmt.Errorf("anthropic system: %w", err)
			}
			parts := make([]string, 0, len(blocks))
			for _, b := range blocks {
				parts = append(parts, b.Text)
			}
			norm.System = strings.Join(parts, "\n\n")
			// cache_control на system сознательно отброшен: openai-семейство
			// апстримов кеширует префикс само (SPEC 8.3).
		}
	}
	for _, t := range in.Tools {
		norm.Tools = append(norm.Tools, NormTool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	if len(in.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"` // auto | any | tool | none
			Name string `json:"name,omitempty"`
		}
		if err := json.Unmarshal(in.ToolChoice, &tc); err == nil {
			switch tc.Type {
			case "auto":
				norm.ToolChoice = &NormToolChoice{Mode: "auto"}
			case "any":
				norm.ToolChoice = &NormToolChoice{Mode: "required"}
			case "tool":
				norm.ToolChoice = &NormToolChoice{Mode: "tool", Name: tc.Name}
			case "none":
				norm.ToolChoice = &NormToolChoice{Mode: "none"}
			}
		}
	}
	for _, m := range in.Messages {
		msgs, err := parseAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		norm.Messages = append(norm.Messages, msgs...)
	}
	return norm, nil
}

// parseAnthropicMessage разворачивает одно сообщение в 0..N нормформ-сообщений:
// tool_result-блоки становятся ОТДЕЛЬНЫМИ role:"tool" сообщениями (обязательство
// few-shot translate), порядок обхода блоков сохраняется — текстовые части
// накапливаются «между» tool_result и отдёргиваются перед ними.
func parseAnthropicMessage(m AnthropicMessage) ([]NormMessage, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return []NormMessage{{Role: m.Role}}, nil
	}
	if s, ok := unmarshalString(m.Content); ok {
		return []NormMessage{{Role: m.Role, Content: []NormPart{{Type: "text", Text: s}}}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("anthropic message %s: %w", m.Role, err)
	}
	out := make([]NormMessage, 0, len(blocks))
	var pending *NormMessage // накопитель текстовых блоков между tool_result
	flush := func() {
		if pending != nil {
			out = append(out, *pending)
			pending = nil
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if pending == nil {
				pending = &NormMessage{Role: m.Role}
			}
			pending.Content = append(pending.Content, NormPart{Type: "text", Text: b.Text, CacheMark: len(b.Cache) > 0})
		case "thinking":
			if pending == nil {
				pending = &NormMessage{Role: m.Role}
			}
			pending.Content = append(pending.Content, NormPart{Type: "thinking", Text: b.Text})
		case "tool_use": // в assistant-сообщении
			if pending == nil {
				pending = &NormMessage{Role: m.Role}
			}
			pending.ToolCalls = append(pending.ToolCalls, NormToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input})
		case "tool_result":
			// Результат инструмента — отдельное сообщение role:"tool";
			// is_error в v1 игнорируется (текст передаём как есть, SPEC 8.3).
			text, err := toolResultText(b.Content)
			if err != nil {
				return nil, fmt.Errorf("anthropic tool_result %s: %w", b.ToolUseID, err)
			}
			flush()
			out = append(out, NormMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    textParts(text, len(b.Cache) > 0),
			})
		default:
			// Симметрия с openai-входом: не-text контент (image, document, ...)
			// в нормформе v1 не представим — ошибка, а не молчаливая потеря.
			return nil, fmt.Errorf("anthropic message %s: unsupported block type %q", m.Role, b.Type)
		}
	}
	flush()
	return out, nil
}

// toolResultText извлекает текст из tool_result.content (string | []block):
// блоки склеиваются "\n\n" — так же, как multi-block system.
func toolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if s, ok := unmarshalString(raw); ok {
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		// Результат инструмента идёт в role:"tool" строкой — не-text блок
		// внутри потерял бы содержимое молча; ошибка честнее (F3).
		if b.Type != "text" {
			return "", fmt.Errorf("unsupported block type %q в tool_result.content", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}
