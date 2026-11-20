// Пакет internal/socks — пул SOCKS-путей с fail-closed семантикой.
// ЕДИНСТВЕННЫЙ создатель http.Client для внешних апстримов в программе (I1).
package socks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const (
	// defaultCheckTimeout — дефолт таймаута одной проверки пути. Живёт в поле
	// Path.checkTimeout: тесты ставят меньшее значение, чтобы проверять
	// собственный дедлайн чека при живом родителе без 10-секундных ожиданий.
	defaultCheckTimeout = 10 * time.Second

	// defaultMinRecheckGap — антишторм: не перебивать blocked-путь чаще этого гэпа.
	// В Path это ПОЛЕ (инициализируется отсюда), а не константа: тесты ставят
	// меньшее значение, чтобы проверять антишторм без time.Sleep. Сама семантика
	// gate (I1–I5) не меняется.
	defaultMinRecheckGap = 3 * time.Second

	// directDialTimeout — таймаут только на установку соединения у Direct-клиента
	// (socks:"none", только локальные base_url); сам запрос/стрим не ограничиваем.
	directDialTimeout = 30 * time.Second

	defaultCheckURL = "https://api.ipify.org"
	defaultInterval = 60 * time.Second
)

// State — состояние доступности пути: healthy (пропускает трафик) или
// blocked (все запросы отклоняются с BlockedError, I4).
type State int32

const (
	StateUnchecked State = iota
	StateHealthy
	StateBlocked
)

// String — человекочитаемое имя состояния (для логов и Status).
func (s State) String() string {
	switch s {
	case StateBlocked:
		return "blocked"
	case StateHealthy:
		return "healthy"
	default:
		return "unchecked"
	}
}

// ResolvedConfig — конфиг пути после разрешения дефолтов (парсинг "60s" делает config).
type ResolvedConfig struct {
	URL           string        // строго socks5h://[user:pass@]host:port
	ExpectedIP    string        // "" — пиннинг выключен, проверяется только достижимость
	CheckURL      string        // дефолт api.ipify.org
	CheckInterval time.Duration // дефолт 60s
}

// Transporter — то, что получает модуль провайдера для аккаунта.
type Transporter interface {
	Client() *http.Client // клиент с встроенным gate
	Name() string         // имя пути из config.socks (или "none")
	Status() Status       // снапшот для WebUI
}

// Path — один SOCKS-путь. Потокобезопасен.
type Path struct {
	name string
	cfg  ResolvedConfig

	// Антишторм-гэп и таймаут проверки — поля с дефолтами из констант: тесты
	// ставят меньшие значения, чтобы проверять антишторм и дедлайн чека
	// без ожиданий. Семантика gate та же.
	minRecheckGap time.Duration
	checkTimeout  time.Duration

	mu        sync.RWMutex // защищает поля ниже
	state     State
	lastIP    string
	lastErr   string
	lastOK    time.Time // последний успешный чек (свежесть для fast-path)
	lastTry   time.Time // последний чек любого исхода (антишторм)
	needCheck bool      // сетевая ошибка запроса требует внеплановой проверки

	checkMu sync.Mutex // сериализует проверки (state не защищает)

	probe  *http.Client // чекер IP: базовый транспорт БЕЗ gate (I3)
	client *http.Client // клиент пути: gateTripper + socks-транспорт
}

var _ Transporter = (*Path)(nil)

// Client — http.Client пути с встроенным gate: ни один запрос не уйдёт мимо
// проверки доступности пути (I1, I4).
func (p *Path) Client() *http.Client { return p.client }

// Name — имя пути из config.socks (у прямого пути — "none").
func (p *Path) Name() string { return p.name }

// New создаёт путь. Ошибка схемы — на этапе загрузки конфига (I2).
func New(name string, cfg ResolvedConfig) (*Path, error) {
	if cfg.CheckURL == "" {
		cfg.CheckURL = defaultCheckURL
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = defaultInterval
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("socks %q: %w", name, err)
	}
	if u.Scheme != "socks5h" {
		return nil, fmt.Errorf(`socks %q: допустима только схема socks5h:// (DNS резолвится прокси), получено %q`, name, u.Scheme)
	}
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		if pw, ok := u.User.Password(); ok {
			auth.Password = pw
		}
	}
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("socks %q: %w", name, err)
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks %q: dialer без поддержки контекста", name)
	}

	// Единственный транспорт пути: и для запросов, и для probe.
	base := &http.Transport{
		DialContext:           ctxDialer.DialContext,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, // до заголовков; сам стрим не ограничен
		// ForceAttemptHTTP2 не включаем: через SOCKS достаточно http/1.1
	}

	p := &Path{
		name:          name,
		cfg:           cfg,
		minRecheckGap: defaultMinRecheckGap,
		checkTimeout:  defaultCheckTimeout,
	}
	// probe берёт таймаут из поля checkTimeout (единый источник с runCheckLocked):
	// захардкоженная константа рассинхронизировалась бы с тестами/будущим конфигом.
	p.probe = &http.Client{Transport: base, Timeout: p.checkTimeout}
	p.client = &http.Client{Transport: &gateTripper{path: p, base: base}}
	return p, nil
}

// Run — фоновая горутина пути: периодическая проверка и recovery blocked.
// Живёт до отмены ctx (релоад/шатдаун).
func (p *Path) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.checkMu.Lock()
			if err := p.runCheckLocked(ctx); err != nil {
				// Периодический чек никому не отвечает: исход и так виден в
				// state (Status). Лог — Debug, чтобы блокировки не засоряли
				// журнал; аборт ctx сюда тоже попадает и state не портит (F1).
				slog.Debug("периодическая проверка пути не прошла", "path", p.name, "err", err)
			}
			p.checkMu.Unlock()
		}
	}
}

// gateTripper: проверка пути перед запросом, регистрация сетевых ошибок после.
// Запрос к апстриму физически не может начаться при закрытом пути.
type gateTripper struct {
	path *Path
	base http.RoundTripper
}

func (g *gateTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := g.path.ensureUsable(req.Context()); err != nil {
		return nil, err // *BlockedError → 503, запрос не создаётся (I4)
	}
	resp, err := g.base.RoundTrip(req)
	// Ошибка RoundTrip = транспортная (dial/reset/EOF/ошибка SOCKS).
	// Отмена контекста клиентом — не вина пути, её не репортим (FC-8).
	if err != nil && req.Context().Err() == nil {
		g.path.reportNetworkError()
	}
	return resp, err
}

// fresh — быстрый путь (RLock): можно ли пустить запрос без проверки.
func (p *Path) fresh() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state == StateHealthy && !p.needCheck && time.Since(p.lastOK) < p.cfg.CheckInterval
}

// ensureUsable — gate перед КАЖДЫМ запросом. Fail-closed: сомневаемся — проверяем.
func (p *Path) ensureUsable(ctx context.Context) error {
	if p.fresh() {
		return nil
	}
	// Медленный путь: проверку выполняет один, остальные ждут её результата.
	p.checkMu.Lock()
	defer p.checkMu.Unlock()
	if p.fresh() { // пока ждали lock, путь уже проверили
		return nil
	}
	p.mu.RLock()
	state, lastTry := p.state, p.lastTry
	p.mu.RUnlock()
	if state == StateBlocked && time.Since(lastTry) < p.minRecheckGap {
		return p.blockedError() // антишторм: недавно проверяли и падали
	}
	return p.runCheckLocked(ctx)
}

// reportNetworkError: сетевая ошибка запроса → следующий запрос сначала проверит путь.
func (p *Path) reportNetworkError() {
	p.mu.Lock()
	p.needCheck = true
	p.mu.Unlock()
}

// ReportNetworkError — экспортированная точка для upstream-провайдеров:
// сетевой обрыв посреди стрима (resp.Body.Read, FC-9) помечает путь
// требующим проверки. Через gateTripper такие обрывы не проходят —
// тело читается уже после возврата RoundTrip.
func (p *Path) ReportNetworkError() { p.reportNetworkError() }

// markAbortedTry фиксирует момент абортированной проверки (обе abort-ветки
// runCheckLocked). lastTry участвует в антишторм-гэпе ensureUsable: если
// отменённые проверки его не сдвигают, каждый новый запрос с отменённым ctx
// немедленно перезапускает чек — клиентские отмены пробивали бы гэп чаще
// minRecheckGap. state/lastErr намеренно не трогаются: аборт — не провал (F1).
func (p *Path) markAbortedTry() {
	p.mu.Lock()
	p.lastTry = time.Now()
	p.mu.Unlock()
}

// runCheckLocked — вызывается под checkMu. Идём в сеть через probe (тот же SOCKS).
func (p *Path) runCheckLocked(ctx context.Context) error {
	// Аборт до старта: контекст уже отменён (релоад/шатдаун/клиент ушёл).
	// Это НЕ провал проверки: клиент получает fail-closed ошибку, но state
	// не трогаем — иначе отмена ctx при полётной проверке пометила бы
	// здоровый путь blocked (ложные 503, F1).
	if err := ctx.Err(); err != nil {
		p.markAbortedTry() // N1: попытка была — гэп не должен пробиваться абортами
		return &BlockedError{Path: p.name, Reason: "check aborted: " + err.Error()}
	}
	cctx, cancel := context.WithTimeout(ctx, p.checkTimeout)
	ip, err := p.fetchIP(cctx)
	cancel()
	// Ошибка при отменённом родителе — тоже аборт, а не провал: проверка не
	// успела завершиться сама, её результат недостоверен. Провал по
	// СОБСТВЕННОМУ дедлайну чека (родитель жив) блокирует, как и раньше.
	if err != nil && ctx.Err() != nil {
		p.markAbortedTry()
		return &BlockedError{Path: p.name, Reason: "check aborted: " + ctx.Err().Error()}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastTry = time.Now()
	p.needCheck = false
	switch {
	case err != nil:
		p.state, p.lastErr = StateBlocked, err.Error()
	case p.cfg.ExpectedIP != "" && !sameIP(ip, p.cfg.ExpectedIP):
		p.state, p.lastIP = StateBlocked, ip
		p.lastErr = fmt.Sprintf("egress ip mismatch: expected %s, got %s", p.cfg.ExpectedIP, ip)
	default:
		p.state, p.lastIP, p.lastErr, p.lastOK = StateHealthy, ip, "", time.Now()
		return nil
	}
	return p.blockedErrorLocked()
}

func sameIP(a, b string) bool {
	left, leftErr := netip.ParseAddr(a)
	right, rightErr := netip.ParseAddr(b)
	return leftErr == nil && rightErr == nil && left == right
}

var ErrCheckInProgress = errors.New("SOCKS check already in progress")

func (p *Path) Recheck(ctx context.Context) error {
	if !p.checkMu.TryLock() {
		return ErrCheckInProgress
	}
	defer p.checkMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.runCheckLocked(ctx)
}

func Check(ctx context.Context, cfg ResolvedConfig) (Status, error) {
	p, err := New("preview", cfg)
	if err != nil {
		return Status{}, err
	}
	defer p.probe.CloseIdleConnections()
	err = p.ensureUsable(ctx)
	return p.Status(), err
}

// fetchIP — GET check_url через базовый SOCKS-транспорт пути.
func (p *Path) fetchIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.CheckURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.probe.Do(req)
	if err != nil {
		return "", fmt.Errorf("ip check через socks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ip check: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil || ip.Zone() != "" {
		return "", errors.New("ip check: ответ не содержит IP-адрес")
	}
	return ip.String(), nil
}

// BlockedError — путь закрыт. Сервер превращает в 503 с этим текстом.
type BlockedError struct {
	Path, Reason string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("socks path %q is blocked: %s", e.Path, e.Reason)
}

func (p *Path) blockedError() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.blockedErrorLocked()
}

func (p *Path) blockedErrorLocked() error {
	return &BlockedError{Path: p.name, Reason: p.lastErr}
}

// Status — снапшот состояния пути для WebUI; безопасен для конкурентного вызова.
type Status struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	ExpectedIP string `json:"expected_ip,omitempty"`
	ActualIP   string `json:"actual_ip,omitempty"`
	LastCheck  string `json:"last_check,omitempty"` // RFC3339
	LastError  string `json:"last_error,omitempty"`
}

func (p *Path) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	lastCheck := ""
	if !p.lastTry.IsZero() {
		// Нулевое время форматировалось бы в "0001-01-01T00:00:00Z" — до первой
		// проверки поле должно быть пустым.
		lastCheck = p.lastTry.Format(time.RFC3339)
	}
	return Status{
		Name: p.name, State: p.state.String(),
		ExpectedIP: p.cfg.ExpectedIP, ActualIP: p.lastIP,
		LastCheck: lastCheck, LastError: p.lastErr,
	}
}

// netDialer — точка dial для Direct-клиента. Обёртка net.Dialer с таймаутом
// только на установку соединения: длинные ответы локальных апстримов не обрываем.
type netDialer struct{}

func (netDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: directDialTimeout}
	return d.DialContext(ctx, network, addr)
}

// Direct — путь "none" (без прокси). Валидация config разрешает его для
// base_url на localhost/127.0.0.1/::1 всегда, а для внешних хостов — только
// с явным allow_direct:true у аккаунта (осознанное исключение, I5). Gate не нужен.
type Direct struct{}

var (
	_            Transporter = Direct{}
	directClient             = &http.Client{ // только локальные хосты
		Transport: &http.Transport{
			DialContext:           (netDialer{}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
)

func (Direct) Client() *http.Client { return directClient }
func (Direct) Name() string         { return "none" }
func (Direct) Status() Status       { return Status{Name: "none", State: "healthy"} }

// ReportNetworkError — заглушка для единообразия с Transporter-путями:
// прямому пути (только localhost/allow_direct) проверка доступности не нужна.
func (Direct) ReportNetworkError() {}
