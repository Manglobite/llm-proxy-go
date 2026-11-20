// Тесты прозрачного форварда forward_paths (SV-4/OA-9): префиксы openai-like
// провайдеров форвардятся апстриму как есть, с auth аккаунта и client-key
// auth на прокси; без forward_paths — 404; сетевая ошибка апстрима — 502 +
// строка errors stage=upstream; строка requests не пишется (не запрос модели).
package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// forwardEnv — окружение с forward_paths у дефолтного openai-like провайдера.
func forwardEnv(t *testing.T, handler func(e *env, w http.ResponseWriter, r *http.Request)) *env {
	t.Helper()
	return newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			fake := cfg["providers"].(map[string]any)["fake"].(map[string]any)
			fake["forward_paths"] = []any{"/api/tags", "/api/show"}
		},
		upstreamHandler: handler,
	})
}

// GET /api/tags?x=1: апстрим получил ровно путь+query, Authorization аккаунта,
// клиентский ключ прокси НЕ утёк; тело и статус — как есть.
func TestForwardTagsWithAuthAndQuery(t *testing.T) {
	var cap upstreamCapture
	e := forwardEnv(t, func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		cap.capture(r, nil)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "from-upstream")
		_, _ = w.Write([]byte(`{"models":[{"name":"gpt-test"}]}`))
	})

	resp := e.do(http.MethodGet, "/api/tags?x=1", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	// Тело и заголовки ответа — как есть.
	if body != `{"models":[{"name":"gpt-test"}]}` {
		t.Fatalf("тело искажено: %s", body)
	}
	if resp.Header.Get("X-Custom-Header") != "from-upstream" {
		t.Fatalf("заголовок апстрима потерян: %v", resp.Header)
	}

	// Апстрим-запрос: путь+query, auth аккаунта, без секрета клиента.
	path, auth, _, query, _ := cap.snapshot()
	if path != "/api/tags" || query != "x=1" {
		t.Fatalf("path=%s query=%s", path, query)
	}
	if auth != "Bearer sk-upstream" {
		t.Fatalf("authorization = %q, ожидался ключ аккаунта", auth)
	}

	// requests: строки НЕТ (не запрос модели, SPEC 9).
	e.waitCond(t, "requests пуст", func() (bool, error) {
		var n int
		if err := e.queryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
			return false, err
		}
		return n == 0, nil
	})
	var n int
	if err := e.queryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("requests rows = %d (err=%v), ожидался 0", n, err)
	}
}

// POST /api/show: метод и тело форвардятся как есть.
func TestForwardShowWithBody(t *testing.T) {
	var cap upstreamCapture
	e := forwardEnv(t, func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		cap.capture(r, b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"modelfile":"FROM x"}`))
	})

	resp := e.postJSON("/api/show", "sk-test-key", []byte(`{"name":"gpt-test"}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK || body != `{"modelfile":"FROM x"}` {
		t.Fatalf("ответ: %d %s", resp.StatusCode, body)
	}
	path, _, _, _, upBody := cap.snapshot()
	if path != "/api/show" {
		t.Fatalf("path = %s", path)
	}
	if upBody != `{"name":"gpt-test"}` {
		t.Fatalf("тело не дошло как есть: %q", upBody)
	}
}

// Не-2xx апстрима при форварде прозрачен: статус и тело апстрима без конверта.
func TestForwardUpstreamStatusPassthrough(t *testing.T) {
	e := forwardEnv(t, func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such model"}`))
	})
	resp := e.do(http.MethodGet, "/api/tags", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusNotFound || body != `{"error":"no such model"}` {
		t.Fatalf("не-2xx должен пройти как есть: %d %s", resp.StatusCode, body)
	}
}

// Без forward_paths в конфиге — 404 JSON, апстрим не трогается (SV-4).
func TestForward404WithoutConfig(t *testing.T) {
	e := newEnv(t, envOptions{}) // провайдер fake без forward_paths

	resp := e.do(http.MethodGet, "/api/tags", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("404 не JSON-конверт: %s", body)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// Путь вне объявленных префиксов — тоже 404.
func TestForwardUnlistedPrefix404(t *testing.T) {
	e := forwardEnv(t, func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	resp := e.do(http.MethodGet, "/api/ps", "sk-test-key", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404", resp.StatusCode)
	}
	e.readBody(resp)
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// Auth: без client key — 401, апстрим не трогается (SV-4).
func TestForwardUnauthorized401(t *testing.T) {
	e := forwardEnv(t, func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	resp := e.do(http.MethodGet, "/api/tags", "", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "invalid_api_key") {
		t.Fatalf("статус %d (%s), ожидался 401", resp.StatusCode, body)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// Форвард идёт ЧЕРЕЗ gate SOCKS-пути аккаунта: blocked путь → 503
// socks_check, к апстриму ноль коннектов (SV-4 + FC-5/I4).
func TestForwardBlockedSocks503(t *testing.T) {
	addr := deadPort(t)
	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			cfg["socks"] = map[string]any{
				"dead": map[string]any{"url": "socks5h://" + addr, "check_interval": "1h"},
			}
			fake := cfg["providers"].(map[string]any)["fake"].(map[string]any)
			fake["forward_paths"] = []any{"/api/tags"}
			// Единственный аккаунт на блокированном пути: форвард обязан уйти
			// в 503 до обращения к апстриму.
			fake["accounts"].([]any)[0].(map[string]any)["socks"] = "dead"
		},
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			_, _ = w.Write([]byte(`{}`))
		},
	})

	resp := e.do(http.MethodGet, "/api/tags", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("статус %d, ожидался 503 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "socks_blocked") || !strings.Contains(body, "dead") {
		t.Fatalf("конверт 503 без имени пути/причины: %s", body)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0 (gate до апстрима)", n)
	}
	e.waitRow(t, "errors(forward blocked)", `SELECT 1 FROM errors WHERE stage='socks_check'`)
}

// Сетевая ошибка апстрима: 502 + строка errors stage=upstream, requests пуст.
func TestForwardUpstreamUnreachable502(t *testing.T) {
	e := forwardEnv(t, func(e *env, w http.ResponseWriter, r *http.Request) {
		e.upHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	e.upstream.Close() // апстрим умирает: форвард упрётся в refused

	resp := e.do(http.MethodGet, "/api/tags", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "network_error") {
		t.Fatalf("конверт 502: %s", body)
	}

	e.waitRow(t, "errors(forward)", `SELECT 1 FROM errors WHERE stage='upstream'`)
	var stage, url string
	if err := e.queryRow(`SELECT stage, request_url FROM errors ORDER BY id DESC LIMIT 1`).Scan(&stage, &url); err != nil {
		t.Fatalf("errors row: %v", err)
	}
	if stage != "upstream" {
		t.Fatalf("stage = %q", stage)
	}
	if !strings.Contains(url, "/api/tags") {
		t.Fatalf("в дампе нет URL форварда: %q", url)
	}
	// Секрет апстрима маскируется в дампе.
	var reqHeaders string
	if err := e.queryRow(`SELECT request_headers FROM errors ORDER BY id DESC LIMIT 1`).Scan(&reqHeaders); err != nil {
		t.Fatalf("headers: %v", err)
	}
	if strings.Contains(reqHeaders, "sk-upstream") || !strings.Contains(reqHeaders, "masked") {
		t.Fatalf("authorization не замаскирован: %s", reqHeaders)
	}
	var n int
	if err := e.queryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("requests rows = %d, ожидался 0", n)
	}
}
