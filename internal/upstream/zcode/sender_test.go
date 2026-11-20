package zcode

// E2E Sender на фейковом z.ai/zcode-стабе (SPEC 7.4): три протокола на обоих
// маршрутах, заголовки zcode_plan, 401 → ре-issue PAT → retry-once, auth-отказы
// без сети, tee-usage passthrough-стримов.

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"llm-proxy/internal/config"
	"llm-proxy/internal/router"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// senderEnv — стаб + Store с записью аккаунта + PATCache + снапшот конфига.
type senderEnv struct {
	stub   *zStub
	store  *Store
	chain  *PATCache
	sender Sender
	key    string
	snap   *config.Snapshot
}

func newSenderEnv(t *testing.T, route string) *senderEnv {
	t.Helper()
	st := newZStub(t)
	store, err := OpenStore(t.TempDir() + "/zcode-tokens.json")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	chain := NewPATCache(store)
	e := &senderEnv{
		stub:   st,
		store:  store,
		chain:  chain,
		sender: Sender{Store: store, Chain: chain},
		key:    AccountKey("a1"),
	}
	e.setRoute(route)
	return e
}

// setRoute — снапшот провайдера с заданным маршрутом (base/zcode_base — стаб).
func (e *senderEnv) setRoute(route string) {
	e.snap = &config.Snapshot{
		Cfg: &config.ResolvedConfig{
			Providers: map[string]config.Provider{
				"zai-plan": {
					Type: config.ProviderZCode,
					ZCode: &config.ZCodeProvider{
						Type:          config.ProviderZCode,
						BaseURL:       e.stub.srv.URL,
						ZCodeBaseURL:  e.stub.srv.URL,
						ClientVersion: "3.17.2",
						Route:         route,
						Accounts: []config.ZCodeAccount{{
							ID: "a1", Auth: config.OAuthAuth{Method: "oauth"},
						}},
					},
				},
			},
		},
	}
}

// withRecord — живая запись аккаунта (после OAuth, SPEC 3.2а).
func (e *senderEnv) withRecord(mut func(*TokenRec)) *senderEnv {
	rec := TokenRec{
		ZaiAccessToken: "zai-tok-1",
		ZcodeJWT:       "zjwt-1",
		UserID:         "u-1",
		Email:          "u@z.ai",
		DeviceMid:      "dev-1",
	}
	if mut != nil {
		mut(&rec)
	}
	if err := e.store.Set(e.key, rec); err != nil {
		panic(err)
	}
	return e
}

// send — прогон Send с recorder-клиентом.
func (e *senderEnv) send(t *testing.T, protocol string, body string, stream bool) (*recorder, *upstream.SendResult) {
	t.Helper()
	rec := newRecorder()
	req := &upstream.SendRequest{
		Snap:        e.snap,
		Target:      router.Target{ProviderType: config.ProviderZCode, ProviderName: "zai-plan", AccountID: "a1", Model: "glm-5.3"},
		Transporter: socks.Direct{},
		Protocol:    protocol,
		Body:        []byte(body),
		Stream:      stream,
		W:           rec,
	}
	res, err := e.sender.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: нарушение контракта вызова: %v", err)
	}
	return rec, res
}

// anthropicSSEUpstream — антропик-SSE с usage обоих событий (SPEC 8.6).
const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"привет"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":17}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// chatSSE — chat-стрим с usage-чанком.
const chatSSE = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"привет\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"!\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":13," +
	"\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
	"data: [DONE]\n\n"

// TestSenderAuthRefusals — нет записи / needs_relogin → auth 502 без сети.
func TestSenderAuthRefusals(t *testing.T) {
	t.Run("нет токена — выполните OAuth", func(t *testing.T) {
		e := newSenderEnv(t, config.ZCodeRoutePAT)
		_, res := e.send(t, upstream.ProtocolAnthropic, `{"model":"x","max_tokens":1,"messages":[]}`, false)
		if res.ErrorKind != upstream.KindAuth || res.ClientStatus != 502 {
			t.Fatalf("результат: %+v", res)
		}
		if !strings.Contains(res.Message, "выполните OAuth") {
			t.Fatalf("сообщение: %q", res.Message)
		}
		if got := e.stub.count("POST", "/api/auth/z/login"); got != 0 {
			t.Fatalf("без токена в сеть ходить нельзя (login = %d)", got)
		}
	})
	t.Run("needs_relogin — повторный вход", func(t *testing.T) {
		e := newSenderEnv(t, config.ZCodeRoutePAT)
		e.withRecord(nil)
		e.store.setStatus(e.key, StatusNeedsRelogin, "бизнес-код 500: Z.ai user information is invalid")
		_, res := e.send(t, upstream.ProtocolAnthropic, `{"model":"x","max_tokens":1,"messages":[]}`, false)
		if res.ErrorKind != upstream.KindAuth || res.ClientStatus != 502 {
			t.Fatalf("результат: %+v", res)
		}
		if !strings.Contains(res.Message, "needs_relogin") {
			t.Fatalf("сообщение: %q", res.Message)
		}
	})
	t.Run("nil Store — нарушение контракта", func(t *testing.T) {
		s := Sender{}
		_, err := s.Send(context.Background(), &upstream.SendRequest{})
		if err == nil || !strings.Contains(err.Error(), "Store не задан") {
			t.Fatalf("ожидали ошибку контракта: %v", err)
		}
	})
}

// TestSenderRoutePatAnthropicStream — антропик-клетка pat-маршрута: passthrough
// SSE + RewriteModel + заголовки (Bearer PAT, anthropic-version) + tee-usage
// кеш-полей (SPEC 8.6).
func TestSenderRoutePatAnthropicStream(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE))
	}

	rec, res := e.send(t, upstream.ProtocolAnthropic,
		`{"model":"zai/5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, true)

	// Заголовки запроса апстрима.
	req := e.stub.lastReq("POST", "/api/anthropic/v1/messages")
	if req == nil {
		t.Fatal("антропик-клетка не достигнута")
	}
	if !strings.HasPrefix(req.Auth, "Bearer ey") {
		t.Fatalf("Authorization = %q, ожидался Bearer <PAT>", req.Auth)
	}
	if req.Body == "" || !strings.Contains(req.Body, `"model":"glm-5.3"`) {
		t.Fatalf("модель не переписана: %q", req.Body)
	}
	if strings.Contains(req.Body, "stream_options") {
		t.Fatalf("stream_options просочился в anthropic-тело: %q", req.Body)
	}

	// SSE passthrough клиенту.
	out := rec.Body.String()
	for _, frag := range []string{"message_start", "text_delta", "message_delta", "message_stop"} {
		if !strings.Contains(out, frag) {
			t.Fatalf("в выводе клиента нет %s:\n%s", frag, out)
		}
	}
	// Tee-usage (SPEC 8.6 строка «glm anthropic»).
	if res.Usage.PromptTokens != 11 || res.Usage.CachedInputTokens != 4 ||
		res.Usage.CacheCreationTokens != 2 || res.Usage.CompletionTokens != 17 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	if res.ErrorKind != "" || res.ClientStatus != 0 {
		t.Fatalf("результат: %+v", res)
	}
}

// TestSenderRoutePatAnthropicNonStream — non-stream passthrough + usage из тела.
func TestSenderRoutePatAnthropicNonStream(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":6}}`)
	}
	_, res := e.send(t, upstream.ProtocolAnthropic,
		`{"model":"zai/5.3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, false)
	if res.ErrorKind != "" {
		t.Fatalf("результат: %+v", res)
	}
	if res.Usage.PromptTokens != 5 || res.Usage.CompletionTokens != 6 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// TestSenderRoutePatOpenAIStream — chat-клетка: passthrough + InjectIncludeUsage
// + tee-usage + [DONE].
func TestSenderRoutePatOpenAIStream(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatSSE))
	}

	rec, res := e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)

	req := e.stub.lastReq("POST", "/api/paas/v4/chat/completions")
	if req == nil {
		t.Fatal("чат-клетка не достигнута")
	}
	if !strings.Contains(req.Body, `"include_usage":true`) {
		t.Fatalf("include_usage не инжектирован: %q", req.Body)
	}
	if !strings.Contains(req.Body, `"model":"glm-5.3"`) {
		t.Fatalf("модель не переписана: %q", req.Body)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"content":"привет"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("passthrough сломан:\n%s", out)
	}
	if res.Usage.PromptTokens != 9 || res.Usage.CompletionTokens != 13 ||
		res.Usage.CachedInputTokens != 3 || res.Usage.ReasoningTokens != 2 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// TestSenderRoutePatResponses — responses-вход → chat-клетка (SPEC 8.2), выход —
// канонические Responses-события (SPEC 8.7), модель — алиас клиента.
func TestSenderRoutePatResponses(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatSSE))
	}

	rec, res := e.send(t, upstream.ProtocolResponses,
		`{"model":"zai/5.3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`, true)

	// Апстрим — ЧАТ-путь с chat-телом (конвертация responses → chat).
	req := e.stub.lastReq("POST", "/api/paas/v4/chat/completions")
	if req == nil {
		t.Fatal("responses-вход должен идти в chat-путь")
	}
	if !strings.Contains(req.Body, `"messages"`) {
		t.Fatalf("тело не chat-формы: %q", req.Body)
	}

	// Канонический Responses-выход (SPEC 8.7).
	out := rec.Body.String()
	for _, frag := range []string{`"response.created"`, `"response.output_text.delta"`, `"response.completed"`, `"input_tokens":9`} {
		if !strings.Contains(out, frag) {
			t.Fatalf("в Responses-выходе нет %s:\n%s", frag, out)
		}
	}
	// Модель в response.created — АЛИАС клиента (SPEC 8.7), не модель апстрима.
	if !strings.Contains(out, `"model":"zai/5.3"`) {
		t.Fatalf("модель должна быть алиасом клиента:\n%s", out)
	}
	if res.Usage.PromptTokens != 9 || res.Usage.CompletionTokens != 13 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// TestSender401ReissueRetryOnce — 401 pat-маршрута: Clear + EnsurePAT (ре-issue)
// → ровно один ретрай, успех.
func TestSender401ReissueRetryOnce(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)

	modelHits := 0
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		e.stub.mu.Lock()
		modelHits++
		first := modelHits == 1
		e.stub.mu.Unlock()
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid Authentication Token","type":"1002"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatSSE))
	}

	rec, res := e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)

	if res.ErrorKind != "" {
		t.Fatalf("ретрай после ре-issue должен быть успешным: %+v", res)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("клиент не получил ответ:\n%s", rec.Body.String())
	}
	if got := e.stub.count("POST", "/access_tokens"); got != 2 {
		t.Fatalf("access_tokens = %d, ожидалось 2 (первый PAT + ре-issue после 401)", got)
	}
	if modelHits != 2 {
		t.Fatalf("запросов к апстриму = %d, ожидалось 2 (retry-once)", modelHits)
	}
}

// TestSender401TwiceIsAuth — повторный 401 → auth 502 (зацикливание запрещено).
func TestSender401TwiceIsAuth(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"nope","type":"1002"}}`))
	}

	_, res := e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)

	if res.ErrorKind != upstream.KindAuth || res.ClientStatus != 502 {
		t.Fatalf("результат: %+v", res)
	}
	if got := e.stub.count("POST", "/access_tokens"); got != 2 {
		t.Fatalf("access_tokens = %d, ожидалось 2 (не зациклились)", got)
	}
}

// TestSenderUpstreamErrorPassthrough — не-2xx апстрима: статус клиенту (конверт
// пишет сервер), дамп приложен.
func TestSenderUpstreamErrorPassthrough(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRoutePAT)
	e.withRecord(nil)
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"plan limits"}}`))
	}
	_, res := e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","messages":[{"role":"user","content":"hi"}]}`, false)
	if res.ErrorKind != upstream.KindUpstream || res.ClientStatus != http.StatusTooManyRequests {
		t.Fatalf("результат: %+v", res)
	}
	if res.Dump == nil || !strings.Contains(string(res.Dump.RespBody), "plan limits") {
		t.Fatalf("дамп: %+v", res.Dump)
	}
}

// TestSenderPlanRouteHeaders — zcode_plan: пути поверх zcode_base, полный набор
// заголовков аттестации из zcode_jwt (SPEC 7.4), PAT-цепочка не запускается.
func TestSenderPlanRouteHeaders(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRouteZcodePlan)
	e.withRecord(nil)

	// Антропик-клетка.
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE))
	}
	_, res := e.send(t, upstream.ProtocolAnthropic,
		`{"model":"zai/5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, true)

	req := e.stub.lastReq("POST", "/api/v1/zcode-plan/anthropic")
	if req == nil {
		t.Fatal("антропик-клетка zcode_plan не достигнута")
	}
	if req.Auth != "Bearer zjwt-1" {
		t.Fatalf("Authorization = %q, ожидался Bearer <zcode_jwt>", req.Auth)
	}
	if req.APIKey != "zjwt-1" {
		t.Fatalf("x-api-key = %q, ожидался <zcode_jwt>", req.APIKey)
	}
	if req.AppVer != "3.17.2" {
		t.Fatalf("x-zcode-app-version = %q", req.AppVer)
	}
	if req.Plat != runtime.GOOS {
		t.Fatalf("x-platform = %q, ожидался %q", req.Plat, runtime.GOOS)
	}
	if req.DevMid != "dev-1" {
		t.Fatalf("x-device-mid = %q, ожидался dev-1 (из записи)", req.DevMid)
	}
	if !strings.Contains(req.Body, `"model":"glm-5.3"`) {
		t.Fatalf("модель не переписана: %q", req.Body)
	}
	if res.Usage.PromptTokens != 11 {
		t.Fatalf("usage: %+v", res.Usage)
	}

	// Чат-клетка.
	_, _ = e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)
	if e.stub.lastReq("POST", "/api/v1/zcode-plan") == nil {
		t.Fatal("чат-клетка zcode_plan не достигнута")
	}
}

// TestSenderPlanRouteNoChain — zcode_plan не должен запускать PAT-цепочку
// (заголовки строятся из zcode_jwt записи).
func TestSenderPlanRouteNoChain(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRouteZcodePlan)
	e.withRecord(nil)
	_, res := e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","messages":[{"role":"user","content":"hi"}]}`, false)
	if res.ErrorKind != "" {
		t.Fatalf("результат: %+v", res)
	}
	if got := e.stub.count("POST", "/api/auth/z/login") + e.stub.count("POST", "/access_tokens"); got != 0 {
		t.Fatalf("PAT-цепочка запустилась на zcode_plan-маршруте (%d запросов)", got)
	}
}

// TestSenderPlan401IsAuth — 401 zcode_plan: auth 502 без ретрая (zcode_jwt без
// refresh-эндпоинта — повторный OAuth, SPEC 7.4).
func TestSenderPlan401IsAuth(t *testing.T) {
	e := newSenderEnv(t, config.ZCodeRouteZcodePlan)
	e.withRecord(nil)
	modelHits := 0
	e.stub.onModel = func(w http.ResponseWriter, r *hitReq) {
		modelHits++
		w.WriteHeader(http.StatusUnauthorized)
	}
	_, res := e.send(t, upstream.ProtocolOpenAI,
		`{"model":"zai/5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)
	if res.ErrorKind != upstream.KindAuth || res.ClientStatus != 502 {
		t.Fatalf("результат: %+v", res)
	}
	if !strings.Contains(res.Message, "повторный OAuth") {
		t.Fatalf("сообщение: %q", res.Message)
	}
	if modelHits != 1 {
		t.Fatalf("запросов к апстриму = %d, ожидался 1 (без ретрая)", modelHits)
	}
}
