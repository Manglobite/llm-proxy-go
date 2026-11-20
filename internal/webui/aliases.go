// GET /api/aliases — экран ② «Алиасы» (SPEC 12.2): алиас → provider/account/model
// + здоровье ссылок (заблокированный SOCKS-путь цели, счётчик запросов за 24ч).
// Форма строки — литерал DEMO_ALIASES из mock.js.
package webui

import (
	"net/http"
	"time"

	"llm-proxy/internal/router"
)

// aliasRow — строка списка алиасов (форма mock.js). Status: ok | err; note —
// причина (только у err).
type aliasRow struct {
	Name    string `json:"name"`
	Account string `json:"account"`
	Model   string `json:"model"`
	Req24h  int64  `json:"req24h"`
	Status  string `json:"status"`
	Note    string `json:"note"`
}

// handleAliases — алиасы живого снапшота + здоровье ссылок:
//   - аккаунт не резолвится (рассинхрон после релоада) → err;
//   - SOCKS-путь цели blocked → err «socks path X: blocked» (формулировка mock.js).
func (h *Handler) handleAliases(w http.ResponseWriter, r *http.Request) {
	snap := h.snap()
	now := h.now()

	counts := map[string]int64{}
	if h.deps.Accounting != nil {
		if m, err := h.deps.Accounting.UsageByAliasCount(now.Add(-24*time.Hour), now); err == nil {
			counts = m
		}
	}

	out := make([]aliasRow, 0, len(snap.Cfg.Aliases))
	for _, name := range sortedKeys(snap.Cfg.Aliases) {
		a := snap.Cfg.Aliases[name]
		row := aliasRow{
			Name:    name,
			Account: a.Account,
			Model:   a.Model,
			Req24h:  counts[name],
			Status:  "ok",
		}
		target, err := router.Resolve(snap, name)
		switch {
		case err != nil:
			// CF-2 валидирует ссылки при загрузке; здесь — защита от гонки
			// релоада: показываем проблему, а не молчаливый ok.
			row.Status, row.Note = "err", err.Error()
		case target.SocksName != "" && target.SocksName != "none":
			if p, ok := snap.Socks[target.SocksName]; ok && p.Status().State == "blocked" {
				row.Status = "err"
				row.Note = "socks path " + target.SocksName + ": blocked"
			}
		}
		out = append(out, row)
	}
	writeJSON(w, out)
}
