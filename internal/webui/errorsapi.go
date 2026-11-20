// GET /api/errors?from&to&limit&offset, GET /api/errors/{id},
// POST /api/errors/cleanup {"older_than"} — экран ⑤ «Ошибки» (SPEC 12.5).
// Список — без тел дампа (CODING 2.5: пагинация на сервере); полный дамп — по id.
// Формы — литералы mock.js (DEMO_ERRORS / dump).
package webui

import (
	"net/http"
	"strconv"
	"time"
)

// errorListRow — строка списка (форма mock.js; id — добавка для GET /api/errors/{id}).
type errorListRow struct {
	ID     int64  `json:"id"`
	Ts     string `json:"ts"`
	Stage  string `json:"stage"`
	Status int    `json:"status"`
	Alias  string `json:"alias"`
	Err    string `json:"err"`
}

// errorDump — вложение полного дампа (форма mock.js: reqHeaders/reqBody — строки).
type errorDump struct {
	Method      string `json:"method"`
	URL         string `json:"url"`
	ReqHeaders  string `json:"reqHeaders"`
	ReqBody     string `json:"reqBody"`
	RespHeaders string `json:"respHeaders"`
	RespBody    string `json:"respBody"`
}

// errorDetailRow — форма GET /api/errors/{id}: строка списка + dump (или null).
type errorDetailRow struct {
	ID     int64      `json:"id"`
	Ts     string     `json:"ts"`
	Stage  string     `json:"stage"`
	Status int        `json:"status"`
	Alias  string     `json:"alias"`
	Err    string     `json:"err"`
	Dump   *errorDump `json:"dump"`
}

// handleErrorsList — страница списка: свежие сверху, окно [from,to),
// limit (дефолт 100, кап 500) / offset. from/to — RFC3339 или дата.
func (h *Handler) handleErrorsList(w http.ResponseWriter, r *http.Request) {
	if h.deps.ErrorsLog == nil {
		writeJSON(w, []errorListRow{})
		return
	}
	now := h.now()
	q := r.URL.Query()

	from := now.Add(-30 * 24 * time.Hour) // дефолт окна: месяц (решение Ф4, задокументировано)
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
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "limit: ожидается положительное число")
			return
		}
		limit = n
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "offset: ожидается неотрицательное число")
			return
		}
		offset = n
	}

	rows, err := h.deps.ErrorsLog.List(from, to, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]errorListRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, errorListRow{
			ID: r.ID, Ts: fmtTs(r.Ts), Stage: r.Stage, Status: r.Status,
			Alias: r.Alias, Err: r.Err,
		})
	}
	writeJSON(w, out)
}

// handleErrorsDetail — полный дамп по id; нет записи → 404 (JSON).
func (h *Handler) handleErrorsDetail(w http.ResponseWriter, r *http.Request) {
	if h.deps.ErrorsLog == nil {
		writeJSONError(w, http.StatusNotFound, "ошибка не найдена")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "id: ожидается число")
		return
	}
	det, ok, err := h.deps.ErrorsLog.Get(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "ошибка не найдена")
		return
	}
	row := errorDetailRow{
		ID: det.ID, Ts: fmtTs(det.Ts), Stage: det.Stage, Status: det.Status,
		Alias: det.Alias, Err: det.Err,
	}
	// dump — только если дамп запроса/ответа сохранился (эталон: dump:null для
	// отказов до апстрима — socks_check/translate).
	if det.URL != "" || det.ReqHeaders != "" || det.RespHeaders != "" || det.ReqBody != "" || det.RespBody != "" {
		row.Dump = &errorDump{
			Method:      det.Method,
			URL:         det.URL,
			ReqHeaders:  det.ReqHeaders,
			ReqBody:     det.ReqBody,
			RespHeaders: det.RespHeaders,
			RespBody:    det.RespBody,
		}
	}
	writeJSON(w, row)
}

// cleanupRequest — тело POST /api/errors/cleanup (форма mock.js: «7d»/«30d»).
type cleanupRequest struct {
	OlderThan string `json:"older_than"`
}

// handleErrorsCleanup — ретеншн (AC-9): older_than — длительность («7d», «24h»,
// «30d») или RFC3339-момент. Ответ {removed: N} (форма mock.js).
func (h *Handler) handleErrorsCleanup(w http.ResponseWriter, r *http.Request) {
	if h.deps.ErrorsLog == nil {
		writeJSON(w, map[string]int64{"removed": 0})
		return
	}
	var req cleanupRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.OlderThan == "" {
		writeJSONError(w, http.StatusBadRequest, "older_than: ожидается длительность (7d) или RFC3339")
		return
	}
	cutoff, err := parseOlderThan(req.OlderThan, h.now())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "older_than: "+err.Error())
		return
	}
	removed, err := h.deps.ErrorsLog.CleanupOlder(cutoff)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"removed": removed})
}

// parseOlderThan — «Nd»/«Nw» (многозначно: «7d», «30d», «2w»), Go-duration
// («24h») или RFC3339. Суффиксы d/w не в time.ParseDuration — парсим руками
// (F4-fix1 F1: прежний однозначный вариант отбрасывал «30d» из селекта фронта).
func parseOlderThan(v string, now time.Time) (time.Time, error) {
	if len(v) >= 2 {
		switch suf := v[len(v)-1]; suf {
		case 'd', 'w':
			n, err := strconv.Atoi(v[:len(v)-1])
			if err != nil || n <= 0 {
				return time.Time{}, &badTimeError{v}
			}
			if suf == 'd' {
				return now.AddDate(0, 0, -n), nil
			}
			return now.AddDate(0, 0, -7*n), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, &badTimeError{v}
}
