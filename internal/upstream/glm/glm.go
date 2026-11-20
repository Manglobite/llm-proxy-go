// Пакет upstream/glm — модуль провайдера glm (z.ai, SPEC 7.2). Публичного
// OAuth нет — только API-ключи (Authorization: Bearer). Два апстрима, выбор
// по ПРОТОКОЛУ входа (passthrough-приоритет, SPEC 8.2):
//
//   - anthropic-вход → {origin}/api/anthropic/v1/messages: passthrough, из
//     трансляций — ТОЛЬКО RewriteModel. InjectIncludeUsage здесь звать
//     ЗАПРЕЩЕНО: он вставил бы openai-поле stream_options в anthropic-тело
//     (заметка Ф1), а anthropic-апстрим такие поля не понимает;
//   - openai-вход → coding_plan аккаунта выбирает эндпоинт:
//     {origin}/api/coding/paas/v4/chat/completions либо
//     {origin}/api/paas/v4/chat/completions — passthrough + RewriteModel +
//     InjectIncludeUsage для стримов (как у openailike, SPEC 7.3/8.2);
//   - responses-вход → ТОТ ЖЕ чат-путь (как openai-вход): конвертация
//     responses → chat (ParseResponses → BuildChat, SPEC 8.3), ответ —
//     ChatToResponses / ChatToResponsesStream (SPEC 8.7).
//
// Заголовки: openai-апстрим — только Bearer; anthropic-апстрим — Bearer
// (z.ai принимает Bearer) + anthropic-version: 2023-06-01 (SPEC 7.2).
//
// Модуль самодостаточен и НЕ импортирует openailike (R3: провайдер не знает
// о других провайдерах) — структура/классификация ошибок повторяют его по
// образцу (connect-таймаут, FC-9, таксономия, дампы).
//
// R1: http.Client берётся только из socks.Transporter аккаунта, своих
// клиентов пакет не создаёт.
package glm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/translate"
	"llm-proxy/internal/upstream"
)

// Пути апстримов z.ai поверх origin (OA-8: ровно эти пути).
const (
	pathAnthropicMessages = "/api/anthropic/v1/messages"
	pathOpenAIChat        = "/api/paas/v4/chat/completions"
	pathOpenAICodingChat  = "/api/coding/paas/v4/chat/completions"

	// anthropicVersion — версия протокола anthropic (SPEC 4/7.2): прокси
	// ставит её сам, что бы ни прислал клиент.
	anthropicVersion = "2023-06-01"
)

// Копы тел/дампов — те же значения, что у openailike (кап дампа = кап
// InsertError, больше в БД всё равно не попадёт).
const (
	maxResponseDump = 1 << 20
	maxSuccessBody  = 32 << 20
	maxErrPreview   = 512
)

// Sender — модуль провайдера glm. Stateless: всё состояние запроса приходит
// в SendRequest (интерфейс server.Sender объявлен потребителем).
type Sender struct{}

// reportable — узкий интерфейс пути SOCKS для репорта сетевого обрыва
// чтения (FC-9); объявлен потребителем (CODING 1.6), socks.Path умеет его
// из коробки, socks.Direct — no-op.
type reportable interface {
	ReportNetworkError()
}

// reportNetworkError пути при сетевом обрыве чтения (FC-9): путь уходит в
// needCheck, следующий запрос сперва проверит его.
func reportNetworkError(tr socks.Transporter) {
	if r, ok := tr.(reportable); ok {
		r.ReportNetworkError()
	}
}

// findAccount — аккаунт по id (валидация конфига гарантирует существование,
// проверка — защита от рассинхронизированного снапшота).
func findAccount(accounts []config.GLMAccount, id string) *config.GLMAccount {
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	return nil
}

// endpointFor — URL апстрима по протоколу входа и coding_plan аккаунта
// (OA-8). origin — эффективный base_url провайдера (config резолвит дефолт
// https://api.z.ai, SPEC 7.2). responses-вход идёт в ЧАТ-путь наравне с
// openai (клетка responses → chat, SPEC 8.2/8.3): выбор coding/standard —
// тот же, по acc.CodingPlan.
func endpointFor(origin string, acc *config.GLMAccount, protocol string) string {
	o := strings.TrimRight(origin, "/")
	switch {
	case protocol == upstream.ProtocolAnthropic:
		return o + pathAnthropicMessages
	case acc.CodingPlan:
		return o + pathOpenAICodingChat
	default:
		return o + pathOpenAIChat
	}
}

// buildUpstreamBody — сборка тела апстрима по клетке матрицы SPEC 8.2.
// Клетки glm openai/anthropic — passthrough (подмена только модели, TR-9:
// cache_control переживает байт-в-байт). InjectIncludeUsage — только на
// openai-клетке и только для стрима; на anthropic-клетке его НЕТ (см. godoc
// пакета). Клетка responses — конвертация roundtrip'ом нормформы (SPEC 8.3);
// BuildChat сам ставит stream_options.include_usage для стрима.
func buildUpstreamBody(r *upstream.SendRequest) ([]byte, error) {
	switch r.Protocol {
	case upstream.ProtocolOpenAI:
		body, err := translate.RewriteModel(r.Body, r.Target.Model)
		if err != nil {
			return nil, err
		}
		if r.Stream {
			return translate.InjectIncludeUsage(body)
		}
		return body, nil
	case upstream.ProtocolAnthropic:
		return translate.RewriteModel(r.Body, r.Target.Model)
	case upstream.ProtocolResponses:
		norm, err := translate.ParseResponses(r.Body)
		if err != nil {
			return nil, err
		}
		return json.Marshal(translate.BuildChat(norm, r.Target.Model, r.Stream))
	default:
		return nil, fmt.Errorf("неизвестный протокол входа %q", r.Protocol)
	}
}

// clientAlias — имя модели из ЗАПРОСА КЛИЕНТА (поле model входа /v1/responses).
// Клиент ждёт в response.created/completed алиас, который сам и просил
// (SPEC 8.7), а не модель апстрима, в которую запрос переписал роутер. Пустая
// строка допустима: транслятор тогда подставит model чанка апстрима (поведение
// SetModel-менее — защита на случай неожиданной сборки).
func clientAlias(body []byte) string {
	var head struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &head)
	return head.Model
}

// translateResult — 400 translate: passthrough-тело не JSON (подменять
// модель в не-объекте нельзя).
func translateResult(err error) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindTranslate,
		Stage:        upstream.StageTranslate,
		ClientStatus: http.StatusBadRequest,
		Message:      err.Error(),
	}
}

// capBytes — кап для дампов (маскирование и финальный кап делает errorslog).
func capBytes(b []byte, max int) []byte {
	if len(b) > max {
		return append(b[:max:max], []byte("...<truncated>")...)
	}
	return b
}

// Send — реализация server.Sender. Контракт — в godoc пакета upstream:
// классифицированные неудачи возвращаются РЕЗУЛЬТАТОМ, (nil, error) — только
// нарушение контракта вызова.
func (Sender) Send(ctx context.Context, r *upstream.SendRequest) (*upstream.SendResult, error) {
	if r.Target.ProviderType != config.ProviderGLM {
		return nil, fmt.Errorf("glm: тип провайдера %q — не %s", r.Target.ProviderType, config.ProviderGLM)
	}
	prov, ok := r.Snap.Cfg.Providers[r.Target.ProviderName]
	if !ok || prov.GLM == nil {
		return nil, fmt.Errorf("glm: провайдер %q не найден в снапшоте", r.Target.ProviderName)
	}
	acc := findAccount(prov.GLM.Accounts, r.Target.AccountID)
	if acc == nil {
		return nil, fmt.Errorf("glm: аккаунт %q не найден у провайдера %q", r.Target.AccountID, r.Target.ProviderName)
	}
	if r.Transporter == nil {
		return nil, errors.New("glm: transporter аккаунта не передан")
	}
	if r.W == nil {
		return nil, errors.New("glm: ResponseWriter клиента не передан")
	}

	body, err := buildUpstreamBody(r)
	if err != nil {
		// Тело клиента не JSON — passthrough невозможен: translate-ошибка,
		// 400 в конверте протокола входа.
		return translateResult(err), nil
	}

	endpoint := endpointFor(prov.GLM.BaseURL, acc, r.Protocol)
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("glm: запрос не собирается: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+acc.Auth.Key)
	if r.Protocol == upstream.ProtocolAnthropic {
		// anthropic-version прокси ставит сам (SPEC 4: заголовок клиента
		// принимается, но к апстриму идёт наш).
		httpReq.Header.Set("anthropic-version", anthropicVersion)
	}
	if r.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	dump := &upstream.Dump{
		Method:     http.MethodPost,
		URL:        endpoint,
		ReqHeaders: httpReq.Header,
		ReqBody:    capBytes(body, maxResponseDump),
	}

	// ConnectTimeout (SPEC 4) нельзя повесить на клиент (его создаёт socks,
	// CODING 1.4) и нельзя вшивать в ctx стрима: таймер-канцеляция вокруг
	// client.Do, после заголовков таймер остановлен, ctx живёт до конца стрима.
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
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
		// F4: выстреливший connect-таймер (в том числе во время синхронного
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
	defer resp.Body.Close()
	if connectFired.Load() {
		// Редкая гонка: заголовки пришли, но таймер успел выстрелить — тело
		// мертво (ctx уже отменён), использовать ответ нельзя.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return &upstream.SendResult{
			ErrorKind:    upstream.KindNetwork,
			Stage:        upstream.StageUpstream,
			ClientStatus: http.StatusBadGateway,
			Message:      fmt.Sprintf("upstream_connect_timeout(%v) истёк одновременно с получением заголовков", r.ConnectTimeout),
			Dump:         dump,
		}, nil
	}

	result := consumeResponse(r, resp, reqCtx, cancelReq, dump)
	result.UpstreamStatus = resp.StatusCode
	result.UpstreamHeader = resp.Header.Clone()
	if result.Dump == nil {
		result.Dump = dump
	}
	return result, nil
}

// consumeResponse — ветка после получения заголовков: не-2xx (ошибка),
// non-stream (JSON) или стрим. Ответ клиенту пишется здесь.
func consumeResponse(r *upstream.SendRequest, resp *http.Response, reqCtx context.Context,
	cancelReq context.CancelFunc, dump *upstream.Dump) *upstream.SendResult {

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return upstreamErrorResult(r, resp, dump)
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")
	if r.Stream && isSSE {
		return streamToClient(r, resp, reqCtx, cancelReq, dump)
	}
	// Стрим просили, но апстрим вернул JSON — обрабатываем как non-stream:
	// выдавать JSON за SSE-поток нельзя.
	return nonStreamToClient(r, resp, dump)
}

// upstreamErrorResult — не-2xx апстрима (ARCHITECTURE §10): клиенту статус
// апстрима + проброс retry-after/x-ratelimit-* (делает сервер), в errors —
// полный запрос+ответ. Клиенту ничего не пишем: конверт протокола входа
// пишет сервер.
func upstreamErrorResult(r *upstream.SendRequest, resp *http.Response, dump *upstream.Dump) *upstream.SendResult {
	body, _, err := readCapped(resp.Body, maxResponseDump)
	if err != nil {
		// Обрыв уже при чтении тела ОШИБКИ — тот же сетевой сбой пути (FC-9).
		reportNetworkError(r.Transporter)
		body = []byte("<ошибка чтения тела ошибки апстрима: " + err.Error() + ">")
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = body
	return &upstream.SendResult{
		ErrorKind:    upstream.KindUpstream,
		Stage:        upstream.StageUpstream,
		ClientStatus: resp.StatusCode, // SPEC 4: клиенту — статус апстрима
		Message: fmt.Sprintf("апстрим ответил %d: %s", resp.StatusCode,
			errPreview(body, maxErrPreview)),
		Dump: dump,
	}
}

// nonStreamToClient — 2xx без SSE: тело уходит клиенту КАК ЕСТЬ (обе клетки
// glm — passthrough), usage — из того же тела единой нормализацией translate
// (SPEC 8.6 «на non-stream — из JSON-тела»).
func nonStreamToClient(r *upstream.SendRequest, resp *http.Response, dump *upstream.Dump) *upstream.SendResult {
	body, truncated, err := readCapped(resp.Body, maxSuccessBody)
	if err != nil {
		// FC-9 не только для стримов: обрыв non-stream тела — тоже сетевой
		// сбой пути, иначе путь остаётся fresh со сломанным транспортом.
		reportNetworkError(r.Transporter)
		return networkResult("чтение тела апстрима оборвалось: " + err.Error())
	}
	if truncated {
		return upstreamResult(fmt.Sprintf("non-stream ответ апстрима превышает %d байт", maxSuccessBody))
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = capBytes(body, maxResponseDump)

	// SPEC 8.7 (non-stream): responses-вход — не passthrough: chat-тело
	// конвертируется в объект Responses API; usage — из того же тела единой
	// нормализацией (TR-3). Битый JSON — 2xx не в chat-формате: upstream-ошибка.
	if r.Protocol == upstream.ProtocolResponses {
		var chat translate.ChatResponse
		if err := json.Unmarshal(body, &chat); err != nil {
			return upstreamResult("апстрим вернул 2xx не в формате chat completions: " + err.Error())
		}
		b, err := json.Marshal(translate.ChatToResponses(&chat, clientAlias(r.Body)))
		if err != nil {
			// Marshal структур под нашим контролем не падает; классификация
			// translate — защита, а не штатная ветка.
			return translateResult(fmt.Errorf("маршалинг responses-ответа: %w", err))
		}
		r.W.Header().Set("Content-Type", "application/json")
		r.W.WriteHeader(http.StatusOK)
		if _, err := r.W.Write(b); err != nil {
			return clientGoneResult("клиент отключился при записи ответа: " + err.Error())
		}
		// Flush сразу после тела (AC-4): ответ уходит клиенту до SQLite-вставок
		// сервера — как у passthrough-веток ниже.
		flushWriter(r.W)
		return okResult(translate.UsageFromChat(chat.Usage))
	}

	var usage translate.Usage
	switch r.Protocol {
	case upstream.ProtocolOpenAI:
		// Разбор тела ТОЛЬКО ради usage (TR-13): битый JSON не мешает
		// passthrough — клиент получает ответ апстрима как есть, учёт —
		// с нулевым usage (2xx — не ошибка).
		var chat translate.ChatResponse
		_ = json.Unmarshal(body, &chat)
		usage = translate.UsageFromChat(chat.Usage)
	case upstream.ProtocolAnthropic:
		var msg struct {
			Usage *translate.AnthropicUsage `json:"usage"`
		}
		_ = json.Unmarshal(body, &msg)
		usage = translate.UsageFromAnthropic(msg.Usage)
	default:
		return translateResult(fmt.Errorf("неизвестный протокол входа %q", r.Protocol))
	}

	// Passthrough: тело байт-в-байт (значения не перекодируем).
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	r.W.Header().Set("Content-Type", ct)
	r.W.WriteHeader(http.StatusOK)
	if _, err := r.W.Write(body); err != nil {
		return clientGoneResult("клиент отключился при записи ответа: " + err.Error())
	}
	// Flush сразу после тела (как у конвертов ошибок сервера): успешный ответ
	// уходит клиенту ДО SQLite-вставок сервера (AC-4), а не после возврата
	// хендлера.
	flushWriter(r.W)
	return okResult(usage)
}

// streamToClient — 2xx SSE: события апстрима уходят клиенту КАК ЕСТЬ
// (passthrough openai/anthropic) либо конвертируются (responses), параллельно
// tee-usage: openai — usage-чанк (chatUsageOf), anthropic —
// message_start/message_delta (AnthropicStreamUsage), responses — usage-чанк
// внутри ChatToResponsesStream (единый источник для БД, TR-3). Keepalive
// ': ping' и idle-таймаут — SPEC 4.
func streamToClient(r *upstream.SendRequest, resp *http.Response, reqCtx context.Context,
	cancelReq context.CancelFunc, dump *upstream.Dump) *upstream.SendResult {

	h := r.W.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	anthropic := r.Protocol == upstream.ProtocolAnthropic

	// responses-клетка — конвертация chat → Responses (SPEC 8.7): модель —
	// алиас из запроса клиента, не модель апстрима.
	var respSt *translate.ChatToResponsesStream
	if r.Protocol == upstream.ProtocolResponses {
		respSt = translate.NewChatToResponses(r.W)
		if alias := clientAlias(r.Body); alias != "" {
			respSt.SetModel(alias)
		}
	}

	var (
		usage        translate.Usage
		anthUsage    translate.AnthropicStreamUsage
		rawSeen      bytes.Buffer // «что успели» от апстрима — для дампа (SPEC 10)
		streamDone   bool         // клиентская сторона корректно терминирована
		upstreamErr  string       // апстрим прислал ошибку событием
		upstreamCode string
	)

	onEvent := func(ev upstream.SSEEvent) error {
		capBuffer(&rawSeen, ev.Data, maxResponseDump)
		if msg, code, isErr := upstreamErrorOf(ev); isErr {
			// Ошибка пришла СОБЫТИЕМ уже после 200: клиенту — только SSE
			// (SPEC 4), классификация upstream (не сеть). Passthrough: уходим
			// как есть (anthropic-форма и так anthropic, openai — openai);
			// сканер дальше не пойдёт.
			if msg == "" {
				// Пустое сообщение не должно маскировать ошибку в классификации.
				msg = "ошибка апстрима"
			}
			upstreamErr, upstreamCode = msg, code
			if respSt != nil {
				// SPEC 8.7: ошибка — событие error (пишет Fail), стрим
				// завершается БЕЗ response.completed.
				respSt.Fail(msg, code)
				return upstream.ErrStop
			}
			if err := writeSSEEvent(r.W, ev.Event, ev.Data); err != nil {
				return err
			}
			if !anthropic {
				// TR-4: openai-выход обязан финишировать [DONE] и после ошибки.
				if err := writeSSEEvent(r.W, "", []byte("[DONE]")); err != nil {
					return err
				}
			}
			streamDone = true
			return upstream.ErrStop
		}
		switch r.Protocol {
		case upstream.ProtocolOpenAI:
			// Tee-usage passthrough: usage-чанк (choices:[], поле usage).
			if u, ok := chatUsageOf(ev.Data); ok {
				usage = u
			}
			if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
				streamDone = true
			}
			return writeSSEEvent(r.W, ev.Event, ev.Data)
		case upstream.ProtocolAnthropic:
			// Tee-usage кеш-полей (cache_read/cache_creation) — SPEC 8.6.
			anthUsage.Feed(ev.Event, ev.Data)
			if ev.Event == "message_stop" {
				streamDone = true
			}
			return writeSSEEvent(r.W, ev.Event, ev.Data)
		case upstream.ProtocolResponses:
			// Конвертация chat-чанков в события Responses: [DONE] внутри Feed
			// закрывает стрим (Finish идемпотентен), usage-чанк копится в tee.
			return respSt.Feed(ev.Event, ev.Data)
		default:
			return upstream.ErrStop
		}
	}
	onIdle := func() {
		// Idle-таймаут — обрыв с SSE-ошибкой в протоколе входа (SPEC 4).
		msg := onIdleMessage(r.IdleTimeout)
		if respSt != nil {
			// SPEC 8.7: event: error {"type":"error","code","message"}.
			respSt.Fail(msg, "idle_timeout")
			return
		}
		if anthropic {
			writeAnthropicErrorEvent(r.W, msg, "idle_timeout")
			return
		}
		writeOpenAIErrorEvent(r.W, msg, "idle_timeout")
	}

	pr := upstream.PumpStream(reqCtx, resp.Body, cancelReq, r.Keepalive, r.IdleTimeout, writePing(r.W), onEvent, onIdle)

	// Tee-usage: у openai-клетки копился в замыкании, у anthropic — в
	// коллекторе, у responses — внутри транслятора (один источник для БД).
	finalUsage := usage
	if respSt != nil {
		finalUsage = respSt.Usage()
	} else if anthropic {
		finalUsage = anthUsage.Usage()
	}
	res := &upstream.SendResult{
		Usage:    finalUsage,
		Streamed: true, // заголовки SSE уже ушли клиенту: HTTP-конверт невозможен
		Dump:     dump,
	}
	dump.RespHeaders = resp.Header.Clone()
	dump.RespBody = capBytes(rawSeen.Bytes(), maxResponseDump)

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
		// Мы оборвали стрим сами — обязаны терминировать его событием ошибки
		// в формате выхода (SPEC 4), иначе клиент ждёт финала до таймаута.
		if respSt != nil {
			respSt.Fail(res.Message, "stream_interrupted")
		} else if anthropic {
			writeAnthropicErrorEvent(r.W, res.Message, "stream_interrupted")
		} else {
			writeOpenAIErrorEvent(r.W, res.Message, "stream_interrupted")
		}
	case upstreamErr != "":
		res.ErrorKind, res.Stage = upstream.KindUpstream, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = fmt.Sprintf("апстрим прислал ошибку посреди стрима (code %q): %s", upstreamCode, upstreamErr)
	default:
		// EOF апстрима. openai-клетка: TR-4 — [DONE] обязателен, даже если
		// апстрим оборвался без него. anthropic-клетка: финал НЕ синтезируем —
		// passthrough не знает, какие блоки остались открытыми, чужой
		// message_stop после наших блоков сломал бы порядок; z.ai сам
		// терминирует стрим message_stop (streamDone — только для классификации).
		// responses-клетка: конвертация обязана завершиться финальным событием
		// (SPEC 8.7: Finish идемпотентен — [DONE] апстрима уже закрыл стрим).
		if respSt != nil {
			if err := respSt.Finish(); err != nil {
				res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
				res.ClientStatus = http.StatusOK
				res.Message = "запись финала responses-стрима: " + err.Error()
			}
		} else if !anthropic && !streamDone {
			if err := writeSSEEvent(r.W, "", []byte("[DONE]")); err != nil {
				res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
				res.ClientStatus = http.StatusOK
				res.Message = "запись [DONE]: " + err.Error()
			}
		}
	}
	return res
}

// okResult — успешный ответ (ответ клиенту уже написан).
func okResult(u translate.Usage) *upstream.SendResult {
	return &upstream.SendResult{Usage: u}
}

// networkResult — сетевая неудача до записи клиенту (502).
func networkResult(msg string) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindNetwork,
		Stage:        upstream.StageUpstream,
		ClientStatus: http.StatusBadGateway,
		Message:      msg,
	}
}

// upstreamResult — апстрим повёл себя не по протоколу (502, kind upstream).
func upstreamResult(msg string) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindUpstream,
		Stage:        upstream.StageUpstream,
		ClientStatus: http.StatusBadGateway,
		Message:      msg,
	}
}

// clientGoneResult — клиент оборвался при записи финала: апстрим отработал,
// но ответ доставлен не был; usage уже посчитан.
func clientGoneResult(msg string) *upstream.SendResult {
	return &upstream.SendResult{
		ErrorKind:    upstream.KindNetwork,
		Stage:        upstream.StageStream,
		ClientStatus: http.StatusOK,
		Message:      msg,
		Streamed:     true,
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

// capBuffer — дозапись в дамп-буфер с капом: «что успели» ограничено капом
// errorslog, держать больше в памяти незачем.
func capBuffer(buf *bytes.Buffer, chunk []byte, max int) {
	if buf.Len() >= max {
		return
	}
	buf.Write(chunk)
	if buf.Len() > max {
		buf.Truncate(max)
	}
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

// flushWriter — Flush после записи финального тела: без него net/http
// придержит байты до возврата хендлера, а тот стоит в SQLite-вставках сервера.
func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// chatUsageOf — usage из чанка/тела chat (usage-чанк стрима: choices:[], поле
// usage). Единая нормализация — translate (TR-3).
func chatUsageOf(data []byte) (translate.Usage, bool) {
	var probe struct {
		Usage *translate.ChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Usage == nil {
		return translate.Usage{}, false
	}
	return translate.UsageFromChat(probe.Usage), true
}

// upstreamErrorOf распознаёт ошибку, пришедшую СОБЫТИЕМ: объект {"error":{...}}
// в данных (openai-форма и anthropic-форма {"type":"error","error":{...}} — у
// обеих верхний ключ error) либо event:error без разбираемых данных. Сначала
// данные: у event:error с телом сообщение должно доехать до клиента и errors.
func upstreamErrorOf(ev upstream.SSEEvent) (msg, code string, ok bool) {
	var probe struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &probe); err == nil && probe.Error != nil {
		code = probe.Error.Type
		if len(probe.Error.Code) > 0 {
			code = strings.Trim(string(probe.Error.Code), `"`)
		}
		return probe.Error.Message, code, true
	}
	if ev.Event == "error" {
		// Ошибка и без тела данных — факт ошибки обязан дожить до классификации.
		return "ошибка апстрима", "upstream_error", true
	}
	return "", "", false
}

// writeSSEEvent фреймит событие для клиента: event:-строка (если есть) +
// data-строки (многострочный data разворачивается по SSE-спеке) + Flush.
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

// writePing возвращает замыкание пинга для pump: SSE-комментарий keepalive
// (SPEC 4) — opencode-парсер комментарии игнорирует, но его chunk-timeout
// не срабатывает.
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
	// Ошибки записи не вернуть: клиент уже получил 200; исход виден в
	// результате (Streamed) и в errorslog.
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
