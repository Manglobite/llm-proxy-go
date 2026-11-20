// features/usage-filter.js — панель фильтров дашборда (SPEC 12.4: client_key/alias/model/
// account; в эталоне — client/alias/период). В демо: период перерезает график, alias —
// таблицу; client_key в поалиасной форме /api/usage не участвует, поэтому фильтр без
// эффекта (как в эталоне, где селекты отрисовываются без обработчиков).
import { createSelect } from "../shared/ui/select.js";
import { createButton } from "../shared/ui/btn.js";

export function createUsageFilter(container, filters, { onAlias, onPeriod, onExport } = {}) {
  container.className = "filters";
  const labelOf = text => { const l = document.createElement("label"); l.textContent = text; return l; };

  container.append(
    labelOf("client"), createSelect(filters.client ?? []),
    labelOf("alias"),  createSelect(filters.alias ?? []),
    labelOf("период"), createSelect(filters.period ?? []),
  );

  const spacer = document.createElement("span");
  spacer.className = "flex-1";
  const exportBtn = createButton({ text: "Экспорт CSV", small: true, onClick: () => onExport?.() });
  container.append(spacer, exportBtn);

  // selects[0] (client) — без обработчика: в демо-данных byAlias нет связи с client_key
  const selects = container.querySelectorAll("select");
  selects[1]?.addEventListener("change", e => onAlias?.(e.target.value));
  selects[2]?.addEventListener("change", e => onPeriod?.(e.target.value));
}
