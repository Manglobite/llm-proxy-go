package translate

// ─── SPEC 8.7 («codex-апстрим»): события Responses API → объект non-stream ──

// CodexToResponsesResponse — non-stream агрегатор: codex-апстрим всегда
// стримит (store:false, stream:true — SPEC 7.1), а клиенту /v1/responses без
// стрима нужен полный объект. Кормится теми же событиями, что стримы-мапперы
// (Feed из ScanSSE); output собирается из output_item.done — полные items,
// тогда как output массива completed в живых событиях бывает заглушкой
// (используется лишь как fallback, если done-событий не было). Passthrough-
// подобный: id/model/items переносятся как есть.
type CodexToResponsesResponse struct {
	done        bool
	id          string
	model       string
	output      []ResponsesOutputItem
	usage       Usage
	usageSet    bool
	inputTokens int    // usage из response.created, если финал его не перевесил
	status      string // completed | incomplete | failed (пусто — финала не было)
	incomplete  string // reason из incomplete_details события response.incomplete
	errMsg      string
	errCode     string
}

var _ Feeder = (*CodexToResponsesResponse)(nil)

// NewCodexToResponsesResponse создаёт агрегатор.
func NewCodexToResponsesResponse() *CodexToResponsesResponse {
	return &CodexToResponsesResponse{}
}

// Feed — реализация Feeder.
func (a *CodexToResponsesResponse) Feed(event string, data []byte) error {
	if a.done {
		return nil // события после финала игнорируются (done-guard, F5)
	}
	ev, ok := decodeCodexEvent(event, data)
	if !ok {
		return nil // неизвестные события Responses API (output_item.added, ...) молча пропускаем
	}
	switch ev.kind {
	case codexStart:
		a.id = orDefault(ev.respID, a.id)
		a.model = orDefault(ev.model, a.model)
		if ev.inputTokens > 0 && !a.usageSet {
			a.usage.PromptTokens = ev.inputTokens // usage на старте, если апстрим сообщил сразу
		}

	case codexItemDone:
		a.output = append(a.output, responsesItemFromCodex(ev.item))

	case codexCompleted:
		a.done = true
		a.status = "completed"
		a.applyFinal(ev)

	case codexIncomplete:
		a.done = true
		a.status = "incomplete"
		a.incomplete = orDefault(ev.incompleteReason, "max_output_tokens")
		a.applyFinal(ev)

	case codexFailed:
		a.done = true
		a.status = "failed"
		a.errMsg, a.errCode = ev.errMsg, ev.errCode
	}
	return nil
}

// applyFinal — общая часть completed/incomplete: id/model и usage финального
// события; output из него — только fallback на случай отсутствия done-событий.
func (a *CodexToResponsesResponse) applyFinal(ev codexEvent) {
	a.id = orDefault(ev.respID, a.id)
	a.model = orDefault(ev.model, a.model)
	a.usage = usageFromResponses(ev.usage)
	a.usageSet = true
	if len(a.output) == 0 {
		for _, it := range ev.output {
			a.output = append(a.output, responsesItemFromCodex(it))
		}
	}
}

// Err — ошибка апстрима из response.failed/error, если была (как у
// CodexToOpenAIResponse); наверху ветвятся через errors.As (CODING 1.2).
func (a *CodexToResponsesResponse) Err() error {
	if a.errMsg == "" && a.errCode == "" {
		return nil
	}
	return &StreamError{Code: a.errCode, Message: orDefault(a.errMsg, a.errCode)}
}

// Finished — пришёл ли финал (completed/incomplete/failed).
func (a *CodexToResponsesResponse) Finished() bool { return a.done }

// Usage — накопленный normalized usage (tee) — единый источник для accounting.
func (a *CodexToResponsesResponse) Usage() Usage { return a.usage }

// Result возвращает накопленный объект. Стрим без финала даёт status
// "completed" с тем, что успело накопиться: усечённость видна провайдеру по
// Finished()/Err() (как у CodexToOpenAIResponse.Result).
func (a *CodexToResponsesResponse) Result() *ResponsesAPIResponse {
	out := a.output
	if out == nil {
		out = []ResponsesOutputItem{}
	}
	resp := &ResponsesAPIResponse{
		ID:     orDefault(a.id, "resp_proxy"),
		Object: "response",
		Status: orDefault(a.status, "completed"),
		Model:  a.model,
		Output: out,
		Usage:  responsesOutUsageFor(a.usage),
	}
	if a.status == "incomplete" {
		resp.IncompleteDetails = &ResponsesIncompleteDetails{Reason: orDefault(a.incomplete, "max_output_tokens")}
	}
	if a.status == "failed" {
		resp.Error = &ResponsesOutError{Code: a.errCode, Message: orDefault(a.errMsg, a.errCode)}
	}
	return resp
}

// responsesItemFromCodex — item апстрима в форму Responses-объекта. Текст
// message живёт в content[].text (реальная форма codex; плоское text —
// упрощение few-shot), reasoning — в summary[].text. Роль message всегда
// "assistant": output-итемы Responses API — только assistant-сторона.
func responsesItemFromCodex(it codexItem) ResponsesOutputItem {
	out := ResponsesOutputItem{
		Type: it.Type, ID: it.ID,
		CallID: it.CallID, Name: it.Name, Arguments: it.Arguments,
	}
	switch it.Type {
	case "message":
		out.Role = "assistant"
		out.Content = make([]ResponsesOutContent, 0, len(it.Content))
		for _, c := range it.Content {
			out.Content = append(out.Content, ResponsesOutContent{Type: "output_text", Text: c.Text})
		}
		if len(out.Content) == 0 && it.Text != "" {
			out.Content = []ResponsesOutContent{{Type: "output_text", Text: it.Text}} // плоская форма few-shot
		}
	case "reasoning":
		out.Summary = make([]ResponsesOutContent, 0, len(it.Summary))
		for _, s := range it.Summary {
			out.Summary = append(out.Summary, ResponsesOutContent{Type: "summary_text", Text: s.Text})
		}
	}
	return out
}
