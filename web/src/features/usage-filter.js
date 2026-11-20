// features/usage-filter.js — панель фильтров дашборда (SPEC 12.4: client_key/alias/model/
// account; в эталоне — client/alias/период).
import { createSelect } from "../shared/ui/select.js";
import { createButton } from "../shared/ui/btn.js";

export function createUsageFilter(container, filters, { onClient, onAlias, onPeriod, onExport } = {}) {
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

  const selects = container.querySelectorAll("select");
  selects[0]?.addEventListener("change", e => onClient?.(e.target.value));
  selects[1]?.addEventListener("change", e => onAlias?.(e.target.value));
  selects[2]?.addEventListener("change", e => onPeriod?.(e.target.value));
}
