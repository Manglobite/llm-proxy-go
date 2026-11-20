// Пакет internal/quotas — квоты (SPEC 11): таблица quota_snapshots, локальный
// расчёт кредитов glm по скользящим окнам, поллинг usage ollama cloud.
// Про HTTP-сервер пакет не знает ничего (R4): снапшоты codex пишет сервер из
// SendResult.Quotas, ollama-поллер стартует main по снапшоту конфигурации.
package quotas

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go драйвер: сборка CGO_ENABLED=0 (CODING 1.1)

	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// dsnPragmas — та же строка подключения, что у accounting/errorslog: один
// файл SQLite, второй (третий) коннект к нему; WAL позволяет читателям и
// писателям сосуществовать, busy_timeout — не падать от SQLITE_BUSY.
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

// schema — SPEC 11 посимвольно: поля quota_snapshots, индекс ts.
// IF NOT EXISTS — миграция идемпотентна.
const schema = `
CREATE TABLE IF NOT EXISTS quota_snapshots (
  id INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,
  provider TEXT NOT NULL, account_id TEXT NOT NULL,
  kind TEXT NOT NULL,        -- 'codex_primary' | 'codex_weekly' (+расширения Ф3: 'codex_limited', 'ollama_usage'; Ф8: 'zcode_usage')
  used_percent REAL, window_minutes INTEGER, reset_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_ts ON quota_snapshots(ts);
`

// KindOllamaUsage — минимальное расширение SPEC 11 (codex-kind'ы объявлены
// в upstream): снапшот поллинга /api/usage. usage_usd хранится в
// used_percent (REAL подходит по типу; SPEC 11 не определяет колонку для
// долларов — новую колонку не вводим, чтобы схема осталась ровно SPEC 11).
const KindOllamaUsage = "ollama_usage"

// SnapshotRow — строка quota_snapshots. Нулевые необязательные поля пишутся
// NULL (HasWindow/HasReset различают «0» и «не сообщено»).
type SnapshotRow struct {
	Ts            time.Time
	Provider      string
	Account       string
	Kind          string
	UsedPercent   float64
	WindowMinutes int64
	HasWindow     bool
	ResetAt       int64
	HasReset      bool
}

// DB — соединение с SQLite (таблица quota_snapshots + READ-канал к requests
// для glm-кредитов). Потокобезопасно: database/sql сериализует через пул.
type DB struct{ db *sql.DB }

// Open открывает БД и накатывает миграцию. Таблица requests (для
// GLMBalance) создаётся accounting при его Open — main обязан открыть
// accounting раньше quotas; здесь зависимость только читается (см. GLMBalance).
func Open(path string) (*DB, error) {
	d, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close() // не оставляем открытое соединение при неудачной миграции
		return nil, fmt.Errorf("quotas schema: %w", err)
	}
	return &DB{db: d}, nil
}

// Close чекпоинтит WAL и закрывает соединение — зеркально accounting.Close
// (после остановки рядом с proxy.db не должен оставаться несобранный -wal).
func (d *DB) Close() error {
	if _, err := d.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		err2 := d.db.Close()
		return fmt.Errorf("wal checkpoint: %v (close: %v)", err, err2)
	}
	return d.db.Close()
}

// Insert пишет один снапшот. Вызывается на каждый ответ codex с x-codex-*
// заголовками и на каждый успешный ollama-полл; сбой БД наверху только
// логируется (AC-4: учёт не роняет ответ клиенту).
func (d *DB) Insert(r SnapshotRow) error {
	var window, reset any
	if r.HasWindow {
		window = r.WindowMinutes
	}
	if r.HasReset {
		reset = r.ResetAt
	}
	_, err := d.db.Exec(`INSERT INTO quota_snapshots
		(ts, provider, account_id, kind, used_percent, window_minutes, reset_at)
		VALUES (?,?,?,?,?,?,?)`,
		r.Ts.UTC().Format(time.RFC3339Nano), r.Provider, r.Account, r.Kind,
		r.UsedPercent, window, reset)
	return err
}

// Recent — ПОСЛЕДНИЕ снапшоты (F1/P1): до limit самых свежих строк, возврат
// в хронологическом порядке (по возрастанию id) — SPEC 11 обещает «текущие
// проценты», а не историю с начала таблицы. Точка чтения для экрана квот
// (Ф4) и тестов: пакет владеет таблицей, наружный SQL не разбросан.
// Необязательные поля отражаются HasWindow/HasReset.
func (d *DB) Recent(limit int) ([]SnapshotRow, error) {
	// Внутренний DESC отбирает хвост таблицы, внешний ASC разворачивает в
	// хронологию — один запрос вместо DESC-выборки с разворотом среза в Go.
	// id в проекции подзапроса нужен только внешнему ORDER BY.
	rows, err := d.db.Query(`SELECT ts, provider, account_id, kind, used_percent, window_minutes, reset_at
		FROM (SELECT id, ts, provider, account_id, kind, used_percent, window_minutes, reset_at
		      FROM quota_snapshots ORDER BY id DESC LIMIT ?) ORDER BY id`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var r SnapshotRow
		var tsRaw string
		var window, reset sql.NullInt64
		if err := rows.Scan(&tsRaw, &r.Provider, &r.Account, &r.Kind, &r.UsedPercent, &window, &reset); err != nil {
			return nil, err
		}
		if r.Ts, err = time.Parse(time.RFC3339Nano, tsRaw); err != nil {
			return nil, fmt.Errorf("ts %q: %w", tsRaw, err)
		}
		if window.Valid {
			r.WindowMinutes, r.HasWindow = window.Int64, true
		}
		if reset.Valid {
			r.ResetAt, r.HasReset = reset.Int64, true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FromUpstream — конвертация нейтральной формы SendResult.Quotas в строки
// снапшотов (kind-ы SPEC 11). 429 usage_limit_reached → отдельная строка
// kind codex_limited с reset_at (минимальное расширение Ф3: «маркер
// исчерпания до resets_at» не выражается окном; used_percent=100 — окно
// исчерпано по определению 429).
func FromUpstream(provider, account string, q *upstream.Quotas, now time.Time) []SnapshotRow {
	if q == nil {
		return nil
	}
	out := make([]SnapshotRow, 0, len(q.Windows)+1)
	for _, w := range q.Windows {
		row := SnapshotRow{
			Ts: now, Provider: provider, Account: account, Kind: w.Kind,
			UsedPercent: w.UsedPercent,
		}
		// F4/P3: не сообщённое поле — NULL, а не 0 (парсер квот отдаёт
		// частичные окна — например, только used-percent): HasWindow только
		// при реально пришедшем window_minutes.
		if w.WindowMinutes > 0 {
			row.WindowMinutes, row.HasWindow = int64(w.WindowMinutes), true
		}
		if w.ResetAtUnix != 0 {
			row.ResetAt, row.HasReset = w.ResetAtUnix, true
		}
		out = append(out, row)
	}
	if q.Limited != nil {
		row := SnapshotRow{
			Ts: now, Provider: provider, Account: account, Kind: upstream.QuotaCodexLimited,
			UsedPercent: 100,
		}
		if q.Limited.ResetAtUnix != 0 {
			row.ResetAt, row.HasReset = q.Limited.ResetAtUnix, true
		}
		out = append(out, row)
	}
	return out
}

// ─── glm: локальный расчёт кредитов (SPEC 7.2/11) ───────────────────────────

// READ-канал glm-кредитов (решение задокументировано): quotas открывает
// ВТОРОЕ соединение к тому же файлу SQLite (Open выше) и читает requests
// READ-ONLY SELECT'ами, вместо интерфейса к accounting. Основание: accounting
// и errorslog уже живут по схеме «каждый владелец таблицы открывает файл сам»
// (main открывает оба на один путь), WAL+busy_timeout делают параллельные
// коннекты штатными; отдельный интерфейс добавил бы связность accounting↔quotas
// ради одного SELECT. Порядок старта: accounting.Open раньше quotas.Open
// (схему requests накатывает accounting) — проверяет main.

// GLMPlan — лимиты glm Coding Plan (config.PlanCredits по значению).
type GLMPlan struct {
	Window5h     int64
	WindowWeekly int64
}

// GLMBalance — локальная оценка расхода glm-аккаунта (SPEC 7.2/11):
// SUM(glm_credits) из requests за скользящие окна 5 часов и 7 суток.
// Проценты считаются от plan (nil → проценты 0); окно z.ai привязано к дате
// подписки — это локальная аппроксимация, экран показывает оценку.
func (d *DB) GLMBalance(accountID string, plan *GLMPlan, now time.Time) (used5h, usedWeek, percent5h, percentWeek float64, err error) {
	// Границы окон — RFC3339 без наносекунд: requests пишет ts строкой
	// RFC3339Nano, и строковое сравнение «same second с долей» может ошибиться
	// на <1s у границы. Для экрана квот это несущественно; тесты опираются на
	// целосекундные границы, где сравнение точное.
	from5h := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	fromWeek := now.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, b := range []struct {
		from string
		out  *float64
	}{{from5h, &used5h}, {fromWeek, &usedWeek}} {
		err = d.db.QueryRow(`SELECT COALESCE(SUM(glm_credits),0) FROM requests
			WHERE account_id = ? AND ts >= ?`, accountID, b.from).Scan(b.out)
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("glm balance: %w", err)
		}
	}
	if plan != nil {
		percent5h = percentOf(used5h, float64(plan.Window5h))
		percentWeek = percentOf(usedWeek, float64(plan.WindowWeekly))
	}
	return used5h, usedWeek, percent5h, percentWeek, nil
}

func percentOf(used, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return used / limit * 100
}

// ─── ollama cloud: поллинг usage (SPEC 7.3/11) ──────────────────────────────

// UsageTarget — цель поллинга: провайдер openai-like с включённым usage_poll.
// Transporter — клиент ПЕРВОГО аккаунта провайдера (R1: http.Client создаёт
// только socks, поллер получает готовый).
type UsageTarget struct {
	Provider, Account string
	BaseURL           string // origin (без суффиксов; путь добавляет поллер)
	APIKey            string
	Transporter       socks.Transporter
	Interval          time.Duration // из конфига usage_poll
}

// StartUsagePolls запускает по горутине на цель. ctx — КОНТЕКСТ СНАПШОТА
// конфигурации: релоад отменяет старый снапшот (старые поллеры умирают) и
// main стартует поллеры нового снапшота; изменившийся usage_poll подхватится
// именно так — «старые отменяются, новые стартуют» (решение по ТЗ Ф3).
func StartUsagePolls(ctx context.Context, db *DB, targets []UsageTarget) {
	for _, tg := range targets {
		if tg.Interval <= 0 || tg.Transporter == nil {
			continue
		}
		go pollUsageLoop(ctx, db, tg, nil)
	}
}

// pollUsageLoop — цикл поллера: первый снапшот сразу, далее по тикеру.
// done — тестовый хук (nil в проде): сигнал после каждой попытки полла, чтобы
// тест ждал результат по каналу, а не time.Sleep (CODING 1.9). Отправка
// хука защищена ctx: если потребитель перестал читать, горутина обязана
// выйти, а не зависнуть на блокирующей посылке (CC-1).
func pollUsageLoop(ctx context.Context, db *DB, tg UsageTarget, done chan struct{}) {
	t := time.NewTicker(tg.Interval)
	defer t.Stop()
	pollUsageOnce(ctx, db, tg)
	if !notifyDone(ctx, done) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pollUsageOnce(ctx, db, tg)
			if !notifyDone(ctx, done) {
				return
			}
		}
	}
}

func notifyDone(ctx context.Context, done chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case done <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// PollUsageOnce — один GET {base}/api/usage?range=24h (SPEC 7.3) через клиент
// аккаунта; результат — снапшот kind ollama_usage (used_percent = usage_usd).
// Отдельный экспортированный метод: детерминированный тест без ожиданий тикера.
func (d *DB) PollUsageOnce(ctx context.Context, tg UsageTarget) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(tg.BaseURL, "/")+"/api/usage?range=24h", nil)
	if err != nil {
		return err
	}
	// OA-10: ollama cloud принимает только Authorization: Bearer (не x-api-key).
	req.Header.Set("Authorization", "Bearer "+tg.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := tg.Transporter.Client().Do(req)
	if err != nil {
		return fmt.Errorf("usage poll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("usage poll: status %d: %s", resp.StatusCode, body)
	}
	var usage struct {
		UsageUSD          float64 `json:"usage_usd"`
		CachedInputTokens int64   `json:"cached_input_tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&usage); err != nil {
		return fmt.Errorf("usage poll: %w", err)
	}
	// cached_input_tokens колонки в SPEC 11 нет (минимальное расширение
	// отклонили) — факт полла логируется, экран Ф4 читает usage_usd из БД.
	slog.Debug("usage poll: снапшот", "provider", tg.Provider, "usage_usd", usage.UsageUSD,
		"cached_input_tokens", usage.CachedInputTokens)
	return d.Insert(SnapshotRow{
		Ts: time.Now(), Provider: tg.Provider, Account: tg.Account,
		Kind: KindOllamaUsage, UsedPercent: usage.UsageUSD,
	})
}

func pollUsageOnce(ctx context.Context, db *DB, tg UsageTarget) {
	if err := db.PollUsageOnce(ctx, tg); err != nil {
		// Полл — фоновая работа для экрана: сбой не фатален, следующий тик повторит.
		slog.Warn("usage poll: не удалось", "provider", tg.Provider, "err", err)
	}
}

// ─── zcode: поллинг программных квот z.ai (Ф8, SPEC 15) ─────────────────────

// KindZCodeUsage — снапшот поллинга /api/monitor/usage/quota/limit (U01–U06,
// research-zcode-oauth.md §4.5). Проценты от лимита Coding Plan — единственная
// метрика квоты, выражаемая колонкой used_percent без расширения схемы.
const KindZCodeUsage = "zcode_usage"

// ZCodeUsageTarget — цель поллинга квот zcode: аккаунт zcode-провайдера с
// включённым usage_poll. PATOf — callback-зависимость (в main замкнут на
// PATCache.EnsurePAT через Store): пакет quotas не знает про OAuth-цепочку
// zcode, а PAT живёт только в PAT-кеше (SPEC 3.2а/7.4). Transporter — клиент
// аккаунта (его SOCKS, R1: http.Client создаёт только socks).
type ZCodeUsageTarget struct {
	Provider, Account string
	BaseURL           string // origin бизнес-API (путь /api/monitor/... добавляет поллер)
	Transporter       socks.Transporter
	Interval          time.Duration // из конфига usage_poll
	PATOf             func(ctx context.Context) (string, error)
}

// StartZCodeUsagePolls — по горутине на цель, образец StartUsagePolls: контекст
// — КОНТЕКСТ СНАПШОТА конфигурации, релоад гасит старые поллеры и стартует
// новые (изменившийся usage_poll подхватывается именно так).
func StartZCodeUsagePolls(ctx context.Context, db *DB, targets []ZCodeUsageTarget) {
	for _, tg := range targets {
		if tg.Interval <= 0 || tg.Transporter == nil || tg.PATOf == nil {
			continue
		}
		go pollZCodeUsageLoop(ctx, db, tg, nil)
	}
}

// pollZCodeUsageLoop — цикл поллера: первый снапшот сразу, далее по тикеру.
// done — тестовый хук, как у pollUsageLoop (CODING 1.9: без time.Sleep).
func pollZCodeUsageLoop(ctx context.Context, db *DB, tg ZCodeUsageTarget, done chan struct{}) {
	t := time.NewTicker(tg.Interval)
	defer t.Stop()
	pollZCodeUsageOnce(ctx, db, tg)
	if !notifyDone(ctx, done) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pollZCodeUsageOnce(ctx, db, tg)
			if !notifyDone(ctx, done) {
				return
			}
		}
	}
}

func pollZCodeUsageOnce(ctx context.Context, db *DB, tg ZCodeUsageTarget) {
	if err := db.PollZCodeUsageOnce(ctx, tg); err != nil {
		// Полл — фоновая работа для экрана: сбой (сеть/5xx/нераспознанный ответ)
		// не фатален, следующий тик повторит; warn раз в тик — не спам-шторм.
		slog.Warn("zcode usage poll: не удалось", "provider", tg.Provider, "account", tg.Account, "err", err)
	}
}

// PollZCodeUsageOnce — один GET {base}/api/monitor/usage/quota/limit с
// Bearer <PAT> через клиент аккаунта; результат — снапшот kind zcode_usage.
// Возврат nil без вставки — легитимный skip (PATOf вернул пусто: записи аккаунта
// в Store ещё нет — OAuth не выполнялся, молча ждём первый успех).
func (d *DB) PollZCodeUsageOnce(ctx context.Context, tg ZCodeUsageTarget) error {
	pat, err := tg.PATOf(ctx)
	if err != nil {
		return fmt.Errorf("usage poll: %w", err)
	}
	if pat == "" {
		// Тихий skip, не ошибка: аккаунт ещё не проходил OAuth, долбить warn-логом
		// каждую секунду значило бы шуметь до первого входа пользователя.
		slog.Debug("zcode usage poll: PAT недоступен, тик пропущен", "provider", tg.Provider, "account", tg.Account)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(tg.BaseURL, "/")+"/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return fmt.Errorf("usage poll: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")
	resp, err := tg.Transporter.Client().Do(req)
	if err != nil {
		return fmt.Errorf("usage poll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("usage poll: status %d: %s", resp.StatusCode, body)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&env); err != nil {
		return fmt.Errorf("usage poll: ответ не парсится: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("usage poll: бизнес-код %d: %s", env.Code, env.Msg)
	}
	pct, ok := findZCodePercent(env.Data)
	if !ok {
		// Мусор вместо квоты — строку НЕ пишем (0% выглядел бы как «всё свободно»),
		// warn уйдёт раз в тик из pollZCodeUsageOnce.
		return fmt.Errorf("usage poll: процент в ответе не распознан (см. Debug-лог формы)")
	}
	slog.Debug("zcode usage poll: снапшот", "provider", tg.Provider, "used_percent", pct, "форма", env.Data)
	return d.Insert(SnapshotRow{
		Ts: time.Now(), Provider: tg.Provider, Account: tg.Account,
		Kind: KindZCodeUsage, UsedPercent: pct,
	})
}

// zcodePercentKeys — приоритет имён процента в data. Канон — used_percent
// (решение Ф8: форма стаба smoke/тестов и самое очевидное имя поля), остальные —
// толерантность к недокументированному wire-формату (research §4.5 ⚠).
var zcodePercentKeys = []string{"used_percent", "usage_percent", "percent"}

// maxZCodeDepth — кап рекурсии поиска в вложенных data (защита от глубокого
// мусора; реальные формы квот — объект/массив объектов).
const maxZCodeDepth = 6

// findZCodePercent — толерантный поиск процента в распарсенном data:
//   - процентное поле по приоритету zcodePercentKeys (число или строка вида
//     "42.5" / "42.5%");
//   - пара used/limit (оба числа, limit>0) → used/limit*100;
//   - массив → первый распознанный элемент; вложенные объекты — рекурсивно.
//
// Одиночный used без limit процентом не считается (может быть счётчиком
// запросов/токенов) — пишем только честные проценты.
func findZCodePercent(v any) (float64, bool) {
	return findZCodePercentDepth(v, 0)
}

func findZCodePercentDepth(v any, depth int) (float64, bool) {
	if depth > maxZCodeDepth {
		return 0, false
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range zcodePercentKeys {
			if n, ok := zcodeNumber(x[k]); ok {
				return n, true
			}
		}
		if used, uok := zcodeNumber(x["used"]); uok {
			if limit, lok := zcodeNumber(x["limit"]); lok && limit > 0 {
				return used / limit * 100, true
			}
		}
		// Ключи карты в Go перебираются недетерминированно — сортируем,
		// чтобы при нескольких кандидатах ответ не зависел от хеша.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ok := findZCodePercentDepth(x[k], depth+1); ok {
				return p, true
			}
		}
	case []any:
		for _, el := range x {
			if p, ok := findZCodePercentDepth(el, depth+1); ok {
				return p, true
			}
		}
	}
	return 0, false
}

// zcodeNumber — число или строка, парсящаяся как число (сервер может прислать
// "42.5" или "42.5%" — толерантность к недокументированному формату). Целые
// тоже принимаются: json в Go даёт float64, но парсер может получить и
// предраспарсенные целые значения.
func zcodeNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSuffix(strings.TrimSpace(n), "%")
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}
