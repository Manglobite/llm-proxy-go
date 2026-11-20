import { api } from "../shared/api/client.js";
import { parseAliases } from "../entities/alias.js";
import { createCard } from "../shared/ui/card.js";

export function mountConnection(section) {
  const connection = createCard("URL для подключения");
  const url = document.createElement("pre");
  url.className = "dump";
  url.textContent = new URL("/v1", window.location.origin).href;
  const hint = document.createElement("p");
  hint.textContent = "Для запросов к API используйте клиентский ключ, а не токен WebUI.";
  connection.append(url, hint);

  const models = createCard("Список моделей в формате /v1/models");
  const status = document.createElement("div");
  status.setAttribute("role", "status");
  const json = document.createElement("pre");
  json.className = "dump";
  json.hidden = true;
  models.append(status, json);
  section.append(connection, models);

  let loading = false;
  async function refresh() {
    if (loading) return;
    loading = true;
    status.textContent = "Загрузка списка моделей…";
    json.hidden = true;
    try {
      const aliases = parseAliases(await api("/api/aliases"));
      const data = aliases.map(({ name, account }) => ({ id: name, object: "model", owned_by: account }));
      json.textContent = JSON.stringify({ object: "list", data }, null, 2);
      json.hidden = false;
      status.textContent = "";
    } catch (err) {
      status.textContent = "Не удалось загрузить список моделей: " + err.message;
    } finally {
      loading = false;
    }
  }
  return { refresh };
}
