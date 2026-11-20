// Пакет internal/config — типы config.json, строгая загрузка/валидация и
// хот-релоад (SIGHUP-оркестрация). Про tokens.json пакет не знает ничего:
// владелец файла — upstream/codex (SPEC 3.2, 13); config лишь резолвит путь.
package config

import (
	"encoding/json"
	"fmt"
	"time"

	"llm-proxy/internal/socks"
)

// Провайдерные type-дискриминаторы (SPEC 3.1).
const (
	ProviderCodex      = "codex"
	ProviderGLM        = "glm"
	ProviderZCode      = "zcode"
	ProviderOpenAILike = "openai-like"
)

// Маршруты к моделям zcode (SPEC 7.4): enum-валидация при загрузке.
const (
	ZCodeRoutePAT       = "pat"        // дефолт: standalone-путь api.z.ai с PAT
	ZCodeRouteZcodePlan = "zcode_plan" // внутренний канал zcode.z.ai с zcode_jwt
)

// Дефолты origin'ов и версии клиента zcode (SPEC 3.1/7.4): два origin'а —
// бизнес-API (PAT-цепочка + модели pat-маршрута) и OAuth/zcode-plan.
const (
	DefaultZCodeOrigin        = "https://api.z.ai"
	DefaultZCodeOAuthOrigin   = "https://zcode.z.ai"
	DefaultZCodeClientVersion = "3.17.2"
)

// Дефолты длительностей (CF-3); значения — из SPEC 3.1 и SPEC 6.
const (
	DefaultStreamIdleTimeout      = 5 * time.Minute
	DefaultUpstreamConnectTimeout = 30 * time.Second
	DefaultShutdownTimeout        = 30 * time.Second
	DefaultSocksCheckInterval     = 60 * time.Second
	// Совпадает с socks.defaultCheckURL (fewshot); дублируется, чтобы config
	// резолвил дефолт без экспорта из socks, а socks.New оставил guard на всякий случай.
	DefaultSocksCheckURL = "https://api.ipify.org"
)

// Duration — строка вида "30s" в config.json. Парсится time.ParseDuration прямо
// при декодировании: опечатка падает на загрузке конфига, а не в рантайме.
type Duration time.Duration

// UnmarshalJSON реализует json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`длительность должна быть JSON-строкой вида "30s": %w`, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("некорректная длительность %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Config — сырой корень config.json (SPEC 3.1). Длительности уже Duration
// (парсятся при декодировании), провайдеры — сырые JSON: их форму выбирает
// поле type, поэтому строгая декодировка каждого элемента идёт отдельно.
type Config struct {
	Server    ServerConfig               `json:"server"`
	Storage   StorageConfig              `json:"storage"`
	Socks     map[string]SocksConfig     `json:"socks"`
	Providers map[string]json.RawMessage `json:"providers"`
	Aliases   map[string]AliasConfig     `json:"aliases"`
	Models    map[string]ModelConfig     `json:"models"`
}

// ServerConfig — секция server.
type ServerConfig struct {
	Listen                 string            `json:"listen"`
	WebUIToken             string            `json:"webui_token"`
	Clients                map[string]string `json:"clients"`
	StreamIdleTimeout      Duration          `json:"stream_idle_timeout"`      // дефолт 5m
	UpstreamConnectTimeout Duration          `json:"upstream_connect_timeout"` // дефолт 30s
	ShutdownTimeout        Duration          `json:"shutdown_timeout"`         // дефолт 30s
}

// StorageConfig — секция storage; пустые пути резолвятся «рядом с config.json».
type StorageConfig struct {
	DBPath          string `json:"db_path"`
	TokensPath      string `json:"tokens_path"`
	ZcodeTokensPath string `json:"zcode_tokens_path"`
}

// SocksConfig — один SOCKS-путь пула.
type SocksConfig struct {
	URL           string   `json:"url"`
	ExpectedIP    string   `json:"expected_ip"`
	CheckURL      string   `json:"check_url"`      // дефолт api.ipify.org
	CheckInterval Duration `json:"check_interval"` // дефолт 60s
}

// DefaultCodexOrigin — апстрим codex по умолчанию (SPEC 3.1/7.1): origin, от
// которого модуль codex достраивает путь /backend-api/codex/responses.
const DefaultCodexOrigin = "https://chatgpt.com"

// DefaultCodexAuthOrigin — OAuth-сервер codex по умолчанию (SPEC 3.1/7.1):
// origin, от которого достраиваются /oauth/authorize и /oauth/token.
const DefaultCodexAuthOrigin = "https://auth.openai.com"

// CodexProvider — провайдер codex. base_url и auth_base_url — опциональные
// ОРИГИНЫ (дефолты https://chatgpt.com и https://auth.openai.com, SPEC 3.1:
// «у codex — аналогично» glm-правилу base_url, ратифицировано для Ф3);
// нужны, чтобы указать прокси на совместимые стабы в тестах/smoke.
type CodexProvider struct {
	Type        string         `json:"type"`
	BaseURL     string         `json:"base_url"`      // дефолт https://chatgpt.com
	AuthBaseURL string         `json:"auth_base_url"` // дефолт https://auth.openai.com
	Accounts    []CodexAccount `json:"accounts"`
}

// CodexAccount — OAuth-аккаунт codex.
type CodexAccount struct {
	ID    string    `json:"id"`
	Email string    `json:"email"`
	Auth  OAuthAuth `json:"auth"`
	Socks string    `json:"socks"`
}

// OAuthAuth — auth-блок codex-аккаунта (только oauth; поля ключа сюда не влезут).
type OAuthAuth struct {
	Method       string `json:"method"`
	CallbackPort int    `json:"callback_port"` // 0/не задан → codex.DefaultCallbackPort (SPEC 7.1); применяется в codex.RedirectURI
}

// DefaultGLMOrigin — апстрим glm по умолчанию (SPEC 7.2): origin, от которого
// модуль glm достраивает пути /api/anthropic/v1/messages и /api/{coding/}paas/v4.
const DefaultGLMOrigin = "https://api.z.ai"

// GLMProvider — провайдер glm (z.ai). base_url опционален и это ОРИГИН
// (scheme://host[:port], SPEC 3.1: все пути добавляет прокси): без него —
// https://api.z.ai. Поле нужно только чтобы указать прокси на
// z.ai-совместимый шлюз (в тестах/smoke — фейковый эндпоинт); поведение с
// дефолтом в точности SPEC 7.2.
type GLMProvider struct {
	Type     string       `json:"type"`
	BaseURL  string       `json:"base_url"`
	Accounts []GLMAccount `json:"accounts"`
}

// GLMAccount — api_key-аккаунт glm. Поле allow_direct у него нет намеренно:
// апстрим glm по умолчанию внешний (api.z.ai), прямого пути для него не
// существует (FC-6). socks:"none" допустим только с переопределённым
// локальным base_url провайдера (фейковый эндпоинт тестов/smoke).
type GLMAccount struct {
	ID          string       `json:"id"`
	Auth        KeyAuth      `json:"auth"`
	CodingPlan  bool         `json:"coding_plan"`
	Socks       string       `json:"socks"`
	PlanCredits *PlanCredits `json:"plan_credits"`
}

// PlanCredits — лимиты glm Coding Plan для экрана квот (SPEC 7.2).
type PlanCredits struct {
	Window5h     int64 `json:"window_5h"`
	WindowWeekly int64 `json:"window_weekly"`
}

// KeyAuth — auth-блок api_key-аккаунтов (glm, openai-like).
type KeyAuth struct {
	Method string `json:"method"`
	Key    string `json:"key"`
}

// ZCodeProvider — провайдер zcode (z.ai-аккаунты через OAuth-цепочку ZCode,
// SPEC 7.4). Два опциональных origin'а: base_url — бизнес-API (PAT-цепочка и
// модели pat-маршрута), zcode_base_url — OAuth-флоу init/poll и маршрут
// zcode_plan; дефолты внешние (api.z.ai / zcode.z.ai), поэтому правило
// socks:"none" применяется к ОБОИМ (FC-6, SPEC 3.1). Переопределение —
// только чтобы указать прокси на совместимые стабы в тестах/smoke.
type ZCodeProvider struct {
	Type          string         `json:"type"`
	BaseURL       string         `json:"base_url"`       // дефолт https://api.z.ai
	ZCodeBaseURL  string         `json:"zcode_base_url"` // дефолт https://zcode.z.ai
	ClientVersion string         `json:"client_version"` // дефолт 3.17.2; пустая строка — ошибка валидации
	Route         string         `json:"route"`          // pat (дефолт) | zcode_plan (SPEC 7.4)
	UsagePoll     Duration       `json:"usage_poll"`     // поллинг /api/monitor/usage/quota/limit (Ф8), 0 — выключен
	Accounts      []ZCodeAccount `json:"accounts"`

	// clientVersionSet — различает «поле отсутствует» (дефолт 3.17.2) и явную
	// пустую строку (ошибка, SPEC 7.4 «client_version непустой»): json в Go
	// декодирует оба случая в "", ставится при парсинге (load.go), в файл не
	// сериализуется.
	clientVersionSet bool
}

// ZCodeAccount — OAuth-аккаунт zcode (method: "oauth"). Токены живут в
// zcode-tokens.json (владелец upstream/zcode.Store), в конфиге их нет.
// plan_credits — лимиты плана для экрана квот (как у glm, SPEC 7.4).
type ZCodeAccount struct {
	ID          string       `json:"id"`
	Email       string       `json:"email"`
	Auth        OAuthAuth    `json:"auth"`
	Socks       string       `json:"socks"`
	PlanCredits *PlanCredits `json:"plan_credits"`
}

// OpenAILikeProvider — generic OpenAI-совместимый провайдер (ollama, прочие).
type OpenAILikeProvider struct {
	Type         string              `json:"type"`
	BaseURL      string              `json:"base_url"`
	UsagePoll    Duration            `json:"usage_poll"`    // 0 — поллинг выключен (дефолт)
	ForwardPaths []string            `json:"forward_paths"` // префиксы прозрачного форварда
	Accounts     []OpenAILikeAccount `json:"accounts"`
}

// OpenAILikeAccount — api_key-аккаунт openai-like; единственный тип аккаунта,
// где схема допускает allow_direct (осознанный прямой путь к внешнему апстриму).
type OpenAILikeAccount struct {
	ID          string  `json:"id"`
	Auth        KeyAuth `json:"auth"`
	Socks       string  `json:"socks"`
	AllowDirect bool    `json:"allow_direct"`
}

// AliasConfig — цель алиаса (одна цель в v1, SPEC 3.1).
type AliasConfig struct {
	Account string `json:"account"` // "provider/account_id"
	Model   string `json:"model"`
}

// ModelConfig — параметры модели для учёта (опционально).
type ModelConfig struct {
	PriceUSDPerM *PricePerM   `json:"price_usd_per_m"`
	GLMCredits   *CreditMults `json:"glm_credits"`
	Subscription bool         `json:"subscription"`
}

// PricePerM — цена за миллион токенов, $ (оценка для PAYG).
type PricePerM struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input"`
}

// CreditMults — мультипликаторы кредитов glm Coding Plan на 10k токенов.
type CreditMults struct {
	InputMult  float64 `json:"input_mult"`
	CachedMult float64 `json:"cached_mult"`
	OutputMult float64 `json:"output_mult"`
}

// Provider — провайдер после парсинга: Type + ровно один не-nil вариант
// конкретной структуры. Будущие фазы свитчатся по Type (R3: без рефлексии).
type Provider struct {
	Type       string
	Codex      *CodexProvider
	GLM        *GLMProvider
	ZCode      *ZCodeProvider
	OpenAILike *OpenAILikeProvider
}

// ResolvedConfig — конфиг после загрузки: длительности — time.Duration,
// дефолты применены, ссылки проверены, для каждого socks-имени готов
// socks.ResolvedConfig. Неизменяема после возврата из Load/Reload.
type ResolvedConfig struct {
	Server    ServerConfig
	Storage   StorageConfig
	Socks     map[string]socks.ResolvedConfig
	Providers map[string]Provider
	Aliases   map[string]AliasConfig
	Models    map[string]ModelConfig
}
