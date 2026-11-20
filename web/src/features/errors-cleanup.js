// features/errors-cleanup.js — ретеншн ошибок (SPEC 12.5): «удалить старше N дней»
// + «Очистить» → POST /api/errors/cleanup {older_than}.
import { createSelect } from "../shared/ui/select.js";
import { createButton } from "../shared/ui/btn.js";

const OPTIONS = ["удалить старше 7 дней", "старше 30 дней"];

export function createErrorsCleanup(container, { onCleanup } = {}) {
  const wrap = document.createElement("div");
  wrap.className = "flex-gap8";
  const select = createSelect(OPTIONS);
  const btn = createButton({
    text: "Очистить",
    small: true,
    onClick: () => onCleanup?.(select.selectedIndex === 1 ? "30d" : "7d"),
  });
  wrap.append(select, btn);
  container.appendChild(wrap);
}
