// E2E-матрица (обе протокола × stream/non-stream против openai-стаба) и
// стрим-механика: keepalive ': ping', idle-таймаут, connect-таймаут (TR-10/12).
// Все интервалы — маленькие значения через параметры сервера, time.Sleep
// запрещён (CODING 1.9): задержки стрима — блокировкой стаба о канал.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// queryRequests — строка requests по алиасу.
func (e *env) queryRequests(t *testing.T, alias string) (prompt, completion, cached, isStream, status int, kind string) {
	t.Helper()
	err := e.queryRow(`SELECT prompt_tokens, completion_tokens, cached_input_tokens,
		is_stream, status, COALESCE(error_kind,'') FROM requests WHERE alias = ?`, alias).
		Scan(&prompt, &completion, &cached, &isStream, &status, &kind)
	if err != nil {
		t.Fatalf("requests(%s): %v", alias, err)
	}
	return
}

// ─── E2E: openai-вход → openai-like апстрим (passthrough) ───────────────────

func TestE2EOpenAINonStream(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	// Passthrough: ответ апстрима как есть.
	if !strings.Contains(body, `"привет"`) || !strings.Contains(body, `"gpt-test"`) {
		t.Fatalf("passthrough-ответ искажён: %s", body)
	}

	// Строка requests: usage из тела (TR-13), модель — апстрим-модель.
	e.waitRow(t, "requests(openai non-stream)", `SELECT 1 FROM requests WHERE alias='work/test' AND is_stream=0 AND protocol='openai'`)
	prompt, completion, cached, isStream, status, kind := e.queryRequests(t, "work/test")
	if prompt != 12 || completion != 34 || cached != 5 {
		t.Fatalf("usage: %d/%d/%d", prompt, completion, cached)
	}
	if isStream != 0 || status != 200 || kind != "" {
		t.Fatalf("is_stream/status/kind: %d/%d/%q", isStream, status, kind)
	}
	var model, protocol, clientKey string
	var account, provider string
	if err := e.queryRow(`SELECT model, protocol, client_key, account_id, provider
		FROM requests WHERE alias='work/test'`).Scan(&model, &protocol, &clientKey, &account, &provider); err != nil {
		t.Fatalf("select: %v", err)
	}
	if model != "gpt-test" || protocol != "openai" || clientKey != "tester" || account != "a1" || provider != "fake" {
		t.Fatalf("row: model=%s protocol=%s client=%s account=%s provider=%s", model, protocol, clientKey, account, provider)
	}
}

func TestE2EOpenAIStream(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"work/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type: %q", ct)
	}
	lines := readSSELines(t, resp)
	joined := strings.Join(lines, "\n")
	// События проходят как есть + [DONE] в финале (TR-4).
	if !strings.Contains(joined, `"привет"`) || !strings.Contains(joined, "finish_reason") {
		t.Fatalf("события апстрима искажены: %s", joined)
	}
	if !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("нет [DONE]: %s", joined)
	}

	// tee-usage passthrough: usage-чанк (choices:[]) доложен в БД
	// (cached у passthrough-чанка нет — апстрим его не прислал).
	e.waitRow(t, "requests(openai stream)", `SELECT 1 FROM requests WHERE alias='work/test' AND is_stream=1 AND protocol='openai'`)
	prompt, completion, cached, isStream, status, kind := e.queryRequests(t, "work/test")
	if prompt != 7 || completion != 9 || cached != 0 {
		t.Fatalf("tee-usage: %d/%d/%d", prompt, completion, cached)
	}
	if isStream != 1 || status != 200 || kind != "" {
		t.Fatalf("is_stream/status/kind: %d/%d/%q", isStream, status, kind)
	}
}

// ─── E2E: anthropic-вход → openai-like апстрим (конвертация) ────────────────

func TestE2EAnthropicNonStream(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/messages", "sk-test-key", []byte(anthropicNonStreamBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	var got struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if got.Type != "message" || got.Role != "assistant" || got.StopReason != "end_turn" {
		t.Fatalf("anthropic-ответ: %+v", got)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "привет" {
		t.Fatalf("content: %+v", got.Content)
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 34 {
		t.Fatalf("usage в ответе: %+v", got.Usage)
	}

	// Строка requests: protocol anthropic, usage из chat-тела.
	e.waitRow(t, "requests(anthropic non-stream)", `SELECT 1 FROM requests WHERE alias='work/test' AND is_stream=0 AND protocol='anthropic'`)
	prompt, completion, cached, isStream, status, kind := e.queryRequests(t, "work/test")
	if prompt != 12 || completion != 34 || cached != 5 {
		t.Fatalf("usage: %d/%d/%d", prompt, completion, cached)
	}
	if isStream != 0 || status != 200 || kind != "" {
		t.Fatalf("is_stream/status/kind: %d/%d/%q", isStream, status, kind)
	}
	var protocol string
	if err := e.queryRow(`SELECT protocol FROM requests WHERE alias='work/test'`).Scan(&protocol); err != nil {
		t.Fatalf("select: %v", err)
	}
	if protocol != "anthropic" {
		t.Fatalf("protocol = %q, ожидался anthropic", protocol)
	}
}

func TestE2EAnthropicStream(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/messages", "sk-test-key",
		[]byte(`{"model":"work/test","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type: %q", ct)
	}
	lines := readSSELines(t, resp)
	joined := strings.Join(lines, "\n")
	// Порядок anthropic-стрима (TR-6): message_start → дельты → message_delta → message_stop.
	for _, want := range []string{"event: message_start", "content_block_delta", "text_delta", "message_delta", "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("нет %q в стриме: %s", want, joined)
		}
	}
	if strings.Index(joined, "message_stop") < strings.Index(joined, "message_delta") {
		t.Fatalf("message_stop раньше message_delta: %s", joined)
	}
	// usage доезжает в message_delta (SPEC 8.5).
	if !strings.Contains(joined, `"output_tokens": 9`) && !strings.Contains(joined, `"output_tokens":9`) {
		t.Fatalf("usage не в message_delta: %s", joined)
	}

	e.waitRow(t, "requests(anthropic stream)", `SELECT 1 FROM requests WHERE alias='work/test' AND is_stream=1 AND protocol='anthropic'`)
	prompt, completion, _, isStream, _, kind := e.queryRequests(t, "work/test")
	if prompt != 7 || completion != 9 {
		t.Fatalf("tee-usage: %d/%d", prompt, completion)
	}
	if isStream != 1 || kind != "" {
		t.Fatalf("is_stream/kind: %d/%q", isStream, kind)
	}
}

// ─── TR-10: keepalive ': ping' при тишине апстрима ──────────────────────────

func TestStreamKeepalivePing(t *testing.T) {
	stall := make(chan struct{})
	e := newEnv(t, envOptions{
		tune: func(p *Params) {
			p.KeepaliveInterval = 40 * time.Millisecond // keepalive — Params (ключа в конфиге нет)
		},
		cfgMutate: func(cfg map[string]any) {
			// idle берётся из снапшота (F1): ставим заведомо больше окна пингов.
			cfg["server"].(map[string]any)["stream_idle_timeout"] = "10s"
		},
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"раз"}}]}`+"\n\n")
			f.Flush()
			<-stall // тишина апстрима, пока тест не соберёт пинги
			fmt.Fprint(w, `data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			f.Flush()
		},
	})
	t.Cleanup(func() { close(stall) })

	var cl http.Client
	cl.Timeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"work/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}

	// Читаем, пока не увидим два пинга подряд (интервал 40ms — пинги идут
	// потоком, время ни на что не «спим»).
	sc := bufio.NewScanner(resp.Body)
	pings, gotChunk := 0, false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "раз") {
			gotChunk = true
		}
		if strings.TrimSpace(line) == ": ping" {
			pings++
			if pings >= 2 {
				break
			}
		}
	}
	cancel()
	if err := resp.Body.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	if !gotChunk {
		t.Fatal("первый чанк апстрима не дошёл")
	}
	if pings < 2 {
		t.Fatalf("получено %d пингов, ожидалось ≥2 (keepalive TR-10)", pings)
	}
}

// ─── TR-12: idle-таймаут → обрыв с SSE-ошибкой в протоколе входа ────────────

func TestStreamIdleTimeout(t *testing.T) {
	stall := make(chan struct{})
	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			cfg["server"].(map[string]any)["stream_idle_timeout"] = "150ms"
		},
		tune: func(p *Params) {
			p.KeepaliveInterval = 10 * time.Second // пинги не успевают до idle
		},
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"раз"}}]}`+"\n\n")
			f.Flush()
			<-stall // апстрим замолчал навсегда — idle обязан оборвать
		},
	})
	t.Cleanup(func() { close(stall) })

	var cl http.Client
	cl.Timeout = 10 * time.Second
	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"work/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d — стрим уже начат", resp.StatusCode)
	}
	lines := readSSELines(t, resp) // до EOF: сервер сам закрывает стрим
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "раз") {
		t.Fatalf("первый чанк потерян: %s", joined)
	}
	// SSE-ошибка openai-выхода + [DONE] (TR-4).
	if !strings.Contains(joined, `"error"`) || !strings.Contains(joined, "stream_idle_timeout") {
		t.Fatalf("нет SSE-ошибки idle: %s", joined)
	}
	if !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("нет [DONE] после idle-ошибки: %s", joined)
	}

	// Учёт: error_kind network, stage stream.
	e.waitRow(t, "requests(idle)", `SELECT 1 FROM requests WHERE error_kind='network'`)
	var kind string
	var isStream int
	if err := e.queryRow(`SELECT error_kind, is_stream FROM requests`).Scan(&kind, &isStream); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if kind != "network" || isStream != 1 {
		t.Fatalf("kind/is_stream: %q/%d", kind, isStream)
	}
	e.waitRow(t, "errors(idle)", `SELECT 1 FROM errors WHERE stage='stream'`)
	var stage string
	if err := e.queryRow(`SELECT stage FROM errors`).Scan(&stage); err != nil {
		t.Fatalf("errors: %v", err)
	}
	if stage != "stream" {
		t.Fatalf("errors stage = %q, ожидался stream", stage)
	}
}

// ─── TR-12: connect-таймаут → 502 network ───────────────────────────────────

func TestConnectTimeout502(t *testing.T) {
	stall := make(chan struct{})
	// Стаб принимает соединение, но заголовки не шлёт никогда.
	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			cfg["server"].(map[string]any)["upstream_connect_timeout"] = "150ms"
		},
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			<-stall
		},
	})
	t.Cleanup(func() { close(stall) })

	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("статус %d, ожидался 502 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "network_error") || !strings.Contains(body, "upstream_connect_timeout") {
		t.Fatalf("конверт: %s", body)
	}
	e.waitRow(t, "requests(connect-timeout)", `SELECT 1 FROM requests WHERE error_kind='network'`)
	var kind string
	var upStatus int
	if err := e.queryRow(`SELECT error_kind, COALESCE(status,0) FROM requests`).Scan(&kind, &upStatus); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if kind != "network" || upStatus != 0 {
		t.Fatalf("kind/status: %q/%d", kind, upStatus)
	}
}

// ─── Обрыв апстрима посреди стрима (FC-9) → SSE-ошибка, errors stage=stream ─

func TestStreamUpstreamInterrupted(t *testing.T) {
	e := newEnv(t, envOptions{
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"начало"}}]}`+"\n\n")
			f.Flush()
			// Грубый обрыв соединения посреди стрима: hijack и закрытие.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("стаб без Hijacker")
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = rw.Flush()
			conn.Close()
		},
	})

	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"work/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	lines := readSSELines(t, resp)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "начало") {
		t.Fatalf("первый чанк потерян: %s", joined)
	}
	if !strings.Contains(joined, "stream_interrupted") || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("нет ошибки обрыва/[DONE]: %s", joined)
	}

	e.waitRow(t, "requests(interrupted)", `SELECT 1 FROM requests WHERE error_kind='network'`)
	var kind, stage string
	if err := e.queryRow(`SELECT error_kind FROM requests`).Scan(&kind); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if kind != "network" {
		t.Fatalf("error_kind = %q, ожидался network", kind)
	}
	e.waitRow(t, "errors(interrupted)", `SELECT 1 FROM errors WHERE stage='stream'`)
	if err := e.queryRow(`SELECT stage FROM errors`).Scan(&stage); err != nil {
		t.Fatalf("errors: %v", err)
	}
	if stage != "stream" {
		t.Fatalf("errors stage = %q, ожидался stream", stage)
	}
}

// ─── Обрыв клиента посреди стрима: прокси отменяет запрос к апстриму ────────

func TestStreamClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamGone := make(chan struct{})
	stall := make(chan struct{})
	e := newEnv(t, envOptions{
		tune: func(p *Params) { p.KeepaliveInterval = 30 * time.Millisecond },
		cfgMutate: func(cfg map[string]any) {
			cfg["server"].(map[string]any)["stream_idle_timeout"] = "30s"
		},
		upstreamHandler: func(e *env, w http.ResponseWriter, r *http.Request) {
			e.upHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"раз"}}]}`+"\n\n")
			f.Flush()
			// Клиент уходит — прокси отменяет ctx запроса; у стаба это видно.
			<-r.Context().Done()
			close(upstreamGone)
			<-stall // держим горутину до конца теста
		},
	})
	t.Cleanup(func() { close(stall) })

	var cl http.Client
	cl.Timeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"work/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	// Прочитали первый чанк и ушли — без ожидания конца стрима.
	sc := bufio.NewScanner(resp.Body)
	sawChunk := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), "раз") {
			sawChunk = true
			break
		}
	}
	if !sawChunk {
		t.Fatal("чанк не получен")
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	cancel()

	select {
	case <-upstreamGone:
		// прокси отменил запрос к апстриму — утечки стрима нет (CC-1)
	case <-time.After(8 * time.Second):
		t.Fatal("прокси не отменил запрос к апстриму после ухода клиента")
	}
}

// httptest stub helper silence
var _ = httptest.NewServer
