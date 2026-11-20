// OAuth-эндпоинты (SPEC 12, ARCHITECTURE §6): POST /api/oauth/start (PKCE через
// codex.Flows + подъём локального callback-listener'а), GET /api/oauth/pending
// (активные флоу, без verifier), POST /api/oauth/paste (redirect-URL целиком —
// режим контейнера), POST /api/oauth/refresh (форс-refresh). Обмен кода и
// refresh идут внутри codex через клиент аккаунта (его SOCKS, OA-3) — пакет
// http.Client не создаёт (I1).
//
// Локальный callback-listener — ЧАСТЬ OAuth-флоу, это браузерная страница:
// здесь легален минимальный HTML (ответ браузеру, не API-конверт). Слушаем
// только 127.0.0.1 (перенаправление делает auth-сервер на машине пользователя
// или в контейнере с проброшенным портом).
package webui

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/upstream/codex"
)

// oauthStartRequest — тело POST /api/oauth/start (форма mock.js: {account}).
type oauthStartRequest struct {
	Account string `json:"account"`
}

// oauthStartResponse — форма mock.js DEMO_OAUTH + поле state (SPEC 12:
// «{url, state}» — state нужен экрану для сборки/сверки callback-URL руками).
type oauthStartResponse struct {
	Account  string `json:"account"`
	URL      string `json:"url"`
	State    string `json:"state"`
	TTL      string `json:"ttl"`
	Callback string `json:"callback"`
}

// pendingResponse — форма GET /api/oauth/pending: state «waiting»/«idle»,
// url активного флоу (или "") + список флоу (аккаунт+state+expires_at, БЕЗ verifier).
type pendingResponse struct {
	State string              `json:"state"`
	URL   string              `json:"url"`
	Flows []codex.PendingFlow `json:"flows"`
}

// accountRequest — тело POST paste/refresh (форма mock.js: {account, url?}).
type accountRequest struct {
	Account string `json:"account"`
	URL     string `json:"url"`
}

// handleOAuthStart — начать флоу аккаунта: валидация codex-аккаунта в живом
// снапшоте, Flows.Start (PKCE+state), подъём callback-listener'а на порту
// redirect_uri (дефолт 1455 — codex.RedirectURI).
func (h *Handler) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req oauthStartRequest
	if !readJSON(w, r, &req) {
		return
	}
	snap := h.snap()
	_, callbackPort, err := h.findCodexAccount(snap, req.Account)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	redirectURI := codex.RedirectURI(callbackPort)
	authURL, state, err := h.deps.Flows.Start(req.Account, redirectURI)
	if err != nil {
		// deps-резолв (клиент/origin) недоступен — настройка аккаунта, не сеть.
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Listener — best-effort: занятый порт не ломает флоу (работает paste-режим,
	// SPEC 12 «альтернативно — поле вставить redirect URL»); отказ логируется.
	// Порт берём из ФАКТИЧЕСКОГО redirect_uri: callback_port=0 в конфиге значит
	// дефолт 1455 (его применяет codex.RedirectURI), а не «слушать :0».
	if port, ok := listenerPort(redirectURI); ok {
		h.listeners.ensure(port, h.handleCallback)
	}
	writeJSON(w, oauthStartResponse{
		Account:  req.Account,
		URL:      authURL,
		State:    state,
		TTL:      "флоу истекает через " + ttlText(codex.DefaultFlowTTL),
		Callback: strings.TrimPrefix(redirectURI, "http://"),
	})
}

// listenerPort — порт callback-listener'а из redirect_uri (дефолт уже применён
// codex.RedirectURI; 0/битый порт → слушателя нет, работает paste-режим).
func listenerPort(redirectURI string) (int, bool) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

// ttlText — «14:52» (mm:ss остатка TTL, форма mock.js).
func ttlText(d time.Duration) string {
	m := int(d / time.Minute)
	s := int(d/time.Second) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

// handleOAuthPending — активные флоу: state/url/expires_at каждого; verifier
// наружу не отдаётся (SPEC 12).
func (h *Handler) handleOAuthPending(w http.ResponseWriter, r *http.Request) {
	flows := h.deps.Flows.Pending()
	sort.Slice(flows, func(i, j int) bool { return flows[i].Account < flows[j].Account })
	state := "idle"
	url := ""
	if len(flows) > 0 {
		state, url = "waiting", flows[len(flows)-1].URL // форма mock.js: {state:"waiting", url}
	}
	writeJSON(w, pendingResponse{State: state, URL: url, Flows: flows})
}

// handleOAuthPaste — завершение вставленным целиком redirect-URL (режим
// контейнера): Flows.Complete валидирует state против активного флоу
// (чужой/просроченный → 400, OA-6), обмен — через клиент аккаунта.
func (h *Handler) handleOAuthPaste(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Account == "" || req.URL == "" {
		writeJSONError(w, http.StatusBadRequest, "ожидаются поля account и url")
		return
	}
	if _, _, err := h.findCodexAccount(h.snap(), req.Account); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := h.deps.Flows.Complete(ctx, req.Account, req.URL); err != nil {
		oauthError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleOAuthRefresh — форс-refresh сейчас: берём refresh_token из Store,
// codex.Refresh через клиент аккаунта, Store.Set перезаписывает ключ целиком.
func (h *Handler) handleOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if !readJSON(w, r, &req) {
		return
	}
	snap := h.snap()
	if _, _, err := h.findCodexAccount(snap, req.Account); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	deps, err := h.deps.CodexDeps(req.Account)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ts, ok := h.deps.Store.Get(req.Account)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "у аккаунта нет токенов: сначала OAuth")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	fresh, err := codex.Refresh(ctx, deps.Client, deps.AuthBase, ts.RefreshToken)
	if err != nil {
		// Сбой сети/token-эндпоинта — 502 (таксономия §10: auth).
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.deps.Store.Set(req.Account, *fresh); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "tokens.json: "+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// findCodexAccount — аккаунт существует и это codex: возвращает аккаунт и
// его callback_port (0 → дефолт применит codex.RedirectURI).
func (h *Handler) findCodexAccount(snap *config.Snapshot, accountKey string) (*config.CodexAccount, int, error) {
	pname, accID, ok := strings.Cut(accountKey, "/")
	if !ok || pname == "" || accID == "" {
		return nil, 0, fmt.Errorf("account: ожидается ключ вида codex/<id>, получено %q", accountKey)
	}
	prov, ok := snap.Cfg.Providers[pname]
	if !ok || prov.Codex == nil {
		return nil, 0, fmt.Errorf("account %q: codex-провайдер не найден (OAuth есть только у codex)", accountKey)
	}
	for i := range prov.Codex.Accounts {
		if prov.Codex.Accounts[i].ID == accID {
			return &prov.Codex.Accounts[i], prov.Codex.Accounts[i].Auth.CallbackPort, nil
		}
	}
	return nil, 0, fmt.Errorf("account %q: аккаунт не найден у провайдера %q", accountKey, pname)
}

// oauthError — конверт ошибки Complete в статус: sentinel'ы флоу (чужой/
// просроченный/неизвестный state, битый URL) — 400 клиента; обмен кода
// (сеть/token-эндпоинт) — 502.
func oauthError(w http.ResponseWriter, err error) {
	switch {
	case isErr(err, codex.ErrFlowMalformed),
		isErr(err, codex.ErrFlowUnknownState),
		isErr(err, codex.ErrFlowAccountMismatch),
		isErr(err, codex.ErrFlowExpired):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONError(w, http.StatusBadGateway, err.Error())
	}
}

// ─── локальный callback-listener ────────────────────────────────────────────

// listenerSet — по одному слушателю на callback-порт (1455 и кастомные).
// Listener живёт до остановки процесса (BaseCtx): порты конфигурируются,
// повторный start того же порта переиспользует слушатель.
type listenerSet struct {
	ctx   context.Context
	mu    sync.Mutex
	ports map[int]struct{}
}

func newListenerSet(ctx context.Context) *listenerSet {
	return &listenerSet{ctx: ctx, ports: map[int]struct{}{}}
}

// ensure — поднять слушатель порта (один раз на порт). Ошибка bind не
// прерывает флоу: paste-режим остаётся рабочим фолбэком (SPEC 12).
func (l *listenerSet) ensure(port int, callback http.HandlerFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.ports[port]; ok {
		return
	}
	l.ports[port] = struct{}{}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Частый легитимный случай: другой процесс (codex CLI) уже слушает
		// callback-порт — флоу завершится через paste.
		slog.Warn("oauth callback-listener: порт занят, доступен paste-режим", "addr", addr, "err", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", callback)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// CC-1: обе горутины выходят — Serve возвращается после Close по ctx,
	// сторож завершается вместе с BaseCtx (graceful shutdown main).
	go func() {
		<-l.ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("oauth callback-listener: остановлен", "addr", addr, "err", err)
		}
	}()
	slog.Info("oauth callback-listener: слушает", "addr", addr)
}

// handleCallback — браузерный коллбэк: собираем полный URL, CompleteByCallback
// резолвит аккаунт по state и валидирует флоу; страница — минимальный HTML.
func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	url := "http://" + r.Host + r.URL.RequestURI()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := h.deps.Flows.CompleteByCallback(ctx, url); err != nil {
		// Браузерная страница ошибки: читабельный текст, секреты не эхоим.
		w.WriteHeader(oauthStatus(err))
		_, _ = w.Write([]byte(callbackPage("OAuth — ошибка", html.EscapeString(err.Error()), false)))
		return
	}
	_, _ = w.Write([]byte(callbackPage("OAuth завершён",
		"Токены обменяны через SOCKS аккаунта и записаны в tokens.json. Аккаунт активен сразу — эту вкладку можно закрыть.", true)))
}

// oauthStatus — статус страницы ошибки (те же sentinel'ы, что у paste).
func oauthStatus(err error) int {
	switch {
	case isErr(err, codex.ErrFlowMalformed),
		isErr(err, codex.ErrFlowUnknownState),
		isErr(err, codex.ErrFlowAccountMismatch),
		isErr(err, codex.ErrFlowExpired):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// callbackPage — минимальная браузерная страница результата (HTML легален:
// это страница для пользователя в браузере, не API-конверт; решение в отчёте Ф4).
func callbackPage(title, message string, ok bool) string {
	color := "#f85149"
	if ok {
		color = "#3fb950"
	}
	return `<!doctype html><html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(title) + ` — llm-proxy</title></head>
<body style="font-family:system-ui,-apple-system,sans-serif;background:#0f1115;color:#dbe2ef;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0">
<div style="max-width:460px;padding:24px;border:1px solid #242b3a;border-radius:12px;background:#161a22">
<h1 style="font-size:18px;margin:0 0 10px;color:` + color + `">` + html.EscapeString(title) + `</h1>
<p style="margin:0;font-size:14px;line-height:1.5">` + html.EscapeString(message) + `</p>
</div></body></html>`
}
