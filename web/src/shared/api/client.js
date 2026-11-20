// shared/api/client.js — единая точка доступа к /api/* (CODING 2.4, SPEC 12).
// Рабочий режим: DEMO=false — fetch с Bearer webui_token из localStorage.
// mock.js остаётся в дереве для офлайн-разработки: достаточно вернуть
// DEMO=true — все вызовы обслужит shared/api/mock.js (литералы эталона),
// формы данных те же, что у ответов /api/*.
// Импорт mock.js — внутрислоевой (shared/api), предписан демо-режимом.
import { mockApi } from "./mock.js";

export const DEMO = false;

// Токен читается на КАЖДЫЙ запрос, а не один раз при загрузке модуля:
// после ввода в диалоге 401 запросы должны идти с новым значением без перезагрузки страницы.
const token = () => localStorage.getItem("webui_token") ?? "";

// Обработчик 401 регистрирует app: shared не может импортировать ui (слои строго вниз).
let onUnauthorized = async () => false;
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

// Одновременные 401 открывают ОДИН диалог: параллельные запросы ждут его
// результата, а не плодят стек модалок (CODING 2.6 — поведение).
let promptInFlight = null;
function unauthorized() {
  if (!promptInFlight) {
    promptInFlight = Promise.resolve(onUnauthorized())
      .finally(() => { promptInFlight = null; });
  }
  return promptInFlight;
}

export async function api(path, opts = {}) {
  if (DEMO) return mockApi(path, opts);
  const init = { ...opts, headers: { Authorization: "Bearer " + token(), ...opts.headers } };
  if (init.body && typeof init.body === "object") {
    init.body = JSON.stringify(init.body);
    init.headers["Content-Type"] = "application/json";
  }
  const r = await fetch(path, init);
  if (r.status === 401) {
    // Диалог ввода токена показывает app; после сохранения токена запрос
    // повторяется (CODING 2.4: 401 → экран ввода → повтор запроса).
    if (await unauthorized()) return api(path, opts);
    throw new Error("unauthorized");
  }
  if (!r.ok) throw new Error(r.status + ": " + await r.text());
  return r.status === 204 ? null : r.json();
}
