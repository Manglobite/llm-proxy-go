// GET /api/usage?from&to&group_by&client_key&alias&model&account — экран ④
// «Дашборд токенов» (SPEC 12.4): сводка, столбцы по дням, разрез по алиасам
// (group_by: day|alias|model|account меняет группировку строк byAlias) с
// фильтрами-плейсхолдерами. Формы — литералы mock.js (DEMO_USAGE);
// человекочитаемые значения («2.9M», «71%», «$0.38») считает webui, SQL — accounting.
package webui

import (
	"net/http"
	"strconv"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
)

// usageSummary — элемент сводки (форма mock.js: value/label/unit?).
type usageSummary struct {
	Value string  `json:"value"`
	Label string  `json:"label"`
	Unit  *string `json:"unit,omitempty"`
}

// usageAliasRow — строка разреза (форма mock.js byAlias): человекочитаемые строки.
type usageAliasRow struct {
	Alias  string `json:"alias"`
	Model  string `json:"model"`
	Req    int64  `json:"req"`
	Prompt string `json:"prompt"`
	Cached string `json:"cached"`
	Hit    int64  `json:"hit"`
	Compl  string `json:"compl"`
	Reason string `json:"reason"`
	Usd    string `json:"usd"`
	Cr     string `json:"cr"`
}

// usageFilters — наборы значений фильтров для селектов (форма mock.js).
type usageFilters struct {
	Client []string `json:"client"`
	Alias  []string `json:"alias"`
	Period []string `json:"period"`
}

type usageResponse struct {
	Summary []usageSummary  `json:"summary"`
	Days    [][]any         `json:"days"`
	ByAlias []usageAliasRow `json:"byAlias"`
	Filters usageFilters    `json:"filters"`
}

// Допустимые значения group_by (SPEC 12.4): день — отдельный агрегат accounting,
// остальные — группировка строк byAlias.
var usageGroupByValues = map[string]struct{}{
	"day": {}, "alias": {}, "model": {}, "account": {},
}

// handleUsage — дашборд. Дефолт окна — 14 дней (подписи сводки mock.js:
// «prompt за 14 дней»); from/to переопределяют. days и byAlias отдаются всегда
// (форма mock.js), group_by выбирает колонку разреза byAlias.
func (h *Handler) handleUsage(w http.ResponseWriter, r *http.Request) {
	if h.deps.Accounting == nil {
		writeJSON(w, usageResponse{
			Summary: []usageSummary{}, Days: [][]any{}, ByAlias: []usageAliasRow{},
			Filters: h.usageFilters(h.snap()),
		})
		return
	}
	now := h.now()
	q := r.URL.Query()

	from := now.Add(-14 * 24 * time.Hour)
	to := now
	if v := q.Get("from"); v != "" {
		t, err := parseQueryTime(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "from: "+err.Error())
			return
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := parseQueryTime(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "to: "+err.Error())
			return
		}
		to = t
	}
	if !from.Before(to) {
		writeJSONError(w, http.StatusBadRequest, "окно пустое: from >= to")
		return
	}

	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "alias"
	}
	if _, ok := usageGroupByValues[groupBy]; !ok {
		writeJSONError(w, http.StatusBadRequest, "group_by: допустимо day|alias|model|account")
		return
	}
	filter := accounting.UsageFilter{
		ClientKey: q.Get("client_key"),
		Alias:     q.Get("alias"),
		Model:     q.Get("model"),
		Account:   q.Get("account"),
	}

	totals, err := h.deps.Accounting.UsageTotals(from, to, filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	where, wargs := usageWhere(filter)
	days, err := h.deps.Accounting.UsageDaily(from, to, where, wargs...)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Строки разреза всегда по алиасам (форма byAlias mock.js); group_by=model/
	// account меняет содержимое ключа — SQL-разрез тем же методом.
	groupCol := groupBy
	if groupCol == "day" {
		groupCol = "alias" // day группирует график; таблица остаётся по алиасам
	}
	groups, err := h.deps.Accounting.UsageByGroup(from, to, groupCol, filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, usageResponse{
		Summary: buildSummary(from, to, totals),
		Days:    daysRows(days),
		ByAlias: groupRows(h.snap().Cfg.Models, groups),
		Filters: h.usageFilters(h.snap()),
	})
}

// buildSummary — 4 стат-плашки сводки (формы значений — mock.js).
func buildSummary(from, to time.Time, t accounting.UsageTotals) []usageSummary {
	days := int(to.Sub(from).Hours() / 24)
	if days < 1 {
		days = 1
	}
	unit := "%"
	var hit int64
	if t.Prompt > 0 {
		hit = t.Cached * 100 / t.Prompt
	}
	return []usageSummary{
		{Value: humanTokens(t.Prompt), Label: "prompt за " + strconv.Itoa(days) + " дней"},
		{Value: strconv.FormatInt(hit, 10), Label: "cache hit rate", Unit: &unit},
		{Value: "$" + fmtF(t.CostUSD, 2), Label: "оценка стоимости (PAYG)"},
		{Value: humanTokens(int64(t.GLMCredits)), Label: "glm-кредиты за " + strconv.Itoa(days) + " дней"},
	}
}

// daysRows — дневные агрегаты → [метка «24.10», prompt тыс., cached тыс., completion тыс.]
// (тысячи — единицы оси графика эталона).
func daysRows(days []accounting.UsageDaily) [][]any {
	out := make([][]any, 0, len(days))
	for _, d := range days {
		out = append(out, []any{dayLabel(d.Day), d.Prompt / 1000, d.Cached / 1000, d.Completion / 1000})
	}
	return out
}

// dayLabel — «2026-10-24» → «24.10» (метки оси графика, mock.js).
func dayLabel(day string) string {
	if len(day) >= 10 {
		return day[8:10] + "." + day[5:7]
	}
	return day
}

// groupRows — строки разреза → форма byAlias. Подписочная модель — «подписка»
// вместо $ (AC-6); нулевые значения — «—».
func groupRows(models map[string]config.ModelConfig, rows []accounting.UsageGroupRow) []usageAliasRow {
	out := make([]usageAliasRow, 0, len(rows))
	for _, r := range rows {
		row := usageAliasRow{
			Alias:  r.Key,
			Model:  r.Model,
			Req:    r.Requests,
			Prompt: humanTokens(r.Prompt),
			Cached: humanTokens(r.Cached),
			Compl:  humanTokens(r.Completion),
			Reason: "—",
			Usd:    "—",
			Cr:     "—",
		}
		if r.Prompt > 0 {
			row.Hit = r.Cached * 100 / r.Prompt
		}
		if r.Reasoning > 0 {
			row.Reason = humanTokens(r.Reasoning)
		}
		if m, ok := models[r.Model]; ok && m.Subscription {
			row.Usd = "подписка"
		} else if r.CostUSD > 0 {
			row.Usd = "$" + fmtF(r.CostUSD, 2)
		}
		if r.GLMCredits > 0 {
			row.Cr = fmtIntRu(int64(r.GLMCredits))
		}
		out = append(out, row)
	}
	return out
}

// usageFilters — наборы селектов: клиенты из истории и конфига, алиасы из конфига,
// период — константы эталона (перерезает график на клиенте).
func (h *Handler) usageFilters(snap *config.Snapshot) usageFilters {
	f := usageFilters{
		Client: []string{"все"},
		Alias:  []string{"все"},
		Period: []string{"14 дней", "7 дней", "30 дней"},
	}
	clients := make(map[string]struct{}, len(snap.Cfg.Server.Clients))
	for name := range snap.Cfg.Server.Clients {
		clients[name] = struct{}{}
	}
	if h.deps.Accounting != nil {
		if names, err := h.deps.Accounting.UsageClientNames(); err == nil {
			for _, name := range names {
				clients[name] = struct{}{}
			}
		}
	}
	f.Client = append(f.Client, sortedKeys(clients)...)
	for _, name := range sortedKeys(snap.Cfg.Aliases) {
		f.Alias = append(f.Alias, name)
	}
	return f
}

// parseQueryTime — RFC3339 или дата «YYYY-MM-DD» (UTC, начало дня).
func parseQueryTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, &badTimeError{v}
}

type badTimeError struct{ v string }

func (e *badTimeError) Error() string {
	return "время " + e.v + ": ожидается RFC3339 или дата YYYY-MM-DD"
}

// usageWhere — фильтр → (доп. WHERE-фрагмент, аргументы) для UsageDaily.
// Значения — только через плейсхолдеры, фрагмент — из констант (CODING 1.7).
func usageWhere(f accounting.UsageFilter) (string, []any) {
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
