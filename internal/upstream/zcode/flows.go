// OAuth-флоу-рантайм zcode (SPEC 7.4/12, ARCHITECTURE §6.2): init синхронно в
// Start (ошибка видна пользователю сразу), дальше горутина-поллер — интервал
// из init, до expires_at (TTL потока 300s). Локального callback НЕТ — код
// авторизации уходит серверному флоу zcode.z.ai. Одновременно ОДИН флоу на
// аккаунт: новый Start заменяет старый (старый поллер гаснет).
//
// Колбэк-этап ready (SPEC 12): СНАЧАЛА персист OAuth-результата в
// zcode-tokens.json (токены не теряются даже при провале цепочки), ЗАТЕМ
// PAT-цепочка как отдельный шаг — success → аккаунт активен; transient-провал →
// тикер доведёт цепочку позже; fatal → ошибка в pending.
package zcode

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// AcctDeps — зависимости аккаунта для OAuth-флоу и PAT-цепочки: SOCKS-клиент
// (все запросы — через путь аккаунта, R1), origin'ы и версия клиента.
type AcctDeps struct {
	AccountKey    string       // ключ записи в zcode-tokens.json ("zcode/<id>")
	Client        *http.Client // клиент аккаунта (его SOCKS с gate)
	Base          string       // origin бизнес-API (дефолт https://api.z.ai) — PAT-цепочка
	ZcodeBase     string       // origin OAuth (дефолт https://zcode.z.ai) — init/poll
	ClientVersion string       // версия «клиента» — body PAT-issue (шаг 4 цепочки)
}

// Deps — резолв AcctDeps по ключу аккаунта (образец codex.Deps). В main
// замыкается на ЖИВОЙ снапшот конфигурации: релоад подменяет пути/origin'ы,
// зависимости не пересоздаются.
type Deps func(accountKey string) (AcctDeps, error)

// Статусы флоу для Pending (форма ответа GET /api/oauth/zcode/pending, SPEC 12).
const (
	PendingNone    = "none"
	PendingPending = "pending"
	PendingReady   = "ready"
	PendingFailed  = "failed"
)

// PendingState — состояние OAuth-флоу аккаунта для WebUI (SPEC 12:
// {"status":"none|pending|ready|failed","authorize_url"?,"error"?}).
type PendingState struct {
	State        string `json:"status"`
	AuthorizeURL string `json:"authorize_url,omitempty"`
	Err          string `json:"error,omitempty"`
}

// activeFlow — активный флоу одного аккаунта.
type activeFlow struct {
	accountKey   string
	authorizeURL string
	flowID       string
	pollToken    string
	expiresAt    time.Time
	interval     time.Duration
	ctx          context.Context // per-flow ctx: cancel при замене новым Start или shutdown
	cancel       context.CancelFunc
	state        string // PendingPending | PendingReady | PendingFailed
	err          string
}

// Flows — активные OAuth-флоу. Потокобезопасно (Start/Pending из HTTP-хендлеров,
// pollLoop из фоновых горутин).
type Flows struct {
	// ctx — контекст ПРОЦЕССА (root): флоу переживают HTTP-запросы и SIGHUP,
	// останавливаются при shutdown. Контекст запроса Start на поллер НЕ идёт.
	ctx   context.Context
	store *Store
	chain *PATCache

	mu     sync.Mutex
	active map[string]*activeFlow // accountKey → флоу (один на аккаунт)
}

// NewFlows создаёт рантайм флоу. ctx — контекст процесса (main/root).
func NewFlows(store *Store, chain *PATCache, ctx context.Context) *Flows {
	return &Flows{
		ctx:    ctx,
		store:  store,
		chain:  chain,
		active: map[string]*activeFlow{},
	}
}

// Start начинает OAuth-флоу: Init синхронно (через ctx параметра — время жизни
// HTTP-хендлера), затем фоновая горутина-поллер на контексте Flows. Возвращает
// authorize_url для браузера (SPEC 12: POST /api/oauth/zcode/start →
// {"authorize_url":...}). Новый Start заменяет активный флоу аккаунта.
func (f *Flows) Start(ctx context.Context, accountKey string, deps Deps) (string, error) {
	d, err := deps(accountKey)
	if err != nil {
		return "", fmt.Errorf("oauth flow: %w", err)
	}
	fd, err := Init(ctx, d.Client, d.ZcodeBase)
	if err != nil {
		return "", fmt.Errorf("oauth flow: %w", err)
	}
	flwCtx, cancel := context.WithCancel(f.ctx)
	flw := &activeFlow{
		accountKey:   accountKey,
		authorizeURL: fd.AuthorizeURL,
		flowID:       fd.FlowID,
		pollToken:    fd.PollToken,
		expiresAt:    fd.ExpiresAt,
		interval:     fd.Interval,
		ctx:          flwCtx,
		cancel:       cancel,
		state:        PendingPending,
	}
	f.mu.Lock()
	if old, ok := f.active[accountKey]; ok {
		old.cancel() // старый поллер гаснет; cleanup не тронет новую запись
	}
	f.active[accountKey] = flw
	f.mu.Unlock()
	go f.pollLoop(flw, deps)
	return flw.authorizeURL, nil
}

// Pending — состояние флоу аккаунта (SPEC 12).
func (f *Flows) Pending(accountKey string) PendingState {
	f.mu.Lock()
	defer f.mu.Unlock()
	flw, ok := f.active[accountKey]
	if !ok {
		return PendingState{State: PendingNone}
	}
	return PendingState{State: flw.state, AuthorizeURL: flw.authorizeURL, Err: flw.err}
}

// pollLoop — горутина-поллер: опрос не чаще interval, до expires_at (TTL 300s,
// docs §3.3: по истечении поток считается истёкшим). pending → ждать;
// ready → колбэк-этап; failed/4xx/TTL → статус failed; 5xx/сеть → ретрай на
// следующем тике. Deps резолвятся на КАЖДЫЙ тик (не фиксируются на Start):
// замыкание живёт на актуальном снапшоте, релоад подменяет пути/origin'ы —
// флоу следует за конфигом.
func (f *Flows) pollLoop(flw *activeFlow, deps Deps) {
	defer flw.cancel()
	t := time.NewTicker(flw.interval)
	defer t.Stop()
	for {
		// Провал резолва deps (аккаунт исчез из снапшота при релоаде) флоу не
		// валит: ретрай на следующем тике, TTL погасит зависший поток.
		if d, err := deps(flw.accountKey); err != nil {
			slog.Warn("zcode oauth: deps не резолвятся, ретрай на следующем тике",
				"account", flw.accountKey, "err", err)
		} else if f.pollOnce(flw, d) {
			return // флоу завершён (ready/failed/dead) — статус уже выставлен
		}
		if !time.Now().Before(flw.expiresAt) {
			f.failFlow(flw, "OAuth flow истёк (TTL до expires_at)")
			return
		}
		select {
		case <-flw.ctx.Done():
			// Флоу заменили новым Start или процесс глушится — удаляем свою
			// запись, если она ещё актуальна (заменённый не тронет новую).
			f.cleanup(flw)
			return
		case <-t.C:
		}
	}
}

// pollOnce — один опрос + обработка результата. true — флоу завершён
// (ready/failed/dead), false — продолжать на следующем тике.
func (f *Flows) pollOnce(flw *activeFlow, d AcctDeps) bool {
	res, err := Poll(flw.ctx, d.Client, d.ZcodeBase, flw.flowID, flw.pollToken)
	switch {
	case err == nil:
		// статус обработается ниже
	case errors.Is(err, ErrFlowDead):
		f.failFlow(flw, err.Error())
		return true
	default:
		// transient (сеть/5xx/429/408/битый ready): ретрай на следующем тике,
		// не валит флоу (SPEC 7.4).
		slog.Warn("zcode oauth: poll не удался, ретрай на следующем тике", "account", flw.accountKey, "err", err)
		return false
	}
	switch res.Status {
	case "pending":
		return false
	case "failed":
		// Провайдер отказал во входе (docs §3.3 «OAuth flow 授权失败»).
		f.failFlow(flw, "OAuth flow провален провайдером (status failed)")
		return true
	case "ready":
		f.finalize(flw, d, res)
		return true
	default:
		f.failFlow(flw, "неизвестный статус poll: "+res.Status)
		return true
	}
}

// finalize — колбэк-этап ready (SPEC 12): СНАЧАЛА персист OAuth-результата
// (Store.Set сразу сбрасывает needs_relogin), ЗАТЕМ PAT-цепочка как отдельный
// шаг. device_mid переиспользуется из существующей записи, иначе новый UUID.
func (f *Flows) finalize(flw *activeFlow, d AcctDeps, res *pollResult) {
	rec := TokenRec{
		ZaiAccessToken: res.ZaiAccessToken,
		ZcodeJWT:       res.Token,
		UserID:         res.UserID,
		// SPEC 3.2а: email из poll-ответа; человекочитаемое имя (name) —
		// fallback, чтобы экран статуса не показывал пустое поле.
		Email:     res.Email,
		DeviceMid: f.deviceMid(flw.accountKey),
	}
	if rec.Email == "" {
		rec.Email = res.Name
	}
	if exp, ok := parseJWTExp(res.ZaiAccessToken); ok {
		rec.ZaiExpiresAt = &exp
	}
	if err := f.store.Set(flw.accountKey, rec); err != nil {
		// Токены получены, но не записаны: флоу failed — пользователь должен
		// увидеть, что состояние не сохранилось (образец codex.Complete).
		f.failFlow(flw, "запись zcode-tokens.json: "+err.Error())
		return
	}
	// Аккаунт активен с момента персиста токенов (SPEC 12).
	f.mu.Lock()
	flw.state = PendingReady
	f.mu.Unlock()

	// PAT-цепочка — отдельный шаг (под per-account мьютексом PATCache).
	if _, err := f.chain.EnsurePAT(flw.ctx, d, &rec); err != nil {
		if IsFatalChainErr(err) || f.store.Status(flw.accountKey).State == StatusNeedsRelogin {
			// fatal → ошибка в pending (SPEC 12); токены уже записаны —
			// повторный OAuth не обязателен, но пользователь видит проблему.
			f.failFlow(flw, "PAT-цепочка после OAuth: "+err.Error())
			return
		}
		// transient: тикер доведёт цепочку позже — аккаунт уже активен,
		// первый запрос дождётся цепочку под мьютексом (SPEC 12).
		slog.Warn("zcode oauth: PAT-цепочка после OAuth не удалась, тикер повторит",
			"account", flw.accountKey, "err", err)
	}
}

// deviceMid — UUID «устройства» (SPEC 7.4: генерится один раз и переиспользуется
// всеми последующими флоу аккаунта, в т.ч. после needs_relogin).
func (f *Flows) deviceMid(accountKey string) string {
	if rec, ok := f.store.Get(accountKey); ok && rec.DeviceMid != "" {
		return rec.DeviceMid
	}
	return newUUID()
}

// failFlow — флоу завершился ошибкой: статус failed + текст (SPEC 12: «ошибка в
// pending»). Запись остаётся в карте — Pending показывает ошибку, пока пользователь
// не начнёт новый флоу.
func (f *Flows) failFlow(flw *activeFlow, msg string) {
	f.mu.Lock()
	flw.state, flw.err = PendingFailed, msg
	f.mu.Unlock()
	slog.Warn("zcode oauth: флоу завершился ошибкой", "account", flw.accountKey, "err", msg)
}

// cleanup — удалить флоу из карты, если он ещё актуален (заменили новым Start —
// не трогаем его запись).
func (f *Flows) cleanup(flw *activeFlow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active[flw.accountKey] == flw {
		delete(f.active, flw.accountKey)
	}
}

// newUUID — uuid v4 из crypto/rand (CODING 1.1: без новой зависимости).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Генерация UUID без энтропии невозможна — но и не должна случиться;
		// детерминированный fallback лучше паники в фоновом флоу.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
