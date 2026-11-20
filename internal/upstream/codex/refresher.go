// Планировщик refresh (ARCHITECTURE §6, SPEC 3.2): горутина тикает раз в
// минуту и обновляет токены аккаунтов, до истечения которых осталось меньше
// 5 минут. Провал refresh — только лог: следующий тик повторит попытку,
// а 401 от апстрима дополнительно страхует прямым refresh-и-retry (Sender).
package codex

import (
	"context"
	"log/slog"
	"time"
)

// Прод-параметры планировщика (SPEC 3.2): тик раз в минуту, refresh за
// 5 минут до expires_at. Вынесены в константы, чтобы main и тесты ссылались
// на одни значения.
const (
	DefaultRefreshTick   = time.Minute
	DefaultRefreshBefore = 5 * time.Minute
)

// Refresher — фоновый обновлятор токенов. Store и deps инжектируются:
// deps замкнут в main на живой снапшот (клиент аккаунта меняется при релоаде
// прозрачным для планировщика образом).
type Refresher struct {
	store  *Store
	deps   Deps
	tick   time.Duration // интервал тика (прод 1m)
	before time.Duration // обновлять, если expires_at - now < before (прод 5m)
}

// NewRefresher собирает планировщик; нулевые tick/before заменяются прод-дефолтами.
func NewRefresher(store *Store, deps Deps, tick, before time.Duration) *Refresher {
	if tick <= 0 {
		tick = DefaultRefreshTick
	}
	if before <= 0 {
		before = DefaultRefreshBefore
	}
	return &Refresher{store: store, deps: deps, tick: tick, before: before}
}

// Run — фоновый цикл: тик → RunOnce. Живёт до отмены ctx (graceful shutdown
// main; релоад планировщик не останавливает — Store вне снапшотов).
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

// RunOnce — один обход всех токенов Store. Отдельный метод (а не тело цикла)
// для детерминированных тестов: канал-тики и time.Sleep не нужны, тест зовёт
// RunOnce напрямую.
func (r *Refresher) RunOnce(ctx context.Context) {
	for _, key := range r.store.Keys() {
		if ctx.Err() != nil {
			return
		}
		ts, ok := r.store.Get(key)
		if !ok {
			continue // ключ удалён между Keys и Get — не падаем
		}
		if until := time.Until(ts.ExpiresAt); until >= r.before {
			continue // до истечения далеко — refresh не нужен
		}
		deps, err := r.deps(key)
		if err != nil {
			// Аккаунт исчез из конфига: ключ в Store остаётся (CF-7 — не
			// удаляем молча), просто нечего обновлять.
			slog.Warn("codex refresh: аккаунт недоступен", "account", key, "err", err)
			continue
		}
		fresh, err := Refresh(ctx, deps.Client, deps.AuthBase, ts.RefreshToken)
		if err != nil {
			// Провал refresh не валит планировщик (CODING 1.3): Warn и следующий
			// тик повторит; пока токен жив — запросы идут старым access-токеном.
			slog.Warn("codex refresh: обновление токена не удалось", "account", key, "err", err)
			continue
		}
		if err := r.store.Set(key, *fresh); err != nil {
			// Новые токены получены, но файл не записан: in-memory уже обновлён
			// (Set пишет карту до файла), в файле останутся старые — при рестарте
			// refresh_token старой записи ещё может сработать. Не молчим.
			slog.Warn("codex refresh: запись tokens.json не удалась", "account", key, "err", err)
		}
		slog.Info("codex refresh: токен обновлён", "account", key, "expires_at", fresh.ExpiresAt.Format(time.RFC3339))
	}
}
