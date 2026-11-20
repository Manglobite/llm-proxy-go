// GET /api/quotas — экран ③ «Квоты» (SPEC 12.3): codex-проценты и reset-at
// (x-codex-* снапшоты), glm-кредиты по окнам против plan_credits (локальный
// расчёт), ollama usage_usd (поллер), 429-события. Формы — литералы mock.js
// (DEMO_QUOTAS / DEMO_RATE_LIMITS).
package webui

import (
	"net/http"
	"strconv"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/upstream"
)

// quotaReset — строка сброса окна (форма mock.js: label/value).
type quotaReset struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// quotaWindow — одно окно карточки (форма mock.js; reset опционален —
// у недельного окна glm его в эталоне нет).
type quotaWindow struct {
	Label string      `json:"label"`
	Value string      `json:"value"`
	Pct   float64     `json:"pct"`
	Level string      `json:"level"`
	Reset *quotaReset `json:"reset,omitempty"`
}

// quotaCard — карточка аккаунта (форма mock.js).
type quotaCard struct {
	Provider string        `json:"provider"`
	Account  string        `json:"account"`
	Title    string        `json:"title"`
	Windows  []quotaWindow `json:"windows"`
	Note     string        `json:"note"`
}

// rateLimits — блок 429-событий (форма mock.js).
type rateLimits struct {
	Title string         `json:"title"`
	Rows  []rateLimitRow `json:"rows"`
}

type rateLimitRow struct {
	When    string `json:"when"`
	Account string `json:"account"`
	Window  string `json:"window"`
	Reset   string `json:"reset"`
}

type quotasResponse struct {
	Cards      []quotaCard `json:"cards"`
	RateLimits rateLimits  `json:"rate_limits"`
}

// handleQuotas — карточки по провайдерам живого снапшота + 429-события.
func (h *Handler) handleQuotas(w http.ResponseWriter, r *http.Request) {
	snap := h.snap()
	now := h.now()
	// F4-fix1 F3: Recent(500) — ОДИН запрос к БД на /api/quotas; срез передаётся
	// всем карточкам и 429-блоку.
	quotaRows := h.quotaRows()
	resp := quotasResponse{
		Cards:      []quotaCard{},
		RateLimits: h.rateLimits(snap, now, quotaRows),
	}
	for _, pname := range sortedKeys(snap.Cfg.Providers) {
		prov := snap.Cfg.Providers[pname]
		switch {
		case prov.Codex != nil:
			for i := range prov.Codex.Accounts {
				if card := h.codexCard(quotaRows, pname, &prov.Codex.Accounts[i], now); card != nil {
					resp.Cards = append(resp.Cards, *card)
				}
			}
		case prov.GLM != nil:
			for i := range prov.GLM.Accounts {
				if card := h.glmCard(snap, pname, &prov.GLM.Accounts[i]); card != nil {
					resp.Cards = append(resp.Cards, *card)
				}
			}
		case prov.ZCode != nil:
			for i := range prov.ZCode.Accounts {
				if card := h.zcodeCard(snap, quotaRows, pname, &prov.ZCode.Accounts[i]); card != nil {
					resp.Cards = append(resp.Cards, *card)
				}
			}
		case prov.OpenAILike != nil:
			for i := range prov.OpenAILike.Accounts {
				if card := h.ollamaCard(quotaRows, pname, &prov.OpenAILike.Accounts[i]); card != nil {
					resp.Cards = append(resp.Cards, *card)
				}
			}
		}
	}
	writeJSON(w, resp)
}

// codexCard — карточка codex: 5-часовое и недельное окно из последних снапшотов.
// Карточка появляется после первого ответа апстрима с x-codex-* (до этого
// показывать «0%» значило бы выдавать отсутствие данных за истину).
func (h *Handler) codexCard(quotaRows []quotas.SnapshotRow, pname string, acc *config.CodexAccount, now time.Time) *quotaCard {
	primary, weekly, _ := latestCodexSnapshots(quotaRows, pname, acc.ID)
	if primary == nil && weekly == nil {
		return nil
	}
	card := &quotaCard{
		Provider: pname,
		Account:  acc.ID,
		Title:    pname + " / " + acc.ID,
		Windows:  []quotaWindow{},
		Note:     "источник: заголовки x-codex-primary/secondary-* последнего ответа",
	}
	if primary != nil {
		w := quotaWindow{
			Label: "5-часовое окно",
			Value: pctText(primary.UsedPercent),
			Pct:   primary.UsedPercent,
			Level: levelOf(primary.UsedPercent),
		}
		if primary.HasReset {
			w.Reset = &quotaReset{Label: "сброс", Value: fmtUntil(time.Unix(primary.ResetAt, 0), now)}
		}
		card.Windows = append(card.Windows, w)
	}
	if weekly != nil {
		w := quotaWindow{
			Label: "недельное окно",
			Value: pctText(weekly.UsedPercent),
			Pct:   weekly.UsedPercent,
			Level: levelOf(weekly.UsedPercent),
		}
		if weekly.HasReset {
			w.Reset = &quotaReset{Label: "сброс", Value: fmtUntil(time.Unix(weekly.ResetAt, 0), now)}
		}
		card.Windows = append(card.Windows, w)
	}
	return card
}

// glmCard — карточка glm: кредиты по скользящим окнам против plan_credits
// (локальная оценка, SPEC 7.2/11). Показывается только аккаунтам с планом:
// без лимитов нечего рисовать (проценты не определены).
func (h *Handler) glmCard(snap *config.Snapshot, pname string, acc *config.GLMAccount) *quotaCard {
	return h.planCreditsCard(snap, pname, acc.ID, acc.PlanCredits)
}

// zcodeCard — карточка zcode-аккаунта: кредиты с plan_credits (те же строки,
// что у glm — SPEC 7.4 «кредиты считаются локально по той же формуле 7.2»)
// плюс строка z.ai usage из поллера программных квот U01–U06 (Ф8), когда он
// включён и уже писал снапшоты. Без plan_credits и без снапшотов карточки нет.
func (h *Handler) zcodeCard(snap *config.Snapshot, quotaRows []quotas.SnapshotRow, pname string, acc *config.ZCodeAccount) *quotaCard {
	card := h.planCreditsCard(snap, pname, acc.ID, acc.PlanCredits)
	s := latestZCodeUsage(quotaRows, pname, acc.ID)
	if s == nil {
		return card // поллер выключен или ещё не писал: показывать 0% — выдавать отсутствие данных за истину
	}
	// pct — сам процент, зажатый в 0..100: полоса остаётся читаемой, точное
	// значение всегда видно в value (как у ollamaCard с долларами).
	pct := s.UsedPercent
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if card == nil {
		card = &quotaCard{
			Provider: pname,
			Account:  acc.ID,
			Title:    pname + " / " + acc.ID,
			Windows:  []quotaWindow{},
		}
	}
	card.Windows = append(card.Windows, quotaWindow{
		Label: "z.ai usage",
		Value: pctText(s.UsedPercent),
		Pct:   pct,
		Level: levelOf(pct),
		Reset: &quotaReset{Label: "источник", Value: "GET /api/monitor/usage/quota/limit (usage_poll)"},
	})
	return card
}

// planCreditsCard — общая карточка кредитов glm/zcode (SPEC 7.2/11): расход
// GLMBalance по скользящим окнам 5ч/неделя против plan_credits аккаунта.
func (h *Handler) planCreditsCard(snap *config.Snapshot, pname, accID string, pc *config.PlanCredits) *quotaCard {
	if pc == nil || h.deps.Quotas == nil {
		return nil
	}
	used5h, usedWeek, p5h, pWeek, err := h.deps.Quotas.GLMBalance(accID,
		&quotas.GLMPlan{Window5h: pc.Window5h, WindowWeekly: pc.WindowWeekly}, h.now())
	if err != nil {
		return nil
	}
	card := &quotaCard{
		Provider: pname,
		Account:  accID,
		Title:    pname + " / " + accID + " · coding plan",
		Windows: []quotaWindow{
			{
				Label: "кредиты 5ч",
				Value: fmtIntRu(int64(used5h)) + " / " + fmtIntRu(pc.Window5h),
				Pct:   p5h, Level: levelOf(p5h),
				Reset: &quotaReset{Label: "восстановление", Value: "скользящее, через 5ч после расхода"},
			},
			{
				Label: "кредиты неделя",
				Value: fmtIntRu(int64(usedWeek)) + " / " + fmtIntRu(pc.WindowWeekly),
				Pct:   pWeek, Level: levelOf(pWeek),
			},
		},
		Note: glmNote(snap, pname, accID),
	}
	return card
}

// glmNote — примечание расчёта с мультипликаторами из config.models: берём
// модель ПЕРВОГО алиаса этого аккаунта (алфавитный порядок — детерминизм).
func glmNote(snap *config.Snapshot, pname, accID string) string {
	suffix := " · без off-peak ×0.5"
	for _, name := range sortedKeys(snap.Cfg.Aliases) {
		a := snap.Cfg.Aliases[name]
		if a.Account != pname+"/"+accID {
			continue
		}
		m, ok := snap.Cfg.Models[a.Model]
		if !ok || m.GLMCredits == nil {
			break
		}
		return "локальный расчёт: (in×" + fmtF(m.GLMCredits.InputMult, 1) +
			" + cached×" + fmtF(m.GLMCredits.CachedMult, 1) +
			" + out×" + fmtF(m.GLMCredits.OutputMult, 1) + ") / 10k" + suffix
	}
	return "локальный расчёт: glm_credits из config.models" + suffix
}

// ollamaCard — карточка ollama cloud: usage_usd из последнего снапшота поллера.
func (h *Handler) ollamaCard(quotaRows []quotas.SnapshotRow, pname string, acc *config.OpenAILikeAccount) *quotaCard {
	s := latestOllamaUsage(quotaRows, pname, acc.ID)
	if s == nil {
		return nil // поллер выключен или ещё не писал: карточки нет (SPEC 12.3 «если включен поллинг»)
	}
	// pct — сам usage_usd, зажатый в 0..100: полоса «расход» остаётся читаемой,
	// точное значение всегда видно в value («$2.41»).
	pct := s.UsedPercent
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return &quotaCard{
		Provider: pname,
		Account:  acc.ID,
		Title:    pname + " / " + acc.ID,
		Windows: []quotaWindow{{
			Label: "расход за 24ч",
			Value: "$" + trimFloatZero(fmtF(s.UsedPercent, 2)),
			Pct:   pct,
			Level: levelOf(pct),
			Reset: &quotaReset{Label: "источник", Value: "GET /api/usage?range=24h (usage_poll)"},
		}},
		Note: "источник: GET /api/usage?range=24h (usage_poll) · used_percent = usage_usd",
	}
}

// rateLimits — 429-события: маркеры codex_limited из quota_snapshots.
// glm-429 фиксируются как ошибки (SPEC 11), здесь показываются только codex-маркеры.
func (h *Handler) rateLimits(snap *config.Snapshot, now time.Time, quotaRows []quotas.SnapshotRow) rateLimits {
	rl := rateLimits{Title: "429-события (апстрим отказал в квоте)", Rows: []rateLimitRow{}}
	// Свежие сверху; Recent отдаёт хронологию — разворачиваем и отбираем limited
	// из ОБЩЕГО среза запроса (F4-fix1 F3).
	var limited []quotas.SnapshotRow
	for i := len(quotaRows) - 1; i >= 0; i-- {
		if quotaRows[i].Kind == upstream.QuotaCodexLimited {
			limited = append(limited, quotaRows[i])
		}
	}
	if len(limited) > 50 {
		limited = limited[:50] // блок — сводка, не история: кап страницы (решение Ф4)
	}
	for i := range limited {
		row := rateLimitRow{
			When:    fmtClock(limited[i].Ts, now),
			Account: limited[i].Provider + "/" + limited[i].Account,
			Window:  "—",
		}
		if primary, _, _ := latestCodexSnapshots(quotaRows, limited[i].Provider, limited[i].Account); primary != nil && primary.HasWindow {
			row.Window = windowText(primary.WindowMinutes)
		}
		if limited[i].HasReset {
			row.Reset = time.Unix(limited[i].ResetAt, 0).Local().Format("15:04")
		} else {
			row.Reset = "—"
		}
		rl.Rows = append(rl.Rows, row)
	}
	return rl
}

// windowText — «5ч» / «90м» (mock: окно 429-события).
func windowText(minutes int64) string {
	if minutes > 0 && minutes%60 == 0 {
		return strconv.FormatInt(minutes/60, 10) + "ч"
	}
	return strconv.FormatInt(minutes, 10) + "м"
}
