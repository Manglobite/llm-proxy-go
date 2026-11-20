package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/config"
)

func TestForwardSlowBodyOutlivesConnectTimeout(t *testing.T) {
	const connect = 100 * time.Millisecond
	release := make(chan struct{})
	// Тело уходит через 3*connect — дольше таймаута. Закрытие по таймеру
	// (не time.Sleep): таймер всегда выстрелит, горутина стаба не зависнет.
	time.AfterFunc(3*connect, func() { close(release) })

	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			fake := cfg["providers"].(map[string]any)["fake"].(map[string]any)
			fake["forward_paths"] = []any{"/api/tags"}
		},
		// Снапшот-таймаут = 0 → форвард берёт значение из Params (F1-фолбэк):
		// так в тест попадает малый connect без правки живого снапшота.
		snapMutate: func(rc *config.ResolvedConfig) {
			rc.Server.UpstreamConnectTimeout = 0
		},
		tune: func(p *Params) { p.ConnectTimeout = connect },
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			// Заголовки — сразу: Do возвращается до выстрела таймера.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-release // тело молчит дольше connect-таймаута
			_, _ = io.WriteString(w, `{"models":[{"name":"gpt-test"}]}`)
		},
	})

	resp := e.do(http.MethodGet, "/api/tags", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, ожидался 200 — connect-таймер убил чтение тела (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "gpt-test") {
		t.Fatalf("тело не дочитано: %q", body)
	}
}
