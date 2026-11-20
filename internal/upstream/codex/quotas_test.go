package codex

// Прямые юнит-тесты парсера квот (AC-7/AC-8) на фикстурах заголовков:
// оба окна (primary/secondary-primary), частичные данные, 429-тела.

import (
	"net/http"
	"testing"

	"llm-proxy/internal/upstream"
)

func hdr(kv map[string]string) http.Header {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return h
}

func TestParseQuotaHeadersBothWindows(t *testing.T) {
	h := hdr(map[string]string{
		hdrPrimaryUsed:   "42.5",
		hdrPrimaryWindow: "300",
		hdrPrimaryReset:  "1730003600",
		hdrWeeklyUsed:    "12",
		hdrWeeklyWindow:  "10080",
		hdrWeeklyReset:   "1730870000",
	})
	q := ParseQuotaHeaders(h)
	if q == nil || len(q.Windows) != 2 {
		t.Fatalf("Quotas = %+v", q)
	}
	p, w := q.Windows[0], q.Windows[1]
	if p.Kind != upstream.QuotaCodexPrimary || p.UsedPercent != 42.5 || p.WindowMinutes != 300 || p.ResetAtUnix != 1730003600 {
		t.Fatalf("primary = %+v", p)
	}
	// Вторичное окно — префикс x-codex-secondary-primary-* (SPEC 7.1),
	// kind в БД — codex_weekly.
	if w.Kind != upstream.QuotaCodexWeekly || w.UsedPercent != 12 || w.WindowMinutes != 10080 || w.ResetAtUnix != 1730870000 {
		t.Fatalf("weekly = %+v", w)
	}
}

func TestParseQuotaHeadersPartialDataKept(t *testing.T) {
	// Процентов без reset-at достаточно для экрана — частичное окно не теряется.
	h := hdr(map[string]string{hdrPrimaryUsed: "7"})
	q := ParseQuotaHeaders(h)
	if q == nil || len(q.Windows) != 1 || q.Windows[0].UsedPercent != 7 || q.Windows[0].ResetAtUnix != 0 {
		t.Fatalf("частичное окно потеряно: %+v", q)
	}
}

func TestParseQuotaHeadersAbsentIsNil(t *testing.T) {
	if q := ParseQuotaHeaders(hdr(map[string]string{"Content-Type": "text/event-stream"})); q != nil {
		t.Fatalf("без x-codex-* заголовков Quotas = %+v, ожидался nil", q)
	}
	if q := ParseQuotaHeaders(hdr(map[string]string{
		hdrPrimaryUsed:   "не-число",
		hdrPrimaryWindow: "300",
	})); q == nil || len(q.Windows) != 1 || q.Windows[0].WindowMinutes != 300 {
		t.Fatalf("битое число хоронит остальные заголовки: %+v", q)
	}
}

func TestParseUsageLimitBody429(t *testing.T) {
	resets, ok := ParseUsageLimitBody([]byte(`{"error":{"type":"usage_limit_reached","resets_at":1730009999,"message":"limit"}}`))
	if !ok || resets != 1730009999 {
		t.Fatalf("resets=%d ok=%v", resets, ok)
	}
	// Форма OpenAI: resets_at верхнего уровня.
	resets, ok = ParseUsageLimitBody([]byte(`{"error":{"type":"usage_limit_reached"},"resets_at":42}`))
	if !ok || resets != 42 {
		t.Fatalf("top-level resets_at: resets=%d ok=%v", resets, ok)
	}
	// Прочие 429 — не маркер исчерпания.
	if _, ok := ParseUsageLimitBody([]byte(`{"error":{"type":"rate_limit_exceeded","resets_at":1}}`)); ok {
		t.Fatal("чужой error.type принят как usage_limit_reached")
	}
	if _, ok := ParseUsageLimitBody([]byte(`не json`)); ok {
		t.Fatal("не-JSON принят")
	}
}
