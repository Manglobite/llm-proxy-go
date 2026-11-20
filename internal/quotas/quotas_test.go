package quotas

// Тесты квот (SPEC 11): схема/NULL-семантика, конвертация upstream.Quotas,
// glm-окна 5ч/неделя на граничных ts (READ из requests той же БД),
// ollama-поллинг на стабе (интервалы — параметры, без time.Sleep).

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream"
)

func openQuotas(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("quotas.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestInsertSelectBackWithNulls(t *testing.T) {
	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	rows := []SnapshotRow{
		{Ts: now, Provider: "codex", Account: "a1", Kind: "codex_primary",
			UsedPercent: 42.5, WindowMinutes: 300, HasWindow: true, ResetAt: 1730003600, HasReset: true},
		{Ts: now, Provider: "codex", Account: "a1", Kind: "codex_weekly",
			UsedPercent: 12, WindowMinutes: 10080, HasWindow: true},
		{Ts: now, Provider: "codex", Account: "a1", Kind: "codex_limited",
			UsedPercent: 100, ResetAt: 1730009999, HasReset: true},
		{Ts: now, Provider: "ollama", Account: "cloud", Kind: "ollama_usage", UsedPercent: 1.25},
	}
	for _, r := range rows {
		if err := db.Insert(r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	type row struct {
		kind   string
		used   float64
		window any
		reset  any
	}
	var got []row
	rset, err := db.db.Query(`SELECT kind, used_percent, window_minutes, reset_at
		FROM quota_snapshots ORDER BY id`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rset.Close()
	for rset.Next() {
		var g row
		if err := rset.Scan(&g.kind, &g.used, &g.window, &g.reset); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, g)
	}
	if len(got) != 4 {
		t.Fatalf("строк = %d", len(got))
	}
	// NULL-семантика: не сообщённое — NULL, а не 0.
	if got[1].reset != nil || got[3].window != nil || got[3].reset != nil {
		t.Fatalf("NULL-поля записаны нулями: %+v", got)
	}
	if got[0].window.(int64) != 300 || got[0].reset.(int64) != 1730003600 {
		t.Fatalf("primary = %+v", got[0])
	}
	if got[2].used != 100 || got[2].reset.(int64) != 1730009999 {
		t.Fatalf("limited = %+v", got[2])
	}
	if got[3].used != 1.25 {
		t.Fatalf("ollama_usage = %+v", got[3])
	}
}

// F1/P1: Recent отдаёт ПОСЛЕДНИЕ (свежие) снапшоты в хронологическом
// порядке, а не первые строки таблицы — SPEC 11 обещает «текущие проценты».
func TestRecentReturnsLatestRowsChronologically(t *testing.T) {
	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		if err := db.Insert(SnapshotRow{
			Ts: now, Provider: "codex", Account: "a1", Kind: "codex_primary",
			UsedPercent: float64(i) * 10,
		}); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	recent, err := db.Recent(3)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("строк = %d, ожидалось 3", len(recent))
	}
	// Строки 3,4,5 (id по возрастанию), НЕ 1,2,3; последняя — с отличимым
	// used_percent=50 (вставлена пятой).
	want := []float64{30, 40, 50}
	for i, r := range recent {
		if r.UsedPercent != want[i] {
			t.Fatalf("Recent[%d].UsedPercent = %v, ожидалось %v (хвост таблицы)", i, r.UsedPercent, want[i])
		}
	}
}

func TestFromUpstreamMapsWindowsAndLimit(t *testing.T) {
	now := time.Now()
	q := &upstream.Quotas{
		Windows: []upstream.QuotaWindow{
			{Kind: upstream.QuotaCodexPrimary, UsedPercent: 42.5, WindowMinutes: 300, ResetAtUnix: 1},
			{Kind: upstream.QuotaCodexWeekly, UsedPercent: 12, WindowMinutes: 10080},
		},
		Limited: &upstream.QuotaLimit{ResetAtUnix: 1730009999},
	}
	rows := FromUpstream("codex", "a1", q, now)
	if len(rows) != 3 {
		t.Fatalf("строк = %d", len(rows))
	}
	if rows[0].Kind != "codex_primary" || rows[0].Provider != "codex" || rows[0].Account != "a1" || !rows[0].HasReset {
		t.Fatalf("rows[0] = %+v", rows[0])
	}
	if rows[1].Kind != "codex_weekly" || rows[1].HasReset {
		t.Fatalf("rows[1] = %+v", rows[1])
	}
	// 429 → codex_limited, used_percent=100 (минимальное расширение Ф3).
	if rows[2].Kind != "codex_limited" || rows[2].UsedPercent != 100 || rows[2].ResetAt != 1730009999 {
		t.Fatalf("rows[2] = %+v", rows[2])
	}
	if FromUpstream("codex", "a1", nil, now) != nil {
		t.Fatal("nil-квоты должны давать nil")
	}
}

// F4/P3: окно только с used-percent (window_minutes апстрим не прислал) —
// в БД пишется NULL, а не 0 (0 был бы легитимным «нулевым окном»).
func TestFromUpstreamPartialWindowIsNullInDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.db")
	db := openQuotas(t, path)
	now := time.Now()
	rows := FromUpstream("codex", "a1", &upstream.Quotas{
		Windows: []upstream.QuotaWindow{
			{Kind: upstream.QuotaCodexPrimary, UsedPercent: 7}, // без WindowMinutes/ResetAt
		},
	}, now)
	if len(rows) != 1 {
		t.Fatalf("строк = %d", len(rows))
	}
	if rows[0].HasWindow || rows[0].HasReset {
		t.Fatalf("частичное окно помечено заполненным: %+v", rows[0])
	}
	if err := db.Insert(rows[0]); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	var window, reset any
	if err := db.db.QueryRow(`SELECT window_minutes, reset_at FROM quota_snapshots
		WHERE kind='codex_primary' ORDER BY id DESC LIMIT 1`).Scan(&window, &reset); err != nil {
		t.Fatalf("select: %v", err)
	}
	if window != nil || reset != nil {
		t.Fatalf("NULL-семантика нарушена: window=%v reset=%v", window, reset)
	}
}

// TestGLMBalanceWindows — скользящие окна 5ч/неделя по SUM(glm_credits) из
// requests (READ из той же SQLite-файла); граничные ts: ровно на границе —
// внутри окна (ts >= from), на секунду раньше границы — снаружи 5ч, но внутри
// недели.
func TestGLMBalanceWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.db")
	acct, err := accounting.Open(path)
	if err != nil {
		t.Fatalf("accounting.Open: %v", err)
	}
	t.Cleanup(func() { _ = acct.Close() })
	db := openQuotas(t, path)

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	mult := &accounting.ModelInfo{GLMIn: 6.9, GLMCached: 1.7, GLMOut: 24}
	insert := func(at time.Time, prompt, completion int) {
		t.Helper()
		if err := acct.InsertRequest(accounting.RequestRow{
			Ts: at, ClientKey: "opencode", Alias: "glm/test", Provider: "glm",
			Account: "plan-a", Model: "glm-5.3", Protocol: "openai",
			Prompt: prompt, Completion: completion,
		}, mult); err != nil {
			t.Fatalf("InsertRequest: %v", err)
		}
	}
	// Кредиты строки с prompt=10000/completion=10000: (10000*6.9+10000*24)/10000 = 30.9.
	// Только prompt=10000: 6.9.
	insert(now.Add(-1*time.Hour), 10000, 10000)           // внутри 5ч и недели → 30.9
	insert(now.Add(-5*time.Hour), 10000, 0)               // ровно граница 5ч → внутри (ts >= from) → 6.9
	insert(now.Add(-5*time.Hour-1*time.Second), 10000, 0) // вне 5ч, внутри недели → 6.9
	insert(now.Add(-8*24*time.Hour), 10000, 10000)        // вне обоих окон → 30.9

	used5h, usedWeek, percent5h, percentWeek, err := db.GLMBalance("plan-a", &GLMPlan{Window5h: 120, WindowWeekly: 600}, now)
	if err != nil {
		t.Fatalf("GLMBalance: %v", err)
	}
	if used5h != 37.8 {
		t.Fatalf("used5h = %v, ожидалось 37.8 (30.9 на границе включена)", used5h)
	}
	if usedWeek != 44.7 {
		t.Fatalf("usedWeek = %v, ожидалось 44.7", usedWeek)
	}
	// Сравнение с допуском: SUM по REAL-колонке копит погрешность float64.
	if math.Abs(percent5h-37.8/120*100) > 1e-9 || math.Abs(percentWeek-44.7/600*100) > 1e-9 {
		t.Fatalf("проценты = %v/%v, ожидалось %v/%v", percent5h, percentWeek, 37.8/120*100, 44.7/600*100)
	}
	// nil-план — проценты 0 (подпись остаётся удобной для Ф4).
	_, _, p5, pw, err := db.GLMBalance("plan-a", nil, now)
	if err != nil || p5 != 0 || pw != 0 {
		t.Fatalf("nil-план: %v %v %v", p5, pw, err)
	}
}

// ─── ollama usage poll ──────────────────────────────────────────────────────

func TestPollUsageOnceWritesSnapshot(t *testing.T) {
	var auth, query string
	var hits int
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		auth, query = r.Header.Get("Authorization"), r.URL.RawQuery
		mu.Unlock()
		// OA-10: только Bearer.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage_usd":1.25,"cached_input_tokens":77}`)
	}))
	t.Cleanup(stub.Close)

	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	tg := UsageTarget{
		Provider: "ollama", Account: "cloud",
		BaseURL: stub.URL, APIKey: "sk-ollama",
		Transporter: socks.Direct{}, Interval: time.Hour,
	}
	if err := db.PollUsageOnce(context.Background(), tg); err != nil {
		t.Fatalf("PollUsageOnce: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer sk-ollama" || query != "range=24h" {
		t.Fatalf("запрос: auth=%q query=%q", auth, query)
	}
	if hits != 1 {
		t.Fatalf("hits = %d", hits)
	}
	var kind string
	var used float64
	if err := db.db.QueryRow(`SELECT kind, used_percent FROM quota_snapshots ORDER BY id DESC LIMIT 1`).
		Scan(&kind, &used); err != nil {
		t.Fatalf("select: %v", err)
	}
	if kind != KindOllamaUsage || used != 1.25 {
		t.Fatalf("снапшот = %s %v", kind, used)
	}
}

func TestPollUsageLoopStopsOnCtxAndUsesInterval(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"usage_usd":0.5}`)
	}))
	t.Cleanup(stub.Close)

	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	tg := UsageTarget{
		Provider: "ollama", Account: "cloud",
		BaseURL: stub.URL, APIKey: "k", Transporter: socks.Direct{},
		Interval: 5 * time.Millisecond, // интервал — параметр (маленький в тесте)
	}
	// done-хук: тест ждёт канал, а не time.Sleep (CODING 1.9); отправки хука
	// защищены ctx в pollUsageLoop — после cancel горутина выходит.
	done := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pollUsageLoop(ctx, db, tg, done)
	}()

	<-done // первый полл сразу
	mu.Lock()
	n1 := hits
	mu.Unlock()
	<-done // тик по интервалу
	mu.Lock()
	n2 := hits
	mu.Unlock()
	if n1 < 1 || n2 <= n1 {
		t.Fatalf("полл не тикает по интервалу: %d → %d", n1, n2)
	}
	cancel()
	// Горутина обязана выйти по ctx.Done (CC-1) — wg.Wait без задержек.
	wg.Wait()
}

func TestPollUsageOnceErrorDoesNotInsert(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(stub.Close)
	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	tg := UsageTarget{Provider: "ollama", Account: "cloud", BaseURL: stub.URL, APIKey: "k", Transporter: socks.Direct{}}
	if err := db.PollUsageOnce(context.Background(), tg); err == nil {
		t.Fatal("не-2xx стаба обязан быть ошибкой")
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("снапшот при ошибке полла: n=%d err=%v", n, err)
	}
}

// ─── zcode usage poll (Ф8, SPEC 15) ─────────────────────────────────────────

// TestFindZCodePercentTolerantForms — толерантный парсер data: канон
// used_percent, альтернативные имена, пара used/limit, вложенность/массивы;
// мусор и used без limit процентом не признаются.
func TestFindZCodePercentTolerantForms(t *testing.T) {
	tests := []struct {
		name string
		data any
		want float64
		ok   bool
	}{
		{"канон used_percent", map[string]any{"used_percent": 42.5}, 42.5, true},
		{"usage_percent", map[string]any{"usage_percent": 7}, 7, true},
		{"percent", map[string]any{"percent": 3.5}, 3.5, true},
		{"строка с процентом", map[string]any{"used_percent": "42.5%"}, 42.5, true},
		{"приоритет used_percent над percent", map[string]any{"used_percent": 1, "percent": 99}, 1, true},
		{"пара used/limit", map[string]any{"used": 30, "limit": 120}, 25, true},
		{"пара used/limit с нулевым limit", map[string]any{"used": 30, "limit": 0}, 0, false},
		{"вложенный объект", map[string]any{"quota": map[string]any{"percent": 9}}, 9, true},
		{"массив объектов", []any{map[string]any{"foo": 1}, map[string]any{"used_percent": 11}}, 11, true},
		{"мусор", map[string]any{"foo": "bar"}, 0, false},
		{"used без limit — не процент", map[string]any{"used": 30}, 0, false},
		{"пустой data", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := findZCodePercent(tt.data)
			if ok != tt.ok || (ok && math.Abs(got-tt.want) > 1e-9) {
				t.Fatalf("findZCodePercent = %v,%v; ожидалось %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestPollZCodeUsageOnceWritesSnapshot — поллер пишет строку kind zcode_usage
// с распознанным процентом; PAT уходит Bearer-заголовком.
func TestPollZCodeUsageOnceWritesSnapshot(t *testing.T) {
	var mu sync.Mutex
	var auth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path != "/api/monitor/usage/quota/limit" {
			t.Errorf("неожиданный путь %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"used_percent":42.5}}`)
	}))
	t.Cleanup(stub.Close)

	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	tg := ZCodeUsageTarget{
		Provider: "zai-plan", Account: "main", BaseURL: stub.URL,
		Transporter: socks.Direct{},
		PATOf:       func(context.Context) (string, error) { return "pat-z", nil },
	}
	if err := db.PollZCodeUsageOnce(context.Background(), tg); err != nil {
		t.Fatalf("PollZCodeUsageOnce: %v", err)
	}
	mu.Lock()
	if auth != "Bearer pat-z" {
		t.Fatalf("auth = %q", auth)
	}
	mu.Unlock()
	var kind string
	var used float64
	if err := db.db.QueryRow(`SELECT kind, used_percent FROM quota_snapshots ORDER BY id DESC LIMIT 1`).
		Scan(&kind, &used); err != nil {
		t.Fatalf("select: %v", err)
	}
	if kind != KindZCodeUsage || math.Abs(used-42.5) > 1e-9 {
		t.Fatalf("снапшот = %s %v", kind, used)
	}
}

// TestPollZCodeUsageOnceSkipsSilentlyWithoutPAT — записи аккаунта в Store нет:
// тихий skip (nil), ни запроса к апстриму, ни строки в БД.
func TestPollZCodeUsageOnceSkipsSilentlyWithoutPAT(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"code":0,"data":{"used_percent":42.5}}`)
	}))
	t.Cleanup(stub.Close)
	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	tg := ZCodeUsageTarget{
		Provider: "zai-plan", Account: "main", BaseURL: stub.URL, Transporter: socks.Direct{},
		PATOf: func(context.Context) (string, error) { return "", nil },
	}
	if err := db.PollZCodeUsageOnce(context.Background(), tg); err != nil {
		t.Fatalf("skip без PAT — не ошибка: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("стаб получил %d запросов, ожидалось 0 (skip до сети)", hits)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("снапшот при skip: n=%d err=%v", n, err)
	}
}

// TestPollZCodeUsageOnceGarbageWritesNothing — мусор вместо квоты: строку НЕ
// пишем (0% выглядел бы как «всё свободно»), ошибка уходит в warn раз в тик.
func TestPollZCodeUsageOnceGarbageWritesNothing(t *testing.T) {
	for name, body := range map[string]string{
		"нераспознанный data": `{"code":0,"data":{"unexpected":{"nested":true}}}`,
		"бизнес-код не 0":     `{"code":1002,"msg":"invalid","data":{"used_percent":42.5}}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(stub.Close)
			db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
			tg := ZCodeUsageTarget{
				Provider: "zai-plan", Account: "main", BaseURL: stub.URL, Transporter: socks.Direct{},
				PATOf: func(context.Context) (string, error) { return "pat-z", nil },
			}
			if err := db.PollZCodeUsageOnce(context.Background(), tg); err == nil {
				t.Fatal("ответ обязан быть ошибкой (строку не пишем)")
			}
			var n int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("мусор попал в БД: n=%d err=%v", n, err)
			}
		})
	}
}

// TestPollZCodeUsageLoopWritesAndStopsOnCtx — цикл поллера: первый снапшот
// сразу, далее по тикеру; выход по cancel без зависаний (CC-1).
func TestPollZCodeUsageLoopWritesAndStopsOnCtx(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"used_percent":42.5}}`)
	}))
	t.Cleanup(stub.Close)

	db := openQuotas(t, filepath.Join(t.TempDir(), "proxy.db"))
	tg := ZCodeUsageTarget{
		Provider: "zai-plan", Account: "main", BaseURL: stub.URL, Transporter: socks.Direct{},
		Interval: 5 * time.Millisecond, // интервал — параметр (маленький в тесте)
		PATOf:    func(context.Context) (string, error) { return "pat-z", nil },
	}
	done := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pollZCodeUsageLoop(ctx, db, tg, done)
	}()

	<-done // первый полл сразу
	<-done // тик по интервалу
	cancel()
	wg.Wait() // горутина обязана выйти по ctx.Done (CODING 1.3/1.9)

	mu.Lock()
	defer mu.Unlock()
	if len(auths) < 2 || auths[0] != "Bearer pat-z" {
		t.Fatalf("запросы к стабу: %v", auths)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots WHERE kind=?`, KindZCodeUsage).Scan(&n); err != nil || n < 2 {
		t.Fatalf("снапшотов kind=zcode_usage = %d (err=%v), ожидалось >=2", n, err)
	}
}
