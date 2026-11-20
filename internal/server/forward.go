// Прозрачный форвард forward_paths (SPEC 3.1/4/7.3, SV-4/OA-9): запросы по
// префиксам из конфига openai-like провайдера (например /api/tags, /api/show
// — нативное discovery opencode-провайдера Ollama) уходят апстриму КАК ЕСТЬ
// (метод, путь+query, тело) через SOCKS аккаунта, с его Authorization.
//
// Это НЕ запрос модели: строка в requests не пишется (SPEC 9 требует model),
// ошибка форварда — дамп в errors (stage=upstream) и статус клиенту.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// maxForwardBody — кап тела ОТВЕТА форварда. Discovery-ответы ollama
// (список моделей, манифесты) — единицы МБ; 16 МБ с запасом, но не даём
// апстриму бесконечно качать память прокси. Решение задокументировано в
// отчёте Ф2b (стриминга у форварда нет — тело читается целиком).
const maxForwardBody = 16 << 20

// hopByHopHeaders — заголовки, непереносимые через прокси (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authentication",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// forwardMatch — куда и с каким аккаунтом форвардить.
type forwardMatch struct {
	providerName string
	prov         *config.OpenAILikeProvider
}

// matchForwardPath — провайдер, чей forward_paths-префикс покрывает путь.
// Провайдеры перебираются по ОТСОРТИРОВАННЫМ именам: при пересечении
// префиксов разных провайдеров выбор детерминирован (иначе один и тот же
// запрос ходил бы к разным апстримам между перезапусками).
func matchForwardPath(snap *config.Snapshot, path string) (forwardMatch, bool) {
	names := make([]string, 0, len(snap.Cfg.Providers))
	for name := range snap.Cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := snap.Cfg.Providers[name]
		if p.OpenAILike == nil {
			continue // forward_paths — только у openai-like (SPEC 3.1)
		}
		for _, prefix := range p.OpenAILike.ForwardPaths {
			if prefix != "" && strings.HasPrefix(path, prefix) {
				return forwardMatch{providerName: name, prov: p.OpenAILike}, true
			}
		}
	}
	return forwardMatch{}, false
}

// handleForwardAPI — записи "/api/": либо прозрачный форвард, либо тот же
// JSON 404, что у catch-all (SPEC 4: ошибки — всегда JSON).
func (s *Server) handleForwardAPI(w http.ResponseWriter, r *http.Request) {
	snap := s.params.Snapshot()
	if snap == nil || snap.Cfg == nil {
		writeError(w, upstream.ProtocolOpenAI, http.StatusServiceUnavailable, "internal_error", "конфигурация недоступна", nil)
		return
	}
	match, ok := matchForwardPath(snap, r.URL.Path)
	if !ok {
		// Ни один провайдер не объявил такой префикс (или префиксов нет
		// вовсе): неотличимо от «маршрут не зарегистрирован» (SV-4).
		s.notFound(w, r)
		return
	}

	// Auth — тот же client key, что и на /v1/* (SV-4).
	clientKey, ok := s.authenticate(r, snap)
	if !ok {
		writeError(w, upstream.ProtocolOpenAI, http.StatusUnauthorized, "invalid_api_key",
			"неизвестный client key: передайте именованный ключ в Authorization: Bearer или x-api-key", nil)
		return
	}

	// Аккаунт-источник — ПЕРВЫЙ аккаунт провайдера (детерминизм: forward_paths
	// — discovery без модели, роутить не по чему; фиксируем порядок конфига).
	if len(match.prov.Accounts) == 0 {
		writeError(w, upstream.ProtocolOpenAI, http.StatusBadGateway, "internal_error",
			fmt.Sprintf("у провайдера %q нет аккаунтов для форварда", match.providerName), nil)
		return
	}
	acc := &match.prov.Accounts[0]

	tr, err := transporterFor(snap, acc.Socks)
	if err != nil {
		writeError(w, upstream.ProtocolOpenAI, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}

	s.forward(w, r, snap, clientKey, match.providerName, acc.ID, tr, strings.TrimRight(match.prov.BaseURL, "/"), acc.Auth.Key)
}

// forward — собственно прозрачный прогон одного запроса.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, snap *config.Snapshot,
	clientKey, providerName, accountID string, tr socks.Transporter, base, apiKey string) {

	dump := &upstream.Dump{Method: r.Method}

	// Тело запроса — как есть (включая Content-Encoding клиента: декодировать
	// и перекодировать незачем, апстрим умеет то, что умеет клиент). Кап тот
	// же, что у /v1/*: сырой размер ограничен MaxBodyBytes.
	body, err := io.ReadAll(io.LimitReader(r.Body, s.params.MaxBodyBytes+1))
	if err != nil {
		writeError(w, upstream.ProtocolOpenAI, http.StatusBadRequest, "invalid_request",
			"не удалось прочитать тело запроса: "+err.Error(), nil)
		return
	}
	if int64(len(body)) > s.params.MaxBodyBytes {
		writeError(w, upstream.ProtocolOpenAI, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("тело запроса превышает %d байт", s.params.MaxBodyBytes), nil)
		return
	}

	target := base + r.URL.EscapedPath() // путь+query байт-в-байт (SPEC 7.3)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, upstream.ProtocolOpenAI, http.StatusInternalServerError, "internal_error",
			"запрос форварда не собирается: "+err.Error(), nil)
		return
	}
	upReq.Header = forwardHeaders(r.Header, apiKey)
	dump.URL = target
	dump.ReqHeaders = upReq.Header
	dump.ReqBody = body

	// Connect-таймаут из ТЕКУЩЕГО снапшота — как у /v1/* (F1): таймер-канцеляция
	// вокруг client.Do, клиент создаёт только socks (I1).
	reqCtx, cancelReq := context.WithCancel(r.Context())
	defer cancelReq()
	var connectFired atomic.Bool
	connect := s.params.ConnectTimeout
	if d := time.Duration(snap.Cfg.Server.UpstreamConnectTimeout); d > 0 {
		connect = d
	}
	var connectTimer *time.Timer
	if connect > 0 {
		connectTimer = time.AfterFunc(connect, func() {
			connectFired.Store(true)
			cancelReq()
		})
	}

	resp, err := tr.Client().Do(upReq.WithContext(reqCtx))
	// P2 (ревью Ф2b): таймер останавливается СРАЗУ после Do, а не по defer —
	// defer держал бы его живым на всё чтение тела (до 16 МБ), и здоровый
	// ответ убивался бы выстрелом connect-таймаута посреди тела.
	if connectTimer != nil {
		connectTimer.Stop()
	}
	if err != nil {
		status := http.StatusBadGateway
		stage := upstream.StageUpstream
		code := "network_error"
		msg := "апстрим недоступен: " + err.Error()
		var be *socks.BlockedError
		if connectFired.Load() {
			msg = fmt.Sprintf("upstream_connect_timeout(%v) истёк до заголовков апстрима", connect)
		} else if errors.As(err, &be) {
			// Gate закрыл путь (I4): 503 с именем пути и причиной.
			status = http.StatusServiceUnavailable
			stage = upstream.StageSocksCheck
			code = errorCodeForKind(upstream.KindSocksBlocked)
			msg = be.Error()
		} else if r.Context().Err() != nil {
			// Клиент сам ушёл — писать и логировать нечего, кроме лога.
			return
		}
		s.recordForwardError(clientKey, providerName, accountID, dump, status, stage, msg)
		writeError(w, upstream.ProtocolOpenAI, status, code, msg, nil)
		return
	}
	defer resp.Body.Close()
	if connectFired.Load() {
		// Гонка «заголовки одновременно с выстрелом» (образец glm.go): тело
		// мертво (ctx уже отменён), использовать ответ нельзя — 502 network.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		msg := fmt.Sprintf("upstream_connect_timeout(%v) истёк одновременно с получением заголовков", connect)
		s.recordForwardError(clientKey, providerName, accountID, dump,
			http.StatusBadGateway, upstream.StageUpstream, msg)
		writeError(w, upstream.ProtocolOpenAI, http.StatusBadGateway, "network_error", msg, nil)
		return
	}

	// Ответ — статус+заголовки+тело как есть. Это не LLM-запрос: своим
	// конвертом ошибки не заменяем ничего — даже не-2xx прозрачен (статус и
	// тело апстрима доезжают до клиента без изменений).
	respBody, truncated, err := readCappedForward(resp.Body, maxForwardBody)
	if err != nil {
		// Обрыв чтения тела — единственное место, где «как есть» невозможно.
		s.recordForwardError(clientKey, providerName, accountID, dump,
			http.StatusBadGateway, upstream.StageUpstream, "чтение тела ответа апстрима оборвалось: "+err.Error())
		writeError(w, upstream.ProtocolOpenAI, http.StatusBadGateway, "network_error",
			"чтение тела ответа апстрима оборвалось", nil)
		return
	}
	if truncated {
		// Молча резать нельзя: клиент получил бы битый JSON. Превышение капа
		// считаем сбоем форварда (для discovery-ответов кап недостижим).
		s.recordForwardError(clientKey, providerName, accountID, dump,
			http.StatusBadGateway, upstream.StageUpstream,
			fmt.Sprintf("тело ответа апстрима превышает %d байт", maxForwardBody))
		writeError(w, upstream.ProtocolOpenAI, http.StatusBadGateway, "network_error",
			fmt.Sprintf("тело ответа апстрима превышает %d байт", maxForwardBody), nil)
		return
	}

	h := w.Header()
	copyForwardHeaders(h, resp.Header)
	// Content-Length явный: тело читано целиком, размер известен; заголовок
	// апстрима мог врать (после readCapped мы отвечаем своей длиной).
	h.Set("Content-Length", fmt.Sprintf("%d", len(respBody)))
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		// Клиент ушёл — ответ доставить некому; это не ошибка апстрима.
		return
	}
	// Flush: тело уходит клиенту до любых наших фоновых работ (AC-4).
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// forwardHeaders — заголовки апстрим-запроса: копия клиентских минус
// hop-by-hop, минус секреты КЛИЕНТА прокси (Authorization/x-api-key — это
// ключ прокси, апстриму его знать не нужно), плюс Authorization аккаунта.
func forwardHeaders(client http.Header, apiKey string) http.Header {
	h := client.Clone()
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
	h.Del("Authorization") // ключ клиента прокси не утекает апстриму
	h.Del("X-Api-Key")
	h.Set("Authorization", "Bearer "+apiKey)
	return h
}

// copyForwardHeaders — заголовки ответа апстрима клиенту как есть, минус
// hop-by-hop. Content-Length ставит forward сам (тело перечитано). Retry-After
// и x-ratelimit-* приезжают вместе со всем остальным: «не вредно» (SPEC 4),
// единого кода дешевле, чем исключения.
func copyForwardHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if lk == "content-length" {
			continue
		}
		hop := false
		for _, h := range hopByHopHeaders {
			if strings.EqualFold(h, k) {
				hop = true
				break
			}
		}
		if hop {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// readCappedForward читает до max байт; truncated — признак превышения капа
// (локальный помощник форварда: upstream-пакеты такой хелпер держат при себе).
func readCappedForward(body io.Reader, max int64) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > max {
		return b[:max], true, nil
	}
	return b, false, nil
}

// recordForwardError — дамп неудачи форварда в errors (SPEC 10). Строки
// requests нет намеренно: это не запрос модели (SPEC 9 требует model).
func (s *Server) recordForwardError(clientKey, providerName, accountID string,
	dump *upstream.Dump, status int, stage, msg string) {

	if s.params.ErrorsLog == nil {
		return
	}
	row := errorslog.ErrorRow{
		Ts:          time.Now(),
		ClientKey:   clientKey,
		Provider:    providerName,
		Account:     accountID,
		Stage:       stage,
		Status:      status,
		Err:         msg,
		Method:      dump.Method,
		URL:         dump.URL,
		ReqHeaders:  errorslog.MaskedHeaders(dump.ReqHeaders),
		ReqBody:     dump.ReqBody,
		RespHeaders: errorslog.MaskedHeaders(dump.RespHeaders),
		RespBody:    dump.RespBody,
	}
	if err := s.params.ErrorsLog.InsertError(row); err != nil {
		// AC-4: сбой БД не роняет ответ клиенту.
		logWarn("errorslog: запись дампа форварда не удалась", "err", err, "stage", stage)
	}
}
