// Пакет internal/upstream — общий контракт модулей провайдеров и общая
// механика прокачки SSE-стримов. Типы SendRequest/SendResult живут здесь
// (а не в server), потому что провайдер НЕ может импортировать server (R5):
// интерфейс Sender при этом объявляет потребитель — internal/server
// (CODING 1.6: интерфейсы объявляют потребитель).
//
// Контракт Send модуля провайдера (реализации: openailike — Ф2a, glm — Ф2b,
// codex — Ф3):
//
//   - успех: (result{ErrorKind:""}, nil); ответ клиенту уже написан провайдером;
//   - классифицированная неудача (blocked/network/upstream/translate/stream):
//     (result с ErrorKind/Stage/Message/ClientStatus, nil) — сервер пишет
//     errorslog, строку accounting и, если !Streamed, конверт ошибки в
//     протоколе входа;
//   - (nil, err) — только нарушение контракта вызова (неизвестный ProviderType/
//     Protocol, nil transporter): сервер трактует как 502 — это баг сборки,
//     а не свойство апстрима.
//
// Провайдер сам решает passthrough или конвертацию по клеткам матрицы
// SPEC 8.2 и обязан вести tee-usage единой нормализацией translate (TR-3).
package upstream

import (
	"net/http"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/router"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/translate"
)

// Входные протоколы сервера (колонка protocol в SPEC 9).
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	ProtocolResponses = "responses" // /v1/responses (SPEC 4/8.3, Ф6b)
)

// error_kind для accounting (SPEC 9) и stage для errorslog (SPEC 10).
// Значения — закрытый список таксономии (ARCHITECTURE §10).
const (
	KindNetwork      = "network"       // сеть к апстриму / обрыв стрима
	KindSocksBlocked = "socks_blocked" // gate SOCKS закрыл путь
	KindAuth         = "auth"          // OAuth exchange/refresh (Ф3)
	KindUpstream     = "upstream"      // не-2xx апстрима / error-событие стрима
	KindTranslate    = "translate"     // конвертация входа не удалась
)

const (
	StageSocksCheck = "socks_check"
	StageAuth       = "auth"
	StageUpstream   = "upstream"
	StageStream     = "stream"
	StageTranslate  = "translate"
)

// SendRequest — всё, что модуль провайдера получает на запрос. Собирает сервер
// (жизненный цикл ARCHITECTURE §3): конфиг-снапшот, резолв алиаса и transporter
// аккаунта сервер добирает сам, провайдер про пул SOCKS не думает.
type SendRequest struct {
	// Snap — текущий снапшот конфигурации: провайдер достаёт из него
	// base_url/ключ своего аккаунта (форма аккаунта — знание провайдера).
	Snap *config.Snapshot

	// Target — куда идти (результат router.Resolve).
	Target router.Target

	// Transporter — клиент аккаунта (snap.Socks[name] или socks.Direct для
	// "none"). R1: http.Client создаёт только internal/socks, провайдер
	// использует готовый — через его gate не проскочить.
	Transporter socks.Transporter

	// Protocol — входной протокол клиента (ProtocolOpenAI | ProtocolAnthropic |
	// ProtocolResponses): определяет и сборку тела (матрица SPEC 8.2), и форму
	// ответа клиенту.
	Protocol string

	// Body — декодированное (identity|gzip|zstd) тело ЗАПРОСА КЛИЕНТА.
	// Пасстраху или конвертации провайдер решает сам по Protocol.
	Body []byte

	// Stream — просит ли клиент стрим (поле stream входного тела).
	Stream bool

	// W — клиентский ResponseWriter. Ответ клиенту пишут трансляторы
	// translate (они принимают http.ResponseWriter); при НЕУДАЧЕ до стрима
	// провайдер в W ничего не пишет — конверт ошибки пишет сервер.
	W http.ResponseWriter

	// Keepalive — интервал SSE-комментария ': ping' при тишине апстрима
	// (0 — без keepalive; SPEC 4: 30s, тесты ставят меньше).
	Keepalive time.Duration

	// ConnectTimeout — лимит до получения заголовков апстрима
	// (server.upstream_connect_timeout, SPEC 4).
	ConnectTimeout time.Duration

	// IdleTimeout — лимит тишины между событиями стрима апстрима
	// (server.stream_idle_timeout, SPEC 4): обрыв с SSE-ошибкой клиенту.
	IdleTimeout time.Duration
}

// Dump — дамп запроса/ответа апстрима для errorslog (SPEC 10). Заголовки
// провайдер кладёт СЫРЫЕ: маскировку (errorslog.MaskedHeaders) делает сервер
// перед InsertError — единственная точка, где секреты ещё живы, это память.
type Dump struct {
	Method, URL string
	ReqHeaders  http.Header
	ReqBody     []byte
	RespHeaders http.Header
	RespBody    []byte
}

// SendResult — итог Send в ЛЮБОМ случае (успех и классифицированная неудача
// несут результат): сервер после Send единым кодом пишет строку accounting
// (успех И ошибка — error_kind, SPEC 9) и, при !Streamed, конверт ошибки.
type SendResult struct {
	// ErrorKind — "" при успехе, иначе значение из SPEC 9.
	ErrorKind string
	// Stage — для errorslog (SPEC 10); "" при успехе.
	Stage string
	// ClientStatus — HTTP-статус, который сервер отдаст клиенту при неудаче
	// (таксономия ARCHITECTURE §10: 503 blocked / 502 network / статус
	// апстрима / 400 translate). При Streamed не используется.
	ClientStatus int
	// Message — человекочитаемое описание ошибки (в конверт клиенту и в errors).
	Message string
	// Streamed — SSE-ответ клиенту уже начат: ошибка ушла SSE-событием
	// (SPEC 4: «200 уже начат → SSE-событие error»), HTTP-конверт писать нельзя.
	Streamed bool

	// Usage — tee-usage (SPEC 8.6); при обрыве стрима — что успело накопиться.
	Usage translate.Usage
	// UpstreamStatus — HTTP-статус ответа апстрима (0 — ответа не было).
	UpstreamStatus int
	// UpstreamHeader — заголовки ответа апстрима: сервер пробрасывает клиенту
	// retry-after / x-ratelimit-* (SPEC 4).
	UpstreamHeader http.Header
	// Dump — дамп для errorslog; nil, если дампить нечего (сервер тогда
	// составляет дамп из клиентского запроса — например, для translate).
	Dump *Dump

	// Quotas — квоты провайдера, извлечённые из ЭТОГО ответа (SPEC 7.1/11):
	// codex кладёт окна из заголовков x-codex-* (приходят с HTTP-ответом
	// стрима, а не в body-чанках — AC-7) и маркер 429 usage_limit_reached.
	// Форма нейтральная, объявлена здесь, чтобы провайдер не экспортировал
	// свои типы в общий контракт, а сервер/quotas писали снапшоты одним кодом.
	// nil — провайдер квот не сообщил.
	Quotas *Quotas
}

// Имена kind в quota_snapshots (SPEC 11); codex_limited — минимальное
// расширение Ф3: маркер 429 usage_limit_reached (SPEC 11 «маркер исчерпания
// до resets_at») не выражается окнами и получает собственный kind.
const (
	QuotaCodexPrimary = "codex_primary" // 5-часовое окно codex
	QuotaCodexWeekly  = "codex_weekly"  // недельное окно codex
	QuotaCodexLimited = "codex_limited" // 429 usage_limit_reached (расширение Ф3)
)

// Quotas — срез квот одного ответа апстрима.
type Quotas struct {
	Windows []QuotaWindow
	// Limited — апстрим ответил 429 usage_limit_reached: окно исчерпано до
	// ResetAtUnix (AC-8).
	Limited *QuotaLimit
}

// QuotaWindow — одно окно квоты.
type QuotaWindow struct {
	Kind          string // QuotaCodexPrimary | QuotaCodexWeekly
	UsedPercent   float64
	WindowMinutes int
	// ResetAtUnix — момент сброса окна (unix seconds); 0 — апстрим не сообщил.
	ResetAtUnix int64
}

// QuotaLimit — маркер исчерпания окна (429 usage_limit_reached).
type QuotaLimit struct {
	ResetAtUnix int64
	Message     string
}
