// widgets/sidebar.js — сайдбар: логотип, навигация экранов, счётчик ошибок,
// футер с версией/адресом и сводкой SOCKS. Активную кнопку и данные наполняет app:
// роутер и глобальные статусы живут в app (CODING 2.2).
export function createSidebar(root) {
  root.insertAdjacentHTML("beforeend", `
  <aside>
    <div class="logo">llm<span>-</span>proxy</div>
    <nav aria-label="Разделы">
      <button type="button" data-screen="accounts" class="active">① Провайдеры</button>
      <button type="button" data-screen="aliases">② Алиасы</button>
      <button type="button" data-screen="quotas">③ Квоты</button>
      <button type="button" data-screen="usage">④ Токены</button>
      <button type="button" data-screen="errors">⑤ Ошибки <span class="count" data-el="err-count">0</span></button>
      <button type="button" data-screen="connection">⑥ Подключение</button>
    </nav>
    <div class="sidebar-footer">
      <div data-el="version"></div>
      <div data-el="socks-summary"></div>
      <div data-el="listen"></div>
    </div>
  </aside>`);

  const el = root.querySelector("aside");
  const nav = el.querySelector("nav");
  const ref = name => el.querySelector("[data-el=" + name + "]");

  return {
    el,
    nav,
    setActive(screen) {
      nav.querySelectorAll("button[data-screen]")
        .forEach(b => b.classList.toggle("active", b.dataset.screen === screen));
    },
    setErrorsCount(n) { ref("err-count").textContent = String(n); },
    setSocksSummary(text) { ref("socks-summary").textContent = text; },
    setMeta(meta) {
      ref("version").textContent = meta.version ?? "";
      ref("listen").textContent = meta.listen ?? "";
    },
  };
}
