// pages/providers.js — экран ① «Провайдеры и аккаунты» (SPEC 12.1): SOCKS-пути с
// IP-пиннингом, клиентские ключи, аккаунты. Данные — GET /api/status → renderStatus().
import { api } from "../shared/api/client.js";
import { esc } from "../shared/lib/esc.js";
import { fmtInt } from "../shared/lib/fmt.js";
import { showNotice } from "../shared/ui/dialog.js";
import { createCard } from "../shared/ui/card.js";
import { createTable } from "../shared/ui/table.js";
import { parseStatus } from "../entities/account.js";
import { ipPinRowHtml } from "../widgets/ip-pin-row.js";
import { createAccountCard } from "../widgets/account-card.js";
import { createOAuthFlow } from "../features/oauth-flow.js";
import { createClientForm, createProviderForm, createSocksForm, revokeClientForm } from "../features/config-form.js";
import { createButton } from "../shared/ui/btn.js";

// один экземпляр модалки: активный флоу — один на аккаунт (SPEC 12)
const oauth = createOAuthFlow();

export function mountProviders(section, { onStatus } = {}) {
  const top = document.createElement("div");
  top.className = "grid cols-2 mb-14";

  const socksCard = createCard();
  const socksHead = document.createElement("div");
  socksHead.className = "flex-between mb-10";
  const socksTitle = document.createElement("h3");
  socksTitle.className = "mb-0";
  socksTitle.textContent = "SOCKS-пути";
  const addSocksBtn = createButton({ text: "Добавить SOCKS-путь", variant: "primary", small: true,
    onClick: () => createSocksForm({ onSuccess: () => refreshAfterSave("SOCKS-путь") }),
  });
  socksHead.append(socksTitle, addSocksBtn);
  socksCard.appendChild(socksHead);
  const socksList = document.createElement("div");
  socksList.className = "grid";
  socksCard.appendChild(socksList);

  const keysCard = createCard();
  const keysHead = document.createElement("div");
  keysHead.className = "flex-between mb-10";
  const keysTitle = document.createElement("h3");
  keysTitle.className = "mb-0";
  keysTitle.textContent = "Клиентские ключи";
  const addKeyBtn = createButton({ text: "Добавить ключ", variant: "primary", small: true,
    onClick: () => createClientForm({ onSuccess: () => refreshAfterSave("Клиентский ключ") }),
  });
  keysHead.append(keysTitle, addKeyBtn);
  keysCard.appendChild(keysHead);
  const keysTable = createTable([
    { label: "имя" },
    { label: "запросов 24ч", num: true },
    { label: "токенов 24ч", num: true },
    { label: "действия" },
  ]);
  keysCard.appendChild(keysTable.el);
  top.append(socksCard, keysCard);

  const accountsCard = createCard();
  const accountsHead = document.createElement("div");
  accountsHead.className = "flex-between mb-10";
  const h3 = document.createElement("h3");
  h3.className = "mb-0";
  h3.textContent = "Аккаунты";
  const addProvBtn = createButton({ text: "Добавить провайдер", variant: "primary", small: true });
  accountsHead.append(h3, addProvBtn);
  accountsCard.appendChild(accountsHead);
  section.append(top, accountsCard);

  const checking = new Set();
  const checkResults = new Map();
  let loadVersion = 0;

  async function recheck(name, button, result) {
    if (checking.has(name)) return;
    checking.add(name);
    checkResults.delete(name);
    button.disabled = true;
    result.textContent = "Проверка…";
    try {
      const { ok } = await api("/api/config/socks/recheck", { method: "POST", body: { name } });
      checkResults.set(name, ok ? "Проверка пройдена" : "Проверка не пройдена");
    } catch (err) {
      const msg = err.message.replace(/^\d+: /, "");
      try { checkResults.set(name, JSON.parse(msg).error || msg); }
      catch { checkResults.set(name, msg); }
    } finally {
      checking.delete(name);
      button.disabled = false;
      result.textContent = checkResults.get(name) ?? "";
    }
    try { await load(); }
    catch { showNotice("Не удалось обновить список SOCKS-путей.", { title: "SOCKS" }); }
  }

  function renderStatus(status) {
    socksList.innerHTML = status.socks.map(ipPinRowHtml).join("");
    status.socks.forEach((path, i) => {
      if (!Object.hasOwn(path, "check_interval_raw")) return;
      const edit = createButton({ text: "Изменить", small: true, onClick: () => {
        createSocksForm({ path, onSuccess: () => refreshAfterSave("SOCKS-путь") });
      } });
      const card = socksList.children[i];
      const result = document.createElement("div");
      result.setAttribute("role", "status");
      result.textContent = checking.has(path.name) ? "Проверка…" : checkResults.get(path.name) ?? "";
      const check = createButton({ text: "Проверить сейчас", small: true, disabled: checking.has(path.name),
        onClick: () => recheck(path.name, check, result),
      });
      card.querySelector("[data-socks-actions]").append(check, edit);
      card.append(result);
    });
    keysTable.tbody.innerHTML = status.clientKeys.map(k => `
      <tr><td class="mono">${esc(k.name)}</td><td class="num">${esc(fmtInt(k.req24h))}</td><td class="num">${esc(k.tokens24h)}</td><td></td></tr>`).join("");
    status.clientKeys.forEach((key, i) => {
      const cell = keysTable.tbody.rows[i].cells[3];
      if (key.revoked) {
        cell.textContent = "Отозван";
        return;
      }
      cell.append(createButton({ text: "Отозвать", small: true,
        onClick: () => revokeClientForm(key.name, { onSuccess: () => refreshAfterSave("Клиентский ключ") }),
      }));
    });
    accountsCard.querySelectorAll(".account").forEach(n => n.remove());
    for (const account of status.accounts) {
      accountsCard.appendChild(createAccountCard(account, status.socks, {
        // рабочие вызовы вместо демо-заглушек: POST /api/oauth/refresh и /api/oauth/start
        onRefresh: async acc => {
          try {
            await api("/api/oauth/refresh", { method: "POST", body: { account: acc.provider + "/" + acc.id } });
            showNotice("Токен обновлён: новый expires_at записан в tokens.json.", { title: "OAuth" });
            load();
          } catch (err) {
            showNotice("Не удалось обновить токен: " + err.message, { title: "OAuth" });
          }
        },
        onOAuth: acc =>
          // режим модалки — из строки статуса: poll (zcode) или pkce (codex)
          oauth.open(acc.provider + "/" + acc.id, acc.oauth_mode === "poll" ? "poll" : "pkce"),
      }));
    }
    onStatus?.(status); // сводка/мета уходят в app → сайдбар и топбар
  }

  async function load() {
    const version = ++loadVersion;
    const status = parseStatus(await api("/api/status", { cache: "no-store" }));
    if (version === loadVersion) renderStatus(status);
  }
  load();
  addProvBtn.addEventListener("click", async () => {
    addProvBtn.disabled = true;
    try {
      const status = parseStatus(await api("/api/status"));
      createProviderForm(status, { onSuccess: () => refreshAfterSave("Провайдер") });
    } catch (err) {
      showNotice("Не удалось загрузить SOCKS-пути: " + err.message, { title: "Провайдер" });
    } finally {
      addProvBtn.disabled = false;
    }
  });

  async function refreshAfterSave(label) {
    try {
      await load();
    } catch (err) {
      showNotice("Изменения сохранены, но список не обновился: " + err.message, { title: label });
    }
  }

  return { refresh: load }; // refresh вызывает setInterval в app (CODING 2.6)
}
