package translate

// ─── Выход: нормформа → Responses API codex (SPEC 7.1, 8.3) ─────────────────

// ResponsesRequest — тело апстрима codex (POST /backend-api/codex/responses).
type ResponsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions,omitempty"` // system-сообщения (конкатенация)
	Input           []ResponsesInput    `json:"input"`
	Tools           []ResponsesTool     `json:"tools,omitempty"`
	ToolChoice      any                 `json:"tool_choice,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	Reasoning       *ResponsesReasoning `json:"reasoning,omitempty"`
	PromptCacheKey  string              `json:"prompt_cache_key,omitempty"` // сохранение промпт-кеша codex между запросами (SPEC 8.3)
	Store           bool                `json:"store"`                      // всегда false (SPEC 7.1)
	Stream          bool                `json:"stream"`                     // всегда true (SPEC 7.1)
}

// ResponsesInput — элемент input: message (текст), function_call (вызов
// инструмента ассистентом) или function_call_output (результат). Формы
// различаются полем type; omitempty отсекает чужие поля каждой формы.
type ResponsesInput struct {
	Type      string `json:"type"` // message | function_call | function_call_output
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // JSON-строка
	Output    string `json:"output,omitempty"`
}

// ResponsesTool — function-инструмент Responses API (flattened: name/
// description на верхнем уровне, в отличие от chat-wire).
type ResponsesTool struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"` // всегда false (SPEC 8.3)
}

// ResponsesReasoning — reasoning-настройка из reasoning_effort входа.
type ResponsesReasoning struct {
	Effort string `json:"effort"`
}

// BuildResponses собирает запрос Responses API из нормформы. model — модель
// АПСТРИМА. Сигнатура без error: все поля нормформы уже типизированы и
// валидированы при парсинге (Parse* проверяют max_tokens anthropic-входа,
// валидность arguments и т.п.), у чистой сборки нет ветвей, способных
// ошибиться, — фиктивная ошибка заставила бы вызывающего обрабатывать nil.
func BuildResponses(norm *NormalizedRequest, model string) *ResponsesRequest {
	req := &ResponsesRequest{
		Model:           model,
		MaxOutputTokens: norm.MaxTokens,
		Temperature:     norm.Temp,
		PromptCacheKey:  norm.CacheKey,
		Store:           false, // сессии codex не хранятся на стороне апстрима (SPEC 7.1)
		Stream:          true,  // codex отвечает только стримом (SPEC 7.1)
	}
	if norm.System != "" {
		req.Instructions = norm.System
	}
	if norm.ReasoningEffort != "" {
		req.Reasoning = &ResponsesReasoning{Effort: norm.ReasoningEffort}
	}
	// Thinking (anthropic-вход) сознательно не переносится: SPEC 8.3 —
	// «thinking → отбрасывать», поле reasoning маппится только из
	// reasoning_effort открытого входа.
	for _, m := range norm.Messages {
		switch m.Role {
		case "tool":
			req.Input = append(req.Input, ResponsesInput{
				Type: "function_call_output", CallID: m.ToolCallID, Output: joinText(m.Content),
			})
		case "assistant":
			if text := joinText(m.Content); text != "" {
				req.Input = append(req.Input, ResponsesInput{Type: "message", Role: "assistant", Content: text})
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, ResponsesInput{
					Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: string(tc.Arguments),
				})
			}
		default: // user
			req.Input = append(req.Input, ResponsesInput{Type: "message", Role: m.Role, Content: joinText(m.Content)})
		}
	}
	for _, t := range norm.Tools {
		req.Tools = append(req.Tools, ResponsesTool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema, Strict: false,
		})
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
			req.ToolChoice = map[string]any{"type": "function", "name": norm.ToolChoice.Name}
		}
	}
	return req
}
