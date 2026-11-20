// OAuth-флоу-рантайм (ARCHITECTURE §6, SPEC 12): активный флоу — in-memory
// state → PKCE verifier, TTL 15 минут, одновременно ОДИН флоу на аккаунт
// (новый Start заменяет старый). HTTP-обвязка (start/paste/callback-listener)
// — Ф4/webui; здесь чистая логика без HTTP-сервера.
package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultFlowTTL — SPEC 12: активный флоу живёт 15 минут.
const DefaultFlowTTL = 15 * time.Minute

// Sentinel-ошибки Complete/CompleteByCallback (тексты — прежние): webui
// ветвится через errors.Is — чужой/просроченный/неизвестный state это 400
// клиента, а сбой exchange — 502 (Ф4).
var (
	ErrFlowMalformed       = errors.New("callback url: нет code или state")
	ErrFlowUnknownState    = errors.New("oauth flow: неизвестный state")
	ErrFlowAccountMismatch = errors.New("oauth flow: state принадлежит другому аккаунту")
	ErrFlowExpired         = errors.New("oauth flow: state истёк (TTL)")
)

// AcctDeps — что флоу нужно знать об аккаунте: его SOCKS-клиент (обмен кода
// идёт через путь аккаунта, OA-3) и origin OAuth-сервера провайдера.
type AcctDeps struct {
	Client   *http.Client
	AuthBase string
}

// Deps — резолв AcctDeps по ключу аккаунта. В main замыкается на ЖИВОЙ
// снапшот конфигурации, поэтому переживает SIGHUP-релоад без пересоздания.
type Deps func(accountKey string) (AcctDeps, error)

// flow — активный OAuth-флоу одного аккаунта.
type flow struct {
	verifier     string
	accountKey   string
	redirectURI  string // запоминается: exchange обязан повторить тот же redirect_uri
	authorizeURL string // для /api/oauth/pending (Ф4): показать активную ссылку повторно
	expiresAt    time.Time
}

// Flows — активные OAuth-флоу. Потокобезопасно (Start/Complete из HTTP-
// хендлеров Ф4 и планировщика concurrently).
type Flows struct {
	ttl   time.Duration
	store *Store
	deps  Deps

	mu        sync.Mutex
	byState   map[string]*flow  // state → флоу (поиск при Complete)
	byAccount map[string]string // accountKey → state (один флоу на аккаунт)
}

// NewFlows создаёт рантайм флоу. ttl <= 0 → дефолт 15 минут (SPEC 12);
// параметр нужен тестам, которые проверяют TTL без ожиданий.
func NewFlows(store *Store, ttl time.Duration, deps Deps) *Flows {
	if ttl <= 0 {
		ttl = DefaultFlowTTL
	}
	return &Flows{
		ttl:       ttl,
		store:     store,
		deps:      deps,
		byState:   map[string]*flow{},
		byAccount: map[string]string{},
	}
}

// Start начинает OAuth-флоу аккаунта: генерирует PKCE и state, заменяет
// предыдущий флоу того же аккаунта (SPEC 12: один активный флоу на аккаунт)
// и возвращает ссылку для браузера.
func (f *Flows) Start(accountKey, redirectURI string) (authorizeURL, state string, err error) {
	deps, err := f.deps(accountKey)
	if err != nil {
		return "", "", err
	}
	verifier, challenge, err := NewPKCE()
	if err != nil {
		return "", "", err
	}
	state, err = NewState()
	if err != nil {
		return "", "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked()
	// Замена старого флоу аккаунта: прежний state перестаёт приниматься —
	// вставленный по нему URL не должен активировать устаревший verifier.
	if old, ok := f.byAccount[accountKey]; ok {
		delete(f.byState, old)
	}
	url := AuthorizeURL(deps.AuthBase, redirectURI, challenge, state)
	f.byState[state] = &flow{
		verifier:     verifier,
		accountKey:   accountKey,
		redirectURI:  redirectURI,
		authorizeURL: url,
		expiresAt:    time.Now().Add(f.ttl),
	}
	f.byAccount[accountKey] = state
	return url, state, nil
}

// Complete завершает флоу по вставленному/полученному callback-URL (режим
// контейнера, SPEC 12): извлекает code и state, валидирует state против
// активного флоу (OA-6 — это security: чужой/устаревший state обязан
// отклоняться), обменивает код через клиент аккаунта и пишет токены в Store.
// Флоу одноразовый: state гасится при первом Complete.
func (f *Flows) Complete(ctx context.Context, accountKey, callbackURL string) (*TokenSet, error) {
	u, err := url.Parse(normalizeCallbackURL(callbackURL))
	if err != nil {
		return nil, fmt.Errorf("callback url: %w", err)
	}
	q := u.Query()
	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		return nil, ErrFlowMalformed
	}

	f.mu.Lock()
	flw, ok := f.byState[state]
	if ok {
		// Одноразовость ДО сетевого обмена: повторный Complete с тем же
		// state не должен уметь повторить exchange.
		delete(f.byState, state)
		if f.byAccount[flw.accountKey] == state {
			delete(f.byAccount, flw.accountKey)
		}
	}
	f.mu.Unlock()

	if !ok {
		return nil, ErrFlowUnknownState
	}
	if flw.accountKey != accountKey {
		// OA-6: state валидируется и против аккаунта — флоу аккаунта A нельзя
		// завершить под именем аккаунта B.
		return nil, ErrFlowAccountMismatch
	}
	if time.Now().After(flw.expiresAt) {
		return nil, ErrFlowExpired
	}
	deps, err := f.deps(accountKey)
	if err != nil {
		return nil, err
	}
	ts, err := ExchangeCode(ctx, deps.Client, deps.AuthBase, flw.redirectURI, code, flw.verifier)
	if err != nil {
		return nil, fmt.Errorf("oauth flow: exchange: %w", err)
	}
	if err := f.store.Set(accountKey, *ts); err != nil {
		// Токены получены, но файл не записан: вернуть ошибку — пользователь
		// должен увидеть, что состояние не сохранилось, а не искать пропажу
		// после рестарта.
		return nil, fmt.Errorf("oauth flow: tokens.json: %w", err)
	}
	return ts, nil
}

// pruneLocked удаляет истёкшие флоу. TTL именно здесь, а не ленивой проверкой
// при Complete: просроченный state аккаунта не должен блокировать слот
// byAccount до следующего Complete.
func (f *Flows) pruneLocked() {
	now := time.Now()
	for st, flw := range f.byState {
		if now.After(flw.expiresAt) {
			delete(f.byState, st)
			if f.byAccount[flw.accountKey] == st {
				delete(f.byAccount, flw.accountKey)
			}
		}
	}
}
