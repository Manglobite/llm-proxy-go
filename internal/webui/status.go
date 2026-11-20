// GET /api/status — экран ① «Провайдеры и аккаунты» (SPEC 12.1): SOCKS-пути с
// IP-пиннингом, клиентские ключи, аккаунты со статусами токенов. Формы строк —
// литералы mock.js (DEMO_SOCKS_PATHS / DEMO_ACCOUNTS / DEMO_CLIENT_KEYS / DEMO_STATUS_META).
package webui

import (
	"fmt"
	"net/http"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/upstream"
	"llm-proxy/internal/upstream/codex"
	"llm-proxy/internal/upstream/zcode"
)

// statusSock — строка SOCKS-пути (форма mock.js).
type statusSock struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	CheckURL      string `json:"check_url,omitempty"`
	IntervalRaw   string `json:"check_interval_raw,omitempty"`
	State         string `json:"state"`
	EgressIP      string `json:"egress_ip"`
	ExpectedIP    string `json:"expected_ip"`
	Match         bool   `json:"match"`
	CheckedAgo    string `json:"checked_ago"`
	CheckInterval string `json:"check_interval"`
	Error         string `json:"error"`
}

// statusAccount — строка аккаунта (форма mock.js). Socks — null для прямых путей.
// OAuthMode — режим OAuth-модалки фронта: "pkce" (codex: paste+callback) или
// "poll" (zcode: ссылка + серверная poll-петля, SPEC 12); пусто — OAuth нет.
type statusAccount struct {
	Provider    string      `json:"provider"`
	Auth        string      `json:"auth"`
	Plan        string      `json:"plan,omitempty"`
	ID          string      `json:"id"`
	Email       string      `json:"email,omitempty"`
	Token       string      `json:"token"`
	Socks       *string     `json:"socks"`
	AllowDirect bool        `json:"allow_direct"`
	CanOAuth    bool        `json:"can_oauth"`
	OAuthMode   string      `json:"oauth_mode,omitempty"`
	Usage       statusUsage `json:"usage"`
}

// statusUsage — мини-бары строки аккаунта (pct/level) и текстовая сводка.
type statusUsage struct {
	Segments []usageSegment `json:"segments"`
	Label    string         `json:"label"`
}

type usageSegment struct {
	Pct   float64 `json:"pct"`
	Level string  `json:"level"`
}

// statusMeta — верхняя панель и футер сайдбара (форма mock.js).
type statusMeta struct {
	Version string `json:"version"`
	Listen  string `json:"listen"`
	Config  string `json:"config"`
	Uptime  string `json:"uptime"`
}

type statusResponse struct {
	Socks      []statusSock    `json:"socks"`
	Accounts   []statusAccount `json:"accounts"`
	ClientKeys []statusClient  `json:"client_keys"`
	Meta       statusMeta      `json:"meta"`
}

// statusClient — «Клиентские ключи» (форма mock.js: req24h + tokens24h).
type statusClient struct {
	Name      string `json:"name"`
	Req24h    int64  `json:"req24h"`
	Tokens24h string `json:"tokens24h"`
}

// handleStatus — сборка ответа по ЖИВОМУ снапшоту: релоад меняет состав
// аккаунтов/путей без рестарта (SPEC 3.3).
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := h.snap()
	admin := snap.Cfg.Server.WebUIToken != "" && validWebUIToken(r, snap.Cfg.Server.WebUIToken)
	if snap.Cfg.Server.WebUIToken != "" && !admin {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	now := h.now()
	// F4-fix1 F3: Recent(500) — ОДИН запрос к БД на /api/status (раньше — по
	// одному на каждый аккаунт); срез передаётся вниз всем читателям снапшотов.
	quotaRows := h.quotaRows()
	resp := statusResponse{
		Socks:      h.statusSocks(snap, now, admin),
		Accounts:   h.statusAccounts(snap, now, quotaRows),
		ClientKeys: h.statusClients(snap, now),
		Meta:       h.statusMeta(snap, now),
	}
	writeJSON(w, resp)
}

// statusSocks — пути из снапшота (socks.Status(): name/state/expected/actual/
// last_check/last_error) + человекочитаемые производные.
func (h *Handler) statusSocks(snap *config.Snapshot, now time.Time, admin bool) []statusSock {
	out := make([]statusSock, 0, len(snap.Cfg.Socks))
	for _, name := range sortedKeys(snap.Cfg.Socks) {
		sc := snap.Cfg.Socks[name]
		row := statusSock{
			Name:          name,
			URL:           config.MaskURL(sc.URL),
			CheckInterval: fmtInterval(sc.CheckInterval),
		}
		if admin {
			row.URL = sc.URL
			row.CheckURL = sc.CheckURL
			row.IntervalRaw = sc.CheckInterval.String()
		}
		p, ok := snap.Socks[name]
		if !ok {
			// Рассинхрон конфиг/снапшот невозможен по построению Manager, но
			// fail-visible: путь показывается заблокированным с причиной.
			row.State, row.Error = "blocked", "путь отсутствует в снапшоте"
			out = append(out, row)
			continue
		}
		st := p.Status()
		row.State = stateText(st.State)
		row.EgressIP = st.ActualIP
		row.ExpectedIP = st.ExpectedIP
		// match: expected не задан → пиннинга нет, нечему расходиться (mock: true).
		row.Match = st.ExpectedIP == "" || row.State == "ok"
		if lc, err := time.Parse(time.RFC3339, st.LastCheck); err == nil && st.LastCheck != "" {
			row.CheckedAgo = agoText(lc, now)
		} else {
			row.CheckedAgo = agoText(time.Time{}, now)
		}
		row.Error = st.LastError
		if row.State == "blocked" {
			row.CheckInterval = "автоповтор " + fmtInterval(sc.CheckInterval)
		}
		out = append(out, row)
	}
	return out
}

// stateText — socks.State → текст бейджа эталона («healthy» → «ok», mock.js).
func stateText(s string) string {
	if s == "healthy" {
		return "ok"
	}
	return s
}

// statusAccounts — аккаунты всех провайдеров в детерминированном порядке
// (провайдеры по имени, аккаунты — в порядке конфига). quotaRows — общий
// срез Recent (F4-fix1 F3: один SELECT на запрос).
func (h *Handler) statusAccounts(snap *config.Snapshot, now time.Time, quotaRows []quotas.SnapshotRow) []statusAccount {
	out := []statusAccount{}
	for _, pname := range sortedKeys(snap.Cfg.Providers) {
		prov := snap.Cfg.Providers[pname]
		switch {
		case prov.Codex != nil:
			for i := range prov.Codex.Accounts {
				out = append(out, h.codexAccount(pname, &prov.Codex.Accounts[i], quotaRows, now))
			}
		case prov.GLM != nil:
			for i := range prov.GLM.Accounts {
				out = append(out, h.glmAccount(pname, &prov.GLM.Accounts[i], snap))
			}
		case prov.ZCode != nil:
			for i := range prov.ZCode.Accounts {
				out = append(out, h.zcodeAccount(pname, &prov.ZCode.Accounts[i], quotaRows))
			}
		case prov.OpenAILike != nil:
			for i := range prov.OpenAILike.Accounts {
				out = append(out, h.openailikeAccount(pname, &prov.OpenAILike.Accounts[i], quotaRows))
			}
		}
	}
	return out
}

// socksPtr — имя пути для строки аккаунта: прямые пути ("none"/"") → null (mock).
func socksPtr(name string) *string {
	if name == "" || name == "none" {
		return nil
	}
	s := name
	return &s
}

// codexAccount — строка codex-аккаунта: email/статус из TokenSet (SPEC 12.1:
// «для codex — email/аккаунт из TokenSet»), конфиг-email — фолбэк до первого OAuth.
func (h *Handler) codexAccount(pname string, acc *config.CodexAccount, quotaRows []quotas.SnapshotRow, now time.Time) statusAccount {
	a := statusAccount{
		Provider:  pname,
		Auth:      "oauth",
		ID:        acc.ID,
		Email:     acc.Email,
		Token:     "нет токена",
		Socks:     socksPtr(acc.Socks),
		CanOAuth:  true,   // кнопки OAuth — только codex (SPEC 7.2)
		OAuthMode: "pkce", // модалка codex: ссылка + paste/callback (SPEC 12)
		Usage:     statusUsage{Segments: []usageSegment{}, Label: ""},
	}
	if ts, ok := h.deps.Store.Get(codex.AccountKey(pname, acc.ID)); ok {
		if ts.Email != "" {
			a.Email = ts.Email
		}
		a.Token = tokenText(now, ts.ExpiresAt)
	}
	a.Usage = h.codexUsage(quotaRows, pname, acc.ID, now)
	return a
}

// tokenText — «ok» / «токен 42м» (формы mock.js): близкое истечение — минутами.
func tokenText(now time.Time, expiresAt time.Time) string {
	until := expiresAt.Sub(now)
	if until > time.Hour {
		return "ok"
	}
	if until <= 0 {
		return "истёк"
	}
	return fmt.Sprintf("токен %dм", int(until.Minutes()))
}

// codexUsage — мини-бары codex: последнее 5-часовое окно (и недельное в подписи).
func (h *Handler) codexUsage(quotaRows []quotas.SnapshotRow, pname, accID string, now time.Time) statusUsage {
	u := statusUsage{Segments: []usageSegment{}}
	primary, weekly, limited := latestCodexSnapshots(quotaRows, pname, accID)
	if primary != nil {
		u.Segments = append(u.Segments, usageSegment{Pct: primary.UsedPercent, Level: levelOf(primary.UsedPercent)})
	}
	label := ""
	if primary != nil {
		label = "5ч окно " + pctText(primary.UsedPercent)
		if weekly != nil {
			label += " · неделя " + pctText(weekly.UsedPercent)
		}
	}
	if limited != nil && limited.HasReset {
		label += " · 429 до " + time.Unix(limited.ResetAt, 0).Local().Format("15:04")
	}
	u.Label = label
	return u
}

// pctText — «37%» / «42.5%».
func pctText(p float64) string { return trimFloatZero(fmtF(p, 1)) + "%" }

// glmAccount — строка glm-аккаунта (api_key): план и локальная оценка кредитов.
func (h *Handler) glmAccount(pname string, acc *config.GLMAccount, snap *config.Snapshot) statusAccount {
	a := statusAccount{
		Provider: pname,
		Auth:     "api_key",
		ID:       acc.ID,
		Token:    "ok", // api_key статичен: «статус токена» показывает только наличие ключа
		Socks:    socksPtr(acc.Socks),
		CanOAuth: false, // SPEC 7.2: публичного OAuth у glm нет — кнопок не показываем
		Usage:    statusUsage{Segments: []usageSegment{}, Label: ""},
	}
	if acc.CodingPlan {
		a.Plan = "coding plan"
	}
	a.Usage = h.glmCreditsUsage(acc.ID, acc.PlanCredits)
	return a
}

// glmCreditsUsage — мини-бары/подпись кредитов plan_credits (общая для glm и
// zcode: SPEC 7.4 «кредиты Coding Plan считаются локально по той же формуле
// 7.2, если у аккаунта задан plan_credits»). Скользящие окна 5ч/неделя против
// лимитов плана (SPEC 11).
func (h *Handler) glmCreditsUsage(accID string, pc *config.PlanCredits) statusUsage {
	u := statusUsage{Segments: []usageSegment{}, Label: ""}
	if pc == nil || h.deps.Quotas == nil {
		return u
	}
	used5h, usedWeek, p5h, pWeek, err := h.deps.Quotas.GLMBalance(accID,
		&quotas.GLMPlan{Window5h: pc.Window5h, WindowWeekly: pc.WindowWeekly}, h.now())
	if err != nil {
		return u
	}
	u.Segments = []usageSegment{{Pct: p5h, Level: levelOf(p5h)}, {Pct: pWeek, Level: levelOf(pWeek)}}
	u.Label = "5ч " + fmtIntRu(int64(used5h)) + "/" + fmtIntRu(pc.Window5h) +
		" cr · нед " + humanTokens(int64(usedWeek)) + "/" + humanTokens(pc.WindowWeekly) + " cr"
	return u
}

// zcodeAccount — строка zcode-аккаунта (SPEC 12 статус: email/user_id из
// Store, PAT live/до-expiry из PAT-кеша, needs_relogin — снова «Начать OAuth»).
// Использование — кредиты с plan_credits (локальная оценка) плюс z.ai usage
// из поллера программных квот (Ф8), если он включён и уже писал снапшоты.
func (h *Handler) zcodeAccount(pname string, acc *config.ZCodeAccount, quotaRows []quotas.SnapshotRow) statusAccount {
	a := statusAccount{
		Provider:  pname,
		Auth:      "oauth",
		ID:        acc.ID,
		Email:     acc.Email, // конфиг-email — фолбэк до первого OAuth (как у codex)
		Token:     "нет токена",
		Socks:     socksPtr(acc.Socks),
		CanOAuth:  true,
		OAuthMode: "poll", // модалка zcode: ссылка + серверная poll-петля (SPEC 12)
		Usage:     h.glmCreditsUsage(acc.ID, acc.PlanCredits),
	}
	if acc.PlanCredits != nil {
		a.Plan = "coding plan" // кредиты считаются локально, как у glm (SPEC 7.4)
	}
	if s := latestZCodeUsage(quotaRows, pname, acc.ID); s != nil {
		// Фактический процент от сервера честнее локальной оценки — в подпись
		// «z.ai usage: N%» (маленькая карточка без отдельного экрана).
		zl := "z.ai usage: " + pctText(s.UsedPercent)
		if a.Usage.Label == "" {
			a.Usage.Label = zl
		} else {
			a.Usage.Label += " · " + zl
		}
		if len(a.Usage.Segments) == 0 {
			pct := s.UsedPercent
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			a.Usage.Segments = []usageSegment{{Pct: pct, Level: levelOf(pct)}}
		}
	}
	if h.deps.ZStore == nil {
		return a
	}
	key := zcode.AccountKey(acc.ID)
	if rec, ok := h.deps.ZStore.Get(key); ok {
		// SPEC 12: email/user_id из Store (email из poll-ответа, fallback name —
		// Flows это уже свёл; если пусто — человекочитаемый user_id).
		if rec.Email != "" {
			a.Email = rec.Email
		} else if rec.UserID != "" {
			a.Email = rec.UserID
		}
		a.Token = zcodeTokenText(h.now(), h.deps.ZChain, key, h.deps.ZStore)
	}
	return a
}

// zcodeTokenText — человекочитаемый статус zcode-аккаунта (SPEC 12/7.4):
// «нет токена» (no_token), «needs_relogin: <ошибка>» (интерактивный OAuth
// заново), «ok» + остаток PAT до expiry (live/до N минут, если кеш даёт).
func zcodeTokenText(now time.Time, chain *zcode.PATCache, key string, store *zcode.Store) string {
	st := store.Status(key)
	switch st.State {
	case zcode.StatusNoToken:
		return "нет токена"
	case zcode.StatusNeedsRelogin:
		if st.Err != "" {
			return zcode.StatusNeedsRelogin + ": " + st.Err
		}
		return zcode.StatusNeedsRelogin
	}
	if chain != nil {
		if _, exp, ok := chain.PATInfo(key); ok {
			until := exp.Sub(now)
			switch {
			case until <= 0:
				return "ok · PAT истёк" // тикер ре-issue'ит — показываем факт
			case until > time.Hour:
				return fmt.Sprintf("ok · PAT %dч", int(until.Hours()))
			default:
				return fmt.Sprintf("ok · PAT %dм", int(until.Minutes()))
			}
		}
	}
	return "ok"
}

// openailikeAccount — строка openai-like аккаунта: бейдж direct (allow_direct)
// и расход ollama cloud из поллера (если включён и писал снапшоты).
func (h *Handler) openailikeAccount(pname string, acc *config.OpenAILikeAccount, quotaRows []quotas.SnapshotRow) statusAccount {
	a := statusAccount{
		Provider:    pname,
		Auth:        "api_key",
		ID:          acc.ID,
		Token:       "ok",
		Socks:       socksPtr(acc.Socks),
		AllowDirect: acc.AllowDirect,
		CanOAuth:    false,
		Usage:       statusUsage{Segments: []usageSegment{}, Label: ""},
	}
	if s := latestOllamaUsage(quotaRows, pname, acc.ID); s != nil {
		a.Usage.Label = "usage за 24ч: $" + trimFloatZero(fmtF(s.UsedPercent, 2))
	}
	return a
}

// statusClients — клиентские ключи из конфига + счётчики accounting за 24ч.
func (h *Handler) statusClients(snap *config.Snapshot, now time.Time) []statusClient {
	out := []statusClient{}
	var stats map[string][2]int64
	if h.deps.Accounting != nil {
		rows, err := h.deps.Accounting.UsageByClient(now.Add(-24*time.Hour), now)
		if err == nil {
			stats = map[string][2]int64{}
			for _, r := range rows {
				stats[r.ClientKey] = [2]int64{r.Requests, r.Tokens}
			}
		}
	}
	for _, name := range sortedKeys(snap.Cfg.Server.Clients) {
		c := statusClient{Name: name}
		if s, ok := stats[name]; ok {
			c.Req24h, c.Tokens24h = s[0], humanTokens(s[1])
		} else {
			c.Tokens24h = "0"
		}
		out = append(out, c)
	}
	return out
}

// statusMeta — версия (codex.Version, проставляется ldflags при сборке), адрес,
// статус конфига (с временем последнего SIGHUP-релоада) и uptime процесса.
func (h *Handler) statusMeta(snap *config.Snapshot, now time.Time) statusMeta {
	note := "ok"
	if h.deps.ConfigNote != nil {
		note = h.deps.ConfigNote()
	}
	uptime := now.Sub(h.deps.StartedAt)
	return statusMeta{
		Version: codex.Version,
		Listen:  snap.Cfg.Server.Listen,
		Config:  note,
		Uptime:  fmt.Sprintf("%dh %02dm", int(uptime.Hours()), int(uptime.Minutes())%60),
	}
}

// ─── чтения quota_snapshots для статусов/квот ───────────────────────────────

// quotaRows — общий хвост quota_snapshots (Recent 500) ОДИН запрос на HTTP-
// запрос экрана: читатели ниже отбирают из готового среза (F4-fix1 F3).
func (h *Handler) quotaRows() []quotas.SnapshotRow {
	if h.deps.Quotas == nil {
		return nil
	}
	rows, err := h.deps.Quotas.Recent(500)
	if err != nil {
		return nil
	}
	return rows
}

// latestCodexSnapshots — последние снапшоты codex-аккаунта по kind из ГОТОВОГО
// среза Recent (отбор — в Go: пакет quotas не знает про «последние по аккаунту»,
// а внешний SQL в webui запрещён принципом владения таблицами).
func latestCodexSnapshots(rows []quotas.SnapshotRow, pname, accID string) (primary, weekly, limited *quotas.SnapshotRow) {
	for i := range rows {
		r := rows[i]
		if r.Provider != pname || r.Account != accID {
			continue
		}
		switch r.Kind {
		case upstream.QuotaCodexPrimary:
			if primary == nil {
				primary = &rows[i]
			}
		case upstream.QuotaCodexWeekly:
			if weekly == nil {
				weekly = &rows[i]
			}
		case upstream.QuotaCodexLimited:
			if limited == nil {
				limited = &rows[i]
			}
		}
	}
	return primary, weekly, limited
}

// latestOllamaUsage — последний снапшот ollama_usage аккаунта (usage_usd в
// used_percent) из готового среза Recent.
func latestOllamaUsage(rows []quotas.SnapshotRow, pname, accID string) *quotas.SnapshotRow {
	for i := range rows {
		r := rows[i]
		if r.Provider == pname && r.Account == accID && r.Kind == quotas.KindOllamaUsage {
			return &rows[i]
		}
	}
	return nil
}

// latestZCodeUsage — СВЕЖАЙШИЙ снапшот zcode_usage аккаунта (Ф8) из готового
// среза Recent: обходим с конца — поллер пишет по тикеру, нас интересует
// последний процент, а не первый попавшийся.
func latestZCodeUsage(rows []quotas.SnapshotRow, pname, accID string) *quotas.SnapshotRow {
	for i := len(rows) - 1; i >= 0; i-- {
		r := &rows[i]
		if r.Provider == pname && r.Account == accID && r.Kind == quotas.KindZCodeUsage {
			return r
		}
	}
	return nil
}
