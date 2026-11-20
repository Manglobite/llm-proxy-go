// Загрузка config.json: строгий парсинг (DisallowUnknownFields — SPEC 3.1),
// валидация (FC-3, FC-6, CF-2, CF-3) и маскировка секретов в ошибках (CF-6).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"llm-proxy/internal/socks"
)

// Load читает config.json: строгий парсинг, валидация, разрешение дефолтов.
// Секреты (пароли socks-URL, api-ключи) в тексте ошибок не появляются (CF-6).
func Load(path string) (*ResolvedConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := decode(data)
	if err != nil {
		return nil, err
	}
	// Дефолты storage «рядом с config.json» — нужен каталог файла (SPEC 3.1).
	return cfg.resolve(filepath.Dir(path))
}

// decode — строгая декодировка корня: неизвестное поле (например expectedip
// вместо expected_ip) — ошибка, а не молчаливый дефолт (security-значимые
// настройки не должны выключаться опечаткой).
func decode(data []byte) (*Config, error) {
	// Дубликаты ключей Decoder молча разрешает (берёт последний) — для
	// security-конфига это способ незаметно переопределить настройку (F4).
	if err := checkDuplicateKeys(data); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// Decoder.Decode пропустил бы мусор после первого значения — строгость до конца файла.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("config: лишние данные после JSON-документа")
	}
	return &cfg, nil
}

// checkDuplicateKeys идёт по JSON-токенам и отклоняет дубликаты ключей в
// объектах. Достаточно уровня парента: дубль в секции (socks, aliases,
// server.clients, models, providers) или в объекте провайдера — ошибка.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	// Стек контейнеров; имя объекта — ключ, под которым он найден
	// (корень — "config"). wantKey: в объекте следующий string-токен — ключ.
	type frame struct {
		name    string
		isObj   bool
		keys    map[string]bool
		wantKey bool
	}
	stack := []*frame{{name: "config", isObj: true, keys: map[string]bool{}, wantKey: true}}
	lastKey := "" // последний ключ текущего объекта — имя для вложенного контейнера
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Синтаксические ошибки оставим основному Decode — у него
			// понятные сообщения с координатами.
			return nil
		}
		cur := stack[len(stack)-1]
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				name := lastKey
				if name == "" {
					name = cur.name // вложенный контейнер без ключа — наследуем родителя
				}
				stack = append(stack, &frame{name: name, isObj: t == '{', keys: map[string]bool{}, wantKey: t == '{'})
				lastKey = ""
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 {
					if parent := stack[len(stack)-1]; parent.isObj {
						parent.wantKey = true // значение (контейнер) прочитано — снова ждём ключ
					}
				}
			}
		case string:
			if cur.isObj && cur.wantKey {
				if cur.keys[t] {
					return fmt.Errorf("duplicate key %q in section %q", t, cur.name)
				}
				cur.keys[t] = true
				cur.wantKey = false
				lastKey = t
			} else if cur.isObj {
				cur.wantKey = true // строка оказалась значением — ждём следующий ключ
			}
		default:
			// Числа/bool/null — скалярные значения: в объекте после них снова ключ.
			if cur.isObj && !cur.wantKey {
				cur.wantKey = true
			}
		}
	}
}

// strictUnmarshal — Decode с DisallowUnknownFields для вложенных сырых значений.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// parseProvider разбирает элемент секции providers: сначала type (дискриминатор),
// затем строгая декодировка в структуру конкретного типа — лишние поля чужого
// типа (например base_url у codex) отваливаются на неизвестном поле (SPEC 3.1).
func parseProvider(pname string, raw json.RawMessage) (Provider, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Provider{}, fmt.Errorf("provider %q: %w", pname, err)
	}
	switch head.Type {
	case ProviderCodex:
		var p CodexProvider
		if err := strictUnmarshal(raw, &p); err != nil {
			return Provider{}, fmt.Errorf("provider %q (codex): %w", pname, err)
		}
		return Provider{Type: ProviderCodex, Codex: &p}, nil
	case ProviderGLM:
		var p GLMProvider
		if err := strictUnmarshal(raw, &p); err != nil {
			return Provider{}, fmt.Errorf("provider %q (glm): %w", pname, err)
		}
		return Provider{Type: ProviderGLM, GLM: &p}, nil
	case ProviderZCode:
		var p ZCodeProvider
		if err := strictUnmarshal(raw, &p); err != nil {
			return Provider{}, fmt.Errorf("provider %q (zcode): %w", pname, err)
		}
		// client_version: отсутствие поля и явная пустая строка различаются
		// (SPEC 7.4 «client_version непустой (дефолт 3.17.2)») — повторный
		// взгляд на сырое значение через указатель, т.к. обычный decode оба
		// случая сваливает в "".
		var probe struct {
			ClientVersion *string `json:"client_version"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return Provider{}, fmt.Errorf("provider %q (zcode): %w", pname, err)
		}
		p.clientVersionSet = probe.ClientVersion != nil
		return Provider{Type: ProviderZCode, ZCode: &p}, nil
	case ProviderOpenAILike:
		var p OpenAILikeProvider
		if err := strictUnmarshal(raw, &p); err != nil {
			return Provider{}, fmt.Errorf("provider %q (openai-like): %w", pname, err)
		}
		return Provider{Type: ProviderOpenAILike, OpenAILike: &p}, nil
	case "":
		return Provider{}, fmt.Errorf("provider %q: отсутствует обязательное поле type", pname)
	default:
		return Provider{}, fmt.Errorf("provider %q: неизвестный type %q (допустимо codex|glm|zcode|openai-like)", pname, head.Type)
	}
}

// resolve валидирует сырой конфиг и собирает ResolvedConfig: длительности уже
// time.Duration, дефолты применены, все ссылки проверены.
func (c *Config) resolve(configDir string) (*ResolvedConfig, error) {
	rc := &ResolvedConfig{
		Socks:     make(map[string]socks.ResolvedConfig, len(c.Socks)),
		Providers: make(map[string]Provider, len(c.Providers)),
		Aliases:   make(map[string]AliasConfig, len(c.Aliases)),
		Models:    make(map[string]ModelConfig, len(c.Models)),
	}

	// server: listen обязателен — пустой адрес заставил бы http.Server слушать
	// все интерфейсы, что противоположно замыслу локального прокси.
	if c.Server.Listen == "" {
		return nil, errors.New("config: server.listen обязателен")
	}
	rc.Server = c.Server
	// Отрицательная длительность — ошибка: «подождать минус пять секунд» —
	// всегда опечатка, молчаливое применение нуля/дефолта скрыло бы её (F5).
	switch {
	case rc.Server.StreamIdleTimeout < 0:
		return nil, fmt.Errorf("config: server.stream_idle_timeout отрицательный (%v)", time.Duration(rc.Server.StreamIdleTimeout))
	case rc.Server.StreamIdleTimeout == 0:
		rc.Server.StreamIdleTimeout = Duration(DefaultStreamIdleTimeout)
	}
	switch {
	case rc.Server.UpstreamConnectTimeout < 0:
		return nil, fmt.Errorf("config: server.upstream_connect_timeout отрицательный (%v)", time.Duration(rc.Server.UpstreamConnectTimeout))
	case rc.Server.UpstreamConnectTimeout == 0:
		rc.Server.UpstreamConnectTimeout = Duration(DefaultUpstreamConnectTimeout)
	}
	switch {
	case rc.Server.ShutdownTimeout < 0:
		return nil, fmt.Errorf("config: server.shutdown_timeout отрицательный (%v)", time.Duration(rc.Server.ShutdownTimeout))
	case rc.Server.ShutdownTimeout == 0:
		rc.Server.ShutdownTimeout = Duration(DefaultShutdownTimeout)
	}

	// storage: пустые пути — дефолт «рядом с config.json» (SPEC 3.1);
	// относительный путь от пользователя — тоже относительно каталога
	// config.json, а не CWD процесса (артефакты не должны зависеть от того,
	// откуда запущен бинарник).
	rc.Storage = c.Storage
	if rc.Storage.DBPath == "" {
		rc.Storage.DBPath = filepath.Join(configDir, "proxy.db")
	} else if !filepath.IsAbs(rc.Storage.DBPath) {
		rc.Storage.DBPath = filepath.Join(configDir, rc.Storage.DBPath)
	}
	if rc.Storage.TokensPath == "" {
		rc.Storage.TokensPath = filepath.Join(configDir, "tokens.json")
	} else if !filepath.IsAbs(rc.Storage.TokensPath) {
		rc.Storage.TokensPath = filepath.Join(configDir, rc.Storage.TokensPath)
	}
	if rc.Storage.ZcodeTokensPath == "" {
		rc.Storage.ZcodeTokensPath = filepath.Join(configDir, "zcode-tokens.json")
	} else if !filepath.IsAbs(rc.Storage.ZcodeTokensPath) {
		rc.Storage.ZcodeTokensPath = filepath.Join(configDir, rc.Storage.ZcodeTokensPath)
	}

	// socks: FC-3 (только socks5h, I2), дефолты чека (CF-3), имя "none" зарезервировано.
	for name, sc := range c.Socks {
		if name == "" {
			return nil, errors.New("config: имя socks-пути не может быть пустым")
		}
		if name == "none" {
			// "none" — синтетическое имя прямого пути в аккаунтах; путь с таким
			// именем сделал бы семантику FC-6 двусмысленной.
			return nil, errors.New(`config: socks: имя "none" зарезервировано для прямого пути аккаунтов`)
		}
		if err := validateSocksURL(sc.URL); err != nil {
			return nil, fmt.Errorf("socks %q: %w", name, err)
		}
		switch {
		case sc.CheckInterval < 0:
			return nil, fmt.Errorf("socks %q: check_interval отрицательный (%v)", name, time.Duration(sc.CheckInterval))
		case sc.CheckInterval == 0:
			sc.CheckInterval = Duration(DefaultSocksCheckInterval)
		}
		if sc.CheckURL == "" {
			sc.CheckURL = DefaultSocksCheckURL
		}
		rc.Socks[name] = socks.ResolvedConfig{
			URL:           sc.URL,
			ExpectedIP:    sc.ExpectedIP,
			CheckURL:      sc.CheckURL,
			CheckInterval: time.Duration(sc.CheckInterval),
		}
	}

	// providers: строгий парсинг по type + валидация аккаунтов.
	for pname, raw := range c.Providers {
		if pname == "" {
			return nil, errors.New("config: имя провайдера не может быть пустым")
		}
		p, err := parseProvider(pname, raw)
		if err != nil {
			return nil, err
		}
		if err := p.validate(rc.Socks, pname); err != nil {
			return nil, err
		}
		rc.Providers[pname] = p
	}

	// aliases: CF-2 — имя без пробелов, ссылка "provider/account" обязана резолвиться.
	for aname, a := range c.Aliases {
		if aname == "" {
			return nil, errors.New("config: имя алиаса не может быть пустым")
		}
		if strings.ContainsFunc(aname, unicode.IsSpace) {
			return nil, fmt.Errorf("alias %q: имя алиаса не должно содержать пробелов (SPEC 3.1)", aname)
		}
		provName, accID, ok := strings.Cut(a.Account, "/")
		if !ok || provName == "" || accID == "" {
			return nil, fmt.Errorf(`alias %q: account %q должен быть вида "provider/account_id"`, aname, a.Account)
		}
		prov, ok := rc.Providers[provName]
		if !ok {
			return nil, fmt.Errorf("alias %q: провайдер %q не найден", aname, provName)
		}
		if !providerHasAccount(prov, accID) {
			return nil, fmt.Errorf("alias %q: аккаунт %q не найден у провайдера %q", aname, accID, provName)
		}
		if a.Model == "" {
			return nil, fmt.Errorf("alias %q: model обязательна", aname)
		}
		rc.Aliases[aname] = a
	}

	// models: переносятся как есть; содержательную интерпретацию ценностей
	// делает фаза учёта (Ф4), здесь валидировать нечего.
	for mname, m := range c.Models {
		rc.Models[mname] = m
	}

	return rc, nil
}

// resolveOrigin — валидация опционального base_url-оригина провайдера:
// пустое значение → дефолт (SPEC 3.1: все пути добавляет прокси, поэтому в
// конфиге только scheme://host[:port]); непустое — парсится, схема строго
// http/https. Единая точка для glm/codex/openai-like — правило base_url
// звучит для всех одинаково, дублировать ветку в каждом type — рассинхрон.
func resolveOrigin(pname, field, raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("provider %q: %s %q не парсится: %v", pname, field, MaskURL(raw), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("provider %q: схема %s должна быть http/https, получено %q", pname, field, u.Scheme)
	}
	return raw, nil
}

// validate — проверки аккаунтов провайдера, зависящие от его типа:
// требуемый auth.method, наличие/локальность base_url, ссылки на socks.
func (p Provider) validate(resolved map[string]socks.ResolvedConfig, pname string) error {
	switch p.Type {
	case ProviderCodex:
		// Оба origins — опциональные base_url (SPEC 3.1, ратифицировано Ф3):
		// дефолты внешние (chatgpt.com / auth.openai.com), поэтому socks:"none"
		// у codex-аккаунта по-прежнему невозможен БЕЗ переопределения origin'ов
		// на локальный стаб (фейковый эндпоинт тестов/smoke); поля allow_direct
		// в схеме типа нет.
		base, err := resolveOrigin(pname, "base_url", p.Codex.BaseURL, DefaultCodexOrigin)
		if err != nil {
			return err
		}
		p.Codex.BaseURL = base
		auth, err := resolveOrigin(pname, "auth_base_url", p.Codex.AuthBaseURL, DefaultCodexAuthOrigin)
		if err != nil {
			return err
		}
		p.Codex.AuthBaseURL = auth
		for _, a := range p.Codex.Accounts {
			if err := validateAccount(resolved, pname, a.ID, a.Socks, false, a.Auth.Method, "oauth", base); err != nil {
				return err
			}
		}
	case ProviderGLM:
		// base_url — опциональный ОРИГИН апстрима (дефолт https://api.z.ai,
		// SPEC 7.2); резолвим дефолт здесь, чтобы и валидация FC-6, и модуль
		// glm видели одно и то же эффективное значение. Схема — как у
		// openai-like: только http/https.
		base, err := resolveOrigin(pname, "base_url", p.GLM.BaseURL, DefaultGLMOrigin)
		if err != nil {
			return err
		}
		p.GLM.BaseURL = base
		// Апстрим glm по умолчанию внешний (api.z.ai): socks:"none" запрещён,
		// как у codex (FC-6); исключение — переопределённый локальный origin
		// (фейковый эндпоинт тестов): isLocalhost внутри validateAccount.
		for _, a := range p.GLM.Accounts {
			if err := validateAccount(resolved, pname, a.ID, a.Socks, false, a.Auth.Method, "api_key", base); err != nil {
				return err
			}
		}
	case ProviderZCode:
		// Два опциональных origin'а (SPEC 3.1/7.4): дефолты внешние
		// (api.z.ai / zcode.z.ai), резолвим здесь, чтобы валидация FC-6 и
		// модуль zcode видели одно и то же эффективное значение.
		base, err := resolveOrigin(pname, "base_url", p.ZCode.BaseURL, DefaultZCodeOrigin)
		if err != nil {
			return err
		}
		p.ZCode.BaseURL = base
		zbase, err := resolveOrigin(pname, "zcode_base_url", p.ZCode.ZCodeBaseURL, DefaultZCodeOAuthOrigin)
		if err != nil {
			return err
		}
		p.ZCode.ZCodeBaseURL = zbase
		// client_version: отсутствие → дефолт, явная пустая строка → ошибка
		// (SPEC 7.4: «client_version непустой (дефолт 3.17.2)»).
		if !p.ZCode.clientVersionSet {
			p.ZCode.ClientVersion = DefaultZCodeClientVersion
		} else if p.ZCode.ClientVersion == "" {
			return fmt.Errorf("provider %q: client_version не может быть пустой строкой (дефолт %q)", pname, DefaultZCodeClientVersion)
		}
		// route: enum pat|zcode_plan, отсутствие → pat (SPEC 3.1/7.4).
		switch p.ZCode.Route {
		case "":
			p.ZCode.Route = ZCodeRoutePAT
		case ZCodeRoutePAT, ZCodeRouteZcodePlan:
			// легальные значения
		default:
			return fmt.Errorf("provider %q: route %q недопустим (допустимо pat|zcode_plan)", pname, p.ZCode.Route)
		}
		// usage_poll: 0/отсутствие — выключено, минус — опечатка («поллить
		// минус пять секунд» не бывает), как у openai-like (SPEC 7.3/15).
		if p.ZCode.UsagePoll < 0 {
			return fmt.Errorf("provider %q: usage_poll отрицательный (%v)", pname, time.Duration(p.ZCode.UsagePoll))
		}
		for _, a := range p.ZCode.Accounts {
			if err := validateAccount(resolved, pname, a.ID, a.Socks, false, a.Auth.Method, "oauth", base); err != nil {
				return err
			}
			// FC-6 для двух origin'ов (SPEC 3.1): socks:"none" у zcode-аккаунта
			// легален только когда ЛОБА origin'а локальные; validateAccount
			// проверил только base_url — второй проверяем здесь (поля
			// allow_direct в схеме zcode-аккаунта нет).
			if a.Socks == "none" && !isLocalhost(zbase) {
				return fmt.Errorf(`provider %q, аккаунт %q: socks "none" запрещён: zcode_base_url не localhost (%s) — для zcode оба origin'а обязаны быть локальными (fail-closed, SPEC 3.1)`,
					pname, a.ID, upstreamDesc(zbase))
			}
		}
	case ProviderOpenAILike:
		// Отрицательный usage_poll — всегда опечатка («поллить минус пять
		// секунд» не бывает): 0/отсутствие = выключено, минус — ошибка
		// валидации, как у остальных длительностей (SPEC 7.3).
		if p.OpenAILike.UsagePoll < 0 {
			return fmt.Errorf("provider %q: usage_poll отрицательный (%v)", pname, time.Duration(p.OpenAILike.UsagePoll))
		}
		base := p.OpenAILike.BaseURL
		if base == "" {
			return fmt.Errorf("provider %q: base_url обязателен", pname)
		}
		base, err := resolveOrigin(pname, "base_url", base, base)
		if err != nil {
			return err
		}
		p.OpenAILike.BaseURL = base
		for _, a := range p.OpenAILike.Accounts {
			if err := validateAccount(resolved, pname, a.ID, a.Socks, a.AllowDirect, a.Auth.Method, "api_key", base); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("provider %q: неизвестный type %q", pname, p.Type)
	}
	return nil
}

// validateAccount — общая проверка аккаунта любого типа провайдера: id,
// auth.method, ссылка на socks и правило socks:"none" (FC-6/I5).
// baseURL — корень апстрима провайдера; пуст для codex/glm (апстрим зашит в модуле).
func validateAccount(resolved map[string]socks.ResolvedConfig, pname, id, socksName string, allowDirect bool, method, wantMethod, baseURL string) error {
	if id == "" {
		return fmt.Errorf("provider %q: у аккаунта пустой id", pname)
	}
	// id участвует в ссылках "provider/account": пробелы и слэш в нём ломают ссылки.
	if strings.ContainsFunc(id, unicode.IsSpace) || strings.Contains(id, "/") {
		return fmt.Errorf(`provider %q: id аккаунта %q не должен содержать пробелов и "/"`, pname, id)
	}
	if method != wantMethod {
		return fmt.Errorf("provider %q, аккаунт %q: auth.method = %q, ожидался %q", pname, id, method, wantMethod)
	}
	if socksName == "none" {
		// FC-6: прямой путь безусловно разрешён только на localhost; для внешнего
		// апстрима нужен явный allow_direct на аккаунте (осознанное исключение).
		if !isLocalhost(baseURL) && !allowDirect {
			return fmt.Errorf(`provider %q, аккаунт %q: socks "none" запрещён: апстрим не localhost (%s), а allow_direct у аккаунта нет (fail-closed, SPEC 3.1)`,
				pname, id, upstreamDesc(baseURL))
		}
		return nil
	}
	if _, ok := resolved[socksName]; !ok {
		return fmt.Errorf("provider %q, аккаунт %q: socks %q отсутствует в секции socks", pname, id, socksName)
	}
	return nil
}

// providerHasAccount — есть ли у провайдера аккаунт с данным id (CF-2).
func providerHasAccount(p Provider, id string) bool {
	switch p.Type {
	case ProviderCodex:
		for _, a := range p.Codex.Accounts {
			if a.ID == id {
				return true
			}
		}
	case ProviderGLM:
		for _, a := range p.GLM.Accounts {
			if a.ID == id {
				return true
			}
		}
	case ProviderZCode:
		for _, a := range p.ZCode.Accounts {
			if a.ID == id {
				return true
			}
		}
	case ProviderOpenAILike:
		for _, a := range p.OpenAILike.Accounts {
			if a.ID == id {
				return true
			}
		}
	}
	return false
}

// isLocalhost — точное равенство hostname одному из localhost|127.0.0.1|::1
// после нормализации (порт отброшен, IPv6 без скобок). Сравнение подстрокой/
// суффиксом запрещено (I5): evil-localhost.com — внешний хост.
func isLocalhost(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// validateSocksURL — схема только socks5h:// : DNS резолвится на стороне SOCKS,
// утечек через локальный резолвер нет (FC-3, I2).
func validateSocksURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url не парсится: %v (%s)", err, MaskURL(raw))
	}
	if u.Scheme != "socks5h" {
		return fmt.Errorf("допустима только схема socks5h:// (DNS резолвится на стороне прокси), получено %q в %s", u.Scheme, MaskURL(raw))
	}
	if u.Host == "" {
		return fmt.Errorf("не указан host:port в %s", MaskURL(raw))
	}
	return nil
}

// MaskURL вырезает userinfo из URL — для сообщений об ошибках и логов (CF-6):
// socks5h://user:pass@host:port → socks5h://***@host:port.
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<непарсируемый url>"
	}
	if u.User == nil {
		return u.String()
	}
	// url.User("***") при выводе экранировал бы * в %2A, поэтому userinfo
	// отбрасываем и вставляем маску в готовую строку вручную.
	clean := *u
	clean.User = nil
	s := clean.String()
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[:i+3] + "***@" + s[i+3:]
	}
	return s
}

// upstreamDesc — человекочитаемое описание апстрима для сообщений об ошибках.
// base_url маскируется: пользователь мог вписать userinfo прямо в него.
func upstreamDesc(baseURL string) string {
	if baseURL == "" {
		return "base_url отсутствует: апстрим зашит в модуле провайдера и внешний"
	}
	return MaskURL(baseURL)
}
