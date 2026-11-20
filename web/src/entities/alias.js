// entities/alias.js — форма GET /api/aliases (SPEC 12.2).
// Создание/правка в v1 — config.json + SIGHUP, поэтому сущность только читается.
export function parseAliases(json) {
  return Array.isArray(json) ? json : (json?.aliases ?? []);
}
