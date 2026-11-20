// Конверты ошибок (SPEC 4): тело ошибки — ВСЕГДА JSON в формате протокола
// входа, никогда HTML (opencode не умеет показывать HTML-ошибки из SSE).
package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"llm-proxy/internal/upstream"
)

// openaiError — конверт OpenAI-style {"error":{"message","type","code"}}.
type openaiError struct {
	Error openaiErrorBody `json:"error"`
}

type openaiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// anthropicError — конверт Anthropic-style
// {"type":"error","error":{"type","message"}}.
type anthropicError struct {
	Type  string             `json:"type"` // всегда "error"
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// openaiErrType — значение type по статусу (формы ответов OpenAI API).
func openaiErrType(status int) string {
	if status < 500 {
		return "invalid_request_error" // 400/401/404/413 — ошибка запроса
	}
	return "api_error" // 5xx — ошибка нашей/апстрим-стороны
}

// anthropicErrType — значение error.type по статусу (формы ответов Anthropic).
func anthropicErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// errorCodeForKind — code в openai-конверте из error_kind таксономии.
func errorCodeForKind(kind string) string {
	switch kind {
	case upstream.KindSocksBlocked:
		return "socks_blocked"
	case upstream.KindNetwork:
		return "network_error"
	case upstream.KindUpstream:
		return "upstream_error"
	case upstream.KindTranslate:
		return "invalid_request"
	case upstream.KindAuth:
		return "auth_error"
	default:
		return "internal_error"
	}
}

// writeError пишет JSON-конверт ошибки в формате протокола входа.
// extra — заголовки для проброса клиенту (retry-after/x-ratelimit-*, SPEC 4).
func writeError(w http.ResponseWriter, protocol string, status int, code, message string, extra http.Header) {
	h := w.Header()
	for k, vs := range extra {
		h[k] = vs
	}
	h.Set("Content-Type", "application/json")

	var body []byte
	switch protocol {
	case upstream.ProtocolAnthropic:
		body, _ = json.Marshal(anthropicError{
			Type: "error",
			Error: anthropicErrorBody{
				Type:    anthropicErrType(status),
				Message: message,
			},
		})
	case upstream.ProtocolResponses:
		// Responses API использует ту же openai-форму {"error":{message,type,code}}
		// (SPEC 8.7: «один конверт с /v1/chat/completions») — отдельная ветка
		// фиксирует это намерение: формы Responses-ошибок не «просто openai по
		// умолчанию», а выбор протокола.
		body, _ = json.Marshal(openaiError{Error: openaiErrorBody{
			Message: message,
			Type:    openaiErrType(status),
			Code:    code,
		}})
	default: // openai
		body, _ = json.Marshal(openaiError{Error: openaiErrorBody{
			Message: message,
			Type:    openaiErrType(status),
			Code:    code,
		}})
	}
	// Content-Length явно: после Flush net/http переходит на chunked-кодировку
	// и терминальный chunk уходит только при возврате хендлера, а следом идут
	// записи в SQLite (busy_timeout до 5s на INSERT) — без явной длины клиент
	// ждал бы завершения хендлера, хотя конверт уже готов (F5).
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	// Ошибку записи конверта не вернуть клиенту нечем — остаётся факт статуса.
	_, _ = w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// rateLimitPrefixes — префиксы заголовков апстрима, пробрасываемые клиенту
// (SPEC 4: opencode их читает). Сравнение в нижнем регистре.
var rateLimitPrefixes = []string{
	"retry-after", // точное имя и префикс retry-after-ms
	"x-ratelimit-",
	"anthropic-ratelimit-",
}

// filterRateLimitHeaders выбирает из заголовков апстрима те, что положено
// пробросить клиенту. nil-safe: на успехе/блокировке апстрим-заголовков нет.
func filterRateLimitHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := http.Header{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		for _, p := range rateLimitPrefixes {
			if strings.HasPrefix(lk, p) {
				out[k] = append([]string(nil), vs...)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
