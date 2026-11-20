package zcode

// Тесты Store zcode-tokens.json (SPEC 3.2а): отсутствие файла не ошибка,
// битый JSON — ошибка, атомарная запись 0600 без остатков .tmp, RFC3339
// roundtrip времён, рантайм-статусы, точечный мёрж UpdateChainState.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenStoreMissingFileIsNotError(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "zcode-tokens.json"))
	if err != nil {
		t.Fatalf("OpenStore отсутствующего файла: %v", err)
	}
	if len(s.Keys()) != 0 {
		t.Fatalf("ожидали пустой Store: %v", s.Keys())
	}
}

func TestOpenStoreBrokenJSONIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zcode-tokens.json")
	if err := writeFile(path, []byte("{не json")); err != nil {
		t.Fatalf("запись: %v", err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("битый JSON обязан быть ошибкой (молчаливая потеря доступа недопустима)")
	}
}

func TestStoreSetGetRoundtripRFC3339(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zcode-tokens.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	zaiExp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	bizExp := time.Now().Add(time.Hour).Truncate(time.Second)
	rec := TokenRec{
		ZaiAccessToken:    "zai-1",
		ZcodeJWT:          "zjwt-1",
		UserID:            "u1",
		Email:             "a@b.ru",
		BusinessJWT:       "biz-1",
		BusinessExpiresAt: bizExp,
		ZaiExpiresAt:      &zaiExp,
		Org:               "org-1",
		Project:           "prj-1",
		APIKeyID:          "key-1",
		DeviceMid:         "dev-1",
	}
	if err := s.Set(AccountKey("main"), rec); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Атомарность: tmp переименован (не остался), права 0600.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp не переименован: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("права файла %v, ожидались 0600", perm)
	}

	// RFC3339 в файле (SPEC 3.2а: business_expires_at/zai_expires_at — RFC3339).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, frag := range []string{`"business_expires_at"`, `"zai_expires_at"`, `"zai_access_token"`, `"device_mid"`} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("в файле нет %s:\n%s", frag, raw)
		}
	}

	// Читается свежим Store (reopen) с точными временами.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := s2.Get(AccountKey("main"))
	if !ok {
		t.Fatal("запись потерялась при reopen")
	}
	if got.ZaiAccessToken != "zai-1" || got.ZcodeJWT != "zjwt-1" || got.UserID != "u1" ||
		got.Email != "a@b.ru" || got.Org != "org-1" || got.Project != "prj-1" ||
		got.APIKeyID != "key-1" || got.DeviceMid != "dev-1" {
		t.Fatalf("roundtrip записи не совпал: %+v", got)
	}
	if !got.BusinessExpiresAt.Equal(bizExp) || got.ZaiExpiresAt == nil || !got.ZaiExpiresAt.Equal(zaiExp) {
		t.Fatalf("времена roundtrip: biz=%v (ожидалось %v), zai=%v", got.BusinessExpiresAt, bizExp, got.ZaiExpiresAt)
	}

	// device_mid повторного чтения — тот же (переиспользование флоу).
	if again, _ := s2.Get(AccountKey("main")); again.DeviceMid != "dev-1" {
		t.Fatalf("device_mid не стабилен: %q", again.DeviceMid)
	}
}

func TestStoreSetDoesNotClobberOtherKeys(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "zcode-tokens.json"))
	if err := s.Set(AccountKey("a"), TokenRec{ZaiAccessToken: "a1", UserID: "ua"}); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := s.Set(AccountKey("b"), TokenRec{ZaiAccessToken: "b1", UserID: "ub"}); err != nil {
		t.Fatalf("Set b: %v", err)
	}
	if rec, ok := s.Get(AccountKey("a")); !ok || rec.ZaiAccessToken != "a1" {
		t.Fatalf("OAuth аккаунта b затёр запись a: %+v", rec)
	}
}

func TestStoreStatusLifecycle(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "zcode-tokens.json"))
	key := AccountKey("main")

	// Нет записи — no_token.
	if st := s.Status(key); st.State != StatusNoToken {
		t.Fatalf("статус без записи = %s, ожидался %s", st.State, StatusNoToken)
	}
	// Запись с пустым zai-токеном — тоже no_token.
	if err := s.Set(key, TokenRec{UserID: "u"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if st := s.Status(key); st.State != StatusNoToken {
		t.Fatalf("статус пустой записи = %s, ожидался %s", st.State, StatusNoToken)
	}
	// Живые токены — ok.
	if err := s.Set(key, TokenRec{ZaiAccessToken: "zai-1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if st := s.Status(key); st.State != StatusOK {
		t.Fatalf("статус после Set = %s, ожидался %s", st.State, StatusOK)
	}
	// needs_relogin ставится цепочкой (setStatus — внутренний механизм).
	s.setStatus(key, StatusNeedsRelogin, "бизнес-код 500: Z.ai user information is invalid")
	if st := s.Status(key); st.State != StatusNeedsRelogin || st.Err == "" {
		t.Fatalf("статус после needs_relogin = %+v", st)
	}
	// Успешный OAuth (Set) сбрасывает статус (SPEC 7.4).
	if err := s.Set(key, TokenRec{ZaiAccessToken: "zai-2"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if st := s.Status(key); st.State != StatusOK {
		t.Fatalf("needs_relogin не сброшен успешным OAuth: %s", st.State)
	}
}

func TestStoreUpdateChainStateMergesNotClobbers(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "zcode-tokens.json"))
	key := AccountKey("main")
	if err := s.Set(key, TokenRec{ZaiAccessToken: "zai-fresh", ZcodeJWT: "zjwt", UserID: "u"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	bizExp := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := s.UpdateChainState(key, func(r *TokenRec) {
		r.BusinessJWT, r.BusinessExpiresAt = "biz-1", bizExp
	}); err != nil {
		t.Fatalf("UpdateChainState: %v", err)
	}
	rec, ok := s.Get(key)
	if !ok {
		t.Fatal("запись исчезла")
	}
	// Свежие OAuth-токены не затёрты (суть точечного мёржа).
	if rec.ZaiAccessToken != "zai-fresh" || rec.ZcodeJWT != "zjwt" {
		t.Fatalf("мёрж затёр OAuth-токены: %+v", rec)
	}
	if rec.BusinessJWT != "biz-1" || !rec.BusinessExpiresAt.Equal(bizExp) {
		t.Fatalf("business-поля не записаны: %+v", rec)
	}

	// Несуществующий ключ — ошибка (не молчаливая запись).
	if err := s.UpdateChainState(AccountKey("ghost"), func(*TokenRec) {}); err == nil {
		t.Fatal("UpdateChainState отсутствующей записи обязан быть ошибкой")
	}
}

func TestStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zcode-tokens.json")
	s, _ := OpenStore(path)
	if err := s.Set(AccountKey("a"), TokenRec{ZaiAccessToken: "z"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete(AccountKey("a")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(AccountKey("a")); ok {
		t.Fatal("запись не удалена")
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := s2.Get(AccountKey("a")); ok {
		t.Fatal("удаление не доехало до файла")
	}
}
