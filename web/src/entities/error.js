// entities/error.js — формы GET /api/errors?from&to и GET /api/errors/{id} (SPEC 12.5),
// подготовка секций дампа для drawer.

export function parseErrors(json) {
  return Array.isArray(json) ? json : (json?.errors ?? []);
}

export function parseErrorDetail(json) {
  return json ?? null;
}

// класс stage-чипа: имя класса = значение stage (как в эталоне: .stage.upstream, .stage.stream…)
export function stageClass(stage) {
  return "stage " + stage;
}

// секции дампа для drawer; null — дамп не сохранился (запрос к апстриму не начинался)
export function dumpSections(err) {
  if (!err?.dump) return null;
  const pretty = s => { try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; } };
  return [
    { label: "запрос · " + err.dump.method + " " + err.dump.url, text: pretty(err.dump.reqHeaders) },
    { label: "тело запроса (кап 1 МБ)", text: err.dump.reqBody },
    { label: "ответ · HTTP " + err.status, text: pretty(err.dump.respHeaders) },
    { label: "тело ответа", text: err.dump.respBody },
  ];
}

export function noDumpText(err) {
  return err.err + "\n\nДамп запроса не сохранился: запрос к апстриму не начинался (stage=" + err.stage + ").";
}
