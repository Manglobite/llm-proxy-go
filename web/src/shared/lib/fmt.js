// shared/lib/fmt.js — форматирование чисел как в эталоне (ru-локаль: 1 842, 3 821).
export const fmtInt = n => Number(n ?? 0).toLocaleString("ru");
