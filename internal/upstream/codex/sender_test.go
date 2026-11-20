package codex

// E2E Sender на codex-стабе (OA-4/7, AC-7/8): SSE Responses на оба входа ×
// stream/non-stream, заголовки SPEC 7.1, auth-отказ без сети, 401 → refresh →
// ровно один ретрай, второй 401 → auth, 429 usage_limit_reached с resets_at.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/router"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// uuidV4RE — форма session-id (RFC 4122, версия 4).
var uuidV4RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// sseOK — эталонный SSE Responses: text-дельта + item.done + completed с
// usage (SPEC 8.6) и x-codex-* заголовками обоих окон (AC-7).
const sseOK = "event: response.created\n" +
	`data: {"response":{"id":"resp_1"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"item_id":"m1","delta":"привет"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"item":{"id":"m1","type":"message","content":[{"type":"output_text","text":"привет"}]}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"response":{"usage":{"input_tokens":12,"output_tokens":34,` +
	`"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":7}},` +
	`"output":[{"id":"m1","type":"message"}]}}` + "\n\n"

// codexStub — один httptest-сервер с апстримом /backend-api/codex/responses
// и auth-эндпоинтом /oauth/token (base_url и auth_base_url совпадают, как в
// smoke).
type codexStub struct {
	srv *httptest.Server

	mu            sync.Mutex
	responsesHits int
	tokenHits     int
	lastHeader    http.Header
	lastBody      []byte
	lastSessions  []string
	// responses — поведение апстрима (401/429/SSE); nil → дефолтный SSE OK.
	responses func(st *codexStub, w http.ResponseWriter, r *http.Request, body []byte)
	// token — поведение /oauth/token; nil → refresh до at-new/rt-new.
	token func(st *codexStub, w http.ResponseWriter, r *http.Request)
}

func newCodexStub(t *testing.T) *codexStub {
	st := &codexStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/backend-api/codex/responses", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.responsesHits++
		st.lastHeader = r.Header.Clone()
		st.lastSessions = append(st.lastSessions, r.Header.Get("session-id"))
		st.mu.Unlock()
		body := readAll(t, r)
		st.mu.Lock()
		st.lastBody = body
		st.mu.Unlock()
		if st.responses != nil {
			st.responses(st, w, r, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", "42.5")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.Header().Set("X-Codex-Primary-Reset-At", "1730003600")
		w.Header().Set("X-Codex-Secondary-Primary-Used-Percent", "12")
		w.Header().Set("X-Codex-Secondary-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Secondary-Primary-Reset-At", "1730870000")
		_, _ = fmt.Fprint(w, sseOK)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.tokenHits++
		token := st.token
		st.mu.Unlock()
		if token != nil {
			token(st, w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tokenJSON(t, "at-new", "rt-new", fakeJWT(t, claimsPayload), 3600))
	})
	st.srv = httptest.NewServer(mux)
	t.Cleanup(st.srv.Close)
	return st
}

func (st *codexStub) snapshot() (hits, tokenHits int, hdr http.Header, body []byte, sessions []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.responsesHits, st.tokenHits, st.lastHeader, st.lastBody, st.lastSessions
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("чтение тела стаба: %v", err)
	}
	return b
}

// senderEnv — стаб + Store с предзаписанным токеном + снапшот конфига.
type senderEnv struct {
	stub   *codexStub
	store  *Store
	snap   *config.Snapshot
	sender Sender
}

func newSenderEnv(t *testing.T, token TokenSet) *senderEnv {
	t.Helper()
	e := &senderEnv{stub: newCodexStub(t)}
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if token.AccessToken != "" || token.RefreshToken != "" {
		if err := store.Set("codex/a1", token); err != nil {
			t.Fatalf("store.Set: %v", err)
		}
	}
	e.store = store
	e.snap = codexSnapshot(t, e.stub.srv.URL)
	e.sender = Sender{Store: store}
	return e
}

// codexSnapshot — конфиг с codex-провайдером на стабе (socks:"none" легален:
// стаб локальный, FC-6) и алиасом codex/test.
func codexSnapshot(t *testing.T, base string) *config.Snapshot {
	t.Helper()
	cfg := fmt.Sprintf(`{
		"server": {"listen": "127.0.0.1:0", "clients": {"tester": "k"}},
		"providers": {"codex": {"type": "codex", "base_url": %q, "auth_base_url": %q,
			"accounts": [{"id": "a1", "email": "w@x.ru", "auth": {"method": "oauth", "callback_port": 1455}, "socks": "none"}]}},
		"aliases": {"codex/test": {"account": "codex/a1", "model": "gpt-6.1"}}
	}`, base, base)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeFile(path, []byte(cfg)); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	rc, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return &config.Snapshot{Cfg: rc, Socks: map[string]*socks.Path{}}
}

func mustTarget(t *testing.T, snap *config.Snapshot, alias string) router.Target {
	t.Helper()
	target, err := router.Resolve(snap, alias)
	if err != nil {
		t.Fatalf("router.Resolve: %v", err)
	}
	return target
}

// send — прогон одного запроса через Sender с recorder-клиентом.
func (e *senderEnv) send(t *testing.T, protocol, body string, stream bool) (*upstream.SendResult, *recorder) {
	t.Helper()
	w := newRecorder()
	req := &upstream.SendRequest{
		Snap:           e.snap,
		Target:         mustTarget(t, e.snap, "codex/test"),
		Transporter:    socks.Direct{},
		Protocol:       protocol,
		Body:           []byte(body),
		Stream:         stream,
		W:              w,
		Keepalive:      time.Second,
		ConnectTimeout: 5 * time.Second,
		IdleTimeout:    5 * time.Second,
	}
	res, err := e.sender.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: нарушение контракта: %v", err)
	}
	return res, w
}

const openAIBody = `{"model":"codex/test","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"prompt_cache_key":"cache-key-1"}`

const anthropicBody = `{"model":"codex/test","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

func TestSendOpenAINonStreamAggregatesUpstreamSSE(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	res, w := e.send(t, upstream.ProtocolOpenAI, openAIBody, false)

	if res.ErrorKind != "" || res.Streamed {
		t.Fatalf("результат = %+v", res)
	}
	// tee-usage по таблице SPEC 8.6 (TR-3): cached ← cached_tokens,
	// reasoning ← output_tokens_details.reasoning_tokens.
	if res.Usage.PromptTokens != 12 || res.Usage.CompletionTokens != 34 ||
		res.Usage.CachedInputTokens != 5 || res.Usage.ReasoningTokens != 7 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	// Агрегатор: полный chat.completion с контентом и finish_reason.
	var chat struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &chat); err != nil {
		t.Fatalf("тело не chat.completion: %v\n%s", err, w.Body.String())
	}
	if chat.Object != "chat.completion" || len(chat.Choices) != 1 ||
		chat.Choices[0].Message.Content != "привет" || chat.Choices[0].FinishReason != "stop" {
		t.Fatalf("агрегат = %s", w.Body.String())
	}
	if chat.Usage == nil || chat.Usage.PromptTokens != 12 {
		t.Fatalf("usage в ответе = %s", w.Body.String())
	}

	// Заголовки апстрима SPEC 7.1 (OA-7) и тело Responses.
	_, _, hdr, body, _ := e.stub.snapshot()
	if hdr.Get("Authorization") != "Bearer at-1" {
		t.Fatalf("Authorization = %q", hdr.Get("Authorization"))
	}
	if hdr.Get("Chatgpt-Account-Id") != "acc_1" {
		t.Fatalf("chatgpt-account-id = %q", hdr.Get("Chatgpt-Account-Id"))
	}
	if hdr.Get("Originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", hdr.Get("Originator"))
	}
	if !uuidV4RE.MatchString(hdr.Get("Session-Id")) {
		t.Fatalf("session-id не uuid v4: %q", hdr.Get("Session-Id"))
	}
	ua := hdr.Get("User-Agent")
	if !strings.HasPrefix(ua, "codex_cli_rs/") || !strings.Contains(ua, "llm-proxy") ||
		!strings.Contains(ua, "linux") {
		t.Fatalf("User-Agent = %q", ua)
	}
	var reqBody struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		Store           bool   `json:"store"`
		Stream          bool   `json:"stream"`
		PromptCacheKey  string `json:"prompt_cache_key"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		t.Fatalf("тело апстрима не JSON: %v", err)
	}
	// store:false/stream:true ВСЕГДА; модель подменена; system → instructions;
	// prompt_cache_key проксирован.
	if reqBody.Store || !reqBody.Stream || reqBody.Model != "gpt-6.1" ||
		reqBody.Instructions != "sys" || reqBody.PromptCacheKey != "cache-key-1" {
		t.Fatalf("тело апстрима = %s", body)
	}

	// Квоты из заголовков ответа едут наверх (AC-7).
	if res.Quotas == nil || len(res.Quotas.Windows) != 2 {
		t.Fatalf("Quotas = %+v", res.Quotas)
	}
	if res.Quotas.Windows[0].Kind != "codex_primary" || res.Quotas.Windows[0].UsedPercent != 42.5 ||
		res.Quotas.Windows[0].WindowMinutes != 300 {
		t.Fatalf("primary = %+v", res.Quotas.Windows[0])
	}
	if res.Quotas.Windows[1].Kind != "codex_weekly" || res.Quotas.Windows[1].UsedPercent != 12 ||
		res.Quotas.Windows[1].WindowMinutes != 10080 || res.Quotas.Windows[1].ResetAtUnix != 1730870000 {
		t.Fatalf("weekly = %+v", res.Quotas.Windows[1])
	}
}

func TestSendOpenAIStreamEmitsChatChunks(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	res, w := e.send(t, upstream.ProtocolOpenAI, `{"model":"codex/test","messages":[{"role":"user","content":"hi"}]}`, true)

	if !res.Streamed || res.ErrorKind != "" {
		t.Fatalf("результат = %+v", res)
	}
	out := w.Body.String()
	for _, want := range []string{
		`"role":"assistant"`,     // role-чанк на response.created
		`"content":"привет"`,     // дельта
		`"finish_reason":"stop"`, // финал
		`"prompt_tokens":12`,     // usage-чанк
		"data: [DONE]",           // TR-4
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в SSE нет %q:\n%s", want, out)
		}
	}
	if res.Usage.CompletionTokens != 34 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestSendAnthropicStreamEmitsAnthropicSSE(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	res, w := e.send(t, upstream.ProtocolAnthropic, anthropicBody, true)

	if !res.Streamed || res.ErrorKind != "" {
		t.Fatalf("результат = %+v", res)
	}
	out := w.Body.String()
	for _, want := range []string{"event: message_start", "text_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в SSE нет %q:\n%s", want, out)
		}
	}
}

func TestSendAnthropicNonStreamAggregates(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	res, w := e.send(t, upstream.ProtocolAnthropic, anthropicBody, false)

	if res.ErrorKind != "" {
		t.Fatalf("результат = %+v", res)
	}
	var msg struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("тело не anthropic message: %v\n%s", err, w.Body.String())
	}
	if msg.Type != "message" || msg.StopReason != "end_turn" || msg.Usage.InputTokens != 12 || msg.Usage.OutputTokens != 34 {
		t.Fatalf("агрегат = %s", w.Body.String())
	}
}

// Цельный JSON-объект Responses API (защитная ветка: апстрим обязан стримить,
// но вернул JSON — bufferedToClient раскладывает его в синтетические события).
const responsesJSON = `{"id":"resp_1",` +
	`"output":[{"id":"m1","type":"message","content":[{"type":"output_text","text":"привет"}]}],` +
	`"usage":{"input_tokens":12,"output_tokens":34,` +
	`"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":7}}}`

// F2/P2: JSON×stream — клиент получает корректную SSE-последовательность
// (роль/контент/финал/usage/[DONE]) из полного объекта, а не сырой JSON.
func TestSendJSONResponseToStreamClient(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	e.stub.responses = func(_ *codexStub, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesJSON))
	}
	res, w := e.send(t, upstream.ProtocolOpenAI, openAIBody, true)

	if !res.Streamed || res.ErrorKind != "" {
		t.Fatalf("результат = %+v", res)
	}
	out := w.Body.String()
	for _, want := range []string{
		`"role":"assistant"`,     // синтетический response.created → role-чанк
		`"content":"привет"`,     // output_item.done без дельт → контент одним куском (TR-5)
		`"finish_reason":"stop"`, // completed → финал
		`"prompt_tokens":12`,     // usage-чанк
		`"cached_tokens":5`,      // детали кеша в usage-чанке
		"data: [DONE]",           // TR-4
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в SSE нет %q:\n%s", want, out)
		}
	}
	// tee-usage собран из синтетического completed.
	if res.Usage.PromptTokens != 12 || res.Usage.CompletionTokens != 34 ||
		res.Usage.CachedInputTokens != 5 || res.Usage.ReasoningTokens != 7 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

// F2/P2: JSON×non-stream — агрегатор собирает полный chat.completion
// с корректным usage (клиент получает JSON протокола входа, не JSON codex).
func TestSendJSONResponseToNonStreamClient(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	e.stub.responses = func(_ *codexStub, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesJSON))
	}
	res, w := e.send(t, upstream.ProtocolOpenAI, openAIBody, false)

	if res.Streamed || res.ErrorKind != "" {
		t.Fatalf("результат = %+v", res)
	}
	var chat struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &chat); err != nil {
		t.Fatalf("тело не chat.completion: %v\n%s", err, w.Body.String())
	}
	if chat.Object != "chat.completion" || len(chat.Choices) != 1 ||
		chat.Choices[0].Message.Content != "привет" || chat.Choices[0].FinishReason != "stop" {
		t.Fatalf("агрегат = %s", w.Body.String())
	}
	if chat.Usage == nil || chat.Usage.PromptTokens != 12 || chat.Usage.CompletionTokens != 34 ||
		chat.Usage.PromptTokensDetails == nil || chat.Usage.PromptTokensDetails.CachedTokens != 5 {
		t.Fatalf("usage = %s", w.Body.String())
	}
	if res.Usage.PromptTokens != 12 || res.Usage.CachedInputTokens != 5 || res.Usage.ReasoningTokens != 7 {
		t.Fatalf("tee-usage = %+v", res.Usage)
	}
}

func TestSendSessionIDNewPerRequest(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	e.send(t, upstream.ProtocolOpenAI, openAIBody, false)
	e.send(t, upstream.ProtocolOpenAI, openAIBody, false)
	_, _, _, _, sessions := e.stub.snapshot()
	if len(sessions) != 2 || sessions[0] == sessions[1] {
		t.Fatalf("session-id не обновляется на запрос: %v", sessions)
	}
}

func TestSendNoTokenFailsWithoutNetwork(t *testing.T) {
	e := newSenderEnv(t, TokenSet{}) // пустой Store
	res, _ := e.send(t, upstream.ProtocolOpenAI, openAIBody, false)

	if res.ErrorKind != upstream.KindAuth || res.Stage != upstream.StageAuth || res.ClientStatus != 502 {
		t.Fatalf("результат = %+v", res)
	}
	if !strings.Contains(res.Message, "OAuth") {
		t.Fatalf("сообщение без подсказки OAuth: %q", res.Message)
	}
	hits, _, _, _, _ := e.stub.snapshot()
	if hits != 0 {
		t.Fatalf("апстрим получил %d запросов при отсутствии токена", hits)
	}
}

func TestSend401RefreshRetryExactlyOnce(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-old", RefreshToken: "rt-old", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	var calls int
	e.stub.responses = func(st *codexStub, w http.ResponseWriter, r *http.Request, _ []byte) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"expired","type":"invalid_token"}}`))
			return
		}
		// Ретрай обязан пойти с НОВЫМ токеном.
		if r.Header.Get("Authorization") != "Bearer at-new" {
			t.Errorf("ретрай со старым токеном: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseOK)
	}
	res, _ := e.send(t, upstream.ProtocolOpenAI, openAIBody, false)

	if res.ErrorKind != "" {
		t.Fatalf("результат = %+v", res)
	}
	hits, tokenHits, _, _, _ := e.stub.snapshot()
	if hits != 2 || tokenHits != 1 {
		t.Fatalf("запросов к апстриму=%d, к token=%d (ожидалось 2/1 — ровно один ретрай)", hits, tokenHits)
	}
	// Store обновлён с ротацией refresh-токена.
	ts, _ := e.store.Get("codex/a1")
	if ts.AccessToken != "at-new" || ts.RefreshToken != "rt-new" || ts.AccountID != "acc_9wBk" {
		t.Fatalf("Store после refresh = %+v", ts)
	}
}

func TestSendSecond401IsAuthError(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-old", RefreshToken: "rt-old", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	e.stub.responses = func(_ *codexStub, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"expired","type":"invalid_token"}}`))
	}
	res, _ := e.send(t, upstream.ProtocolOpenAI, openAIBody, false)

	// Второй 401 — KindAuth/StageAuth 502 (OA-4), не upstream.
	if res.ErrorKind != upstream.KindAuth || res.Stage != upstream.StageAuth || res.ClientStatus != 502 {
		t.Fatalf("результат = %+v", res)
	}
	hits, tokenHits, _, _, _ := e.stub.snapshot()
	if hits != 2 || tokenHits != 1 {
		t.Fatalf("запросов=%d/%d (второго refresh быть не должно)", hits, tokenHits)
	}
}

func TestSend429UsageLimitReached(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	e.stub.responses = func(_ *codexStub, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","resets_at":1730009999,"message":"limit"}}`))
	}
	res, _ := e.send(t, upstream.ProtocolOpenAI, openAIBody, false)

	// Клиенту — статус апстрима (SPEC 4), классификация upstream + маркер.
	if res.ClientStatus != 429 || res.ErrorKind != upstream.KindUpstream {
		t.Fatalf("результат = %+v", res)
	}
	if res.Quotas == nil || res.Quotas.Limited == nil {
		t.Fatalf("маркер 429 потерян: %+v", res.Quotas)
	}
	if res.Quotas.Limited.ResetAtUnix != 1730009999 {
		t.Fatalf("resets_at = %d", res.Quotas.Limited.ResetAtUnix)
	}
	// Заголовки окна с 429 тоже доезжают (AC-7).
	if len(res.Quotas.Windows) != 1 || res.Quotas.Windows[0].UsedPercent != 100 {
		t.Fatalf("окна с 429 = %+v", res.Quotas.Windows)
	}
}

func TestSendTranslateErrorAnthropicWithoutMaxTokens(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	res, _ := e.send(t, upstream.ProtocolAnthropic, `{"model":"codex/test","messages":[{"role":"user","content":"hi"}]}`, false)

	if res.ErrorKind != upstream.KindTranslate || res.ClientStatus != 400 {
		t.Fatalf("результат = %+v", res)
	}
	hits, _, _, _, _ := e.stub.snapshot()
	if hits != 0 {
		t.Fatalf("в сеть ушли при translate-ошибке (%d запросов)", hits)
	}
}

func TestSendUpstreamErrorEventMidStream(t *testing.T) {
	e := newSenderEnv(t, TokenSet{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc_1", ExpiresAt: time.Now().Add(time.Hour)})
	e.stub.responses = func(_ *codexStub, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.failed\n"+
			`data: {"response":{"error":{"message":"boom","code":"server_error"}}}`+"\n\n")
	}
	res, w := e.send(t, upstream.ProtocolOpenAI, openAIBody, true)

	if res.ErrorKind != upstream.KindUpstream || !res.Streamed {
		t.Fatalf("результат = %+v", res)
	}
	// Клиенту ушло SSE-событие ошибки + [DONE] (TR-4), а не молчаливый обрыв.
	out := w.Body.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("SSE без ошибки/финала:\n%s", out)
	}
}
