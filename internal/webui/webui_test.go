// Тесты JSON API: auth (SV-5), статика, формы каждого эндпоинта = литералы
// mock.js (golden shape), usage-фильтры/пагинация, ретеншн ошибок.
package webui

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/upstream"
	"llm-proxy/internal/upstream/codex"
)

// TestWebUIAuthGates — webui_token задан → /api/* закрыт Bearer (SV-5);
// не задан → без авторизации (CF-5). Статика auth не проходит никогда.
func TestWebUIAuthGates(t *testing.T) {
	e := newEnv(t, "webui-secret")

	// Без токена / с неверным — 401 с JSON-конвертом.
	for _, token := range []string{"", "wrong"} {
		rec := e.get("/api/status", token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status с токеном %q: %d, ожидался 401", token, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "unauthorized") {
			t.Fatalf("401 тело не JSON-конверт: %s", rec.Body.String())
		}
	}
	// С верным — 200.
	if rec := e.get("/api/status", "webui-secret"); rec.Code != http.StatusOK {
		t.Fatalf("status с верным токеном: %d, тело: %s", rec.Code, rec.Body.String())
	}
	// Статика — без авторизации (SPEC 12: только /api/* закрыт).
	if rec := e.get("/", ""); rec.Code != http.StatusOK {
		t.Fatalf("index без токена: %d, ожидался 200", rec.Code)
	}
	// Токен не задан → /api/* открыт (CF-5).
	e2 := newEnv(t, "")
	if rec := e2.get("/api/status", ""); rec.Code != http.StatusOK {
		t.Fatalf("status без webui_token в конфиге: %d, ожидался 200", rec.Code)
	}
	// 405 — метод-гард (JSON, не текст ServeMux).
	rec := e.post("/api/status", "webui-secret", "{}")
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("POST /api/status: %d %s", rec.Code, rec.Body.String())
	}
}

// TestWebUIStatic — index на "/", ассеты с content-type, ревалидация по ETag.
func TestWebUIStatic(t *testing.T) {
	e := newEnv(t, "")

	rec := e.get("/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("index content-type: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `<script type="module" src="src/app/main.js">`) {
		t.Fatalf("index не содержит каркас эталона")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index Cache-Control: %q (ожидался no-cache)", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("index без ETag")
	}
	// Ревалидация: If-None-Match → 304 (кэш-политика решения Ф4).
	req2 := httptestGet("/", map[string]string{"If-None-Match": etag})
	rec2 := httptest.NewRecorder()
	e.ui.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, ожидался 304", rec2.Code)
	}
	// Ассеты: css и js с правильным content-type.
	for path, want := range map[string]string{
		"/src/shared/ui/tokens.css": "text/css",
		"/src/app/main.js":          "text/javascript",
		"/src/shared/api/mock.js":   "text/javascript",
	} {
		rec := e.get(path, "")
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), want) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func httptestGet(path string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// TestWebUIStatusShape — форма /api/status = литерал mock.js (DEMO_STATUS_META +
// DEMO_SOCKS_PATHS + DEMO_ACCOUNTS + DEMO_CLIENT_KEYS); факты значений — точечно.
func TestWebUIStatusShape(t *testing.T) {
	e := newEnv(t, "")
	// Данные: квоты codex из снапшотов, glm-баланс из requests.
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "codex", "c1", upstream.QuotaCodexPrimary, 37, 300, true, testNow.Add(2*time.Hour).Unix(), true)
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "codex", "c1", upstream.QuotaCodexWeekly, 61, 10080, true, 0, false)
	insertRequest(t, e.acct, testNow.Add(-time.Hour), "opencode", "glm/5.3", "glm", "g1", "glm-5.3", 100000, 0, 50000)
	insertRequest(t, e.acct, testNow.Add(-time.Hour), "zcode", "glm/5.3", "glm", "g1", "glm-5.3", 500000, 0, 100000)

	rec := e.get("/api/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	checkShape(t, rec, "status.golden.json")

	v := decode(t, rec).(map[string]any)
	// codex-аккаунт: email из конфига (OAuth ещё не был), can_oauth=true (SPEC 7.2).
	acc0 := v["accounts"].([]any)[0].(map[string]any)
	if acc0["provider"] != "codex" || acc0["email"] != "cfg@t.ru" || acc0["can_oauth"] != true {
		t.Fatalf("codex-аккаунт: %v", acc0)
	}
	if acc0["token"] != "нет токена" {
		t.Fatalf("codex без токена: token=%v", acc0["token"])
	}
	// glm: can_oauth=false (SPEC 7.2: кнопок OAuth у glm нет) + план.
	var glmAcc map[string]any
	for _, a := range v["accounts"].([]any) {
		am := a.(map[string]any)
		if am["provider"] == "glm" {
			glmAcc = am
		}
	}
	if glmAcc == nil || glmAcc["can_oauth"] != false || glmAcc["plan"] != "coding plan" {
		t.Fatalf("glm-аккаунт: %v", glmAcc)
	}
	// ollama: direct-бейдж-факт (allow_direct) + socks null.
	var olAcc map[string]any
	for _, a := range v["accounts"].([]any) {
		am := a.(map[string]any)
		if am["provider"] == "ollama" {
			olAcc = am
		}
	}
	if olAcc == nil || olAcc["allow_direct"] != true || olAcc["socks"] != nil {
		t.Fatalf("ollama-аккаунт: %v", olAcc)
	}
	socks0 := v["socks"].([]any)[0].(map[string]any)
	if socks0["state"] != "unchecked" || socks0["match"] != true || socks0["checked_ago"] != "не проверялся" {
		t.Fatalf("socks-строка: %v", socks0)
	}
	// meta: uptime от StartedAt (6ч в харнессе).
	meta := v["meta"].(map[string]any)
	if meta["listen"] != "127.0.0.1:8787" || !strings.HasPrefix(meta["uptime"].(string), "6h") {
		t.Fatalf("meta: %v", meta)
	}
	// Токен codex есть → статус ok/минуты; client_keys по обоим клиентам.
	if err := e.store.Set("codex/c1", codexTokenSet(testNow.Add(40*time.Minute))); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	rec = e.get("/api/status", "")
	acc0 = decode(t, rec).(map[string]any)["accounts"].([]any)[0].(map[string]any)
	if acc0["token"] != "токен 40м" {
		t.Fatalf("codex токен 40м: %v", acc0["token"])
	}
	keys := decode(t, rec).(map[string]any)["client_keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("client_keys: %v", keys)
	}
}

// codexTokenSet — TokenSet для статусов/refresh-тестов.
func codexTokenSet(expiresAt time.Time) codex.TokenSet {
	return codex.TokenSet{
		AccessToken: "at", RefreshToken: "fake-refresh-2",
		ExpiresAt: expiresAt, AccountID: "acc_test_1", Email: "flow@test.ru",
	}
}

// TestWebUIAliases — форма = DEMO_ALIASES; битый путь цели → err с note mock.js.
func TestWebUIAliases(t *testing.T) {
	e := newEnv(t, "")
	// Путь work заблокировать вручную (без сети: state не выставляется снаружи,
	// поэтому блокируем проверкой факта «socks path blocked» через stub-путь).
	// socks.Path state недоступен извне — вместо этого проверяем ok-ветку и
	// факт req24h; блокировка пути покрыта smoke (fail-closed стек).
	insertRequest(t, e.acct, testNow.Add(-time.Hour), "opencode", "glm/5.3", "glm", "g1", "glm-5.3", 10, 0, 5)

	rec := e.get("/api/aliases", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("aliases: %d", rec.Code)
	}
	checkShape(t, rec, "aliases.golden.json")
	rows := decode(t, rec).([]any)
	if len(rows) != 3 {
		t.Fatalf("алиасов: %d, ожидалось 3", len(rows))
	}
	first := rows[0].(map[string]any) // сортировка: glm/5.3 первый
	if first["name"] != "glm/5.3" || first["req24h"] != float64(1) || first["status"] != "ok" {
		t.Fatalf("алиас glm/5.3: %v", first)
	}
}

// TestWebUIQuotasShape — форма /api/quotas = DEMO_QUOTAS + DEMO_RATE_LIMITS.
func TestWebUIQuotasShape(t *testing.T) {
	e := newEnv(t, "")
	reset := testNow.Add(2*time.Hour + 21*time.Minute).Unix()
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "codex", "c1", upstream.QuotaCodexPrimary, 37, 300, true, reset, true)
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "codex", "c1", upstream.QuotaCodexWeekly, 61, 10080, true, 0, false)
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "codex", "c1", upstream.QuotaCodexLimited, 100, 0, false, reset, true)
	insertRequest(t, e.acct, testNow.Add(-time.Hour), "opencode", "glm/5.3", "glm", "g1", "glm-5.3", 100000, 0, 50000)

	rec := e.get("/api/quotas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("quotas: %d %s", rec.Code, rec.Body.String())
	}
	checkShape(t, rec, "quotas.golden.json")

	v := decode(t, rec).(map[string]any)
	cards := v["cards"].([]any)
	if len(cards) != 3 { // codex (снапшоты) + glm и zcode (plan_credits); ollama — без поллера
		t.Fatalf("карточек: %d, ожидалось 3: %s", len(cards), rec.Body.String())
	}
	c0 := cards[0].(map[string]any)
	if c0["provider"] != "codex" || len(c0["windows"].([]any)) != 2 {
		t.Fatalf("codex-карточка: %v", c0)
	}
	w0 := c0["windows"].([]any)[0].(map[string]any)
	if w0["value"] != "37%" || w0["level"] != "ok" {
		t.Fatalf("codex 5ч окно: %v", w0)
	}
	// 429-события: маркер codex_limited → строка с окном от primary.
	rows := v["rate_limits"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("429-строк: %d, ожидалась 1", len(rows))
	}
	r0 := rows[0].(map[string]any)
	if r0["account"] != "codex/c1" || r0["window"] != "5ч" || r0["reset"] == "—" {
		t.Fatalf("429-строка: %v", r0)
	}
	// glm-карточка: кредиты из requests (4213) против плана.
	c1 := cards[1].(map[string]any)
	w := c1["windows"].([]any)[0].(map[string]any)
	glmUsed := accounting.CalcGLMCredits(&accounting.ModelInfo{GLMIn: 6.9, GLMCached: 1.7, GLMOut: 24}, 100000, 0, 50000)
	if w["value"] != fmtIntRu(int64(glmUsed))+" / 12 000" {
		t.Fatalf("glm кредиты 5ч: %v", w)
	}
	// zcode-карточка: та же строка кредитов, что у glm (SPEC 7.4), расход пока нулевой.
	c2 := cards[2].(map[string]any)
	if c2["provider"] != "zai-plan" || c2["account"] != "main" {
		t.Fatalf("zcode-карточка: %v", c2)
	}
	if wz := c2["windows"].([]any)[0].(map[string]any); wz["value"] != "0 / 12 000" {
		t.Fatalf("zcode кредиты 5ч: %v", wz)
	}
}

// TestWebUIUsage — форма = DEMO_USAGE; фильтры client_key/alias/model/account
// плейсхолдерами, group_by day|alias|model|account, from/to.
func TestWebUIUsage(t *testing.T) {
	e := newEnv(t, "")
	day := func(d int) time.Time { return time.Date(2026, 11, d, 10, 0, 0, 0, time.UTC) }
	insertRequest(t, e.acct, day(1), "opencode", "glm/5.3", "glm", "g1", "glm-5.3", 2_000_000, 1_400_000, 300_000)
	insertRequest(t, e.acct, day(2), "zcode", "work/terra", "codex", "c1", "gpt-6.1-terra", 400_000, 200_000, 80_000)
	insertRequest(t, e.acct, day(3), "opencode", "qwen/cloud", "ollama", "o1", "qwen3-coder", 90_000, 10_000, 30_000)

	rec := e.get("/api/usage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	checkShape(t, rec, "usage.golden.json")
	v := decode(t, rec).(map[string]any)
	if len(v["byAlias"].([]any)) != 3 || len(v["days"].([]any)) != 3 {
		t.Fatalf("usage: byAlias=%d days=%d", len(v["byAlias"].([]any)), len(v["days"].([]any)))
	}
	// unit («%») обязателен у cache hit rate (форма mock.js: stat с unit).
	hit := v["summary"].([]any)[1].(map[string]any)
	if hit["unit"] != "%" || hit["value"] == "" {
		t.Fatalf("cache hit rate stat: %v", hit)
	}
	// Подписочная модель → «подписка» (AC-6); glm-кредиты форматированы разрядами.
	byAlias := map[string]map[string]any{}
	for _, r := range v["byAlias"].([]any) {
		m := r.(map[string]any)
		byAlias[m["alias"].(string)] = m
	}
	if byAlias["work/terra"]["usd"] != "подписка" {
		t.Fatalf("work/terra usd: %v", byAlias["work/terra"]["usd"])
	}
	glmCr := accounting.CalcGLMCredits(&accounting.ModelInfo{GLMIn: 6.9, GLMCached: 1.7, GLMOut: 24}, 2_000_000, 1_400_000, 300_000)
	olCost := accounting.CalcCostUSD(&accounting.ModelInfo{PriceIn: 0.15, PriceCached: 0.01, PriceOut: 2.0}, 90_000, 10_000, 30_000)
	if byAlias["glm/5.3"]["cr"] != fmtIntRu(int64(glmCr)) || byAlias["qwen/cloud"]["usd"] != "$"+fmtF(olCost, 2) {
		t.Fatalf("glm cr / ollama usd: %v / %v", byAlias["glm/5.3"]["cr"], byAlias["qwen/cloud"]["usd"])
	}
	// Фильтр client_key (плейсхолдер): только opencode.
	rec = e.get("/api/usage?client_key=opencode", "")
	rows := decode(t, rec).(map[string]any)["byAlias"].([]any)
	if len(rows) != 2 {
		t.Fatalf("client_key=opencode: %d строк, ожидалось 2", len(rows))
	}
	// Фильтр alias.
	rec = e.get("/api/usage?alias=glm/5.3", "")
	rows = decode(t, rec).(map[string]any)["byAlias"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["alias"] != "glm/5.3" {
		t.Fatalf("alias-фильтр: %v", rows)
	}
	// group_by=model: разрез по моделям, work/terra и qwen по одной строке.
	rec = e.get("/api/usage?group_by=model", "")
	rows = decode(t, rec).(map[string]any)["byAlias"].([]any)
	if len(rows) != 3 {
		t.Fatalf("group_by=model: %d строк, ожидалось 3", len(rows))
	}
	// group_by=account.
	rec = e.get("/api/usage?group_by=account", "")
	if len(decode(t, rec).(map[string]any)["byAlias"].([]any)) != 3 {
		t.Fatalf("group_by=account: ожидалось 3 строки")
	}
	// Недопустимый group_by → 400.
	if rec = e.get("/api/usage?group_by=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("group_by=bogus: %d, ожидался 400", rec.Code)
	}
	// from/to: только один день.
	rec = e.get("/api/usage?from=2026-11-02&to=2026-11-03", "")
	if len(decode(t, rec).(map[string]any)["days"].([]any)) != 1 {
		t.Fatalf("from/to: ожидался 1 день")
	}
	// Битое from → 400.
	if rec = e.get("/api/usage?from=завтра", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("from=завтра: %d, ожидался 400", rec.Code)
	}
}

// TestWebUIErrorsListPagination — список ошибок: форма = DEMO_ERRORS (без dump),
// свежие первыми, limit/offset.
func TestWebUIErrorsListPagination(t *testing.T) {
	e := newEnv(t, "")
	for i, d := range []int{1, 2, 3} {
		insertError(t, e.elog, testNow.AddDate(0, 0, -d), "upstream", "work/terra", "err-"+string(rune('a'+i)))
	}
	rec := e.get("/api/errors?limit=2&offset=0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("errors: %d %s", rec.Code, rec.Body.String())
	}
	checkShape(t, rec, "errors.golden.json")
	rows := decode(t, rec).([]any)
	if len(rows) != 2 {
		t.Fatalf("limit=2: %d строк", len(rows))
	}
	if rows[0].(map[string]any)["err"] != "err-c" { // свежие первыми
		t.Fatalf("порядок: %v", rows[0])
	}
	rec = e.get("/api/errors?limit=2&offset=2", "")
	rows = decode(t, rec).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["err"] != "err-a" {
		t.Fatalf("offset=2: %v", rows)
	}
	// Битый limit → 400.
	if rec = e.get("/api/errors?limit=abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc: %d", rec.Code)
	}
}

// TestWebUIErrorsDetailAndCleanup — дамп по id (форма mock dump), 404 на чужой id,
// ретеншн «7d» и RFC3339 (AC-9).
func TestWebUIErrorsDetailAndCleanup(t *testing.T) {
	e := newEnv(t, "")
	insertError(t, e.elog, testNow.AddDate(0, 0, -1), "upstream", "work/terra", "usage_limit_reached")
	rows := decode(t, e.get("/api/errors", "")).([]any)
	id := int64(rows[0].(map[string]any)["id"].(float64))

	rec := e.get("/api/errors/"+strconv.FormatInt(id, 10), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	checkShape(t, rec, "errors_detail.golden.json")
	dump := decode(t, rec).(map[string]any)["dump"].(map[string]any)
	if dump["method"] != "POST" || dump["reqHeaders"] != `{"Authorization":"<masked>"}` {
		t.Fatalf("dump: %v", dump)
	}
	// Чужой id → 404.
	if rec = e.get("/api/errors/99999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("detail 99999: %d, ожидался 404", rec.Code)
	}
	// Ретеншн «7d»: записи старше 7 дней нет в этот момент — 0.
	rec = e.post("/api/errors/cleanup", "", `{"older_than":"7d"}`)
	if rec.Code != http.StatusOK || decode(t, rec).(map[string]any)["removed"] != float64(0) {
		t.Fatalf("cleanup 7d: %d %s", rec.Code, rec.Body.String())
	}
	// F4-fix1 F1: многозначные «30d»/«2w» принимаются (прежде «30d» падал в 400).
	rec = e.post("/api/errors/cleanup", "", `{"older_than":"30d"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup 30d: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.post("/api/errors/cleanup", "", `{"older_than":"2w"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup 2w: %d %s", rec.Code, rec.Body.String())
	}
	// «xd» — не число перед суффиксом → 400.
	rec = e.post("/api/errors/cleanup", "", `{"older_than":"xd"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cleanup xd: %d, ожидался 400", rec.Code)
	}
	// RFC3339-момент в будущем удаляет всё.
	rec = e.post("/api/errors/cleanup", "", `{"older_than":"2026-11-07T00:00:00Z"}`)
	if decode(t, rec).(map[string]any)["removed"] != float64(1) {
		t.Fatalf("cleanup RFC3339: %s", rec.Body.String())
	}
	// Пустой/битый older_than → 400.
	if rec = e.post("/api/errors/cleanup", "", `{"older_than":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("cleanup пустой: %d", rec.Code)
	}
	if rec = e.post("/api/errors/cleanup", "", `{"older_than":"никогда"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("cleanup битый: %d", rec.Code)
	}
	// Список после ретеншна пуст.
	if rows := decode(t, e.get("/api/errors", "")).([]any); len(rows) != 0 {
		t.Fatalf("после cleanup остались строки: %v", rows)
	}
}

// TestWebUIUsageDailyWhere — UsageDaily принимает фрагмент usageWhere
// (плейсхолдеры), форма дней не меняется.
func TestWebUIUsageDailyWhere(t *testing.T) {
	e := newEnv(t, "")
	insertRequest(t, e.acct, testNow.Add(-time.Hour), "opencode", "glm/5.3", "glm", "g1", "glm-5.3", 1000, 0, 500)
	insertRequest(t, e.acct, testNow.Add(-2*time.Hour), "zcode", "glm/5.3", "glm", "g1", "glm-5.3", 1000, 0, 500)
	where, args := usageWhere(accounting.UsageFilter{ClientKey: "opencode"})
	days, err := e.acct.UsageDaily(testNow.Add(-24*time.Hour), testNow, where, args...)
	if err != nil {
		t.Fatalf("UsageDaily: %v", err)
	}
	if len(days) != 1 || days[0].Requests != 1 {
		t.Fatalf("UsageDaily с фильтром: %+v", days)
	}
	// Snapshot'ы/others: KindOllamaUsage пишется поллером, читается Recent.
	insertSnapshot(t, e.qdb, testNow.Add(-time.Minute), "ollama", "o1", quotas.KindOllamaUsage, 2.41, 0, false, 0, false)
	snap, err := e.qdb.Recent(10)
	if err != nil || len(snap) != 1 || snap[0].UsedPercent != 2.41 {
		t.Fatalf("Recent: %v %+v", err, snap)
	}
}
