// Пакет internal/server — маршруты /v1/* и /healthz: auth клиентов, decode тел
// (identity|gzip|zstd), роутинг алиасов, конверты ошибок протокола входа,
// учёт (accounting) и дампы ошибок (errorslog) (SPEC 4–5, ARCHITECTURE §3).
// R5: никто из internal не импортирует server — зависимости направлены вниз;
// модули провайдеров подключаются через интерфейс Sender (объявлен здесь,
// CODING 1.6) — glm/codex встают без переделки сервера.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/upstream"
)

// Дефолты параметров сервера (SPEC 4); конфиг задаёт таймауты, keepalive и
// кап тела — параметры New (тесты ставят маленькие значения, CODING 1.9).
const (
	DefaultStreamIdleTimeout       = 5 * time.Minute
	DefaultConnectTimeout          = 30 * time.Second
	DefaultKeepaliveInterval       = 30 * time.Second
	DefaultMaxBodyBytes      int64 = 64 << 20 // кап РАСПАКОВАННОГО тела (анти-бомба)
)

// Params — сборка сервера. Snapshot и хотя бы один модуль провайдера
// (Sender или Senders) обязательны, БД опциональны (nil — учёт не пишется;
// полезно узким тестам протокола).
type Params struct {
	// Snapshot — доступ к живому снапшоту конфигурации (релоад подменяет
	// снапшот атомарно; сервер берёт его на каждый запрос — клиенты/алиасы/
	// таймауты подхватываются без рестарта).
	Snapshot func() *config.Snapshot

	// Senders — модули провайдеров ПО ТИПУ (выбор по Target.ProviderType,
	// SPEC 7): "openai-like" → upstream/openailike, "glm" → upstream/glm.
	// "codex" добавляется здесь же в Ф3 — сервер не меняется.
	Senders map[string]Sender

	// Sender — единичный модуль на ВСЕ типы (режим Ф2a): используется для
	// типа, которого нет в Senders. Оставлен для узких тестов протокола и
	// монопровайдерных сборок; main подключает оба модуля через Senders.
	Sender Sender

	// Accounting/ErrorsLog — SQLite учёт; nil отключает запись.
	Accounting *accounting.DB
	ErrorsLog  *errorslog.DB
	// Quotas — SQLite quota_snapshots (SPEC 11, Ф3); nil отключает запись.
	// Снапшоты пишет из SendResult.Quotas (провайдер парсит свои x-codex-*),
	// форма нейтральная — объявлена в upstream.Quotas.
	Quotas *quotas.DB

	// WebUI — статика + JSON API /api/* (Ф4, SPEC 12); nil → WebUI отключён
	// (узкие тесты протокола работают без него). Интерфейс объявлен
	// потребителем (CODING 1.6); реализация — internal/webui.Handler, которая
	// сама сообщает точные паттерны маршрутов. Регистрация ТОЧНЫМИ путями
	// (а не префиксом): ServeMux отдаёт им приоритет над "/api/" форварда.
	// Коллизия имени forward_paths-префикса с webui-маршрутом — ответственность
	// конфигуратора: победит webui-маршрут (более специфичный).
	WebUI WebUI

	// StreamIdleTimeout — idle между событиями апстрима (SPEC 4, из конфига).
	StreamIdleTimeout time.Duration
	// ConnectTimeout — коннект к апстриму до заголовков (из конфига).
	ConnectTimeout time.Duration
	// KeepaliveInterval — интервал ': ping' в стриме. В config.json ключа нет
	// (SPEC 4 фиксирует 30s), поэтому параметр: тестам нужны малые значения
	// без time.Sleep.
	KeepaliveInterval time.Duration
	// MaxBodyBytes — кап распакованного тела запроса (SPEC 4: 64 МБ).
	MaxBodyBytes int64
}

// Sender — интерфейс модуля провайдера, ОБЪЯВЛЕН ПОТРЕБИТЕЛЕМ (CODING 1.6).
// Реализации: upstream/openailike (Ф2a), дальше upstream/glm (Ф2b) и
// upstream/codex (Ф3) — сервер при их подключении не меняется. Контракт
// Send задокументирован у upstream.SendResult.
type Sender interface {
	Send(ctx context.Context, r *upstream.SendRequest) (*upstream.SendResult, error)
}

// WebUI — статика и JSON API /api/* (Ф4, SPEC 12), ОБЪЯВЛЕН ПОТРЕБИТЕЛЕМ:
// сервер не импортирует internal/webui (R5 остаётся транзитивно чистым —
// webui не знает про server). Реализация — webui.Handler.
type WebUI interface {
	http.Handler
	// Patterns — точные паттерны ServeMux ("/{$}", "/api/status", пути
	// статики): их сервер регистрирует вместо одного catch-all, чтобы
	// сохранялся JSON-404 для незарегистрированных путей (F2).
	Patterns() []string
}

// Server — HTTP-сервер API /v1/*. Stateless, потокобезопасен: всё состояние
// запроса живёт в хендлере, конфигурация приходит через Snapshot().
type Server struct {
	params Params
}

// senderFor — модуль провайдера по типу из роутинга (SPEC 7): запись Senders
// точнее фолбэка. Отсутствие записи при сконфигурированном типе — ошибка
// сборки (hендлер отвечает 502): так бывает только если тип включили в конфиг
// раньше, чем подключили модуль (codex до Ф3).
func (s *Server) senderFor(providerType string) Sender {
	if snd, ok := s.params.Senders[providerType]; ok && snd != nil {
		return snd
	}
	return s.params.Sender // единичный модуль (Ф2a-стиль): один на все типы
}

// New собирает сервер; дефолты применяются к нулевым длительностям.
func New(p Params) (*Server, error) {
	if p.Snapshot == nil {
		return nil, errParams("Snapshot не задан")
	}
	if p.Sender == nil && len(p.Senders) == 0 {
		return nil, errParams("Sender/Senders не задан")
	}
	if p.StreamIdleTimeout <= 0 {
		p.StreamIdleTimeout = DefaultStreamIdleTimeout
	}
	if p.ConnectTimeout <= 0 {
		p.ConnectTimeout = DefaultConnectTimeout
	}
	if p.KeepaliveInterval <= 0 {
		p.KeepaliveInterval = DefaultKeepaliveInterval
	}
	if p.MaxBodyBytes <= 0 {
		p.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return &Server{params: p}, nil
}

// Handler собирает маршруты (Go 1.22 ServeMux). Методы проверяются внутри
// хендлеров: 405 обязан быть JSON-конвертом протокола входа, а не текстом
// ServeMux (SPEC 4: ошибки — всегда JSON, никогда HTML). Catch-all "/"
// отдаёт JSON 404 для незарегистрированных путей (F2).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.methodGuard(http.MethodPost, upstream.ProtocolOpenAI, s.handleChatCompletions))
	mux.HandleFunc("/v1/messages", s.methodGuard(http.MethodPost, upstream.ProtocolAnthropic, s.handleMessages))
	mux.HandleFunc("/v1/responses", s.methodGuard(http.MethodPost, upstream.ProtocolResponses, s.handleResponses))
	mux.HandleFunc("/v1/models", s.methodGuard(http.MethodGet, upstream.ProtocolOpenAI, s.handleModels))
	// healthz — без авторизации, для оркестрации контейнера (SPEC 4);
	// метод-гард внутри: 405 тоже JSON.
	mux.HandleFunc("/healthz", s.methodGuard(http.MethodGet, upstream.ProtocolOpenAI, s.handleHealthz))
	// forward_paths (SV-4/OA-9): /api/* — прозрачный форвард префиксов
	// forward_paths openai-like провайдеров. Проверка «есть ли совпадение» —
	// на каждый запрос по ЖИВОМУ снапшоту: список префиксов релоадится, а
	// ServeMux собирается один раз. Нет совпадения — тот же JSON 404, что и у
	// catch-all (поведение «маршруты не регистрировались» сохранено). Точные
	// маршруты WebUI (Ф4) в ServeMux главнее этого префикса.
	mux.HandleFunc("/api/", s.handleForwardAPI)
	// WebUI (Ф4): точные паттерны статики и /api/* — ServeMux-специфичность
	// делает их главнее префикса "/api/" и catch-all; auth (webui_token)
	// внутри webui-гарда, /healthz и статика без него (SV-5/CF-5).
	if s.params.WebUI != nil {
		for _, pattern := range s.params.WebUI.Patterns() {
			mux.Handle(pattern, s.params.WebUI)
		}
	}
	// catch-all: незарегистрированные пути — JSON 404 (F2); при включённом
	// WebUI неопознанные пути НЕ отдают index.html — opencode-клиент должен
	// получать JSON-ошибку на любую /v1/*-опечатку (SPEC 4).
	mux.HandleFunc("/", s.notFound)
	return mux
}

// errParams — ошибка сборки (возврат до старта сервера).
func errParams(msg string) error { return &paramError{msg} }

type paramError struct{ msg string }

func (e *paramError) Error() string { return "server: " + e.msg }

// logWarn — единая точка предупреждений сервера (slog структурно, CODING 1.8).
func logWarn(msg string, args ...any) { slog.Warn(msg, args...) }
