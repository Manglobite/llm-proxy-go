// entities/quota.js — форма GET /api/quotas (SPEC 12.3) и классы уровней заливки.
// Уровень (ok/mid/high) едет в данных: в эталоне расставлен вручную
// (31% → lv-mid при 35% → lv-ok), чистого порога нет — не выдумываем.
export function parseQuotas(json) {
  return {
    cards: json?.cards ?? [],
    rateLimits: json?.rate_limits ?? { title: "", rows: [] },
  };
}

// класс полосы .qbar (виджет quota-bar)
export const QLEVEL_CLASS = { ok: "lv-ok", mid: "lv-mid", high: "lv-high" };
// класс заливки мини-баров строки аккаунта (виджет account-card)
export const QLEVEL_FILL = { ok: "bg-ok", mid: "bg-warn", high: "bg-err" };
