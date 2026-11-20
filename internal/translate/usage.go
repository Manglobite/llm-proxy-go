package translate

// ─── Usage-нормализация (SPEC 8.6, единая для всех путей) ───────────────────

// Usage — нормализованный usage (SPEC 8.6), единый для всех путей. Tee-коллектор
// транслятора копит его параллельно с эмиссией и отдаёт в accounting по
// завершении стрима (ARCHITECTURE §5) — один источник для БД.
type Usage struct {
	PromptTokens        int
	CompletionTokens    int
	CachedInputTokens   int
	CacheCreationTokens int
	ReasoningTokens     int
}

// responsesUsage — usage из событий Responses API (SPEC 8.6, строка «codex
// responses»). cache_write_tokens — это СОЗДАНИЕ кеша, а не попадание в него:
// перепутать — и кеш-статистика дашборда врёт (TR-3).
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// usageFromResponses — SPEC 8.6: cached ← input_tokens_details.cached_tokens,
// cache_creation ← input_tokens_details.cache_write_tokens, reasoning ←
// output_tokens_details.reasoning_tokens.
func usageFromResponses(u responsesUsage) Usage {
	out := Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CachedInputTokens = u.InputTokensDetails.CachedTokens
		out.CacheCreationTokens = u.InputTokensDetails.CacheWriteTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// ChatUsage — usage openai-chat (glm, openai-like); детали опциональны, у части
// апстримов их нет вовсе.
type ChatUsage struct {
	PromptTokens            int              `json:"prompt_tokens"`
	CompletionTokens        int              `json:"completion_tokens"`
	PromptTokensDetails     *ChatUsageDetail `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *ChatUsageDetail `json:"completion_tokens_details,omitempty"`
}

// ChatUsageDetail — блок деталей: оба поля в одном типе, т.к. различаются они
// только родительским ключом (prompt_tokens_details / completion_tokens_details).
type ChatUsageDetail struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// usageFromChat — SPEC 8.6, строка «glm/openai chat». nil-safe: usage-чанк
// может отсутствовать, если клиент не попросил include_usage.
func usageFromChat(u *ChatUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CachedInputTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// UsageFromChat — экспортированная обёртка usageFromChat для upstream-пакетов:
// tee-usage passthrough-путей (chat→chat) должен пользоваться ЕДИНОЙ
// нормализацией из translate, а не своей копией таблицы SPEC 8.6 (TR-3).
func UsageFromChat(u *ChatUsage) Usage { return usageFromChat(u) }

// AnthropicUsage — usage anthropic (glm anthropic passthrough, tee-парсинг).
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// usageFromAnthropic — SPEC 8.6, строка «glm anthropic»; reasoning в
// anthropic-usage нет (рассуждения живут в thinking-блоках).
func usageFromAnthropic(u *AnthropicUsage) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:        u.InputTokens,
		CompletionTokens:    u.OutputTokens,
		CachedInputTokens:   u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	}
}

// UsageFromAnthropic — экспортированная обёртка usageFromAnthropic для
// upstream-пакетов (аналог UsageFromChat): tee-usage glm-антропика обязан
// пользоваться ЕДИНОЙ нормализацией из translate, а не своей копией таблицы
// SPEC 8.6 (TR-3).
func UsageFromAnthropic(u *AnthropicUsage) Usage { return usageFromAnthropic(u) }
