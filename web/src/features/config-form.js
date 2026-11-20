import { api } from "../shared/api/client.js";
import { createDialog } from "../shared/ui/dialog.js";
import { createInput } from "../shared/ui/input.js";
import { createSelect } from "../shared/ui/select.js";
import { createButton } from "../shared/ui/btn.js";

function errorMessage(err) {
  const msg = err.message.replace(/^\d+: /, "");
  try { return JSON.parse(msg).error || msg; }
  catch { return msg; }
}

function openForm(title, endpoint, build, onSuccess, method = "POST", submitText = "Сохранить") {
  const dlg = createDialog({ label: title, onClose: () => dlg.el.remove() });
  const form = document.createElement("form");
  form.className = "modal";
  form.style.maxHeight = "90vh";
  form.style.overflowY = "auto";
  const heading = document.createElement("h2");
  heading.textContent = title;
  form.append(heading);
  const field = (text, control) => {
    const label = document.createElement("label");
    label.style.display = "block";
    label.className = "mb-10";
    const caption = document.createElement("span");
    caption.style.display = "block";
    caption.textContent = text;
    label.append(caption, control);
    form.append(label);
    return label;
  };
  const payload = build(field, form);
  const error = document.createElement("p");
  error.setAttribute("role", "alert");
  error.hidden = true;
  const row = document.createElement("div");
  row.className = "flex-gap8";
  const save = createButton({ text: submitText, variant: "primary", disabled: !payload });
  save.type = "submit";
  row.append(save, createButton({ text: "Отмена", onClick: dlg.close }));
  form.append(error, row);
  form.addEventListener("input", () => { error.hidden = true; });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    save.disabled = true;
    error.hidden = true;
    let result;
    try {
      result = await api(endpoint, { method, body: payload() });
    } catch (err) {
      error.textContent = errorMessage(err);
      error.hidden = false;
      return;
    } finally {
      save.disabled = false;
    }
    dlg.close();
    onSuccess?.(result);
  });
  dlg.el.append(form);
  document.body.append(dlg.el);
  dlg.open();
  return dlg;
}

function text(placeholder, required = false, type = "text") {
  const el = createInput({ placeholder });
  el.type = type;
  el.required = required;
  return el;
}

export function revokeClientForm(name, { onSuccess } = {}) {
  return openForm("Отозвать клиентский ключ", "/api/config/clients/revoke", (_, form) => {
    const hint = document.createElement("p");
    hint.textContent = "Ключ клиента «" + name + "» сразу перестанет работать. Статистика сохранится.";
    form.append(hint);
    return () => ({ name });
  }, onSuccess, "POST", "Отозвать ключ");
}

export function createClientForm({ onSuccess } = {}) {
  return openForm("Добавить клиентский ключ", "/api/config/clients", field => {
    const name = text("opencode", true);
    name.pattern = "\\s*\\S+\\s*";
    name.maxLength = 128;
    field("Имя клиента (без пробелов)", name);
    return () => ({ name: name.value.trim() });
  }, ({ key }) => {
    const title = "Клиентский ключ создан";
    const dlg = createDialog({ label: title, onClose: () => dlg.el.remove() });
    const modal = document.createElement("div");
    modal.className = "modal";
    const heading = document.createElement("h2");
    heading.textContent = title;
    const hint = document.createElement("p");
    hint.textContent = "Скопируйте ключ в настройки клиента. После закрытия он больше не отображается в WebUI.";
    const value = createInput({ value: key });
    value.className = "mb-10";
    value.style.width = "100%";
    value.readOnly = true;
    value.autocomplete = "off";
    value.setAttribute("aria-label", "Клиентский API-ключ");
    const row = document.createElement("div");
    row.className = "flex-gap8";
    row.append(createButton({ text: "Скопировать", onClick: async () => {
      value.focus();
      value.select();
      try {
        await navigator.clipboard.writeText(key);
        hint.textContent = "Ключ скопирован. После закрытия он больше не отображается в WebUI.";
      } catch {
        hint.textContent = "Ключ выделен: нажмите Ctrl+C или ⌘C, чтобы скопировать.";
      }
    } }), createButton({ text: "Закрыть", onClick: dlg.close }));
    modal.append(heading, hint, value, row);
    dlg.el.append(modal);
    document.body.append(dlg.el);
    dlg.open();
    value.select();
    onSuccess?.();
  });
}

export function createSocksForm({ path, onSuccess } = {}) {
  const endpoint = "/api/config/socks";
  return openForm(path ? "Изменить SOCKS-путь" : "Добавить SOCKS-путь", endpoint, (field, form) => {
    form.autocomplete = "off";
    const name = text("work", true);
    name.readOnly = !!path;
    name.value = path?.name ?? "";
    name.pattern = "\\s*\\S+\\s*";
    const url = text("socks5h://host:1080", true);
    url.pattern = ".*\\S.*";
    const login = text("Необязательно");
    const password = text("Необязательно", false, "password");
    password.autocomplete = "new-password";
    if (path) {
      const parsed = new URL(path.url);
      login.value = decodeURIComponent(parsed.username);
      password.value = decodeURIComponent(parsed.password);
      parsed.username = "";
      parsed.password = "";
      url.value = parsed.href;
    }
    const ip = text("Необязательно");
    ip.value = path?.expected_ip ?? "";
    const checkURL = text("По умолчанию: https://api.ipify.org", false, "url");
    checkURL.value = path?.check_url ?? "";
    const interval = text("По умолчанию: 60s");
    interval.value = path?.check_interval_raw ?? "";
    field("Имя SOCKS-пути", name);
    field("SOCKS URL (учётные данные из URL имеют приоритет)", url);
    field("Логин", login);
    field("Пароль", password);
    field("Ожидаемый IP выхода", ip);
    field("URL проверки IP", checkURL);
    field("Интервал проверки (например, 60s)", interval);

    const payload = () => {
      const raw = url.value.trim();
      const socks = { url: raw.includes("://") ? raw : "socks5h://" + raw };
      if (ip.value.trim()) socks.expected_ip = ip.value.trim();
      if (checkURL.value.trim()) socks.check_url = checkURL.value.trim();
      if (interval.value.trim()) socks.check_interval = interval.value.trim();
      if (!/^[^:]+:\/\/[^/?#]*@/.test(socks.url)) {
        if (login.value) socks.login = login.value;
        if (password.value) socks.password = password.value;
      }
      return { name: path?.name ?? name.value.trim(), socks };
    };

    const result = document.createElement("p");
    result.setAttribute("role", "status");
    result.hidden = true;
    const showResult = (message, failed = false) => {
      result.textContent = message;
      result.className = failed ? "err-text" : "note";
      result.hidden = false;
    };
    let revision = 0;
    const check = createButton({ text: "Проверить SOCKS", onClick: async () => {
      if (!form.reportValidity()) return;
      const current = ++revision;
      check.disabled = true;
      showResult("Проверка…");
      try {
        const response = await api(endpoint + "/check", { method: "POST", body: payload() });
        if (current !== revision || !form.isConnected) return;
        if (response.ok) showResult(response.ip ? "IP выхода: " + response.ip : "Проверка пройдена");
        else showResult((response.error || "Проверка не пройдена") + (response.ip ? " · IP выхода: " + response.ip : ""), true);
      } catch (err) {
        if (current === revision && form.isConnected) showResult(errorMessage(err), true);
      } finally {
        if (current === revision) check.disabled = false;
      }
    } });
    form.addEventListener("input", () => {
      revision++;
      check.disabled = false;
      result.hidden = true;
      result.textContent = "";
    });
    form.append(check, result);
    return payload;
  }, onSuccess, path ? "PUT" : "POST");
}

export function createProviderForm(status, { onSuccess } = {}) {
  return openForm("Добавить провайдера", "/api/config/providers", (field, form) => {
    const name = text("ollama", true);
    const type = createSelect(["openai-like", "glm", "codex", "zcode"]);
    const base = text("http://127.0.0.1:11434", false, "url");
    const id = text("main", true);
    const socks = createSelect([...status.socks.map(s => s.name), "none"]);
    const key = text("Не нужен для локальной Ollama", false, "password");
    key.autocomplete = "off";
    const email = text("Необязательно", false, "email");
    const port = text("1455", false, "number");
    port.min = "1";
    port.max = "65535";
    const direct = text("", false, "checkbox");
    const plan = text("", false, "checkbox");
    const route = createSelect(["pat", "zcode_plan"]);
    field("Имя провайдера", name);
    field("Тип провайдера", type);
    const baseField = field("Базовый URL без /v1", base);
    field("ID аккаунта", id);
    field("SOCKS-путь", socks);
    const directField = field("Разрешить прямой выход к внешнему серверу", direct);
    const keyField = field("API-ключ провайдера", key);
    const emailField = field("Электронная почта", email);
    const portField = field("Порт обратного вызова", port);
    const planField = field("GLM Coding Plan", plan);
    const routeField = field("Маршрут ZCode", route);
    const hint = document.createElement("p");
    hint.className = "note";
    form.append(hint);
    const toggle = (label, control, show) => {
      label.hidden = !show;
      label.style.display = show ? "block" : "none";
      control.disabled = !show;
    };
    function update() {
      const openai = type.value === "openai-like";
      const oauth = type.value === "codex" || type.value === "zcode";
      toggle(baseField, base, openai);
      base.required = openai;
      toggle(keyField, key, !oauth);
      key.required = type.value === "glm";
      toggle(emailField, email, oauth);
      toggle(portField, port, type.value === "codex");
      toggle(planField, plan, type.value === "glm");
      toggle(routeField, route, type.value === "zcode");
      toggle(directField, direct, openai && socks.value === "none");
      if (direct.disabled) direct.checked = false;
      hint.textContent = !openai && socks.value === "none" ?
        "Для этого типа выберите SOCKS-путь. Если путей нет, добавьте его в разделе «SOCKS-пути»." :
        oauth ? "После сохранения нажмите «Начать OAuth» в карточке аккаунта." : "";
    }
    type.addEventListener("change", update);
    socks.addEventListener("change", update);
    update();
    return () => {
      const provider = { type: type.value };
      const oauth = type.value === "codex" || type.value === "zcode";
      const account = { id: id.value.trim(), socks: socks.value, auth: { method: oauth ? "oauth" : "api_key" } };
      if (!base.disabled) provider.base_url = base.value.trim();
      if (!route.disabled) provider.route = route.value;
      if (!key.disabled && key.value.trim()) account.auth.key = key.value.trim();
      if (!email.disabled && email.value.trim()) account.email = email.value.trim();
      if (!port.disabled && port.value) account.auth.callback_port = Number(port.value);
      if (!direct.disabled && direct.checked) account.allow_direct = true;
      if (!plan.disabled && plan.checked) account.coding_plan = true;
      provider.accounts = [account];
      return { name: name.value.trim(), provider };
    };
  }, onSuccess);
}

export function createAliasForm(accounts, { onSuccess } = {}) {
  return openForm("Создать алиас", "/api/config/aliases", (field, form) => {
    if (!accounts.length) {
      const hint = document.createElement("p");
      hint.textContent = "Сначала добавьте провайдера на экране «Провайдеры и аккаунты».";
      form.append(hint);
      return null;
    }
    const name = text("local/qwen", true);
    const account = createSelect(accounts.map(a => a.provider + "/" + a.id));
    const model = text("qwen3:8b", true);
    field("Имя алиаса", name);
    field("Аккаунт", account);
    const search = text("Часть ID модели", false, "search");
    const choices = createSelect([]);
    const status = document.createElement("p");
    status.setAttribute("role", "status");
    form.append(status);
    field("Поиск моделей", search);
    field("Модели аккаунта", choices);
    field("Модель апстрима (можно ввести вручную)", model);

    let models = [];
    let revision = 0;
    function showStatus(message, failed = false) {
      status.textContent = message;
      status.className = failed ? "err-text" : "note";
    }
    function setChoices(matches) {
      const placeholder = new Option("Выберите модель", "");
      placeholder.disabled = true;
      choices.replaceChildren(placeholder, ...matches.map(id => new Option(id, id)));
      choices.value = "";
      choices.disabled = !matches.length;
    }
    function filterModels() {
      const query = search.value.trim().toLowerCase();
      const matches = models.filter(id => id.toLowerCase().includes(query));
      setChoices(matches);
      showStatus(matches.length ? "Найдено моделей: " + matches.length : "Нет моделей по этому запросу.");
    }
    async function loadModels() {
      const current = ++revision;
      models = [];
      model.value = "";
      search.value = "";
      search.disabled = true;
      setChoices([]);
      showStatus("Загрузка моделей…");
      try {
        const response = await api("/api/config/models?account=" + encodeURIComponent(account.value));
        if (current !== revision || !form.isConnected) return;
        if (response?.error) throw new Error(response.error);
        if (!Array.isArray(response?.models) || response.models.some(id => typeof id !== "string")) {
          throw new Error("Некорректный ответ сервера");
        }
        models = response.models;
        if (!models.length) {
          showStatus("В этом аккаунте нет доступных моделей.");
          return;
        }
        search.disabled = false;
        filterModels();
      } catch (err) {
        if (current !== revision || !form.isConnected) return;
        showStatus("Не удалось загрузить модели: " + errorMessage(err), true);
      }
    }
    account.addEventListener("change", loadModels);
    search.addEventListener("input", filterModels);
    search.addEventListener("keydown", event => {
      if (event.key === "Enter" && !event.isComposing) event.preventDefault();
    });
    choices.addEventListener("change", () => { model.value = choices.value; });
    model.addEventListener("input", () => { choices.value = ""; });
    loadModels();
    return () => ({ name: name.value.trim(), alias: { account: account.value, model: model.value.trim() } });
  }, onSuccess);
}
