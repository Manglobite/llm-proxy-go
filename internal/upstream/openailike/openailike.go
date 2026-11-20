// Пакет upstream/openailike — модуль провайдера «openai-like» (SPEC 7.3):
// generic OpenAI-совместимые апстримы (ollama cloud/local, любые /v1/chat/
// completions). Реализует интерфейс server.Sender (объявлен потребителем).
//
// Покрытые клетки матрицы SPEC 8.2:
//   - openai-вход    → openai chat апстрим: passthrough + подмена модели +
//     инжект include_usage (стримы); ответ — как есть (non-stream JSON /
//     SSE-события как есть) + tee-usage;
//   - anthropic-вход → openai chat апстрим: ParseAnthropic → BuildChat;
//     ответ — ChatToAnthropic / ChatToAnthropicStream;
//   - responses-вход → openai chat апстрим: ParseResponses → BuildChat
//     (SPEC 8.3); ответ — ChatToResponses / ChatToResponsesStream (SPEC 8.7).
//
// R1/R3: http.Client берётся только из socks.Transporter аккаунта, своих
// клиентов пакет не создаёт; про другие провайдеры не знает ничего.
package openailike

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

// maxResponseDump — кап тела не-2xx ответа и «что успели» стрима для дампа
// errorslog (совпадает с капом InsertError: больше в БД всё равно не попадёт).
const maxResponseDump = 1 << 20

// maxSuccessBody — кап тела УСПЕШНОГО non-stream ответа: читаем целиком ради
// passthrough и tee-usage; тела больше считаем ошибкой апстрима, а не молча —
// клиенту иначе ушёл бы урезанный JSON.
const maxSuccessBody = 32 << 20

// maxErrPreview — сколько байт тела не-2xx ответа попадает в текст ошибки
// (сообщение уходит клиенту и в errors; целиком тело лежит в дампе).
const maxErrPreview = 512

// Sender — модуль провайдера openai-like. Stateless: всё состояние запроса
// приходит в SendRequest (интерфейс server.Sender объявлен потребителем).
type Sender struct{}

// reportable — узкий интерфейс пути SOCKS для репорта сетевого обрыва стрима
// (FC-9). Опциональный: gateTripper обрывает транспортные ошибки до возврата
// RoundTrip, а чтение ТЕЛА происходит уже в провайдере — путь должен узнать
// об обрыве отсюда. socks.Direct умеет это из коробки (no-op).
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
func findAccount(accounts []config.OpenAILikeAccount, id string) *config.OpenAILikeAccount {
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	return nil
}

// buildUpstreamBody — сборка тела апстрима по клетке матрицы SPEC 8.2.
// openai-вход: passthrough (RewriteModel; для стримов InjectIncludeUsage —
// ТОЛЬКО клетка chat→chat; в anthropic-passthrough glm её не зовут).
// anthropic-вход: ParseAnthropic → BuildChat (BuildChat сам ставит
// stream_options.include_usage для стрима). responses-вход: ParseResponses →
// BuildChat — конвертация roundtrip'ом нормформы (SPEC 8.3).
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
		norm, err := translate.ParseAnthropic(r.Body)
		if err != nil {
			return nil, err // TR-7: max_tokens и прочие требования anthropic
		}
		return json.Marshal(translate.BuildChat(norm, r.Target.Model, r.Stream))
	case upstream.ProtocolResponses:
		norm, err := translate.ParseResponses(r.Body)
		if err != nil {
			return nil, err // previous_response_id и прочие требования SPEC 4/8.3
		}
		return json.Marshal(translate.BuildChat(norm, r.Target.Model, r.Stream))
	default:
		return nil, fmt.Errorf("неизвестный протокол входа %q", r.Protocol)
	}
}

// clientAlias — имя модели из ЗАПРОСА КЛИЕНТА (поле model входа /v1/responses):
// клиент ждёт в response.created/completed алиас, который сам и просил
// (SPEC 8.7), а не модель апстрима, в которую запрос переписал роутер. Пустая
// строка допустима: транслятор тогда подставит model чанка/ответа апстрима
// (поведение SetModel-менее — защита на случай неожиданной сборки).
func clientAlias(body []byte) string {
	var head struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &head)
	return head.Model
}

// translateResult — 400 translate: конвертация входа не удалась (тело клиента
// не соответствует протоколу). Дампа апстрима нет — сервер приложит клиентский.
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

// Send — реализация upstream.Sender. Контракт — в godoc интерфейса
// (upstream.Sender): классифицированные неудачи возвращаются РЕЗУЛЬТАТОМ,
// (nil, error) — только нарушение контракта вызова.
func (Sender) Send(ctx context.Context, r *upstream.SendRequest) (*upstream.SendResult, error) {
	if r.Target.ProviderType != config.ProviderOpenAILike {
		return nil, fmt.Errorf("openailike: тип провайдера %q — не %s", r.Target.ProviderType, config.ProviderOpenAILike)
	}
	prov, ok := r.Snap.Cfg.Providers[r.Target.ProviderName]
	if !ok || prov.OpenAILike == nil {
		return nil, fmt.Errorf("openailike: провайдер %q не найден в снапшоте", r.Target.ProviderName)
	}
	acc := findAccount(prov.OpenAILike.Accounts, r.Target.AccountID)
	if acc == nil {
		return nil, fmt.Errorf("openailike: аккаунт %q не найден у провайдера %q", r.Target.AccountID, r.Target.ProviderName)
	}
	if r.Transporter == nil {
		return nil, errors.New("openailike: transporter аккаунта не передан")
	}
	if r.W == nil {
		return nil, errors.New("openailike: ResponseWriter клиента не передан")
	}

	body, err := buildUpstreamBody(r)
	if err != nil {
		// Ошибки конвертации входа (ParseAnthropic: max_tokens, роли, блоки) —
		// translate-ошибка: 400 в конверте протокола входа.
		return translateResult(err), nil
	}

	endpoint := strings.TrimRight(prov.OpenAILike.BaseURL, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openailike: запрос не собирается: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// SPEC 7.3/OA-10: только Authorization: Bearer — x-api-key апстримы
	// семейства не принимают.
	httpReq.Header.Set("Authorization", "Bearer "+acc.Auth.Key)
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
	// CODING 1.4) и нельзя вшивать в ctx стрима — лимит действует только ДО
	// заголовков. Поэтому таймер-канцеляция вокруг client.Do: после заголовков
	// таймер остановлен, ctx живёт до конца стрима (idle-таймаут, клиент).
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
		// F4: если выстрелил connect-таймер (в том числе во время синхронного
		// гейт-чека — gate возвращает «check aborted» ошибкой *BlockedError),
		// это сетевой таймаут клиента, а не блокировка пути: 502 network.
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
// non-stream (JSON) или стрим. Ответ клиенту пишется здесь; результат несёт
// классификацию для сервера.
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
	// выдавать JSON за SSE-поток нельзя, а конвертация применима к обоим формам.
	return nonStreamToClient(r, resp, dump)
}

// upstreamErrorResult — не-2xx апстрима (таксономия ARCHITECTURE §10):
// клиенту статус апстрима + проброс retry-after/x-ratelimit-* (делает сервер),
// в errors — полный запрос+ответ. Клиенту ничего не пишем: конверт — работа
// сервера (протокол входа знает только он).
func upstreamErrorResult(r *upstream.SendRequest, resp *http.Response, dump *upstream.Dump) *upstream.SendResult {
	body, _, err := readCapped(resp.Body, maxResponseDump)
	if err != nil {
		// Обрыв уже при чтении тела ОШИБКИ — тот же сетевой сбой пути (FC-9):
		// путь помечается требующим проверки, как при обрыве стрима.
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

// nonStreamToClient — 2xx без SSE: тело читается целиком, usage — из тела
// (SPEC 8.6 «на non-stream — из JSON-тела»), тело уходит клиенту passthrough
// (openai-вход) или конвертируется (anthropic-вход).
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
	// нормализацией (TR-3), модель — алиас клиента.
	if r.Protocol == upstream.ProtocolResponses {
		var chat translate.ChatResponse
		if err := json.Unmarshal(body, &chat); err != nil {
			// Конвертация невозможна: апстрим вернул 2xx не в chat-формате.
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
		// N1/AC-4: как в остальных ветках — тело клиенту до SQLite-вставок.
		flushClient(r.W)
		return okResult(translate.UsageFromChat(chat.Usage))
	}

	var chat translate.ChatResponse
	unmarshalErr := json.Unmarshal(body, &chat)

	switch r.Protocol {
	case upstream.ProtocolOpenAI:
		// Passthrough: тело байт-в-байт (значения не перекодируем), usage —
		// из того же тела единой нормализацией (TR-3). Битый JSON не мешает
		// passthrough: клиент получает ответ апстрима как есть, учёт —
		// с нулевым usage (2xx — не ошибка).
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		r.W.Header().Set("Content-Type", ct)
		r.W.WriteHeader(http.StatusOK)
		if _, err := r.W.Write(body); err != nil {
			return clientGoneResult("клиент отключился при записи ответа: " + err.Error())
		}
		// N1/AC-4: Flush сразу после тела — иначе net/http придержит его до
		// возврата хендлера, а тот стоит в SQLite-вставках сервера (запись
		// usage может ждать busy_timeout); клиент должен получить ответ раньше.
		flushClient(r.W)
		return okResult(translate.UsageFromChat(chat.Usage))

	case upstream.ProtocolAnthropic:
		if unmarshalErr != nil {
			// Конвертация невозможна: апстрим вернул 2xx не в chat-формате.
			return upstreamResult("апстрим вернул 2xx не в формате chat completions: " + unmarshalErr.Error())
		}
		out := translate.ChatToAnthropic(&chat)
		b, err := json.Marshal(out)
		if err != nil {
			// Marshal структур под нашим контролем не падает; на всякий случай
			// классифицируем как translate (сервер вернёт 400).
			return translateResult(fmt.Errorf("маршалинг anthropic-ответа: %w", err))
		}
		r.W.Header().Set("Content-Type", "application/json")
		r.W.WriteHeader(http.StatusOK)
		if _, err := r.W.Write(b); err != nil {
			return clientGoneResult("клиент отключился при записи ответа: " + err.Error())
		}
		// N1/AC-4: как в openai-ветке — тело клиенту до SQLite-вставок.
		flushClient(r.W)
		return okResult(translate.UsageFromChat(chat.Usage))

	default:
		return translateResult(fmt.Errorf("неизвестный протокол входа %q", r.Protocol))
	}
}

// streamToClient — 2xx SSE: прокачка событий апстрима клиенту (passthrough
// или конвертация) с keepalive ': ping' и idle-таймаутом (SPEC 4). Заголовки
// SSE выставляются ДО первой записи: первый ping может уйти раньше события.
func streamToClient(r *upstream.SendRequest, resp *http.Response, reqCtx context.Context,
	cancelReq context.CancelFunc, dump *upstream.Dump) *upstream.SendResult {

	h := r.W.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	var (
		usage        translate.Usage
		rawSeen      bytes.Buffer // «что успели» от апстрима — для дампа (SPEC 10)
		streamDone   bool         // клиентская сторона корректно терминирована ([DONE])
		upstreamErr  string       // апстрим прислал error-событие / {"error":...}
		upstreamCode string
	)

	var anthropic *translate.ChatToAnthropicStream
	if r.Protocol == upstream.ProtocolAnthropic {
		anthropic = translate.NewChatToAnthropic(r.W)
	}

	// responses-ветка — конвертация chat → Responses (SPEC 8.7): модель —
	// алиас из запроса клиента, не модель апстрима.
	var respSt *translate.ChatToResponsesStream
	if r.Protocol == upstream.ProtocolResponses {
		respSt = translate.NewChatToResponses(r.W)
		if alias := clientAlias(r.Body); alias != "" {
			respSt.SetModel(alias)
		}
	}

	onEvent := func(ev upstream.SSEEvent) error {
		capBuffer(&rawSeen, ev.Data, maxResponseDump)
		if msg, code, isErr := upstreamErrorOf(ev); isErr {
			// Ошибка пришла СОБЫТИЕМ уже после 200: клиенту — только SSE
			// (SPEC 4), классификация upstream (не сеть).
			upstreamErr, upstreamCode = msg, code
			if anthropic != nil {
				anthropic.Fail(msg, code)
				return upstream.ErrStop // Fail уже написал error-событие
			}
			if respSt != nil {
				// SPEC 8.7: событие error (пишет Fail), БЕЗ response.completed.
				respSt.Fail(msg, code)
				return upstream.ErrStop
			}
			// openai-passthrough: событие ошибки уходит клиенту КАК ЕСТЬ,
			// финал [DONE] дописываем сами (TR-4) — сканер дальше не пойдёт.
			if err := writeSSEEvent(r.W, ev.Event, ev.Data); err != nil {
				return err
			}
			if err := writeSSEEvent(r.W, "", []byte("[DONE]")); err != nil {
				return err
			}
			streamDone = true
			return upstream.ErrStop
		}
		switch r.Protocol {
		case upstream.ProtocolOpenAI:
			// Tee-usage passthrough: usage-чанк (choices:[], поле usage)
			// копится той же нормализацией (TR-3), событие уходит как есть.
			if u, ok := chatUsageOf(ev.Data); ok {
				usage = u
			}
			if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
				streamDone = true
			}
			return writeSSEEvent(r.W, ev.Event, ev.Data)
		case upstream.ProtocolAnthropic:
			// [DONE] внутри Feed закрывает стрим (Finish) — идемпотентно.
			return anthropic.Feed(ev.Event, ev.Data)
		case upstream.ProtocolResponses:
			// Конвертация chat-чанков в события Responses; [DONE] внутри Feed
			// закрывает стрим (Finish), usage-чанк копится в tee.
			return respSt.Feed(ev.Event, ev.Data)
		default:
			return upstream.ErrStop
		}
	}
	onIdle := func() {
		// Idle-таймаут — обрыв с SSE-ошибкой в протоколе входа (SPEC 4);
		// ClientGone сюда не попадает: клиент ушёл — писать некому.
		msg := onIdleMessage(r.IdleTimeout)
		if anthropic != nil {
			anthropic.Fail(msg, "idle_timeout")
			return
		}
		if respSt != nil {
			// SPEC 8.7: event: error {"type":"error","code","message"}.
			respSt.Fail(msg, "idle_timeout")
			return
		}
		writeOpenAIErrorEvent(r.W, msg, "idle_timeout")
	}

	pr := upstream.PumpStream(reqCtx, resp.Body, cancelReq, r.Keepalive, r.IdleTimeout, writePing(r.W), onEvent, onIdle)

	// Tee-usage: у passthrough он копился в замыкании, у конвертации — внутри
	// транслятора (Feed копит usage-чанк независимо от эмиссии).
	finalUsage := usage
	if anthropic != nil {
		finalUsage = anthropic.Usage()
	} else if respSt != nil {
		finalUsage = respSt.Usage()
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
		if anthropic != nil {
			anthropic.Fail(res.Message, "stream_interrupted")
		} else if respSt != nil {
			respSt.Fail(res.Message, "stream_interrupted")
		} else {
			writeOpenAIErrorEvent(r.W, res.Message, "stream_interrupted")
		}
	case upstreamErr != "":
		res.ErrorKind, res.Stage = upstream.KindUpstream, upstream.StageStream
		res.ClientStatus = http.StatusOK
		res.Message = fmt.Sprintf("апстрим прислал ошибку посреди стрима (code %q): %s", upstreamCode, upstreamErr)
	default:
		// Чтение апстрима закончилось штатно (EOF).
		if anthropic != nil {
			// [DONE] мог не дойти: Finish обязан закрыть anthropic-стрим
			// всегда (TR-4, идемпотентно).
			if err := anthropic.Finish(); err != nil {
				res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
				res.ClientStatus = http.StatusOK
				res.Message = "запись финала anthropic-стрима: " + err.Error()
			}
		} else if respSt != nil {
			// SPEC 8.7: responses-стрим обязан завершиться финальным событием
			// (Finish идемпотентен — [DONE] апстрима уже закрыл стрим).
			if err := respSt.Finish(); err != nil {
				res.ErrorKind, res.Stage = upstream.KindNetwork, upstream.StageStream
				res.ClientStatus = http.StatusOK
				res.Message = "запись финала responses-стрима: " + err.Error()
			}
		} else if !streamDone {
			// TR-4: openai-выход обязан завершиться [DONE], даже если апстрим
			// оборвался без него.
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

// flushClient — Flush ответа клиенту (N1/AC-4): байты уходят в сеть сразу,
// а не вместе с возвратом хендлера из SQLite-вставок сервера.
func flushClient(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
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
// но ответ доставлен не был; usage уже посчитан, SSE/конверт писать поздно.
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

// chatUsageOf — usage из чанка/тела chat (usage-чанк стрима: choices:[], поле
// usage; non-stream: то же поле тела). Единая нормализация — translate (TR-3).
func chatUsageOf(data []byte) (translate.Usage, bool) {
	var probe struct {
		Usage *translate.ChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Usage == nil {
		return translate.Usage{}, false
	}
	return translate.UsageFromChat(probe.Usage), true
}

// upstreamErrorOf распознаёт ошибку, пришедшую СОБЫТИЕМ: event:error или
// объект {"error":{...}} в данных (формы error-событий SPEC 8.4 и открытых
// апстримов). [DONE] и обычные чанки ошибками не считаются.
func upstreamErrorOf(ev upstream.SSEEvent) (msg, code string, ok bool) {
	if ev.Event == "error" {
		return "", "upstream_error", true
	}
	var probe struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &probe); err != nil || probe.Error == nil {
		return "", "", false
	}
	code = probe.Error.Type
	if len(probe.Error.Code) > 0 {
		code = strings.Trim(string(probe.Error.Code), `"`)
	}
	return probe.Error.Message, code, true
}

// writeSSEEvent фреймит событие для клиента: event:-строка (если есть) +
// data-строки. ScanSSE склеивает многострочный data через \n — при обратной
// сборке разворачиваем в несколько data:-строк (SSE-спека), иначе клиентский
// парсер увидит \n внутри data.
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
	// Flush после каждого события (ARCHITECTURE §5): без него клиент видит
	// данные пачками или не видит вовсе.
	flushClient(w)
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
		flushClient(w)
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
