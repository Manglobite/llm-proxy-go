// Парсер квот codex (SPEC 7.1, AC-7): заголовки x-codex-* приходят с
// HTTP-ответом стрима (resp.Header), а НЕ в body-чанках SSE — читать их из
// тела ошибки («типовая ловушка исполнителя», REVIEW §14) нельзя. Два окна:
// primary (5-часовое) и secondary primary (недельное, SPEC именует префиксом
// x-codex-secondary-primary-*).
package codex

import (
	"encoding/json"
	"net/http"
	"strconv"

	"llm-proxy/internal/upstream"
)

// Префиксы/имена заголовков ровно из SPEC 7.1.
const (
	hdrPrimaryUsed   = "X-Codex-Primary-Used-Percent"
	hdrPrimaryWindow = "X-Codex-Primary-Window-Minutes"
	hdrPrimaryReset  = "X-Codex-Primary-Reset-At"

	hdrWeeklyUsed   = "X-Codex-Secondary-Primary-Used-Percent"
	hdrWeeklyWindow = "X-Codex-Secondary-Primary-Window-Minutes"
	hdrWeeklyReset  = "X-Codex-Secondary-Primary-Reset-At"
)

// windowFromHeaders читает тройку used/window/reset конкретного окна.
// Частичные данные не отбрасываются: процентов без reset-at достаточно для
// экрана (reset_at — украшение); битое число в одном заголовке не должно
// хоронить остальные два.
func windowFromHeaders(kind, usedH, windowH, resetH string, h http.Header) (upstream.QuotaWindow, bool) {
	var w upstream.QuotaWindow
	seen := false
	if v, err := strconv.ParseFloat(h.Get(usedH), 64); err == nil {
		w.UsedPercent = v
		seen = true
	}
	if v, err := strconv.Atoi(h.Get(windowH)); err == nil {
		w.WindowMinutes = v
		seen = true
	}
	if v, err := strconv.ParseInt(h.Get(resetH), 10, 64); err == nil {
		w.ResetAtUnix = v
		seen = true
	}
	if !seen {
		return w, false
	}
	w.Kind = kind
	return w, true
}

// ParseQuotaHeaders — квоты из заголовков ответа апстрима. nil, если ни
// одно окно не сообщено (не-codex апстрим или апстрим без квот).
func ParseQuotaHeaders(h http.Header) *upstream.Quotas {
	q := &upstream.Quotas{}
	if w, ok := windowFromHeaders(upstream.QuotaCodexPrimary, hdrPrimaryUsed, hdrPrimaryWindow, hdrPrimaryReset, h); ok {
		q.Windows = append(q.Windows, w)
	}
	if w, ok := windowFromHeaders(upstream.QuotaCodexWeekly, hdrWeeklyUsed, hdrWeeklyWindow, hdrWeeklyReset, h); ok {
		q.Windows = append(q.Windows, w)
	}
	if len(q.Windows) == 0 {
		return nil
	}
	return q
}

// ParseUsageLimitBody — 429 usage_limit_reached (SPEC 7.1, AC-8): тело
// {"error":{"type":"usage_limit_reached",...,"resets_at":...}}; resets_at
// читается и из error-объекта, и с верхнего уровня (форма OpenAI). ok=false
// для прочих 429 (не исчерпание окна — обычная ошибка апстрима).
func ParseUsageLimitBody(body []byte) (resetsAt int64, ok bool) {
	var probe struct {
		ResetsAt int64 `json:"resets_at"`
		Error    *struct {
			Type     string `json:"type"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, false
	}
	if probe.Error == nil || probe.Error.Type != "usage_limit_reached" {
		return 0, false
	}
	if probe.Error.ResetsAt != 0 {
		return probe.Error.ResetsAt, true
	}
	return probe.ResetsAt, true
}
