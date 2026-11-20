// E2E входа /v1/responses (Ф6b, SPEC 4/8.3/8.7): клетки матрицы 8.2 —
// responses → chat (openai-like и glm апстримы) и responses → responses
// (codex), конверты ошибок (openai-форма — Responses API использует её же),
// previous_response_id → 400, метод-гвард, JSON-404. Полный стек httptest —
// по образцу e2e_test.go / codex_e2e_test.go.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/upstream/codex"
)

// responsesRowWait — ожидание строки requests responses-входа по алиасу.
func responsesRowWait(t *testing.T, e *env, what string) {
	t.Helper()
	e.waitRow(t, what, `SELECT 1 FROM requests WHERE alias='work/test' AND protocol='responses'`)
}

// ─── responses → chat (openai-like апстрим, дефолтный стаб e2e) ─────────────

// Non-stream: chat-ответ конвертируется в объект Responses API (canon SPEC
// 8.7); модель в ответе — алиас клиента, не модель апстрима; usage из тела
// chat доезжает до requests с protocol='responses'.
func TestE2EResponsesNonStream(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"work/test","input":"hi"}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var got struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if got.Object != "response" || got.Status != "completed" {
		t.Fatalf("объект Responses: %+v (%s)", got, body)
	}
	// Клиент ждёт модель, которую сам и просил (алиас), а не gpt-test апстрима.
	if got.Model != "work/test" {
		t.Fatalf("model = %q, ожидался алиас work/test", got.Model)
	}
	if len(got.Output) != 1 || got.Output[0].Type != "message" ||
		got.Output[0].Content[0].Text != "привет" {
		t.Fatalf("output: %+v (%s)", got.Output, body)
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 34 {
		t.Fatalf("usage в ответе: %+v", got.Usage)
	}

	responsesRowWait(t, e, "requests(responses non-stream)")
	prompt, completion, cached, isStream, status, kind := e.queryRequests(t, "work/test")
	if prompt != 12 || completion != 34 || cached != 5 {
		t.Fatalf("usage: %d/%d/%d", prompt, completion, cached)
	}
	if isStream != 0 || status != 200 || kind != "" {
		t.Fatalf("is_stream/status/kind: %d/%d/%q", isStream, status, kind)
	}
}

// Стрим: канон SPEC 8.7 — response.created первым событием,
// response.completed последним, [DONE] НЕ эмитится (протокол Responses его
// не имеет); tee-usage usage-чанка — в requests.
func TestE2EResponsesStream(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"work/test","stream":true,"input":"hi"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type: %q", ct)
	}
	lines := readSSELines(t, resp)
	joined := strings.Join(lines, "\n")
	if lines[0] != "event: response.created" {
		t.Fatalf("первая строка стрима = %q, ожидалась event: response.created", lines[0])
	}
	for _, want := range []string{"response.output_item.added", "response.output_text.delta", `"привет"`, "response.output_item.done"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("нет %q в стриме: %s", want, joined)
		}
	}
	// Модель — алиас клиента (SPEC 8.7: клиент видит запрошенное имя).
	if !strings.Contains(joined, `"model":"work/test"`) {
		t.Fatalf("в response.created/completed нет алиаса: %s", joined)
	}
	if strings.Contains(joined, "[DONE]") {
		t.Fatalf("[DONE] запрещён в Responses-стриме: %s", joined)
	}
	// Последнее событие — response.completed (usage-чанк доложен в финал).
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}
	if !strings.Contains(last, `"type":"response.completed"`) {
		t.Fatalf("последняя строка = %q, ожидался data response.completed", last)
	}
	if !strings.Contains(last, `"input_tokens":7`) || !strings.Contains(last, `"output_tokens":9`) {
		t.Fatalf("usage не в response.completed: %s", last)
	}

	responsesRowWait(t, e, "requests(responses stream)")
	prompt, completion, _, isStream, _, kind := e.queryRequests(t, "work/test")
	if prompt != 7 || completion != 9 {
		t.Fatalf("tee-usage: %d/%d", prompt, completion)
	}
	if isStream != 1 || kind != "" {
		t.Fatalf("is_stream/kind: %d/%q", isStream, kind)
	}
}

// Idle-таймаут посреди responses-стрима: SSE-событие error в Responses-форме
// {"type":"error","code","message"}, response.completed НЕ шлётся (SPEC 8.7),
// строка requests — error_kind=network.
func TestE2EResponsesStreamIdleTimeout(t *testing.T) {
	stall := make(chan struct{})
	e := newEnv(t, envOptions{
		cfgMutate: func(cfg map[string]any) {
			cfg["server"].(map[string]any)["stream_idle_timeout"] = "150ms"
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

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"work/test","stream":true,"input":"hi"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d — стрим уже начат", resp.StatusCode)
	}
	lines := readSSELines(t, resp)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"раз"`) {
		t.Fatalf("первый чанк потерян: %s", joined)
	}
	// Ошибка — событием error Responses-формы; финального completed нет.
	if !strings.Contains(joined, `event: error`) || !strings.Contains(joined, `"type":"error"`) ||
		!strings.Contains(joined, "idle_timeout") {
		t.Fatalf("нет SSE-ошибки idle в Responses-форме: %s", joined)
	}
	if strings.Contains(joined, "response.completed") {
		t.Fatalf("response.completed после ошибки запрещён (SPEC 8.7): %s", joined)
	}

	e.waitRow(t, "requests(responses idle)", `SELECT 1 FROM requests WHERE error_kind='network'`)
	var kind string
	if err := e.queryRow(`SELECT error_kind FROM requests`).Scan(&kind); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if kind != "network" {
		t.Fatalf("error_kind = %q, ожидался network", kind)
	}
}

// ─── Ошибки входа: конверты openai-формы (Responses API использует её же) ───

// previous_response_id непустой → 400 с понятным сообщением (прокси
// stateless, SPEC 4); апстрим не получает ни одного запроса.
func TestE2EResponsesPreviousResponseID400(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"work/test","input":"hi","previous_response_id":"resp_prev"}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400 (%s)", resp.StatusCode, body)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("тело не openai-конверт: %v (%s)", err, body)
	}
	if !strings.Contains(got.Error.Message, "previous_response_id") {
		t.Fatalf("сообщение без previous_response_id: %q", got.Error.Message)
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// GET /v1/responses/abc — stateless-ресурс не существует (SPEC 4): 404 JSON
// в openai-форме, никогда HTML.
func TestE2EResponsesGetSubResource404(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.do(http.MethodGet, "/v1/responses/abc", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404 (%s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: %q (никогда HTML, SPEC 4)", ct)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "not_found") {
		t.Fatalf("404 не openai-конверт: %s", body)
	}
}

// Неизвестный алиас → 404 openai-формы со списком доступных (SV-2).
func TestE2EResponsesUnknownAlias404(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"no/such","input":"hi"}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус %d, ожидался 404 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{"model_not_found", "work/test", "boom"} {
		if !strings.Contains(body, want) {
			t.Fatalf("в 404 нет %q: %s", want, body)
		}
	}
	if n := e.upHits.Load(); n != 0 {
		t.Fatalf("апстрим получил %d коннектов, ожидался 0", n)
	}
}

// Метод-гвард: GET /v1/responses → 405 JSON-конвертом (SPEC 4: никогда HTML).
func TestE2EResponsesMethodGuard405(t *testing.T) {
	e := newEnv(t, envOptions{})

	resp := e.do(http.MethodGet, "/v1/responses", "sk-test-key", nil, nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("статус %d, ожидался 405 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "method_not_allowed") {
		t.Fatalf("405 не openai-конверт: %s", body)
	}
}

// ─── responses → chat (glm апстрим): coding-эндпоинт выбирается как для
// openai-входа (SPEC 8.2), ответ — конвертация в Responses ───────────────────

func TestE2EGLMResponsesCodingEndpoint(t *testing.T) {
	e, cap := newGLMEnv(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ок"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1}}}`))
	})

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"glm/test","input":"hi"}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, `"model":"glm/test"`) {
		t.Fatalf("responses-ответ искажён: %s", body)
	}

	// Апстрим: чат-путь (coding_plan), тело — chat с моделью апстрима.
	path, _, _, _, upBody := cap.snapshot()
	if path != "/api/coding/paas/v4/chat/completions" {
		t.Fatalf("путь апстрима = %s, ожидался coding-эндпоинт", path)
	}
	if !strings.Contains(upBody, `"model":"glm-5.3"`) || !strings.Contains(upBody, `"messages"`) {
		t.Fatalf("тело апстрима не chat: %s", upBody)
	}

	e.waitRow(t, "requests(glm responses)", `SELECT 1 FROM requests WHERE alias='glm/test' AND protocol='responses'`)
	var prompt, completion, cached int
	if err := e.queryRow(`SELECT prompt_tokens, completion_tokens, cached_input_tokens
		FROM requests WHERE alias='glm/test'`).Scan(&prompt, &completion, &cached); err != nil {
		t.Fatalf("requests row: %v", err)
	}
	if prompt != 4 || completion != 2 || cached != 1 {
		t.Fatalf("usage в requests: %d/%d/%d", prompt, completion, cached)
	}
}

// ─── responses → responses (codex апстрим, SPEC 8.7 «codex-апстрим») ────────

// Стрим: события апстрима УЖЕ Responses — passthrough как есть (стабильные
// события стаба доходят байт-в-байт), tee-usage codex-формы — в requests.
func TestE2ECodexResponsesStream(t *testing.T) {
	e, _, _, _ := newCodexEnv(t, codexToken(), codexSSEHandler)

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"codex/test","stream":true,"input":"привет"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, e.readBody(resp))
	}
	lines := readSSELines(t, resp)
	joined := strings.Join(lines, "\n")
	// События стаба прошли насквозь без перекодирования (passthrough).
	for _, want := range []string{
		"event: response.created",
		`data: {"response":{"id":"resp_1"}}`,
		`data: {"item_id":"m1","delta":"привет"}`,
		"event: response.completed",
		`"input_tokens":12`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("passthrough потерял %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "[DONE]") {
		t.Fatalf("[DONE] запрещён в Responses-стриме: %s", joined)
	}

	e.waitRow(t, "requests(codex responses stream)", `SELECT 1 FROM requests WHERE alias='codex/test' AND protocol='responses' AND is_stream=1`)
	var prompt, completion, cached, reasoning, isStream int
	var proto string
	if err := e.queryRow(`SELECT prompt_tokens, completion_tokens, cached_input_tokens,
		reasoning_tokens, is_stream, protocol FROM requests WHERE alias='codex/test'`).
		Scan(&prompt, &completion, &cached, &reasoning, &isStream, &proto); err != nil {
		t.Fatalf("requests: %v", err)
	}
	// usage codex-формы (SPEC 8.6: cached/reasoning из details).
	if prompt != 12 || completion != 34 || cached != 5 || reasoning != 7 || isStream != 1 || proto != "responses" {
		t.Fatalf("строка requests: usage=%d/%d/%d/%d stream=%d proto=%s",
			prompt, completion, cached, reasoning, isStream, proto)
	}
}

// Non-stream: SSE апстрима агрегируется в полный объект Responses API
// (output — из output_item.done; usage — из response.completed).
func TestE2ECodexResponsesNonStream(t *testing.T) {
	e, _, _, _ := newCodexEnv(t, codexToken(), func(_ *codexCapture, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\n"+`data: {"response":{"id":"resp_1"}}`+"\n\n"+
			"event: response.output_item.done\n"+
			`data: {"item":{"id":"m1","type":"message","content":[{"type":"output_text","text":"привет"}]}}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"response":{"usage":{"input_tokens":12,"output_tokens":34,`+
			`"input_tokens_details":{"cached_tokens":5}}}}`+"\n\n")
	})

	resp := e.postJSON("/v1/responses", "sk-test-key",
		[]byte(`{"model":"codex/test","input":"привет"}`), nil)
	body := e.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, body)
	}
	var got struct {
		Object string `json:"object"`
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if got.Object != "response" || got.ID != "resp_1" || got.Status != "completed" {
		t.Fatalf("агрегированный объект: %+v (%s)", got, body)
	}
	if len(got.Output) == 0 || got.Output[0].Type != "message" || got.Output[0].Content[0].Text != "привет" {
		t.Fatalf("output: %+v (%s)", got.Output, body)
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 34 {
		t.Fatalf("usage в ответе: %+v", got.Usage)
	}

	e.waitRow(t, "requests(codex responses non-stream)", `SELECT 1 FROM requests WHERE alias='codex/test' AND protocol='responses' AND is_stream=0`)
}

// codexToken — валидный токен для codex-стаба (образец codex_e2e_test.go).
func codexToken() codex.TokenSet {
	return codex.TokenSet{
		AccessToken: "t", RefreshToken: "rt", AccountID: "acc_1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
}
