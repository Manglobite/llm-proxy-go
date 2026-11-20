// shared/ui/card.js — карточка эталона: .card > h3 (+ содержимое).
// titleHtml — статическая разметка заголовка из кода страницы (не данные);
// контент вызывающий кладёт после h3 напрямую, без лишних обёрток (как в эталоне).
export function createCard(titleHtml = "") {
  const el = document.createElement("div");
  el.className = "card";
  if (titleHtml) {
    const h = document.createElement("h3");
    h.innerHTML = titleHtml;
    el.appendChild(h);
  }
  return el;
}
