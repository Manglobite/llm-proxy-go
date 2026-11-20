// Тесты патча Ф2a-fix1: F1 таймауты из снапшота, F2 catch-all JSON 404,
// F3 FC-9 для non-stream, F5 конверт ошибки до SQLite-вставок.
package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-proxy/internal/config"
)

// ─── F1: idle/connect таймауты берутся из ТЕКУЩЕГО снапшота ─────────────────

// stalledUpstream — стаб, который принимает соединение, но заголовки не шлёт:
// единственный способ увидеть, ЧЕЙ connect-таймаут сработал.
func stalledUpstream(stall <-chan struct{}) func(e *env, w http.ResponseWriter, r *http.Request) {
	return func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		<-stall
	}
}

func TestTimeoutsComeFromSnapshotNotParams(t *testing.T) {
	// Снапшот побеждает Params: в конфиге 150ms, в Params 5s — запрос обязан
	// отвалиться по 150ms. Раньше копия Params пережила бы SIGHUP (F1).
	stall := make(chan struct{})
	e := newEnv(t, envOptions{
		tune:            func(p *Params) { p.ConnectTimeout = 5 * time.Second },
		cfgMutate:       func(cfg map[string]any) { cfg["server"].(map[string]any)["upstream_connect_timeout"] = "150ms" },
		upstreamHandler: stalledUpstream(stall),
	})
	t.Cleanup(func() { close(stall) })

	start := time.Now()
	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	elapsed := time.Since(start)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("запрос ждал %v — использован Params(5s), а не снапшот(150ms)", elapsed)
	}
	if !strings.Contains(body, "upstream_connect_timeout") {
		t.Fatalf("конверт не про connect-таймаут: %s", body)
	}
}

func TestTimeoutsFallbackToParamsOnZeroSnapshot(t *testing.T) {
	// Нулевое значение в снапшоте (собран вручную) — фолбэк на Params(150ms):
	// если код взял снапшотовый ноль «как есть», провайдер не поставил бы
	// таймер вовсе, и запрос повис бы до ResponseHeaderTimeout(60s).
	stall := make(chan struct{})
	e := newEnv(t, envOptions{
		tune:            func(p *Params) { p.ConnectTimeout = 150 * time.Millisecond },
		snapMutate:      func(rc *config.ResolvedConfig) { rc.Server.UpstreamConnectTimeout = 0 },
		upstreamHandler: stalledUpstream(stall),
	})
	t.Cleanup(func() { close(stall) })

	start := time.Now()
	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	elapsed := time.Since(start)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("запрос ждал %v — фолбэк на Params(150ms) не сработал", elapsed)
	}
}

// ─── F2: catch-all JSON 404 и JSON 405 /healthz ─────────────────────────────

func TestCatchAllJSON404(t *testing.T) {
	e := newEnv(t, envOptions{})

	// GET /v1/unknown → 404 в openai-конверте.
	resp := e.do(http.MethodGet, "/v1/unknown", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404 (%s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("catch-all отдал %q, а не JSON (SPEC 4)", ct)
	}
	for _, want := range []string{`"error"`, "invalid_request_error", "not_found"} {
		if !strings.Contains(body, want) {
			t.Fatalf("нет %q в 404: %s", want, body)
		}
	}

	// POST /v1/messages/nonexistent → anthropic-конверт (протокол угадан по пути).
	resp2 := e.do(http.MethodPost, "/v1/messages/nonexistent", "sk-test-key", nil, nil)
	body2 := e.readBody(resp2)
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404 (%s)", resp2.StatusCode, body2)
	}
	for _, want := range []string{`"type":"error"`, "not_found_error"} {
		if !strings.Contains(body2, want) {
			t.Fatalf("нет %q в anthropic-404: %s", want, body2)
		}
	}

	// GET / → 404 JSON (openai по умолчанию).
	resp3 := e.do(http.MethodGet, "/", "sk-test-key", nil, nil)
	body3 := e.readBody(resp3)
	if resp3.StatusCode != http.StatusNotFound || !strings.Contains(body3, `"error"`) {
		t.Fatalf("GET /: %d %s", resp3.StatusCode, body3)
	}

	// POST /healthz → 405 JSON; GET /healthz жив.
	resp4 := e.do(http.MethodPost, "/healthz", "", nil, nil)
	body4 := e.readBody(resp4)
	if resp4.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(body4, `"error"`) {
		t.Fatalf("POST /healthz: %d %s", resp4.StatusCode, body4)
	}
	resp5 := e.do(http.MethodGet, "/healthz", "", nil, nil)
	if body5 := e.readBody(resp5); resp5.StatusCode != http.StatusOK || !strings.Contains(body5, `"ok"`) {
		t.Fatalf("GET /healthz: %d %s", resp5.StatusCode, body5)
	}

	// К апстриму маршрутизация 404 не ходит.
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// ─── F3: обрыв non-stream тела взводит needCheck пути (FC-9) ────────────────

func TestNonStreamBodyBreakReportsPath(t *testing.T) {
	var checkHits atomic.Int64
	// Чек-сервер (как в тестах internal/socks): считаем синхронные проверки.
	check := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkHits.Add(1)
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	defer check.Close()

	stub := newSocksStub(t)

	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			cfg["socks"] = map[string]any{
				"work": map[string]any{
					"url":            stub.url,
					"check_url":      check.URL,
					"check_interval": "1h", // тикер не вмешивается: чеки только по gate
				},
			}
			fake := cfg["providers"].(map[string]any)["fake"].(map[string]any)
			fake["accounts"] = append(fake["accounts"].([]any), map[string]any{
				"id":    "a3",
				"auth":  map[string]any{"method": "api_key", "key": "sk-upstream"},
				"socks": "work",
			})
			cfg["aliases"].(map[string]any)["brk/test"] = map[string]any{
				"account": "fake/a3", "model": "gpt-test",
			}
		},
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			// 200 с обещанным Content-Length, тело обрывается сразу.
			fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"partial\":")
			_ = rw.Flush()
		},
	})

	// Запрос 1: первичный gate-чек (счётчик 0→1), путь healthy, тело рвётся.
	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"brk/test","messages":[{"role":"user","content":"hi"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if got := checkHits.Load(); got != 1 {
		t.Fatalf("чеков после запроса 1: %d, ожидался 1 (первичный gate-чек)", got)
	}
	e.waitRow(t, "requests(non-stream break)", `SELECT 1 FROM requests WHERE error_kind='network'`)
	var kind string
	var upStatus int
	if err := e.queryRow(`SELECT error_kind, status FROM requests`).Scan(&kind, &upStatus); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if kind != "network" || upStatus != 200 {
		t.Fatalf("requests: kind=%q status=%d (апстрим вернул 200 до обрыва)", kind, upStatus)
	}

	// Запрос 2: needCheck взведён обрывом ТЕЛА (FC-9) → синхронный чек (1→2).
	resp2 := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"brk/test","messages":[{"role":"user","content":"hi"}]}`), nil)
	e.readBody(resp2)
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус 2: %d, ожидался 502", resp2.StatusCode)
	}
	if got := checkHits.Load(); got != 2 {
		t.Fatalf("чеков после запроса 2: %d, ожидалось 2 — non-stream обрыв НЕ взвёл needCheck (FC-9)", got)
	}
}

// ─── F5: конверт ошибки уходит клиенту ДО SQLite-вставок ────────────────────

func TestErrorEnvelopeNotDelayedBySQLite(t *testing.T) {
	e := newEnv(t, envOptions{})

	// Держим write-lock на БД: INSERT'ы прокси упрутся в busy_timeout(5s).
	// Конверт 500 обязан прийти быстрее — writeError выполняется до записей.
	lockDB, err := sql.Open("sqlite", e.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("lock db: %v", err)
	}
	defer lockDB.Close()
	tx, err := lockDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS _f5_lock(x INTEGER)`); err != nil {
		t.Fatalf("create lock table: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO _f5_lock VALUES (1)`); err != nil {
		t.Fatalf("take write lock: %v", err)
	}
	defer func() {
		_ = tx.Rollback() // отпускаем лок: зависшие INSERT'ы прокси доедут
	}()

	start := time.Now()
	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"boom","messages":[{"role":"user","content":"hi"}]}`), nil)
	respElapsed := time.Since(start)

	// Тело обязано дочитаться при ВСЁ ЕЩЁ удержанном локе: Content-Length в
	// конверте (F5) завершает ответ без участия хендлера, который в этот
	// момент стоит в INSERT'ах.
	bodyStart := time.Now()
	body := e.readBody(resp)
	bodyElapsed := time.Since(bodyStart)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("статус %d, ожидался 500 (%s)", resp.StatusCode, body)
	}
	if respElapsed >= 3*time.Second || bodyElapsed >= 3*time.Second {
		t.Fatalf("конверт 500: заголовки за %v, тело за %v — writeError ждал SQLite (F5: конверт до INSERT'ов)", respElapsed, bodyElapsed)
	}
	// defer tx.Rollback() отпускает лок: вставки прокси завершатся, хендлер
	// выйдет, cleanup не зависнет.
}
