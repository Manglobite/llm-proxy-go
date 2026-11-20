// widgets/account-card.js — строка аккаунта на экране «Провайдеры»: провайдер/id + email,
// статус токена, socks/direct-бейдж, мини-бары квоты, действия.
// «OAuth…» — для всех OAuth-аккаунтов (codex — pkce, zcode — poll, SPEC 12);
// «Обновить токен» — только codex (у zcode refresh-эндпоинта нет, SPEC 7.4).
import { esc } from "../shared/lib/esc.js";
import { badgeHtml } from "../shared/ui/badge.js";
import { createButton } from "../shared/ui/btn.js";
import { authLine, accountSocksBadge, socksByName } from "../entities/account.js";
import { QLEVEL_FILL } from "../entities/quota.js";

// tokenBadgeClass — класс бейджа токена по тексту статуса (SPEC 12.1: ok / нет
// токена / needs_relogin). Текст бейджа обязателен: статус не только цветом.
export function tokenBadgeClass(token) {
  const t = String(token ?? "");
  if (t.startsWith("нет")) return "err";
  if (t.startsWith("needs_relogin")) return "warn";
  return "ok";
}

export function createAccountCard(account, socks, { onRefresh, onOAuth } = {}) {
  const el = document.createElement("div");
  el.className = "account";

  const who = document.createElement("div");
  who.className = "who";
  // email-часть собрана конкатенацией: вложенные backtick-шаблоны в интерполяциях
  // запрещены (grep-гейт WU-2), текст — всегда через esc
  who.innerHTML = `<span class="prov">${esc(authLine(account))}</span>
    <span class="id">${esc(account.id)}${account.email ? ' <span class="sub inline-sub">' + esc(account.email) + "</span>" : ""}</span>`;
  el.appendChild(who);

  // класс бейджа токена — по тексту статуса; строка API та же, что в эталоне
  el.insertAdjacentHTML("beforeend", badgeHtml(tokenBadgeClass(account.token), account.token));
  const sb = accountSocksBadge(account, socksByName(socks, account.socks));
  el.insertAdjacentHTML("beforeend", badgeHtml(sb.cls, sb.text, sb.title ?? ""));

  const usageWrap = document.createElement("div");
  usageWrap.className = "minibar-wrap";
  for (const seg of account.usage?.segments ?? []) {
    let bar = usageWrap.querySelector(".minibar");
    if (!bar) {
      bar = document.createElement("div");
      bar.className = "minibar";
      usageWrap.appendChild(bar);
    }
    const i = document.createElement("i");
    i.style.width = seg.pct + "%"; // инлайн — только вычисляемое из данных значение
    i.className = QLEVEL_FILL[seg.level] ?? "bg-ok";
    bar.appendChild(i);
  }
  const usageLabel = document.createElement("span");
  usageLabel.textContent = account.usage?.label ?? "";
  usageWrap.appendChild(usageLabel);
  el.appendChild(usageWrap);

  if (account.can_oauth) {
    const actions = document.createElement("div");
    actions.className = "flex-gap6";
    if (account.oauth_mode !== "poll") {
      // codex: refresh есть (SPEC 3.2); legacy "" (строка без oauth_mode) — тоже codex.
      actions.append(createButton({ text: "Обновить токен", small: true, onClick: () => onRefresh?.(account) }));
    }
    actions.append(createButton({ text: "OAuth…", variant: "primary", small: true, onClick: () => onOAuth?.(account) }));
    el.appendChild(actions);
  } else {
    el.appendChild(document.createElement("div")); // пустая колонка сетки, как в эталоне
  }
  return el;
}
