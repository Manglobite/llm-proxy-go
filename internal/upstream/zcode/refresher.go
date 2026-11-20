// Планировщик PAT/бизнес-JWT (SPEC 7.4, ARCHITECTURE §6.2/§9): горутина тикает
// раз в минуту; аккаунт, чей PAT или бизнес-JWT истекает раньше запаса
// (120s), прогоняется через EnsurePAT (единый PAT-кеш, single-flight —
// параллельные триггеры тикера/сендера/флоу ждут одну execution). Провал —
// только лог: transient повторится следующим тиком; needs_relogin-аккаунты
// пропускаются (интерактивный OAuth заново, долбить мёртвую цепочку незачем).
package zcode

import (
	"context"
	"log/slog"
	"time"
)

// Прод-параметры планировщика (SPEC 7.4): тик раз в минуту, пере-issue/ре-логин
// за 120s до expiry. Вынесены в константы, чтобы main и тесты ссылались на
// одни значения.
const (
	DefaultRefreshTick   = time.Minute
	DefaultRefreshBefore = 120 * time.Second
)

// Refresher — фоновый обновлятель PAT/бизнес-JWT. Store и deps инжектируются:
// deps замкнут в main на живой снапшот (клиент аккаунта меняется при релоаде
// прозрачным для планировщика образом).
type Refresher struct {
	store  *Store
	chain  *PATCache
	deps   Deps
	tick   time.Duration // интервал тика (прод 1m)
	before time.Duration // обновлять, если expiry - now < before (прод 120s)
}

// NewRefresher собирает планировщик; нулевые tick/before заменяются прод-дефолтами.
func NewRefresher(store *Store, chain *PATCache, deps Deps, tick, before time.Duration) *Refresher {
	if tick <= 0 {
		tick = DefaultRefreshTick
	}
	if before <= 0 {
		before = DefaultRefreshBefore
	}
	return &Refresher{store: store, chain: chain, deps: deps, tick: tick, before: before}
}

// Run — фоновый цикл: тик → RunOnce. Живёт до отмены ctx (graceful shutdown
// main; релоад планировщик не останавливает — Store и PATCache вне снапшотов).
func (r *Refresher) Run(ctx context.Context) {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce — один обход всех записей Store. Отдельный метод (а не тело цикла)
// для детерминированных тестов: канал-тики и time.Sleep не нужны, тест зовёт
// RunOnce напрямую.
func (r *Refresher) RunOnce(ctx context.Context) {
	now := time.Now()
	for _, key := range r.store.Keys() {
		if ctx.Err() != nil {
			return
		}
		rec, ok := r.store.Get(key)
		if !ok {
			continue // ключ удалён между Keys и Get — не падаем
		}
		if rec.ZaiAccessToken == "" {
			continue // OAuth ещё не выполнялся — обновлять нечего
		}
		if st := r.store.Status(key); st.State == StatusNeedsRelogin {
			continue // SPEC 7.4: интерактивный OAuth заново, тикер бессилен
		}
		// Триггеры: PAT отсутствует/истекает ИЛИ бизнес-JWT пуст/истекает —
		// обе беды чинит одна и та же цепочка (шаг 1 ре-логин, шаг 4 PAT).
		_, patExp, hasPAT := r.chain.PATInfo(key)
		patExpiring := !hasPAT || !now.Add(r.before).Before(patExp)
		bizExpiring := rec.BusinessJWT == "" || !now.Add(r.before).Before(rec.BusinessExpiresAt)
		if !patExpiring && !bizExpiring {
			continue // до истечения всего далеко
		}
		deps, err := r.deps(key)
		if err != nil {
			// Аккаунт исчез из конфига: ключ в Store остаётся (CF-7 — не
			// удаляем молча), просто нечего обновлять.
			slog.Warn("zcode refresh: аккаунт недоступен", "account", key, "err", err)
			continue
		}
		// EnsurePAT мутирует копию и персистит точечно через UpdateChainState —
		// свежие OAuth-токены записи не затираются.
		recCopy := rec
		pat, err := r.chain.EnsurePAT(ctx, deps, &recCopy)
		if err != nil {
			slog.Warn("zcode refresh: PAT-цепочка не удалась", "account", key, "err", err)
			continue
		}
		if _, exp, ok := r.chain.PATInfo(key); ok {
			slog.Info("zcode refresh: PAT готов", "account", key, "expires_at", exp.Format(time.RFC3339))
		} else {
			slog.Info("zcode refresh: PAT готов", "account", key, "len", len(pat))
		}
	}
}
