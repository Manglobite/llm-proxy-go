// Агрегатные чтения для WebUI-дашборда (Ф4, SPEC 12.4): фильтры — только
// плейсхолдерами (CODING 1.7), формы ответов собирает webui. Все методы
// ДОБАВЛЯЮТ чтения — поведение записи (InsertRequest) не меняется.
package accounting

import (
	"fmt"
	"time"
)

// UsageFilter — фильтры дашборда (SPEC 12.4: client_key/alias/model/account).
// Пустое поле — без фильтра. Значения попадают в SQL только через плейсхолдеры.
type UsageFilter struct {
	ClientKey string
	Alias     string
	Model     string
	Account   string
}

// clause — дополнительный WHERE-фрагмент ("AND …") и аргументы плейсхолдеров.
// Фрагмент собирается из констант, пользовательский ввод — только значениями (CODING 1.7).
func (f UsageFilter) clause() (string, []any) {
	var conds []string
	var args []any
	if f.ClientKey != "" {
		conds = append(conds, "client_key = ?")
		args = append(args, f.ClientKey)
	}
	if f.Alias != "" {
		conds = append(conds, "alias = ?")
		args = append(args, f.Alias)
	}
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.Account != "" {
		conds = append(conds, "account_id = ?")
		args = append(args, f.Account)
	}
	if len(conds) == 0 {
		return "", nil
	}
	out := "AND " + conds[0]
	for _, c := range conds[1:] {
		out += " AND " + c
	}
	return out, args
}

// UsageTotals — итоги за окно [from, to) с фильтром: сводка дашборда
// (prompt/cached → cache hit rate, оценка $, glm-кредиты).
type UsageTotals struct {
	Requests   int64
	Prompt     int64
	Completion int64
	Cached     int64
	Reasoning  int64
	CostUSD    float64
	GLMCredits float64
}

// UsageTotals — один SELECT с SUM по таблице requests.
func (d *DB) UsageTotals(from, to time.Time, f UsageFilter) (UsageTotals, error) {
	where, args := f.clause()
	var t UsageTotals
	err := d.db.QueryRow(`SELECT COUNT(*),
	       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
	       COALESCE(SUM(cached_input_tokens),0), COALESCE(SUM(reasoning_tokens),0),
	       COALESCE(SUM(cost_usd),0), COALESCE(SUM(glm_credits),0)
	  FROM requests WHERE ts >= ? AND ts < ? `+where,
		append([]any{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}, args...)...,
	).Scan(&t.Requests, &t.Prompt, &t.Completion, &t.Cached, &t.Reasoning, &t.CostUSD, &t.GLMCredits)
	return t, err
}

// UsageGroupRow — строка разреза по alias|model|account (group_by дашборда).
// Key — значение группирующей колонки; Model — модель апстрима (полезна в
// разрезе алиасов, в прочих разрезах совпадает с Key или пуста).
type UsageGroupRow struct {
	Key        string
	Model      string
	Requests   int64
	Prompt     int64
	Completion int64
	Cached     int64
	Reasoning  int64
	CostUSD    float64
	GLMCredits float64
}

// Разрезы group_by, поддержанные SQL (SPEC 12.4). Сортировка — по количеству
// запросов вниз: дашборд показывает самые нагруженные строки сверху.
var usageGroupColumns = map[string]string{
	"alias":   "alias",
	"model":   "model",
	"account": "account_id",
}

// UsageByGroup — агрегат по одной колонке с фильтром. by — ключ из
// usageGroupColumns ("alias"|"model"|"account"); неизвестное значение — ошибка
// (валидация group_by на границе webui, здесь страховка от SQL-инъекции).
func (d *DB) UsageByGroup(from, to time.Time, by string, f UsageFilter) ([]UsageGroupRow, error) {
	col, ok := usageGroupColumns[by]
	if !ok {
		return nil, fmt.Errorf("accounting: неизвестный group_by %q", by)
	}
	where, args := f.clause()
	rows, err := d.db.Query(`SELECT `+col+`, model, COUNT(*),
	       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
	       COALESCE(SUM(cached_input_tokens),0), COALESCE(SUM(reasoning_tokens),0),
	       COALESCE(SUM(cost_usd),0), COALESCE(SUM(glm_credits),0)
	  FROM requests WHERE ts >= ? AND ts < ? `+where+`
	  GROUP BY `+col+` ORDER BY 3 DESC`,
		append([]any{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}, args...)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageGroupRow
	for rows.Next() {
		var r UsageGroupRow
		if err := rows.Scan(&r.Key, &r.Model, &r.Requests, &r.Prompt, &r.Completion,
			&r.Cached, &r.Reasoning, &r.CostUSD, &r.GLMCredits); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) UsageClientNames() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT client_key FROM requests WHERE client_key <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// UsageClientRow — сводка по клиентскому ключу (экран ①: «запросов 24ч»,
// «токенов 24ч» = prompt+completion).
type UsageClientRow struct {
	ClientKey string
	Requests  int64
	Tokens    int64
}

// UsageByClient — агрегат по client_key за окно (обычно 24ч). Сортировка по
// числу запросов вниз.
func (d *DB) UsageByClient(from, to time.Time) ([]UsageClientRow, error) {
	rows, err := d.db.Query(`SELECT client_key, COUNT(*),
	       COALESCE(SUM(prompt_tokens + completion_tokens),0)
	  FROM requests WHERE ts >= ? AND ts < ?
	  GROUP BY client_key ORDER BY 2 DESC`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageClientRow
	for rows.Next() {
		var r UsageClientRow
		if err := rows.Scan(&r.ClientKey, &r.Requests, &r.Tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByAliasCount — количество запросов по алиасам за окно (экран ②:
// «запросов 24ч» у каждой строки алиаса). Отдельный лёгкий метод: экрану
// алиасов нужен только счётчик, без остальной экономики.
func (d *DB) UsageByAliasCount(from, to time.Time) (map[string]int64, error) {
	rows, err := d.db.Query(`SELECT alias, COUNT(*) FROM requests
	  WHERE ts >= ? AND ts < ? GROUP BY alias`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var alias string
		var n int64
		if err := rows.Scan(&alias, &n); err != nil {
			return nil, err
		}
		out[alias] = n
	}
	return out, rows.Err()
}
