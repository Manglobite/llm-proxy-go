// Пакет internal/translate — нормформа запросов и конвертации протоколов
// LLM-провайдеров (SPEC 8). Правило R2 (ARCHITECTURE §2): чистые функции —
// только stdlib, ни сети, ни HTTP-сервера, ни глобального состояния; единственное
// исключение — приём http.ResponseWriter в SSE-трансляторах (few-shot sse).
package translate

import "encoding/json"

// NormalizedRequest — единое внутреннее представление запроса (SPEC 8.1).
// Любой вход разбирается в него (ParseAnthropic / ParseOpenAIChat), из него
// собирается любой апстрим (BuildChat / BuildResponses).
type NormalizedRequest struct {
	System          string // конкатенация system-частей (multi-block — через "\n\n")
	Messages        []NormMessage
	Tools           []NormTool
	ToolChoice      *NormToolChoice
	MaxTokens       int
	Temp            *float64
	Stop            []string
	Stream          bool
	ReasoningEffort string        // reasoning_effort из openai-входа; строка как есть — валидность решает апстрим
	Thinking        *NormThinking // thinking из anthropic-входа; билдеры v1 отбрасывают (SPEC 8.3), парсим чтобы не терять молча
	CacheKey        string        // prompt_cache_key клиента (только openai-вход; у anthropic такого поля нет)
}

// NormThinking — настройка рассуждений anthropic-входа
// {"type":"enabled","budget_tokens":N}.
type NormThinking struct {
	Enabled      bool
	BudgetTokens int
}

// NormMessage — одно сообщение. Role "tool" — результат вызова инструмента
// (anthropic tool_result / openai role:"tool").
type NormMessage struct {
	Role       string     // "user" | "assistant" | "tool"
	Content    []NormPart // текстовые части
	ToolCalls  []NormToolCall
	ToolCallID string // для Role == "tool"
}

// NormPart — часть контента. Type "thinking" — рассуждения: они сохраняются
// в нормформе, но в openai-chat апстрим не переносятся (few-shot translate).
type NormPart struct {
	Type      string // "text" | "thinking"
	Text      string
	CacheMark bool // у блока-источника был cache_control (anthropic) или z.ai-маркер
}

// NormTool — инструмент. Теги совпадают с openai-wire (name/description/
// parameters), чтобы значение вставало в ChatTool без перекладывания полей:
// few-shot использует NormTool прямо в ChatTool.Function.
type NormTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"parameters,omitempty"` // JSON Schema
}

// NormToolCall — вызов инструмента ассистентом. Arguments — сырой JSON-объект
// «как есть»: перекодирование нарушило бы порядок ключей и экранирование.
type NormToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// NormToolChoice — принуждение к выбору инструмента.
type NormToolChoice struct {
	Mode string // "auto" | "required" | "none" | "tool"
	Name string // для Mode == "tool"
}

// unmarshalString пытается прочитать RawMessage как JSON-строку: поля content
// и system бывают string | []block, выбор ветки делается попыткой.
func unmarshalString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// textParts — одна текстовая часть; пустой текст без кеш-маркера частей не
// создаёт, чтобы не плодить пустые сообщения в нормформе.
func textParts(text string, cacheMark bool) []NormPart {
	if text == "" && !cacheMark {
		return nil
	}
	return []NormPart{{Type: "text", Text: text, CacheMark: cacheMark}}
}
