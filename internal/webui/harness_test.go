// Тест-харнесс webui: собирается полный стек WebUI (SQLite в tmp, codex Store
// на tokens.json во tmp, Flows на фейковом auth-стабе, socks-пути без Run),
// формы ответов сверяются с литералами mock.js через shape-сравнение (CODING 1.9:
// стабы httptest, никаких реальных внешних хостов и time.Sleep).
package webui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream/codex"
	"llm-proxy/internal/upstream/zcode"
)

// env — окружение одного теста.
type env struct {
	t        *testing.T
	ui       *Handler
	snap     *config.Snapshot
	acct     *accounting.DB
	elog     *errorslog.DB
	qdb      *quotas.DB
	store    *codex.Store
	flows    *codex.Flows
	depsFn   codex.Deps       // резолв аккаунта (клиент Direct на локальный стаб)
	auth     *httptest.Server // фейковый auth.openai.com
	authBase string
	tmp      string
	port     int // callback-порт codex-аккаунта (свободный, выдан заранее)

	// zcode-рантайм (Ф7b): стаб z.ai + Store/PATCache/Flows на клиенте Direct.
	zf     *zfake
	zstore *zcode.Store
	zchain *zcode.PATCache
	zflows *zcode.Flows
	zdeps  zcode.Deps
}

// rebuildUI — пересобрать Handler (тест подменил flows/снапшот и т.п.).
func (e *env) rebuildUI() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	ui, err := New(Deps{
		Snapshot:   func() *config.Snapshot { return e.snap },
		CodexDeps:  e.depsFn,
		Store:      e.store,
		Flows:      e.flows,
		ZStore:     e.zstore,
		ZFlows:     e.zflows,
		ZChain:     e.zchain,
		ZcodeDeps:  e.zdeps,
		Accounting: e.acct,
		ErrorsLog:  e.elog,
		Quotas:     e.qdb,
		StartedAt:  testNow.Add(-6 * time.Hour),
		BaseCtx:    ctx,
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		e.t.Fatalf("webui.New (rebuild): %v", err)
	}
	e.ui = ui
}

// codexAcc — codex-аккаунт для снапшота (callback_port общий с c1 — listener один).
func codexAcc(id string, port int) config.CodexAccount {
	return config.CodexAccount{ID: id, Auth: config.OAuthAuth{Method: "oauth", CallbackPort: port}, Socks: "none"}
}

// freePort — свободный TCP-порт (для callback-listener'а теста).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// fakeAuthHandler — стаб auth-сервера codex: authorize отвечает CALLBACK_URL
// (как fakeupstream smoke-стаба), token — фиксированные токены с id_token-claims.
func fakeAuthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		redirect := r.URL.Query().Get("redirect_uri") // как реальный auth-сервер: эхо redirect_uri
		if state == "" || redirect == "" {
			http.Error(w, "нет state/redirect_uri", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("CALLBACK_URL=" + redirect + "?code=fake-code&state=" + state))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("refresh_token") == "fake-refresh-expired" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// id_token — неподписанный JWT с claims codex (SPEC 3.2), парсится
		// codex.parseIDTokenClaims: account_id/email попадают в TokenSet.
		payload := base64.RawURLEncoding.EncodeToString([]byte(
			`{"https://api.openai.com/auth":{"chatgpt_account_id":"acc_test_1"},` +
				`"https://api.openai.com/profile":{"email":"flow@test.ru"}}`))
		access := "fake-access"
		if rt := r.PostFormValue("refresh_token"); rt == "fake-refresh-2" {
			access = "fake-access-2" // refresh меняет access — проверяемо
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": "fake-refresh-2",
			"id_token": "hdr." + payload + ".sig", "expires_in": 3600,
		})
	})
	return mux
}

// newEnv — полный стек WebUI. webuiToken — "" или заданный токен.
func newEnv(t *testing.T, webuiToken string) *env {
	t.Helper()
	tmp := t.TempDir()

	// Фейковый auth-стаб (до deps-замыкания — нужен URL).
	auth := httptest.NewServer(fakeAuthHandler())
	t.Cleanup(auth.Close)

	// SQLite: три модуля на один файл (как в main).
	dbPath := filepath.Join(tmp, "proxy.db")
	acct, err := accounting.Open(dbPath)
	if err != nil {
		t.Fatalf("accounting.Open: %v", err)
	}
	t.Cleanup(func() { _ = acct.Close() })
	elog, err := errorslog.Open(dbPath)
	if err != nil {
		t.Fatalf("errorslog.Open: %v", err)
	}
	t.Cleanup(func() { _ = elog.Close() })
	qdb, err := quotas.Open(dbPath)
	if err != nil {
		t.Fatalf("quotas.Open: %v", err)
	}
	t.Cleanup(func() { _ = qdb.Close() })

	// codex Store + Flows на клиенте Direct (цель — httptest на localhost;
	// http.Client создаёт только internal/socks — I1).
	store, err := codex.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	deps := func(string) (codex.AcctDeps, error) {
		return codex.AcctDeps{Client: socks.Direct{}.Client(), AuthBase: auth.URL}, nil
	}
	flows := codex.NewFlows(store, 0, deps)

	// zcode: стаб z.ai + Store/PATCache/Flows (Ф7b) на клиенте Direct
	// (цель — httptest на localhost, I1 соблюдён).
	zf := newZFake(t, true)
	zstore, err := zcode.OpenStore(filepath.Join(tmp, "zcode-tokens.json"))
	if err != nil {
		t.Fatalf("zcode.OpenStore: %v", err)
	}
	zchain := zcode.NewPATCache(zstore)
	zdeps := func(accountKey string) (zcode.AcctDeps, error) {
		return zcode.AcctDeps{
			AccountKey:    accountKey,
			Client:        socks.Direct{}.Client(),
			Base:          zf.srv.URL,
			ZcodeBase:     zf.srv.URL,
			ClientVersion: "3.17.2",
		}, nil
	}
	flowsCtx, flowsCancel := context.WithCancel(context.Background())
	t.Cleanup(flowsCancel)
	zflows := zcode.NewFlows(zstore, zchain, flowsCtx)

	// Конфиг-снапшот: кодекс с callback_port на свободном порту (листенер
	// поднимется только в OAuth-тесте — стартуем до ensure).
	port := freePort(t)
	snap := &config.Snapshot{
		Cfg: &config.ResolvedConfig{
			Server: config.ServerConfig{
				Listen:     "127.0.0.1:8787",
				WebUIToken: webuiToken,
				Clients:    map[string]string{"opencode": "sk-oc", "zcode": "sk-zc"},
			},
			Storage: config.StorageConfig{DBPath: dbPath, TokensPath: filepath.Join(tmp, "tokens.json")},
			Socks: map[string]socks.ResolvedConfig{
				"work": {URL: "socks5h://127.0.0.1:1", CheckInterval: time.Minute},
			},
			Providers: map[string]config.Provider{
				"codex": {Type: "codex", Codex: &config.CodexProvider{
					Type: "codex",
					Accounts: []config.CodexAccount{
						{ID: "c1", Email: "cfg@t.ru", Auth: config.OAuthAuth{Method: "oauth", CallbackPort: port}, Socks: "none"},
					},
				}},
				"glm": {Type: "glm", GLM: &config.GLMProvider{
					Type: "glm",
					Accounts: []config.GLMAccount{
						{ID: "g1", Auth: config.KeyAuth{Method: "api_key", Key: "sk-glm"}, CodingPlan: true, Socks: "none",
							PlanCredits: &config.PlanCredits{Window5h: 12000, WindowWeekly: 60000}},
					},
				}},
				"zai-plan": {Type: "zcode", ZCode: &config.ZCodeProvider{
					Type: "zcode", Route: "pat",
					Accounts: []config.ZCodeAccount{
						{ID: "main", Auth: config.OAuthAuth{Method: "oauth"}, Socks: "none",
							PlanCredits: &config.PlanCredits{Window5h: 12000, WindowWeekly: 60000}},
					},
				}},
				"ollama": {Type: "openai-like", OpenAILike: &config.OpenAILikeProvider{
					Type: "openai-like", BaseURL: "http://127.0.0.1:1",
					Accounts: []config.OpenAILikeAccount{
						{ID: "o1", Auth: config.KeyAuth{Method: "api_key", Key: "sk-ol"}, Socks: "none", AllowDirect: true},
					},
				}},
			},
			Aliases: map[string]config.AliasConfig{
				"work/terra": {Account: "codex/c1", Model: "gpt-6.1-terra"},
				"glm/5.3":    {Account: "glm/g1", Model: "glm-5.3"},
				"qwen/cloud": {Account: "ollama/o1", Model: "qwen3-coder"},
			},
			Models: map[string]config.ModelConfig{
				"glm-5.3":       {GLMCredits: &config.CreditMults{InputMult: 6.9, CachedMult: 1.7, OutputMult: 24}},
				"gpt-6.1-terra": {Subscription: true},
			},
		},
		Socks: map[string]*socks.Path{},
	}
	// Путь work без Run: Status() отдаёт начальное состояние (healthy, без чеков).
	p, err := socks.New("work", snap.Cfg.Socks["work"])
	if err != nil {
		t.Fatalf("socks.New: %v", err)
	}
	snap.Socks["work"] = p

	e := &env{
		t: t, snap: snap, acct: acct, elog: elog, qdb: qdb,
		store: store, flows: flows, depsFn: deps,
		auth: auth, authBase: auth.URL,
		tmp: tmp, port: port,
		zf: zf, zstore: zstore, zchain: zchain, zflows: zflows, zdeps: zdeps,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ui, err := New(Deps{
		Snapshot:   func() *config.Snapshot { return e.snap },
		CodexDeps:  deps,
		Store:      store,
		Flows:      flows,
		ZStore:     zstore,
		ZFlows:     zflows,
		ZChain:     zchain,
		ZcodeDeps:  zdeps,
		Accounting: acct,
		ErrorsLog:  elog,
		Quotas:     qdb,
		StartedAt:  testNow.Add(-6 * time.Hour), // uptime детерминирован тестовой точкой времени
		BaseCtx:    ctx,
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	e.ui = ui
	return e
}

// testNow — фиксированная точка времени: все «человеческие» значения детерминированы.
var testNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// get — запрос к webui с опциональным Bearer.
func (e *env) get(path, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ui.ServeHTTP(rec, req)
	return rec
}

// post — POST c JSON-телом.
func (e *env) post(path, token, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ui.ServeHTTP(rec, req)
	return rec
}

// decode — разобрать JSON-ответ.
func decode(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode ответа %d: %v: %s", rec.Code, err, rec.Body.String())
	}
	return v
}

// shape — каноническая структура JSON: map(key:shape,...) по сортированным
// ключам, arr(shapes…), листья — str/num/bool/null. Golden-сравнение форм
// с литералами mock.js (имена полей, вложенность — значения игнорируются).
func shape(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortStrings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+shape(x[k]))
		}
		return "map(" + strings.Join(parts, ",") + ")"
	case []any:
		if len(x) == 0 {
			return "arr()"
		}
		// Массивы объектов гомогенны (форма mock.js): сравниваем ПЕРВЫЙ
		// элемент — состав строк уже проверен golden'ом; массивы скаляров
		// (дни графика) сравниваем целиком — там важны типы по позициям.
		allScalars := true
		for _, el := range x {
			switch el.(type) {
			case map[string]any, []any:
				allScalars = false
			}
		}
		if allScalars {
			parts := make([]string, 0, len(x))
			for _, el := range x {
				parts = append(parts, shape(el))
			}
			return "arr(" + strings.Join(parts, ",") + ")"
		}
		return "arr(" + shape(x[0]) + ")"
	case string:
		return "str"
	case float64:
		return "num"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "any"
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// checkShape — сверка формы ответа с golden-файлом testdata (порт литерала mock.js).
func checkShape(t *testing.T, rec *httptest.ResponseRecorder, golden string) {
	t.Helper()
	wantRaw, err := os.ReadFile(filepath.Join("testdata", golden))
	if err != nil {
		t.Fatalf("golden %s: %v", golden, err)
	}
	var want any
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("golden %s не парсится: %v", golden, err)
	}
	got := shape(decode(t, rec))
	w := shape(want)
	if got != w {
		t.Fatalf("форма ответа %s расходится с %s (mock.js):\n got: %s\nwant: %s", golden, golden, got, w)
	}
}

// insertRequest / insertError / insertSnapshot — наполнение БД.
// ModelInfo строится по имени модели (экономика тестовых моделей = SPEC 7.2:
// glm-5.3 6.9/1.7/24; gpt-6.1-terra — подписка); InsertRequest сам пересчитывает
// cost/credits, передавать их вручную не нужно.
func insertRequest(t *testing.T, d *accounting.DB, ts time.Time, client, alias, provider, account, model string, prompt, cached, compl int64) {
	t.Helper()
	m := &accounting.ModelInfo{Subscription: model == "gpt-6.1-terra"}
	switch model {
	case "glm-5.3":
		m.GLMIn, m.GLMCached, m.GLMOut = 6.9, 1.7, 24
	case "qwen3-coder": // PAYG-модель для cost_usd
		m.PriceIn, m.PriceCached, m.PriceOut = 0.15, 0.01, 2.0
	}
	err := d.InsertRequest(accounting.RequestRow{
		Ts: ts, ClientKey: client, Alias: alias, Provider: provider, Account: account,
		Model: model, Protocol: "openai",
		Prompt: int(prompt), CachedInput: int(cached), Completion: int(compl),
		Status: 200,
	}, m)
	if err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
}

func insertError(t *testing.T, d *errorslog.DB, ts time.Time, stage, alias, errText string) {
	t.Helper()
	err := d.InsertError(errorslog.ErrorRow{
		Ts: ts, ClientKey: "opencode", Alias: alias, Provider: "glm", Account: "g1",
		Stage: stage, Status: 429,
		Method: "POST", URL: "https://chatgpt.com/backend-api/codex/responses",
		ReqHeaders: []byte(`{"Authorization":"<masked>"}`), ReqBody: []byte(`{"model":"x"}`),
		RespHeaders: []byte(`{"content-type":"application/json"}`), RespBody: []byte(`{"error":"usage_limit_reached"}`),
		Err: errText,
	})
	if err != nil {
		t.Fatalf("InsertError: %v", err)
	}
}

func insertSnapshot(t *testing.T, d *quotas.DB, ts time.Time, provider, account, kind string, pct float64, window int64, hasWindow bool, reset int64, hasReset bool) {
	t.Helper()
	err := d.Insert(quotas.SnapshotRow{
		Ts: ts, Provider: provider, Account: account, Kind: kind,
		UsedPercent: pct, WindowMinutes: window, HasWindow: hasWindow,
		ResetAt: reset, HasReset: hasReset,
	})
	if err != nil {
		t.Fatalf("Insert snapshot: %v", err)
	}
}
