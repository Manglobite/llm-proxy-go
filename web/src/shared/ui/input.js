// shared/ui/input.js — текстовое поле эталона (input[type=text] стилизуется в ui.css).
export function createInput({ placeholder = "", value = "", flex = false } = {}) {
  const input = document.createElement("input");
  input.type = "text";
  if (placeholder) input.placeholder = placeholder;
  input.value = value;
  if (flex) input.className = "flex-1"; // поле вставки redirect-URL растёт на всю строку (эталон)
  return input;
}
