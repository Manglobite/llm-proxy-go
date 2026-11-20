package codex

// Тесты планировщика refresh (OA-4): refresh за 5 минут до expires_at,
// без ожидалок (RunOnce зовётся напрямую, CODING 1.9), провал не валит
// планировщик, ротация refresh-токена пишется в Store.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// refresherStub — счётчик refresh-вызовов с управляемым ответом.
type refresherStub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	hits     int
	failures int // сколько следующих вызовов ответить 500
}

func newRefresherStub(t *testing.T) *refresherStub {
	st := &refresherStub{}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.hits++
		fail := st.failures > 0
		if fail {
			st.failures--
		}
		st.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"server down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tokenJSON(t, "at-fresh", "rt-fresh", fakeJWT(t, claimsPayload), 3600))
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func (st *refresherStub) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hits
}

func (st *refresherStub) failNext() {
	st.mu.Lock()
	st.failures = 1
	st.mu.Unlock()
}

func TestRefresherRunOnceRefreshesOnlyExpiring(t *testing.T) {
	st := newRefresherStub(t)
	store, _ := OpenStore(t.TempDir() + "/tokens.json")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("store.Set: %v", err)
		}
	}
	// near: до истечения 4 минуты (< 5m) — refresh обязателен.
	must(store.Set("codex/near", TokenSet{AccessToken: "at-old", RefreshToken: "rt-near", ExpiresAt: time.Now().Add(4 * time.Minute)}))
	// far: до истечения час — refresh не нужен.
	must(store.Set("codex/far", TokenSet{AccessToken: "at-far", RefreshToken: "rt-far", ExpiresAt: time.Now().Add(time.Hour)}))

	deps := func(key string) (AcctDeps, error) {
		return AcctDeps{Client: directClient(), AuthBase: st.srv.URL}, nil
	}
	r := NewRefresher(store, deps, DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background())

	if got := st.count(); got != 1 {
		t.Fatalf("refresh-вызовов = %d, ожидался 1 (только near)", got)
	}
	// near обновлён с ротацией; far не тронут.
	if ts, _ := store.Get("codex/near"); ts.AccessToken != "at-fresh" || ts.RefreshToken != "rt-fresh" {
		t.Fatalf("near не обновлён: %+v", ts)
	}
	if ts, _ := store.Get("codex/far"); ts.AccessToken != "at-far" {
		t.Fatalf("far перезаписан: %+v", ts)
	}
}

func TestRefresherFailureDoesNotKillScheduler(t *testing.T) {
	st := newRefresherStub(t)
	store, _ := OpenStore(t.TempDir() + "/tokens.json")
	if err := store.Set("codex/near", TokenSet{AccessToken: "at-old", RefreshToken: "rt-near", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	deps := func(string) (AcctDeps, error) {
		return AcctDeps{Client: directClient(), AuthBase: st.srv.URL}, nil
	}
	r := NewRefresher(store, deps, DefaultRefreshTick, DefaultRefreshBefore)

	st.failNext()
	r.RunOnce(context.Background()) // провал — только Warn
	if ts, _ := store.Get("codex/near"); ts.AccessToken != "at-old" {
		t.Fatalf("токен перезаписан при провале refresh: %+v", ts)
	}
	// Планировщик жив: следующий тик повторяет попытку и успешно обновляет.
	r.RunOnce(context.Background())
	if ts, _ := store.Get("codex/near"); ts.AccessToken != "at-fresh" {
		t.Fatalf("повторная попытка не выполнена: %+v", ts)
	}
	if got := st.count(); got != 2 {
		t.Fatalf("refresh-вызовов = %d, ожидалось 2", got)
	}
}

func TestRefresherSkipsAccountsWithoutDeps(t *testing.T) {
	st := newRefresherStub(t)
	store, _ := OpenStore(t.TempDir() + "/tokens.json")
	// Ключ остался в Store, аккаунт исчез из конфига (CF-7 — не удалять молча).
	if err := store.Set("codex/gone", TokenSet{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	deps := func(key string) (AcctDeps, error) {
		if key == "codex/gone" {
			return AcctDeps{}, errGone
		}
		return AcctDeps{Client: directClient(), AuthBase: st.srv.URL}, nil
	}
	r := NewRefresher(store, deps, DefaultRefreshTick, DefaultRefreshBefore)
	r.RunOnce(context.Background()) // не паникует и не валится
	if got := st.count(); got != 0 {
		t.Fatalf("refresh-вызовов = %d, ожидалось 0 (аккаунт недоступен)", got)
	}
	// Ключ НЕ удалён из Store (CF-7).
	if _, ok := store.Get("codex/gone"); !ok {
		t.Fatal("ключ исчезнувшего аккаунта удалён молча (CF-7)")
	}
}

var errGone = &goneError{}

type goneError struct{}

func (*goneError) Error() string { return "аккаунт исчез из конфига" }
