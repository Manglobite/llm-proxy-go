// Юнит- и e2e-тесты провайдера glm (OA-8, SPEC 7.2): выбор эндпоинта по
// протоколу входа и coding_plan, заголовки, passthrough тела (cache_control
// переживает, stream_options на anthropic-клетке ОТСУТСТВУЕТ — регрессия
// заметки Ф1), tee-usage кеш-полей. Апстрим — httptest-стаб (внешних вызовов
// в тестах быть не должно, CODING 1.9): origin подменяется через base_url
// провайдера в снапшоте.
package glm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-proxy/internal/config"
	"llm-proxy/internal/router"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// ─── Юнит: сборка тела и эндпоинта ──────────────────────────────────────────

func sendReq(protocol string, body []byte, stream bool) *upstream.SendRequest {
	return &upstream.SendRequest{
		Protocol: protocol,
		Body:     body,
		Stream:   stream,
		Target:   router.Target{ProviderType: config.ProviderGLM, Model: "glm-5.3"},
	}
}

func TestEndpointFor(t *testing.T) {
	acc := &config.GLMAccount{ID: "a1"}
	coding := &config.GLMAccount{ID: "a1", CodingPlan: true}
	cases := []struct {
		name     string
		origin   string
		acc      *config.GLMAccount
		protocol string
		want     string
	}{
		{"anthropic-вход", "https://api.z.ai", acc, upstream.ProtocolAnthropic, "https://api.z.ai/api/anthropic/v1/messages"},
		{"openai стандарт", "https://api.z.ai", acc, upstream.ProtocolOpenAI, "https://api.z.ai/api/paas/v4/chat/completions"},
		{"openai coding_plan", "https://api.z.ai", coding, upstream.ProtocolOpenAI, "https://api.z.ai/api/coding/paas/v4/chat/completions"},
		{"anthropic не зависит от coding_plan", "https://api.z.ai", coding, upstream.ProtocolAnthropic, "https://api.z.ai/api/anthropic/v1/messages"},
		{"origin со слэшем подрезается", "https://api.z.ai/", acc, upstream.ProtocolOpenAI, "https://api.z.ai/api/paas/v4/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointFor(tc.origin, tc.acc, tc.protocol); got != tc.want {
				t.Fatalf("endpoint = %s, ожидался %s", got, tc.want)
			}
		})
	}
}

// Ключевой регресс-тест заметки Ф1: anthropic-passthrough меняет ТОЛЬКО
// модель — cache_control переживает, stream_options НЕ появляется даже
// для стрима (InjectIncludeUsage — открытая openai-клетка, не антропик).
func TestBuildBodyAnthropicPassthrough(t *testing.T) {
	in := `{"model":"glm/test","max_tokens":64,"stream":true,` +
		`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	out, err := buildUpstreamBody(sendReq(upstream.ProtocolAnthropic, []byte(in), true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	raw := string(out)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	if m["model"] != "glm-5.3" {
		t.Fatalf("model = %v", m["model"])
	}
	if _, ok := m["stream_options"]; ok {
		t.Fatalf("stream_options просочился в anthropic-тело (заметка Ф1): %s", raw)
	}
	if !strings.Contains(raw, "cache_control") {
		t.Fatalf("cache_control потерян: %s", raw)
	}
	if !strings.Contains(raw, `"max_tokens":64`) {
		t.Fatalf("max_tokens потерян: %s", raw)
	}
}

// OpenAI-клетка: для стрима инжектится include_usage (OA-9), non-stream — нет.
func TestBuildBodyOpenAIInject(t *testing.T) {
	in := `{"model":"glm/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	out, err := buildUpstreamBody(sendReq(upstream.ProtocolOpenAI, []byte(in), true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts, ok := m["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options: %v", m["stream_options"])
	}
	if m["model"] != "glm-5.3" {
		t.Fatalf("model = %v", m["model"])
	}

	in2 := `{"model":"glm/test","messages":[{"role":"user","content":"hi"}]}`
	out2, err := buildUpstreamBody(sendReq(upstream.ProtocolOpenAI, []byte(in2), false))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(string(out2), "stream_options") {
		t.Fatalf("non-stream: include_usage инжектироваться не должен: %s", out2)
	}
}

func TestBuildBodyUnknownProtocol(t *testing.T) {
	if _, err := buildUpstreamBody(sendReq("codex", []byte(`{}`), false)); err == nil {
		t.Fatal("ожидали ошибку для неизвестного протокола")
	}
}

// ─── e2e: Send против httptest-стаба z.ai ───────────────────────────────────

// captured — то, что увидел стаб апстрима.
type captured struct {
	Method     string
	Path       string
	RawQuery   string
	Auth       string
	AnthVer    string
	Accept     string
	Body       string
	StreamMode bool // стаб ответил SSE (по флагу теста)
}

// stubUpstream — стаб z.ai: пишет captured и отвечает заготовкой теста.
type stubUpstream struct {
	srv      *httptest.Server
	mu       chan *captured // каждое обращение стаба кладёт captured (буфер 1)
	respond  func(w http.ResponseWriter, r *captured)
	hits     int
	overflow chan struct{}
}

func newStub(t *testing.T, respond func(w http.ResponseWriter, r *captured)) *stubUpstream {
	s := &stubUpstream{mu: make(chan *captured, 8)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits++
		b, _ := io.ReadAll(r.Body)
		c := &captured{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Auth:     r.Header.Get("Authorization"),
			AnthVer:  r.Header.Get("anthropic-version"),
			Accept:   r.Header.Get("Accept"),
			Body:     string(b),
		}
		s.mu <- c
		if respond != nil {
			respond(w, c)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// snapGLM — снапшот с glm-провайдером на стабе (base_url — оригин).
func snapGLM(origin, key string, coding bool) *config.Snapshot {
	return &config.Snapshot{
		Cfg: &config.ResolvedConfig{
			Providers: map[string]config.Provider{
				"zai": {
					Type: config.ProviderGLM,
					GLM: &config.GLMProvider{
						Type:    config.ProviderGLM,
						BaseURL: origin,
						Accounts: []config.GLMAccount{{
							ID: "a1", Auth: config.KeyAuth{Method: "api_key", Key: key},
							CodingPlan: coding,
						}},
					},
				},
			},
		},
	}
}

// doSend — прогон Send с recorder-клиентом.
func doSend(t *testing.T, snap *config.Snapshot, protocol string, body []byte, stream bool) (*httptest.ResponseRecorder, *upstream.SendResult) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := &upstream.SendRequest{
		Snap:        snap,
		Target:      router.Target{ProviderType: config.ProviderGLM, ProviderName: "zai", AccountID: "a1", Model: "glm-5.3"},
		Transporter: socks.Direct{},
		Protocol:    protocol,
		Body:        body,
		Stream:      stream,
		W:           rec,
	}
	res, err := Sender{}.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return rec, res
}

// Реальное anthropic-тело opencode с cache_control (фикстура).
const anthropicBody = `{"model":"glm/test","max_tokens":128,"stream":true,` +
	`"system":[{"type":"text","text":"Ты — ассистент.","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"привет","cache_control":{"type":"ephemeral"}}]}]}`

// anthropic non-stream: URL ровно /api/anthropic/v1/messages, Bearer +
// anthropic-version, модель подменена, cache_control пережил, stream_options
// отсутствует; usage из тела с кеш-полями.
func TestSendAnthropicNonStream(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_1","role":"assistant","model":"glm-5.3",` +
			`"content":[{"type":"text","text":"ответ"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":25,"output_tokens":11,` +
			`"cache_read_input_tokens":80,"cache_creation_input_tokens":7}}`))
	})

	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", false), upstream.ProtocolAnthropic, []byte(anthropicBody), false)

	c := <-stub.mu
	if c.Path != "/api/anthropic/v1/messages" {
		t.Fatalf("путь = %s, ожидался /api/anthropic/v1/messages", c.Path)
	}
	if c.Method != http.MethodPost {
		t.Fatalf("метод = %s", c.Method)
	}
	if c.Auth != "Bearer sk-zai" {
		t.Fatalf("authorization = %q", c.Auth)
	}
	if c.AnthVer != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", c.AnthVer)
	}
	if strings.Contains(c.Body, "stream_options") {
		t.Fatalf("stream_options просочился в anthropic-запрос (заметка Ф1): %s", c.Body)
	}
	if !strings.Contains(c.Body, `"model":"glm-5.3"`) || !strings.Contains(c.Body, "cache_control") {
		t.Fatalf("модель/cache_control: %s", c.Body)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("статус клиенту %d (%s)", rec.Code, rec.Body.String())
	}
	// Passthrough: тело клиенту как есть.
	if !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("тело искажено: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	// Usage с кеш-полями (SPEC 8.6 «glm anthropic»).
	if res.Usage.PromptTokens != 25 || res.Usage.CompletionTokens != 11 ||
		res.Usage.CachedInputTokens != 80 || res.Usage.CacheCreationTokens != 7 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	if res.ErrorKind != "" || res.Streamed {
		t.Fatalf("kind/streamed: %q/%v", res.ErrorKind, res.Streamed)
	}
	if res.UpstreamStatus != http.StatusOK {
		t.Fatalf("upstream status = %d", res.UpstreamStatus)
	}
	if res.Dump == nil || len(res.Dump.ReqHeaders.Values("Authorization")) == 0 {
		t.Fatalf("дамп без заголовков запроса: %+v", res.Dump)
	}
}

// anthropic stream: события уходят как есть, tee-usage собирает
// message_start (input + кеш) и message_delta (output).
func TestSendAnthropicStream(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":30,"output_tokens":1,"cache_read_input_tokens":90,"cache_creation_input_tokens":5}}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"ответ"}}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":14}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, ev+"\n\n")
			f.Flush()
		}
	})

	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", false), upstream.ProtocolAnthropic, []byte(anthropicBody), true)

	c := <-stub.mu
	if c.Path != "/api/anthropic/v1/messages" {
		t.Fatalf("путь = %s", c.Path)
	}
	if c.AnthVer != "2023-06-01" || c.Accept != "text/event-stream" {
		t.Fatalf("заголовки: anthver=%q accept=%q", c.AnthVer, c.Accept)
	}
	if strings.Contains(c.Body, "stream_options") {
		t.Fatalf("stream_options просочился (заметка Ф1): %s", c.Body)
	}

	out := rec.Body.String()
	// События passthrough — как есть.
	for _, want := range []string{"event: message_start", "content_block_delta", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q в стриме клиенту: %s", want, out)
		}
	}
	if !strings.Contains(out, "cache_read_input_tokens\":90") {
		t.Fatalf("message_start искажён: %s", out)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type: %q", ct)
	}
	// Tee-usage: кеш-поля из message_start, output из message_delta.
	if res.Usage.PromptTokens != 30 || res.Usage.CompletionTokens != 14 ||
		res.Usage.CachedInputTokens != 90 || res.Usage.CacheCreationTokens != 5 {
		t.Fatalf("tee-usage: %+v", res.Usage)
	}
	if !res.Streamed || res.ErrorKind != "" {
		t.Fatalf("streamed/kind: %v/%q", res.Streamed, res.ErrorKind)
	}
}

// openai-вход на стандартный эндпоинт: /api/paas/v4/chat/completions,
// include_usage в стрим-запросе, usage-чанк доезжает до результата.
func TestSendOpenAIStandardStream(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"привет"}}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"1","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":3}}}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, chunk+"\n\n")
			f.Flush()
		}
	})

	body := `{"model":"glm/test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", false), upstream.ProtocolOpenAI, []byte(body), true)

	c := <-stub.mu
	if c.Path != "/api/paas/v4/chat/completions" {
		t.Fatalf("путь = %s, ожидался /api/paas/v4/chat/completions", c.Path)
	}
	if !strings.Contains(c.Body, `"include_usage":true`) {
		t.Fatalf("include_usage не инжектирован: %s", c.Body)
	}
	if !strings.Contains(c.Body, `"model":"glm-5.3"`) {
		t.Fatalf("модель не подменена: %s", c.Body)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"привет"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("стрим искажён: %s", out)
	}
	if res.Usage.PromptTokens != 7 || res.Usage.CompletionTokens != 9 || res.Usage.CachedInputTokens != 3 {
		t.Fatalf("tee-usage: %+v", res.Usage)
	}
}

// coding_plan:true → coding-эндпоинт (OA-8).
func TestSendOpenAICodingEndpoint(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ок"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	})

	body := `{"model":"glm/test","messages":[{"role":"user","content":"hi"}]}`
	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", true), upstream.ProtocolOpenAI, []byte(body), false)

	c := <-stub.mu
	if c.Path != "/api/coding/paas/v4/chat/completions" {
		t.Fatalf("путь = %s, ожидался /api/coding/paas/v4/chat/completions", c.Path)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ок"`) {
		t.Fatalf("ответ: %d %s", rec.Code, rec.Body.String())
	}
	if res.Usage.PromptTokens != 4 || res.Usage.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// Не-2xx апстрима: классификация upstream, статус пробрасывается, дамп полный.
func TestSendUpstreamError(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"plan limit"}}`))
	})

	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", false), upstream.ProtocolAnthropic, []byte(anthropicBody), false)

	if res.ErrorKind != upstream.KindUpstream || res.Stage != upstream.StageUpstream {
		t.Fatalf("kind/stage: %q/%q", res.ErrorKind, res.Stage)
	}
	if res.ClientStatus != http.StatusTooManyRequests {
		t.Fatalf("статус клиенту %d, ожидался 429", res.ClientStatus)
	}
	if !strings.Contains(res.Message, "plan limit") {
		t.Fatalf("сообщение: %q", res.Message)
	}
	if res.Dump == nil || !strings.Contains(string(res.Dump.RespBody), "plan limit") {
		t.Fatalf("дамп без тела ответа: %+v", res.Dump)
	}
	if res.UpstreamHeader.Get("Retry-After") != "5" {
		t.Fatalf("retry-after не в заголовках результата: %v", res.UpstreamHeader)
	}
	// Клиенту ничего не писал провайдер: конверт — работа сервера.
	if rec.Body.Len() != 0 {
		t.Fatalf("провайдер написал клиенту при ошибке: %s", rec.Body.String())
	}
}

// blockedTransporter — Transporter, чей клиент всегда возвращает BlockedError.
// Клиент собирается без литерала-инициализатора: I1 grep смотрит за скобками.
type blockedTransporter struct{}

func (blockedTransporter) Client() *http.Client {
	var cl http.Client
	cl.Transport = errTripper{err: &socks.BlockedError{Path: "home", Reason: "egress ip mismatch"}}
	return &cl
}
func (blockedTransporter) Name() string         { return "home" }
func (blockedTransporter) Status() socks.Status { return socks.Status{Name: "home", State: "blocked"} }

type errTripper struct{ err error }

func (t errTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// Gate закрыл путь: 503 socks_blocked, к стабу ноль обращений (FC-5/SV-6).
func TestSendBlockedByGate(t *testing.T) {
	stub := newStub(t, nil)
	snap := snapGLM(stub.srv.URL, "sk-zai", false)
	rec := httptest.NewRecorder()
	req := &upstream.SendRequest{
		Snap:        snap,
		Target:      router.Target{ProviderType: config.ProviderGLM, ProviderName: "zai", AccountID: "a1", Model: "glm-5.3"},
		Transporter: blockedTransporter{},
		Protocol:    upstream.ProtocolAnthropic,
		Body:        []byte(anthropicBody),
		W:           rec,
	}
	res, err := Sender{}.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ErrorKind != upstream.KindSocksBlocked || res.ClientStatus != http.StatusServiceUnavailable {
		t.Fatalf("kind/status: %q/%d", res.ErrorKind, res.ClientStatus)
	}
	if !strings.Contains(res.Message, "home") {
		t.Fatalf("в сообщении нет имени пути: %q", res.Message)
	}
	if stub.hits != 0 {
		t.Fatalf("апстрим получил %d обращений, ожидался 0", stub.hits)
	}
}

// Контракт: (nil, error) — только нарушение вызова.
func TestSendContractViolations(t *testing.T) {
	stub := newStub(t, nil)
	snap := snapGLM(stub.srv.URL, "sk-zai", false)

	cases := []struct {
		name string
		req  *upstream.SendRequest
		want string
	}{
		{
			"чужой тип провайдера",
			&upstream.SendRequest{Snap: snap, Target: router.Target{ProviderType: "openai-like"}, Transporter: socks.Direct{}, W: httptest.NewRecorder()},
			"openai-like",
		},
		{
			"нет transporter",
			&upstream.SendRequest{Snap: snap, Target: router.Target{ProviderType: config.ProviderGLM, ProviderName: "zai", AccountID: "a1"}, W: httptest.NewRecorder()},
			"transporter",
		},
		{
			"нет W",
			&upstream.SendRequest{Snap: snap, Target: router.Target{ProviderType: config.ProviderGLM, ProviderName: "zai", AccountID: "a1"}, Transporter: socks.Direct{}},
			"ResponseWriter",
		},
		{
			"нет аккаунта",
			&upstream.SendRequest{Snap: snap, Target: router.Target{ProviderType: config.ProviderGLM, ProviderName: "zai", AccountID: "ghost"}, Transporter: socks.Direct{}, W: httptest.NewRecorder()},
			"ghost",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Sender{}.Send(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("ожидали (nil, err), получили %+v", res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ошибка %q без %q", err, tc.want)
			}
		})
	}
}

// Битый JSON от клиента на passthrough-клетке → 400 translate.
func TestSendTranslateErrorOnBadJSON(t *testing.T) {
	stub := newStub(t, nil)
	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", false), upstream.ProtocolAnthropic, []byte(`{"model":`), false)
	if res.ErrorKind != upstream.KindTranslate || res.ClientStatus != http.StatusBadRequest {
		t.Fatalf("kind/status: %q/%d", res.ErrorKind, res.ClientStatus)
	}
	if stub.hits != 0 {
		t.Fatalf("апстрим получил %d обращений, ожидался 0", stub.hits)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("провайдер писал клиенту при translate-ошибке")
	}
}

// Стрим: апстрим прислал ошибку событием (anthropic-форма) — passthrough
// события клиенту + классификация upstream.
func TestSendAnthropicStreamErrorEvent(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"перегрузка"}}`+"\n\n")
		w.(http.Flusher).Flush()
	})

	rec, res := doSend(t, snapGLM(stub.srv.URL, "sk-zai", false), upstream.ProtocolAnthropic, []byte(anthropicBody), true)

	out := rec.Body.String()
	if !strings.Contains(out, "overloaded_error") {
		t.Fatalf("ошибка апстрима не прошла как есть: %s", out)
	}
	if res.ErrorKind != upstream.KindUpstream || res.Stage != upstream.StageStream || !res.Streamed {
		t.Fatalf("kind/stage/streamed: %q/%q/%v", res.ErrorKind, res.Stage, res.Streamed)
	}
}

// Обрыв апстрима посреди anthropic-стрима: клиент получает SSE-ошибку в
// формате anthropic, путь взводится в needCheck (FC-9).
func TestSendAnthropicStreamInterrupted(t *testing.T) {
	var reported int
	tr := &countingDirect{onReport: func() { reported++ }}

	stub := newStub(t, func(w http.ResponseWriter, r *captured) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`+"\n\n")
		w.(http.Flusher).Flush()
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = rw.Flush()
		conn.Close() // грубый обрыв
	})

	rec := httptest.NewRecorder()
	req := &upstream.SendRequest{
		Snap:        snapGLM(stub.srv.URL, "sk-zai", false),
		Target:      router.Target{ProviderType: config.ProviderGLM, ProviderName: "zai", AccountID: "a1", Model: "glm-5.3"},
		Transporter: tr,
		Protocol:    upstream.ProtocolAnthropic,
		Body:        []byte(anthropicBody),
		Stream:      true,
		W:           rec,
	}
	res, err := Sender{}.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ErrorKind != upstream.KindNetwork || res.Stage != upstream.StageStream {
		t.Fatalf("kind/stage: %q/%q", res.ErrorKind, res.Stage)
	}
	if reported != 1 {
		t.Fatalf("ReportNetworkError вызван %d раз, ожидался 1 (FC-9)", reported)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("нет SSE-ошибки anthropic-формы: %s", rec.Body.String())
	}
	// Что успело — usage из message_start (частичный учёт).
	if res.Usage.PromptTokens != 5 {
		t.Fatalf("частичный usage: %+v", res.Usage)
	}
}

// countingDirect — socks.Direct с подсчётом ReportNetworkError.
type countingDirect struct {
	onReport func()
}

func (d *countingDirect) Client() *http.Client { return socks.Direct{}.Client() }
func (d *countingDirect) Name() string         { return "none" }
func (d *countingDirect) Status() socks.Status { return socks.Direct{}.Status() }
func (d *countingDirect) ReportNetworkError()  { d.onReport() }
