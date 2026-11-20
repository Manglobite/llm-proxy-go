// Дополнение флоу-рантайма для WebUI (Ф4, SPEC 12): снимок активных флоу для
// GET /api/oauth/pending и завершение callback-URL БЕЗ известного аккаунта —
// локальный callback-listener видит из запроса только state (браузер
// редиректится auth-сервером, аккаунт в URL не передаётся). Логика завершения
// переиспользуется из Complete (тот же обмен/валидации, ноль дублирования).
package codex

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// PendingFlow — активный флоу для WebUI. Verifier сознательно не отдаётся
// (SPEC 12: «без verifier» — PKCE-секрет остаётся в памяти флоу).
type PendingFlow struct {
	Account   string    `json:"account"`
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"` // authorize-ссылка флоу
	ExpiresAt time.Time `json:"expires_at"`
}

// Pending — снимок активных флоу (все аккаунты). Порядок не специфицирован;
// webui сортирует сам.
func (f *Flows) Pending() []PendingFlow {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked() // истёкшие не показываем — они не примут коллбэк
	out := make([]PendingFlow, 0, len(f.byState))
	for state, flw := range f.byState {
		out = append(out, PendingFlow{
			Account:   flw.accountKey,
			State:     state,
			URL:       flw.authorizeURL,
			ExpiresAt: flw.expiresAt,
		})
	}
	return out
}

// normalizeCallbackURL — вставленная пользователем ссылка бывает без схемы
// (Ф4-fix1 F2): url.Parse на «127.0.0.1:1455/auth/callback?…» не даёт scheme,
// и state не извлекается. Redirect_uri OAuth codex всегда http на 127.0.0.1 —
// молча достраиваем схему (однобуквенные хосты вида "a:1" не валидируем:
// state всё равно решает, валиден ли флоу).
func normalizeCallbackURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if u, err := url.Parse(trimmed); err == nil && u.Scheme != "" {
		return trimmed
	}
	return "http://" + trimmed
}

// CompleteByCallback завершает флоу по callback-URL, resolves аккаунт по
// state внутри (listener не знает аккаунт). Все проверки/одноразовость —
// в Complete: здесь только поиск accountKey по state под тем же мьютексом.
func (f *Flows) CompleteByCallback(ctx context.Context, callbackURL string) (*TokenSet, error) {
	u, err := url.Parse(normalizeCallbackURL(callbackURL))
	if err != nil {
		return nil, err
	}
	state := u.Query().Get("state")
	if state == "" {
		return nil, ErrFlowMalformed
	}
	f.mu.Lock()
	flw, ok := f.byState[state]
	f.mu.Unlock()
	if !ok {
		return nil, ErrFlowUnknownState
	}
	// Complete сам валидирует аккаунт/TTL и гасит state (одноразовость).
	return f.Complete(ctx, flw.accountKey, callbackURL)
}
