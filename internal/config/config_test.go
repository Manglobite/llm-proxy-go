package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigExamples(t *testing.T) {
	for _, name := range []string{"config.example.json", "config.local.example.json"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(filepath.Join("..", "..", name))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.Listen != "127.0.0.1:8787" {
				t.Fatal("example must listen on loopback")
			}
			if name == "config.local.example.json" {
				if cfg.Server.WebUIToken != "" || len(cfg.Server.Clients)+len(cfg.Socks)+len(cfg.Providers)+len(cfg.Aliases) != 0 {
					t.Fatal("first-run config must not enable sample credentials or upstreams")
				}
				return
			}
			types := make(map[string]bool)
			for _, provider := range cfg.Providers {
				types[provider.Type] = true
			}
			for _, kind := range []string{ProviderOpenAILike, ProviderGLM, ProviderCodex, ProviderZCode} {
				if !types[kind] {
					t.Errorf("missing example for %s", kind)
				}
			}
			if len(cfg.Socks) == 0 || len(cfg.Aliases) == 0 || len(cfg.Models) == 0 || len(cfg.Server.Clients) == 0 {
				t.Fatal("example must cover SOCKS, aliases, models and client keys")
			}
		})
	}
}

// writeConfig пишет конфиг во временный каталог и возвращает путь к нему.
func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("запись %s: %v", path, err)
	}
	return path
}

// SPEC 3.1: строгое декодирование — опечатанное поле (`expectedip` вместо
// `expected_ip`) должно быть ошибкой валидации, а не молчаливым дефолтом.
func TestLoadRejectsUnknownFieldInSocksConfig(t *testing.T) {
	cfg := `{"socks": {"home": {"url": "socks5h://127.0.0.1:1080", "expectedip": "1.2.3.4"}}}`
	_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err == nil {
		t.Fatal("ожидали ошибку строгого парсинга")
	}
	if !strings.Contains(err.Error(), "expectedip") {
		t.Fatalf("ошибка должна называть опечатанное поле: %v", err)
	}
}

// Строгость действует и на верхний уровень документа.
func TestLoadRejectsUnknownTopLevelField(t *testing.T) {
	cfg := `{"server": {"listen": "127.0.0.1:8787"}, "socksz": {}}`
	_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err == nil || !strings.Contains(err.Error(), "socksz") {
		t.Fatalf("ожидали ошибку на неизвестном верхнеуровневом поле: %v", err)
	}
}

// FC-3: схема SOCKS-URL — только socks5h:// (DNS резолвится на стороне прокси, I2).
func TestLoadRejectsNonSocks5hScheme(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"socks5 без h", "socks5://10.0.0.5:1080"},
		{"http", "http://10.0.0.5:1080"},
		{"пустой url", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`{"server": {"listen": "127.0.0.1:8787"}, "socks": {"home": {"url": %q}}}`, tt.url)
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if err == nil || !strings.Contains(err.Error(), "socks5h") {
				t.Fatalf("ожидали ошибку схемы socks5h, получили: %v", err)
			}
		})
	}
}

// FC-6/I5: socks:"none" разрешён безусловно только на localhost (точное равенство
// hostname); внешний апстрим — только с явным allow_direct на аккаунте.
func TestSocksNoneRequiresLocalhostBaseURLorAllowDirect(t *testing.T) {
	tests := []struct {
		name        string
		baseURL     string
		allowDirect bool
		wantErr     bool
	}{
		{"внешний без allow_direct", "https://api.example.com", false, true},
		{"внешний с allow_direct", "https://api.example.com", true, false},
		{"localhost", "http://localhost:11434", false, false},
		{"127.0.0.1", "http://127.0.0.1:11434", false, false},
		{"::1", "http://[::1]:11434", false, false},
		{"evil-localhost.com — внешний", "https://evil-localhost.com", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allow := ""
			if tt.allowDirect {
				allow = `, "allow_direct": true`
			}
			cfg := fmt.Sprintf(`{
				"server": {"listen": "127.0.0.1:8787"},
				"providers": {"p": {"type": "openai-like", "base_url": %q,
					"accounts": [{"id": "a1", "auth": {"method": "api_key", "key": "k"}, "socks": "none"%s}]}}
			}`, tt.baseURL, allow)
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if tt.wantErr && err == nil {
				t.Fatalf("ожидали reject для base_url %s", tt.baseURL)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ожидали ok для base_url %s, получили: %v", tt.baseURL, err)
			}
		})
	}
}

// CF-2: ссылочная целостность алиасов "provider/account" и имена без пробелов.
// (Уникальность имени структурно гарантирована: алиасы — ключи JSON-объекта.)
func TestLoadValidatesAliasReferencesAndNames(t *testing.T) {
	tests := []struct {
		name      string
		aliasName string
		account   string
		wantErr   string
	}{
		{"ссылка на несуществующего провайдера", "glm/5.3", "ghost/a1", "ghost"},
		{"ссылка на несуществующий аккаунт", "glm/5.3", "glm/nope", "nope"},
		{"пробел в имени алиаса", "work terra", "glm/a1", "work terra"},
		{"валидная ссылка", "glm/5.3", "glm/a1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
				"providers": {"glm": {"type": "glm", "accounts": [
					{"id": "a1", "auth": {"method": "api_key", "key": "k"}, "socks": "home"}]}},
				"aliases": {%q: {"account": %q, "model": "glm-5.3"}}
			}`, tt.aliasName, tt.account)
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ожидали ok, получили: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantErr, err)
			}
		})
	}
}

// CF-3: дефолты длительностей и парсинг пользовательских значений.
func TestLoadAppliesDurationDefaultsAndParsesCustom(t *testing.T) {
	t.Run("дефолты при пустых полях", func(t *testing.T) {
		cfg := `{"server": {"listen": "127.0.0.1:8787"}}`
		rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if time.Duration(rc.Server.StreamIdleTimeout) != 5*time.Minute {
			t.Fatalf("stream_idle_timeout: %v, ожидалось 5m", rc.Server.StreamIdleTimeout)
		}
		if time.Duration(rc.Server.UpstreamConnectTimeout) != 30*time.Second {
			t.Fatalf("upstream_connect_timeout: %v, ожидалось 30s", rc.Server.UpstreamConnectTimeout)
		}
		if time.Duration(rc.Server.ShutdownTimeout) != 30*time.Second {
			t.Fatalf("shutdown_timeout: %v, ожидалось 30s", rc.Server.ShutdownTimeout)
		}
	})

	t.Run("пользовательские значения парсятся", func(t *testing.T) {
		cfg := `{
			"server": {"listen": "127.0.0.1:8787", "stream_idle_timeout": "3m", "upstream_connect_timeout": "10s", "shutdown_timeout": "7s"},
			"socks": {"home": {"url": "socks5h://127.0.0.1:1080", "check_interval": "2m", "check_url": "http://127.0.0.1:1/ip", "expected_ip": "1.2.3.4"}}
		}`
		rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if time.Duration(rc.Server.StreamIdleTimeout) != 3*time.Minute ||
			time.Duration(rc.Server.UpstreamConnectTimeout) != 10*time.Second ||
			time.Duration(rc.Server.ShutdownTimeout) != 7*time.Second {
			t.Fatalf("длительности server применились неверно: %+v", rc.Server)
		}
		sc, ok := rc.Socks["home"]
		if !ok {
			t.Fatal("ожидали socks-путь home")
		}
		if sc.CheckInterval != 2*time.Minute || sc.CheckURL != "http://127.0.0.1:1/ip" || sc.ExpectedIP != "1.2.3.4" {
			t.Fatalf("socks-конфиг применился неверно: %+v", sc)
		}
	})

	t.Run("опечатка в длительности — ошибка", func(t *testing.T) {
		cfg := `{"server": {"listen": "127.0.0.1:8787", "stream_idle_timeout": "5x"}}`
		_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err == nil || !strings.Contains(err.Error(), "5x") {
			t.Fatalf("ожидали ошибку парсинга длительности: %v", err)
		}
	})

	t.Run("дефолт check_interval при отсутствии", func(t *testing.T) {
		cfg := `{"server": {"listen": "127.0.0.1:8787"}, "socks": {"home": {"url": "socks5h://127.0.0.1:1080"}}}`
		rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := rc.Socks["home"].CheckInterval; got != 60*time.Second {
			t.Fatalf("check_interval: %v, ожидалось 60s", got)
		}
	})
}

// CF-6: в ошибках валидации нет ни пароля из socks-URL, ни api-ключа.
func TestLoadErrorsDoNotLeakSecrets(t *testing.T) {
	t.Run("пароль socks-URL маскируется", func(t *testing.T) {
		cfg := `{"server": {"listen": "127.0.0.1:8787"}, "socks": {"home": {"url": "socks5://user:sup3rs3cret@10.0.0.5:1080"}}}`
		_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err == nil {
			t.Fatal("ожидали ошибку схемы")
		}
		if !strings.Contains(err.Error(), "***") {
			t.Fatalf("в ошибке должен быть маскированный URL: %v", err)
		}
		if strings.Contains(err.Error(), "sup3rs3cret") {
			t.Fatalf("пароль попал в текст ошибки: %v", err)
		}
	})

	t.Run("api-ключ не печатается", func(t *testing.T) {
		cfg := `{
			"server": {"listen": "127.0.0.1:8787"},
			"socks": {"home": {"url": "socks5h://10.0.0.5:1080"}},
			"providers": {"glm": {"type": "glm", "accounts": [
				{"id": "a1", "auth": {"method": "basic", "key": "sk-live-secret-key"}, "socks": "home"}]}}
		}`
		_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err == nil {
			t.Fatal("ожидали ошибку auth.method")
		}
		if strings.Contains(err.Error(), "sk-live-secret-key") {
			t.Fatalf("api-ключ попал в текст ошибки: %v", err)
		}
	})
}

// Строгий парсинг провайдеров: лишние поля чужого типа отпадают на decode.
func TestLoadRejectsForeignProviderFields(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		wantSubstr string
	}{
		{
			// Ф3: base_url/auth_base_url у codex легальны (SPEC 3.1 «у codex —
			// аналогично» glm-правилу base_url, ратифицировано координатором);
			// чужое поле для codex теперь usage_poll из openai-like.
			name:       "codex с usage_poll",
			provider:   `{"type": "codex", "usage_poll": "15m", "accounts": [{"id": "a", "email": "e@x.ru", "auth": {"method": "oauth", "callback_port": 1455}, "socks": "work"}]}`,
			wantSubstr: "usage_poll",
		},
		{
			name:       "glm аккаунт с allow_direct",
			provider:   `{"type": "glm", "accounts": [{"id": "a", "auth": {"method": "api_key", "key": "k"}, "socks": "home", "allow_direct": true}]}`,
			wantSubstr: "allow_direct",
		},
		{
			name:       "codex auth с key",
			provider:   `{"type": "codex", "accounts": [{"id": "a", "auth": {"method": "oauth", "key": "k"}, "socks": "work"}]}`,
			wantSubstr: "key",
		},
		{
			name:       "неизвестный type",
			provider:   `{"type": "anthropic", "accounts": []}`,
			wantSubstr: "неизвестный type",
		},
		{
			name:       "отсутствующий type",
			provider:   `{"accounts": []}`,
			wantSubstr: "type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}, "work": {"url": "socks5h://10.0.0.5:1080"}},
				"providers": {"p": %s}
			}`, tt.provider)
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantSubstr, err)
			}
		})
	}
}

// auth.method проверяется по типу провайдера: codex → oauth, glm → api_key.
func TestLoadValidatesAuthMethodPerProviderType(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantErr  string
	}{
		{"codex с api_key — reject", `{"type": "codex", "accounts": [{"id": "a", "auth": {"method": "api_key"}, "socks": "work"}]}`, "oauth"},
		{"glm с oauth — reject", `{"type": "glm", "accounts": [{"id": "a", "auth": {"method": "oauth"}, "socks": "home"}]}`, "api_key"},
		{"codex с oauth — ok", `{"type": "codex", "accounts": [{"id": "a", "email": "e@x.ru", "auth": {"method": "oauth", "callback_port": 1455}, "socks": "work"}]}`, ""},
		{"glm с api_key — ok", `{"type": "glm", "accounts": [{"id": "a", "auth": {"method": "api_key", "key": "k"}, "socks": "home"}]}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}, "work": {"url": "socks5h://10.0.0.5:1080"}},
				"providers": {"p": %s}
			}`, tt.provider)
			rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ожидали ok, получили: %v", err)
				}
				// Заодно проверяем, что типизированная структура дошла до результата.
				if rc.Providers["p"].Type != ProviderCodex {
					return // glm-кейс
				}
				if got := rc.Providers["p"].Codex.Accounts[0].Auth.CallbackPort; got != 1455 {
					t.Fatalf("callback_port: %d, ожидался 1455", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantErr, err)
			}
		})
	}
}

// Ссылки аккаунтов на socks: имя должно существовать (кроме "none").
func TestLoadValidatesAccountSocksReferences(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantErr  string
	}{
		{"несуществующее имя", `{"type": "glm", "accounts": [{"id": "a", "auth": {"method": "api_key", "key": "k"}, "socks": "ghost"}]}`, "ghost"},
		{"существующее имя — ok", `{"type": "glm", "accounts": [{"id": "a", "auth": {"method": "api_key", "key": "k"}, "socks": "home"}]}`, ""},
		{"codex с none — reject: апстрим внешний", `{"type": "codex", "accounts": [{"id": "a", "auth": {"method": "oauth"}, "socks": "none"}]}`, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
				"providers": {"p": %s}
			}`, tt.provider)
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ожидали ok, получили: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantErr, err)
			}
		})
	}
}

// Имя socks-пути "none" зарезервировано: оно означает прямой путь в аккаунтах.
func TestLoadRejectsReservedSocksNameNone(t *testing.T) {
	cfg := `{"server": {"listen": "127.0.0.1:8787"}, "socks": {"none": {"url": "socks5h://127.0.0.1:1080"}}}`
	_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err == nil || !strings.Contains(err.Error(), "none") {
		t.Fatalf("ожидали ошибку про зарезервированное имя: %v", err)
	}
}

// F4: дубликаты JSON-ключей в секциях-объектах — ошибка, а не молчаливое
// «последний выигрывает».
func TestLoadRejectsDuplicateJSONKeys(t *testing.T) {
	tests := []struct {
		name       string
		cfg        string
		wantSubstr string
	}{
		{
			name: "дубль ключа в aliases",
			cfg: `{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
				"providers": {"glm": {"type": "glm", "accounts": [
					{"id": "a1", "auth": {"method": "api_key", "key": "k"}, "socks": "home"}]}},
				"aliases": {"glm/5.3": {"account": "glm/a1", "model": "glm-5.3"},
					"glm/5.3": {"account": "glm/a1", "model": "glm-5.3-flash"}}
			}`,
			wantSubstr: `duplicate key "glm/5.3" in section "aliases"`,
		},
		{
			name: "дубль ключа в server.clients",
			cfg: `{
				"server": {"listen": "127.0.0.1:8787", "clients": {"opencode": "sk-1", "opencode": "sk-2"}}
			}`,
			wantSubstr: `duplicate key "opencode" in section "clients"`,
		},
		{
			name: "дубль имени провайдера в providers",
			cfg: `{
				"server": {"listen": "127.0.0.1:8787"},
				"providers": {"glm": {"type": "glm", "accounts": []}, "glm": {"type": "glm", "accounts": []}}
			}`,
			wantSubstr: `duplicate key "glm" in section "providers"`,
		},
		{
			name: "дубль имени пути в socks",
			cfg: `{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}, "home": {"url": "socks5h://127.0.0.1:1081"}}
			}`,
			wantSubstr: `duplicate key "home" in section "socks"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", tt.cfg))
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("ожидали ошибку %q, получили: %v", tt.wantSubstr, err)
			}
		})
	}
}

// F5: отрицательные длительности — ошибка валидации (отсутствие/0 → дефолт).
func TestLoadRejectsNegativeDurations(t *testing.T) {
	tests := []struct {
		name       string
		cfg        string
		wantSubstr string
	}{
		{
			name:       "stream_idle_timeout",
			cfg:        `{"server": {"listen": "127.0.0.1:8787", "stream_idle_timeout": "-5s"}}`,
			wantSubstr: "stream_idle_timeout",
		},
		{
			name:       "upstream_connect_timeout",
			cfg:        `{"server": {"listen": "127.0.0.1:8787", "upstream_connect_timeout": "-5s"}}`,
			wantSubstr: "upstream_connect_timeout",
		},
		{
			name:       "shutdown_timeout",
			cfg:        `{"server": {"listen": "127.0.0.1:8787", "shutdown_timeout": "-5s"}}`,
			wantSubstr: "shutdown_timeout",
		},
		{
			name:       "socks check_interval",
			cfg:        `{"server": {"listen": "127.0.0.1:8787"}, "socks": {"home": {"url": "socks5h://127.0.0.1:1080", "check_interval": "-5s"}}}`,
			wantSubstr: "check_interval",
		},
		{
			// N3: отрицательный usage_poll у openai-like — ошибка валидации;
			// 0/отсутствие = выключено, как и раньше.
			name:       "openai-like usage_poll",
			cfg:        `{"server": {"listen": "127.0.0.1:8787"}, "providers": {"o": {"type": "openai-like", "base_url": "http://127.0.0.1:11434", "usage_poll": "-5s", "accounts": [{"id": "a", "auth": {"method": "api_key", "key": "k"}, "socks": "none"}]}}}`,
			wantSubstr: "usage_poll",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, t.TempDir(), "config.json", tt.cfg))
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantSubstr, err)
			}
			if err != nil && !strings.Contains(err.Error(), "-5s") {
				t.Fatalf("в ошибке должно быть значение: %v", err)
			}
		})
	}
}

// F6: пустое имя алиаса — ошибка валидации.
func TestLoadRejectsEmptyAliasName(t *testing.T) {
	cfg := `{
		"server": {"listen": "127.0.0.1:8787"},
		"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
		"providers": {"glm": {"type": "glm", "accounts": [
			{"id": "a1", "auth": {"method": "api_key", "key": "k"}, "socks": "home"}]}},
		"aliases": {"": {"account": "glm/a1", "model": "glm-5.3"}}
	}`
	_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err == nil || !strings.Contains(err.Error(), "пустым") {
		t.Fatalf("ожидали ошибку про пустое имя алиаса: %v", err)
	}
}

// base_url провайдера glm — опциональный ОРИГИН (дефолт https://api.z.ai,
// SPEC 7.2): правило socks:"none" применяется к эффективному origin —
// локальный фейковый эндпоинт разрешает none, дефолтный внешний запрещает.
func TestLoadGLMBaseURLAndNoneRule(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string // "" — без переопределения
		socks      string
		wantErr    string // "" — конфиг валиден
		wantOrigin string // ожидаемый эффективный base_url
	}{
		{
			name:  "дефолт z.ai + socks home — ok",
			socks: "home", wantOrigin: "https://api.z.ai",
		},
		{
			name:    "локальный origin + none — ok (фейковый эндпоинт тестов)",
			baseURL: "http://127.0.0.1:9", socks: "none", wantOrigin: "http://127.0.0.1:9",
		},
		{
			name:    "внешний origin + none — reject (FC-6)",
			baseURL: "https://zai-mirror.example.com", socks: "none",
			wantErr: "none",
		},
		{
			name:    "внешний origin + socks home — ok",
			baseURL: "https://zai-mirror.example.com", socks: "home",
			wantOrigin: "https://zai-mirror.example.com",
		},
		{
			name:    "base_url с чужой схемой — reject",
			baseURL: "ftp://127.0.0.1:9", socks: "home",
			wantErr: "http/https",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := ""
			if tt.baseURL != "" {
				base = `"base_url": "` + tt.baseURL + `",`
			}
			cfg := `{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
				"providers": {"glm": {"type": "glm", ` + base + `"accounts": [
					{"id": "a1", "auth": {"method": "api_key", "key": "k"}, "socks": "` + tt.socks + `"}]}}
			}`
			rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if got := rc.Providers["glm"].GLM.BaseURL; got != tt.wantOrigin {
				t.Fatalf("эффективный base_url = %q, ожидался %q", got, tt.wantOrigin)
			}
		})
	}
}

// Ф3: origin'ы codex — base_url и auth_base_url (SPEC 3.1 «у codex —
// аналогично» glm): дефолты применяются при загрузке, схема строго http/https,
// socks:"none" легален только для локального base_url (FC-6).
func TestLoadCodexOriginsDefaultsAndOverride(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		wantUpstream  string
		wantAuthBase  string
		wantErrSubstr string
	}{
		{
			name:         "без origin'ов — дефолты SPEC 7.1",
			provider:     `"type": "codex", "accounts": [{"id": "a", "auth": {"method": "oauth"}, "socks": "work"}]`,
			wantUpstream: "https://chatgpt.com",
			wantAuthBase: "https://auth.openai.com",
		},
		{
			name:         "локальные стабы — socks none легален",
			provider:     `"type": "codex", "base_url": "http://127.0.0.1:9", "auth_base_url": "http://127.0.0.1:9", "accounts": [{"id": "a", "auth": {"method": "oauth"}, "socks": "none"}]`,
			wantUpstream: "http://127.0.0.1:9",
			wantAuthBase: "http://127.0.0.1:9",
		},
		{
			name:          "внешний апстрим + socks none — reject (FC-6)",
			provider:      `"type": "codex", "base_url": "https://chatgpt.com", "auth_base_url": "http://127.0.0.1:9", "accounts": [{"id": "a", "auth": {"method": "oauth"}, "socks": "none"}]`,
			wantErrSubstr: "socks",
		},
		{
			name:          "auth_base_url с ftp-схемой — reject",
			provider:      `"type": "codex", "auth_base_url": "ftp://127.0.0.1:9", "accounts": [{"id": "a", "auth": {"method": "oauth"}, "socks": "work"}]`,
			wantErrSubstr: "http/https",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := `{
				"server": {"listen": "127.0.0.1:8787"},
				"socks": {"work": {"url": "socks5h://10.0.0.5:1080"}},
				"providers": {"codex": {` + tt.provider + `}}
			}`
			rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
			if tt.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("ожидали ошибку с %q, получили: %v", tt.wantErrSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if got := rc.Providers["codex"].Codex.BaseURL; got != tt.wantUpstream {
				t.Fatalf("base_url = %q, ожидался %q", got, tt.wantUpstream)
			}
			if got := rc.Providers["codex"].Codex.AuthBaseURL; got != tt.wantAuthBase {
				t.Fatalf("auth_base_url = %q, ожидался %q", got, tt.wantAuthBase)
			}
		})
	}
}
