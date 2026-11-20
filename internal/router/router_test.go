package router

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"llm-proxy/internal/config"
)

// loadSnapshot грузит конфиг из временного файла: Resolve работает по снапшоту
// (валидация ссылочной целостности уже сделана при загрузке).
func loadSnapshot(t *testing.T, jsonCfg string) *config.Snapshot {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonCfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rc, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Socks-пути роутеру не нужны (про сеть он не знает) — только Cfg.
	return &config.Snapshot{Cfg: rc}
}

const testCfg = `{
  "server": {"listen": "127.0.0.1:0", "clients": {"opencode": "sk-1"}},
  "socks": {"home": {"url": "socks5h://127.0.0.1:1080"}},
  "providers": {
    "ollama": {
      "type": "openai-like",
      "base_url": "http://127.0.0.1:11434",
      "accounts": [
        {"id": "local", "auth": {"method": "api_key", "key": "k1"}, "socks": "none"},
        {"id": "cloud", "auth": {"method": "api_key", "key": "k2"}, "socks": "home"}
      ]
    },
    "glm": {
      "type": "glm",
      "accounts": [{"id": "plan-a", "auth": {"method": "api_key", "key": "k3"}, "socks": "home"}]
    }
  },
  "aliases": {
    "qwen/local": {"account": "ollama/local", "model": "qwen3-coder:480b"},
    "qwen/cloud": {"account": "ollama/cloud", "model": "qwen3-coder:480b"},
    "glm/5.3":    {"account": "glm/plan-a", "model": "glm-5.3"}
  }
}`

func TestResolveOpenAILike(t *testing.T) {
	snap := loadSnapshot(t, testCfg)
	got, err := Resolve(snap, "qwen/cloud")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Target{
		ProviderName: "ollama", ProviderType: "openai-like",
		AccountID: "cloud", Model: "qwen3-coder:480b", SocksName: "home",
	}
	if got != want {
		t.Fatalf("Target = %+v, ожидалось %+v", got, want)
	}
}

func TestResolveSocksNone(t *testing.T) {
	// socks:"none" — легальное имя пути (сервер возьмёт socks.Direct).
	snap := loadSnapshot(t, testCfg)
	got, err := Resolve(snap, "qwen/local")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SocksName != "none" || got.ProviderType != "openai-like" {
		t.Fatalf("Target = %+v", got)
	}
}

func TestResolveGLM(t *testing.T) {
	snap := loadSnapshot(t, testCfg)
	got, err := Resolve(snap, "glm/5.3")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ProviderName != "glm" || got.ProviderType != "glm" || got.AccountID != "plan-a" || got.Model != "glm-5.3" || got.SocksName != "home" {
		t.Fatalf("Target = %+v", got)
	}
}

func TestResolveUnknownAliasTyped(t *testing.T) {
	snap := loadSnapshot(t, testCfg)
	_, err := Resolve(snap, "no/such-alias")
	if err == nil {
		t.Fatal("ожидали ошибку для неизвестного алиаса")
	}
	// Типизированная ошибка: сервер различает 404 и внутренние сбои.
	if !errors.Is(err, ErrUnknownAlias) {
		t.Fatalf("err = %v, ожидался ErrUnknownAlias", err)
	}
}

func TestAvailableAliasesSorted(t *testing.T) {
	snap := loadSnapshot(t, testCfg)
	got := AvailableAliases(snap)
	want := []string{"glm/5.3", "qwen/cloud", "qwen/local"}
	if len(got) != len(want) {
		t.Fatalf("алиасы = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("алиасы = %v, ожидалось %v (порядок должен быть отсортированным)", got, want)
		}
	}
}
