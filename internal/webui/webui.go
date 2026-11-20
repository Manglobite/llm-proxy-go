// Пакет internal/webui — WebUI (Ф4, SPEC 12, ARCHITECTURE §2/§11): статика
// (go:embed, файлы в web/ на корне репо), JSON API /api/* и локальный
// callback-listener OAuth. Про translate пакет не знает ничего (R2/§2);
// http.Client НЕ создаёт (I1): слушатель — локальный net.Listen на
// 127.0.0.1 + свой мини-мукс, исходящих соединений у пакета нет; обмен кода
// идёт внутри codex.Flows через клиент аккаунта.
package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/upstream/codex"
	"llm-proxy/internal/upstream/zcode"
	staticfs "llm-proxy/web"
)

// Deps — зависимости WebUI. БД-модули опциональны (nil → соответствующие
// разделы API отдают пустые данные: WebUI собирают и в узких сборках).
type Deps struct {
	// Snapshot — живой снапшот конфигурации (релоад подменяет атомарно;
	// webui_token читается на каждый запрос — релоад подхватывает его).
	Snapshot    func() *config.Snapshot
	AddProvider func(token, name string, data json.RawMessage) error
	AddAlias    func(token, name string, data json.RawMessage) error
	AddClient   func(token, name string) (string, error)
	AddSocks    func(token, name string, data json.RawMessage) error
	UpdateSocks func(token, name string, data json.RawMessage) error

	// CodexDeps — резолв клиента/origin OAuth-аккаунта (closure из main,
	// общий с Refresher): форс-refresh делает codex.Refresh через него.
	CodexDeps codex.Deps
	// Store — tokens.json (статусы токенов, запись после paste/refresh).
	Store *codex.Store
	// Flows — активные OAuth-флоу (start/pending/paste/callback).
	Flows *codex.Flows

	// ZStore — zcode-tokens.json (данные/статусы zcode-аккаунтов экрана ①,
	// SPEC 3.2а). ZFlows — активные OAuth-флоу zcode (режим poll, SPEC 12);
	// ZChain — PAT-кеш (PATInfo для «PAT live/до-expiry» в статусе). Все три
	// опциональны (nil → zcode-часть API отвечает 503: узкие сборки без
	// провайдера zcode), в main собираются всегда.
	ZStore    *zcode.Store
	ZFlows    *zcode.Flows
	ZChain    *zcode.PATCache
	ZcodeDeps zcode.Deps

	// Accounting/ErrorsLog/Quotas — SQLite-чтения для экранов ①–⑤.
	Accounting *accounting.DB
	ErrorsLog  *errorslog.DB
	Quotas     *quotas.DB

	// StartedAt — момент старта процесса (uptime для meta экрана ①).
	StartedAt time.Time
	// BaseCtx — контекст callback-листенеров: cancel при остановке процесса
	// гасит их горутины (CC-1). Релоад листенеры НЕ гасит — порт переживает.
	BaseCtx context.Context
	// ConfigNote — статус конфига для meta ("ok · reload HH:MM (SIGHUP)");
	// nil → "ok".
	ConfigNote func() string
	// Now — точка времени для тестов (nil → time.Now).
	Now func() time.Time
}

// Handler — WebUI: собственный ServeMux (статика + /api/*) и список точных
// паттернов для регистрации в серверном mux. Потокобезопасен: состояние —
// только листенеры под мьютексом.
type Handler struct {
	deps Deps
	mux  *http.ServeMux

	files     map[string]staticFile // путь URL → содержимое/etag/content-type
	listeners *listenerSet
}

// staticFile — один встроенный файл: содержимое, etag, content-type.
type staticFile struct {
	body  []byte
	etag  string
	ctype string
}

// New собирает WebUI. Требует Snapshot/Store/Flows — без них OAuth-часть
// бессмысленна (ошибка сборки, а не молчаливая деградация).
func New(d Deps) (*Handler, error) {
	switch {
	case d.Snapshot == nil:
		return nil, errParams("Snapshot не задан")
	case d.Store == nil:
		return nil, errParams("Store не задан")
	case d.Flows == nil:
		return nil, errParams("Flows не задан")
	case d.BaseCtx == nil:
		return nil, errParams("BaseCtx не задан")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	h := &Handler{
		deps:      d,
		mux:       http.NewServeMux(),
		listeners: newListenerSet(d.BaseCtx),
	}
	h.files = h.staticFiles()

	// Статика: точные пути каждого файла (индекс — "/{$}"). Регистрация
	// точными путями (а не префиксом "/") — осознанное решение: в серверном
	// mux остаётся JSON-404 catch-all для /v1/* (SPEC 4: ошибки — всегда JSON),
	// а webui-маршруты главнее префикса /api/ форварда (ServeMux-специфичность).
	for urlPath, f := range h.files {
		file := f
		h.mux.HandleFunc(urlPath, func(w http.ResponseWriter, r *http.Request) {
			h.serveFile(w, r, file)
		})
	}

	// JSON API /api/* (SPEC 12). Методы проверяются guard'ом: 405 обязан быть
	// JSON, а не текстом ServeMux. guard также ставит Bearer-проверку
	// webui_token (SV-5), если он задан в конфиге.
	h.mux.HandleFunc("/api/status", h.guard(http.MethodGet, h.handleStatus))
	h.mux.HandleFunc("/api/aliases", h.guard(http.MethodGet, h.handleAliases))
	h.mux.HandleFunc("/api/config/providers", h.guard(http.MethodPost, h.configCreate("provider", d.AddProvider)))
	h.mux.HandleFunc("/api/config/aliases", h.guard(http.MethodPost, h.configCreate("alias", d.AddAlias)))
	h.mux.HandleFunc("/api/config/models", h.guard(http.MethodGet, h.handleModelCatalog))
	h.mux.HandleFunc("/api/config/clients", h.guard(http.MethodPost, h.configAction("", d.AddClient != nil, h.createClient)))
	addSocks := h.guard(http.MethodPost, h.configCreate("socks", d.AddSocks))
	updateSocks := h.guard(http.MethodPut, h.configCreate("socks", d.UpdateSocks))
	h.mux.HandleFunc("/api/config/socks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			updateSocks(w, r)
			return
		}
		addSocks(w, r)
	})
	h.mux.HandleFunc("/api/config/socks/check", h.guard(http.MethodPost, h.configAction("socks", true, checkSocks)))
	h.mux.HandleFunc("/api/config/socks/recheck", h.guard(http.MethodPost, h.recheckSocks))
	h.mux.HandleFunc("/api/quotas", h.guard(http.MethodGet, h.handleQuotas))
	h.mux.HandleFunc("/api/usage", h.guard(http.MethodGet, h.handleUsage))
	h.mux.HandleFunc("/api/errors", h.guard(http.MethodGet, h.handleErrorsList))
	h.mux.HandleFunc("/api/errors/cleanup", h.guard(http.MethodPost, h.handleErrorsCleanup))
	h.mux.HandleFunc("/api/errors/{id}", h.guard(http.MethodGet, h.handleErrorsDetail))
	h.mux.HandleFunc("/api/oauth/start", h.guard(http.MethodPost, h.handleOAuthStart))
	h.mux.HandleFunc("/api/oauth/pending", h.guard(http.MethodGet, h.handleOAuthPending))
	h.mux.HandleFunc("/api/oauth/paste", h.guard(http.MethodPost, h.handleOAuthPaste))
	h.mux.HandleFunc("/api/oauth/refresh", h.guard(http.MethodPost, h.handleOAuthRefresh))
	h.mux.HandleFunc("/api/oauth/zcode/start", h.guard(http.MethodPost, h.handleZcodeStart))
	h.mux.HandleFunc("/api/oauth/zcode/pending", h.guard(http.MethodGet, h.handleZcodePending))
	return h, nil
}

// ServeHTTP — точка входа webui как http.Handler (его регистрирует server).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Patterns — точные паттерны для регистрации в серверном ServeMux
// (интерфейс server.WebUI). Включает и API-маршруты, и пути статики:
// регистрация точными путями не затеняет JSON-404 сервера и выигрывает
// у префикса "/api/" форварда (коллизия имён forward_paths с webui-маршрутами —
// ответственность конфигуратора: победит webui, поведение задокументировано).
func (h *Handler) Patterns() []string {
	out := make([]string, 0, len(h.files)+15)
	out = append(out,
		"/{$}",
		"/api/status", "/api/aliases", "/api/quotas", "/api/usage",
		"/api/errors", "/api/errors/{id}", "/api/errors/cleanup",
		"/api/oauth/start", "/api/oauth/pending", "/api/oauth/paste", "/api/oauth/refresh",
		"/api/oauth/zcode/start", "/api/oauth/zcode/pending",
		"/api/config/providers", "/api/config/aliases", "/api/config/models", "/api/config/clients", "/api/config/socks", "/api/config/socks/check", "/api/config/socks/recheck",
	)
	fixed := len(out)
	for p := range h.files {
		if p != "/{$}" {
			out = append(out, p)
		}
	}
	sort.Strings(out[fixed:]) // фиксированные маршруты первыми, статика — сортированная
	return out
}

// staticFiles — карта встроенных файлов: index.html → "/{$}", остальное — по
// имени в FS (web/src/... → /src/...). ETag — sha256 содержимого: без сборки
// имена файлов не хешированы, ревалидация по ETag даёт 304 после обновления
// бинаря и гарантирует свежую статику сразу (решение задокументировано).
func (h *Handler) staticFiles() map[string]staticFile {
	files := map[string]staticFile{}
	fs.WalkDir(staticfs.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(staticfs.FS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		urlPath := "/" + p
		if p == "index.html" {
			urlPath = "/{$}"
		}
		files[urlPath] = staticFile{
			body:  body,
			etag:  `"` + hex.EncodeToString(sum[:16]) + `"`,
			ctype: contentType(p),
		}
		return nil
	})
	return files
}

// contentType — content-type по расширению (дерево статики закрытое: html/css/js/json).
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// serveFile — отдача статики с ревалидацией. Cache-Control: no-cache для ВСЕХ
// файлов (не только index): кеш браузера обязан сверять ETag — статика без
// сборки может меняться вместе с бинарем, а 304 делает ревалидацию дешёвой.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, f staticFile) {
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.body)
}

// errParams — ошибка сборки (см. server.errParams).
type paramError struct{ msg string }

func (e *paramError) Error() string { return "webui: " + e.msg }

func errParams(msg string) error { return &paramError{msg} }
