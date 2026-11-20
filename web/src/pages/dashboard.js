// pages/dashboard.js — экран ④ «Дашборд токенов» (SPEC 12.4): сводка, стек-график
// по дням, таблица по алиасам + фильтры client/alias/период.
// Данные — GET /api/usage?from&to&group_by=day|alias → renderChart()/renderUsageByAlias().
import { api } from "../shared/api/client.js";
import { esc } from "../shared/lib/esc.js";
import { fmtInt } from "../shared/lib/fmt.js";
import { showNotice } from "../shared/ui/dialog.js";
import { createCard } from "../shared/ui/card.js";
import { createTable } from "../shared/ui/table.js";
import { parseUsage, chartMax, chartHeights } from "../entities/usage.js";
import { createUsageFilter } from "../features/usage-filter.js";

export function mountDashboard(section) {
  const summary = document.createElement("div");
  summary.className = "summary";
  section.appendChild(summary);

  const chartCard = createCard(`Токены по дням <span class="h3-plain">(GET /api/usage?from&amp;to&amp;group_by=day)</span>`);
  chartCard.className = "card mb-14";
  const chart = document.createElement("div");
  chart.className = "chart";
  const chartX = document.createElement("div");
  chartX.className = "chart-x";
  const legend = document.createElement("div");
  legend.className = "legend";
  // цвета чипов = цвета сегментов графика; литерал #2f81f7 из эталона — это --reasoning
  legend.innerHTML = `<span><i class="bg-reasoning"></i>prompt</span>
    <span><i class="bg-cache"></i>cached</span>
    <span><i class="bg-ok"></i>completion</span>`;
  chartCard.append(chart, chartX, legend);
  section.appendChild(chartCard);

  const tableCard = createCard(); // карточка без h3, как в эталоне
  const filters = document.createElement("div");
  tableCard.appendChild(filters);
  const table = createTable([
    { label: "алиас / модель" },
    { label: "запр.", num: true }, { label: "prompt", num: true }, { label: "cached", num: true },
    { label: "hit", num: true }, { label: "compl.", num: true }, { label: "reason.", num: true },
    { label: "$ est", num: true }, { label: "glm cr", num: true },
  ]);
  tableCard.appendChild(table.el);
  section.appendChild(tableCard);

  let days = [];
  let rows = [];

  function renderChart() {
    const max = chartMax(days);
    chart.innerHTML = days.map(d => {
      const [h1, h2, h3] = chartHeights(d, max); // cached входит в prompt: uncached+cached+compl
      return `<div class="col" title="${esc(d[0])}: prompt ${esc(d[1])}k (cached ${esc(d[2])}k), completion ${esc(d[3])}k">
      <i class="bg-ok" style="height:${esc(h3)}%"></i><i class="bg-cache" style="height:${esc(h2)}%"></i><i class="bg-reasoning" style="height:${esc(h1)}%"></i></div>`;
    }).join("");
    chartX.innerHTML = days.map(d => `<span>${esc(d[0])}</span>`).join("");
  }

  function renderUsageByAlias() {
    table.tbody.innerHTML = rows.map(r => `
    <tr><td><b class="mono">${esc(r.alias)}</b><div class="sub">${esc(r.model)}</div></td>
    <td class="num">${esc(fmtInt(r.req))}</td><td class="num">${esc(r.prompt)}</td><td class="num">${esc(r.cached)}</td>
    <td class="num">${esc(r.hit)}%</td><td class="num">${esc(r.compl)}</td><td class="num">${esc(r.reason)}</td>
    <td class="num">${esc(r.usd)}</td><td class="num">${esc(r.cr)}</td></tr>`).join("");
  }

  async function load() {
    const usage = parseUsage(await api("/api/usage"));
    summary.innerHTML = usage.summary.map(s => `
      <div class="stat"><div class="v">${esc(s.value)}${s.unit ? "<small>" + esc(s.unit) + "</small>" : ""}</div><div class="l">${esc(s.label)}</div></div>`).join("");
    days = usage.days;
    rows = usage.byAlias;
    renderChart();
    renderUsageByAlias();
    if (!filters.childElementCount) { // панель строится один раз — обработчики не дублируются
      createUsageFilter(filters, usage.filters, {
        onAlias: value => {
          rows = value === "все" ? usage.byAlias : usage.byAlias.filter(r => r.alias === value);
          renderUsageByAlias();
        },
        onPeriod: value => {
          const n = parseInt(value, 10) || 0;
          days = n ? usage.days.slice(-n) : usage.days;
          renderChart();
        },
        onExport: () => showNotice("Экспорт CSV в v1 не входит (SPEC 15)."),
      });
    }
  }
  load().catch(err => showNotice("Не удалось загрузить usage: " + err.message, { title: "API" }));
  return {};
}
