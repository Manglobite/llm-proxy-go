// Хендлеры /v1/* и /healthz (SPEC 4–5): auth → decode → route → Send →
// учёт/ошибки → конверт. Жизненный цикл — ARCHITECTURE §3.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/router"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// authHead — поля входного тела, нужные ДО роутинга (все три протокола зовут
// их одинаково и держат top-level: model/stream, у Responses API — тоже).
// Полный парсинг делает модуль провайдера: passthrough-клетки не должны
// отказываться из-за строгости нашего парсера (неподдерживаемые нами части
// контента валиден сам апстрим).
type authHead struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// methodGuard — 405 обязан быть JSON-конвертом (SPEC 4: никогда HTML), поэтому
// маршруты /v1/* регистрируются без метода, а проверка — здесь.
func (s *Server) methodGuard(method, protocol string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			writeError(w, protocol, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("метод %q не поддерживается, ожидается %s", r.Method, method), nil)
			return
		}
		h(w, r)
	}
}

// handleChatCompletions — POST /v1/chat/completions (openai-вход).
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleV1(w, r, upstream.ProtocolOpenAI)
}

// handleMessages — POST /v1/messages (anthropic-вход).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleV1(w, r, upstream.ProtocolAnthropic)
}

// handleResponses — POST /v1/responses (Responses API, SPEC 4/8.3/8.7).
// Конвейер общий с остальными входами: отличия — конверт ошибок (openai-форма,
// envelope.go) и клетки матрицы SPEC 8.2 в модулях провайдеров.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleV1(w, r, upstream.ProtocolResponses)
}

// handleHealthz — GET /healthz без авторизации (SPEC 4).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		// Клиент уже ушёл — ответить корректно невозможно, остаётся лог.
		logWarn("healthz: не удалось записать ответ", "err", err)
	}
}

// notFound — catch-all для незарегистрированных путей: JSON-конверт 404 вместо
// text/plain ServeMux (SPEC 4: ошибки — всегда JSON, никогда HTML). Протокол
// входа угадываем по пути (auth не требуется: это 404 маршрутизации, а не
// аутентификации — ключ проверяют только настоящие маршруты). /v1/responses
// проверяется раньше openai-дефолта: и POST несуществующих субпутей
// (/v1/responses/{id} — stateless, SPEC 4), и GET основного пути обязаны
// отвечать в форме Responses API (та же openai-форма).
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	protocol := upstream.ProtocolOpenAI
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/messages"):
		protocol = upstream.ProtocolAnthropic
	case strings.HasPrefix(r.URL.Path, "/v1/responses"):
		protocol = upstream.ProtocolResponses
	}
	writeError(w, protocol, http.StatusNotFound, "not_found",
		fmt.Sprintf("маршрут %s %q не существует", r.Method, r.URL.Path), nil)
}

// handleV1 — общий конвейер всех входных протоколов.
func (s *Server) handleV1(w http.ResponseWriter, r *http.Request, protocol string) {
	snap := s.params.Snapshot()
	if snap == nil || snap.Cfg == nil {
		// Снапшот исчезнуть не может (Manager хранит последний валидный) —
		// защита от неверной сборки, а не штатная ветка.
		writeError(w, protocol, http.StatusServiceUnavailable, "internal_error", "конфигурация недоступна", nil)
		return
	}

	// 1. Auth (SV-1): Bearer или x-api-key, оба протокола.
	clientKey, ok := s.authenticate(r, snap)
	if !ok {
		writeError(w, protocol, http.StatusUnauthorized, "invalid_api_key",
			"неизвестный client key: передайте именованный ключ в Authorization: Bearer или x-api-key", nil)
		return
	}

	// 2. Decode (TR-11): identity|gzip|zstd, кап распакованного.
	body, he := s.decodeBody(r)
	if he != nil {
		writeError(w, protocol, he.status, he.code, he.msg, nil)
		return
	}

	var head authHead
	if err := json.Unmarshal(body, &head); err != nil || head.Model == "" {
		msg := "тело запроса не содержит строковое поле model (или не является JSON-объектом)"
		// SPEC 10: ошибка разбора входа — трансляция; дамп клиентского запроса.
		s.recordClientError(clientKey, r, body, http.StatusBadRequest, msg)
		writeError(w, protocol, http.StatusBadRequest, "invalid_request", msg, nil)
		return
	}

	// 3. Роутинг (SPEC 5): алиас → (провайдер, аккаунт, модель апстрима).
	target, err := router.Resolve(snap, head.Model)
	switch {
	case errors.Is(err, router.ErrUnknownAlias):
		// SV-2: 404 со списком доступных алиасов, конверт протокола входа.
		writeError(w, protocol, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("неизвестная модель %q; доступные алиасы: %s",
				head.Model, strings.Join(router.AvailableAliases(snap), ", ")), nil)
		return
	case err != nil:
		// Рассинхронизация снапшота — не ошибка клиента.
		writeError(w, protocol, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}

	// 4. Transporter аккаунта из снапшота (роутер про сеть не знает).
	tr, err := transporterFor(snap, target.SocksName)
	if err != nil {
		writeError(w, protocol, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}

	// 4a. Модуль провайдера по ТИПУ (SPEC 7): glm/openai-like сейчас, codex —
	// с Ф3 (запись в Params.Senders, сервер не меняется).
	snd := s.senderFor(target.ProviderType)
	if snd == nil {
		// Тип сконфигурирован, модуль не подключен — ошибка сборки, не
		// свойство апстрима: 502 без записи в requests (запрос не отправлялся).
		writeError(w, protocol, http.StatusBadGateway, "internal_error",
			fmt.Sprintf("модуль провайдера типа %q не подключен (codex — фаза Ф3)", target.ProviderType), nil)
		return
	}

	// 5. Отправка модулем провайдера; ответ клиенту пишет провайдер.
	// Таймауты — из ТЕКУЩЕГО снапшота (релоадятся, ARCHITECTURE §7): копия в
	// Params пережила бы SIGHUP и врала бы после него. Нулевые значения в
	// снапшоте невозможны после config.Load (дефолты применяются там), фолбэк
	// на Params — защита на случай снапшота, собранного вручную.
	idle := s.params.StreamIdleTimeout
	if d := time.Duration(snap.Cfg.Server.StreamIdleTimeout); d > 0 {
		idle = d
	}
	connect := s.params.ConnectTimeout
	if d := time.Duration(snap.Cfg.Server.UpstreamConnectTimeout); d > 0 {
		connect = d
	}
	sendReq := &upstream.SendRequest{
		Snap:           snap,
		Target:         target,
		Transporter:    tr,
		Protocol:       protocol,
		Body:           body,
		Stream:         head.Stream,
		W:              w,
		Keepalive:      s.params.KeepaliveInterval, // снапшот-независимый (ключа в конфиге нет)
		ConnectTimeout: connect,
		IdleTimeout:    idle,
	}
	start := time.Now()
	res, err := snd.Send(r.Context(), sendReq)
	latencyMS := time.Since(start).Milliseconds()
	if err != nil || res == nil {
		var be *socks.BlockedError
		if errors.As(err, &be) {
			// FC-5/I4: BlockedError от провайдера мимо классификации — единый
			// 503-конверт с именем пути и причиной (защита на случай, если
			// будущий провайдер не обернёт ошибку gate в SendResult).
			res = &upstream.SendResult{
				ErrorKind:    upstream.KindSocksBlocked,
				Stage:        upstream.StageSocksCheck,
				ClientStatus: http.StatusServiceUnavailable,
				Message:      be.Error(),
			}
		} else {
			// Нарушение контракта Sender (upstream.Sender, «(nil, err)») —
			// трактуем как 502: это баг сборки, клиенту всё равно нужен ответ.
			logWarn("нарушение контракта Sender", "err", err)
			res = &upstream.SendResult{
				ErrorKind:    upstream.KindNetwork,
				Stage:        upstream.StageUpstream,
				ClientStatus: http.StatusBadGateway,
				Message:      "внутренняя ошибка отправки: " + errorText(err),
			}
		}
	}

	// 6. Неудача: конверт клиенту ДО записи в БД — клиент не должен ждать
	// INSERT'ы с busy_timeout(5s), если SQLite занята (F5). Streamed уже
	// получил SSE-ошибку от провайдера — HTTP-конверт писать нельзя (SPEC 4).
	if res.ErrorKind != "" && !res.Streamed {
		writeError(w, protocol, res.ClientStatus, errorCodeForKind(res.ErrorKind),
			res.Message, filterRateLimitHeaders(res.UpstreamHeader))
	}

	// 7. Учёт: строка requests на любой исход (успех И ошибка, SPEC 9) —
	// после ответа клиенту; сбой БД не роняет ответ (AC-4).
	s.recordRequest(snap, target, clientKey, head.Model, protocol, head.Stream, latencyMS, res)

	// 7a. Квоты (SPEC 11, Ф3): снапшоты из ответа провайдера (codex кладёт
	// окна x-codex-* и маркер 429 в SendResult.Quotas). nil-DB — учёт квот
	// отключён; сбой БД не роняет ответ (AC-4).
	if res.Quotas != nil && s.params.Quotas != nil {
		s.recordQuotas(target, res.Quotas)
	}

	// 8. Дамп неудачи в errors.
	if res.ErrorKind != "" {
		s.recordError(snap, target, clientKey, head.Model, r, body, res)
	}
}

// handleModels — GET /v1/models (SV-3): список алиасов в openai-форме.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	snap := s.params.Snapshot()
	if snap == nil || snap.Cfg == nil {
		writeError(w, upstream.ProtocolOpenAI, http.StatusServiceUnavailable, "internal_error", "конфигурация недоступна", nil)
		return
	}
	if _, ok := s.authenticate(r, snap); !ok {
		writeError(w, upstream.ProtocolOpenAI, http.StatusUnauthorized, "invalid_api_key",
			"неизвестный client key: передайте именованный ключ в Authorization: Bearer или x-api-key", nil)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: make([]model, 0)}
	for _, alias := range router.AvailableAliases(snap) {
		t, err := router.Resolve(snap, alias)
		if err != nil {
			continue // ссылка проверена при загрузке; не роняем список из-за одной
		}
		list.Data = append(list.Data, model{
			ID: alias, Object: "model",
			OwnedBy: t.ProviderName + "/" + t.AccountID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(list)
	if err != nil {
		writeError(w, upstream.ProtocolOpenAI, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	if _, err := w.Write(b); err != nil {
		// Клиент уже ушёл — статус уже отдан 200, остаётся лог.
		logWarn("models: не удалось записать ответ", "err", err)
	}
}

// authenticate — клиентский ключ (SV-1): Authorization: Bearer <key> или
// x-api-key: <key> на обоих протоколах. Возвращает ИМЯ клиента (учёт ведётся
// по именам, SPEC 2) и факт успеха.
func (s *Server) authenticate(r *http.Request, snap *config.Snapshot) (string, bool) {
	provided := r.Header.Get("X-Api-Key")
	if provided == "" {
		auth := r.Header.Get("Authorization")
		if rest, found := strings.CutPrefix(auth, "Bearer "); found {
			provided = strings.TrimSpace(rest)
		}
	}
	if provided == "" {
		return "", false
	}
	// Перебор ключей, а не map[значение]: один ключ может быть назначен
	// нескольким именам — имя берём первое найденное.
	for name, key := range snap.Cfg.Server.Clients {
		if key != "" && constantTimeEqual(provided, key) {
			return name, true
		}
	}
	return "", false
}

// constantTimeEqual — сравнение секретов за постоянное время: длина ответа не
// должна подсказывать перебирающему, сколько символов совпало.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// errorText — nil-safe текст ошибки для сообщения.
func errorText(err error) string {
	if err == nil {
		return "Sender вернул пустой результат"
	}
	return err.Error()
}
