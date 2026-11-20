// E2E полного стека для провайдера glm (Ф2b): клиент → server → router →
// upstream/glm → стаб z.ai. Проверяет OA-8 на живом стеке (URL, заголовки)
// и доездку tee-usage кеш-полей (cache_read/cache_creation) до строки
// requests (SPEC 8.6/9). Провайдер подключается через Params.Senders —
// диспетчеризация по типу. Стаб z.ai — собственный httptest-сервер теста:
// glm-провайдер смотрит на него через base_url (локальный оригин →
// socks:"none" легален, FC-6).
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llm-proxy/internal/config"
	"llm-proxy/internal/upstream/glm"
	"llm-proxy/internal/upstream/openailike"
)

// upstreamCapture — то, что увидел стаб апстрима (потокобезопасно: хендлер
// стаба живёт в чужой горутине, тест читает после получения ответа).
type upstreamCapture struct {
	mu      sync.Mutex
	path    string
	auth    string
	anthVer string
	query   string
	body    string
}

func (c *upstreamCapture) capture(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.auth, c.anthVer, c.query, c.body =
		r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("anthropic-version"),
		r.URL.RawQuery, string(body)
}

func (c *upstreamCapture) snapshot() (path, auth, anthVer, query, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.auth, c.anthVer, c.query, c.body
}

// addGLMProvider — glm-провайдер на стабе (base_url — оригин) и алиас glm/test.
func addGLMProvider(cfg map[string]any, url string) {
	cfg["providers"].(map[string]any)["zai"] = map[string]any{
		"type":     "glm",
		"base_url": url,
		"accounts": []any{
			map[string]any{
				"id":          "g1",
				"auth":        map[string]any{"method": "api_key", "key": "sk-zai"},
				"coding_plan": true,
				"socks":       "none",
			},
		},
	}
	cfg["aliases"].(map[string]any)["glm/test"] = map[string]any{
		"account": "zai/g1", "model": "glm-5.3",
	}
}

// glmSenders — оба модуля через карту типов (диспетчеризация Ф2b).
func glmSenders(p *Params) {
	p.Senders = map[string]Sender{
		config.ProviderGLM:        glm.Sender{},
		config.ProviderOpenAILike: openailike.Sender{},
	}
}

// anthropic-тело клиента с cache_control (как шлёт opencode); стрим-вариант —
// со stream:true, non-stream — без.
const glmAnthropicBody = `{"model":"glm/test","max_tokens":64,` +
	`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"привет"}]}`

const glmAnthropicStreamBody = `{"model":"glm/test","max_tokens":64,"stream":true,` +
	`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"привет"}]}`

// newGLMEnv — окружение с glm-провайдером на собственном стабе апстрима.
func newGLMEnv(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, body []byte)) (*env, *upstreamCapture) {
	t.Helper()
	var cap upstreamCapture
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.capture(r, b)
		respond(w, r, b)
	}))
	t.Cleanup(stub.Close)

	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			addGLMProvider(cfg, stub.URL)
		},
		tune: glmSenders,
	})
	return e, &cap
}

// E2E anthropic-вход non-stream: стаб видит /api/anthropic/v1/messages с
// Bearer + anthropic-version, тело — passthrough с подменой модели и живым
// cache_control, кеш-поля usage попадают в requests.
func TestE2EGLMAnthropicNonStream(t *testing.T) {
	e, cap := newGLMEnv(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_1","role":"assistant",` +
			`"content":[{"type":"text","text":"ответ"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":25,"output_tokens":11,` +
			`"cache_read_input_tokens":80,"cache_creation_input_tokens":7}}`))
	})

	resp := e.postJSON("/v1/messages", "sk-test-key", []byte(glmAnthropicBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	// Passthrough: тело клиенту как есть.
	if !strings.Contains(body, `"stop_reason":"end_turn"`) || !strings.Contains(body, `"msg_1"`) {
		t.Fatalf("passthrough-ответ искажён: %s", body)
	}

	// Апстрим-запрос: URL/заголовки/тело (OA-8 + регрессия заметки Ф1).
	path, auth, anthVer, _, upBody := cap.snapshot()
	if path != "/api/anthropic/v1/messages" {
		t.Fatalf("путь апстрима = %s, ожидался /api/anthropic/v1/messages", path)
	}
	if auth != "Bearer sk-zai" {
		t.Fatalf("authorization = %q", auth)
	}
	if anthVer != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", anthVer)
	}
	if strings.Contains(upBody, "stream_options") {
		t.Fatalf("stream_options просочился в anthropic-тело (заметка Ф1): %s", upBody)
	}
	if !strings.Contains(upBody, `"model":"glm-5.3"`) || !strings.Contains(upBody, "cache_control") {
		t.Fatalf("модель/cache_control: %s", upBody)
	}

	// Строка requests: usage с кеш-полями (SPEC 8.6 «glm anthropic»).
	e.waitRow(t, "requests(glm non-stream)", `SELECT 1 FROM requests WHERE alias='glm/test' AND protocol='anthropic' AND is_stream=0`)
	var prompt, completion, cached, creation int
	if err := e.queryRow(`SELECT prompt_tokens, completion_tokens, cached_input_tokens, cache_creation_tokens
		FROM requests WHERE alias='glm/test'`).Scan(&prompt, &completion, &cached, &creation); err != nil {
		t.Fatalf("requests row: %v", err)
	}
	if prompt != 25 || completion != 11 || cached != 80 || creation != 7 {
		t.Fatalf("usage в requests: %d/%d/%d/%d", prompt, completion, cached, creation)
	}
	var kind string
	if err := e.queryRow(`SELECT COALESCE(error_kind,'') FROM requests WHERE alias='glm/test'`).Scan(&kind); err != nil {
		t.Fatalf("kind: %v", err)
	}
	if kind != "" {
		t.Fatalf("error_kind = %q, ожидался успех", kind)
	}
}

// E2E anthropic-вход stream: события passthrough, tee-usage кеш-полей из
// message_start/message_delta — в requests.
func TestE2EGLMAnthropicStream(t *testing.T) {
	e, cap := newGLMEnv(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
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

	resp := e.postJSON("/v1/messages", "sk-test-key", []byte(glmAnthropicStreamBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	joined := strings.Join(readSSELines(t, resp), "\n")
	// События ушли клиенту как есть (passthrough SPEC 8.2).
	for _, want := range []string{"event: message_start", "content_block_delta", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("нет %q в стриме: %s", want, joined)
		}
	}
	if !strings.Contains(joined, "cache_read_input_tokens\":90") {
		t.Fatalf("message_start искажён: %s", joined)
	}

	path, _, anthVer, _, _ := cap.snapshot()
	if path != "/api/anthropic/v1/messages" || anthVer != "2023-06-01" {
		t.Fatalf("апстрим: path=%s anthver=%q", path, anthVer)
	}

	// Строка requests: кеш-поля из tee-коллектора.
	e.waitRow(t, "requests(glm stream)", `SELECT 1 FROM requests WHERE alias='glm/test' AND protocol='anthropic' AND is_stream=1`)
	var prompt, completion, cached, creation int
	if err := e.queryRow(`SELECT prompt_tokens, completion_tokens, cached_input_tokens, cache_creation_tokens
		FROM requests WHERE alias='glm/test'`).Scan(&prompt, &completion, &cached, &creation); err != nil {
		t.Fatalf("requests row: %v", err)
	}
	if prompt != 30 || completion != 14 || cached != 90 || creation != 5 {
		t.Fatalf("tee-usage в requests: %d/%d/%d/%d", prompt, completion, cached, creation)
	}
}

// openai-вход на glm coding_plan-аккаунте: путь апстрима — coding-эндпоинт
// (OA-8), usage-чанк доезжает до requests.
func TestE2EGLMOpenAICodingEndpoint(t *testing.T) {
	e, cap := newGLMEnv(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ок"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1}}}`))
	})

	resp := e.postJSON("/v1/chat/completions", "sk-test-key",
		[]byte(`{"model":"glm/test","messages":[{"role":"user","content":"hi"}]}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}

	path, auth, _, _, upBody := cap.snapshot()
	if path != "/api/coding/paas/v4/chat/completions" {
		t.Fatalf("путь апстрима = %s, ожидался coding-эндпоинт", path)
	}
	if auth != "Bearer sk-zai" {
		t.Fatalf("authorization = %q", auth)
	}
	// non-stream: include_usage НЕ инжектируется.
	if strings.Contains(upBody, "stream_options") {
		t.Fatalf("non-stream: include_usage не должен инжектироваться: %s", upBody)
	}

	e.waitRow(t, "requests(glm openai)", `SELECT 1 FROM requests WHERE alias='glm/test' AND protocol='openai'`)
	var prompt, completion, cached int
	if err := e.queryRow(`SELECT prompt_tokens, completion_tokens, cached_input_tokens
		FROM requests WHERE alias='glm/test'`).Scan(&prompt, &completion, &cached); err != nil {
		t.Fatalf("requests row: %v", err)
	}
	if prompt != 4 || completion != 2 || cached != 1 {
		t.Fatalf("usage в requests: %d/%d/%d", prompt, completion, cached)
	}
}

// glm-апстрим вернул ошибку: клиенту — статус апстрима в конверте входа,
// errors — дамп с маскированным ключом.
func TestE2EGLMUpstreamError(t *testing.T) {
	e, _ := newGLMEnv(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"plan limit exceeded"}}`))
	})

	resp := e.postJSON("/v1/messages", "sk-test-key", []byte(glmAnthropicBody), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("статус %d, ожидался 429 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "plan limit") {
		t.Fatalf("anthropic-конверт 429: %s", body)
	}

	e.waitRow(t, "requests(glm 429)", `SELECT 1 FROM requests WHERE error_kind='upstream'`)
	var kind, account string
	if err := e.queryRow(`SELECT error_kind, account_id FROM requests`).Scan(&kind, &account); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if kind != "upstream" || account != "g1" {
		t.Fatalf("requests: kind=%q account=%q", kind, account)
	}

	e.waitRow(t, "errors(glm 429)", `SELECT 1 FROM errors WHERE stage='upstream'`)
	var reqHeaders string
	if err := e.queryRow(`SELECT request_headers FROM errors WHERE stage='upstream'`).Scan(&reqHeaders); err != nil {
		t.Fatalf("errors: %v", err)
	}
	if strings.Contains(reqHeaders, "sk-zai") {
		t.Fatalf("ключ апстрима просочился в дамп: %s", reqHeaders)
	}
}
