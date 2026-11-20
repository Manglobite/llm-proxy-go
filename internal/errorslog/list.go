// Чтение таблицы errors для WebUI (Ф4, SPEC 12.5): список с пагинацией
// (CODING 2.5 — большие списки режутся на сервере) и полный дамп по id.
// Пакет владеет таблицей, наружный SQL не разбросан — как Recent у quotas.
package errorslog

import (
	"database/sql"
	"fmt"
	"time"
)

// ListRow — строка списка ошибок: без тел дампа (они весят до 2 МБ на строку,
// список отдаёт только факт и текст ошибки; полный дамп — Get по id).
type ListRow struct {
	ID     int64
	Ts     time.Time
	Stage  string
	Status int
	Alias  string
	Err    string
}

// List — свежие ошибки первыми (ORDER BY id DESC), окно [from, to), limit/offset.
// limit <= 0 → 100 (дефолт страницы), > 500 → 500 (кап страницы);
// offset < 0 → 0. Значения по умолчанию — решение webui-контракта, задокументировано в отчёте Ф4.
func (d *DB) List(from, to time.Time, limit, offset int) ([]ListRow, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.db.Query(`SELECT id, ts, COALESCE(stage,''), COALESCE(status,0), COALESCE(alias,''), COALESCE(error,'')
		FROM errors WHERE ts >= ? AND ts < ?
		ORDER BY id DESC LIMIT ? OFFSET ?`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListRow
	for rows.Next() {
		var r ListRow
		var tsRaw string
		if err := rows.Scan(&r.ID, &tsRaw, &r.Stage, &r.Status, &r.Alias, &r.Err); err != nil {
			return nil, err
		}
		if r.Ts, err = parseTs(tsRaw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Detail — полный дамп одной ошибки (SPEC 10: все колонки строки errors).
// JSON-форму ответа собирает webui (форма mock.js); тела — строки
// (BLOB из SQLite читается как []byte).
type Detail struct {
	ID          int64
	Ts          time.Time
	ClientKey   string
	Alias       string
	Provider    string
	Account     string
	Stage       string
	Status      int
	Method      string
	URL         string
	ReqHeaders  string
	ReqBody     string
	RespHeaders string
	RespBody    string
	Err         string
}

// Get — дамп по id; ok=false, если записи с таким id нет (webui отвечает 404).
func (d *DB) Get(id int64) (det Detail, ok bool, err error) {
	var tsRaw string
	var reqH, respH sql.NullString
	var reqB, respB []byte
	err = d.db.QueryRow(`SELECT id, ts, COALESCE(client_key,''), COALESCE(alias,''), COALESCE(provider,''), COALESCE(account_id,''),
	       COALESCE(stage,''), COALESCE(status,0), COALESCE(request_method,''), COALESCE(request_url,''),
	       request_headers, request_body, response_headers, response_body, COALESCE(error,'')
		FROM errors WHERE id = ?`, id).
		Scan(&det.ID, &tsRaw, &det.ClientKey, &det.Alias, &det.Provider, &det.Account,
			&det.Stage, &det.Status, &det.Method, &det.URL,
			&reqH, &reqB, &respH, &respB, &det.Err)
	switch {
	case err == sql.ErrNoRows:
		return Detail{}, false, nil
	case err != nil:
		return Detail{}, false, err
	}
	ts, err := parseTs(tsRaw)
	if err != nil {
		return Detail{}, false, err
	}
	det.Ts = ts
	det.ReqHeaders, det.ReqBody = reqH.String, string(reqB)
	det.RespHeaders, det.RespBody = respH.String, string(respB)
	return det, true, nil
}

// parseTs — ts хранится строкой RFC3339Nano (InsertError); Reader'ы парсят её
// в одном месте, чтобы формат совпадал во всех выдачах.
func parseTs(tsRaw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, tsRaw)
	if err != nil {
		return time.Time{}, fmt.Errorf("errors ts %q: %w", tsRaw, err)
	}
	return t, nil
}
