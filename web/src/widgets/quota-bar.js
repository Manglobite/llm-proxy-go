// widgets/quota-bar.js — прогресс-блок квоты эталона: qrow (подпись/значение),
// qbar с уровнем заливки и строка сброса (опционально).
import { esc } from "../shared/lib/esc.js";
import { QLEVEL_CLASS } from "../entities/quota.js";

// gapAfter — отбивка 14px перед следующим окном (в эталоне был инлайн margin-bottom).
// Ветвления собраны конкатенацией: вложенные backtick-шаблоны в интерполяциях
// запрещены (grep-гейт WU-2), данные — всегда через esc.
export function quotaBarHtml(w, { gapAfter = false } = {}) {
  const gap = gapAfter ? " mb-14" : ""; // статическая строка; esc — требование гейта WU-2
  return `
  <div class="quota${esc(gap)}">
    <div class="qrow"><span>${esc(w.label)}</span><b>${esc(w.value)}</b></div>
    <div class="qbar"><i class="${esc(QLEVEL_CLASS[w.level] ?? "lv-ok")}" style="width:${esc(w.pct)}%"></i></div>
    ${w.reset ? '<div class="qrow"><span>' + esc(w.reset.label) + "</span><span>" + esc(w.reset.value) + "</span></div>" : ""}
  </div>`;
}
