// shared/ui/dialog.js — модалка и drawer (CODING 2.3): role="dialog" aria-modal="true",
// focus-trap (фокус внутрь при открытии, возврат при закрытии), закрытие по ESC
// и клику по фону. Контент внутрь кладёт вызывающий слой (features/widgets).
// Модуль самодостаточный: без импортов, динамические тексты — через textContent.

const FOCUSABLE = 'button, input, select, textarea, a[href], [tabindex]:not([tabindex="-1"])';

// Стек открытых диалогов: ESC закрывает только верхний (notice поверх OAuth-модалки).
const stack = [];

export function createDialog({ mode = "modal", label = "", onClose = null } = {}) {
  const el = document.createElement("div");
  el.className = mode === "drawer" ? "drawer" : "overlay";
  el.setAttribute("role", "dialog");
  el.setAttribute("aria-modal", "true");
  if (label) el.setAttribute("aria-label", label);
  el.tabIndex = -1; // куда поставить фокус, если фокусируемых внутри нет

  const api = { el, open, close };
  let opener = null;
  let isOpen = false;

  function onKeydown(e) {
    if (stack[stack.length - 1] !== api) return; // события ESC/Tab глотает только верхний диалог
    if (e.key === "Escape") { e.preventDefault(); close(); return; }
    if (e.key !== "Tab") return;
    // focus-trap: Tab циклично ходит по фокусируемым внутри диалога
    const items = [...el.querySelectorAll(FOCUSABLE)].filter(n => !n.disabled && n.offsetParent !== null);
    if (!items.length) return;
    const first = items[0], last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
  // модалка: фон — сам .overlay; drawer фона не имеет — закрываемся по клику вне панели
  function onBackdrop(e) { if (e.target === el) close(); }
  function onOutside(e) { if (isOpen && !el.contains(e.target)) close(); }

  function open() {
    if (isOpen) return;
    isOpen = true;
    opener = document.activeElement;
    stack.push(api);
    document.addEventListener("keydown", onKeydown, true);
    if (mode === "modal") el.addEventListener("click", onBackdrop);
    else document.addEventListener("mousedown", onOutside);
    el.classList.add("open");
    const target = el.querySelector(FOCUSABLE);
    (target ?? el).focus();
  }
  function close() {
    if (!isOpen) return;
    isOpen = false;
    stack.splice(stack.indexOf(api), 1);
    document.removeEventListener("keydown", onKeydown, true);
    el.removeEventListener("click", onBackdrop);
    document.removeEventListener("mousedown", onOutside);
    el.classList.remove("open");
    if (opener && typeof opener.focus === "function") opener.focus(); // возврат фокуса
    opener = null;
    if (onClose) onClose();
  }
  return api;
}

// showNotice — замена alert-заглушек эталона (CODING 2.6): диалог с сообщением и «Понятно».
export function showNotice(message, { title = "Демо" } = {}) {
  const dlg = createDialog({ mode: "modal", label: title, onClose: () => dlg.el.remove() });
  const modal = document.createElement("div");
  modal.className = "modal";
  const h = document.createElement("h2");
  h.textContent = title;
  const p = document.createElement("p");
  p.textContent = message;
  const row = document.createElement("div");
  row.className = "flex-gap6";
  const ok = document.createElement("button");
  ok.type = "button";
  ok.className = "btn primary";
  ok.textContent = "Понятно";
  ok.addEventListener("click", () => dlg.close());
  row.appendChild(ok);
  modal.append(h, p, row);
  dlg.el.appendChild(modal);
  document.body.appendChild(dlg.el);
  dlg.open();
  return dlg;
}
