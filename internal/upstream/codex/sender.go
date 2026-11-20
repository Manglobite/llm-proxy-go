// Sender codex (SPEC 7.1): POST {base_url}/backend-api/codex/responses, тело
// — Responses API из нормформы (все клетки матрицы SPEC 8.2: openai-вход →
// ParseOpenAIChat → BuildResponses, anthropic-вход → ParseAnthropic →
// BuildResponses, responses-вход → ParseResponses → BuildResponses —
// roundtrip нормформой; store:false, stream:true ставит BuildResponses
// всегда). Апстрим отвечает ТОЛЬКО SSE, поэтому клиент-не-стрим обслуживают
// агрегаторы (CodexToOpenAIResponse / CodexToAnthropicResponse /
// CodexToResponsesResponse), клиент-стрим — CodexToOpenAIStream /
// CodexToAnthropicStream через PumpStream (keepalive, idle/connect-таймауты —
// по образцу glm); responses-вход — passthrough событий (апстрим УЖЕ в
// Responses-форме, SPEC 8.7) с tee-usage агрегатором.
//
// Auth: Bearer из Store (tokens.json) по ключу "<provider>/<account>";
// 401 → refresh → повтор запроса РОВНО один раз (OA-4), второй 401 —
// KindAuth/StageAuth 502. Квоты x-codex-* читаются из заголовков ответа
// (AC-7) и едут наверх через SendResult.Quotas — в БД их пишет сервер.
//
// Модуль самодостаточен и НЕ импортирует glm/openailike (R3): утилиты
// повторяют их по образцу. R1: http.Client только из socks.Transporter.
package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/translate"
	"llm-proxy/internal/upstream"
)

// pathResponses — путь апстрима поверх origin (SPEC 7.1).
const pathResponses = "/backend-api/codex/responses"

// originator — SPEC 7.1: заголовок originator: codex_cli_rs.
const originator = "codex_cli_rs"

// Version — версия сборки прокси для User-Agent; проставляется при линковке
// (Makefile build: -ldflags -X), дефолт для go test/dev-сборок — "dev".
var Version = "dev"

// Копы капов glm (cap дампа = кап InsertError, больше в БД всё равно не
// попадёт).
const (
	maxResponseDump = 1 << 20
	maxSuccessBody  = 32 << 20
	maxErrPreview   = 512
)

// maxAuthRetry — OA-4: ровно один refresh-и-retry на запрос (attempt
// индексируется с 0). Два запроса к апстриму на один клиентский — предел,
// иначе 401↔refresh зацикливается.
const maxAuthRetry = 1

// Sender — модуль провайдера codex. Нужен Store: access/chatgpt-account-id
// живут в tokens.json, конфигом их не передать.
type Sender struct {
	Store *Store
}

// reportable — узкий интерфейс пути SOCKS для репорта сетевого обрыва чтения
// (FC-9); socks.Path умеет из коробки, socks.Direct — no-op.
type reportable interface {
	ReportNetworkError()
}

func reportNetworkError(tr socks.Transporter) {
	if r, ok := tr.(reportable); ok {
		r.ReportNetworkError()
	}
}

// findAccount — аккаунт по id (защита от рассинхронизированного снапшота).
func findAccount(accounts []config.CodexAccount, id string) *config.CodexAccount {
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	return nil
}

// userAgent — SPEC 7.1: «правдоподобная строка, версия из сборки прокси».
// Версия ОС без новых зависимостей недоступна (CODING 1.1) — в скобках
// повторяется версия сборки (решение зафиксировано в отчёте Ф3).
func userAgent() string {
	return fmt.Sprintf("codex_cli_rs/%s (%s %s; %s) llm-proxy", Version, runtime.GOOS, Version, runtime.GOARCH)
}

// sessionID — uuid v4 из crypto/rand (SPEC 7.1: новый на каждый запрос,
// прокси не хранит сессии; uuid без новой зависимости — CODING 1.1).
func sessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// buildUpstreamBody — сборка тела Responses API по клетке матрицы SPEC 8.2.
// BuildResponses сам ставит store:false/stream:true и переносит
// prompt_cache_key (SPEC 8.3). responses-вход — roundtrip нормформой:
// ParseResponses → BuildResponses (SPEC 8.7, «responses → responses»).
func buildUpstreamBody(r *upstream.SendRequest) ([]byte, error) {
	var norm *translate.NormalizedRequest
	switch r.Protocol {
	case upstream.ProtocolOpenAI:
		n, err := translate.ParseOpenAIChat(r.Body)
		if err != nil {
			return nil, err
		}
		norm = n
	case upstream.ProtocolAnthropic:
		n, err := translate.ParseAnthropic(r.Body)
		if err != nil {
			return nil, err // TR-7: max_tokens и прочие требования anthropic
		}
		norm = n
	case upstream.ProtocolResponses:
		n, err := translate.ParseResponses(r.Body)
		if err != nil {
			return nil, err // previous_response_id и прочие требования SPEC 4/8.3
		}
		norm = n
	default:
		return nil, fmt.Errorf("неизвестный протокол входа %q", r.Protocol)
	}
	return json.Marshal(translate.BuildResponses(norm, r.Target.Model))
}

// translateResult — 400 translate: вход не разбирается в нормформу.
func translateResult(err error) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindTranslate,
		Stage:        upstream.StageTranslate,
		ClientStatus: http.StatusBadRequest,
		Message:      err.Error(),
	}
}

// authResult — OAuth-неудача (нет токена / 401 после refresh / провал
// refresh): 502 auth (таксономия ARCHITECTURE §10).
func authResult(msg string, dump *upstream.Dump) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindAuth,
		Stage:        upstream.StageAuth,
		ClientStatus: http.StatusBadGateway,
		Message:      msg,
		Dump:         dump,
	}
}

// upstreamResult — апстрим повёл себя не по протоколу (502).
func upstreamResult(msg string, dump *upstream.Dump) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindUpstream,
		Stage:        upstream.StageUpstream,
		ClientStatus: http.StatusBadGateway,
		Message:      msg,
		Dump:         dump,
	}
}

// capBytes — кап для дампов (маскирование и финальный кап делает errorslog).
func capBytes(b []byte, max int) []byte {
	if len(b) > max {
		return append(b[:max:max], []byte("...<truncated>")...)
	}
	return b
}

// capBuffer — дозапись в дамп-буфер с капом («что успели» ограничено капом
// errorslog; образец glm).
func capBuffer(buf *bytes.Buffer, chunk []byte, max int) {
	if buf.Len() >= max {
		return
	}
	buf.Write(chunk)
	if buf.Len() > max {
		buf.Truncate(max)
	}
}

// readCapped читает до max байт; truncated — признак превышения капа.
func readCapped(body io.Reader, max int64) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > max {
		return b[:max], true, nil
	}
	return b, false, nil
}

// errPreview — первые max байт тела в тексте ошибки (переносы убираем:
// сообщение уходит в JSON-конверт и в лог).
func errPreview(b []byte, max int) string {
	s := string(b)
	if len(s) > max {
		s = s[:max] + "…<truncated>"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// onIdleMessage — текст idle-обрыва (общий для SSE-клиенту и для errors).
func onIdleMessage(idle time.Duration) string {
	return fmt.Sprintf("стрим апстрима молчал дольше stream_idle_timeout(%v)", idle)
}

// flushWriter — Flush после записи (образец glm).
func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeSSEEvent фреймит событие для клиента: event:-строка (если есть) +
// data-строки + Flush (образец glm).
func writeSSEEvent(w http.ResponseWriter, event string, data []byte) error {
	var sb strings.Builder
	if event != "" {
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteByte('\n')
	}
	for _, line := range strings.Split(string(data), "\n") {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	if _, err := io.WriteString(w, sb.String()); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

// writePing — keepalive-комментарий ': ping' для pump (SPEC 4).
func writePing(w http.ResponseWriter) func() error {
	return func() error {
		if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
			return err
		}
		flushWriter(w)
		return nil
	}
}

// writeOpenAIErrorEvent — SSE-ошибка openai-выхода + [DONE] (TR-4: финал
// обязателен и после ошибки).
func writeOpenAIErrorEvent(w http.ResponseWriter, message, code string) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "api_error", "code": code},
	})
	if err != nil {
		payload = []byte(`{"error":{"message":"stream failed","type":"api_error"}}`)
	}
	_ = writeSSEEvent(w, "", payload)
	_ = writeSSEEvent(w, "", []byte("[DONE]"))
}

// writeAnthropicErrorEvent — SSE-ошибка anthropic-выхода (SPEC 8.4: событие
// error вместо молчаливого обрыва).
func writeAnthropicErrorEvent(w http.ResponseWriter, message, code string) {
	payload, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
	if err != nil {
		payload = []byte(`{"type":"error","error":{"type":"api_error","message":"stream failed"}}`)
	}
	_ = writeSSEEvent(w, "error", payload)
}

// writeResponsesErrorEvent — SSE-ошибка Responses-выхода (SPEC 8.7: событие
// error с data {"type":"error","code","message"}); response.completed после
// ошибки НЕ шлётся — стрим не должен выглядеть завершённым (та же семантика,
// что у ChatToResponsesStream.Fail — здесь passthrough, транслятора нет).
func writeResponsesErrorEvent(w http.ResponseWriter, message, code string) {
	if message == "" {
		message = "upstream stream failed" // пустое сообщение не должно маскировать ошибку
	}
	payload, err := json.Marshal(map[string]any{
		"type": "error", "code": code, "message": message,
	})
	if err != nil {
		payload = []byte(`{"type":"error","message":"stream failed"}`)
	}
	_ = writeSSEEvent(w, "error", payload)
}

// Send — реализация server.Sender. Контракт — в godoc пакета upstream:
// классифицированные неудачи возвращаются РЕЗУЛЬТАТОМ, (nil, error) — только
// нарушение контракта вызова.
func (s Sender) Send(ctx context.Context, r *upstream.SendRequest) (*upstream.SendResult, error) {
	if s.Store == nil {
		return nil, errors.New("codex: Store не задан (main обязан открыть tokens.json)")
	}
	if r.Target.ProviderType != config.ProviderCodex {
		return nil, fmt.Errorf("codex: тип провайдера %q — не %s", r.Target.ProviderType, config.ProviderCodex)
	}
	provFull, ok := r.Snap.Cfg.Providers[r.Target.ProviderName]
	if !ok || provFull.Codex == nil {
		return nil, fmt.Errorf("codex: провайдер %q не найден в снапшоте", r.Target.ProviderName)
	}
	prov := provFull.Codex
	acc := findAccount(prov.Accounts, r.Target.AccountID)
	if acc == nil {
		return nil, fmt.Errorf("codex: аккаунт %q не найден у провайдера %q", r.Target.AccountID, r.Target.ProviderName)
	}
	if r.Transporter == nil {
		return nil, errors.New("codex: transporter аккаунта не передан")
	}
	if r.W == nil {
		return nil, errors.New("codex: ResponseWriter клиента не передан")
	}

	key := AccountKey(r.Target.ProviderName, r.Target.AccountID)
	ts, ok := s.Store.Get(key)
	// Нет токена — в сеть не ходим вовсе (SPEC 7.1): OAuth ещё не выполнялся.
	if !ok || ts.AccessToken == "" {
		return authResult("нет токена для аккаунта "+key+", выполните OAuth", nil), nil
	}

	body, err := buildUpstreamBody(r)
	if err != nil {
		return translateResult(err), nil
	}

	// Цикл попыток: максимум первая + один refresh-и-retry (OA-4).
	for attempt := 0; ; attempt++ {
		httpReq, dump, err := s.newUpstreamRequest(prov, ts, body)
		if err != nil {
			return nil, err // нарушение сборки (endpoint/uuid), не свойство апстрима
		}

		// ConnectTimeout (SPEC 4) — таймер-канцеляция вокруг client.Do
		// (образец glm): после заголовков таймер остановлен, ctx живёт до
		// конца стрима.
		reqCtx, cancelReq := context.WithCancel(ctx)
		var connectFired atomic.Bool
		var connectTimer *time.Timer
		if r.ConnectTimeout > 0 {
			connectTimer = time.AfterFunc(r.ConnectTimeout, func() {
				connectFired.Store(true)
				cancelReq()
			})
		}

		resp, err := r.Transporter.Client().Do(httpReq.WithContext(reqCtx))
		if connectTimer != nil {
			connectTimer.Stop()
		}
		if err != nil {
			cancelReq()
			// Выстреливший connect-таймер (в том числе во время синхронного
			// гейт-чека) — сетевой таймаут, а не блокировка пути: 502 network.
			if connectFired.Load() {
				return &upstream.SendResult{
					ErrorKind:    upstream.KindNetwork,
					Stage:        upstream.StageUpstream,
					ClientStatus: http.StatusBadGateway,
					Message:      fmt.Sprintf("upstream_connect_timeout(%v) истёк до заголовков апстрима", r.ConnectTimeout),
					Dump:         dump,
				}, nil
			}
			var be *socks.BlockedError
			if errors.As(err, &be) {
				// Gate закрыл путь ДО создания соединения (I4): 503, к апстриму
				// никто не сходил (FC-5/SV-6).
				return &upstream.SendResult{
					ErrorKind:    upstream.KindSocksBlocked,
					Stage:        upstream.StageSocksCheck,
					ClientStatus: http.StatusServiceUnavailable,
					Message:      be.Error(),
					Dump:         dump,
				}, nil
			}
			return &upstream.SendResult{
				ErrorKind:    upstream.KindNetwork,
				Stage:        upstream.StageUpstream,
				ClientStatus: http.StatusBadGateway,
				Message:      "апстрим недоступен: " + err.Error(),
				Dump:         dump,
			}, nil
		}
		if connectFired.Load() {
			// Редкая гонка: заголовки пришли, но таймер успел выстрелить — тело
			// мертво (ctx уже отменён), использовать ответ нельзя (образец glm).
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cancelReq()
			return &upstream.SendResult{
				ErrorKind:    upstream.KindNetwork,
				Stage:        upstream.StageUpstream,
				ClientStatus: http.StatusBadGateway,
				Message:      fmt.Sprintf("upstream_connect_timeout(%v) истёк одновременно с получением заголовков", r.ConnectTimeout),
				Dump:         dump,
			}, nil
		}

		if resp.StatusCode == http.StatusUnauthorized {
			// 401 (OA-4): refresh через клиент аккаунта → повтор РОВНО один
			// раз; второй 401 — auth 502 (не upstream: причина в токенах).
			dump401, rerr := drainForDump(resp, dump)
			resp.Body.Close()
			cancelReq()
			if rerr != nil {
				reportNetworkError(r.Transporter)
			}
			if attempt >= maxAuthRetry {
				return authResult("апстрим ответил 401 и после обновления токена — требуется повторный OAuth", dump401), nil
			}
			fresh, rerr := refreshAccount(ctx, r.Transporter, prov.AuthBaseURL, ts)
			if rerr != nil {
				return authResult("refresh токена не удался после 401: "+rerr.Error(), dump401), nil
			}
			if serr := s.Store.Set(key, *fresh); serr != nil {
				// In-memory уже перезаписан (Set пишет карту до файла) — ретрай
				// пойдёт с новым токеном, файл догонит при следующем Set.
				slog.Warn("codex: запись tokens.json не удалась", "account", key, "err", serr)
			}
			ts = *fresh
			continue
		}

		res := s.consumeResponse(r, resp, reqCtx, cancelReq, dump)
		res.UpstreamStatus = resp.StatusCode
		res.UpstreamHeader = resp.Header.Clone()
		if res.Dump == nil {
			res.Dump = dump
		}
		return res, nil
	}
}

// newUpstreamRequest — запрос апстрима со всеми заголовками SPEC 7.1 (OA-7).
// Каждый вызов генерирует новый session-id (SPEC: на каждый запрос) и новый
// reader тела (после 401-ретрая тело перечитывается).
func (s Sender) newUpstreamRequest(prov *config.CodexProvider, ts TokenSet, body []byte) (*http.Request, *upstream.Dump, error) {
	endpoint := strings.TrimRight(prov.BaseURL, "/") + pathResponses
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("codex: запрос не собирается: %w", err)
	}
	sid, err := sessionID()
	if err != nil {
		return nil, nil, fmt.Errorf("codex: session-id: %w", err)
	}
	h := httpReq.Header
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+ts.AccessToken)
	// chatgpt-account-id — из tokens.json (claims id_token, SPEC 3.2/7.1).
	h.Set("chatgpt-account-id", ts.AccountID)
	h.Set("originator", originator)
	h.Set("User-Agent", userAgent())
	h.Set("session-id", sid)
	// Апстрим всегда стримит (stream:true в теле) — Accept SSE безусловно.
	h.Set("Accept", "text/event-stream")

	dump := &upstream.Dump{
		Method:     http.MethodPost,
		URL:        endpoint,
		ReqHeaders: httpReq.Header,
		ReqBody:    capBytes(body, maxResponseDump),
	}
	return httpReq, dump, nil
}

// refreshAccount — refresh через клиент аккаунта (его SOCKS, OA-3).
// Ротация refresh-токена обеспечивается самим Refresh: ответ без нового
// refresh_token отклоняется, ключ Store перезаписывается целиком свежим
// TokenSet (вызов Store.Set — в Send, рядом с логом сбоя записи).
func refreshAccount(ctx context.Context, tr socks.Transporter, authBase string, prev TokenSet) (*TokenSet, error) {
	if prev.RefreshToken == "" {
		return nil, errors.New("refresh_token отсутствует — требуется повторный OAuth")
	}
	return Refresh(ctx, tr.Client(), authBase, prev.RefreshToken)
}

// drainForDump — читает тело не-2xx ответа для дампа (ошибка чтения — не
// повод отказываться от дампа: фиксируем, что успели).
func drainForDump(resp *http.Response, dump *upstream.Dump) (*upstream.Dump, error) {
	body, _, err := readCapped(resp.Body, maxResponseDump)
	if err != nil {
		body = []byte("<ошибка чтения тела ошибки апстрима: " + err.Error() + ">")
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = body
	return dump, err
}

// consumeResponse — ветка после получения заголовков: не-2xx (квоты + 429),
// SSE-стрим или неожиданный цельный JSON. Владеет resp.Body и cancelReq.
func (s Sender) consumeResponse(r *upstream.SendRequest, resp *http.Response,
	reqCtx context.Context, cancelReq context.CancelFunc, dump *upstream.Dump) *upstream.SendResult {

	defer resp.Body.Close()
	defer cancelReq()

	// AC-7: x-codex-* приходят с HTTP-ответом стрима — читаем из resp.Header,
	// а НЕ из body-чанков (REVIEW §14, типовая ловушка).
	quotas := ParseQuotaHeaders(resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return upstreamErrorResult(r, resp, dump, quotas)
	}

	// Ветка выбирается по ФОРМЕ ОТВЕТА апстрима И желанию клиента: апстрим
	// codex всегда стримит (stream:true), клиент может просить и то и другое.
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "text/event-stream") && r.Stream:
		return s.pumpToClient(r, resp, reqCtx, cancelReq, dump, quotas)
	case strings.Contains(ct, "text/event-stream"):
		// Клиент не просил стрим: живой SSE агрегируется синхронно —
		// полный ответ пишется клиенту одним JSON (агрегаторы Ф1).
		return s.scanToClient(r, resp, dump, quotas)
	}
	// Апстрим обязан стримить, но вернул цельный JSON — обрабатываем
	// защищённо: полный объект Responses API раскладывается в синтетические
	// события и идёт через ТЕ ЖЕ трансляторы (выдавать JSON клиенту за SSE
	// нельзя, глотать ответ тоже нельзя).
	return s.bufferedToClient(r, resp, dump, quotas)
}

// upstreamErrorResult — не-2xx апстрима (ARCHITECTURE §10): клиенту статус
// апстрима + проброс retry-after/x-ratelimit-* (делает сервер), в errors —
// полный запрос+ответ. 429 usage_limit_reached (AC-8) дополнительно
// фиксируется как исчерпание окна (Quotas.Limited).
func upstreamErrorResult(r *upstream.SendRequest, resp *http.Response, dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {
	body, _, err := readCapped(resp.Body, maxResponseDump)
	if err != nil {
		// Обрыв уже при чтении тела ОШИБКИ — сетевой сбой пути (FC-9).
		reportNetworkError(r.Transporter)
		body = []byte("<ошибка чтения тела ошибки апстрима: " + err.Error() + ">")
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = body

	res := &upstream.SendResult{
		ErrorKind:    upstream.KindUpstream,
		Stage:        upstream.StageUpstream,
		ClientStatus: resp.StatusCode, // SPEC 4: клиенту — статус апстрима
		Message: fmt.Sprintf("апстрим ответил %d: %s", resp.StatusCode,
			errPreview(body, maxErrPreview)),
		Quotas: quotas,
		Dump:   dump,
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if resets, ok := ParseUsageLimitBody(body); ok {
			if quotas == nil {
				quotas = &upstream.Quotas{}
			}
			quotas.Limited = &upstream.QuotaLimit{
				ResetAtUnix: resets,
				Message:     errPreview(body, maxErrPreview),
			}
			res.Quotas = quotas // res мог быть создан с nil — переприсваиваем после сборки
		}
	}
	return res
}

// pumpToClient — 2xx SSE: события Responses API через трансляторы Ф1
// (CodexToOpenAIStream / CodexToAnthropicStream) и PumpStream (keepalive
// ': ping', idle-таймаут, FC-9 — по образцу glm).
func (s Sender) pumpToClient(r *upstream.SendRequest, resp *http.Response,
	reqCtx context.Context, cancelReq context.CancelFunc, dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {

	h := r.W.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	anthropic := r.Protocol == upstream.ProtocolAnthropic
	var (
		openai  *translate.CodexToOpenAIStream
		anth    *translate.CodexToAnthropicStream
		respAgg *translate.CodexToResponsesResponse
	)
	var feeder translate.Feeder
	switch r.Protocol {
	case upstream.ProtocolOpenAI:
		openai = translate.NewCodexToOpenAI(r.W)
		feeder = openai
	case upstream.ProtocolAnthropic:
		anth = translate.NewCodexToAnthropic(r.W)
		feeder = anth
	case upstream.ProtocolResponses:
		// SPEC 8.7 (codex-апстрим): события апстрима УЖЕ Responses —
		// passthrough как есть (байт-в-байт по образцу passthrough glm);
		// транслятора нет, tee-usage/финал/ошибку копит агрегатор.
		respAgg = translate.NewCodexToResponsesResponse()
	default:
		return translateResult(fmt.Errorf("неизвестный протокол входа %q", r.Protocol))
	}

	var rawSeen bytes.Buffer // «что успели» от апстрима — для дампа (SPEC 10)
	onEvent := func(ev upstream.SSEEvent) error {
		capBuffer(&rawSeen, ev.Data, maxResponseDump)
		if respAgg != nil {
			// Tee агрегатором (Feed игнорирует неизвестные события и события
			// после финала), событие клиенту — как есть.
			if err := respAgg.Feed(ev.Event, ev.Data); err != nil {
				return err
			}
			return writeSSEEvent(r.W, ev.Event, ev.Data)
		}
		return feeder.Feed(ev.Event, ev.Data)
	}
	onIdle := func() {
		// Idle-таймаут — обрыв с SSE-ошибкой в протоколе выхода (SPEC 4).
		msg := onIdleMessage(r.IdleTimeout)
		if respAgg != nil {
			writeResponsesErrorEvent(r.W, msg, "idle_timeout")
			return
		}
		if anthropic {
			writeAnthropicErrorEvent(r.W, msg, "idle_timeout")
			return
		}
		writeOpenAIErrorEvent(r.W, msg, "idle_timeout")
	}

	pr := upstream.PumpStream(reqCtx, resp.Body, cancelReq, r.Keepalive, r.IdleTimeout, writePing(r.W), onEvent, onIdle)

	res := &upstream.SendResult{
		Streamed: true, // заголовки SSE уже ушли клиенту: HTTP-конверт невозможен
		Quotas:   quotas,
		Dump:     dump,
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = capBytes(rawSeen.Bytes(), maxResponseDump)

	var (
		usage     translate.Usage
		finished  bool
		streamErr error
	)
	if respAgg != nil {
		usage, finished, streamErr = respAgg.Usage(), respAgg.Finished(), respAgg.Err()
	} else {
		usage, finished, streamErr = translatorState(openai, anth)
	}
	res.Usage = usage

	switch {
	case pr.Idle:
		res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
		res.ClientStatus = http.StatusOK // клиент уже получил 200 + SSE-ошибку
		res.Message = "обрыв стрима: " + onIdleMessage(r.IdleTimeout)
	case pr.ClientGone || pr.WriteErr != nil:
		res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "клиент отключился посреди стрима"
	case pr.ReadErr != nil:
		// reportNetworkError пути (FC-9): сетевой обрыв посреди стрима.
		reportNetworkError(r.Transporter)
		res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "обрыв чтения апстрима посреди стрима: " + pr.ReadErr.Error()
		// Терминируем стрим событием ошибки в формате выхода (SPEC 4), иначе
		// клиент ждёт финала до своего таймаута.
		if respAgg != nil {
			writeResponsesErrorEvent(r.W, res.Message, "stream_interrupted")
		} else if anthropic {
			writeAnthropicErrorEvent(r.W, res.Message, "stream_interrupted")
		} else {
			writeOpenAIErrorEvent(r.W, res.Message, "stream_interrupted")
		}
	case streamErr != nil:
		// response.failed/error пришёл СОБЫТИЕМ и уже эмитнут клиенту
		// транслятором (SPEC 8.4: error-событие вместо молчаливого обрыва).
		res.ErrorKind, res.Stage = upstream.KindUpstream, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "апстрим прислал ошибку посреди стрима: " + streamErr.Error()
	case !finished:
		// EOF без финального события: ответ усечён — терминируем и фиксируем.
		res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "стрим апстрима оборвался без финального события Responses API"
		if respAgg != nil {
			writeResponsesErrorEvent(r.W, res.Message, "stream_interrupted")
		} else if anthropic {
			writeAnthropicErrorEvent(r.W, res.Message, "stream_interrupted")
		} else {
			writeOpenAIErrorEvent(r.W, res.Message, "stream_interrupted")
		}
	}
	return res
}

// scanToClient — 2xx SSE при НЕ-стрим клиенте: живой поток агрегируется
// синхронно (агрегаторы Ф1), полный JSON пишется одним ответом. Клиент ничего
// не получил до завершения агрегации — любая неудача возвращает
// классифицированный результат, конверт пишет сервер.
func (s Sender) scanToClient(r *upstream.SendRequest, resp *http.Response,
	dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {

	var rawSeen bytes.Buffer
	usage, finished, streamErr, readErr, payload, mErr := aggregateProtocol(r, func(yield func(string, []byte) error) error {
		return translate.ScanSSE(resp.Body, func(event string, data []byte) error {
			capBuffer(&rawSeen, data, maxResponseDump)
			return yield(event, data)
		})
	})
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = capBytes(rawSeen.Bytes(), maxResponseDump)

	if readErr != nil {
		// Сетевой обрыв чтения апстрима (FC-9): путь уходит в needCheck.
		reportNetworkError(r.Transporter)
		return &upstream.SendResult{
			ErrorKind:    upstream.KindNetwork,
			Stage:        upstream.StageStream,
			ClientStatus: http.StatusBadGateway,
			Message:      "обрыв чтения апстрима посреди стрима: " + readErr.Error(),
			Quotas:       quotas,
			Dump:         dump,
		}
	}
	return writeAggregate(r, usage, finished, streamErr, payload, mErr, dump, quotas)
}

// aggregateProtocol — прогон источника событий через агрегатор протокола
// входа; общий для живого SSE (scanToClient) и синтетики (emitAggregate).
// Возвращает (usage, finished, streamErr, payload, marshalErr).
func aggregateProtocol(r *upstream.SendRequest, src func(yield func(string, []byte) error) error) (
	usage translate.Usage, finished bool, streamErr error, feedErr error, payload []byte, mErr error) {

	switch r.Protocol {
	case upstream.ProtocolOpenAI:
		agg := translate.NewCodexToOpenAIResponse()
		if err := src(agg.Feed); err != nil {
			return agg.Usage(), agg.Finished(), agg.Err(), err, nil, nil
		}
		p, err := json.Marshal(agg.Result())
		return agg.Usage(), agg.Finished(), agg.Err(), nil, p, err
	case upstream.ProtocolAnthropic:
		agg := translate.NewCodexToAnthropicResponse()
		if err := src(agg.Feed); err != nil {
			return agg.Usage(), agg.Finished(), agg.Err(), err, nil, nil
		}
		p, err := json.Marshal(agg.Result())
		return agg.Usage(), agg.Finished(), agg.Err(), nil, p, err
	case upstream.ProtocolResponses:
		// SPEC 8.7 («codex-апстрим», non-stream): агрегатор собирает события
		// в полный объект Responses API (id/model/items — как есть).
		agg := translate.NewCodexToResponsesResponse()
		if err := src(agg.Feed); err != nil {
			return agg.Usage(), agg.Finished(), agg.Err(), err, nil, nil
		}
		p, err := json.Marshal(agg.Result())
		return agg.Usage(), agg.Finished(), agg.Err(), nil, p, err
	default:
		return translate.Usage{}, false, nil, fmt.Errorf("неизвестный протокол входа %q", r.Protocol), nil, nil
	}
}

// writeAggregate — общий хвост агрегации: классификация неудач (клиент ещё
// НИЧЕГО не получил — конверт пишет сервер) и запись полного JSON-ответа.
func writeAggregate(r *upstream.SendRequest, usage translate.Usage, finished bool, streamErr error,
	payload []byte, mErr error, dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {

	if mErr != nil {
		return upstreamResult("marshal ответа: "+mErr.Error(), dump)
	}
	if streamErr != nil {
		return &upstream.SendResult{
			ErrorKind:    upstream.KindUpstream,
			Stage:        upstream.StageUpstream,
			ClientStatus: http.StatusBadGateway,
			Message:      "апстрим прислал ошибку посреди стрима: " + streamErr.Error(),
			Quotas:       quotas,
			Dump:         dump,
		}
	}
	if !finished {
		return &upstream.SendResult{
			ErrorKind:    upstream.KindNetwork,
			Stage:        upstream.StageStream,
			ClientStatus: http.StatusBadGateway,
			Message:      "стрим апстрима оборвался без финального события Responses API",
			Quotas:       quotas,
			Dump:         dump,
		}
	}
	r.W.Header().Set("Content-Type", "application/json")
	r.W.WriteHeader(http.StatusOK)
	if _, err := r.W.Write(payload); err != nil {
		// Клиент ушёл при записи финала: апстрим отработал, usage посчитан.
		return &upstream.SendResult{
			ErrorKind:    upstream.KindNetwork,
			Stage:        upstream.StageStream,
			ClientStatus: http.StatusOK,
			Message:      "клиент отключился при записи ответа: " + err.Error(),
			Streamed:     true,
			Usage:        usage,
			Quotas:       quotas,
			Dump:         dump,
		}
	}
	// Flush сразу после тела: успешный ответ уходит клиенту до SQLite-вставок
	// сервера (AC-4), как у glm.
	flushWriter(r.W)
	return &upstream.SendResult{Usage: usage, Quotas: quotas, Dump: dump}
}

// translatorState — (usage, finished, err) стрим-транслятора по протоколу:
// общий движок один, но конкретные типы различаются — свитч в одном месте.
func translatorState(openai *translate.CodexToOpenAIStream, anth *translate.CodexToAnthropicStream) (translate.Usage, bool, error) {
	if anth != nil {
		return anth.Usage(), anth.Finished(), anth.Err()
	}
	return openai.Usage(), openai.Finished(), openai.Err()
}

// bufferedToClient — 2xx без SSE (защитная ветка): полный объект Responses API
// раскладывается в синтетические события и проходит через те же трансляторы.
func (s Sender) bufferedToClient(r *upstream.SendRequest, resp *http.Response,
	dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {

	body, truncated, err := readCapped(resp.Body, maxSuccessBody)
	if err != nil {
		// FC-9 не только для стримов: обрыв non-stream тела — тоже сетевой
		// сбой пути, иначе путь остаётся fresh со сломанным транспортом.
		reportNetworkError(r.Transporter)
		return &upstream.SendResult{
			ErrorKind:    upstream.KindNetwork,
			Stage:        upstream.StageUpstream,
			ClientStatus: http.StatusBadGateway,
			Message:      "чтение тела апстрима оборвалось: " + err.Error(),
		}
	}
	if truncated {
		return upstreamResult(fmt.Sprintf("non-stream ответ апстрима превышает %d байт", maxSuccessBody), dump)
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = capBytes(body, maxResponseDump)

	events, err := syntheticEvents(body)
	if err != nil {
		return upstreamResult("тело апстрима не парсится как объект Responses API: "+err.Error(), dump)
	}

	if r.Stream {
		// Клиент просил SSE: синтезируем корректный стрим из полного объекта.
		h := r.W.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		return s.emitStream(r, events, dump, quotas)
	}
	return s.emitAggregate(r, events, dump, quotas)
}

// syntheticEvents — полный объект Responses API → последовательность событий
// того же декодера, что ест живой стрим (decodeCodexEvent): created,
// output_item.done на каждый item (messageText/tool-аргументы читаются там)
// и completed (usage + stop-причина из output).
func syntheticEvents(body []byte) ([]upstream.SSEEvent, error) {
	var probe struct {
		Output []json.RawMessage `json:"output"`
		Usage  json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if probe.Usage == nil {
		// completed без usage сломал бы учёт — считаем тело не ответом API.
		return nil, errors.New("нет поля usage")
	}
	events := make([]upstream.SSEEvent, 0, len(probe.Output)+2)
	events = append(events, upstream.SSEEvent{Event: "response.created", Data: []byte(`{"response":{}}`)})
	for _, item := range probe.Output {
		data, err := json.Marshal(map[string]json.RawMessage{"item": item})
		if err != nil {
			return nil, err
		}
		events = append(events, upstream.SSEEvent{Event: "response.output_item.done", Data: data})
	}
	completed := append(append(make([]byte, 0, len(body)+16), `{"response":`...), body...)
	completed = append(completed, '}')
	events = append(events, upstream.SSEEvent{Event: "response.completed", Data: completed})
	return events, nil
}

// emitStream — прогон готовых событий через стрим-транслятор (SSE клиенту).
func (s Sender) emitStream(r *upstream.SendRequest, events []upstream.SSEEvent,
	dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {

	res := &upstream.SendResult{Streamed: true, Quotas: quotas, Dump: dump}

	var usage translate.Usage
	var finished bool
	var streamErr, writeErr error
	if r.Protocol == upstream.ProtocolAnthropic {
		t := translate.NewCodexToAnthropic(r.W)
		for _, ev := range events {
			if err := t.Feed(ev.Event, ev.Data); err != nil {
				writeErr = err
				break
			}
		}
		usage, finished, streamErr = t.Usage(), t.Finished(), t.Err()
	} else if r.Protocol == upstream.ProtocolResponses {
		// Синтетические события уже в форме Responses — passthrough как в
		// живом стриме (pumpToClient); usage/финал копит tee-агрегатор.
		agg := translate.NewCodexToResponsesResponse()
		for _, ev := range events {
			if err := agg.Feed(ev.Event, ev.Data); err != nil {
				break // теоретически недостижимо: Feed агрегатора не ошибается
			}
			if err := writeSSEEvent(r.W, ev.Event, ev.Data); err != nil {
				writeErr = err
				break
			}
		}
		usage, finished, streamErr = agg.Usage(), agg.Finished(), agg.Err()
	} else {
		t := translate.NewCodexToOpenAI(r.W)
		for _, ev := range events {
			if err := t.Feed(ev.Event, ev.Data); err != nil {
				writeErr = err
				break
			}
		}
		usage, finished, streamErr = t.Usage(), t.Finished(), t.Err()
	}
	res.Usage = usage

	switch {
	case writeErr != nil:
		res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "клиент отключился посреди стрима: " + writeErr.Error()
	case streamErr != nil:
		res.ErrorKind, res.Stage = upstream.KindUpstream, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "апстрим прислал ошибку посреди стрима: " + streamErr.Error()
	case !finished:
		res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = "ответ апстрима без финального события Responses API"
	}
	return res
}

// emitAggregate — прогон готовых (синтетических) событий через агрегатор и
// запись полного JSON-ответа протокола входа (клиент не просил стрим).
func (s Sender) emitAggregate(r *upstream.SendRequest, events []upstream.SSEEvent,
	dump *upstream.Dump, quotas *upstream.Quotas) *upstream.SendResult {

	usage, finished, streamErr, feedErr, payload, mErr := aggregateProtocol(r, func(yield func(string, []byte) error) error {
		for _, ev := range events {
			if err := yield(ev.Event, ev.Data); err != nil {
				return err
			}
		}
		return nil
	})
	if feedErr != nil {
		return upstreamResult("агрегатор: "+feedErr.Error(), dump)
	}
	return writeAggregate(r, usage, finished, streamErr, payload, mErr, dump, quotas)
}
