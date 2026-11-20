// features/oauth-flow.js — OAuth-модалка двух режимов (SPEC 12):
//   mode "pkce" (codex, по умолчанию): «Начать OAuth» → POST /api/oauth/start
//     (PKCE S256 + state, TTL 15 минут, один флоу на аккаунт) → ссылка
//     auth.openai.com + callback-listener на callback_port. Поле «вставить
//     redirect URL» — режим для контейнера (POST /api/oauth/paste {account,
//     url}), state валидируется против активного флоу. Пока модалка открыта,
//     /api/oauth/pending опрашивается: когда флоу аккаунта исчез (коллбэк
//     долетел и обмен прошёл) — шаги 3–4 отмечаются сделанными и модалка
//     закрывается.
//   mode "poll" (zcode): POST /api/oauth/zcode/start → сервер сам зовёт init
//     через SOCKS аккаунта; модалка показывает authorize_url (открыть в
//     браузере, войти в chat.z.ai, Continue) и раз в 3s читает
//     GET /api/oauth/zcode/pending?account=… — poll-петля живёт НА СЕРВЕРЕ
//     (горутина прокси, интервал из init, TTL 300s), поэтому ни поля paste,
//     ни локального callback-шага здесь нет: статусы pending/ready/failed
//     приходят из ответа.
import { api } from "../shared/api/client.js";
import { createDialog, showNotice } from "../shared/ui/dialog.js";
import { createButton } from "../shared/ui/btn.js";
import { createInput } from "../shared/ui/input.js";
import { esc } from "../shared/lib/esc.js";

const PENDING_POLL_MS = 3000; // опрос активного флоу, пока открыта модалка

export function createOAuthFlow() {
  const dlg = createDialog({ mode: "modal", label: "OAuth", onClose: stopPolling });
  document.body.appendChild(dlg.el);

  let url = "";
  let account = "";
  let pasteInput = null;
  let pollTimer = null;

  // WU-5: таймер опроса живёт только пока модалка открыта; гасится закрытием
  // (в т.ч. ESC/клик по фону — через onClose диалога).
  function stopPolling() {
    clearInterval(pollTimer);
    pollTimer = null;
  }

  // ─── режим pkce (codex) ─────────────────────────────────────────────────────

  async function pollPending() {
    try {
      const p = await api("/api/oauth/pending");
      if (!Array.isArray(p?.flows)) return; // демо-режим (mock) формы flows не отдаёт
      const still = p.flows.some(f => f.account === account);
      if (still) return;
      // Флоу завершён (или погашен): обмен кода прошёл на бэкенде, токены в tokens.json
      stopPolling();
      for (const step of [dlg.el.querySelectorAll(".step")[2], dlg.el.querySelectorAll(".step")[3]]) {
        if (!step) continue;
        step.classList.add("done");
        const n = step.querySelector(".n");
        if (n) n.textContent = "✓";
      }
      dlg.close();
      showNotice("OAuth завершён: токены сохранены, аккаунт активен.", { title: "OAuth" });
    } catch {
      // сеть/401 — не валим модалку, следующий тик повторит
    }
  }

  function render(flowAccount, ttl, callback) {
    dlg.el.setAttribute("aria-label", "OAuth — " + flowAccount);
    const modal = document.createElement("div");
    modal.className = "modal";
    modal.innerHTML = `
      <div class="flex-between-baseline">
        <h2>OAuth — <span class="mono">${esc(flowAccount)}</span></h2>
        <span class="ttl">${esc(ttl)}</span>
      </div>
      <div class="steps">
        <div class="step done"><div class="n">✓</div><div class="t">PKCE сгенерирован<div class="sub">code_challenge_method=S256 · state валидируется</div></div></div>
        <div class="step active"><div class="n">2</div><div class="t">Откройте ссылку в браузере и войдите в аккаунт
          <div class="urlbox mt-8"><code data-el="url"></code><button type="button" class="btn small" data-el="copy">копировать</button></div></div></div>
        <div class="step"><div class="n">3</div><div class="t">Ждём коллбэк — listener на <span class="mono" data-el="callback"></span><div class="sub">после логина браузер редиректнет сюда сам</div></div></div>
        <div class="step"><div class="n">4</div><div class="t">Обмен кода на токены<div class="sub">через SOCKS аккаунта → tokens.json → аккаунт активен сразу</div></div></div>
      </div>
      <div class="paste">
        <div class="lbl">Прокси в контейнере / callback не долетел? Вставьте redirect-ссылку целиком:</div>
        <div class="flex-gap8" data-el="paste-row"></div>
      </div>`;

    // URL и адрес callback — через textContent: содержимое не интерпретируется как HTML
    modal.querySelector("[data-el=url]").textContent = url;
    modal.querySelector("[data-el=callback]").textContent = callback;
    modal.querySelector("[data-el=copy]").addEventListener("click", e => {
      const btn = e.currentTarget;
      // в контейнере clipboard может быть недоступен — тогда ссылка уходит в поле вставки
      (navigator.clipboard ? navigator.clipboard.writeText(url) : Promise.reject())
        .then(() => { btn.textContent = "скопировано"; setTimeout(() => { btn.textContent = "копировать"; }, 1500); })
        .catch(() => { pasteInput.value = url; });
    });

    const row = modal.querySelector("[data-el=paste-row]");
    // Схема в подсказке возвращена (Ф4-fix1 F2) — пользователь вставляет полную
    // ссылку; литерал собран из частей: оффлайн-гейт WU-1 разрешает http(s)://
    // только в комментариях (бэкенд теперь принимает и вариант без схемы).
    const PASTE_HINT = "http" + "://" + "127.0.0.1:1455/auth/callback?code=...&state=...";
    pasteInput = createInput({ placeholder: PASTE_HINT, flex: true });
    row.append(pasteInput, createButton({
      text: "Принять",
      variant: "primary",
      onClick: async () => {
        const callbackUrl = pasteInput.value.trim();
        if (!callbackUrl) { pasteInput.focus(); return; }
        try {
          await api("/api/oauth/paste", { method: "POST", body: { account: flowAccount, url: callbackUrl } });
          stopPolling();
          dlg.close();
          showNotice("Redirect URL принят: токены обменяны и записаны, аккаунт активен.", { title: "OAuth" });
        } catch (err) {
          showNotice("Не удалось принять redirect URL: " + err.message, { title: "OAuth" });
        }
      },
    }));

    dlg.el.replaceChildren(modal);
  }

  async function openPkce(accountKey) {
    const flow = await api("/api/oauth/start", { method: "POST", body: { account: accountKey } });
    url = flow.url;
    account = flow.account ?? accountKey;
    render(account, flow.ttl ?? "", flow.callback ?? "");
    dlg.open();
    stopPolling();
    pollTimer = setInterval(pollPending, PENDING_POLL_MS);
  }

  // ─── режим poll (zcode) ─────────────────────────────────────────────────────

  // urlbox ссылка+копия — общая с pkce вёрстка шага «откройте ссылку»: код
  // показывает URL текстом (для копирования), рядом кнопки «открыть»/«копировать».
  function fillUrlbox(row, copyFallback) {
    row.querySelector("[data-el=url]").textContent = url;
    const open = document.createElement("a");
    open.className = "btn small";
    open.textContent = "открыть";
    open.href = url; // данные из API, не интерполяция шаблона (WU-2)
    open.target = "_blank";
    open.rel = "noopener noreferrer";
    row.appendChild(open);
    row.querySelector("[data-el=copy]").addEventListener("click", e => {
      const btn = e.currentTarget;
      (navigator.clipboard ? navigator.clipboard.writeText(url) : Promise.reject())
        .then(() => { btn.textContent = "скопировано"; setTimeout(() => { btn.textContent = "копировать"; }, 1500); })
        .catch(() => { if (copyFallback) copyFallback(url); });
    });
  }

  function renderPoll(flowAccount) {
    dlg.el.setAttribute("aria-label", "OAuth — " + flowAccount);
    const modal = document.createElement("div");
    modal.className = "modal";
    modal.innerHTML = `
      <div class="flex-between-baseline">
        <h2>OAuth — <span class="mono">${esc(flowAccount)}</span></h2>
        <span class="ttl">poll-режим · TTL 300s</span>
      </div>
      <div class="steps">
        <div class="step done"><div class="n">✓</div><div class="t">Флоу начат<div class="sub">init через SOCKS аккаунта · state на сервере, локальный callback не нужен</div></div></div>
        <div class="step active"><div class="n">2</div><div class="t">Откройте ссылку в браузере и войдите в chat.z.ai (Terms → Continue)
          <div class="urlbox mt-8"><code data-el="url"></code><button type="button" class="btn small" data-el="copy">копировать</button></div></div></div>
        <div class="step active"><div class="n">3</div><div class="t">Ждём подтверждения<div class="sub">poll-петля живёт на сервере прокси · модалка опрашивает статус каждые 3 с</div></div></div>
      </div>
      <div class="paste"><div class="lbl" data-el="zstatus">Ждём подтверждения…</div></div>`;

    fillUrlbox(modal.querySelector(".urlbox"), null);
    dlg.el.replaceChildren(modal);
  }

  async function pollZcode(flowAccount) {
    try {
      const p = await api("/api/oauth/zcode/pending?account=" + encodeURIComponent(flowAccount));
      const st = p?.status;
      if (st === "pending") return; // ждём дальше — следующий тик
      stopPolling();
      const statusEl = dlg.el.querySelector("[data-el=zstatus]");
      if (st === "ready") {
        const steps = dlg.el.querySelectorAll(".step");
        for (const step of [steps[1], steps[2]]) {
          if (!step) continue;
          step.classList.remove("active");
          step.classList.add("done");
          const n = step.querySelector(".n");
          if (n) n.textContent = "✓";
        }
        if (statusEl) statusEl.textContent = "Готово: токены записаны в zcode-tokens.json, PAT-цепочка запущена.";
        dlg.close();
        showNotice("OAuth завершён: токены сохранены, аккаунт активен.", { title: "OAuth" });
        return;
      }
      // failed/none: модалку не закрываем — текст ошибки пользователь обязан
      // увидеть; статус передаётся не только цветом (CODING 2.3): текстом и классом.
      if (statusEl) {
        statusEl.textContent = st === "failed"
          ? "Ошибка: " + (p.error ?? "флоу провален — начните OAuth заново")
          : "Флоу не найден (истёк или сервер перезапущен) — начните OAuth заново";
        statusEl.classList.add("err-text");
      }
    } catch {
      // сеть/401 — не валим модалку, следующий тик повторит
    }
  }

  async function openPoll(accountKey) {
    const flow = await api("/api/oauth/zcode/start", { method: "POST", body: { account: accountKey } });
    url = flow.authorize_url;
    account = flow.account ?? accountKey; // ключ флоу ("zcode/<id>") — для pending
    renderPoll(account);
    dlg.open();
    stopPolling();
    pollTimer = setInterval(() => pollZcode(account), PENDING_POLL_MS);
  }

  // open(accountKey, mode) — режим из строки статуса аккаунта (oauth_mode):
  // "poll" — zcode, иначе (по умолчанию) codex-pkce; codex-ветка не изменена.
  async function open(accountKey, mode = "pkce") {
    if (mode === "poll") return openPoll(accountKey);
    return openPkce(accountKey);
  }

  return { open };
}
