// Пакет internal/errorslog — SQLite-дампы ошибок (таблица errors, SPEC 10):
// маскировка секретов в заголовках ДО записи, кап тел 1 МБ, ретеншн.
// Про HTTP-сервер пакет не знает ничего (R4). Эталон —
// docs/fewshots/accounting-usage.md.
package errorslog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// maxDump — кап одного тела дампа (SPEC 10): полные тела ошибок нужны для
// разбора, но гигабайтные ответы в БД писать нельзя.
const maxDump = 1 << 20 // 1 МБ

// dsnPragmas — та же строка подключения, что у accounting: одна общая точка
// открытия SQLite (WAL + busy_timeout, fewshot «Обязательства»).
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

// schema — миграция при старте (SPEC 10 посимвольно: поля, индекс ts).
const schema = `
CREATE TABLE IF NOT EXISTS errors (
  id INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,
  client_key TEXT, alias TEXT, provider TEXT, account_id TEXT,
  stage TEXT,               -- 'socks_check' | 'auth' | 'upstream' | 'stream' | 'translate'
  status INTEGER,
  request_method TEXT, request_url TEXT,
  request_headers TEXT,     -- JSON; Authorization/x-api-key/api-key маскируются
  request_body BLOB,        -- кап 1 МБ
  response_headers TEXT,
  response_body BLOB,       -- кап 1 МБ
  error TEXT
);
CREATE INDEX IF NOT EXISTS idx_errors_ts ON errors(ts);
`

// ErrorRow — полный дамп неудачного запроса.
type ErrorRow struct {
	Ts                                  time.Time
	ClientKey, Alias, Provider, Account string
	Stage                               string // socks_check | auth | upstream | stream | translate
	Status                              int
	Method, URL                         string
	ReqHeaders, RespHeaders             []byte // JSON; секреты замаскированы ДО передачи сюда
	ReqBody, RespBody                   []byte
	Err                                 string
}

// secretHeaders — что маскировать (SPEC 10 / ARCHITECTURE §10). Ключ — нижний
// регистр: заголовки приходят канонизированными, но сравниваем лениво.
var secretHeaders = map[string]struct{}{
	"authorization": {},
	"x-api-key":     {},
	"api-key":       {},
	"cookie":        {},
}

// MaskedHeaders сериализует заголовки, затирая секретные значения "<masked>".
// Маскировка происходит на стороне вызова (server/upstream), до записи — чтобы
// сырые секреты не существовали даже в промежуточных структурах (fewshot).
func MaskedHeaders(h http.Header) []byte {
	out := http.Header{}
	for k, vs := range h {
		out[k] = append([]string(nil), vs...) // копия: вход не трогаем
	}
	for k := range out {
		if _, secret := secretHeaders[strings.ToLower(k)]; secret {
			out.Set(k, "<masked>")
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		// http.Header marshaling'у не поддаётся только при невалидных UTF-8
		// значениях: дамп важнее строгой валидности — пишем ошибку строкой.
		return []byte(fmt.Sprintf(`{"marshal error":%q}`, err.Error()))
	}
	return b
}

// trunc — кап тела с маркером обрезки. Именован не cap (затенял бы встроенный
// cap — обязательство fewshot).
func trunc(b []byte) []byte {
	if len(b) > maxDump {
		return append(b[:maxDump:maxDump], []byte("...<truncated>")...)
	}
	return b
}

// DB — соединение с SQLite (таблица errors). Потокобезопасно.
type DB struct{ db *sql.DB }

// Open открывает/создаёт БД и накатывает миграцию.
func Open(path string) (*DB, error) {
	d, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("errorslog schema: %w", err)
	}
	return &DB{db: d}, nil
}

// Close чекпоинтит WAL и закрывает БД (SPEC 3.3) — см. accounting.DB.Close.
func (d *DB) Close() error {
	if _, err := d.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		err2 := d.db.Close()
		return fmt.Errorf("wal checkpoint: %v (close: %v)", err, err2)
	}
	return d.db.Close()
}

// InsertError пишет дамп. Тела капаются внутри (trunc): вызывающий может
// передать сырые байты, БД не раздуется.
func (d *DB) InsertError(r ErrorRow) error {
	_, err := d.db.Exec(`INSERT INTO errors
		(ts, client_key, alias, provider, account_id, stage, status,
		 request_method, request_url, request_headers, request_body,
		 response_headers, response_body, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Ts.UTC().Format(time.RFC3339Nano), r.ClientKey, r.Alias, r.Provider, r.Account, r.Stage, r.Status,
		r.Method, r.URL, trunc(r.ReqHeaders), trunc(r.ReqBody), trunc(r.RespHeaders), trunc(r.RespBody), r.Err)
	return err
}

// CleanupOlder — ретеншн из WebUI (⑤, AC-9). Плейсхолдер, никаких склеек.
func (d *DB) CleanupOlder(cutoff time.Time) (int64, error) {
	res, err := d.db.Exec(`DELETE FROM errors WHERE ts < ?`, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
