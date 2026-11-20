// Тесты zcode-части WebUI (Ф7b, SPEC 12 «OAuth-флоу в WebUI (zcode, режим
// poll)»): start (валидация аккаунта, 502 init), pending (форма none/pending/
// ready/failed), полный флоу до ready через настоящий поллер (ожидание — по
// каналам стаба, CODING 1.9 без time.Sleep), строки статуса/квот zcode.
package webui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/upstream/zcode"
)

// TestWebUIZcodeStartValidation — start только для zcode-аккаунтов (SPEC 7.2):
// чужой kind / неизвестный / битый ключ → 400; тело не JSON → 400.
func TestWebUIZcodeStartValidation(t *testing.T) {
	e := newEnv(t, "")
	for _, acc := range []string{"glm/g1", "codex/c1", "ollama/o1", "zai-plan/nope", "без-слэша", "zcode/ghost"} {
		rec := e.post("/api/oauth/zcode/start", "", `{"account":"`+acc+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("start %q: %d, ожидался 400", acc, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "OAuth zcode есть только у zcode-аккаунтов") &&
			!strings.Contains(rec.Body.String(), "не найден") &&
			!strings.Contains(rec.Body.String(), "ожидается ключ вида") {
			t.Fatalf("start %q: текст ошибки без пояснения: %s", acc, rec.Body.String())
		}
	}
	if rec := e.post("/api/oauth/zcode/start", "", `не-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("start не-json: %d", rec.Code)
	}
	// Чужой kind — именно про zcode (образец findCodexAccount).
	rec := e.post("/api/oauth/zcode/start", "", `{"account":"glm/g1"}`)
	if !strings.Contains(rec.Body.String(), "OAuth zcode есть только у zcode-аккаунтов") {
		t.Fatalf("чужой kind: %s", rec.Body.String())
	}
}

// TestWebUIZcodeStartPending — happy-path без завершения: start → 200 с
// authorize_url (init через клиент аккаунта), pending → pending с той же
// ссылкой; аккаунт без флоу → none; pending чужого аккаунта → 400.
func TestWebUIZcodeStartPending(t *testing.T) {
	e := newEnv(t, "")
	e.zf.holdPending.Store(true)

	// start принимает обе формы ключа: имя провайдера и literal "zcode/<id>".
	rec := e.post("/api/oauth/zcode/start", "", `{"account":"zai-plan/main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	start := decode(t, rec).(map[string]any)
	url, _ := start["authorize_url"].(string)
	if url == "" || !strings.HasPrefix(url, "https://chat.z.ai/") {
		t.Fatalf("start authorize_url: %v", start)
	}
	if start["account"] != "zcode/main" {
		t.Fatalf("start account (ключ флоу): %v", start)
	}
	// pending: форма SPEC 12 {"status","authorize_url","error"}.
	rec = e.get("/api/oauth/zcode/pending?account=zai-plan/main", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending: %d %s", rec.Code, rec.Body.String())
	}
	p := decode(t, rec).(map[string]any)
	if p["status"] != "pending" || p["authorize_url"] != url || p["error"] != nil {
		t.Fatalf("pending: %v", p)
	}
	// literal-ключ записи (SPEC 3.2а) указывает на тот же флоу.
	rec = e.get("/api/oauth/zcode/pending?account=zcode/main", "")
	if decode(t, rec).(map[string]any)["status"] != "pending" {
		t.Fatalf("pending по literal-ключу: %s", rec.Body.String())
	}
	// Аккаунт без флоу → none.
	rec = e.get("/api/oauth/zcode/pending?account=zai-plan/ghost", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pending ghost: %d, ожидался 400", rec.Code)
	}
	// Без параметра account → 400.
	if rec := e.get("/api/oauth/zcode/pending", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("pending без account: %d", rec.Code)
	}
	// Повторный start заменяет флоу (новая ссылка — новый init).
	rec = e.post("/api/oauth/zcode/start", "", `{"account":"zcode/main"}`)
	if rec.Code != http.StatusOK || decode(t, rec).(map[string]any)["authorize_url"] == url {
		t.Fatalf("повторный start должен дать новый init: %s", rec.Body.String())
	}
}

// TestWebUIZcodeStartInitError — init упал (сеть/5xx апстрима через SOCKS
// аккаунта) → 502 auth (таксономия §10), флоу не регистрируется.
func TestWebUIZcodeStartInitError(t *testing.T) {
	e := newEnv(t, "")
	e.zf.initFails.Store(true)
	rec := e.post("/api/oauth/zcode/start", "", `{"account":"zai-plan/main"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("init 500: %d, ожидался 502: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("502 не JSON-конверт: %s", rec.Body.String())
	}
	rec = e.get("/api/oauth/zcode/pending?account=zai-plan/main", "")
	if decode(t, rec).(map[string]any)["status"] != "none" {
		t.Fatalf("флоу не должен регистрироваться при провале init: %s", rec.Body.String())
	}
}

// TestWebUIZcodeFlowReady — полный флоу через настоящий поллер: pending →
// ready, OAuth-результат персистнут в Store, PAT-цепочка доведена (кеш
// заполнен — тот же single-flight, что у тикера/sender'а).
func TestWebUIZcodeFlowReady(t *testing.T) {
	e := newEnv(t, "")
	rec := e.post("/api/oauth/zcode/start", "", `{"account":"zai-plan/main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	// Ожидание — по каналу стаба: loginHit означает, что finalize уже персистил
	// токены и выставил ready (цепочка идёт ПОСЛЕ готовности, SPEC 12).
	select {
	case <-e.zf.loginHit:
	case <-time.After(15 * time.Second):
		t.Fatal("флоу не дошёл до PAT-цепочки за 15s")
	}
	rec = e.get("/api/oauth/zcode/pending?account=zai-plan/main", "")
	p := decode(t, rec).(map[string]any)
	if p["status"] != "ready" {
		t.Fatalf("pending = %v, ожидался ready", p)
	}
	// Персист СНАЧАЛА (SPEC 12): запись в Store до/независимо от цепочки.
	key := zcode.AccountKey("main")
	r, ok := e.zstore.Get(key)
	if !ok || r.ZaiAccessToken == "" || r.ZcodeJWT != "zjwt-web" ||
		r.UserID != "u-web" || r.Email != "web@z.ai" {
		t.Fatalf("OAuth-результат не персистнут: %+v ok=%v", r, ok)
	}
	// PAT-цепочка доведена: EnsurePAT под тем же per-account мьютексом
	// (single-flight) дождётся флоу-цепочку и вернёт кеш без сети.
	deps, err := e.zdeps(key)
	if err != nil {
		t.Fatalf("zdeps: %v", err)
	}
	rc := r
	if _, err := e.zchain.EnsurePAT(context.Background(), deps, &rc); err != nil {
		t.Fatalf("EnsurePAT после флоу: %v", err)
	}
	if _, _, ok := e.zchain.PATInfo(key); !ok {
		t.Fatal("PAT не оказался в кеше после флоу")
	}
}

// zcodeRow — строка zcode-аккаунта из /api/status (helper тестов).
func zcodeRow(t *testing.T, e *env) map[string]any {
	t.Helper()
	v := decode(t, e.get("/api/status", "")).(map[string]any)
	for _, a := range v["accounts"].([]any) {
		m := a.(map[string]any)
		if m["provider"] == "zai-plan" && m["id"] == "main" {
			return m
		}
	}
	t.Fatal("zcode-строка не найдена в /api/status")
	return nil
}

// TestWebUIZcodeStatusBeforeOAuth — до OAuth: «нет токена», кнопки OAuth в
// poll-режиме, реквизиты строки те же, что у прочих провайдеров.
func TestWebUIZcodeStatusBeforeOAuth(t *testing.T) {
	e := newEnv(t, "")
	r := zcodeRow(t, e)
	if r["token"] != "нет токена" || r["can_oauth"] != true || r["oauth_mode"] != "poll" {
		t.Fatalf("zcode до OAuth: %v", r)
	}
	if r["auth"] != "oauth" || r["socks"] != nil || r["allow_direct"] != false {
		t.Fatalf("zcode реквизиты: %v", r)
	}
}

// TestWebUIZcodeStatusNeedsRelogin — статус needs_relogin попадает в строку
// с человочитаемым текстом ошибки (SPEC 7.4: интерактивный OAuth заново).
func TestWebUIZcodeStatusNeedsRelogin(t *testing.T) {
	e := newEnv(t, "")
	key := zcode.AccountKey("main")
	// Персист токенов без флоу + цепочка против стаба с мёртвым login
	// (детектор SPEC 7.4: бизнес-код ≠0 → needs_relogin).
	if err := e.zstore.Set(key, zcode.TokenRec{ZaiAccessToken: "zai-dead", UserID: "u-web"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	errStub := newZFake(t, false)
	deps, err := e.zdeps(key)
	if err != nil {
		t.Fatalf("zdeps: %v", err)
	}
	deps.Base = errStub.srv.URL
	deps.ZcodeBase = errStub.srv.URL
	rec, ok := e.zstore.Get(key)
	if !ok {
		t.Fatal("запись не персистнулась")
	}
	if _, err := e.zchain.EnsurePAT(context.Background(), deps, &rec); err == nil {
		t.Fatal("цепочка против мёртвого login должна падать")
	}
	if st := e.zstore.Status(key); st.State != zcode.StatusNeedsRelogin {
		t.Fatalf("статус = %s, ожидался needs_relogin", st.State)
	}
	if tok, _ := zcodeRow(t, e)["token"].(string); !strings.HasPrefix(tok, "needs_relogin") {
		t.Fatalf("token = %q, ожидался needs_relogin…", tok)
	}
}

// TestWebUIZcodeStatusOKWithPAT — после OAuth + цепочки: email из Store,
// «ok · PAT …» с остатком до expiry (SPEC 12 «PAT live/до N мин»).
func TestWebUIZcodeStatusOKWithPAT(t *testing.T) {
	e := newEnv(t, "")
	key := zcode.AccountKey("main")
	if err := e.zstore.Set(key, zcode.TokenRec{ZaiAccessToken: "zai-tok", UserID: "u-web", Email: "web@z.ai"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	deps, err := e.zdeps(key)
	if err != nil {
		t.Fatalf("zdeps: %v", err)
	}
	rec, ok := e.zstore.Get(key)
	if !ok {
		t.Fatal("запись не персистнулась")
	}
	if _, err := e.zchain.EnsurePAT(context.Background(), deps, &rec); err != nil {
		t.Fatalf("EnsurePAT: %v", err)
	}
	r := zcodeRow(t, e)
	if r["email"] != "web@z.ai" {
		t.Fatalf("email из Store: %v", r["email"])
	}
	if tok, _ := r["token"].(string); !strings.HasPrefix(tok, "ok · PAT ") {
		t.Fatalf("token = %q, ожидался ok · PAT …", tok)
	}
	// usage: план задан → две полосы кредитов (5ч/неделя) с подписью.
	usage := r["usage"].(map[string]any)
	if len(usage["segments"].([]any)) != 2 {
		t.Fatalf("zcode usage segments: %v", usage)
	}
	if lbl, _ := usage["label"].(string); !strings.HasPrefix(lbl, "5ч 0/12 000 cr") {
		t.Fatalf("zcode usage label: %v", lbl)
	}
}
