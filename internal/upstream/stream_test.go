// Тесты общей прокачки SSE (upstream.PumpStream): регрессия переиспользования
// буфера ScanSSE и idle-таймаут. Полные сценарии стрима — в server e2e.
package upstream

import (
	"context"
	"io"
	"testing"
	"time"
)

// eventsReader — io.Reader, отдающий готовые SSE-фреймы построчно.
type eventsReader struct {
	frames []string
	i      int
	buf    []byte
}

func (r *eventsReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.i >= len(r.frames) {
			return 0, io.EOF
		}
		r.buf = []byte(r.frames[r.i])
		r.i++
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// TestPumpStreamCopiesEventData — регрессия: буфер data у ScanSSE
// переиспользуется, событие в канале не должно затираться следующим.
func TestPumpStreamCopiesEventData(t *testing.T) {
	r := &eventsReader{frames: []string{
		"data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: {\"n\":3}\n\n",
	}}
	var got []SSEEvent
	pr := PumpStream(context.Background(), r, func() {},
		0, time.Hour,
		nil,
		func(ev SSEEvent) error {
			got = append(got, SSEEvent{Event: ev.Event, Data: append([]byte(nil), ev.Data...)})
			return nil
		},
		nil)
	if pr.ReadErr != nil || pr.Idle || pr.WriteErr != nil {
		t.Fatalf("pump: %+v", pr)
	}
	if len(got) != 3 {
		t.Fatalf("событий: %d, ожидалось 3", len(got))
	}
	for i, want := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		if string(got[i].Data) != want {
			t.Fatalf("событие %d = %q, ожидалось %q (буфер затёрт?)", i, got[i].Data, want)
		}
	}
}

// TestPumpStreamIdleTimeout — тишина дольше idle: onIdle вызван, pump вернулся.
func TestPumpStreamIdleTimeout(t *testing.T) {
	// Reader, который молчит, но читаемость сохраняет (в реале — живой body
	// без данных; здесь — труба, которую закроем при уборке).
	pr, pw := io.Pipe()
	defer pw.Close()

	idleFired := make(chan struct{})
	go func() {
		res := PumpStream(context.Background(), pr, func() {},
			0, 30*time.Millisecond,
			nil,
			func(ev SSEEvent) error { return nil },
			func() { close(idleFired) })
		if !res.Idle {
			t.Errorf("pump не зафиксировал idle: %+v", res)
		}
	}()
	select {
	case <-idleFired:
	case <-time.After(5 * time.Second):
		t.Fatal("idle-таймаут не сработал за 5с")
	}
}

// TestPumpStreamErrStop — onEvent вернул ErrStop: прокачка штатно остановлена,
// cancelUpstream вызван (чтение апстрима гасится).
func TestPumpStreamErrStop(t *testing.T) {
	r := &eventsReader{frames: []string{
		"data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: {\"n\":3}\n\n",
	}}
	cancelled := make(chan struct{})
	var seen int
	pr := PumpStream(context.Background(), r, func() { close(cancelled) },
		0, time.Hour,
		nil,
		func(ev SSEEvent) error {
			seen++
			return ErrStop
		},
		nil)
	if !pr.Stopped {
		t.Fatalf("pump: %+v, ожидался Stopped", pr)
	}
	if seen != 1 {
		t.Fatalf("onEvent вызван %d раз, ожидался 1", seen)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancelUpstream не вызван при ErrStop")
	}
}

// TestPumpStreamPing — keepalive-пинг уходит при тишине апстрима. Формат
// строки ': ping' проверен в server e2e — здесь механика вызова замыкания.
func TestPumpStreamPing(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	pings := make(chan struct{}, 4)
	go func() {
		PumpStream(context.Background(), pr, func() {},
			20*time.Millisecond, time.Hour,
			func() error { pings <- struct{}{}; return nil },
			func(ev SSEEvent) error { return nil },
			nil)
	}()
	select {
	case <-pings:
	case <-time.After(5 * time.Second):
		t.Fatal("ping не пришёл за 5с")
	}
}
