// shared/ui/badge.js — бейдж статуса эталона (ok/err/warn/muted).
// Текст обязателен: статус передаётся не только цветом (CODING 2.3).
// Единственный импорт внутри shared/ui: esc из shared/lib — иначе компонент
// не сможет гарантировать экранирование текста без дублирования esc (CODING 2.5).
import { esc } from "../lib/esc.js";

export function badgeHtml(cls, text, title = "") {
  // Конкатенация вместо шаблона: в эталоне был ${t}-переносчик готового
  // атрибута — grep-гейт WU-2 требует esc()/badgeHtml() в КАЖДОЙ интерполяции,
  // а повторный esc поверх уже экранированного атрибута исказил бы текст.
  return '<span class="badge ' + esc(cls) + '"' +
    (title ? ' title="' + esc(title) + '"' : "") +
    ">" + esc(text) + "</span>";
}
