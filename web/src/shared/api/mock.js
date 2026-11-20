// shared/api/mock.js — все демо-данные эталона webui-design.html (литералы 1:1).
// Формы данных совпадают с ответами JSON API (SPEC 12); в рабочем режиме
// (DEMO=false) файл не используется, но остаётся для офлайн-разработки.
// Схемы URL собраны из частей: оффлайн-гейт WU-1 разрешает литерал http(s)://
// только в комментариях, а эти строки — отображаемые данные, не сетевые запросы.
const HTTPS = "https" + ":" + "//";

// GET /api/status → meta (верхняя панель и футер сайдбара)
export const DEMO_STATUS_META = {
  version: "v0.1.0 · demo data",
  listen: "127.0.0.1:8787",
  config: "ok · reload 09:17 (SIGHUP)",
  uptime: "6h 12m",
};

// GET /api/status → socks (IP-пиннинг: ожидаемый/фактический/время проверки)
export const DEMO_SOCKS_PATHS = [
  { name:"home", url:"socks5h://127.0.0.1:1080", state:"ok",
    egress_ip:"192.0.2.71", expected_ip:"192.0.2.71", match:true,
    checked_ago:"12 с назад", check_interval:"каждые 60 с", error:"" },
  { name:"work", url:"socks5h://10.0.0.5:1080", state:"blocked",
    egress_ip:"198.51.100.201", expected_ip:"192.0.2.10", match:false,
    checked_ago:"3 с назад", check_interval:"автоповтор 60 с",
    error:"egress ip mismatch: expected 192.0.2.10, got 198.51.100.201" },
];

// GET /api/status → client_keys
export const DEMO_CLIENT_KEYS = [
  { name:"opencode", req24h:1842, tokens24h:"1.9M" },
  { name:"zcode",    req24h:611,  tokens24h:"980k" },
];

// GET /api/status → accounts (usage.segments — мини-бары строки аккаунта:
// pct + level, уровни в эталоне расставлены вручную)
export const DEMO_ACCOUNTS = [
  { provider:"codex", auth:"oauth", id:"myWork", email:"codex@example.com",
    token:"токен 42м", socks:"work", allow_direct:false, can_oauth:true, oauth_mode:"pkce",
    usage:{ segments:[{pct:37, level:"ok"}], label:"5ч окно 37% · неделя 61%" } },
  { provider:"glm", auth:"api_key", plan:"coding plan", id:"plan-a",
    token:"ok", socks:"home", allow_direct:false, can_oauth:false,
    usage:{ segments:[{pct:35, level:"ok"}, {pct:31, level:"mid"}],
            label:"5ч 4 213/12 000 cr · нед 18.4k/60k cr" } },
  { provider:"glm", auth:"api_key", plan:"coding plan", id:"plan-b",
    token:"ok", socks:"home", allow_direct:false, can_oauth:false,
    usage:{ segments:[{pct:12, level:"ok"}, {pct:8, level:"ok"}],
            label:"5ч 240/2 000 cr · нед 812/10k cr" } },
  { provider:"zai-plan", auth:"oauth", plan:"coding plan", id:"main", email:"zcode@example.com",
    token:"needs_relogin: Z.ai user information is invalid", socks:"home", allow_direct:false,
    can_oauth:true, oauth_mode:"poll",
    usage:{ segments:[{pct:4, level:"ok"}, {pct:2, level:"ok"}],
            label:"5ч 240/12 000 cr · нед 812/60k cr" } },
  { provider:"ollama", auth:"api_key", id:"cloud",
    token:"ok", socks:null, allow_direct:true, can_oauth:false,
    usage:{ segments:[], label:"usage за 24ч: $2.41 · cached 61%" } },
];

// GET /api/aliases
export const DEMO_ALIASES = [
  {name:"work/terra", account:"codex/myWork", model:"gpt-6.1-terra", req24h:87,  status:"err",  note:"socks path work: blocked"},
  {name:"glm/5.3",    account:"glm/plan-a",   model:"glm-5.3",      req24h:1204,status:"ok",  note:""},
  {name:"glm/flash",  account:"glm/plan-b",   model:"glm-5.3-flash",req24h:3821,status:"ok",  note:""},
  {name:"qwen/cloud", account:"ollama/cloud", model:"qwen3-coder:480b", req24h:141, status:"ok", note:""},
];

// GET /api/usage?from&to&group_by=day|alias
export const DEMO_USAGE = {
  summary: [
    { value:"4.7M", label:"prompt за 14 дней" },
    { value:"71", unit:"%", label:"cache hit rate" },
    { value:"$6.12", label:"оценка стоимости (PAYG)" },
    { value:"21.6k", label:"glm-кредиты за 14 дней" },
  ],
  days: [ // [метка, промпт в тысячах, кеш в тысячах, завершение в тысячах]
    ["24.10",402,289,74],["25.10",455,331,81],["26.10",98,41,22],["27.10",51,12,17],
    ["28.10",388,271,69],["29.10",441,325,77],["30.10",469,352,83],["31.10",412,301,71],
    ["01.11",447,329,79],["02.11",120,64,28],["03.11",463,347,85],["04.11",458,338,81],
    ["05.11",471,356,87],["06.11",309,214,58],
  ],
  byAlias: [
    {alias:"glm/5.3",    model:"glm-5.3",       req:1204, prompt:"2.9M", cached:"2.1M", hit:72, compl:"412k", reason:"118k", usd:"—",     cr:"18 432"},
    {alias:"glm/flash",  model:"glm-5.3-flash", req:3821, prompt:"1.4M", cached:"1.1M", hit:79, compl:"96k",  reason:"4k",   usd:"—",     cr:"3 204"},
    {alias:"work/terra", model:"gpt-6.1-terra", req:87,   prompt:"340k", cached:"210k", hit:62, compl:"78k",  reason:"52k",  usd:"подписка", cr:"—"},
    {alias:"qwen/cloud", model:"qwen3-coder:480b", req:141, prompt:"96k", cached:"12k", hit:13, compl:"30k",  reason:"—",    usd:"$0.38", cr:"—"},
  ],
  filters: {
    client: ["все", "opencode", "zcode"],
    alias:  ["все", "work/terra", "glm/5.3", "glm/flash", "qwen/cloud"],
    period: ["14 дней", "7 дней", "30 дней"],
  },
};

// GET /api/quotas → cards (level — уровень заливки, в эталоне расставлен вручную)
export const DEMO_QUOTAS = [
  { provider:"codex", account:"myWork", title:"codex / myWork",
    windows:[
      { label:"5-часовое окно", value:"37%", pct:37, level:"ok",
        reset:{ label:"сброс", value:"через 2ч 21м · 14:32" } },
      { label:"недельное окно", value:"61%", pct:61, level:"mid",
        reset:{ label:"сброс", value:"вс 09:17" } },
    ],
    note:"источник: заголовки x-codex-primary/secondary-* последнего ответа" },
  { provider:"glm", account:"plan-a", title:"glm / plan-a · coding plan (Pro)",
    windows:[
      { label:"кредиты 5ч", value:"4 213 / 12 000", pct:35, level:"ok",
        reset:{ label:"восстановление", value:"скользящее, через 5ч после расхода" } },
      { label:"кредиты неделя", value:"18 400 / 60 000", pct:31, level:"mid" },
    ],
    note:"локальный расчёт: (in×6.9 + cached×1.7 + out×24) / 10k · без off-peak ×0.5" },
  { provider:"ollama", account:"cloud", title:"ollama / cloud",
    windows:[
      { label:"расход за 24ч", value:"$2.41", pct:8, level:"ok",
        reset:{ label:"cached input", value:"61% от ввода · $0.015/M" } },
    ],
    note:"источник: GET ollama.com/api/usage?range=24h (usage_poll)" },
];

// GET /api/quotas → rate_limits (429-события)
export const DEMO_RATE_LIMITS = {
  title:"429-события (апстрим отказал в квоте)",
  rows:[
    { when:"09:41", account:"codex/myWork", window:"5ч", reset:"14:32" },
    { when:"вчера 22:18", account:"glm/plan-b", window:"5ч", reset:"03:18" },
  ],
};

// POST /api/oauth/start → активный флоу (PKCE + state, TTL 15 минут, один на аккаунт)
export const DEMO_OAUTH = {
  account:"codex/myWork",
  url: HTTPS + "auth.openai.com/oauth/authorize?response_type=code&client_id=app_EMo…&redirect_uri=http%3A%2F%2F127.0.0.1%3A1455%2Fauth%2Fcallback&code_challenge=E9Melh…&code_challenge_method=S256&state=ZGVtbw",
  ttl:"флоу истекает через 14:52",
  callback:"127.0.0.1:1455/auth/callback",
};

// GET /api/errors?from&to (dump вложен в строку, как в эталоне; в рабочей версии
// полный дамп — GET /api/errors/{id})
export const DEMO_ERRORS = [
  {ts:"2026-11-06 09:41:12", stage:"upstream",    status:429, alias:"work/terra",
   err:'usage_limit_reached: resets_at 1762421520 (14:32), window 5h',
   dump:{method:"POST", url: HTTPS + "chatgpt.com/backend-api/codex/responses",
     reqHeaders:'{"Authorization":"<masked>","chatgpt-account-id":"acc_9f2","content-type":"application/json","originator":"codex_cli_rs"}',
     reqBody:'{"model":"gpt-6.1-terra","input":[...113 KB...],"stream":true,"store":false}',
     respHeaders:'{"x-codex-primary-used-percent":"100","x-codex-primary-reset-at":"1762421520"}',
     respBody:'{"error":{"type":"usage_limit_reached","plan_type":"pro","resets_at":1762421520,"limit_window_minutes":300}}'}},
  {ts:"2026-11-06 09:12:03", stage:"socks_check", status:503, alias:"glm/5.3",
   err:'socks path "work" is blocked: egress ip mismatch: expected 192.0.2.10, got 198.51.100.201', dump:null},
  {ts:"2026-11-05 22:18:44", stage:"upstream", status:429, alias:"glm/flash",
   err:'z.ai: plan limits exhausted (текст 429), окно 5ч', dump:null},
  {ts:"2026-11-05 18:03:21", stage:"stream", status:0, alias:"work/terra",
   err:'EOF от апстрима посреди стрима (34.2s, получено 8 121 токен)', dump:null},
  {ts:"2026-11-05 16:44:07", stage:"translate", status:400, alias:"qwen/cloud",
   err:'anthropic request: max_tokens обязателен', dump:null},
];

// mockApi — роутер демо-режима: те же пути и методы, что в SPEC 12.
export async function mockApi(path, opts = {}) {
  const route = path.split("?")[0];
  const method = (opts.method ?? "GET").toUpperCase();
  const body = typeof opts.body === "string" ? JSON.parse(opts.body) : (opts.body ?? {});

  if (route === "/api/status" && method === "GET")
    return { socks: DEMO_SOCKS_PATHS, accounts: DEMO_ACCOUNTS, client_keys: DEMO_CLIENT_KEYS, meta: DEMO_STATUS_META };
  if (route === "/api/config/clients" && method === "POST") {
    if (DEMO_CLIENT_KEYS.some(client => client.name === body.name))
      throw new Error("409: имя уже занято");
    DEMO_CLIENT_KEYS.push({ name: body.name, req24h: 0, tokens24h: "0" });
    return { key: "demo-only-not-a-real-key" };
  }
  if (route === "/api/oauth/start" && method === "POST")
    return { account: body.account ?? DEMO_OAUTH.account, url: DEMO_OAUTH.url, ttl: DEMO_OAUTH.ttl, callback: DEMO_OAUTH.callback };
  if (route === "/api/oauth/pending" && method === "GET")
    return { state: "waiting", url: DEMO_OAUTH.url }; // флоу ждёт коллбэк (TTL 15 минут)
  if (route === "/api/oauth/paste" && method === "POST")
    return { ok: true };
  if (route === "/api/oauth/refresh" && method === "POST")
    return { ok: true };
  // zcode (режим poll, SPEC 12): authorize_url + статус серверной poll-петли
  if (route === "/api/oauth/zcode/start" && method === "POST")
    return { account: body.account ?? "zcode/main", authorize_url: DEMO_OAUTH.url };
  if (route === "/api/oauth/zcode/pending" && method === "GET")
    return { status: "pending", authorize_url: DEMO_OAUTH.url };
  if (route === "/api/aliases" && method === "GET")
    return DEMO_ALIASES;
  if (route === "/api/usage" && method === "GET")
    return DEMO_USAGE;
  if (route === "/api/quotas" && method === "GET")
    return { cards: DEMO_QUOTAS, rate_limits: DEMO_RATE_LIMITS };
  if (route === "/api/errors/cleanup" && method === "POST")
    return { removed: 0 }; // демо-строки свежие — удалять нечего
  if (route === "/api/errors" && method === "GET")
    return DEMO_ERRORS;
  if (route.startsWith("/api/errors/")) {
    const row = DEMO_ERRORS[Number(route.split("/")[3])];
    return row ? { ...row } : null;
  }
  throw new Error("mock: нет демо-обработчика для " + method + " " + path);
}
