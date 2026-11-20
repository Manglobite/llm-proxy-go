// pages/quotas.js — экран ③ «Квоты» (SPEC 12.3): codex-проценты + reset-at,
// glm-кредиты по окнам против плана, ollama usage_usd, 429-события.
// Данные — GET /api/quotas → renderQuotas().
import { api } from "../shared/api/client.js";
import { esc } from "../shared/lib/esc.js";
import { showNotice } from "../shared/ui/dialog.js";
import { createCard } from "../shared/ui/card.js";
import { createTable } from "../shared/ui/table.js";
import { parseQuotas } from "../entities/quota.js";
import { quotaBarHtml } from "../widgets/quota-bar.js";

export function mountQuotas(section) {
  const grid = document.createElement("div");
  grid.className = "grid cols-2";
  section.appendChild(grid);

  function renderQuotas({ cards, rateLimits }) {
    grid.innerHTML = "";
    for (const card of cards) {
      const el = createCard(esc(card.title));
      card.windows.forEach((w, i) =>
        el.insertAdjacentHTML("beforeend", quotaBarHtml(w, { gapAfter: i < card.windows.length - 1 })));
      if (card.note) el.insertAdjacentHTML("beforeend", `<p class="note-xs">${esc(card.note)}</p>`);
      grid.appendChild(el);
    }

    const rl = createCard(esc(rateLimits.title));
    const table = createTable([
      { label: "когда" }, { label: "аккаунт" }, { label: "окно" }, { label: "сброс" },
    ]);
    table.tbody.innerHTML = rateLimits.rows.map(r => `
      <tr><td>${esc(r.when)}</td><td class="mono">${esc(r.account)}</td><td>${esc(r.window)}</td><td>${esc(r.reset)}</td></tr>`).join("");
    rl.appendChild(table.el);
    grid.appendChild(rl);
  }

  async function load() {
    renderQuotas(parseQuotas(await api("/api/quotas")));
  }
  load().catch(err => showNotice("Не удалось загрузить квоты: " + err.message, { title: "API" }));
  return { refresh: load }; // квоты — «живой» экран: обновляются по setInterval из app
}
