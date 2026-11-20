package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ─── Вход: Responses API → нормформа (SPEC 4, 8.3) ──────────────────────────

// ResponsesAPIRequest — тело POST /v1/responses (вход клиента; не путать с
// ResponsesRequest — телом апстрима codex, которое собирает BuildResponses).
type ResponsesAPIRequest struct {
	Model              string              `json:"model"`
	Instructions       string              `json:"instructions,omitempty"`
	Input              json.RawMessage     `json:"input"` // string | []item
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	Temp               *float64            `json:"temperature,omitempty"`
	Stream             bool                `json:"stream"`
	Tools              []ResponsesTool     `json:"tools,omitempty"`
	ToolChoice         json.RawMessage     `json:"tool_choice,omitempty"` // строка | {"type":"function","name":...}
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
}

// ResponsesAPIItem — элемент input: форма различается полем Type
// (message | function_call | function_call_output | reasoning). Content и
// Output бывают строкой или массивом контент-частей — RawMessage, ветку
// выбирает попытка разбора (как content у AnthropicMessage).
type ResponsesAPIItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // message: string | []part
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"` // JSON-строка
	Output    json.RawMessage `json:"output,omitempty"`    // function_call_output: string | []part
}

// responsesInPart — контент-часть message/output (SPEC 8.3: только текстовые
// input_text/output_text).
type responsesInPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseResponses разбирает responses-запрос в нормформу (SPEC 4, 8.3).
// previous_response_id непустой — ошибка: прокси stateless, история приходит
// в input[] целиком (так работает и сам codex CLI с store:false); reasoning-
// items отбрасываются — симметрично «thinking → отбрасывать» в 8.3.
func ParseResponses(body []byte) (*NormalizedRequest, error) {
	var in ResponsesAPIRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("responses request: %w", err)
	}
	if in.PreviousResponseID != "" {
		return nil, errors.New("responses request: previous_response_id не поддерживается: прокси stateless — история должна приходить в input[] целиком (SPEC 4)")
	}
	norm := &NormalizedRequest{
		System:    in.Instructions,
		MaxTokens: in.MaxOutputTokens,
		Temp:      in.Temp,
		Stream:    in.Stream,
		CacheKey:  in.PromptCacheKey,
	}
	if in.Reasoning != nil {
		norm.ReasoningEffort = in.Reasoning.Effort
	}
	if len(in.Input) == 0 || string(in.Input) == "null" {
		return nil, errors.New("responses request: input обязателен (строка или массив items)")
	}
	if s, ok := unmarshalString(in.Input); ok {
		// Строковый input — одно user-сообщение (SPEC 8.3).
		norm.Messages = append(norm.Messages, NormMessage{Role: "user", Content: textParts(s, false)})
	} else {
		var items []ResponsesAPIItem
		if err := json.Unmarshal(in.Input, &items); err != nil {
			return nil, fmt.Errorf("responses input: %w", err)
		}
		for _, it := range items {
			if err := parseResponsesItem(norm, it); err != nil {
				return nil, err
			}
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("responses request: tool %q: неподдерживаемый type %q (v1 — только function)", t.Name, t.Type)
		}
		norm.Tools = append(norm.Tools, NormTool{Name: t.Name, Description: t.Description, Schema: t.Parameters})
	}
	if len(in.ToolChoice) > 0 {
		tc, err := parseResponsesToolChoice(in.ToolChoice)
		if err != nil {
			return nil, err
		}
		norm.ToolChoice = tc
	}
	return norm, nil
}

// parseResponsesItem — один item input[] в нормформу. system/developer-роли
// message копятся в norm.System (конкатенация, как в openai-входе).
func parseResponsesItem(norm *NormalizedRequest, it ResponsesAPIItem) error {
	switch it.Type {
	case "message":
		text, err := responsesContentText(it.Content, "message content")
		if err != nil {
			return err
		}
		switch it.Role {
		case "system", "developer":
			if norm.System != "" {
				norm.System += "\n\n"
			}
			norm.System += text
			return nil
		case "user", "assistant":
			norm.Messages = append(norm.Messages, NormMessage{Role: it.Role, Content: textParts(text, false)})
			return nil
		default:
			return fmt.Errorf("responses input: message: неподдерживаемая роль %q", it.Role)
		}

	case "function_call":
		args, err := responsesCallArguments(it.Name, it.Arguments)
		if err != nil {
			return err
		}
		// Вызов примыкает к предыдущему assistant-сообщению (mixed-история
		// codex CLI идёт сплошными assistant-ходами); иначе — новое сообщение.
		if n := len(norm.Messages); n > 0 && norm.Messages[n-1].Role == "assistant" {
			norm.Messages[n-1].ToolCalls = append(norm.Messages[n-1].ToolCalls,
				NormToolCall{ID: it.CallID, Name: it.Name, Arguments: args})
			return nil
		}
		norm.Messages = append(norm.Messages, NormMessage{
			Role:      "assistant",
			ToolCalls: []NormToolCall{{ID: it.CallID, Name: it.Name, Arguments: args}},
		})
		return nil

	case "function_call_output":
		text, err := responsesContentText(it.Output, "function_call_output output")
		if err != nil {
			return err
		}
		norm.Messages = append(norm.Messages, NormMessage{
			Role: "tool", ToolCallID: it.CallID, Content: textParts(text, false),
		})
		return nil

	case "reasoning":
		// SPEC 4/8.3: reasoning-items в chat-апстрим не переносятся, dropping
		// сохраняет смежность assistant-ходов (function_call остаётся рядом
		// со своим output даже после отбрасывания).
		return nil

	default:
		return fmt.Errorf("responses input: неподдерживаемый type %q", it.Type)
	}
}

// responsesContentText — текст из content/output (строка | []part). Не-текст
// части — ошибка: в нормформе v1 места им нет, молча терять контент нельзя
// (симметрия с openai/anthropic-входами).
func responsesContentText(raw json.RawMessage, what string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if s, ok := unmarshalString(raw); ok {
		return s, nil
	}
	var parts []responsesInPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("responses %s: %w", what, err)
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" {
			return "", fmt.Errorf("responses %s: неподдерживаемый тип контент-части %q (v1 — только input_text/output_text)", what, p.Type)
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

// responsesCallArguments — тот же контракт, что toolCallArguments (openai_in.go):
// пустая строка → "{}", невалидный JSON → ошибка. Свой префикс: 400 на stage
// translate должен называть входной протокол, а не «openai chat».
func responsesCallArguments(name, args string) (json.RawMessage, error) {
	raw, err := toolCallArguments(name, args)
	if err != nil {
		return nil, fmt.Errorf("responses input: function_call %q: arguments не являются валидным JSON", name)
	}
	return raw, nil
}

// parseResponsesToolChoice — строка (auto/required/none) или flattened-объект
// Responses API {"type":"function","name":...}.
func parseResponsesToolChoice(raw json.RawMessage) (*NormToolChoice, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "required", "none":
			return &NormToolChoice{Mode: s}, nil
		default:
			return nil, fmt.Errorf("responses request: tool_choice %q не поддерживается", s)
		}
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("responses request: tool_choice: %w", err)
	}
	if tc.Type == "function" && tc.Name != "" {
		return &NormToolChoice{Mode: "tool", Name: tc.Name}, nil
	}
	return nil, fmt.Errorf(`responses request: tool_choice: ожидается строка или {"type":"function","name":...}`)
}
