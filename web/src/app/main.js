// app/main.js — точка входа (CODING 2.2): монтирование каркаса в #root,
// роутер экранов, setInterval-планировщик обновления статусов с очисткой при смене экрана.
// Глобальное состояние (активный экран, планировщик) — только здесь.
import { mountProviders } from "../pages/providers.js";
import { mountAliases } from "../pages/aliases.js";
import { mountQuotas } from "../pages/quotas.js";
import { mountDashboard } from "../pages/dashboard.js";
import { mountErrors } from "../pages/errors.js";
import { createSidebar } from "../widgets/sidebar.js";
import { setUnauthorizedHandler } from "../shared/api/client.js";
import { createDialog } from "../shared/ui/dialog.js";
import { createInput } from "../shared/ui/input.js";
import { createButton } from "../shared/ui/btn.js";
import { parseStatus, socksSummary } from "../entities/account.js";

const TITLES = {
  accounts: "Провайдеры и аккаунты",
  aliases: "Алиасы",
  quotas: "Квоты",
  usage: "Учёт токенов",
  errors: "Ошибки",
};
const POLL_MS = 30000; // период обновления статусов (CODING 2.6)

// showTokenPrompt — реальная замена демо-заглушки 401 (CODING 2.4): диалог ввода
// webui_token, сохранение в localStorage и повтор запроса (api() ретраит, когда
// диалог resolves true). Обработчик регистрирует app, потому что shared не может
// импортировать ui (слои строго вниз, CODING 2.2).
function showTokenPrompt() {
  return new Promise(resolve => {
    // settled: ESC/фон и «Сохранить» оба закрывают диалог через onClose —
    // promise должен осесть ровно один раз, первым исходом
    let settled = false;
    const settle = v => { if (!settled) { settled = true; resolve(v); } };
    const dlg = createDialog({
      mode: "modal",
      label: "webui_token",
      onClose: () => { dlg.el.remove(); settle(false); },
    });
    const modal = document.createElement("div");
    modal.className = "modal";
    const h = document.createElement("h2");
    h.textContent = "Требуется webui_token";
    const p = document.createElement("p");
    p.textContent = "Сервер отвечает 401 — в config.json задан webui_token (SPEC 12). Введите его: значение сохранится в этом браузере и будет уходить в Authorization: Bearer.";
    const input = createInput({ placeholder: "webui-…", value: localStorage.getItem("webui_token") ?? "", flex: true });
    const row = document.createElement("div");
    row.className = "flex-gap8";
    const save = createButton({
      text: "Сохранить",
      variant: "primary",
      onClick: () => {
        const v = input.value.trim();
        if (!v) { input.focus(); return; }
        localStorage.setItem("webui_token", v);
        settle(true);
        dlg.close();
      },
    });
    row.appendChild(save);
    modal.append(h, p, input, row);
    dlg.el.appendChild(modal);
    document.body.appendChild(dlg.el);
    dlg.open();
    input.focus();
  });
}

setUnauthorizedHandler(showTokenPrompt);

const root = document.getElementById("root");
const sidebar = createSidebar(root);
const main = document.createElement("main");
root.appendChild(main);

const topbar = document.createElement("div");
topbar.className = "topbar";
topbar.innerHTML = `<h1 data-el="title">Провайдеры и аккаунты</h1>
  <div class="meta"><span data-el="config"></span><span data-el="uptime"></span></div>`;
main.appendChild(topbar);
const titleEl = topbar.querySelector("[data-el=title]");
const configEl = topbar.querySelector("[data-el=config]");
const uptimeEl = topbar.querySelector("[data-el=uptime]");

const screens = {};
for (const name of Object.keys(TITLES)) {
  const section = document.createElement("section");
  section.className = "screen";
  section.id = "screen-" + name;
  main.appendChild(section);
  screens[name] = section;
}

// монтирование экранов; перекрёстные вызовы (счётчик ошибок, сводка SOCKS, мета) —
// только через обратные вызовы, замкнутые здесь (CODING 2.2: глобальное — в app)
const providers = mountProviders(screens.accounts, {
  onStatus: status => {
    sidebar.setSocksSummary(socksSummary(status.socks));
    sidebar.setMeta(status.meta);
    configEl.textContent = "config: " + (status.meta.config ?? "");
    uptimeEl.textContent = "uptime " + (status.meta.uptime ?? "");
  },
});
mountAliases(screens.aliases);
const quotas = mountQuotas(screens.quotas);
mountDashboard(screens.usage);
mountErrors(screens.errors, { onCount: n => sidebar.setErrorsCount(n) });

let pollTimer = null;
function restartPolling(name) {
  // CODING 2.6: setInterval с очисткой при смене экрана; живые данные — SOCKS и квоты
  clearInterval(pollTimer);
  pollTimer = null;
  const live = { accounts: providers.refresh, quotas: quotas.refresh }[name];
  if (live) pollTimer = setInterval(live, POLL_MS);
}

// показ экрана: подсветка в сайдбаре, .active на секции (иначе section.screen
// навсегда display:none — экран скрыт), заголовок и перезапуск поллинга
function show(name) {
  sidebar.setActive(name);
  for (const [n, sec] of Object.entries(screens)) sec.classList.toggle("active", n === name);
  titleEl.textContent = TITLES[name];
  restartPolling(name);
}

sidebar.nav.addEventListener("click", e => {
  const btn = e.target.closest("button[data-screen]");
  if (!btn) return;
  show(btn.dataset.screen);
});

show("accounts"); // стартовый экран активируем сразу, не только по клику
