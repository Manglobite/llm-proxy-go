// widgets/ip-pin-row.js — карточка SOCKS-пути с IP-пиннингом: имя+бейдж, url,
// ожидаемый/фактический egress ip, текст ошибки, время проверки (SPEC 12.1).
import { esc } from "../shared/lib/esc.js";
import { badgeHtml } from "../shared/ui/badge.js";
import { socksBadge, egressIpText, checkText } from "../entities/account.js";

export function ipPinRowHtml(s) {
  const b = socksBadge(s);
  const url = s.url ?? "";
  return `
  <div class="socks-card">
    <div class="flex-between"><b class="mono">${esc(s.name)}</b><div class="flex-gap8" data-socks-actions>${badgeHtml(b.cls, b.text)}</div></div>
    <div class="kv"><span class="k">url</span><span class="mono">${esc(url)}</span></div>
    <div class="kv"><span class="k">egress ip</span><span class="mono">${esc(egressIpText(s))}</span></div>
    ${s.error ? '<div class="err-text">' + esc(s.error) + "</div>" : ""}
    <div class="kv"><span class="k">проверка</span><span>${esc(checkText(s))}</span></div>
  </div>`;
}
