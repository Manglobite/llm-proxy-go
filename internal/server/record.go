// Учёт и дампы (SPEC 9–10, AC-4): строки requests пишутся на любой исход
// запроса, ошибки — в errors с маскированными заголовками. Сбои БД логируются
// и НЕ мешают ответу клиенту: клиентский ответ к этому моменту уже отдан или
// отдат следом независимо от результата записи.
package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/router"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

// transporterFor — клиент аккаунта по имени его SOCKS-пути: "none" → прямой
// путь (socks.Direct; его легальность уже проверил config при загрузке, I5),
// иначе путь из пула снапшота. Роутер про сеть не знает — transporter
// добирает сервер (нужен и LLM-запросам, и форварду forward_paths).
func transporterFor(snap *config.Snapshot, socksName string) (socks.Transporter, error) {
	if socksName == "none" {
		return socks.Direct{}, nil
	}
	p, ok := snap.Socks[socksName]
	if !ok {
		return nil, errors.New("socks-путь " + socksName + " отсутствует в снапшоте (рассинхронизация конфигурации)")
	}
	return p, nil
}

// modelInfo — экономика модели для cost/credits (config.models → accounting).
func modelInfo(models map[string]config.ModelConfig, model string) *accounting.ModelInfo {
	mc, ok := models[model]
	if !ok {
		return nil // неизвестная модель: стоимость не считаем (Calc* nil-safe)
	}
	mi := &accounting.ModelInfo{Subscription: mc.Subscription}
	if mc.PriceUSDPerM != nil {
		mi.PriceIn = mc.PriceUSDPerM.Input
		mi.PriceOut = mc.PriceUSDPerM.Output
		mi.PriceCached = mc.PriceUSDPerM.CachedInput
	}
	if mc.GLMCredits != nil {
		mi.GLMIn = mc.GLMCredits.InputMult
		mi.GLMCached = mc.GLMCredits.CachedMult
		mi.GLMOut = mc.GLMCredits.OutputMult
	}
	return mi
}

// recordRequest — строка requests на завершённый запрос (SPEC 9).
func (s *Server) recordRequest(snap *config.Snapshot, target router.Target,
	clientKey, alias, protocol string, isStream bool, latencyMS int64, res *upstream.SendResult) {

	if s.params.Accounting == nil {
		return // учёт отключён (узкие тесты протокола)
	}
	row := accounting.RequestRow{
		Ts: time.Now(),
		// client_key — ИМЯ клиента (SPEC 2), а не сам ключ: секрет не попадает в БД.
		ClientKey: clientKey, Alias: alias,
		Provider: target.ProviderName, Account: target.AccountID,
		Model: target.Model, Protocol: protocol,
		Prompt:        res.Usage.PromptTokens,
		Completion:    res.Usage.CompletionTokens,
		CachedInput:   res.Usage.CachedInputTokens,
		CacheCreation: res.Usage.CacheCreationTokens,
		Reasoning:     res.Usage.ReasoningTokens,
		IsStream:      isStream,
		LatencyMS:     latencyMS,
		Status:        res.UpstreamStatus,
		ErrorKind:     res.ErrorKind,
	}
	// Стоимость/кредиты считает accounting по конфигу снапшота (SPEC 9).
	if err := s.params.Accounting.InsertRequest(row, modelInfo(snap.Cfg.Models, target.Model)); err != nil {
		// AC-4: сбой БД не роняет ответ — только лог.
		slog.Warn("accounting: запись запроса не удалась", "err", err, "alias", alias)
	}
}

// recordQuotas — снапшоты квот (SPEC 11): конвертация нейтральной формы
// upstream.Quotas в строки quota_snapshots и вставка. Вызывается на каждый
// ответ с квотами (у codex — с x-codex-* заголовками каждого ответа, AC-7,
// и с маркером 429 usage_limit_reached, AC-8).
func (s *Server) recordQuotas(target router.Target, q *upstream.Quotas) {
	for _, row := range quotas.FromUpstream(target.ProviderName, target.AccountID, q, time.Now()) {
		if err := s.params.Quotas.Insert(row); err != nil {
			// AC-4: сбой БД не роняет ответ клиенту — только лог.
			logWarn("quotas: запись снапшота не удалась", "err", err, "kind", row.Kind)
		}
	}
}

// recordClientError — дамп отказа ДО роутинга (битый JSON / model отсутствует):
// в errors под stage=translate, дамп — клиентский запрос. Провайдер/аккаунт
// неизвестны (роутинг не состоялся), строки requests для таких запросов нет:
// SPEC 9 требует заполненные provider/account/model.
func (s *Server) recordClientError(clientKey string, r *http.Request, body []byte, status int, msg string) {
	if s.params.ErrorsLog == nil {
		return
	}
	row := errorslog.ErrorRow{
		Ts:         time.Now(),
		ClientKey:  clientKey,
		Stage:      upstream.StageTranslate,
		Status:     status,
		Method:     r.Method,
		URL:        r.URL.String(),
		ReqHeaders: errorslog.MaskedHeaders(r.Header),
		ReqBody:    body,
		Err:        msg,
	}
	if err := s.params.ErrorsLog.InsertError(row); err != nil {
		slog.Warn("errorslog: запись дампа не удалась", "err", err, "stage", row.Stage)
	}
}

// recordError — дамп неудачи в errors (SPEC 10). Заголовки маскируются ЗДЕСЬ,
// перед записью: сырые секреты живут только в памяти дампа (fewshot AC-2).
func (s *Server) recordError(snap *config.Snapshot, target router.Target,
	clientKey, alias string, r *http.Request, clientBody []byte, res *upstream.SendResult) {

	if s.params.ErrorsLog == nil {
		return
	}
	row := errorslog.ErrorRow{
		Ts:        time.Now(),
		ClientKey: clientKey, Alias: alias,
		Provider: target.ProviderName, Account: target.AccountID,
		Stage:  res.Stage,
		Status: res.ClientStatus,
		Err:    res.Message,
	}
	// Дамп: апстрим-запрос (провайдер знает его точнее всего); при отсутствии
	// (translate до запроса) — клиентский запрос: в дампе ошибки конвертации
	// интересно именно то, что прислал клиент.
	dump := res.Dump
	if dump == nil {
		dump = &upstream.Dump{
			Method:     r.Method,
			URL:        r.URL.String(),
			ReqHeaders: r.Header,
			ReqBody:    clientBody,
		}
	}
	row.Method = dump.Method
	row.URL = dump.URL
	row.ReqHeaders = errorslog.MaskedHeaders(dump.ReqHeaders)
	row.ReqBody = dump.ReqBody
	row.RespHeaders = errorslog.MaskedHeaders(dump.RespHeaders)
	row.RespBody = dump.RespBody

	if err := s.params.ErrorsLog.InsertError(row); err != nil {
		// AC-4: сбой БД не роняет ответ клиенту.
		slog.Warn("errorslog: запись дампа не удалась", "err", err, "stage", res.Stage)
	}
}
