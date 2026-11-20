// pages/errors.js — экран ⑤ «Ошибки» (SPEC 12.5): таблица + полный дамп по клику
// (GET /api/errors/{id}) + ретеншн. Данные — GET /api/errors?from&to&limit&offset →
// renderErrors(); ретеншн — POST /api/errors/cleanup {older_than} с перезагрузкой списка.
import { api } from "../shared/api/client.js";
import { esc } from "../shared/lib/esc.js";
import { showNotice } from "../shared/ui/dialog.js";
import { createCard } from "../shared/ui/card.js";
import { createTable } from "../shared/ui/table.js";
import { parseErrors, parseErrorDetail, stageClass } from "../entities/error.js";
import { createErrorsCleanup } from "../features/errors-cleanup.js";
import { createErrorsDrawer } from "../widgets/errors-drawer.js";

const LIST_LIMIT = 100; // пагинация на сервере (CODING 2.5): первая страница списка

export function mountErrors(section, { onCount } = {}) {
  const card = createCard();

  const head = document.createElement("div");
  head.className = "flex-between mb-10";
  const h3 = document.createElement("h3");
  h3.className = "mb-0";
  h3.innerHTML = `Лог ошибок <span class="h3-plain">(GET /api/errors?from&amp;to; клик — дамп /api/errors/{id})</span>`;
  head.appendChild(h3);
  createErrorsCleanup(head, { onCleanup: cleanup });
  card.appendChild(head);

  const table = createTable([
    { label: "время" },
    { label: "stage" },
    { label: "http", num: true },
    { label: "алиас" },
    { label: "ошибка" },
  ]);
  card.appendChild(table.el);
  section.appendChild(card);

  const drawer = createErrorsDrawer();

  function renderErrors(list) {
    table.tbody.innerHTML = list.map(e => `
    <tr class="clickable" data-id="${esc(e.id ?? 0)}" tabindex="0" aria-label="Открыть дамп ошибки">
    <td class="mono">${esc(e.ts)}</td><td><span class="${esc(stageClass(e.stage))}">${esc(e.stage)}</span></td>
    <td class="num">${esc(e.status || "—")}</td><td class="mono">${esc(e.alias)}</td>
    <td class="cell-err" title="${esc(e.err)}">${esc(e.err)}</td></tr>`).join("");
    onCount?.(list.length);
  }

  async function openDump(id) {
    drawer.open(parseErrorDetail(await api("/api/errors/" + id)));
  }
  table.tbody.addEventListener("click", e => {
    const tr = e.target.closest("tr.clickable");
    if (tr) openDump(tr.dataset.id).catch(err => showNotice("Дамп недоступен: " + err.message, { title: "API" }));
  });
  // строка с tabindex — клавиатурный аналог клика (таблица не может быть <button>)
  table.tbody.addEventListener("keydown", e => {
    const tr = e.target.closest("tr.clickable");
    if (tr && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      openDump(tr.dataset.id).catch(err => showNotice("Дамп недоступен: " + err.message, { title: "API" }));
    }
  });

  async function cleanup(olderThan) {
    // catch обязателен: без него сбой ретеншна — тихий unhandled rejection
    // (Ф4-fix1 F1); ошибка ретеншна — диалог эталона, не консоль.
    try {
      const res = await api("/api/errors/cleanup", { method: "POST", body: { older_than: olderThan } });
      showNotice("Удалено записей старше " + olderThan + ": " + (res?.removed ?? 0), { title: "Очистка" });
      await load(); // перечитываем список после ретеншна
    } catch (err) {
      showNotice("Очистка не удалась: " + err.message, { title: "Очистка" });
    }
  }

  async function load() {
    renderErrors(parseErrors(await api("/api/errors?limit=" + LIST_LIMIT + "&offset=0")));
  }
  load().catch(err => showNotice("Не удалось загрузить ошибки: " + err.message, { title: "API" }));
  return {};
}
