// shared/ui/table.js — таблица эталона с thead/th scope="col" (CODING 2.3).
// cols: [{ label, num }] — num даёт класс .num (правая колонка, mono).
// labels — статические подписи из кода страницы, не данные; строка собирается
// конкатенацией: вложенные backtick-шаблоны в интерполяциях запрещены
// (grep-гейт WU-2), esc — тот же принцип, что в badge (CODING 2.5).
import { esc } from "../lib/esc.js";

export function createTable(cols) {
  const el = document.createElement("table");
  const thead = document.createElement("thead");
  thead.innerHTML = "<tr>" + cols.map(c =>
    '<th scope="col"' + (c.num ? ' class="num"' : "") + ">" + esc(c.label) + "</th>").join("") + "</tr>";
  const tbody = document.createElement("tbody");
  el.append(thead, tbody);
  return { el, tbody };
}
