// widgets/errors-drawer.js — drawer дампа ошибки (GET /api/errors/{id}, SPEC 12.5).
// Диалоговая механика (role=dialog, aria-modal, focus-trap, ESC, клик вне панели) —
// в shared/ui/dialog.js; здесь только наполнение.
import { createDialog } from "../shared/ui/dialog.js";
import { esc } from "../shared/lib/esc.js";
import { dumpSections, noDumpText } from "../entities/error.js";

export function createErrorsDrawer() {
  const dlg = createDialog({ mode: "drawer", label: "Дамп ошибки" });
  dlg.el.innerHTML = `
    <header><b data-el="title">Ошибка</b>
      <button type="button" class="close" data-el="close" aria-label="Закрыть">×</button></header>
    <div class="body" data-el="body"></div>`;
  document.body.appendChild(dlg.el);
  dlg.el.querySelector("[data-el=close]").addEventListener("click", () => dlg.close());

  function open(err) {
    dlg.el.querySelector("[data-el=title]").textContent = err.ts + " · " + err.alias;
    const body = dlg.el.querySelector("[data-el=body]");
    const sections = dumpSections(err);
    body.innerHTML = sections
      ? sections.map(s =>
          `<div><div class="dump-label">${esc(s.label)}</div><pre class="dump">${esc(s.text)}</pre></div>`).join("")
      : `<div class="dump-label">ошибка</div><pre class="dump">${esc(noDumpText(err))}</pre>`;
    dlg.open();
  }

  return { open, close: () => dlg.close() };
}
