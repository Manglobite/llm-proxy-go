package config

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// managerConfig — валидный конфиг с одним socks-путём; check_interval большой,
// чтобы фоновый тикер не делал сетевых вызовов во время теста. Все параметры
// проверки параметризованы: на них строится таблица условий переиспользования (F2).
func managerConfig(socksURL, expectedIP, checkURL, checkInterval string) string {
	return fmt.Sprintf(`{
		"server": {"listen": "127.0.0.1:8787"},
		"socks": {"home": {"url": %q, "expected_ip": %q, "check_url": %q, "check_interval": %q}},
		"providers": {"glm": {"type": "glm", "accounts": [
			{"id": "a1", "auth": {"method": "api_key", "key": "sk-test"}, "socks": "home"}]}},
		"aliases": {"glm/5.3": {"account": "glm/a1", "model": "glm-5.3"}}
	}`, socksURL, expectedIP, checkURL, checkInterval)
}

// Значения по умолчанию для managerConfig в тестах релоада.
const (
	mgrURL   = "socks5h://127.0.0.1:1080"
	mgrIP    = "203.0.113.7"
	mgrCURL  = "http://127.0.0.1:1/check"
	mgrIVL   = "1h"
	mgrModel = "glm-5.3"
)

// CF-1: релоад с невалидным конфигом возвращает ошибку и не трогает живой снапшот.
func TestReloadWithInvalidConfigKeepsOldSnapshot(t *testing.T) {
	dir := t.TempDir()
	good := writeConfig(t, dir, "config.json", managerConfig(mgrURL, mgrIP, mgrCURL, mgrIVL))
	mgr, err := NewManager(context.Background(), good)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Stop()

	old := mgr.Snapshot()
	bad := writeConfig(t, dir, "bad.json", `{"socks": {"home": {"url": "http://wrong-scheme"}}}`)
	if err := mgr.Reload(bad); err == nil {
		t.Fatal("ожидали ошибку валидации нового конфига")
	}
	if mgr.Snapshot() != old {
		t.Fatal("ошибка релоада не должна заменять живой снапшот (CF-1)")
	}
	// Путь из старого снапшота остаётся рабочим объектом.
	if st := old.Socks["home"].Status(); st.Name != "home" {
		t.Fatalf("путь из старого снапшота недоступен: %+v", st)
	}
	// Старый снапшот не отменён: фоновая жизнь продолжается.
	if old.Ctx().Err() != nil {
		t.Fatal("после ошибки релоада старый снапшот не должен отменяться")
	}
}

// CF-4: путь с неизменными url+expected_ip+check_url+check_interval
// переиспользуется со своим state, даже если остальной конфиг изменился.
func TestReloadReusesPathWithUnchangedSocksParams(t *testing.T) {
	dir := t.TempDir()
	v1 := writeConfig(t, dir, "v1.json", managerConfig(mgrURL, mgrIP, mgrCURL, mgrIVL))
	mgr, err := NewManager(context.Background(), v1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Stop()
	old := mgr.Snapshot()

	// socks-параметры те же, но конфиг реально меняется (модель алиаса).
	v2json := strings.Replace(managerConfig(mgrURL, mgrIP, mgrCURL, mgrIVL), mgrModel, "glm-5.3-flash", 1)
	v2 := writeConfig(t, dir, "v2.json", v2json)
	if err := mgr.Reload(v2); err != nil {
		t.Fatalf("релоад: %v", err)
	}
	fresh := mgr.Snapshot()
	if fresh.Socks["home"] != old.Socks["home"] {
		t.Fatal("путь с неизменными параметрами проверки должен переиспользоваться (CF-4)")
	}
	if got := fresh.Cfg.Aliases["glm/5.3"].Model; got != "glm-5.3-flash" {
		t.Fatalf("новый конфиг должен примениться: model=%q", got)
	}
	if old.Ctx().Err() == nil {
		t.Fatal("контекст старого снапшота должен быть отменён после релоада")
	}
	if fresh.Ctx().Err() != nil {
		t.Fatal("контекст нового снапшота должен быть жив")
	}
}

// F2 (ARCHITECTURE §7.5): изменение ЛЮБОГО параметра проверки — url,
// expected_ip, check_url или check_interval — пересоздаёт *socks.Path.
func TestReloadRebuildsPathWhenAnyCheckParamChanged(t *testing.T) {
	tests := []struct {
		name string
		url  string
		ip   string
		curl string
		ivl  string
	}{
		{"изменился url", "socks5h://127.0.0.1:1081", mgrIP, mgrCURL, mgrIVL},
		{"изменился expected_ip", mgrURL, "203.0.113.8", mgrCURL, mgrIVL},
		{"изменился check_url", mgrURL, mgrIP, "http://127.0.0.1:2/check", mgrIVL},
		{"изменился check_interval", mgrURL, mgrIP, mgrCURL, "2h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			v1 := writeConfig(t, dir, "v1.json", managerConfig(mgrURL, mgrIP, mgrCURL, mgrIVL))
			mgr, err := NewManager(context.Background(), v1)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			defer mgr.Stop()
			old := mgr.Snapshot()

			v2 := writeConfig(t, dir, "v2.json", managerConfig(tt.url, tt.ip, tt.curl, tt.ivl))
			if err := mgr.Reload(v2); err != nil {
				t.Fatalf("релоад: %v", err)
			}
			if mgr.Snapshot().Socks["home"] == old.Socks["home"] {
				t.Fatal("изменившийся параметр проверки должен пересоздать путь (F2)")
			}
			if old.Ctx().Err() == nil {
				t.Fatal("контекст старого снапшота должен быть отменён после релоада")
			}
		})
	}
}
