// shared/ui/btn.js — кнопка эталона: .btn (+ .primary, .small), инвентарь CODING 2.3.
// Вариант .danger в эталоне не встречается, поэтому стиля для него в ui.css нет.
export function createButton({ text = "", variant = "", small = false, title = "", disabled = false, onClick = null } = {}) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "btn" + (variant ? " " + variant : "") + (small ? " small" : "");
  if (title) b.title = title;
  if (disabled) b.disabled = true;
  b.textContent = text;
  if (onClick) b.addEventListener("click", onClick);
  return b;
}
