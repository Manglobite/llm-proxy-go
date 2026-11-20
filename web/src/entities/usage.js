// entities/usage.js — форма GET /api/usage?from&to&group_by=day|alias (SPEC 12.4)
// и математика стек-графика (cached входит в prompt, как в эталоне).

export function parseUsage(json) {
  return {
    summary: json?.summary ?? [],
    days: json?.days ?? [],
    byAlias: json?.byAlias ?? [],
    filters: json?.filters ?? { client: [], alias: [], period: [] },
  };
}

// максимальная высота столба: стек = uncached + cached + completion = prompt + completion
export function chartMax(days) {
  return Math.max(...days.map(d => d[1] + d[3]));
}

// высоты трёх сегментов столба в процентах: [uncached, cached, completion]
export function chartHeights(day, max) {
  const [, p, c, o] = day;
  return [(p - c) / max * 100, c / max * 100, o / max * 100];
}
