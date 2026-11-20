// Обвязка JSON API: Bearer-гард webui_token (SV-5), JSON-конверты ошибок и
// форматтеры человекочитаемых значений (формы — mock.js, SPEC 12).
package webui

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/socks"
)

// guard — обёртка хендлера /api/*: метод + auth. webui_token задан → всё
// /api/* закрыто Bearer-токеном (SV-5, сравнение за постоянное время);
// не задан → без авторизации (CF-5). /healthz и статика гард не проходят.
func (h *Handler) guard(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: "+method)
			return
		}
		if token := h.deps.Snapshot().Cfg.Server.WebUIToken; token != "" && !validWebUIToken(r, token) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func validWebUIToken(r *http.Request, token string) bool {
	provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(provided)), []byte(token)) == 1
}

// writeJSON — стандартный ответ 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError — JSON-конверт ошибки (SPEC 4: ошибки — всегда JSON, никогда HTML).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// readJSON — декодирование тела с капом (CODING 1.3): OAuth/ретеншн-тела —
// единицы килобайт, 64 КБ с запасом.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "не удалось прочитать тело: "+err.Error())
		return false
	}
	if len(body) > 64*1024 {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "тело запроса больше 64 КБ")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "невалидный JSON: "+err.Error())
		return false
	}
	return true
}

func (h *Handler) configCreate(field string, add func(string, string, json.RawMessage) error) http.HandlerFunc {
	return h.configAction(field, add != nil, func(w http.ResponseWriter, r *http.Request, token, name string, data json.RawMessage) error {
		if err := add(token, name, data); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
}

func (h *Handler) configAction(field string, enabled bool, action func(http.ResponseWriter, *http.Request, string, string, json.RawMessage) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		tokenConfigured := h.snap().Cfg.Server.WebUIToken
		if tokenConfigured == "" {
			writeJSONError(w, http.StatusForbidden, config.ErrConfigDisabled.Error())
			return
		}
		if !validWebUIToken(r, tokenConfigured) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !enabled {
			writeJSONError(w, http.StatusServiceUnavailable, "запись конфигурации недоступна")
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			writeJSONError(w, http.StatusUnsupportedMediaType, "требуется application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		var payload struct {
			Name     string          `json:"name"`
			Provider json.RawMessage `json:"provider"`
			Alias    json.RawMessage `json:"alias"`
			Socks    json.RawMessage `json:"socks"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&payload)
		if err == nil {
			switch next := decoder.Decode(new(any)); next {
			case io.EOF:
			case nil:
				err = config.ErrConfigInvalid
			default:
				err = next
			}
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "тело запроса больше 64 КБ")
				return
			}
			writeJSONError(w, http.StatusBadRequest, "невалидный JSON")
			return
		}
		fields := map[string]json.RawMessage{"provider": payload.Provider, "alias": payload.Alias, "socks": payload.Socks}
		data := fields[field]
		delete(fields, field)
		for _, extra := range fields {
			if len(extra) != 0 {
				writeJSONError(w, http.StatusBadRequest, "неверная форма запроса")
				return
			}
		}
		if field != "" && len(data) == 0 {
			writeJSONError(w, http.StatusBadRequest, "неверная форма запроса")
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		err = action(w, r, strings.TrimSpace(token), payload.Name, data)
		switch {
		case err == nil:
			return
		case errors.Is(err, config.ErrConfigAuth):
			writeJSONError(w, http.StatusUnauthorized, config.ErrConfigAuth.Error())
		case errors.Is(err, config.ErrConfigDisabled):
			writeJSONError(w, http.StatusForbidden, config.ErrConfigDisabled.Error())
		case errors.Is(err, config.ErrConfigConflict):
			writeJSONError(w, http.StatusConflict, config.ErrConfigConflict.Error())
		case errors.Is(err, config.ErrConfigInvalid):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, "не удалось сохранить config.json; проверьте права записи")
		}
	}
}

func (h *Handler) createClient(w http.ResponseWriter, _ *http.Request, token, name string, _ json.RawMessage) error {
	key, err := h.deps.AddClient(token, name)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]string{"key": key})
	return nil
}

func checkSocks(w http.ResponseWriter, r *http.Request, _, name string, data json.RawMessage) error {
	cfg, err := config.ParseSocksEntry(name, data)
	if err != nil {
		return err
	}
	status, err := socks.Check(r.Context(), cfg)
	if err != nil {
		message := "Не удалось проверить IP через SOCKS: проверьте адрес, авторизацию и URL проверки"
		if strings.HasPrefix(status.LastError, "egress ip mismatch:") {
			message = "Фактический IP не совпадает с ожидаемым"
		}
		writeJSON(w, map[string]any{"ok": false, "ip": status.ActualIP, "error": message})
		return nil
	}
	writeJSON(w, map[string]any{"ok": true, "ip": status.ActualIP})
	return nil
}

func (h *Handler) recheckSocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	snap := h.snap()
	token := snap.Cfg.Server.WebUIToken
	if token == "" {
		writeJSONError(w, http.StatusForbidden, "для ручной проверки задайте server.webui_token")
		return
	}
	if !validWebUIToken(r, token) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &payload) {
		return
	}
	path, ok := snap.Socks[payload.Name]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "SOCKS-путь не найден")
		return
	}
	err := path.Recheck(r.Context())
	if errors.Is(err, socks.ErrCheckInProgress) {
		writeJSONError(w, http.StatusConflict, "SOCKS-путь уже проверяется")
		return
	}
	if current := h.snap().Socks[payload.Name]; current != path {
		writeJSONError(w, http.StatusConflict, "SOCKS-путь изменился во время проверки; повторите её")
		return
	}
	writeJSON(w, map[string]bool{"ok": err == nil})
}

// now — точка времени с тестовой подменой.
func (h *Handler) now() time.Time { return h.deps.Now() }

// snap — текущий снапшот конфигурации.
func (h *Handler) snap() *config.Snapshot { return h.deps.Snapshot() }

// ─── форматтеры (формы строк — mock.js) ─────────────────────────────────────

// humanTokens — «1.9M», «980k», «412k», «96» (токены/счётчики для сводки).
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		s := strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64)
		return trimFloatZero(s) + "M"
	case n >= 1_000:
		s := strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64)
		return trimFloatZero(s) + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
}

// trimFloatZero — «1.0» → «1», «18.4» → «18.4».
func trimFloatZero(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// fmtF — FormatFloat с фиксированной точностью (шорткат форматтеров).
func fmtF(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }

// fmtIntRu — целое с разделением разрядов пробелом: 18432 → «18 432» (mock).
func fmtIntRu(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// fmtInterval — интервал проверки: «60 с» (mock); длинные — минутами.
func fmtInterval(d time.Duration) string {
	sec := int(d / time.Second)
	if sec < 3600 {
		return strconv.Itoa(sec) + " с"
	}
	return strconv.Itoa(sec/60) + " мин"
}

// agoText — «12 с назад» (mock); до первой проверки — «не проверялся».
func agoText(since time.Time, now time.Time) string {
	if since.IsZero() {
		return "не проверялся"
	}
	d := now.Sub(since)
	switch {
	case d < 0:
		return "0 с назад"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + " с назад"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " м назад"
	default:
		return strconv.Itoa(int(d/time.Hour)) + " ч назад"
	}
}

// levelOf — уровень заливки по проценту. В эталоне уровни расставлены вручную
// (entities/quota.js: «чистого порога нет»), рабочей версии нужен детерминизм —
// порог 50/80 зафиксирован решением Ф4 (задокументирован).
func levelOf(pct float64) string {
	switch {
	case pct >= 80:
		return "high"
	case pct >= 50:
		return "mid"
	default:
		return "ok"
	}
}

// fmtUntil — «через 2ч 21м · 14:32» (до суток) или «вс 09:17» (дальше).
func fmtUntil(resetAt time.Time, now time.Time) string {
	hhmm := resetAt.Format("15:04")
	d := resetAt.Sub(now)
	if d <= 0 {
		return hhmm
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("через %dч %02dм · %s", int(d.Hours()), int(d.Minutes())%60, hhmm)
	}
	return ruWeekday(resetAt) + " " + hhmm
}

// ruWeekday — «вс»/«пн»/… (mock: «вс 09:17»).
func ruWeekday(t time.Time) string {
	return []string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}[int(t.Weekday())]
}

// fmtTs — «2026-11-06 09:41:12» (mock: список ошибок).
func fmtTs(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

// fmtClock — «09:41» (429-события). Сегодняшние — время, вчерашние — «вчера 22:18»,
// более старые — дата и время.
func fmtClock(t time.Time, now time.Time) string {
	today := now.Local().Truncate(24 * time.Hour)
	if !t.Local().Before(today) {
		return t.Local().Format("15:04")
	}
	if !t.Local().Before(today.AddDate(0, 0, -1)) {
		return "вчера " + t.Local().Format("15:04")
	}
	return t.Local().Format("02.01 15:04")
}

// sortedKeys — детерминированный обход карт конфига (Go-карта случайна,
// а экран обязан показывать один и тот же порядок между запросами).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errText — nil-safe текст ошибки.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isErr — errors.Is-обёртка для читаемости ветвлений.
func isErr(err error, target error) bool { return errors.Is(err, target) }
