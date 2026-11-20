// OAuth-эндпоинты zcode (Ф7b, SPEC 12 «OAuth-флоу в WebUI (zcode, режим poll)»,
// ARCHITECTURE §6.2): POST /api/oauth/zcode/start (init синхронно через клиент
// аккаунта — его SOCKS, OA-3) и GET /api/oauth/zcode/pending (poll-петля живёт
// горутиной прокси внутри zcode.Flows — WebUI только читает статус). Локального
// callback и paste-режима НЕТ: код авторизации уходит серверному флоу
// zcode.z.ai. http.Client пакет не создаёт (I1) — вся сеть внутри Flows.
package webui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/upstream/zcode"
)

// zcodeStartResponse — форма POST /api/oauth/zcode/start (SPEC 12:
// {"authorize_url":"…"}; account — ключ флоу, по которому читать pending).
type zcodeStartResponse struct {
	Account      string `json:"account"`
	AuthorizeURL string `json:"authorize_url"`
}

// zcodeReady — zcode-рантайм собран (Deps-компоненты Ф7b). Узкие сборки без
// провайдера zcode не передают их — тогда часть API отвечает 503, а не паникует.
func (h *Handler) zcodeReady() bool {
	return h.deps.ZStore != nil && h.deps.ZFlows != nil
}

// handleZcodeStart — начать флоу: валидация zcode-аккаунта в живом снапшоте,
// резолв deps (клиент/origin'ы), Flows.Start (init синхронно + фоновый поллер).
func (h *Handler) handleZcodeStart(w http.ResponseWriter, r *http.Request) {
	if !h.zcodeReady() {
		writeJSONError(w, http.StatusServiceUnavailable, "zcode-рантайм не собран (нет провайдера zcode)")
		return
	}
	var req oauthStartRequest
	if !readJSON(w, r, &req) {
		return
	}
	_, accID, err := h.findZcodeAccount(h.snap(), req.Account)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Флоу живёт под ключом записи zcode-tokens.json (SPEC 3.2а, literal
	// "zcode/<id>" — как zcodeDeps/Store в main), а не под именем провайдера:
	// Store/тикет/флоу — один на процесс.
	flowKey := zcode.AccountKey(accID)
	deps, err := h.deps.ZcodeDeps(flowKey)
	if err != nil {
		// Клиент/путь/origin недоступны — настройка аккаунта, не сеть.
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// deps уже разрешены — Start получает константное замыкание: его ошибки
	// остаются только сетевыми/HTTP init (классифицируются ниже).
	url, err := h.deps.ZFlows.Start(ctx, flowKey, func(string) (zcode.AcctDeps, error) { return deps, nil })
	if err != nil {
		// init через SOCKS аккаунта упал: сеть/не-2xx апстрима — 502 auth
		// (таксономия §10; 400 выше — только валидация запроса/конфига).
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, zcodeStartResponse{Account: flowKey, AuthorizeURL: url})
}

// handleZcodePending — состояние флоу (SPEC 12):
// {"status":"none|pending|ready|failed","authorize_url"?,"error"?}.
func (h *Handler) handleZcodePending(w http.ResponseWriter, r *http.Request) {
	if !h.zcodeReady() {
		writeJSONError(w, http.StatusServiceUnavailable, "zcode-рантайм не собран (нет провайдера zcode)")
		return
	}
	account := r.URL.Query().Get("account")
	if account == "" {
		writeJSONError(w, http.StatusBadRequest, "ожидается параметр account")
		return
	}
	_, accID, err := h.findZcodeAccount(h.snap(), account)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, h.deps.ZFlows.Pending(zcode.AccountKey(accID)))
}

// findZcodeAccount — аккаунт существует и это zcode (образец findCodexAccount).
// Принимает обе формы ключа: "<провайдер>/<id>" (фронт строит из строки статуса)
// и literal "zcode/<id>" (ключ записи zcode-tokens.json, SPEC 3.2а — форма из
// SPEC 12; провайдеров типа zcode в v1 один, поэтому перебор детерминирован).
// Возвращает аккаунт и его id.
func (h *Handler) findZcodeAccount(snap *config.Snapshot, accountKey string) (*config.ZCodeAccount, string, error) {
	pname, accID, ok := strings.Cut(accountKey, "/")
	if !ok || pname == "" || accID == "" {
		return nil, "", fmt.Errorf("account: ожидается ключ вида zcode/<id> или <провайдер>/<id>, получено %q", accountKey)
	}
	if prov, exists := snap.Cfg.Providers[pname]; exists {
		if prov.ZCode == nil {
			return nil, "", fmt.Errorf("account %q: OAuth zcode есть только у zcode-аккаунтов", accountKey)
		}
		if acc := zcodeAccountByID(prov.ZCode.Accounts, accID); acc != nil {
			return acc, accID, nil
		}
		return nil, "", fmt.Errorf("account %q: аккаунт не найден у провайдера %q", accountKey, pname)
	}
	if pname != zcode.StoreKeyPrefix {
		return nil, "", fmt.Errorf("account %q: zcode-провайдер не найден (OAuth zcode есть только у zcode-аккаунтов)", accountKey)
	}
	for _, name := range sortedKeys(snap.Cfg.Providers) {
		p := snap.Cfg.Providers[name]
		if p.ZCode == nil {
			continue
		}
		if acc := zcodeAccountByID(p.ZCode.Accounts, accID); acc != nil {
			return acc, accID, nil
		}
	}
	return nil, "", fmt.Errorf("account %q: zcode-аккаунт не найден в снапшоте", accountKey)
}

// zcodeAccountByID — линейный поиск аккаунта по id в провайдере.
func zcodeAccountByID(accounts []config.ZCodeAccount, id string) *config.ZCodeAccount {
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	return nil
}
