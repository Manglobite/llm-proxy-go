// Тест монтирования WebUI (Ф4): точные маршруты webui главнее префикса /api/
// форварда (ServeMux-специфичность), JSON-404 catch-all сохраняется (F2),
// WebUI == nil — прежнее поведение (SPEC 12, ARCHITECTURE §11).
package server

import (
	"net/http"
	"strings"
	"testing"
)

// stubWebUI — заглушка webui.Handler: помечает запросы и отдаёт точные паттерны
// (реальный webui в server не импортируется — интерфейс объявлен потребителем).
type stubWebUI struct{ hits int }

func (s *stubWebUI) ServeHTTP(_ http.ResponseWriter, r *http.Request) { s.hits++ }

func (s *stubWebUI) Patterns() []string {
	return []string{"/{$}", "/api/status", "/src/app/main.js"}
}

// TestWebUIMounted — WebUI подключён: "/" и его маршруты уходят в webui,
// /api/tags (форвард, не webui-маршрут) — нет, /v1/unknown — JSON 404.
func TestWebUIMounted(t *testing.T) {
	stub := &stubWebUI{}
	e := newEnv(t, envOptions{tune: func(p *Params) { p.WebUI = stub }})

	for _, path := range []string{"/", "/api/status", "/src/app/main.js"} {
		resp := e.do(http.MethodGet, path, "sk-test-key", nil, nil)
		_ = resp
		if stub.hits == 0 {
			t.Fatalf("%s не попал в WebUI", path)
		}
		stub.hits = 0
	}
	// /api/tags — форвард-префикс (нет провайдера с forward_paths → JSON 404),
	// но точно не webui.
	resp := e.do(http.MethodGet, "/api/tags", "sk-test-key", nil, nil)
	if stub.hits != 0 {
		t.Fatalf("/api/tags попал в WebUI")
	}
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(e.readBody(resp), `"error"`) {
		t.Fatalf("/api/tags без forward_paths: %d", resp.StatusCode)
	}
	// /v1/unknown — JSON 404 (не index.html: SPEC 4 требует JSON для /v1/*).
	resp = e.do(http.MethodGet, "/v1/unknown", "sk-test-key", nil, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(e.readBody(resp), `"error"`) {
		t.Fatalf("/v1/unknown: %d", resp.StatusCode)
	}
}

// TestWebUINil — WebUI не задан: "/" — прежний JSON 404 (узкие сборки/тесты).
func TestWebUINil(t *testing.T) {
	e := newEnv(t, envOptions{})
	resp := e.do(http.MethodGet, "/", "sk-test-key", nil, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(e.readBody(resp), `"error"`) {
		t.Fatalf("/ без WebUI: %d", resp.StatusCode)
	}
}
