// Прокачка SSE-стрима апстрима (ARCHITECTURE §5): события читаются в одной
// фоновой горутине, но ПИШУТСЯ клиенту только из горутины запроса (backpressure
// без фан-аута), с keepalive-комментарием ': ping' и idle-таймаутом (SPEC 4).
package upstream

import (
	"context"
	"errors"
	"io"
	"time"

	"llm-proxy/internal/translate"
)

// SSEEvent — событие апстрима после разбора ScanSSE.
type SSEEvent struct {
	Event string // "" для событий без event:-строки (data-only, [DONE])
	Data  []byte
}

// ErrStop — onEvent возвращает его, чтобы остановить прокачку штатно
// (например, после отправки клиенту SSE-ошибки по событию error апстрима).
// Не считается сбоем ни чтения, ни записи.
var ErrStop = errors.New("upstream: stream stopped by handler")

// PumpResult — чем закончилась прокачка.
type PumpResult struct {
	// ReadErr — ошибка чтения апстрима (сетевой обрыв посреди стрима, FC-9).
	ReadErr error
	// Idle — сработал idle-таймаут тишины апстрима (onIdle уже вызван).
	Idle bool
	// Stopped — onEvent вернул ErrStop (штатная остановка обработчиком).
	Stopped bool
	// ClientGone — отменён ctx запроса (клиент ушёл / сервер глушится).
	ClientGone bool
	// WriteErr — ошибка записи клиенту (ping или onEvent-писатель).
	WriteErr error
}

// PumpStream прокачивает SSE-поток апстрима:
//   - каждое событие уходит в onEvent (в горутине, владеющей записью в клиента);
//   - пока апстрим молчит, каждые keepalive вызывается ping (комментарий ': ping',
//     SPEC 4: opencode-парсер их игнорирует, молчание >15m убивает его таймаут);
//   - тишина дольше idle → onIdle (пишет SSE-ошибку в протоколе выхода) и стоп;
//   - cancelUpstream вызывается при ЛЮБОМ досрочном выходе — иначе чтение
//     апстрима продолжалось бы в фоне.
//
// onIdle/ping обязаны быть неблокирующими и писать из текущей горутины: pump —
// единственный писатель в клиента.
func PumpStream(ctx context.Context, body io.Reader, cancelUpstream context.CancelFunc,
	keepalive, idle time.Duration,
	ping func() error, onEvent func(SSEEvent) error, onIdle func()) PumpResult {

	// done — гарантия выхода горутине-читателю: если pump вернулся (idle/ошибка
	// записи), заблокированная отправка в evCh должна отваливаться, иначе
	// горутина утекает навсегда (CC-1).
	done := make(chan struct{})
	defer close(done)
	// cancelUpstream на любом выходе, кроме естественного EOF: после него
	// чтение тела всё равно завершится само, но раньше — лучше.
	defer cancelUpstream()

	evCh := make(chan SSEEvent, 16) // буфер сглаживает рывки апстрима
	errCh := make(chan error, 1)    // буфер 1: читатель не блокируется, если pump уже ушёл
	go func() {
		err := translate.ScanSSE(body, func(event string, data []byte) error {
			// Копия ОБЯЗАТЕЛЬНА: буфер data у ScanSSE переиспользуется после
			// возврата колбэка, а событие живёт в канале, пока pump его не
			// заберёт — без копии следующее событие затирает предыдущее.
			cp := make([]byte, len(data))
			copy(cp, data)
			select {
			case evCh <- SSEEvent{Event: event, Data: cp}:
				return nil
			case <-done:
				// pump уже вернулся — глушим сканер, горутина выходит.
				return errors.New("pump stopped")
			}
		})
		errCh <- err
		close(evCh)
	}()

	var (
		idleTimer *time.Timer
		idleC     <-chan time.Time
	)
	if idle > 0 {
		idleTimer = time.NewTimer(idle)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}
	var (
		pingTimer *time.Timer
		pingC     <-chan time.Time
	)
	if keepalive > 0 && ping != nil {
		pingTimer = time.NewTimer(keepalive)
		defer pingTimer.Stop()
		pingC = pingTimer.C
	}

	// resetIdle/resetPing — перезапуск таймеров после каждого события/пинга.
	// Stop()==false значит «уже стрельнул»: сбрасываем значение из канала,
	// иначе следующий select мгновенно увидит устаревшее срабатывание.
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleC:
			default:
			}
		}
		idleTimer.Reset(idle)
	}
	resetPing := func() {
		if pingTimer == nil {
			return
		}
		if !pingTimer.Stop() {
			select {
			case <-pingC:
			default:
			}
		}
		pingTimer.Reset(keepalive)
	}

	var res PumpResult
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				// Читатель закончил: ошибка чтения уже в errCh (буфер 1,
				// отправлена до close(evCh)) — чтение не блокируется.
				res.ReadErr = <-errCh
				return res
			}
			resetIdle()
			resetPing()
			if err := onEvent(ev); err != nil {
				if errors.Is(err, ErrStop) {
					res.Stopped = true
					return res
				}
				// Ошибка записи клиенту (клиент ушёл) — дальше писать некому.
				res.WriteErr = err
				return res
			}
		case <-pingC:
			if err := ping(); err != nil {
				res.WriteErr = err
				return res
			}
			resetPing()
		case <-idleC:
			// SPEC 4: молчание апстрима дольше stream_idle_timeout — обрыв
			// с ошибкой в протоколе входа (пишет onIdle).
			res.Idle = true
			if onIdle != nil {
				onIdle()
			}
			return res
		case <-ctx.Done():
			// Клиент отключился / сервер глушится: SSE-ошибку писать некому.
			res.ClientGone = true
			return res
		}
	}
}
