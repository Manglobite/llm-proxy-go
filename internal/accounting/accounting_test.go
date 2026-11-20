package accounting

import (
	"database/sql"
	"math"
	"testing"
	"time"
)

// Числа SPEC 7.2: GLM-5.3 — мультипликаторы кредитов 6.9/1.7/24,
// цены 0.6/0.07/2.2 $ за 1M токенов (AC-5).
func glmModel() *ModelInfo {
	return &ModelInfo{PriceIn: 0.6, PriceOut: 2.2, PriceCached: 0.07, GLMIn: 6.9, GLMCached: 1.7, GLMOut: 24}
}

func TestCalcGLMCreditsFormula(t *testing.T) {
	// (1000×6.9 + 200×1.7 + 300×24) / 10000 = (6900 + 340 + 7200) / 10000 = 1.444
	got := CalcGLMCredits(glmModel(), 1000, 200, 300)
	if math.Abs(got-1.444) > 1e-9 {
		t.Fatalf("glm credits = %v, ожидалось 1.444", got)
	}
}

func TestCalcCostUSDFormula(t *testing.T) {
	// uncached = 1000 − 200 = 800: (800×0.6 + 200×0.07 + 300×2.2)/1e6 = 1154/1e6
	got := CalcCostUSD(glmModel(), 1000, 200, 300)
	if math.Abs(got-0.001154) > 1e-12 {
		t.Fatalf("cost = %v, ожидалось 0.001154", got)
	}
}

func TestCalcSubscriptionZero(t *testing.T) {
	// Подписочная модель (AC-6): платим планом — и $, и кредиты = 0.
	m := &ModelInfo{PriceIn: 0.6, PriceOut: 2.2, PriceCached: 0.07, Subscription: true}
	if got := CalcCostUSD(m, 1000, 200, 300); got != 0 {
		t.Fatalf("cost подписочной = %v, ожидался 0", got)
	}
	if got := CalcGLMCredits(m, 1000, 200, 300); got != 0 {
		t.Fatalf("credits подписочной = %v, ожидалось 0", got)
	}
	// Неизвестная модель (nil) — тоже 0, а не паника: учёт не должен ронять запрос.
	if got := CalcCostUSD(nil, 10, 0, 10); got != 0 {
		t.Fatalf("cost nil-модели = %v, ожидался 0", got)
	}
}

func TestCalcCostUSDUncachedClamp(t *testing.T) {
	// cached > prompt (некорректный апстрим): uncached клампится в 0 МОЛЧА —
	// так требует fewshot; тест фиксирует это поведение (SPEC 9 ⚠ проверка:
	// при живом расхождении семантики кеша формулу придётся пересмотреть).
	m := &ModelInfo{PriceIn: 0.6, PriceOut: 2.2, PriceCached: 0.07}
	// (0 + 200×0.07 + 0)/1e6 = 14/1e6
	got := CalcCostUSD(m, 100, 200, 0)
	if math.Abs(got-0.000014) > 1e-12 {
		t.Fatalf("cost при cached>prompt = %v, ожидалось 0.000014 (uncached=0)", got)
	}
}

func TestCalcGLMCreditsNoMults(t *testing.T) {
	// Модель без glm_credits (GLMOut == 0) — не glm-модель, кредиты не считаем.
	m := &ModelInfo{PriceIn: 0.6, PriceOut: 2.2, PriceCached: 0.07}
	if got := CalcGLMCredits(m, 1000, 100, 100); got != 0 {
		t.Fatalf("credits не-glm модели = %v, ожидалось 0", got)
	}
}

func TestInsertRequestSelectBack(t *testing.T) {
	path := t.TempDir() + "/test.db"
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	ts := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	row := RequestRow{
		Ts: ts, ClientKey: "opencode", Alias: "glm/5.3",
		Provider: "glm", Account: "plan-a", Model: "glm-5.3",
		Protocol: "anthropic", Prompt: 1000, Completion: 300,
		CachedInput: 200, Reasoning: 50, IsStream: true,
		LatencyMS: 1234, Status: 200,
	}
	if err := d.InsertRequest(row, glmModel()); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
	// Ошибочная строка: error_kind заполнен, статус апстрима 502.
	errRow := RequestRow{
		Ts: ts, ClientKey: "opencode", Alias: "glm/5.3",
		Provider: "glm", Account: "plan-a", Model: "glm-5.3",
		Protocol: "anthropic", Status: 502, ErrorKind: "upstream",
	}
	if err := d.InsertRequest(errRow, glmModel()); err != nil {
		t.Fatalf("InsertRequest(error): %v", err)
	}

	var (
		clientKey, alias, provider, account, model, protocol, errorKind sql.NullString
		prompt, completion, cached, reasoning                           int
		isStream                                                        int
		latencyMS                                                       int64
		status                                                          int
		cost, credits                                                   float64
	)
	err = d.db.QueryRow(`SELECT client_key, alias, provider, account_id, model, protocol,
		prompt_tokens, completion_tokens, cached_input_tokens, reasoning_tokens,
		is_stream, latency_ms, status, error_kind, cost_usd, glm_credits
		FROM requests WHERE error_kind IS NULL`).Scan(
		&clientKey, &alias, &provider, &account, &model, &protocol,
		&prompt, &completion, &cached, &reasoning, &isStream, &latencyMS, &status, &errorKind, &cost, &credits)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if clientKey.String != "opencode" || alias.String != "glm/5.3" || provider.String != "glm" ||
		account.String != "plan-a" || model.String != "glm-5.3" || protocol.String != "anthropic" {
		t.Fatalf("не совпали строковые поля: %v %v %v %v %v %v", clientKey, alias, provider, account, model, protocol)
	}
	if prompt != 1000 || completion != 300 || cached != 200 || reasoning != 50 {
		t.Fatalf("usage: %d %d %d %d", prompt, completion, cached, reasoning)
	}
	if isStream != 1 || latencyMS != 1234 || status != 200 || errorKind.Valid {
		t.Fatalf("is_stream/latency/status/error_kind: %d %d %d %v", isStream, latencyMS, status, errorKind)
	}
	if math.Abs(cost-0.001154) > 1e-12 || math.Abs(credits-1.444) > 1e-9 {
		t.Fatalf("cost/credits в строке: %v/%v", cost, credits)
	}

	// Ошибочная строка: kind виден, usage нулевой, cost/credits посчитались от нулей.
	var kind string
	var zeroPrompt int
	err = d.db.QueryRow(`SELECT error_kind, prompt_tokens FROM requests WHERE error_kind IS NOT NULL`).Scan(&kind, &zeroPrompt)
	if err != nil {
		t.Fatalf("select error row: %v", err)
	}
	if kind != "upstream" || zeroPrompt != 0 {
		t.Fatalf("error-строка: kind=%q prompt=%d", kind, zeroPrompt)
	}
}

func TestUsageDailyAggregate(t *testing.T) {
	path := t.TempDir() + "/test.db"
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Две строки за один день: агрегат должен их сложить.
	for _, p := range []int{100, 50} {
		row := RequestRow{
			Ts:        time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
			ClientKey: "opencode", Alias: "a", Provider: "glm", Account: "a1", Model: "glm-5.3",
			Protocol: "openai", Prompt: p, Completion: 10,
		}
		if err := d.InsertRequest(row, glmModel()); err != nil {
			t.Fatalf("InsertRequest: %v", err)
		}
	}
	from := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	rows, err := d.UsageDaily(from, to, "")
	if err != nil {
		t.Fatalf("UsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("строк агрегата: %d, ожидалась 1", len(rows))
	}
	u := rows[0]
	if u.Day != "2026-11-08" || u.Requests != 2 || u.Prompt != 150 || u.Completion != 20 {
		t.Fatalf("агрегат: %+v", u)
	}
	if u.CostUSD <= 0 {
		t.Fatalf("cost_usd в агрегате не посчитан: %v", u.CostUSD)
	}
}
