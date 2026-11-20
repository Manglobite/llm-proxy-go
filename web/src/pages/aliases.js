// pages/aliases.js — экран ② «Алиасы» (SPEC 12.2): список + подсветка битых ссылок
// и заблокированных путей. Данные — GET /api/aliases → renderAliases().
import { api } from "../shared/api/client.js";
import { esc } from "../shared/lib/esc.js";
import { fmtInt } from "../shared/lib/fmt.js";
import { showNotice } from "../shared/ui/dialog.js";
import { badgeHtml } from "../shared/ui/badge.js";
import { createCard } from "../shared/ui/card.js";
import { createTable } from "../shared/ui/table.js";
import { createButton } from "../shared/ui/btn.js";
import { parseAliases } from "../entities/alias.js";
import { createAliasForm } from "../features/config-form.js";

export function mountAliases(section) {
  const card = createCard();

  const head = document.createElement("div");
  head.className = "flex-between mb-10";
  const h3 = document.createElement("h3");
  h3.className = "mb-0";
  h3.textContent = "Алиасы";
  const createBtn = createButton({ text: "Создать алиас", variant: "primary", small: true });
  head.append(h3, createBtn);
  card.appendChild(head);

  const table = createTable([
    { label: "алиас" },
    { label: "аккаунт" },
    { label: "модель апстрима" },
    { label: "запросов 24ч", num: true },
    { label: "статус" },
  ]);
  card.appendChild(table.el);
  section.appendChild(card);

  createBtn.addEventListener("click", async () => {
    createBtn.disabled = true;
    try {
      const status = await api("/api/status");
      createAliasForm(status.accounts ?? [], { onSuccess: load });
    } catch (err) {
      showApiError(err);
    } finally {
      createBtn.disabled = false;
    }
  });

  function renderAliases(rows) {
    table.tbody.innerHTML = rows.map(a => `
    <tr><td class="mono"><b>${esc(a.name)}</b></td><td class="mono">${esc(a.account)}</td><td>${esc(a.model)}</td>
    <td class="num">${esc(fmtInt(a.req24h))}</td>
    <td>${a.status === "ok" ? badgeHtml("ok", "ok") : badgeHtml("err", a.note, a.note)}</td></tr>`).join("");
  }

  async function load() {
    try {
      const aliases = parseAliases(await api("/api/aliases"));
      renderAliases(aliases);
    } catch (err) {
      showApiError(err);
    }
  }
  load();
  return { refresh: load };
}

// showApiError — сообщения о сбое загрузки идут диалогом, не консолью (CODING 2.6)
function showApiError(err) {
  showNotice("Не удалось загрузить данные: " + err.message, { title: "API" });
}
