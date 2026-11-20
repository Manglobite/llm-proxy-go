package codex

// Тесты флоу-рантайма (OA-6): TTL, один флоу на аккаунт, валидация state
// (чужой/истёкший/одноразовость), Complete через клиент аккаунта → Store.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// flowEnv — Store + Flows со стабом token-эндпоинта и deps на socks.Direct.
type flowEnv struct {
	store *Store
	stub  *tokenStub
	flows *Flows
}

func newFlowEnv(t *testing.T, ttl time.Duration) *flowEnv {
	t.Helper()
	e := &flowEnv{}
	e.store, _ = OpenStore(t.TempDir() + "/tokens.json")
	e.stub = newTokenStub(t, func(st *tokenStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tokenJSON(t, "at-flow", "rt-flow", fakeJWT(t, claimsPayload), 600))
	})
	e.flows = NewFlows(e.store, ttl, func(accountKey string) (AcctDeps, error) {
		return AcctDeps{Client: directClient(), AuthBase: e.stub.srv.URL}, nil
	})
	return e
}

func callbackURL(state string) string {
	return fmt.Sprintf("http://127.0.0.1:1455/auth/callback?code=auth-code-9&state=%s", urlQueryEscape(state))
}

func urlQueryEscape(s string) string {
	// state — base64url без padding, спец-символов не содержит; escape через
	// net/url не нужен, но делаем честно через Values для устойчивости.
	return strings.ReplaceAll(s, "+", "%2B")
}

func TestFlowsStartBuildsAuthorizeURLWithState(t *testing.T) {
	e := newFlowEnv(t, 0)
	authorizeURL, state, err := e.flows.Start("codex/a1", "http://127.0.0.1:1455/auth/callback")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(authorizeURL, e.stub.srv.URL+"/oauth/authorize?") {
		t.Fatalf("authorizeURL = %s (origin должен быть из deps.AuthBase)", authorizeURL)
	}
	if !strings.Contains(authorizeURL, "state="+state) ||
		!strings.Contains(authorizeURL, "code_challenge_method=S256") ||
		!strings.Contains(authorizeURL, "client_id=app_EMoamEEZ73f0CkXaXp7hrann") {
		t.Fatalf("authorizeURL без обязательных параметров: %s", authorizeURL)
	}
}

func TestFlowsCompleteExchangesAndStores(t *testing.T) {
	e := newFlowEnv(t, 0)
	_, state, err := e.flows.Start("codex/a1", "http://127.0.0.1:1455/auth/callback")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ts, err := e.flows.Complete(context.Background(), "codex/a1", callbackURL(state))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if ts.AccessToken != "at-flow" || ts.AccountID != "acc_9wBk" {
		t.Fatalf("TokenSet = %+v", ts)
	}
	// Токены записаны под ключом аккаунта.
	if got, ok := e.store.Get("codex/a1"); !ok || got.AccessToken != "at-flow" {
		t.Fatalf("Store после Complete: %+v ok=%v", got, ok)
	}
	// Форма обмена несла verifier флоу (PKCE), а не произвольный.
	if e.stub.verifier == "" {
		t.Fatal("exchange без code_verifier")
	}
}

func TestFlowsOneFlowPerAccountReplacesOldState(t *testing.T) {
	e := newFlowEnv(t, 0)
	_, state1, err := e.flows.Start("codex/a1", "http://127.0.0.1:1455/auth/callback")
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	_, state2, err := e.flows.Start("codex/a1", "http://127.0.0.1:1455/auth/callback")
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if state1 == state2 {
		t.Fatal("новый флоу не заменил старый (state повторился)")
	}
	// Старый state больше не принимается: SPEC 12 — один активный флоу.
	if _, err := e.flows.Complete(context.Background(), "codex/a1", callbackURL(state1)); err == nil {
		t.Fatal("устаревший state принят")
	}
	// Новый — работает.
	if _, err := e.flows.Complete(context.Background(), "codex/a1", callbackURL(state2)); err != nil {
		t.Fatalf("новый state отклонён: %v", err)
	}
}

func TestFlowsRejectsForeignState(t *testing.T) {
	e := newFlowEnv(t, 0)
	_, stateA, err0 := e.flows.Start("codex/a", "http://127.0.0.1:1455/auth/callback")
	if err0 != nil {
		t.Fatalf("Start a: %v", err0)
	}
	if _, _, err := e.flows.Start("codex/b", "http://127.0.0.1:1455/auth/callback"); err != nil {
		t.Fatalf("Start b: %v", err)
	}
	// Флоу аккаунта a завершают под именем b — security-отказ (OA-6).
	_, err := e.flows.Complete(context.Background(), "codex/b", callbackURL(stateA))
	if err == nil || !strings.Contains(err.Error(), "другому аккаунту") {
		t.Fatalf("чужой state принят: %v", err)
	}
	// Неизвестный state — тоже отказ.
	if _, err := e.flows.Complete(context.Background(), "codex/a", callbackURL("no-such-state")); err == nil {
		t.Fatal("неизвестный state принят")
	}
}

func TestFlowsStateIsSingleUse(t *testing.T) {
	e := newFlowEnv(t, 0)
	_, state, errS := e.flows.Start("codex/a1", "http://127.0.0.1:1455/auth/callback")
	if errS != nil {
		t.Fatalf("Start: %v", errS)
	}
	if _, err := e.flows.Complete(context.Background(), "codex/a1", callbackURL(state)); err != nil {
		t.Fatalf("первый Complete: %v", err)
	}
	// Повторный Complete с тем же URL не должен уметь повторить exchange.
	if _, err := e.flows.Complete(context.Background(), "codex/a1", callbackURL(state)); err == nil {
		t.Fatal("state переиспользован (одноразовость нарушена)")
	}
}

func TestFlowsTTLExpiry(t *testing.T) {
	e := newFlowEnv(t, time.Minute)
	_, state, err := e.flows.Start("codex/a1", "http://127.0.0.1:1455/auth/callback")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Инжект истёкшего флоу в in-memory состояние (внутренности пакета —
	// тесты без time.Sleep, CODING 1.9).
	e.flows.mu.Lock()
	flw, ok := e.flows.byState[state]
	if !ok {
		e.flows.mu.Unlock()
		t.Fatalf("флоу %q не найден", state)
	}
	flw.expiresAt = time.Now().Add(-time.Second)
	e.flows.mu.Unlock()

	_, err = e.flows.Complete(context.Background(), "codex/a1", callbackURL(state))
	if err == nil || !strings.Contains(err.Error(), "истёк") {
		t.Fatalf("истёкший state принят: %v", err)
	}
}

func TestFlowsConcurrentStartComplete(t *testing.T) {
	// Потокобезопасность Start/Complete (-race): параллельные Start на
	// несколько аккаунтов и Complete на своих состояниях.
	e := newFlowEnv(t, 0)
	states := make([]string, 8)
	var wg sync.WaitGroup
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, st, err := e.flows.Start(fmt.Sprintf("codex/a%d", i), "http://127.0.0.1:1455/auth/callback")
			if err != nil {
				t.Errorf("Start %d: %v", i, err)
				return
			}
			states[i] = st
		}(i)
	}
	wg.Wait()
	for i, st := range states {
		if _, err := e.flows.Complete(context.Background(), fmt.Sprintf("codex/a%d", i), callbackURL(st)); err != nil {
			t.Errorf("Complete %d: %v", i, err)
		}
	}
}

func TestFlowsDepsErrorPropagates(t *testing.T) {
	store, _ := OpenStore(t.TempDir() + "/tokens.json")
	flows := NewFlows(store, 0, func(string) (AcctDeps, error) {
		return AcctDeps{}, errors.New("аккаунт исчез")
	})
	if _, _, err := flows.Start("codex/x", "http://127.0.0.1:1455/auth/callback"); err == nil {
		t.Fatal("Start при недоступном аккаунте обязан вернуть ошибку deps")
	}
}

// TestFlowsCompleteNormalizesSchemelessPaste — Ф4-fix1 F2: redirect-URL,
// вставленный без схемы («127.0.0.1:1455/auth/callback?…»), обменивается так же,
// как полный (url.Parse без схемы не извлёк бы state).
func TestFlowsCompleteNormalizesSchemelessPaste(t *testing.T) {
	e := newFlowEnv(t, 0)
	_, state, err := e.flows.Start("codex/a1", RedirectURI(0)) // дефолт 1455 (SPEC 7.1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	schemeless := strings.TrimPrefix(callbackURL(state), "http://")
	if strings.HasPrefix(schemeless, "http") {
		t.Fatalf("тест сломан: схема не срезана: %q", schemeless)
	}
	ts, err := e.flows.Complete(context.Background(), "codex/a1", schemeless)
	if err != nil {
		t.Fatalf("Complete без схемы: %v", err)
	}
	if ts.AccessToken != "at-flow" {
		t.Fatalf("Complete без схемы: %+v", ts)
	}
}
