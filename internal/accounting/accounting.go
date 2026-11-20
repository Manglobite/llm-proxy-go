// Пакет internal/accounting — SQLite-учёт токенов (таблица requests, SPEC 9)
// и расчёт стоимости/кредитов. Про HTTP-сервер пакет не знает ничего (R4):
// сервер пишет строки через InsertRequest после завершения запроса.
// Эталон — docs/fewshots/accounting-usage.md.
package accounting

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go драйвер: сборка CGO_ENABLED=0 (CODING 1.1)
)

// dsn — общая точка открытия SQLite (DSN с WAL-прагмами). WAL нужен, чтобы
// фоновые записи не блокировали чтения дашборда; busy_timeout — чтобы
// параллельные INSERT (requests + errors) не падали с SQLITE_BUSY.
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

// schema — миграция при старте (SPEC 9 посимвольно: поля, индексы).
// IF NOT EXISTS у индексов — миграция обязана быть идемпотентной.
const schema = `
CREATE TABLE IF NOT EXISTS requests (
  id INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,                -- RFC3339
  client_key TEXT NOT NULL,
  alias TEXT NOT NULL,
  provider TEXT NOT NULL,
  account_id TEXT NOT NULL,
  model TEXT NOT NULL,             -- модель апстрима
  protocol TEXT NOT NULL,          -- 'openai' | 'anthropic' | 'responses' (вход)
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  is_stream INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER,
  status INTEGER,
  error_kind TEXT,                 -- NULL если успех: 'network','socks_blocked','auth','upstream','translate'
  cost_usd REAL,                   -- оценка по price_usd_per_m
  glm_credits REAL                 -- кредиты Coding Plan по формуле 7.2 (для glm-аккаунтов)
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_account_ts ON requests(account_id, ts);
`

// RequestRow — строка таблицы requests (SPEC 9).
type RequestRow struct {
	Ts                 time.Time
	ClientKey, Alias   string
	Provider, Account  string
	Model              string // модель апстрима
	Protocol           string // 'openai' | 'anthropic' | 'responses' (вход)
	Prompt, Completion int
	CachedInput        int
	CacheCreation      int
	Reasoning          int
	IsStream           bool
	LatencyMS          int64
	Status             int // HTTP-статус апстрима (0 = до запроса)
	ErrorKind          string
	CostUSD            float64
	GLMCredits         float64
}

// ModelInfo — экономика модели из config.models.
type ModelInfo struct {
	PriceIn, PriceOut, PriceCached float64 // $ за 1M токенов; 0 = неизвестно
	GLMIn, GLMCached, GLMOut       float64 // мультипликаторы кредитов (на 10k токенов)
	Subscription                   bool    // плата подпиской, не за токены
}

// DB — соединение с SQLite (таблица requests). Потокобезопасно: database/sql
// сериализует через пул соединений.
type DB struct{ db *sql.DB }

// Open открывает/создаёт БД и накатывает миграцию. Ошибка схемы — фатальна
// для старта процесса: без таблицы учёт работать не может.
func Open(path string) (*DB, error) {
	d, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close() // не оставляем открытое соединение при неудачной миграции
		return nil, fmt.Errorf("accounting schema: %w", err)
	}
	return &DB{db: d}, nil
}

// Close чекпоинтит WAL (PRAGMA wal_checkpoint(TRUNCATE)) и закрывает БД:
// после остановки рядом с proxy.db не должен оставаться несобранный -wal
// (SPEC 3.3, graceful shutdown).
func (d *DB) Close() error {
	if _, err := d.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		// Чекпоинт не удался — всё равно закрываем соединение: утечка ресурса
		// хуже несобранного wal-файла (его подберёт следующий запуск).
		err2 := d.db.Close()
		return fmt.Errorf("wal checkpoint: %v (close: %v)", err, err2)
	}
	return d.db.Close()
}

// CalcCostUSD — оценка стоимости (для подписочных моделей 0: платим планом).
// uncached = prompt − cached клампится в 0: спецификация требует не отдавать
// отрицательную стоимость, даже если апстрим прислал cached > prompt
// (SPEC 9 ⚠ проверка; поведение фиксируется тестом).
func CalcCostUSD(m *ModelInfo, prompt, cached, completion int) float64 {
	if m == nil || m.Subscription {
		return 0
	}
	uncached := prompt - cached
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*m.PriceIn + float64(cached)*m.PriceCached + float64(completion)*m.PriceOut) / 1_000_000
}

// CalcGLMCredits — кредиты Coding Plan (SPEC 7.2):
// (input×input_mult + cached×cached_mult + output×output_mult) / 10000.
// В отличие от стоимости, здесь input берётся ПОЛНЫЙ (не uncached) — формула
// спеки не вычитает кеш из входа. Подписочные/не-glm модели → 0.
func CalcGLMCredits(m *ModelInfo, prompt, cached, completion int) float64 {
	if m == nil || m.GLMOut == 0 {
		return 0
	}
	return (float64(prompt)*m.GLMIn + float64(cached)*m.GLMCached + float64(completion)*m.GLMOut) / 10000
}

// InsertRequest пишет строку запроса. Вызывается один раз на завершённый
// запрос (и для успеха, и для ошибки — тогда ErrorKind != "").
// Стоимость/кредиты считаются на записи (SPEC 9): цена берётся из конфига,
// действовавшего в момент запроса.
func (d *DB) InsertRequest(r RequestRow, m *ModelInfo) error {
	r.CostUSD = CalcCostUSD(m, r.Prompt, r.CachedInput, r.Completion)
	r.GLMCredits = CalcGLMCredits(m, r.Prompt, r.CachedInput, r.Completion)
	// SPEC 9: error_kind NULL если успех — пустая строка маскировала бы
	// «успех» и «ошибка с пустым kind», а дашборд фильтрует по IS NULL.
	errorKind := sql.NullString{String: r.ErrorKind, Valid: r.ErrorKind != ""}
	_, err := d.db.Exec(`INSERT INTO requests
		(ts, client_key, alias, provider, account_id, model, protocol,
		 prompt_tokens, completion_tokens, cached_input_tokens, cache_creation_tokens,
		 reasoning_tokens, is_stream, latency_ms, status, error_kind, cost_usd, glm_credits)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Ts.UTC().Format(time.RFC3339Nano), r.ClientKey, r.Alias, r.Provider, r.Account, r.Model, r.Protocol,
		r.Prompt, r.Completion, r.CachedInput, r.CacheCreation,
		r.Reasoning, r.IsStream, r.LatencyMS, r.Status, errorKind, r.CostUSD, r.GLMCredits)
	return err
}

// UsageDaily — агрегат для дашборда (④).
type UsageDaily struct {
	Day                 string
	Requests            int
	Prompt, Completion  int64
	Cached              int64
	CostUSD, GLMCredits float64
}

// UsageDaily — дневные агрегаты за [from, to). where — ДОПОЛНИТЕЛЬНЫЙ фрагмент
// WHERE с плейсхолдерами ("AND client_key = ?") и его аргументы; фрагмент
// составляется только константами вызывающего, пользовательский ввод в него
// не подставляется (CODING 1.7: строки в SQL — только плейсхолдеры).
func (d *DB) UsageDaily(from, to time.Time, where string, args ...any) ([]UsageDaily, error) {
	q := `SELECT substr(ts,1,10), COUNT(*), SUM(prompt_tokens), SUM(completion_tokens),
	             SUM(cached_input_tokens), SUM(cost_usd), SUM(glm_credits)
	      FROM requests WHERE ts >= ? AND ts < ? ` + where + ` GROUP BY 1 ORDER BY 1`
	rows, err := d.db.Query(q, append([]any{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageDaily
	for rows.Next() {
		var u UsageDaily
		if err := rows.Scan(&u.Day, &u.Requests, &u.Prompt, &u.Completion, &u.Cached, &u.CostUSD, &u.GLMCredits); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
