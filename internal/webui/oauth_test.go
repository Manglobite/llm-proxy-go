// Тесты OAuth-части WebUI (SPEC 12): start→pending→paste, чужой/просроченный
// state → 400 (OA-6), callback-listener (браузер → tokens.json → Store),
// форс-refresh. Формы — golden'ы mock.js (DEMO_OAUTH).
package webui

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream/codex"
)

// doAuthorize — «браузер»: GET authorize-ссылки у фейкового auth (socks.Direct —
// цель localhost, I1 соблюдён: клиент создаёт только internal/socks).
func doAuthorize(t *testing.T, client *http.Client, authorizeURL string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, authorizeURL, nil)
	if err != nil {
		t.Fatalf("authorize req: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		t.Fatalf("authorize body: %v", err)
	}
	const prefix = "CALLBACK_URL="
	s := strings.TrimSpace(string(body))
	if !strings.HasPrefix(s, prefix) {
		t.Fatalf("authorize: в теле нет CALLBACK_URL: %q", s)
	}
	return strings.TrimPrefix(s, prefix)
}

// TestWebUIOAuthStartPendingPaste — полный paste-флоу (режим контейнера).
func TestWebUIOAuthStartPendingPaste(t *testing.T) {
	e := newEnv(t, "")

	// start: форма = DEMO_OAUTH.
	rec := e.post("/api/oauth/start", "", `{"account":"codex/c1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	checkShape(t, rec, "oauth_start.golden.json")
	start := decode(t, rec).(map[string]any)
	if start["account"] != "codex/c1" || start["callback"] != "127.0.0.1:"+strconv.Itoa(e.port)+"/auth/callback" {
		t.Fatalf("start: %v", start)
	}
	if !strings.HasPrefix(start["url"].(string), e.authBase+"/oauth/authorize?") {
		t.Fatalf("start url: %v", start["url"])
	}
	// pending: форма = flows [{account,state,url,expires_at}], state waiting.
	rec = e.get("/api/oauth/pending", "")
	checkShape(t, rec, "oauth_pending.golden.json")
	pending := decode(t, rec).(map[string]any)
	if pending["state"] != "waiting" {
		t.Fatalf("pending state: %v", pending)
	}
	flow := pending["flows"].([]any)[0].(map[string]any)
	if flow["account"] != "codex/c1" || flow["state"] == "" {
		t.Fatalf("pending flow: %v", flow)
	}
	// В pending НЕТ verifier — только аккаунт/state/expires_at (SPEC 12).
	if strings.Contains(rec.Body.String(), "verifier") {
		t.Fatalf("pending раскрыл verifier: %s", rec.Body.String())
	}

	// «Браузер»: получаем callback-URL у fake auth и вставляем целиком.
	callbackURL := doAuthorize(t, socks.Direct{}.Client(), start["url"].(string))
	rec = e.post("/api/oauth/paste", "", `{"account":"codex/c1","url":`+quote(callbackURL)+`}`)
	if rec.Code != http.StatusOK || decode(t, rec).(map[string]any)["ok"] != true {
		t.Fatalf("paste: %d %s", rec.Code, rec.Body.String())
	}
	// tokens.json обновлён (Store), claims из id_token распознаны.
	ts, ok := e.store.Get("codex/c1")
	if !ok || ts.AccessToken != "fake-access" || ts.Email != "flow@test.ru" || ts.AccountID != "acc_test_1" {
		t.Fatalf("store после paste: %+v ok=%v", ts, ok)
	}
	// Флоу одноразовый: pending пуст.
	pending = decode(t, e.get("/api/oauth/pending", "")).(map[string]any)
	if pending["state"] != "idle" || len(pending["flows"].([]any)) != 0 {
		t.Fatalf("pending после paste: %v", pending)
	}
	// Повторный paste того же URL → 400 (state погашен).
	rec = e.post("/api/oauth/paste", "", `{"account":"codex/c1","url":`+quote(callbackURL)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("повторный paste: %d, ожидался 400", rec.Code)
	}
}

// TestWebUIOAuthForeignState — чужой state (флоу другого аккаунта) → 400.
func TestWebUIOAuthForeignState(t *testing.T) {
	e := newEnv(t, "")
	// Второй codex-аккаунт в снапшоте: флоу A, paste под именем B.
	e.snap.Cfg.Providers["codex"].Codex.Accounts = append(e.snap.Cfg.Providers["codex"].Codex.Accounts,
		codexAcc("c2", e.port))
	rec := e.post("/api/oauth/start", "", `{"account":"codex/c1"}`)
	start := decode(t, rec).(map[string]any)
	callbackURL := doAuthorize(t, socks.Direct{}.Client(), start["url"].(string))
	rec = e.post("/api/oauth/paste", "", `{"account":"codex/c2","url":`+quote(callbackURL)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("чужой state: %d, ожидался 400", rec.Code)
	}
}

// TestWebUIOAuthExpired — TTL-истёкший state → 400 (Flows с микроскопическим TTL).
func TestWebUIOAuthExpired(t *testing.T) {
	e := newEnv(t, "")
	// Подменяем Flows на тестируемый TTL=1ns: после Start флоу уже просрочен
	// (CODING 1.9 — без time.Sleep).
	e.flows = codex.NewFlows(e.store, 1, func(string) (codex.AcctDeps, error) {
		return codex.AcctDeps{Client: socks.Direct{}.Client(), AuthBase: e.authBase}, nil
	})
	e.rebuildUI()
	rec := e.post("/api/oauth/start", "", `{"account":"codex/c1"}`)
	start := decode(t, rec).(map[string]any)
	callbackURL := doAuthorize(t, socks.Direct{}.Client(), start["url"].(string))
	rec = e.post("/api/oauth/paste", "", `{"account":"codex/c1","url":`+quote(callbackURL)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("истёкший state: %d, ожидался 400", rec.Code)
	}
}

// TestWebUICallbackListener — локальный listener: браузер GET /auth/callback →
// обмен через аккаунт → tokens.json → Store; страница — читабельный HTML.
func TestWebUICallbackListener(t *testing.T) {
	e := newEnv(t, "")
	// start поднимает listener на callback_port аккаунта (свободный порт харнесса).
	rec := e.post("/api/oauth/start", "", `{"account":"codex/c1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	start := decode(t, rec).(map[string]any)
	callbackURL := doAuthorize(t, socks.Direct{}.Client(), start["url"].(string))

	// Браузер редиректится на локальный listener: GET callback URL.
	u, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatalf("callback url: %v", err)
	}
	// listener слушает 127.0.0.1:port — запрос через прямой клиент.
	ln := "http://" + u.Host + u.Path + "?" + u.RawQuery
	resp, err := socks.Direct{}.Client().Get(ln)
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "OAuth завершён") {
		t.Fatalf("callback: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("callback content-type: %q", ct)
	}
	// Токены в Store (exchange прошёл внутри Flows через клиент аккаунта).
	ts, ok := e.store.Get("codex/c1")
	if !ok || ts.AccessToken != "fake-access" {
		t.Fatalf("store после callback: %+v", ts)
	}
	// Ошибка state → страница ошибки 400 (читабельная, не JSON).
	bad, err := socks.Direct{}.Client().Get("http://" + u.Host + u.Path + "?code=x&state=foreign")
	if err != nil {
		t.Fatalf("callback bad GET: %v", err)
	}
	defer bad.Body.Close()
	bbody, _ := io.ReadAll(io.LimitReader(bad.Body, 16*1024))
	if bad.StatusCode != http.StatusBadRequest || !strings.Contains(string(bbody), "OAuth — ошибка") {
		t.Fatalf("callback bad: %d %s", bad.StatusCode, bbody)
	}
}

// TestWebUIOAuthRefresh — форс-refresh: refresh_token ротируется, access меняется.
func TestWebUIOAuthRefresh(t *testing.T) {
	e := newEnv(t, "")
	if err := e.store.Set("codex/c1", codexTokenSet(testNow.Add(-time.Hour))); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	rec := e.post("/api/oauth/refresh", "", `{"account":"codex/c1"}`)
	if rec.Code != http.StatusOK || decode(t, rec).(map[string]any)["ok"] != true {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	ts, _ := e.store.Get("codex/c1")
	if ts.AccessToken != "fake-access-2" { // стаб отдаёт новый access на refresh
		t.Fatalf("refresh не сменил access: %+v", ts)
	}
	// Не-codex аккаунт → 400 (SPEC 7.2: OAuth только codex).
	rec = e.post("/api/oauth/refresh", "", `{"account":"glm/g1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh glm: %d, ожидался 400", rec.Code)
	}
	// Неизвестный аккаунт → 400.
	rec = e.post("/api/oauth/refresh", "", `{"account":"codex/nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh неизвестный: %d, ожидался 400", rec.Code)
	}
	// У аккаунта нет токенов → 400.
	rec = e.post("/api/oauth/refresh", "", `{"account":"codex/c2"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh без токенов: %d, ожидался 400", rec.Code)
	}
}

// TestWebUIStartValidation — start для не-codex/несуществующего → 400.
func TestWebUIStartValidation(t *testing.T) {
	e := newEnv(t, "")
	for _, acc := range []string{"glm/g1", "codex/nope", "без-слэша"} {
		rec := e.post("/api/oauth/start", "", `{"account":"`+acc+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("start %q: %d, ожидался 400", acc, rec.Code)
		}
	}
	if rec := e.post("/api/oauth/start", "", `не-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("start не-json: %d", rec.Code)
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// TestWebUIListenerPortDefault — callback_port=0 в конфиге: дефолт 1455 достаётся
// из фактического redirect_uri (не «слушать :0»); битый redirect_uri — без листенера.
func TestWebUIListenerPortDefault(t *testing.T) {
	port, ok := listenerPort(codex.RedirectURI(0))
	if !ok || port != codex.DefaultCallbackPort {
		t.Fatalf("дефолт: %d %v, ожидался %d", port, ok, codex.DefaultCallbackPort)
	}
	if port, ok := listenerPort(codex.RedirectURI(7777)); !ok || port != 7777 {
		t.Fatalf("кастомный порт: %d %v", port, ok)
	}
	if _, ok := listenerPort("не-url"); ok {
		t.Fatalf("битый redirect_uri не должен давать порт")
	}
	if _, ok := listenerPort("http://127.0.0.1/auth/callback"); ok {
		t.Fatalf("redirect_uri без порта не должен давать листенера")
	}
}
