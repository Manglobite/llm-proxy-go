// Хот-релоад конфига (ARCHITECTURE §7): атомарная подмена снапшота, переиспользование
// путей и отмена контекста старого снапшота. Сигнальная обвязка (SIGHUP) — в main.
package config

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"llm-proxy/internal/socks"
)

// Manager — владелец живого снапшота конфига. Читатели (будущий server/провайдеры)
// один раз берут Snapshot() и дальше работают только с его полями — без блокировок.
type Manager struct {
	// root — общий корень всех снапшотов; cancel в graceful shutdown гасит всё разом.
	root context.Context

	mu       sync.Mutex
	path     string
	digest   [sha256.Size]byte
	snapshot atomic.Pointer[Snapshot]
}

// Snapshot — неизменяемый срез конфигурации: resolved-конфиг + запущенные
// socks-пути. Выполняющиеся запросы держат ссылки на старые объекты и
// дозавершаются на них после релоада (SPEC 3.3, ARCHITECTURE §7.4).
type Snapshot struct {
	Cfg   *ResolvedConfig
	Socks map[string]*socks.Path

	ctx    context.Context // отменяется при замене снапшота: Path.Run останавливаются
	cancel context.CancelFunc
}

// Ctx — контекст снапшота: отменяется при релоаде/остановке (для фоновых
// горутин, привязанных к конкретной конфигурации).
func (s *Snapshot) Ctx() context.Context { return s.ctx }

// NewManager загружает конфиг и запускает фоновые проверки путей.
// root обязан быть живым контекстом процесса.
func NewManager(root context.Context, path string) (*Manager, error) {
	m := &Manager{root: root}
	if err := m.Reload(path); err != nil {
		return nil, err
	}
	return m, nil
}

// Snapshot возвращает текущий снапшот (никогда nil после NewManager).
func (m *Manager) Snapshot() *Snapshot { return m.snapshot.Load() }

// Reload загружает и валидирует НОВЫЙ конфиг в сторону; при ошибке живой снапшот
// не трогается (CF-1). При успехе пути с неизменными name+url+expected_ip
// переиспользуются со своим state (CF-4), снапшот подменяется атомарно, контекст
// старого снапшота отменяется — его Path.Run останавливаются.
func (m *Manager) Reload(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	cfg, err := decode(data)
	if err != nil {
		return err
	}
	resolved, err := cfg.resolve(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := m.apply(resolved, nil); err != nil {
		return err
	}
	m.path = path
	m.digest = sha256.Sum256(data)
	return nil
}

func (m *Manager) apply(cfg *ResolvedConfig, persist func() error) error {
	old := m.snapshot.Load()
	ctx, cancel := context.WithCancel(m.root)
	paths := make(map[string]*socks.Path, len(cfg.Socks))
	for name, sc := range cfg.Socks {
		if old != nil {
			// Переиспользование по ВСЕМ параметрам проверки (ARCHITECTURE §7.5):
			// изменил url/expected_ip/check_url/check_interval — путь пересоздаётся,
			// иначе конфиг показывал бы одно, а путь делал другое.
			if oldSC, ok := old.Cfg.Socks[name]; ok &&
				oldSC.URL == sc.URL &&
				oldSC.ExpectedIP == sc.ExpectedIP &&
				oldSC.CheckURL == sc.CheckURL &&
				oldSC.CheckInterval == sc.CheckInterval {
				paths[name] = old.Socks[name] // CF-4: state пути переживает релоад
				continue
			}
		}
		p, err := socks.New(name, sc)
		if err != nil {
			cancel() // созданные выше пути не запущены — просто гасим их контекст
			return err
		}
		paths[name] = p
	}
	if persist != nil {
		if err := persist(); err != nil {
			cancel()
			return err
		}
	}
	// Run нужен для ВСЕХ путей нового снапшота, включая переиспользованные:
	// их прежний тикер умрёт вместе со старым ctx. Кратковременное дублирование
	// проверок безопасно — runCheck сериализуется checkMu внутри пути.
	for _, p := range paths {
		go p.Run(ctx)
	}
	m.snapshot.Store(&Snapshot{Cfg: cfg, Socks: paths, ctx: ctx, cancel: cancel})
	if old != nil {
		old.cancel() // старый снапшот остановлен; in-flight запросы дозавершаются
	}
	return nil
}

var (
	ErrConfigConflict = errors.New("имя или ID уже заняты либо config.json изменён вручную; после ручной правки перезагрузите конфиг")
	ErrConfigInvalid  = errors.New("проверьте имя, тип, URL, аккаунт, SOCKS-путь и обязательные поля")
	ErrConfigDisabled = errors.New("для записи задайте server.webui_token в config.json и перезагрузите прокси")
	ErrConfigAuth     = errors.New("unauthorized")
	ErrClientNotFound = errors.New("клиентский ключ не найден")
	ErrClientShared   = errors.New("один ключ используется несколькими клиентами; удалите все его записи вручную и перезагрузите конфиг")
)

func (m *Manager) AddProvider(token, name string, raw json.RawMessage) error {
	if !validEntryName(name) || strings.Contains(name, "/") || checkDuplicateKeys(raw) != nil {
		return ErrConfigInvalid
	}
	p, err := parseProvider(name, raw)
	if err != nil {
		return ErrConfigInvalid
	}
	switch p.Type {
	case ProviderCodex:
		if p.Codex.BaseURL != "" || p.Codex.AuthBaseURL != "" || len(p.Codex.Accounts) != 1 ||
			p.Codex.Accounts[0].Auth.CallbackPort < 0 || p.Codex.Accounts[0].Auth.CallbackPort > 65535 {
			return ErrConfigInvalid
		}
	case ProviderZCode:
		if p.ZCode.BaseURL != "" || p.ZCode.ZCodeBaseURL != "" || len(p.ZCode.Accounts) != 1 || p.ZCode.Accounts[0].Auth.CallbackPort != 0 {
			return ErrConfigInvalid
		}
	case ProviderGLM:
		if p.GLM.BaseURL != "" || len(p.GLM.Accounts) != 1 || strings.TrimSpace(p.GLM.Accounts[0].Auth.Key) == "" {
			return ErrConfigInvalid
		}
	case ProviderOpenAILike:
		u, err := url.Parse(p.OpenAILike.BaseURL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			(u.Scheme != "https" && (u.Scheme != "http" || !isLocalhost(p.OpenAILike.BaseURL))) {
			return fmt.Errorf("%w: base_url должен быть HTTPS (HTTP только для localhost), без логина, пароля, query и фрагмента", ErrConfigInvalid)
		}
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
			return fmt.Errorf("%w: укажите base_url без конечного /v1 — прокси добавляет его сам", ErrConfigInvalid)
		}
		if len(p.OpenAILike.Accounts) != 1 {
			return ErrConfigInvalid
		}
	}
	return m.addEntry(token, "providers", name, raw)
}

func (m *Manager) AddAlias(token, name string, raw json.RawMessage) error {
	var a AliasConfig
	if !validEntryName(name) || checkDuplicateKeys(raw) != nil || strictUnmarshal(raw, &a) != nil || strings.TrimSpace(a.Model) == "" {
		return ErrConfigInvalid
	}
	return m.addEntry(token, "aliases", name, raw)
}

func (m *Manager) AddClient(token, name string) (string, error) {
	if !validEntryName(name) || len(name) > 128 {
		return "", fmt.Errorf("%w: укажите имя клиента без пробелов (до 128 байт)", ErrConfigInvalid)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	key := "sk-" + hex.EncodeToString(secret[:])
	if err := m.addEntry(token, "clients", name, json.RawMessage(strconv.Quote(key))); err != nil {
		return "", err
	}
	return key, nil
}

func (m *Manager) RevokeClient(token, name string) error {
	return m.writeEntry(token, "clients", name, nil, true)
}

func (m *Manager) AddSocks(token, name string, raw json.RawMessage) error {
	return m.saveSocks(token, name, raw, false)
}

func (m *Manager) UpdateSocks(token, name string, raw json.RawMessage) error {
	return m.saveSocks(token, name, raw, true)
}

func (m *Manager) saveSocks(token, name string, raw json.RawMessage, update bool) error {
	path, err := ParseSocksEntry(name, raw)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		URL           string `json:"url"`
		ExpectedIP    string `json:"expected_ip,omitempty"`
		CheckURL      string `json:"check_url,omitempty"`
		CheckInterval string `json:"check_interval,omitempty"`
	}{path.URL, path.ExpectedIP, path.CheckURL, path.CheckInterval.String()})
	if err != nil {
		return err
	}
	return m.writeEntry(token, "socks", name, data, update)
}

func ParseSocksEntry(name string, raw json.RawMessage) (socks.ResolvedConfig, error) {
	var input struct {
		SocksConfig
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !validEntryName(name) || name == "none" || checkDuplicateKeys(raw) != nil || strictUnmarshal(raw, &input) != nil {
		return socks.ResolvedConfig{}, fmt.Errorf("%w: задайте имя (кроме none), адрес и интервал в формате 60s", ErrConfigInvalid)
	}
	path := input.SocksConfig
	path.URL = strings.TrimSpace(path.URL)
	if !strings.Contains(path.URL, "://") {
		path.URL = "socks5h://" + path.URL
	}
	u, err := url.Parse(path.URL)
	if err != nil || u.Scheme != "socks5h" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return socks.ResolvedConfig{}, fmt.Errorf("%w: требуется SOCKS URL вида socks5h://host:port", ErrConfigInvalid)
	}
	if port, err := strconv.ParseUint(u.Port(), 10, 16); err != nil || port == 0 {
		return socks.ResolvedConfig{}, fmt.Errorf("%w: укажите порт SOCKS от 1 до 65535", ErrConfigInvalid)
	}
	if u.User == nil && (input.Login != "" || input.Password != "") {
		u.User = url.User(input.Login)
		if input.Password != "" {
			u.User = url.UserPassword(input.Login, input.Password)
		}
		path.URL = u.String()
	}
	if path.ExpectedIP != "" {
		if ip, err := netip.ParseAddr(path.ExpectedIP); err != nil || ip.Zone() != "" {
			return socks.ResolvedConfig{}, fmt.Errorf("%w: ожидаемый IP должен быть адресом IPv4 или IPv6", ErrConfigInvalid)
		}
	}
	if path.CheckInterval < 0 {
		return socks.ResolvedConfig{}, fmt.Errorf("%w: интервал проверки не может быть отрицательным", ErrConfigInvalid)
	}
	if path.CheckURL != "" {
		u, err := url.Parse(path.CheckURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return socks.ResolvedConfig{}, fmt.Errorf("%w: URL проверки должен быть HTTP(S) без учётных данных и фрагмента", ErrConfigInvalid)
		}
	}
	return socks.ResolvedConfig{URL: path.URL, ExpectedIP: path.ExpectedIP, CheckURL: path.CheckURL, CheckInterval: time.Duration(path.CheckInterval)}, nil
}

func validEntryName(name string) bool {
	return name != "" && !strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func (m *Manager) addEntry(token, section, name string, raw json.RawMessage) error {
	return m.writeEntry(token, section, name, raw, false)
}

func (m *Manager) writeEntry(token, section, name string, raw json.RawMessage, update bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	configured := m.Snapshot().Cfg.Server.WebUIToken
	if configured == "" {
		return ErrConfigDisabled
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(configured)) != 1 {
		return ErrConfigAuth
	}
	path, err := filepath.EvalSymlinks(m.path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != m.digest {
		return ErrConfigConflict
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	parent := document
	var serverKey string
	if section == "clients" {
		serverKey, parent, err = configObject(document, "server")
		if err != nil {
			return err
		}
	}
	key, entries, err := configObject(parent, section)
	if err != nil {
		return err
	}
	if section == "clients" && raw == nil {
		clients := m.Snapshot().Cfg.Server.Clients
		key, exists := clients[name]
		if !exists {
			return ErrClientNotFound
		}
		for other, value := range clients {
			if other != name && key != "" && key == value {
				return ErrClientShared
			}
		}
		delete(entries, name)
	} else {
		if _, exists := entries[name]; update != exists {
			return ErrConfigConflict
		}
		entries[name] = raw
	}
	parent[key], err = json.Marshal(entries)
	if err != nil {
		return ErrConfigInvalid
	}
	if serverKey != "" {
		document[serverKey], err = json.Marshal(parent)
		if err != nil {
			return ErrConfigInvalid
		}
	}
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return ErrConfigInvalid
	}
	updated = append(updated, '\n')
	cfg, err := decode(updated)
	if err != nil {
		return ErrConfigInvalid
	}
	resolved, err := cfg.resolve(filepath.Dir(m.path))
	if err != nil {
		return ErrConfigInvalid
	}
	if section == "providers" && resolved.Providers[name].Type == ProviderZCode {
		id := resolved.Providers[name].ZCode.Accounts[0].ID
		for other, provider := range resolved.Providers {
			if other != name && provider.Type == ProviderZCode && providerHasAccount(provider, id) {
				return ErrConfigConflict
			}
		}
	}
	if err := m.apply(resolved, func() error { return m.replaceConfig(path, updated) }); err != nil {
		return err
	}
	m.digest = sha256.Sum256(updated)
	return nil
}

func configObject(parent map[string]json.RawMessage, field string) (string, map[string]json.RawMessage, error) {
	key := field
	seen := make([]string, 0, len(parent))
	for k := range parent {
		for _, previous := range seen {
			if strings.EqualFold(k, previous) {
				return "", nil, ErrConfigInvalid
			}
		}
		seen = append(seen, k)
		if strings.EqualFold(k, field) {
			key = k
		}
	}
	var object map[string]json.RawMessage
	if raw := parent[key]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &object); err != nil {
			return "", nil, ErrConfigInvalid
		}
	}
	if object == nil {
		object = make(map[string]json.RawMessage)
	}
	return key, object, nil
}

func (m *Manager) replaceConfig(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fs.ErrInvalid
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".llm-proxy-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(current) != m.digest {
		return ErrConfigConflict
	}
	return os.Rename(tmp.Name(), path)
}

// Stop отменяет контекст текущего снапшота — graceful shutdown (SPEC 3.3).
func (m *Manager) Stop() {
	if s := m.snapshot.Load(); s != nil {
		s.cancel()
	}
}
