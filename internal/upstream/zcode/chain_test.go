package zcode

// Тесты PAT-цепочки и single-flight кеша (SPEC 7.4): полный успех со счётчиками
// (login 1, customerInfo 1, api_keys GET→POST, access_tokens 1), заголовки
// шагов (raw JWT на getCustomerInfo, Bearer на остальных), needs_relogin,
// transient, enable:false → Clear, кеш-хит, single-flight 10 горутин → 1 цепочка.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// chainDeps — deps на стаб (client аккаунта = directClient теста).
func chainDeps(key, base, clientVersion string) AcctDeps {
	return AcctDeps{
		AccountKey:    key,
		Client:        directClient(),
		Base:          base,
		ZcodeBase:     base,
		ClientVersion: clientVersion,
	}
}

// freshRec — запись с живым zai-токеном.
func freshRec() TokenRec {
	return TokenRec{ZaiAccessToken: "zai-tok-1", UserID: "u-1"}
}

// TestChainFullSuccess — весь путь первый раз: ровно по одному запросу каждого
// шага, заголовки/тела по SPEC 7.4, Store обновлён, PAT в кеше.
func TestChainFullSuccess(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)
	deps := chainDeps(key, st.srv.URL, "3.17.2")

	rec := freshRec()
	pat, err := chain.EnsurePAT(context.Background(), deps, &rec)
	if err != nil {
		t.Fatalf("EnsurePAT: %v", err)
	}
	if pat == "" {
		t.Fatal("пустой PAT")
	}

	// Счётчики: ровно по одному шагу; ключ отсутствовал в списке → создан.
	if got := st.count("POST", "/api/auth/z/login"); got != 1 {
		t.Fatalf("login = %d, ожидался 1", got)
	}
	if got := st.count("GET", "/customer"); got != 1 {
		t.Fatalf("customerInfo = %d, ожидался 1", got)
	}
	if got := st.count("GET", "/api_keys"); got != 1 {
		t.Fatalf("api_keys GET = %d, ожидался 1", got)
	}
	if got := st.count("POST", "/api_keys"); got != 1 {
		t.Fatalf("api_keys POST (создание) = %d, ожидался 1", got)
	}
	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("access_tokens = %d, ожидался 1", got)
	}

	// Шаг 2: raw бизнес-JWT БЕЗ Bearer (research §3.1 шаг 5).
	if req := st.lastReq("GET", "/customer"); req == nil || req.Auth == "" || strings.HasPrefix(req.Auth, "Bearer ") {
		t.Fatalf("getCustomerInfo должен нести РАВНЫЙ бизнес-JWT, получено %q", req.Auth)
	}
	// Шаги 3–4: Bearer (дефолтный бизнес-JWT стаба начинается с "eyJ").
	if req := st.lastReq("GET", "/api_keys"); req == nil || !strings.HasPrefix(req.Auth, "Bearer ey") {
		t.Fatalf("api_keys должен нести Bearer <бизнес-JWT>, получено %q", req.Auth)
	}
	if req := st.lastReq("POST", "/api_keys"); req == nil || req.Body != `{"name":"zcode-api-key","usageScene":1}` {
		t.Fatalf("тело создания ключа = %q", req.Body)
	}
	if req := st.lastReq("POST", "/access_tokens"); req == nil {
		t.Fatal("access_tokens не дошёл")
	} else {
		if !strings.Contains(req.Body, `"clientType":"zcode"`) || !strings.Contains(req.Body, `"clientVersion":"3.17.2"`) {
			t.Fatalf("тело access_tokens = %q", req.Body)
		}
	}

	// Store обновлён цепочкой (business JWT, локация, Key ID).
	recSt, ok := store.Get(key)
	if !ok || recSt.BusinessJWT == "" || recSt.Org != "org-1" || recSt.Project != "prj-1" || recSt.APIKeyID != "key-1" {
		t.Fatalf("Store после цепочки: %+v (ok=%v)", recSt, ok)
	}
	if !recSt.BusinessExpiresAt.After(time.Now()) {
		t.Fatalf("business_expires_at не в будущем: %v", recSt.BusinessExpiresAt)
	}
	// zai-токен не затёрт точечным мёржем.
	if recSt.ZaiAccessToken != "zai-tok-1" {
		t.Fatalf("мёрж затёр zai-токен: %+v", recSt)
	}

	// PAT в кеше с TTL.
	if _, exp, ok := chain.PATInfo(key); !ok || !exp.After(time.Now().Add(patMinLife)) {
		t.Fatalf("PATInfo: exp=%v ok=%v", exp, ok)
	}

	// Повторный EnsurePAT — кеш-хит, сеть не дёргается.
	pat2, err := chain.EnsurePAT(context.Background(), deps, &rec)
	if err != nil || pat2 != pat {
		t.Fatalf("кеш-хит: pat2=%q err=%v", pat2, err)
	}
	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("кеш-хит сходил в сеть (access_tokens = %d)", got)
	}
}

// TestChainNeedsRelogin — бизнес-код ≠0 от auth/z/login (issue #1001) →
// needs_relogin: статус в Store, клиентские шаги не выполняются.
func TestChainNeedsRelogin(t *testing.T) {
	st := newZStub(t)
	st.onLogin = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"code":500,"msg":"Z.ai user information is invalid"}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)

	_, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), freshRecP())
	if err == nil {
		t.Fatal("ожидали ошибку needs_relogin")
	}
	if !IsFatalChainErr(err) {
		t.Fatalf("needs_relogin должен быть фатальным для флоу: %v", err)
	}
	if !strings.Contains(err.Error(), "бизнес-код 500") {
		t.Fatalf("текст ошибки должен нести бизнес-код: %v", err)
	}
	if got := st.count("GET", "/customer"); got != 0 {
		t.Fatalf("после needs_relogin цепочка не должна продолжаться (customer = %d)", got)
	}
	if stt := store.Status(key); stt.State != StatusNeedsRelogin {
		t.Fatalf("статус Store = %s, ожидался needs_relogin", stt.State)
	}
}

// TestChainNeedsReloginStaleAfterRotation — гонка «sender взял старую запись →
// параллельный OAuth записал свежий zai-токен → старая цепочка дождалась
// мьютекса и логинится устаревшим zai»: бизнес-код 500 НЕ должен ставить
// needs_relogin поверх свежего OAuth (ложный статус жил бы до следующего
// входа: sender отказывает, тикер пропускает). Ошибка — transient: тикер
// повторит цепочку уже со свежим токеном.
func TestChainNeedsReloginStaleAfterRotation(t *testing.T) {
	st := newZStub(t)
	st.onLogin = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"code":500,"msg":"Z.ai user information is invalid"}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)

	// Sender успел взять копию записи ДО ротации.
	stale := freshRecP()
	// Параллельный OAuth-флоу персистит свежие токены (Set стирает статус).
	if err := store.Set(key, TokenRec{ZaiAccessToken: "zai-tok-2", UserID: "u-1"}); err != nil {
		t.Fatalf("Set (ротация): %v", err)
	}

	_, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), stale)
	if err == nil {
		t.Fatal("ожидали ошибку цепочки (старый zai-токен отвергнут)")
	}
	// Сценарий тот: цепочка логинилась СТАРЫМ токеном.
	if req := st.lastReq("POST", "/api/auth/z/login"); req == nil || !strings.Contains(req.Body, "zai-tok-1") {
		t.Fatalf("цепочка должна логиниться устаревшим zai-токеном, запрос: %+v", req)
	}
	if !strings.Contains(err.Error(), "ротирован параллельным OAuth") {
		t.Fatalf("текст ошибки: %v", err)
	}
	if IsFatalChainErr(err) {
		t.Fatalf("ротированный needs_relogin должен быть transient: %v", err)
	}
	if stt := store.Status(key); stt.State != StatusOK {
		t.Fatalf("статус Store = %s, ожидался ok (свежий OAuth не должен перечёркиваться)", stt.State)
	}
}

// TestChainTransientNoStatus — HTTP 500 от login: ошибка transient, статус
// аккаунта не меняется (SPEC 7.4: старые токены живут, ретрай тикером).
func TestChainTransientNoStatus(t *testing.T) {
	st := newZStub(t)
	st.onLogin = func(w http.ResponseWriter, r *hitReq) { jsonResponse(w, 500, `{"error":"down"}`) }
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)

	_, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), freshRecP())
	if err == nil || IsFatalChainErr(err) {
		t.Fatalf("HTTP 500 login должен быть transient: %v", err)
	}
	if stt := store.Status(key); stt.State != StatusOK {
		t.Fatalf("transient не должен менять статус: %s", stt.State)
	}
	if _, _, ok := chain.PATInfo(key); ok {
		t.Fatal("PAT не должен появиться в кеше при провале")
	}
}

// TestChainEnableFalseClears — enable:false от access_tokens → fatal + очистка
// PAT-кеша (SPEC 7.4 «мгновенная очистка PAT-кеша»).
func TestChainEnableFalseClears(t *testing.T) {
	st := newZStub(t)
	st.onAccessTokens = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"code":0,"data":{"enable":false,"accessToken":"","expiresIn":3600}}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)

	_, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), freshRecP())
	if err == nil || !IsFatalChainErr(err) {
		t.Fatalf("enable:false должен быть fatal: %v", err)
	}
	if !strings.Contains(err.Error(), "enable:false") {
		t.Fatalf("текст должен называть enable:false: %v", err)
	}
	if _, _, ok := chain.PATInfo(key); ok {
		t.Fatal("PAT-кеш не очищен после fatal (SPEC 7.4)")
	}
}

// TestChainAccessTokens404Fatal — 404/401/403 от access_tokens → fatal.
func TestChainAccessTokens404Fatal(t *testing.T) {
	for _, code := range []int{401, 403, 404} {
		t.Run(fmt.Sprintf("HTTP %d", code), func(t *testing.T) {
			st := newZStub(t)
			st.onAccessTokens = func(w http.ResponseWriter, r *hitReq) {
				w.WriteHeader(code)
			}
			store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
			key := AccountKey("main")
			if err := store.Set(key, freshRec()); err != nil {
				t.Fatalf("Set: %v", err)
			}
			chain := NewPATCache(store)
			_, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), freshRecP())
			if err == nil || !IsFatalChainErr(err) {
				t.Fatalf("HTTP %d от access_tokens должен быть fatal: %v", code, err)
			}
		})
	}
}

// TestChainTeamOnlyAccount — только team-проекты (projectType=="2") → явная
// ошибка «team-аккаунты не поддерживаются» (SPEC 7.4).
func TestChainTeamOnlyAccount(t *testing.T) {
	st := newZStub(t)
	st.onCustomer = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"code":0,"data":{"organizations":[{"organizationId":"org-team",`+
			`"isDefault":true,"projects":[{"projectId":"prj-team","projectType":"2","isDefault":true}]}]}}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)
	_, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), freshRecP())
	if err == nil || !IsFatalChainErr(err) {
		t.Fatalf("team-only аккаунт должен дать фатальную ошибку: %v", err)
	}
	if !strings.Contains(err.Error(), "team-аккаунты не поддерживаются") {
		t.Fatalf("текст ошибки: %v", err)
	}
}

// TestEnsurePATSingleFlight — 10 горутин EnsurePAT параллельно → ровно ОДНА
// цепочка (счётчик access_tokens = 1), все получили один PAT (SPEC 7.4).
func TestEnsurePATSingleFlight(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)
	deps := chainDeps(key, st.srv.URL, "3.17.2")

	const n = 10
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pat, err := chain.EnsurePAT(context.Background(), deps, freshRecP())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seen[pat]++
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("параллельные EnsurePAT вернули ошибки: %v", errs)
	}
	if len(seen) != 1 {
		t.Fatalf("горутины получили разные PAT: %v", seen)
	}
	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("access_tokens = %d, ожидался РОВНО 1 (single-flight)", got)
	}
	if got := st.count("POST", "/api/auth/z/login"); got != 1 {
		t.Fatalf("login = %d, ожидался 1", got)
	}
}

// TestEnsurePATClearForcesReissue — Clear + EnsurePAT выдаёт НОВЫЙ PAT
// (механизм 401-ретрая sender'а): стаб выдаёт уникальный accessToken на вызов.
func TestEnsurePATClearForcesReissue(t *testing.T) {
	st := newZStub(t)
	var patN int
	st.onAccessTokens = func(w http.ResponseWriter, r *hitReq) {
		patN++
		jsonResponse(w, 200, `{"code":0,"msg":"","data":{"enable":true,"accessToken":"pat-`+strconv.Itoa(patN)+`","expiresIn":3600}}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	if err := store.Set(key, freshRec()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)
	deps := chainDeps(key, st.srv.URL, "3.17.2")

	pat1, err := chain.EnsurePAT(context.Background(), deps, freshRecP())
	if err != nil {
		t.Fatalf("первый EnsurePAT: %v", err)
	}
	chain.Clear(key)
	pat2, err := chain.EnsurePAT(context.Background(), deps, freshRecP())
	if err != nil {
		t.Fatalf("EnsurePAT после Clear: %v", err)
	}
	if pat1 == pat2 {
		t.Fatal("после Clear PAT должен быть перевыпущен")
	}
	if got := st.count("POST", "/access_tokens"); got != 2 {
		t.Fatalf("access_tokens = %d, ожидалось 2", got)
	}
}

// TestChainReuseCachedLocation — запись с кэшированной локацией/Key ID делает
// цепочку из одного access_tokens (локация и ключ не перезапрашиваются).
func TestChainReuseCachedLocation(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	rec := freshRec()
	rec.Org, rec.Project, rec.APIKeyID = "org-1", "prj-1", "key-1"
	rec.BusinessJWT = fakeJWT(t, `{"typ":"biz","exp":`+fmt.Sprint(time.Now().Add(time.Hour).Unix())+`}`)
	rec.BusinessExpiresAt = time.Now().Add(time.Hour)
	if err := store.Set(key, rec); err != nil {
		t.Fatalf("Set: %v", err)
	}
	chain := NewPATCache(store)

	pat, err := chain.EnsurePAT(context.Background(), chainDeps(key, st.srv.URL, "3.17.2"), &rec)
	if err != nil {
		t.Fatalf("EnsurePAT: %v", err)
	}
	if pat == "" {
		t.Fatal("пустой PAT")
	}
	if got := st.count("GET", "/customer"); got != 0 {
		t.Fatalf("кэшированная локация не должна перезапрашиваться (customer = %d)", got)
	}
	if got := st.count("GET", "/api_keys"); got != 0 {
		t.Fatalf("кэшированный Key ID не должен перезапрашиваться (api_keys = %d)", got)
	}
	if got := st.count("POST", "/access_tokens"); got != 1 {
		t.Fatalf("access_tokens = %d, ожидался 1", got)
	}
}

// TestEnsurePATValidations — пустые deps/rec отклоняются до сети.
func TestEnsurePATValidations(t *testing.T) {
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	chain := NewPATCache(store)
	empty := AcctDeps{Client: directClient()}
	if _, err := chain.EnsurePAT(context.Background(), empty, freshRecP()); err == nil {
		t.Fatal("пустой AccountKey должен быть ошибкой")
	}
	deps := chainDeps(AccountKey("x"), "http://127.0.0.1:1", "3.17.2")
	if _, err := chain.EnsurePAT(context.Background(), deps, &TokenRec{}); err == nil {
		t.Fatal("rec без zai-токена должен быть ошибкой")
	}
	if _, _, ok := chain.PATInfo(AccountKey("x")); ok {
		t.Fatal("кеш не должен заполняться при отклонённых вызовах")
	}
	// (nil, err) контракт: сетевые ошибки возвращаются как результат-ошибка цепочки,
	// не паника — проверяем недостижимый сервер.
	rec := freshRec()
	_, err := chain.EnsurePAT(context.Background(), deps, &rec)
	if err == nil {
		t.Fatal("недостижимый сервер должен дать ошибку цепочки")
	}
	var ce *chainError
	if !errors.As(err, &ce) || ce.kind != chainTransient {
		t.Fatalf("сетевой сбой должен классифицироваться transient: %v", err)
	}
}

// freshRecP — указатель на свежую запись (хелпер: рекорд в тестах передаётся
// копией, чтобы EnsurePAT мутировал именно копию).
func freshRecP() *TokenRec {
	r := freshRec()
	return &r
}
