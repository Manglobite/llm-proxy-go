package config

// Тесты провайдера zcode в конфиге (SPEC 3.1/7.4): дефолты origin'ов/версии/
// маршрута, enum route, явная пустая client_version, FC-6 для ДВУХ origin'ов,
// auth.method "oauth", дефолт zcode_tokens_path, ссылочная целостность алиасов.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func zcodeCfg(base, zbase, extra, socks string) string {
	if base != "" {
		base = `"base_url": "` + base + `",`
	}
	if zbase != "" {
		zbase = `"zcode_base_url": "` + zbase + `",`
	}
	return `{
		"server": {"listen": "127.0.0.1:8787"},
		"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
		"providers": {"zai-plan": {"type": "zcode", ` + base + zbase + extra + `
			"accounts": [{"id": "a1", "auth": {"method": "oauth"}, "socks": "` + socks + `"}]}}
	}`
}

// Дефолты SPEC 3.1/7.4: оба origin'а, client_version 3.17.2, route pat.
func TestLoadZCodeDefaults(t *testing.T) {
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg("", "", `"route": "pat",`, "home")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	zp := rc.Providers["zai-plan"].ZCode
	if zp == nil {
		t.Fatal("zcode-провайдер не распарсен")
	}
	if zp.BaseURL != "https://api.z.ai" {
		t.Fatalf("base_url = %q, ожидался дефолт https://api.z.ai", zp.BaseURL)
	}
	if zp.ZCodeBaseURL != "https://zcode.z.ai" {
		t.Fatalf("zcode_base_url = %q, ожидался дефолт https://zcode.z.ai", zp.ZCodeBaseURL)
	}
	if zp.ClientVersion != "3.17.2" {
		t.Fatalf("client_version = %q, ожидался дефолт 3.17.2", zp.ClientVersion)
	}
	if zp.Route != ZCodeRoutePAT {
		t.Fatalf("route = %q, ожидался дефолт pat", zp.Route)
	}
	if zp.Accounts[0].Auth.Method != "oauth" {
		t.Fatalf("auth.method = %q", zp.Accounts[0].Auth.Method)
	}
	// Дефолт storage.zcode_tokens_path — рядом с конфигом (SPEC 3.1).
	if !strings.HasSuffix(rc.Storage.ZcodeTokensPath, "zcode-tokens.json") {
		t.Fatalf("zcode_tokens_path = %q, ожидался суффикс zcode-tokens.json", rc.Storage.ZcodeTokensPath)
	}
}

// Явные значения прокидываются; пользовательский путь storage резолвится рядом
// с config.json.
func TestLoadZCodeExplicitValues(t *testing.T) {
	cfg := `{
		"server": {"listen": "127.0.0.1:8787"},
		"storage": {"zcode_tokens_path": "tokens/zcode.json"},
		"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
		"providers": {"zai-plan": {"type": "zcode",
			"base_url": "https://api.example.com",
			"zcode_base_url": "https://zcode.example.com",
			"client_version": "9.9.9",
			"route": "zcode_plan",
			"accounts": [{"id": "a1", "email": "u@z.ai", "auth": {"method": "oauth"}, "socks": "home"}]}}
	}`
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	zp := rc.Providers["zai-plan"].ZCode
	if zp.BaseURL != "https://api.example.com" || zp.ZCodeBaseURL != "https://zcode.example.com" ||
		zp.ClientVersion != "9.9.9" || zp.Route != ZCodeRouteZcodePlan {
		t.Fatalf("явные значения не применились: %+v", zp)
	}
	if !strings.HasSuffix(rc.Storage.ZcodeTokensPath, filepath.Join("tokens", "zcode.json")) {
		t.Fatalf("zcode_tokens_path = %q, ожидался относительный путь рядом с config.json", rc.Storage.ZcodeTokensPath)
	}
}

// route — enum pat|zcode_plan (SPEC 7.4), чужое значение — ошибка конфига.
func TestLoadZCodeRouteEnum(t *testing.T) {
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg("", "", `"route": "grpc",`, "home")))
	if err == nil || !strings.Contains(err.Error(), "route") || !strings.Contains(err.Error(), "grpc") {
		t.Fatalf("ожидали ошибку enum route, получили: %v", err)
	}
	_ = rc
}

// client_version: отсутствие → дефолт; явная пустая строка — ошибка
// (SPEC 7.4 «client_version непустой»).
func TestLoadZCodeClientVersionEmptyIsError(t *testing.T) {
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg("", "", `"client_version": "",`, "home")))
	if err == nil || !strings.Contains(err.Error(), "client_version") {
		t.Fatalf("пустая client_version обязана быть ошибкой, получили: %v", err)
	}
	_ = rc
}

// FC-6 для двух origin'ов (SPEC 3.1): socks:"none" легален только когда ОБА
// origin'а локальные; один локальный + один внешний — reject.
func TestLoadZCodeSocksNoneRequiresBothOriginsLocal(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		zbase   string
		socks   string
		wantErr string
	}{
		{
			name: "оба локальные — ok",
			base: "http://127.0.0.1:9", zbase: "http://127.0.0.1:9", socks: "none",
		},
		{
			name: "localhost-имя — ok",
			base: "http://localhost:9", zbase: "http://localhost:9", socks: "none",
		},
		{
			name: "base внешний — reject",
			base: "https://api.example.com", zbase: "http://127.0.0.1:9", socks: "none",
			wantErr: "none",
		},
		{
			name: "zcode_base внешний — reject",
			base: "http://127.0.0.1:9", zbase: "https://zcode.example.com", socks: "none",
			wantErr: "zcode_base_url",
		},
		{
			name:  "оба дефолтные (внешние) + socks home — ok",
			socks: "home",
		},
		{
			name: "evil-localhost.com — внешний",
			base: "https://evil-localhost.com", zbase: "http://127.0.0.1:9", socks: "none",
			wantErr: "none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, err := Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg(tt.base, tt.zbase, "", tt.socks)))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ожидали reject с %q, получили: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			_ = rc
		})
	}
}

// auth.method строго "oauth" (glm-style api_key у zcode не существует);
// поле key в OAuthAuth не существует — строгий парсинг его отвергает.
func TestLoadZCodeAuthMethod(t *testing.T) {
	t.Run("method api_key — reject", func(t *testing.T) {
		cfg := `{
			"server": {"listen": "127.0.0.1:8787"},
			"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
			"providers": {"zai-plan": {"type": "zcode",
				"accounts": [{"id": "a1", "auth": {"method": "api_key"}, "socks": "home"}]}}
		}`
		_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err == nil || !strings.Contains(err.Error(), "oauth") {
			t.Fatalf("ожидали ошибку auth.method (ожидался oauth), получили: %v", err)
		}
	})
	t.Run("auth с key — reject строгим парсингом", func(t *testing.T) {
		cfg := `{
			"server": {"listen": "127.0.0.1:8787"},
			"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
			"providers": {"zai-plan": {"type": "zcode",
				"accounts": [{"id": "a1", "auth": {"method": "oauth", "key": "sk-secret"}, "socks": "home"}]}}
		}`
		_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
		if err == nil || !strings.Contains(err.Error(), "key") {
			t.Fatalf("поле key в zcode-auth отсутствует — строгий парсинг обязан отвергнуть: %v", err)
		}
	})
}

// Ссылка алиаса "provider/account" резолвится по zcode-аккаунтам (CF-2).
func TestLoadZCodeAliasReference(t *testing.T) {
	cfg := `{
		"server": {"listen": "127.0.0.1:8787"},
		"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
		"providers": {"zai-plan": {"type": "zcode",
			"accounts": [{"id": "main", "auth": {"method": "oauth"}, "socks": "home"}]}},
		"aliases": {"zai/5.3": {"account": "zai-plan/main", "model": "glm-5.3"}}
	}`
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !providerHasAccount(rc.Providers["zai-plan"], "main") {
		t.Fatal("providerHasAccount не видит zcode-аккаунт")
	}
	// И чужой аккаунт не признаёт.
	if providerHasAccount(rc.Providers["zai-plan"], "ghost") {
		t.Fatal("providerHasAccount признал несуществующий аккаунт")
	}
}

// plan_credits переносится (экран квот, SPEC 7.4/7.2).
func TestLoadZCodePlanCredits(t *testing.T) {
	cfg := `{
		"server": {"listen": "127.0.0.1:8787"},
		"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
		"providers": {"zai-plan": {"type": "zcode",
			"accounts": [{"id": "a1", "auth": {"method": "oauth"}, "socks": "home",
				"plan_credits": {"window_5h": 12000, "window_weekly": 60000}}]}}
	}`
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pc := rc.Providers["zai-plan"].ZCode.Accounts[0].PlanCredits
	if pc == nil || pc.Window5h != 12000 || pc.WindowWeekly != 60000 {
		t.Fatalf("plan_credits = %+v", pc)
	}
}

// Чужие поля чужого типа в zcode-провайдере отклоняются строгим парсингом
// (forward_paths — поле openai-like; usage_poll с Ф8 — легальное поле zcode).
func TestLoadZCodeRejectsForeignFields(t *testing.T) {
	cfg := `{
		"server": {"listen": "127.0.0.1:8787"},
		"socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
		"providers": {"zai-plan": {"type": "zcode",
			"forward_paths": ["/api/tags"],
			"accounts": [{"id": "a1", "auth": {"method": "oauth"}, "socks": "home"}]}}
	}`
	_, err := Load(writeConfig(t, t.TempDir(), "config.json", cfg))
	if err == nil || !strings.Contains(err.Error(), "forward_paths") {
		t.Fatalf("ожидали reject неизвестного поля, получили: %v", err)
	}
}

// usage_poll (Ф8, SPEC 15): значение прокидывается, отсутствие — 0 (выключен),
// отрицательный — ошибка валидации (как у openai-like, SPEC 7.3).
func TestLoadZCodeUsagePoll(t *testing.T) {
	rc, err := Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg("", "", `"usage_poll": "15m",`, "home")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := time.Duration(rc.Providers["zai-plan"].ZCode.UsagePoll); got != 15*time.Minute {
		t.Fatalf("usage_poll = %v, ожидалось 15m", got)
	}
	rc, err = Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg("", "", "", "home")))
	if err != nil {
		t.Fatalf("Load без usage_poll: %v", err)
	}
	if got := time.Duration(rc.Providers["zai-plan"].ZCode.UsagePoll); got != 0 {
		t.Fatalf("usage_poll без поля = %v, ожидался 0 (выключен)", got)
	}
	_, err = Load(writeConfig(t, t.TempDir(), "config.json", zcodeCfg("", "", `"usage_poll": "-5s",`, "home")))
	if err == nil || !strings.Contains(err.Error(), "usage_poll") {
		t.Fatalf("отрицательный usage_poll обязан быть ошибкой, получили: %v", err)
	}
}
