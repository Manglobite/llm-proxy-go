package zcode

// Тесты планировщика PAT/бизнес-JWT (SPEC 7.4): пере-issue по мере истечения
// (короткие TTL — RunOnce зовётся напрямую, без ожидалок), пропуски
// needs_relogin/отсутствующих deps, ложных срабатываний быть не должно.

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// seed — запись в Store с живым zai-токеном и кэшированной локацией (чтобы
// счётчики цепочки были точными: только login/access_tokens).
func seed(t *testing.T, store *Store, key string, bizTTL, patTTL time.Duration) *PATCache {
	t.Helper()
	rec := freshRec()
	rec.Org, rec.Project, rec.APIKeyID = "org-1", "prj-1", "key-1"
	rec.BusinessJWT = fakeJWT(t, `{"typ":"biz"}`)
	rec.BusinessExpiresAt = time.Now().Add(bizTTL)
	if err := store.Set(key, rec); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)
	if patTTL > 0 {
		// Предзаполняем кеш PAT с заданным TTL (как после успешной цепочки).
		e := chain.entry(key)
		e.pat, e.patExp = "pat-old", time.Now().Add(patTTL)
	}
	return chain
}

// TestRefresherReissuesExpiringPAT — PAT истекает <120s → EnsurePAT
// (пере-issue), свежий PAT и бизнес-JWT не трогаются.
func TestRefresherReissuesExpiringPAT(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	chain := seed(t, store, key, time.Hour /* biz жив */, 60*time.Second /* PAT истекает */)

	r := NewRefresher(store, chain, refresherDeps(st), DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())

	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("access_tokens = %d, ожидался 1 (пере-issue PAT)", got)
	}
	if got := st.count("POST", "/api/auth/z/login"); got != 0 {
		t.Fatalf("живой бизнес-JWT не должен ре-логиниться (login = %d)", got)
	}
	if _, exp, ok := chain.PATInfo(key); !ok || !exp.After(time.Now().Add(patMinLife)) {
		t.Fatalf("PAT не обновлён: exp=%v ok=%v", exp, ok)
	}
}

// TestRefresherReloginsExpiringBusinessJWT — бизнес-JWT истекает <120s →
// цепочка: login 1 + access_tokens 1.
func TestRefresherReloginsExpiringBusinessJWT(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	chain := seed(t, store, key, 60*time.Second /* biz истекает */, time.Hour /* PAT жив */)

	r := NewRefresher(store, chain, refresherDeps(st), DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())

	if got := st.count("POST", "/api/auth/z/login"); got != 1 {
		t.Fatalf("login = %d, ожидался 1 (ре-логин бизнес-JWT)", got)
	}
	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("access_tokens = %d, ожидался 1 (PAT пере-issue той же цепочкой)", got)
	}
}

// TestRefresherSkipsFresh — всё свежее → ноль запросов.
func TestRefresherSkipsFresh(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	chain := seed(t, store, key, time.Hour, time.Hour)

	r := NewRefresher(store, chain, refresherDeps(st), DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())

	if got := st.count("POST", "/access_tokens") + st.count("POST", "/api/auth/z/login"); got != 0 {
		t.Fatalf("свежие токены не должны обновляться (запросов = %d)", got)
	}
}

// TestRefresherIssuesMissingPAT — PAT в кеше нет (рестарт/fatal-очистка) →
// EnsurePAT по тикеру (SPEC 7.4: «ретрай тикером» после fatal).
func TestRefresherIssuesMissingPAT(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	chain := seed(t, store, key, time.Hour, 0 /* PAT отсутствует */)

	r := NewRefresher(store, chain, refresherDeps(st), DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())

	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("access_tokens = %d, ожидался 1", got)
	}
}

// TestRefresherSkipsNeedsRelogin — needs_relogin-аккаунт не долбится тикером
// (интерактивный OAuth заново — SPEC 7.4).
func TestRefresherSkipsNeedsRelogin(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	chain := seed(t, store, key, 30*time.Second, 30*time.Second)
	store.setStatus(key, StatusNeedsRelogin, "бизнес-код 500")

	depsCalls := 0
	deps := func(string) (AcctDeps, error) {
		depsCalls++
		return refresherDeps(st)(key)
	}
	r := NewRefresher(store, chain, deps, DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())

	if depsCalls != 0 {
		t.Fatalf("deps не должны зваться для needs_relogin (звались %d раз)", depsCalls)
	}
	if got := st.count("POST", "/access_tokens"); got != 0 {
		t.Fatalf("needs_relogin аккаунт не должен обновляться (access_tokens = %d)", got)
	}
	// Статус не сброшен.
	if stt := store.Status(key); stt.State != StatusNeedsRelogin {
		t.Fatalf("статус изменился: %s", stt.State)
	}
}

// TestRefresherSkipsNoOAuth — записи без zai-токена (OAuth не выполнялся)
// пропускаются.
func TestRefresherSkipsNoOAuth(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	if err := store.Set(AccountKey("empty"), TokenRec{UserID: "u"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	r := NewRefresher(store, NewPATCache(store), refresherDeps(st), DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())
	if got := st.count("POST", "/api/auth/z/login"); got != 0 {
		t.Fatalf("без zai-токена цепочка не должна запускаться (login = %d)", got)
	}
}

// TestRefresherSkipsAccountWithoutDeps — аккаунт исчез из конфига: ключ в Store
// остаётся (CF-7), warn, без паники.
func TestRefresherSkipsAccountWithoutDeps(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("gone")
	seed(t, store, key, 30*time.Second, 30*time.Second)

	deps := func(accountKey string) (AcctDeps, error) {
		if accountKey == key {
			return AcctDeps{}, errAccountGone
		}
		return refresherDeps(st)(accountKey)
	}
	r := NewRefresher(store, NewPATCache(store), deps, DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background()) // не паникует

	if got := st.count("POST", "/access_tokens"); got != 0 {
		t.Fatalf("недоступный аккаунт не должен обновляться (access_tokens = %d)", got)
	}
	if _, ok := store.Get(key); !ok {
		t.Fatal("ключ исчезнувшего аккаунта удалён молча (CF-7)")
	}
}

// TestRefresherTransientDoesNotKillScheduler — провал цепочки не валит
// планировщик: следующий RunOnce повторяет попытку.
func TestRefresherTransientDoesNotKillScheduler(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	chain := seed(t, store, key, time.Hour, 60*time.Second)

	// Первый прогон: access_tokens отдаёт 500 (transient).
	st.onAccessTokens = func(w http.ResponseWriter, r *hitReq) {
		w.WriteHeader(500)
	}
	r := NewRefresher(store, chain, refresherDeps(st), DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())
	if stt := store.Status(key); stt.State == StatusNeedsRelogin {
		t.Fatalf("transient не должен ставить needs_relogin: %+v", stt)
	}

	// Второй прогон: стаб отвечает хорошо — цепочка проходит.
	st.onAccessTokens = nil
	r.RunOnce(context.Background())
	if got := st.count("POST", "/access_tokens"); got != 2 {
		t.Fatalf("access_tokens = %d, ожидалось 2 (ретрай следующим тиком)", got)
	}
	if _, exp, ok := chain.PATInfo(key); !ok || !exp.After(time.Now().Add(patMinLife)) {
		t.Fatalf("PAT не обновлён ретраем: exp=%v ok=%v", exp, ok)
	}
}

// refresherDeps — deps-замыкание на стаб.
func refresherDeps(st *zStub) Deps {
	return func(accountKey string) (AcctDeps, error) {
		return AcctDeps{
			AccountKey:    accountKey,
			Client:        directClient(),
			Base:          st.srv.URL,
			ZcodeBase:     st.srv.URL,
			ClientVersion: "3.17.2",
		}, nil
	}
}

var errAccountGone = &accountGoneError{}

type accountGoneError struct{}

func (*accountGoneError) Error() string { return "аккаунт исчез из конфига" }
