package translate

import (
	"encoding/json"
	"fmt"
)

// RewriteModel — passthrough-подмена модели (клетки passthrough SPEC 8.2:
// openai→openai, anthropic→anthropic). Тело десериализуется в карту RawMessage:
// заменяется ТОЛЬКО ключ model, все остальные значения остаются байт-в-байт,
// поэтому cache_control (anthropic-блоки и z.ai-маркеры в частях) переживает
// подмену нетронутым (TR-9). Ключи верхнего уровня при этом сортируются —
// это плата encoding/json за сохранность значений; оба протокола порядок
// ключей объекта не фиксируют.
func RewriteModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("passthrough body: %w", err)
	}
	mb, err := json.Marshal(model) // экранирование строки модели
	if err != nil {
		return nil, fmt.Errorf("passthrough model: %w", err)
	}
	m["model"] = json.RawMessage(mb)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("passthrough body: %w", err)
	}
	return out, nil
}

// InjectIncludeUsage — SPEC 7.3/8.2, passthrough-клетка chat→chat: в стрим-запрос
// добавляется stream_options.include_usage, чтобы usage-чанк дошёл до tee-парсера
// и клиента, даже если клиент не попросил сам. Тело обрабатывается как карта
// RawMessage — меняется/добавляется только этот ключ. Запрос без stream:true
// (или вовсе без stream), как и запрос с уже включённым include_usage,
// возвращается нетронутым байт-в-байт. Не-объектный stream_options — ошибка:
// молча затирать чужое значение нельзя.
func InjectIncludeUsage(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("passthrough body: %w", err)
	}
	rawStream, ok := m["stream"]
	if !ok {
		return body, nil
	}
	var stream bool
	if err := json.Unmarshal(rawStream, &stream); err != nil || !stream {
		return body, nil
	}
	rawOpts, ok := m["stream_options"]
	if !ok {
		m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		var opts map[string]json.RawMessage
		if err := json.Unmarshal(rawOpts, &opts); err != nil {
			return nil, fmt.Errorf("passthrough: stream_options ожидался объектом, получено %s", rawOpts)
		}
		switch {
		case opts == nil:
			// stream_options: null — бессмысленное значение, заменяем целиком.
			m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		case includeUsageAlreadyOn(opts):
			return body, nil
		default:
			opts["include_usage"] = json.RawMessage("true")
			mb, err := json.Marshal(opts)
			if err != nil {
				return nil, fmt.Errorf("passthrough stream_options: %w", err)
			}
			m["stream_options"] = mb
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("passthrough body: %w", err)
	}
	return out, nil
}

// includeUsageAlreadyOn — include_usage присутствует и равен true.
func includeUsageAlreadyOn(opts map[string]json.RawMessage) bool {
	v, ok := opts["include_usage"]
	if !ok {
		return false
	}
	var on bool
	return json.Unmarshal(v, &on) == nil && on
}
