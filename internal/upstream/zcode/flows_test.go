package zcode

// Тесты рантайма OAuth-флоу (SPEC 7.4/12): Start (init синхронно, ошибки видны),
// Pending-статусы, pollOnce-переходы (pending/ready/failed/смерть), колбэк-этап
// (персист токенов СРАЗУ → PAT-цепочка; device_mid переиспользуется; transient
// не портит ready). Переходы драйвятся pollOnce напрямую на синтетическом флоу
// — детерминировано, без гонок с горутиной-поллером (CODING 1.9).

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestFlows — рантайм флоу на отменяемом контексте (cleanup гасит горутины).
func newTestFlows(t *testing.T, store *Store, chain *PATCache) *Flows {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewFlows(store, chain, ctx)
}

// flowDeps — deps-замыкание на стаб.
func flowDeps(st *zStub, store *Store) Deps {
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

// mustDeps — deps для прямых вызовов pollOnce/finalize.
func mustDeps(st *zStub, key string) AcctDeps {
	d, _ := flowDeps(st, nil)(key)
	return d
}

// readyPollBody — poll-ответ ready (все обязательные поля; zai-токен — JWT
// с exp, чтобы проверять извлечение zai_expires_at).
func readyPollBody() string {
	zai := fakeJWTStatic(`{"exp":` + strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10) + `}`)
	return `{"code":0,"data":{"status":"ready","token":"zjwt-flow",` +
		`"user":{"user_id":"u-flow","name":"Вася","email":"v@z.ai"},` +
		`"zai":{"access_token":"` + zai + `"}}}`
}

// readyResult — готовый poll-результат ready.
func readyResult() *pollResult {
	return &pollResult{
		Status:         "ready",
		Token:          "zjwt-flow",
		UserID:         "u-flow",
		Name:           "Вася",
		Email:          "v@z.ai",
		ZaiAccessToken: fakeJWTStatic(`{"exp":` + strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10) + `}`),
	}
}

// activeFlowForTest — доступ к внутренней записи флоу для прямого драйва
// pollOnce/finalize (без реальной горутины-поллера); если Start не звали —
// синтетический флоу РЕГИСТРИРУЕТСЯ в карте, чтобы Pending его видел.
func (f *Flows) activeFlowForTest(accountKey string) *activeFlow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flw, ok := f.active[accountKey]; ok {
		return flw
	}
	ctx, cancel := context.WithCancel(f.ctx)
	flw := &activeFlow{
		accountKey: accountKey,
		ctx:        ctx,
		cancel:     cancel,
		state:      PendingPending,
	}
	f.active[accountKey] = flw
	return flw
}

func TestFlowsStartReturnsURLAndPending(t *testing.T) {
	st := newZStub(t)
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	f := newTestFlows(t, store, NewPATCache(store))
	key := AccountKey("main")

	url, err := f.Start(context.Background(), key, flowDeps(st, store))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if url != "https://chat.z.ai/api/oauth/authorize?client_id=client_x&state=srvstate" {
		t.Fatalf("authorize_url = %q", url)
	}
	p := f.Pending(key)
	if p.State != PendingPending || p.AuthorizeURL != url {
		t.Fatalf("Pending = %+v", p)
	}
	if p2 := f.Pending(AccountKey("ghost")); p2.State != PendingNone {
		t.Fatalf("чужой аккаунт: %+v", p2)
	}
}

func TestFlowsStartInitError(t *testing.T) {
	st := newZStub(t)
	st.onInit = func(w http.ResponseWriter, r *hitReq) { jsonResponse(w, 500, `{"code":2007,"msg":"http error"}`) }
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	f := newTestFlows(t, store, NewPATCache(store))

	_, err := f.Start(context.Background(), AccountKey("main"), flowDeps(st, store))
	if err == nil {
		t.Fatal("провал init должен быть ошибкой Start (виден пользователю)")
	}
	if p := f.Pending(AccountKey("main")); p.State != PendingNone {
		t.Fatalf("флоу не должен регистрироваться при провале init: %+v", p)
	}
}

func TestFlowsStartReplacesOldFlow(t *testing.T) {
	st := newZStub(t)
	// Каждый init даёт новый flow_id/authorize_url.
	var initN int
	st.onInit = func(w http.ResponseWriter, r *hitReq) {
		initN++
		n := strconv.Itoa(initN)
		jsonResponse(w, 200, `{"code":0,"data":{"flow_id":"flow-`+n+`","poll_token":"ptok-`+n+`",`+
			`"authorize_url":"https://chat.z.ai/authorize?n=`+n+`",`+
			`"expires_at":`+futureUnix()+`,"poll_interval_sec":2}}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	f := newTestFlows(t, store, NewPATCache(store))
	key := AccountKey("main")

	url1, err := f.Start(context.Background(), key, flowDeps(st, store))
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	url2, err := f.Start(context.Background(), key, flowDeps(st, store))
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if url1 == url2 {
		t.Fatal("второй Start должен заменить флоу (новая ссылка)")
	}
	if p := f.Pending(key); p.AuthorizeURL != url2 {
		t.Fatalf("Pending показывает старый флоу: %+v", p)
	}
}

// TestFlowsPollOnceLifecycle — переходы pollOnce на синтетическом флоу.
func TestFlowsPollOnceLifecycle(t *testing.T) {
	t.Run("ready → колбэк-этап", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) { jsonResponse(w, 200, readyPollBody()) }
		store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
		chain := NewPATCache(store)
		f := newTestFlows(t, store, chain)
		key := AccountKey("main")
		flw := f.activeFlowForTest(key)

		if !f.pollOnce(flw, mustDeps(st, key)) {
			t.Fatal("ready должен завершить флоу")
		}
		if p := f.Pending(key); p.State != PendingReady {
			t.Fatalf("Pending = %+v, ожидался ready", p)
		}
		// Персист СРАЗУ (SPEC 12): запись уже в Store до/независимо от цепочки.
		rec, ok := store.Get(key)
		if !ok || rec.ZaiAccessToken == "" || rec.ZcodeJWT != "zjwt-flow" ||
			rec.UserID != "u-flow" || rec.Email != "v@z.ai" {
			t.Fatalf("OAuth-результат не персистнут: %+v", rec)
		}
		// PAT-цепочка отработала (отдельный шаг).
		if got := st.count("POST", "/access_tokens"); got != 1 {
			t.Fatalf("access_tokens = %d, ожидался 1", got)
		}
		if stt := store.Status(key); stt.State != StatusOK {
			t.Fatalf("статус после OAuth = %s, ожидался ok", stt.State)
		}
		// zai_expires_at: exp-клеймо опционально парсится (SPEC 3.2а).
		if rec.ZaiExpiresAt == nil || !rec.ZaiExpiresAt.After(time.Now()) {
			t.Fatalf("zai_expires_at не извлечён: %v", rec.ZaiExpiresAt)
		}
	})

	t.Run("failed провайдера", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 200, `{"code":0,"data":{"status":"failed"}}`)
		}
		store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
		f := newTestFlows(t, store, NewPATCache(store))
		key := AccountKey("main")
		if !f.pollOnce(f.activeFlowForTest(key), mustDeps(st, key)) {
			t.Fatal("failed должен завершить флоу")
		}
		if p := f.Pending(key); p.State != PendingFailed || p.Err == "" {
			t.Fatalf("Pending = %+v, ожидался failed с ошибкой", p)
		}
	})

	t.Run("400 — смерть флоу", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 400, `{"code":3004,"msg":"invalid_flow"}`)
		}
		store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
		f := newTestFlows(t, store, NewPATCache(store))
		key := AccountKey("main")
		if !f.pollOnce(f.activeFlowForTest(key), mustDeps(st, key)) {
			t.Fatal("смерть флоу должна завершить поллер")
		}
		if p := f.Pending(key); p.State != PendingFailed || !strings.Contains(p.Err, "флоу умер") {
			t.Fatalf("Pending = %+v", p)
		}
	})

	t.Run("5xx — ретрай, флоу жив", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 500, `{"boom":1}`)
		}
		store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
		f := newTestFlows(t, store, NewPATCache(store))
		key := AccountKey("main")
		if f.pollOnce(f.activeFlowForTest(key), mustDeps(st, key)) {
			t.Fatal("5xx не должен завершать флоу (ретрай на следующем тике)")
		}
		if p := f.Pending(key); p.State != PendingPending {
			t.Fatalf("Pending = %+v, ожидался pending", p)
		}
	})
}

// TestFlowsFinalizeDeviceMidReuse — device_mid переиспользуется из записи,
// иначе генерится новый uuid v4 (SPEC 7.4).
func TestFlowsFinalizeDeviceMidReuse(t *testing.T) {
	t.Run("reuse существующего", func(t *testing.T) {
		st := newZStub(t)
		store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
		key := AccountKey("main")
		if err := store.Set(key, TokenRec{ZaiAccessToken: "old", DeviceMid: "dev-existing"}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		f := newTestFlows(t, store, NewPATCache(store))
		f.finalize(f.activeFlowForTest(key), mustDeps(st, key), readyResult())
		rec, _ := store.Get(key)
		if rec.DeviceMid != "dev-existing" {
			t.Fatalf("device_mid не переиспользован: %q", rec.DeviceMid)
		}
	})
	t.Run("новый uuid v4 и стабильность между флоу", func(t *testing.T) {
		st := newZStub(t)
		store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
		key := AccountKey("main")
		f := newTestFlows(t, store, NewPATCache(store))

		// Первый флоу: device_mid генерится и персистится вместе с токенами.
		f.finalize(f.activeFlowForTest(key), mustDeps(st, key), readyResult())
		rec1, ok := store.Get(key)
		if !ok {
			t.Fatal("запись не создана")
		}
		if !uuidV4RE.MatchString(rec1.DeviceMid) {
			t.Fatalf("device_mid не uuid v4: %q", rec1.DeviceMid)
		}
		// Повторный флоу (например после needs_relogin): тот же device_mid.
		f.finalize(f.activeFlowForTest(key), mustDeps(st, key), readyResult())
		rec2, _ := store.Get(key)
		if rec2.DeviceMid != rec1.DeviceMid {
			t.Fatalf("device_mid не стабилен между флоу: %q vs %q", rec1.DeviceMid, rec2.DeviceMid)
		}
	})
}

// TestFlowsFinalizeTransientKeepsReady — transient-провал цепочки после OAuth:
// аккаунт активен (pending ready), статус ok, тикер доведёт (SPEC 12).
func TestFlowsFinalizeTransientKeepsReady(t *testing.T) {
	st := newZStub(t)
	st.onAccessTokens = func(w http.ResponseWriter, r *hitReq) { w.WriteHeader(500) }
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	chain := NewPATCache(store)
	key := AccountKey("main")
	f := newTestFlows(t, store, chain)

	f.finalize(f.activeFlowForTest(key), mustDeps(st, key), readyResult())
	if p := f.Pending(key); p.State != PendingReady {
		t.Fatalf("transient цепочки не должен портить ready: %+v", p)
	}
	if stt := store.Status(key); stt.State != StatusOK {
		t.Fatalf("статус = %s, ожидался ok (токены записаны)", stt.State)
	}
	if _, _, ok := chain.PATInfo(key); ok {
		t.Fatal("PAT не должен быть в кеше при провале цепочки")
	}
}

// TestFlowsFinalizeFatalFailsPending — fatal цепочки после OAuth → failed
// с ошибкой в pending (SPEC 12), токены при этом сохранены.
func TestFlowsFinalizeFatalFailsPending(t *testing.T) {
	st := newZStub(t)
	st.onAccessTokens = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"code":0,"data":{"enable":false}}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	f := newTestFlows(t, store, NewPATCache(store))

	f.finalize(f.activeFlowForTest(key), mustDeps(st, key), readyResult())
	if p := f.Pending(key); p.State != PendingFailed || !strings.Contains(p.Err, "PAT-цепочка") {
		t.Fatalf("Pending = %+v, ожидался failed с ошибкой цепочки", p)
	}
	// Токены сохранены, несмотря на fatal цепочки (SPEC 12: персист СНАЧАЛА).
	if rec, ok := store.Get(key); !ok || rec.ZaiAccessToken == "" {
		t.Fatalf("OAuth-токены потеряны при fatal цепочки: %+v ok=%v", rec, ok)
	}
}

// TestFlowsFinalizeNeedsReloginDetector — бизнес-код ≠0 от login ПОСЛЕ свежего
// OAuth: детектор SPEC 7.4 честно ставит needs_relogin (сервер не признал токен),
// pending → failed.
func TestFlowsFinalizeNeedsReloginDetector(t *testing.T) {
	st := newZStub(t)
	st.onLogin = func(w http.ResponseWriter, r *hitReq) {
		jsonResponse(w, 200, `{"code":500,"msg":"Z.ai user information is invalid"}`)
	}
	store, _ := OpenStore(t.TempDir() + "/zcode-tokens.json")
	key := AccountKey("main")
	f := newTestFlows(t, store, NewPATCache(store))

	f.finalize(f.activeFlowForTest(key), mustDeps(st, key), readyResult())
	if stt := store.Status(key); stt.State != StatusNeedsRelogin {
		t.Fatalf("статус = %s, ожидался needs_relogin по детектору login", stt.State)
	}
	if p := f.Pending(key); p.State != PendingFailed || !strings.Contains(p.Err, "needs_relogin") &&
		!strings.Contains(p.Err, "бизнес-код") {
		t.Fatalf("Pending = %+v", p)
	}
}
