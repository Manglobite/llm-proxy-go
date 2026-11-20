// Точка входа прокси: загрузка конфига, сборка server (маршруты /v1/* и
// /healthz), учёт (SQLite), OAuth-рантайм codex (Store tokens.json + планировщик
// refresh), OAuth-рантайм zcode (zcode-tokens.json + PAT-кеш + тикер PAT +
// poll-флоу, SPEC 7.4), поллеры usage ollama, SIGHUP-релоад, graceful shutdown
// (SPEC 3.3). Порядок остановки: server.Shutdown → закрыть БД (WAL checkpoint)
// → manager.Stop → exit 0.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"llm-proxy/internal/accounting"
	"llm-proxy/internal/config"
	"llm-proxy/internal/errorslog"
	"llm-proxy/internal/quotas"
	"llm-proxy/internal/server"
	"llm-proxy/internal/socks"
	"llm-proxy/internal/upstream/codex"
	"llm-proxy/internal/upstream/glm"
	"llm-proxy/internal/upstream/openailike"
	"llm-proxy/internal/upstream/zcode"
	"llm-proxy/internal/webui"
)

func main() {
	configPath := flag.String("config", "./config.json", "путь к config.json")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// SIGTERM/SIGINT → graceful shutdown (SPEC 3.3); SIGHUP обрабатывается отдельно ниже.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()

	// Невалидный конфиг — fatal до старта сервера: лог + exit 1 (без паники).
	mgr, err := config.NewManager(rootCtx, *configPath)
	if err != nil {
		logger.Error("загрузка конфига не удалась", "err", err)
		os.Exit(1)
	}
	snap := mgr.Snapshot()

	// SQLite: один файл (storage.db_path), таблицы requests, errors и
	// quota_snapshots. Ошибка открытия — фатальна: без учёта прокси теряет
	// главный контракт. accounting открывается ПЕРВЫМ: он накатывает схему
	// requests, которую quotas читает (GLMBalance).
	acct, err := accounting.Open(snap.Cfg.Storage.DBPath)
	if err != nil {
		logger.Error("открытие БД учёта не удалось", "path", snap.Cfg.Storage.DBPath, "err", err)
		os.Exit(1)
	}
	elog, err := errorslog.Open(snap.Cfg.Storage.DBPath)
	if err != nil {
		logger.Error("открытие БД ошибок не удалось", "path", snap.Cfg.Storage.DBPath, "err", err)
		acct.Close()
		os.Exit(1)
	}
	qdb, err := quotas.Open(snap.Cfg.Storage.DBPath)
	if err != nil {
		logger.Error("открытие БД квот не удалось", "path", snap.Cfg.Storage.DBPath, "err", err)
		acct.Close()
		elog.Close()
		os.Exit(1)
	}

	// tokens.json: Store открывается РОВНО ОДИН раз и живёт до остановки
	// процесса. SIGHUP-релоад Store НЕ трогает и НЕ перечитывает (CF-7:
	// мёрж — это накопленная in-memory карта; ключи исчезнувших аккаунтов
	// не удаляются молча) — владелец файла только upstream/codex.Store.
	store, err := codex.OpenStore(snap.Cfg.Storage.TokensPath)
	if err != nil {
		logger.Error("чтение tokens.json не удалось", "path", snap.Cfg.Storage.TokensPath, "err", err)
		acct.Close()
		elog.Close()
		qdb.Close()
		os.Exit(1)
	}

	// zcode-tokens.json: Store открывается ОДИН раз и живёт до остановки
	// процесса; владелец файла — upstream/zcode.Store (SPEC 3.2а). SIGHUP-мёрж
	// — накопленная in-memory карта, как у tokens.json (CF-7).
	zstore, err := zcode.OpenStore(snap.Cfg.Storage.ZcodeTokensPath)
	if err != nil {
		logger.Error("чтение zcode-tokens.json не удалось", "path", snap.Cfg.Storage.ZcodeTokensPath, "err", err)
		acct.Close()
		elog.Close()
		qdb.Close()
		os.Exit(1)
	}

	// codexDeps — резолв клиента аккаунта (его SOCKS с gate, OA-3) и origin
	// OAuth-сервера по ключу "provider/account". Замкнут на ЖИВОЙ снапшот:
	// релоад подменяет пути/origin'ы, зависимости не пересоздаются. Общий для
	// планировщика refresh и (Ф4) OAuth-флоу.
	codexDeps := func(accountKey string) (codex.AcctDeps, error) {
		s := mgr.Snapshot()
		pname, accID, ok := strings.Cut(accountKey, "/")
		if !ok || pname == "" || accID == "" {
			return codex.AcctDeps{}, fmt.Errorf("ключ аккаунта %q не вида provider/account", accountKey)
		}
		prov, ok := s.Cfg.Providers[pname]
		if !ok || prov.Codex == nil {
			return codex.AcctDeps{}, fmt.Errorf("codex-провайдер %q не найден в снапшоте", pname)
		}
		var acc *config.CodexAccount
		for i := range prov.Codex.Accounts {
			if prov.Codex.Accounts[i].ID == accID {
				acc = &prov.Codex.Accounts[i]
				break
			}
		}
		if acc == nil {
			return codex.AcctDeps{}, fmt.Errorf("аккаунт %q не найден у провайдера %q", accID, pname)
		}
		tr, err := transporterFor(s, acc.Socks)
		if err != nil {
			return codex.AcctDeps{}, err
		}
		return codex.AcctDeps{Client: tr.Client(), AuthBase: prov.Codex.AuthBaseURL}, nil
	}

	// Планировщик refresh (SPEC 3.2/ARCHITECTURE §9): тик раз в минуту,
	// refresh за 5 минут до expires_at. Живёт до отмены rootCtx (сигнал
	// остановки); релоад его не останавливает — Store вне снапшотов.
	refresher := codex.NewRefresher(store, codexDeps, codex.DefaultRefreshTick, codex.DefaultRefreshBefore)
	go refresher.Run(rootCtx)

	// OAuth-флоу WebUI (Ф4, SPEC 12): тот же codexDeps (обмен кода через
	// клиент аккаунта) и Store. TTL — дефолт 15 минут SPEC 12.
	flows := codex.NewFlows(store, 0, codexDeps)

	// zcodeDeps — резолв зависимостей zcode-аккаунта (SOCKS-клиент, origin'ы,
	// версия клиента) по ключу записи. Ключ — literal "zcode/<account_id>"
	// (SPEC 3.2а, не имя провайдера), поэтому ищем ЛЮБОГО провайдера типа
	// zcode с таким аккаунтом; перебор по отсортированным именам — детерминизм.
	zcodeDeps := func(accountKey string) (zcode.AcctDeps, error) {
		s := mgr.Snapshot()
		_, accID, ok := strings.Cut(accountKey, "/")
		if !ok || accID == "" {
			return zcode.AcctDeps{}, fmt.Errorf("ключ аккаунта %q не вида zcode/account", accountKey)
		}
		names := make([]string, 0, len(s.Cfg.Providers))
		for n := range s.Cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := s.Cfg.Providers[n]
			if p.ZCode == nil {
				continue
			}
			for i := range p.ZCode.Accounts {
				if p.ZCode.Accounts[i].ID != accID {
					continue
				}
				tr, err := transporterFor(s, p.ZCode.Accounts[i].Socks)
				if err != nil {
					return zcode.AcctDeps{}, err
				}
				return zcode.AcctDeps{
					AccountKey:    accountKey,
					Client:        tr.Client(),
					Base:          p.ZCode.BaseURL,
					ZcodeBase:     p.ZCode.ZCodeBaseURL,
					ClientVersion: p.ZCode.ClientVersion,
				}, nil
			}
		}
		return zcode.AcctDeps{}, fmt.Errorf("zcode-аккаунт %q не найден в снапшоте", accID)
	}

	// Единый PAT-кеш (SPEC 7.4: общий для тикера, sender'а и финализации
	// OAuth-флоу, single-flight на аккаунт) и рантайм OAuth-флоу zcode
	// (горутины-поллеры живут на rootCtx — переживают запросы и SIGHUP).
	// Start флоу зовут эндпоинты WebUI /api/oauth/zcode/* (Ф7b, SPEC 12).
	zchain := zcode.NewPATCache(zstore)
	zflows := zcode.NewFlows(zstore, zchain, rootCtx)

	// Планировщик zcode (SPEC 7.4/ARCHITECTURE §9): тик раз в минуту, PAT
	// ре-issue и ре-логин бизнес-JWT за 120s до expiry. Живёт до отмены
	// rootCtx; релоад его не останавливает — Store/PATCache вне снапшотов.
	zrefresher := zcode.NewRefresher(zstore, zchain, zcodeDeps, 0, 0)
	go zrefresher.Run(rootCtx)

	// Статус конфига для meta экрана ①: время последнего успешного SIGHUP.
	startedAt := time.Now()
	var lastReload atomic.Int64
	reloadNote := func() string {
		if t := lastReload.Load(); t > 0 {
			return "ok · reload " + time.Unix(t, 0).Format("15:04")
		}
		return "ok"
	}

	// Поллеры usage ollama (SPEC 7.3/11) и программных квот zcode (Ф8, SPEC 15):
	// по горутине на цель с включённым usage_poll. Контекст — КОНТЕКСТ СНАПШОТА:
	// релоад отменяет старые поллеры и стартует новые (изменившийся usage_poll
	// подхватится именно так). startQuotaPollers идемпотентен на живом снапшоте.
	startQuotaPollers := func() {
		s := mgr.Snapshot()
		quotas.StartUsagePolls(s.Ctx(), qdb, usageTargets(s))
		quotas.StartZCodeUsagePolls(s.Ctx(), qdb, zcodeUsageTargets(s, zstore, zchain, zcodeDeps))
	}
	startQuotaPollers()
	var configMu sync.Mutex
	applyConfig := func(change func() error) error {
		configMu.Lock()
		defer configMu.Unlock()
		if err := change(); err != nil {
			return err
		}
		lastReload.Store(time.Now().Unix())
		startQuotaPollers()
		return nil
	}
	wrapAdd := func(add func(string, string, json.RawMessage) error) func(string, string, json.RawMessage) error {
		return func(token, name string, data json.RawMessage) error {
			return applyConfig(func() error { return add(token, name, data) })
		}
	}

	// SIGHUP — хот-релоад: ошибка валидации нового конфига не трогает живой
	// снапшот, предыдущая конфигурация продолжает работать (CF-1, SPEC 3.3).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := applyConfig(func() error { return mgr.Reload(*configPath) }); err != nil {
				slog.Warn("релоад конфига не удался, работает предыдущая конфигурация", "err", err)
				continue
			}
			slog.Info("конфиг перезагружен", "path", *configPath)
		}
	}()

	// WebUI (Ф4, SPEC 12): статика + JSON API /api/* + OAuth-callback-listener.
	// Ошибка сборки — фатальна: конфигурация обещает WebUI, частично без него
	// не собираемся (oauth-рантайм без экрана вводит пользователя в заблуждение).
	ui, err := webui.New(webui.Deps{
		Snapshot:    mgr.Snapshot,
		AddProvider: wrapAdd(mgr.AddProvider),
		AddAlias:    wrapAdd(mgr.AddAlias),
		AddClient: func(token, name string) (key string, err error) {
			err = applyConfig(func() error {
				key, err = mgr.AddClient(token, name)
				return err
			})
			return key, err
		},
		AddSocks:    wrapAdd(mgr.AddSocks),
		UpdateSocks: wrapAdd(mgr.UpdateSocks),
		CodexDeps:   codexDeps,
		Store:       store,
		Flows:       flows,
		ZStore:      zstore,
		ZFlows:      zflows,
		ZChain:      zchain,
		ZcodeDeps:   zcodeDeps,
		Accounting:  acct,
		ErrorsLog:   elog,
		Quotas:      qdb,
		StartedAt:   startedAt,
		BaseCtx:     rootCtx,
		ConfigNote:  reloadNote,
	})
	if err != nil {
		logger.Error("сборка WebUI не удалась", "err", err)
		acct.Close()
		elog.Close()
		qdb.Close()
		os.Exit(1)
	}

	srvAPI, err := server.New(server.Params{
		Snapshot: mgr.Snapshot,
		// Модули провайдеров по типу (SPEC 7): сервер выбирает по
		// Target.ProviderType. codex подключён с Ф3 — сервер не меняется
		// (интерфейс объявлен сервером).
		Senders: map[string]server.Sender{
			config.ProviderOpenAILike: openailike.Sender{},
			config.ProviderGLM:        glm.Sender{},
			config.ProviderCodex:      codex.Sender{Store: store},
			config.ProviderZCode:      zcode.Sender{Store: zstore, Chain: zchain},
		},
		Accounting: acct,
		ErrorsLog:  elog,
		Quotas:     qdb,
		WebUI:      ui,
		// Значения ниже — ФОЛБЭК: живые таймауты сервер берёт из ТЕКУЩЕГО
		// снапшота на каждом запросе, поэтому SIGHUP-релоад обновляет их без
		// перезапуска (ARCHITECTURE §7 «релоадится: таймауты»).
		StreamIdleTimeout: time.Duration(snap.Cfg.Server.StreamIdleTimeout),
		ConnectTimeout:    time.Duration(snap.Cfg.Server.UpstreamConnectTimeout),
	})
	if err != nil {
		logger.Error("сборка сервера не удалась", "err", err)
		acct.Close()
		elog.Close()
		qdb.Close()
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    snap.Cfg.Server.Listen,
		Handler: srvAPI.Handler(),
		// Дефолт нуля недопустим: без ReadHeaderTimeout соединение можно
		// держать открытым вечно, не присылая заголовков.
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("прокси слушает", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-rootCtx.Done():
		slog.Info("получен сигнал остановки")
	case err := <-serveErr:
		// ErrServerClosed до сигнала попасть не может (Shutdown ещё не звали),
		// значит это реальная ошибка старта (например, занятый порт).
		logger.Error("сервер остановился с ошибкой", "err", err)
		acct.Close()
		elog.Close()
		qdb.Close()
		os.Exit(1)
	}

	// 1. Graceful: перестать принимать, дозавершить in-flight запросы/стримы
	// (мягкий таймаут из конфига, SPEC 3.3).
	shutdownTimeout := mgr.Snapshot().Cfg.Server.ShutdownTimeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(shutdownTimeout))
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown не уложился в таймаут, соединения разорваны", "err", err)
	}
	// ListenAndServe завершается следом за закрытием слушателя; отличная от
	// ErrServerClosed ошибка здесь — только реальная проблема, логируем её.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("неожиданная ошибка сервера при остановке", "err", err)
	}
	// 2. Закрыть БД с WAL-checkpoint TRUNCATE — рядом с proxy.db не должен
	// оставаться несобранный -wal (SPEC 3.3).
	if err := acct.Close(); err != nil {
		slog.Warn("закрытие БД учёта с ошибкой", "err", err)
	}
	if err := elog.Close(); err != nil {
		slog.Warn("закрытие БД ошибок с ошибкой", "err", err)
	}
	if err := qdb.Close(); err != nil {
		slog.Warn("закрытие БД квот с ошибкой", "err", err)
	}
	// 3. Остановить фоновые проверки SOCKS-путей и поллеры живого снапшота
	// (rootCtx уже отменён сигналом — планировщик refresh вышел сам).
	mgr.Stop()
	slog.Info("остановка завершена")
}

// transporterFor — клиент аккаунта по имени его SOCKS-пути ("none" → прямой
// путь; его легальность уже проверил config при загрузке, I5). Копия server-логики
// для фоновых зависимостей main: импортировать server нельзя (R5).
func transporterFor(snap *config.Snapshot, socksName string) (socks.Transporter, error) {
	if socksName == "none" {
		return socks.Direct{}, nil
	}
	p, ok := snap.Socks[socksName]
	if !ok {
		return nil, fmt.Errorf("socks-путь %s отсутствует в снапшоте (рассинхронизация конфигурации)", socksName)
	}
	return p, nil
}

// usageTargets — цели ollama-поллинга (SPEC 7.3): провайдеры openai-like с
// включённым usage_poll; клиент — ПЕРВОГО аккаунта провайдера (детерминизм,
// как у forward_paths).
func usageTargets(snap *config.Snapshot) []quotas.UsageTarget {
	var targets []quotas.UsageTarget
	for name, prov := range snap.Cfg.Providers {
		if prov.OpenAILike == nil || prov.OpenAILike.UsagePoll <= 0 || len(prov.OpenAILike.Accounts) == 0 {
			continue
		}
		acc := &prov.OpenAILike.Accounts[0]
		tr, err := transporterFor(snap, acc.Socks)
		if err != nil {
			// Аккаунт без валидного пути — провайдер пропускается: полл не
			// критичен для работы прокси, а конфиг уже прошёл валидацию.
			slog.Warn("usage poll: аккаунт без транспорта, провайдер пропущен", "provider", name, "err", err)
			continue
		}
		targets = append(targets, quotas.UsageTarget{
			Provider:    name,
			Account:     acc.ID,
			BaseURL:     strings.TrimRight(prov.OpenAILike.BaseURL, "/"),
			APIKey:      acc.Auth.Key,
			Transporter: tr,
			Interval:    time.Duration(prov.OpenAILike.UsagePoll),
		})
	}
	return targets
}

// zcodeUsageTargets — цели поллинга программных квот z.ai (Ф8, SPEC 15):
// аккаунты zcode-провайдеров с включённым usage_poll (цель — АККАУНТ, не
// провайдер: PAT у каждого свой). PATOf замкнут на единый PAT-кеш (EnsurePAT,
// single-flight общий с тикером/сендером/флоу) и живой снапшот через zdeps;
// отсутствие записи в Store — тихий skip поллера (OAuth ещё не выполнялся).
func zcodeUsageTargets(snap *config.Snapshot, zstore *zcode.Store, zchain *zcode.PATCache, zdeps zcode.Deps) []quotas.ZCodeUsageTarget {
	var targets []quotas.ZCodeUsageTarget
	for name, prov := range snap.Cfg.Providers {
		if prov.ZCode == nil || prov.ZCode.UsagePoll <= 0 {
			continue
		}
		for i := range prov.ZCode.Accounts {
			acc := &prov.ZCode.Accounts[i]
			tr, err := transporterFor(snap, acc.Socks)
			if err != nil {
				// Конфиг уже прошёл валидацию: пропущенный аккаунт — не фатально.
				slog.Warn("zcode usage poll: аккаунт без транспорта, пропущен", "provider", name, "account", acc.ID, "err", err)
				continue
			}
			key := zcode.AccountKey(acc.ID)
			targets = append(targets, quotas.ZCodeUsageTarget{
				Provider:    name,
				Account:     acc.ID,
				BaseURL:     strings.TrimRight(prov.ZCode.BaseURL, "/"),
				Transporter: tr,
				Interval:    time.Duration(prov.ZCode.UsagePoll),
				PATOf: func(ctx context.Context) (string, error) {
					rec, ok := zstore.Get(key)
					if !ok {
						return "", nil // записи нет — тихий skip, не ошибка
					}
					deps, err := zdeps(key)
					if err != nil {
						return "", err
					}
					// EnsurePAT мутирует копию записи и персистит точечно через
					// UpdateChainState — свежие OAuth-токены не затираются.
					return zchain.EnsurePAT(ctx, deps, &rec)
				},
			})
		}
	}
	return targets
}
