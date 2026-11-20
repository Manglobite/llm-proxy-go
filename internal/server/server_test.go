// Тесты сервера (Ф2a): полный стек httptest — апстрим-стаб + прокси.
// Отдельные сценарии: fail-closed (FC-5/FC-10), auth (SV-1), роутинг 404 (SV-2),
// /v1/models (SV-3), decode gzip/zstd/413 (TR-11), учёт и errors (AC-2..AC-4).
package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite" // верификация строк в БД напрямую

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream/openailike"
)

// zstdNewWriter — кодировщик zstd для тестов (декодер проверяет сервер).
func zstdNewWriter(w io.Writer) (*zstd.Encoder, error) { return zstd.NewWriter(w) }

// env — окружение полного стека: апстрим-стаб + прокси + SQLite.
type env struct {
	t *testing.T

	upstream *httptest.Server
	upHits   atomic.Int64 // коннекты к апстрим-стабу (fail-closed ассертит ноль)

	proxy    *httptest.Server
	cfg      *config.ResolvedConfig
	dbPath   string
	verifyDB *sql.DB
	acct     *accounting.DB
	elog     *errorslog.DB
	paths    map[string]*socks.Path //Transporter-пути снапшота (для инспекции)
}

// envOptions — настройка окружения конкретным тестом.
type envOptions struct {
	// cfgMutate правит сырой конфиг (картой) до валидации.
	cfgMutate func(cfg map[string]any)
	// snapMutate правит резолвнутый конфиг ПОСЛЕ Load (до сборки снапшота):
	// позволяет получить значения, которых config.Load не оставит — например
	// нулевые таймауты (фолбэк на Params).
	snapMutate func(rc *config.ResolvedConfig)
	// tune правит server.Params до сборки (маленькие таймауты/капы).
	tune func(*Params)
	// upstreamHandler — поведение апстрим-стаба (дефолт: openai-стаб).
	// e валидна к моменту первого запроса: замыкание ловит переменную.
	upstreamHandler func(e *env, w http.ResponseWriter, r *http.Request)
}

// defaultUpstream — openai-стаб: non-stream JSON и стрим чанков с usage;
// модель "boom" → 500 с Retry-After (форс ошибки апстрима).
func (e *env) defaultUpstream(w http.ResponseWriter, r *http.Request) {
	e.upHits.Add(1)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(b, &body)
	if body.Model == "boom" {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
		return
	}
	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-test",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"привет"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":12,"completion_tokens":34,"prompt_tokens_details":{"cached_tokens":5}}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	for _, chunk := range []string{
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"привет"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		f.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	f.Flush()
}

// newEnv собирает окружение: стаб апстрима → конфиг → socks-пути → БД → сервер.
func newEnv(t *testing.T, opts envOptions) *env {
	t.Helper()
	// Замыкание ловит ПЕРЕМЕННУЮ e: к моменту первого запроса она присвоена.
	var e *env
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.upstreamHandler == nil {
			e.defaultUpstream(w, r)
			return
		}
		opts.upstreamHandler(e, w, r)
	})
	e = &env{t: t}
	e.upstream = httptest.NewServer(handler)
	t.Cleanup(e.upstream.Close)

	cfgJSON := map[string]any{
		"server": map[string]any{
			"listen":  "127.0.0.1:0",
			"clients": map[string]string{"tester": "sk-test-key"},
		},
		"storage": map[string]any{"db_path": "proxy.db"},
		"providers": map[string]any{
			"fake": map[string]any{
				"type":     "openai-like",
				"base_url": e.upstream.URL,
				"accounts": []any{
					map[string]any{
						"id":    "a1",
						"auth":  map[string]any{"method": "api_key", "key": "sk-upstream"},
						"socks": "none",
					},
				},
			},
		},
		"aliases": map[string]any{
			"work/test": map[string]any{"account": "fake/a1", "model": "gpt-test"},
			"boom":      map[string]any{"account": "fake/a1", "model": "boom"},
		},
	}
	if opts.cfgMutate != nil {
		opts.cfgMutate(cfgJSON)
	}
	raw, err := json.Marshal(cfgJSON)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	rc, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if opts.snapMutate != nil {
		opts.snapMutate(rc)
	}
	e.cfg = rc
	e.dbPath = rc.Storage.DBPath

	// Путь socks строим, но Run не запускаем: проверки идут по требованию
	// через gate (как в тестах internal/socks).
	paths := make(map[string]*socks.Path, len(rc.Socks))
	for name, sc := range rc.Socks {
		p, err := socks.New(name, sc)
		if err != nil {
			t.Fatalf("socks.New(%s): %v", name, err)
		}
		paths[name] = p
	}
	e.paths = paths
	snap := &config.Snapshot{Cfg: rc, Socks: paths}

	e.acct, err = accounting.Open(e.dbPath)
	if err != nil {
		t.Fatalf("accounting.Open: %v", err)
	}
	e.elog, err = errorslog.Open(e.dbPath)
	if err != nil {
		t.Fatalf("errorslog.Open: %v", err)
	}
	// Отдельное соединение для верификации строк (server не экспонирует пул).
	e.verifyDB, err = sql.Open("sqlite", e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("verify db: %v", err)
	}

	p := Params{
		Snapshot:   func() *config.Snapshot { return snap },
		Sender:     openailike.Sender{},
		Accounting: e.acct,
		ErrorsLog:  e.elog,
	}
	if opts.tune != nil {
		opts.tune(&p)
	}
	srv, err := New(p)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	e.proxy = httptest.NewServer(srv.Handler())

	t.Cleanup(func() {
		e.proxy.Close()
		e.verifyDB.Close()
		e.elog.Close()
		e.acct.Close()
	})
	return e
}

// do — произвольный метод/путь на прокси (для catch-all и method-guard тестов).
func (e *env) do(method, path, key string, body []byte, extra map[string]string) *http.Response {
	e.t.Helper()
	var cl http.Client // объявление без литерала-инициализатора: I1 grep смотрит за скобками
	cl.Timeout = 15 * time.Second
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.proxy.URL+path, rd)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// postJSON — POST на прокси с клиентским ключом.
func (e *env) postJSON(path, key string, body []byte, extra map[string]string) *http.Response {
	e.t.Helper()
	return e.do(http.MethodPost, path, key, body, extra)
}

// readBody дочитывает и закрывает тело ответа.
func (e *env) readBody(resp *http.Response) string {
	e.t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		e.t.Logf("close body: %v", err)
	}
	return string(b)
}

// queryRow — скалярный запрос к верификационной БД.
func (e *env) queryRow(q string, args ...any) *sql.Row {
	e.t.Helper()
	return e.verifyDB.QueryRow(q, args...)
}

// waitRow ждёт появления строки в БД: конверт клиенту теперь уходит ДО
// SQLite-вставок (F5), поэтому строки появляются асинхронно относительно
// получения ответа. Опрос коротким таймером — фиксированный sleep запрещён
// (CODING 1.9), дедлайн страхует от вечного цикла.
func (e *env) waitRow(t *testing.T, what, q string, args ...any) {
	t.Helper()
	e.waitCond(t, what, func() (bool, error) {
		var one int
		err := e.verifyDB.QueryRow(q, args...).Scan(&one)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	})
}

// waitCond опрашивает условие до дедлайна (5s).
func (e *env) waitCond(t *testing.T, what string, check func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	delay := time.NewTimer(5 * time.Millisecond)
	defer delay.Stop()
	for {
		ok, err := check()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s: условие не выполнено за 5s", what)
		}
		<-delay.C
		delay.Reset(5 * time.Millisecond)
	}
}

// deadPort — гарантированно закрытый порт на localhost (connection refused
// мгновенно, без таймаутов).
func deadPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

const (
	openaiNonStreamBody    = `{"model":"work/test","messages":[{"role":"user","content":"hi"}]}`
	anthropicNonStreamBody = `{"model":"work/test","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
)

// ─── FC-10: fail-closed полный стек ─────────────────────────────────────────

func TestFailClosedDeadSocksFullStack(t *testing.T) {
	addr := deadPort(t)
	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			cfg["socks"] = map[string]any{
				"dead": map[string]any{"url": "socks5h://" + addr},
			}
			fake := cfg["providers"].(map[string]any)["fake"].(map[string]any)
			fake["accounts"] = append(fake["accounts"].([]any), map[string]any{
				"id":    "a2",
				"auth":  map[string]any{"method": "api_key", "key": "sk-upstream"},
				"socks": "dead",
			})
			cfg["aliases"].(map[string]any)["dead/test"] = map[string]any{
				"account": "fake/a2", "model": "gpt-test",
			}
		},
	})

	// openai-вход: 503, конверт openai, имя пути и причина в сообщении.
	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(`{"model":"dead/test","messages":[{"role":"user","content":"hi"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("статус %d, ожидался 503 (тело: %s)", resp.StatusCode, body)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("тело не openai-конверт: %v (%s)", err, body)
	}
	if !strings.Contains(got.Error.Message, "dead") || !strings.Contains(got.Error.Message, "blocked") {
		t.Fatalf("в сообщении нет имени пути/причины: %q", got.Error.Message)
	}
	if got.Error.Code != "socks_blocked" || got.Error.Type != "api_error" {
		t.Fatalf("type/code: %q/%q", got.Error.Type, got.Error.Code)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: %q (никогда HTML, SV-2)", ct)
	}
	// FC-10: апстрим-стаб получил НОЛЬ коннектов.
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}

	// anthropic-вход на тот же путь: 503 конвертом anthropic.
	resp2 := e.postJSON("/v1/messages", "sk-test-key",
		[]byte(`{"model":"dead/test","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"X-Api-Key": "sk-test-key", "Authorization": ""})
	body2 := e.readBody(resp2)
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("anthropic статус %d, ожидался 503 (%s)", resp2.StatusCode, body2)
	}
	var got2 struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body2), &got2); err != nil {
		t.Fatalf("тело не anthropic-конверт: %v (%s)", err, body2)
	}
	if got2.Type != "error" || got2.Error.Type != "api_error" || !strings.Contains(got2.Error.Message, "dead") {
		t.Fatalf("anthropic-конверт: %+v", got2)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов после anthropic-запроса, ожидался 0", n)
	}

	// errorslog: stage=socks_check, статус 503 (I4: текст ошибки и в errors).
	e.waitRow(t, "errors(socks_check)", `SELECT 1 FROM errors WHERE stage='socks_check'`)
	var stage string
	var status int
	if err := e.queryRow(`SELECT stage, status FROM errors ORDER BY id DESC LIMIT 1`).Scan(&stage, &status); err != nil {
		t.Fatalf("errors row: %v", err)
	}
	if stage != "socks_check" || status != 503 {
		t.Fatalf("errors: stage=%q status=%d", stage, status)
	}
}

// ─── SV-1: auth клиентов ────────────────────────────────────────────────────

func TestAuthUnknownKey401(t *testing.T) {
	e := newEnv(t, envOptions{})

	cases := []struct {
		name     string
		path     string
		headers  map[string]string
		protocol string
	}{
		{"нет ключа", "/v1/chat/completions", nil, "openai"},
		{"неизвестный bearer", "/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-wrong"}, "openai"},
		{"нет ключа anthropic", "/v1/messages", nil, "anthropic"},
		{"неизвестный x-api-key", "/v1/messages", map[string]string{"X-Api-Key": "sk-wrong"}, "anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.postJSON(tc.path, "", []byte(`{"model":"work/test"}`), tc.headers)
			body := e.readBody(resp)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("статус %d, ожидался 401 (%s)", resp.StatusCode, body)
			}
			if tc.protocol == "openai" {
				if !strings.Contains(body, `"error"`) || !strings.Contains(body, "invalid_api_key") {
					t.Fatalf("openai-конверт 401: %s", body)
				}
			} else {
				if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "authentication_error") {
					t.Fatalf("anthropic-конверт 401: %s", body)
				}
			}
		})
	}
	// Валидный ключ допускается обоими заголовками на обоих протоколах.
	resp := e.postJSON("/v1/chat/completions", "", []byte(openaiNonStreamBody), map[string]string{"X-Api-Key": "sk-test-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key на openai-входе: %d (%s)", resp.StatusCode, e.readBody(resp))
	}
	resp2 := e.postJSON("/v1/messages", "sk-test-key", []byte(anthropicNonStreamBody), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Bearer на anthropic-входе: %d (%s)", resp2.StatusCode, e.readBody(resp2))
	}
}

// ─── SV-2: неизвестный алиас → 404 со списком ───────────────────────────────

func TestUnknownAlias404WithList(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(`{"model":"no/such","messages":[]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{"model_not_found", "work/test", "boom"} {
		if !strings.Contains(body, want) {
			t.Fatalf("в 404 нет %q: %s", want, body)
		}
	}
	// anthropic-конверт.
	resp2 := e.postJSON("/v1/messages", "sk-test-key", []byte(`{"model":"no/such","max_tokens":1,"messages":[]}`), nil)
	body2 := e.readBody(resp2)
	if resp2.StatusCode != http.StatusNotFound || !strings.Contains(body2, "not_found_error") {
		t.Fatalf("anthropic 404: %d %s", resp2.StatusCode, body2)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0 (роутинг до апстрима)", n)
	}
}

// ─── SV-3: /v1/models ───────────────────────────────────────────────────────

func TestModelsList(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/models", "sk-test-key", nil, nil)
	// postJSON шлёт POST — /v1/models принимает GET: проверяем 405-конверт.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models: %d, ожидался 405", resp.StatusCode)
	}
	if body := e.readBody(resp); !strings.Contains(body, `"error"`) {
		t.Fatalf("405 не конверт: %s", body)
	}

	var cl http.Client
	cl.Timeout = 5 * time.Second
	req, err := http.NewRequest(http.MethodGet, e.proxy.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp2, err := cl.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	body := e.readBody(resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models: %d (%s)", resp2.StatusCode, body)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("models: %+v", got)
	}
	// Порядок отсортирован, owned_by = provider/account.
	if got.Data[0].ID != "boom" || got.Data[0].Object != "model" || got.Data[0].OwnedBy != "fake/a1" {
		t.Fatalf("data[0]: %+v", got.Data[0])
	}
	if got.Data[1].ID != "work/test" || got.Data[1].OwnedBy != "fake/a1" {
		t.Fatalf("data[1]: %+v", got.Data[1])
	}

	// Без ключа — 401.
	req3, _ := http.NewRequest(http.MethodGet, e.proxy.URL+"/v1/models", nil)
	resp3, err := cl.Do(req3)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("models без ключа: %d", resp3.StatusCode)
	}
	e.readBody(resp3)
}

// ─── TR-11: decode identity|gzip|zstd, 413 на бомбу ─────────────────────────

func TestDecodeGzipZstd(t *testing.T) {
	e := newEnv(t, envOptions{})

	// gzip
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, _ = gz.Write([]byte(openaiNonStreamBody))
	_ = gz.Close()
	resp := e.postJSON("/v1/chat/completions", "sk-test-key", gzBuf.Bytes(),
		map[string]string{"Content-Encoding": "gzip"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gzip: %d (%s)", resp.StatusCode, e.readBody(resp))
	}
	e.readBody(resp)

	// zstd
	var zzBuf bytes.Buffer
	zw, err := zstdNewWriter(&zzBuf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	_, _ = zw.Write([]byte(openaiNonStreamBody))
	_ = zw.Close()
	resp2 := e.postJSON("/v1/chat/completions", "sk-test-key", zzBuf.Bytes(),
		map[string]string{"Content-Encoding": "zstd"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("zstd: %d (%s)", resp2.StatusCode, e.readBody(resp2))
	}
	e.readBody(resp2)

	// Неизвестная кодировка → 415.
	resp3 := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody),
		map[string]string{"Content-Encoding": "br"})
	if resp3.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("br: %d, ожидался 415", resp3.StatusCode)
	}
	e.readBody(resp3)
}

func TestDecodeBomb413(t *testing.T) {
	// Кап 1024 байта: тело ~40 байт сжатого, но 10000 распакованного — 413
	// (анти-декомпрессионная бомба; SPEC 4).
	e := newEnv(t, envOptions{
		tune: func(p *Params) { p.MaxBodyBytes = 1024 },
	})

	payload := bytes.Repeat([]byte("0"), 10000)

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, _ = gz.Write(payload)
	_ = gz.Close()
	resp := e.postJSON("/v1/chat/completions", "sk-test-key", gzBuf.Bytes(),
		map[string]string{"Content-Encoding": "gzip"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("gzip-бомба: %d, ожидался 413 (%s)", resp.StatusCode, e.readBody(resp))
	}
	e.readBody(resp)

	var zzBuf bytes.Buffer
	zw, err := zstdNewWriter(&zzBuf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	_, _ = zw.Write(payload)
	_ = zw.Close()
	resp2 := e.postJSON("/v1/chat/completions", "sk-test-key", zzBuf.Bytes(),
		map[string]string{"Content-Encoding": "zstd"})
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("zstd-бомба: %d, ожидался 413", resp2.StatusCode)
	}
	e.readBody(resp2)
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// ─── Ошибки апстрима: строка в errors, маскированные заголовки, статус ──────

func TestUpstream500RecordedAndProxied(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"boom","messages":[{"role":"user","content":"hi"}]}`), nil)
	body := e.readBody(resp)
	// SPEC 4: клиенту — статус апстрима.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("статус %d, ожидался 500 апстрима (%s)", resp.StatusCode, body)
	}
	// retry-after пробрасывается (SPEC 4).
	if ra := resp.Header.Get("Retry-After"); ra != "7" {
		t.Fatalf("Retry-After = %q, ожидался 7", ra)
	}
	if !strings.Contains(body, "upstream exploded") || !strings.Contains(body, `"code":"upstream_error"`) {
		t.Fatalf("конверт 500: %s", body)
	}

	// errors: полный дамп, маскированные заголовки (AC-2), кап не нужен.
	e.waitRow(t, "errors(upstream)", `SELECT 1 FROM errors WHERE stage='upstream'`)
	var reqHeaders, respBody string
	var reqBody []byte
	var stage string
	var status int
	err := e.queryRow(`SELECT stage, status, request_headers, request_body, response_body
		FROM errors ORDER BY id DESC LIMIT 1`).Scan(&stage, &status, &reqHeaders, &reqBody, &respBody)
	if err != nil {
		t.Fatalf("errors row: %v", err)
	}
	if stage != "upstream" || status != 500 {
		t.Fatalf("stage/status: %q/%d", stage, status)
	}
	if strings.Contains(reqHeaders, "sk-upstream") {
		t.Fatalf("ключ апстрима просочился в дамп: %s", reqHeaders)
	}
	// "<" в JSON-строке уходит как \u003c — проверяем слово masked.
	if !strings.Contains(reqHeaders, "masked") {
		t.Fatalf("authorization не замаскирован: %s", reqHeaders)
	}
	if !strings.Contains(string(reqBody), `"boom"`) {
		t.Fatalf("в дампе запроса нет тела: %s", reqBody)
	}
	if !strings.Contains(respBody, "upstream exploded") {
		t.Fatalf("в дампе ответа нет тела: %s", respBody)
	}

	// requests: строка с error_kind=upstream (учёт и ошибок, SPEC 9).
	e.waitRow(t, "requests(upstream)", `SELECT 1 FROM requests WHERE error_kind='upstream'`)
	var kind string
	var upStatus int
	if err := e.queryRow(`SELECT error_kind, status FROM requests`).Scan(&kind, &upStatus); err != nil {
		t.Fatalf("requests row: %v", err)
	}
	if kind != "upstream" || upStatus != 500 {
		t.Fatalf("requests: kind=%q status=%d", kind, upStatus)
	}
}

// ─── Translate-ошибка → 400, stage=translate ────────────────────────────────

func TestTranslateError400Stage(t *testing.T) {
	e := newEnv(t, envOptions{})

	// anthropic-вход без max_tokens (TR-7): ParseAnthropic в провайдере.
	resp := e.postJSON("/v1/messages", "sk-test-key",
		[]byte(`{"model":"work/test","messages":[{"role":"user","content":"hi"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "max_tokens") {
		t.Fatalf("anthropic-конверт 400: %s", body)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}

	e.waitRow(t, "errors(translate)", `SELECT 1 FROM errors WHERE stage='translate'`)
	var stage, msg string
	var status int
	if err := e.queryRow(`SELECT stage, status, error FROM errors ORDER BY id DESC LIMIT 1`).Scan(&stage, &status, &msg); err != nil {
		t.Fatalf("errors row: %v", err)
	}
	if stage != "translate" || status != 400 {
		t.Fatalf("errors: stage=%q status=%d", stage, status)
	}
	if !strings.Contains(msg, "max_tokens") {
		t.Fatalf("error текст: %q", msg)
	}

	// openai-вход с битым JSON — тоже 400 (и дамп stage=translate).
	resp2 := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(`{"model":`), nil)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("битый json: %d, ожидался 400", resp2.StatusCode)
	}
	e.readBody(resp2)
	e.waitCond(t, "две translate-строки в errors", func() (bool, error) {
		var cnt int
		if err := e.queryRow(`SELECT COUNT(*) FROM errors WHERE stage='translate'`).Scan(&cnt); err != nil {
			return false, err
		}
		return cnt >= 2, nil
	})
	var cnt int
	if err := e.queryRow(`SELECT COUNT(*) FROM errors WHERE stage='translate'`).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt < 2 {
		t.Fatalf("в errors %d translate-строк, ожидалось 2", cnt)
	}
}

// ─── Соединение с апстримом: сетевая ошибка → 502 network ───────────────────

func TestUpstreamConnectionRefused502(t *testing.T) {
	// Апстрим-стаб закрываем сразу: запрос идёт на мёртвый порт → 502.
	e := newEnv(t, envOptions{})
	e.upstream.Close()

	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "network_error") {
		t.Fatalf("конверт 502: %s", body)
	}
	e.waitRow(t, "requests(network)", `SELECT 1 FROM requests WHERE error_kind='network'`)
	var kind string
	if err := e.queryRow(`SELECT error_kind FROM requests`).Scan(&kind); err != nil {
		t.Fatalf("requests row: %v", err)
	}
	if kind != "network" {
		t.Fatalf("error_kind = %q, ожидался network", kind)
	}
}

// ─── AC-4: сбой БД не роняет ответ клиенту ──────────────────────────────────

func TestDBFailureDoesNotBreakResponse(t *testing.T) {
	e := newEnv(t, envOptions{})
	// Ломаем обе БД до запроса: вставки обязаны уйти в лог, а не в ответ.
	if err := e.acct.Close(); err != nil {
		t.Fatalf("close acct: %v", err)
	}
	if err := e.elog.Close(); err != nil {
		t.Fatalf("close elog: %v", err)
	}

	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d при сломанной БД, ожидался 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"привет"`) {
		t.Fatalf("ответ искажён при сломанной БД: %s", body)
	}
}

// ─── Стрим-помощники (используются e2e_test.go) ─────────────────────────────

// readSSELines читает строки SSE до EOF (с дедлайном клиента).
func readSSELines(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("чтение SSE: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	return lines
}
