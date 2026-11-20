// shared/ui/select.js — select эталона; options — массив строк (значение = подпись, как в эталоне).
export function createSelect(options) {
  const select = document.createElement("select");
  for (const opt of options) {
    const o = document.createElement("option");
    o.value = opt;
    o.textContent = opt;
    select.appendChild(o);
  }
  return select;
}
