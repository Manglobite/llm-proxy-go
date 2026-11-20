// entities/account.js — форма GET /api/status (аккаунты + socks) и деривации
// для экрана «Провайдеры» (SPEC 12.1). Без DOM: только данные и их производные.

export function parseStatus(json) {
  return {
    socks: Array.isArray(json?.socks) ? json.socks : [],
    accounts: Array.isArray(json?.accounts) ? json.accounts : [],
    clientKeys: Array.isArray(json?.client_keys) ? json.client_keys : [],
    meta: json?.meta ?? {},
  };
}

export function socksByName(socks, name) {
  return socks.find(s => s.name === name) ?? null;
}

// строка «egress ip»: факт = expected ✓ / факт ≠ expected (как в эталоне)
export function egressIpText(s) {
  if (!s.egress_ip) return "—";
  if (!s.expected_ip) return s.egress_ip;
  return s.match ? s.egress_ip + " = expected ✓" : s.egress_ip + " ≠ expected";
}

export function checkText(s) {
  return [s.checked_ago, s.check_interval].filter(Boolean).join(" · ") || "—";
}

export function socksBadge(s) {
  if (s.state === "unchecked") return { cls: "muted", text: "unchecked" };
  return s.state === "ok" ? { cls: "ok", text: "healthy" } : { cls: "err", text: "blocked" };
}

// бейдж SOCKS у аккаунта: заблокированный путь → err; direct-аккаунт — warn с объяснением
export function accountSocksBadge(account, path) {
  if (account.allow_direct || !account.socks)
    return { cls: "warn", text: "direct · allow_direct",
             title: "запросы идут с IP машины, минуя SOCKS (allow_direct)" };
  const text = "socks: " + account.socks;
  if (path?.state === "unchecked") return { cls: "muted", text: text + " · unchecked" };
  return { cls: path ? socksBadge(path).cls : "ok", text };
}

// «codex · oauth», «glm · api_key · coding plan», «ollama · api_key»
export function authLine(account) {
  return [account.provider, account.auth, account.plan].filter(Boolean).join(" · ");
}

// сводка сайдбара: «SOCKS: 1 ok · 1 blocked»
export function socksSummary(socks) {
  const ok = socks.filter(s => s.state === "ok").length;
  const unchecked = socks.filter(s => s.state === "unchecked").length;
  const blocked = socks.length - ok - unchecked;
  return "SOCKS: " + ok + " ok · " + blocked + " blocked" + (unchecked ? " · " + unchecked + " unchecked" : "");
}
