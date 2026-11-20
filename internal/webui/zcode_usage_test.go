package webui

// Тесты Ф8 в WebUI: снапшот kind zcode_usage поллера программных квот z.ai
// (U01–U06) виден на экране квот (окно «z.ai usage») и в строке статуса
// zcode-аккаунта («z.ai usage: N%»). Отдельного экрана нет (SPEC 12.3).

import (
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/quotas"
)

func TestWebUIZCodeUsageVisible(t *testing.T) {
	e := newEnv(t, "")
	// Два тика поллера: свежайший (42.5) обязан победить более старый (30).
	insertSnapshot(t, e.qdb, testNow.Add(-2*time.Minute), "zai-plan", "main", quotas.KindZCodeUsage, 30, 0, false, 0, false)
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "zai-plan", "main", quotas.KindZCodeUsage, 42.5, 0, false, 0, false)

	// Экран квот: карточка zcode-аккаунта с окном «z.ai usage».
	rec := e.get("/api/quotas", "")
	if rec.Code != 200 {
		t.Fatalf("quotas: %d %s", rec.Code, rec.Body.String())
	}
	var card map[string]any
	for _, c := range decode(t, rec).(map[string]any)["cards"].([]any) {
		if m := c.(map[string]any); m["provider"] == "zai-plan" && m["account"] == "main" {
			card = m
		}
	}
	if card == nil {
		t.Fatal("карточки zcode нет")
	}
	var usage map[string]any
	for _, w := range card["windows"].([]any) {
		if m := w.(map[string]any); m["label"] == "z.ai usage" {
			usage = m
		}
	}
	if usage == nil {
		t.Fatalf("окна z.ai usage нет: %v", card)
	}
	if usage["value"] != "42.5%" || usage["pct"] != float64(42.5) {
		t.Fatalf("окно z.ai usage: %v", usage)
	}

	// Экран статуса: подпись «z.ai usage: N%» в строке аккаунта.
	rec = e.get("/api/status", "")
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	var acc map[string]any
	for _, a := range decode(t, rec).(map[string]any)["accounts"].([]any) {
		if m := a.(map[string]any); m["provider"] == "zai-plan" && m["id"] == "main" {
			acc = m
		}
	}
	if acc == nil {
		t.Fatal("строки zcode-аккаунта нет")
	}
	u := acc["usage"].(map[string]any)
	if !strings.Contains(u["label"].(string), "z.ai usage: 42.5%") {
		t.Fatalf("usage.label = %q, ожидалась подпись z.ai usage: 42.5%%", u["label"])
	}
	if len(u["segments"].([]any)) == 0 {
		t.Fatal("сегменты бара usage пусты")
	}
}

// Без снапшотов поллера экран остаётся как раньше: карточка только с
// plan_credits, подпись без z.ai usage (отсутствие данных не выдаётся за 0%).
func TestWebUIZCodeUsageAbsentWithoutSnapshots(t *testing.T) {
	e := newEnv(t, "")
	rec := e.get("/api/status", "")
	for _, a := range decode(t, rec).(map[string]any)["accounts"].([]any) {
		m := a.(map[string]any)
		if m["provider"] == "zai-plan" && strings.Contains(m["usage"].(map[string]any)["label"].(string), "z.ai usage") {
			t.Fatalf("без снапшотов подпись z.ai usage не должна появляться: %v", m["usage"])
		}
	}
}
