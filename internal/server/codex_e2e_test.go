package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/upstream/codex"
)

// codexSSE — SSE Responses стаба: дельта + completed с usage (SPEC 8.6).
const codexSSE = "event: response.created\n" +
	`data: {"response":{"id":"resp_1"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"item_id":"m1","delta":"привет"}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"response":{"usage":{"input_tokens":12,"output_tokens":34,` +
	`"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":7}},` +
	`"output":[{"id":"m1","type":"message"}]}}` + "\n\n"

// codexCapture — то, что увидел стаб (потокобезопасно).
type codexCapture struct {
	mu         sync.Mutex
	hits       int
	auth       string
	accountID  string
	originator string
	sessionID  string
	ua         string
	body       string
}

func (c *codexCapture) capture(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits++
	c.auth = r.Header.Get("Authorization")
	c.accountID = r.Header.Get("Chatgpt-Account-Id")
	c.originator = r.Header.Get("Originator")
	c.sessionID = r.Header.Get("Session-Id")
	c.ua = r.Header.Get("User-Agent")
	c.body = string(body)
}

func (c *codexCapture) snapshot() (hits int, auth, accountID, originator, sessionID, ua, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.auth, c.accountID, c.originator, c.sessionID, c.ua, c.body
}

// addCodexProvider — codex-провайдер на стабе и алиас codex/test.
func addCodexProvider(cfg map[string]any, url string) {
	cfg["providers"].(map[string]any)["codex"] = map[string]any{
		"type":          "codex",
		"base_url":      url,
		"auth_base_url": url,
		"accounts": []any{
			map[string]any{
				"id":    "a1",
				"email": "w@x.ru",
				"auth":  map[string]any{"method": "oauth", "callback_port": 1455},
				"socks": "none",
			},
		},
	}
	cfg["aliases"].(map[string]any)["codex/test"] = map[string]any{
		"account": "codex/a1", "model": "gpt-6.1",
	}
}

// codexTune — Senders с codex-модулем (prefilled Store) и quotas-DB.
func codexTune(store *codex.Store, qdb *quotas.DB) func(*Params) {
	return func(p *Params) {
		if p.Senders == nil {
			p.Senders = map[string]Sender{}
		}
		p.Senders[config.ProviderCodex] = codex.Sender{Store: store}
		p.Quotas = qdb
	}
}

// newCodexStore — Store с предзаписанным токеном (или пустой).
func newCodexStore(t *testing.T, token codex.TokenSet) *codex.Store {
	t.Helper()
	store, err := codex.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if token.AccessToken != "" {
		if err := store.Set("codex/a1", token); err != nil {
			t.Fatalf("store.Set: %v", err)
		}
	}
	return store
}

// newCodexEnv — окружение по образцу newGLMEnv: собственный стаб апстрима,
// codex-провайдер на нём, Store и quotas-DB в Params. respond — поведение
// стаба (SSE/429/401); capt заполняется на каждый запрос.
func newCodexEnv(t *testing.T, token codex.TokenSet,
	respond func(cap *codexCapture, w http.ResponseWriter, r *http.Request, body []byte)) (*env, *quotas.DB, *codex.Store, *codexCapture) {

	t.Helper()
	var cap codexCapture
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.capture(r, b)
		respond(&cap, w, r, b)
	}))
	t.Cleanup(stub.Close)

	store := newCodexStore(t, token)
	qpath := filepath.Join(t.TempDir(), "quota.db")
	qdb, err := quotas.Open(qpath)
	if err != nil {
		t.Fatalf("quotas.Open: %v", err)
	}
	t.Cleanup(func() { _ = qdb.Close() })

	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			addCodexProvider(cfg, stub.URL)
		},
		tune: codexTune(store, qdb),
	})
	// Отдельное соединение к quota.db для верификации строк.
	return e, qdb, store, &cap
}

// codexSSEHandler — дефолтный стаб: SSE с x-codex-* заголовками обоих окон.
func codexSSEHandler(_ *codexCapture, w http.ResponseWriter, _ *http.Request, _ []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Codex-Primary-Used-Percent", "42.5")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-At", "1730003600")
	h.Set("X-Codex-Secondary-Primary-Used-Percent", "12")
	h.Set("X-Codex-Secondary-Primary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Primary-Reset-At", "1730870000")
	_, _ = fmt.Fprint(w, codexSSE)
}

// quotaRowEventually — детерминированное ожидание снапшота по kind. Сервер
// пишет учёт ПОСЛЕ отправки ответа клиенту (AC-4), поэтому строка может
// появиться позже чтения тела ответа: ждём УСЛОВИЕ, а не время — каждый
// Recent сам блокируется на SQLite-запросе (доли мс), time.Sleep не нужен
// (CODING 1.9).
func quotaRowEventually(t *testing.T, qdb *quotas.DB, kind string) *quotas.SnapshotRow {
	t.Helper()
	for i := 0; i < 2000; i++ {
		recent, err := qdb.Recent(200)
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		for j := range recent {
			if recent[j].Kind == kind {
				return &recent[j]
			}
		}
	}
	t.Fatalf("снапшот %s не появился за лимит запросов Recent", kind)
	return nil
}

// E2E openai-вход → codex-стаб: SSE агрегирован в chat.completion-чанки,
// заголовки SPEC 7.1 у апстрима, usage в requests, окна x-codex-* в
// quota_snapshots (AC-7).
func TestE2ECodexOpenAIStream(t *testing.T) {
	e, qdb, _, cap := newCodexEnv(t, codex.TokenSet{
		AccessToken: "smoke-preset-token", RefreshToken: "rt", AccountID: "acc_e2e_1",
		ExpiresAt: time.Now().Add(time.Hour),
	}, codexSSEHandler)

	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"codex/test","stream":true,"messages":[{"role":"user","content":"привет"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{`"content":"привет"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE без %q:\n%s", want, body)
		}
	}

	// Заголовки апстрима (OA-7).
	hits, auth, accountID, originator, sessionID, ua, ubody := cap.snapshot()
	if hits != 1 {
		t.Fatalf("hits = %d", hits)
	}
	if auth != "Bearer smoke-preset-token" {
		t.Fatalf("Authorization = %q", auth)
	}
	if accountID != "acc_e2e_1" {
		t.Fatalf("chatgpt-account-id = %q", accountID)
	}
	if originator != "codex_cli_rs" {
		t.Fatalf("originator = %q", originator)
	}
	if len(sessionID) < 32 {
		t.Fatalf("session-id = %q", sessionID)
	}
	if !strings.HasPrefix(ua, "codex_cli_rs/") || !strings.Contains(ua, "llm-proxy") {
		t.Fatalf("User-Agent = %q", ua)
	}
	var reqBody struct {
		Model          string `json:"model"`
		Instructions   string `json:"instructions"`
		Store          bool   `json:"store"`
		Stream         bool   `json:"stream"`
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal([]byte(ubody), &reqBody); err != nil {
		t.Fatalf("тело апстрима не JSON: %v", err)
	}
	if reqBody.Store || !reqBody.Stream || reqBody.Model != "gpt-6.1" {
		t.Fatalf("тело апстрима = %s", ubody)
	}

	// requests: usage codex по таблице SPEC 8.6.
	var prompt, completion, cached, reasoning, isStream int
	var model, proto string
	if err := e.verifyDB.QueryRow(`SELECT model, protocol, prompt_tokens, completion_tokens,
		cached_input_tokens, reasoning_tokens, is_stream FROM requests WHERE alias='codex/test'`).
		Scan(&model, &proto, &prompt, &completion, &cached, &reasoning, &isStream); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if model != "gpt-6.1" || proto != "openai" || prompt != 12 || completion != 34 ||
		cached != 5 || reasoning != 7 || isStream != 1 {
		t.Fatalf("строка requests: model=%s proto=%s usage=%d/%d/%d/%d stream=%d",
			model, proto, prompt, completion, cached, reasoning, isStream)
	}

	// quota_snapshots: оба окна из заголовков ответа (AC-7) — select обратно.
	primary := quotaRowEventually(t, qdb, "codex_primary")
	if primary.UsedPercent != 42.5 || !primary.HasWindow || primary.WindowMinutes != 300 ||
		!primary.HasReset || primary.ResetAt != 1730003600 {
		t.Fatalf("primary = %+v", primary)
	}
	weekly := quotaRowEventually(t, qdb, "codex_weekly")
	if weekly.UsedPercent != 12 || weekly.WindowMinutes != 10080 {
		t.Fatalf("weekly = %+v", weekly)
	}
}

// E2E anthropic-вход → codex: агрегатор Ф1 собирает message (вторая клетка).
func TestE2ECodexAnthropicNonStream(t *testing.T) {
	e, _, _, _ := newCodexEnv(t, codex.TokenSet{
		AccessToken: "t", RefreshToken: "rt", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour),
	}, codexSSEHandler)
	resp := e.postJSON("/v1/messages", "sk-test-key",
		[]byte(`{"model":"codex/test","max_tokens":64,"messages":[{"role":"user","content":"привет"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{`"type":"message"`, `"text":"привет"`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("ответ без %q:\n%s", want, body)
		}
	}
}

// Нет токена — auth-конверт 502 ДО сети (стаб получил 0 запросов).
func TestE2ECodexNoTokenAuthEnvelope(t *testing.T) {
	e, _, _, cap := newCodexEnv(t, codex.TokenSet{}, codexSSEHandler)
	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"codex/test","messages":[{"role":"user","content":"привет"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "auth_error") || !strings.Contains(body, "OAuth") {
		t.Fatalf("конверт без kind=auth/подсказки OAuth: %s", body)
	}
	if hits, _, _, _, _, _, _ := cap.snapshot(); hits != 0 {
		t.Fatalf("апстрим получил %d запросов без токена", hits)
	}
}

// 429 usage_limit_reached: клиенту статус апстрима, в quota_snapshots —
// маркер codex_limited с resets_at (AC-8).
func TestE2ECodex429UsageLimit(t *testing.T) {
	e, qdb, _, _ := newCodexEnv(t, codex.TokenSet{
		AccessToken: "t", RefreshToken: "rt", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour),
	}, func(_ *codexCapture, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","resets_at":1730009999,"message":"limit"}}`))
	})
	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"codex/test","messages":[{"role":"user","content":"привет"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("статус %d, ожидался 429 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "usage_limit_reached") {
		t.Fatalf("конверт без тела апстрима: %s", body)
	}
	limited := quotaRowEventually(t, qdb, "codex_limited")
	if !limited.HasReset || limited.ResetAt != 1730009999 || limited.UsedPercent != 100 {
		t.Fatalf("codex_limited = %+v", limited)
	}
}
