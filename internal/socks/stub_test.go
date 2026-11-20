// Тестовые стабы пакета: минимальный no-auth SOCKS5-сервер (только CONNECT + pipe),
// check-сервер IP и httptest-цель. Живут только в _test-файлах: в проде прокси
// сам SOCKS-сервером не является.
package socks

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	socksVersion    = 5
	socksCmdConnect = 1

	// Дедлайн только на рукопожатие: битый клиент не должен подвесить тест.
	stubHandshakeTimeout = 10 * time.Second
)

// socksStub — настоящий SOCKS5-сервер (no-auth) на localhost: через него проверки
// и запросы идут через настоящий диалер golang.org/x/net/proxy, как в проде.
type socksStub struct {
	ln  net.Listener
	url string

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{} // живые соединения: Close закрывает их, чтобы горутины не переживали тест
}

func newSocksStub(t *testing.T) *socksStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("стаб SOCKS: listen: %v", err)
	}
	s := &socksStub{ln: ln, conns: make(map[net.Conn]struct{})}
	s.url = "socks5h://" + ln.Addr().String()
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// URL — адрес стаба в формате конфига (socks5h://host:port).
func (s *socksStub) URL() string { return s.url }

// Close останавливает приём и закрывает все живые соединения.
func (s *socksStub) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	// Close у Listener/Conn идемпотентен: ошибка может означать только
	// «уже закрыто», что для остановки стаба не имеет значения.
	defer s.ln.Close()
	for c := range s.conns {
		defer c.Close()
	}
}

func (s *socksStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // слушатель закрыт — стаб остановлен
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *socksStub) handle(conn net.Conn) {
	defer s.dropConn(conn)

	if err := conn.SetDeadline(time.Now().Add(stubHandshakeTimeout)); err != nil {
		return // соединение уже закрыто
	}
	target, err := s.handshake(conn)
	if err != nil {
		return // ответ на ошибку отправлен внутри handshake (или соединение битое)
	}
	// После рукопожатия это просто труба: дедлайн снимаем, данные могут течь долго.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		target.Close()
		return
	}
	pipe(conn, target)
}

func (s *socksStub) dropConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	defer conn.Close()
}

// handshake — серверная часть SOCKS5: приветствие no-auth, затем CONNECT.
// Контракт: target != nil тогда и только тогда, когда err == nil. При отказе
// клиенту отправляется код ошибки SOCKS, и он получает транспортную ошибку —
// именно её и ждут тесты fail-closed/needCheck.
func (s *socksStub) handshake(conn net.Conn) (net.Conn, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	if head[0] != socksVersion {
		return nil, fmt.Errorf("стаб SOCKS: версия %d, ожидается %d", head[0], socksVersion)
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return nil, err
	}
	// Всегда соглашаемся на no-auth (0x00) — других методов стаб не умеет.
	if _, err := conn.Write([]byte{socksVersion, 0}); err != nil {
		return nil, err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return nil, err
	}
	if req[1] != socksCmdConnect {
		// 0x07 — command not supported: других команд стаб не обслуживает.
		if err := s.replyFailure(conn, 7); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("стаб SOCKS: команда %d не поддерживается", req[1])
	}
	host, err := readSocksAddr(conn, req[3])
	if err != nil {
		return nil, err
	}
	var portBE [2]byte
	if _, err := io.ReadFull(conn, portBE[:]); err != nil {
		return nil, err
	}
	port := int(portBE[0])<<8 | int(portBE[1])
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		// 0x01 — general SOCKS server failure: клиент видит сетевую ошибку,
		// что и нужно для сценариев «цель недоступна».
		if werr := s.replyFailure(conn, 1); werr != nil {
			return nil, werr
		}
		return nil, fmt.Errorf("стаб SOCKS: цель %s:%d недоступна: %w", host, port, err)
	}
	if err := s.replySuccess(conn); err != nil {
		defer target.Close()
		return nil, err
	}
	return target, nil
}

func (s *socksStub) replyFailure(conn net.Conn, code byte) error {
	_, err := conn.Write([]byte{socksVersion, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

func (s *socksStub) replySuccess(conn net.Conn) error {
	// BND.ADDR/PORT нулевые: клиентов стаба адрес ответной стороны не интересует.
	_, err := conn.Write([]byte{socksVersion, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

// readSocksAddr разбирает ATYP+адрес CONNECT-запроса (порт читает вызывающий).
func readSocksAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case 1: // IPv4
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	case 3: // домен: 1 байт длины + имя
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 4: // IPv6
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	default:
		return "", fmt.Errorf("стаб SOCKS: неожиданный ATYP %d", atyp)
	}
}

// pipe перекачивает данные в обе стороны, пока одна из сторон не оборвётся.
func pipe(client, target net.Conn) {
	done := make(chan struct{}, 2)
	go relay(client, target, done)
	go relay(target, client, done)
	<-done // первый relay закрыл оба conn → второй завершается следом
	<-done
}

// relay копирует в одну сторону и закрывает оба соединения. Ошибка io.Copy здесь —
// штатный обрыв трубы (клиент или цель закрыл соединение): различать нечего,
// горутина обязана просто завершиться, не протекая.
func relay(dst, src net.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

// ipCheckServer — стаб сервиса проверки egress IP (check_url): отдаёт фиксированный
// адрес, считает хиты, умеет ломаться (статус ≠ 200) и молчать (тест таймаута/аборта).
type ipCheckServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	ip     string
	status int
	block  bool
	hits   int
	held   chan struct{} // сигнал «чек-запрос дошёл до хендлера и встал на паузу»
}

func newIPCheckServer(t *testing.T, ip string) *ipCheckServer {
	t.Helper()
	c := &ipCheckServer{ip: ip, status: http.StatusOK, held: make(chan struct{}, 4)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		block, status, ip := c.block, c.status, c.ip
		c.hits++
		c.mu.Unlock()
		if block {
			c.signalHeld()
			// Молчим до отмены запроса клиентом (дедлайн чека/аборт) — иначе
			// httptest.Close будет ждать завершения хендлера.
			<-r.Context().Done()
			return
		}
		w.WriteHeader(status)
		if _, err := w.Write([]byte(ip)); err != nil {
			return // читатель уже ушёл (таймаут/отмена) — для стаба это не ошибка
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *ipCheckServer) signalHeld() {
	select {
	case c.held <- struct{}{}:
	default:
	}
}

func (c *ipCheckServer) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-c.held:
	case <-time.After(5 * time.Second):
		t.Fatal("чек-запрос не дошёл до стаба за 5s")
	}
}

func (c *ipCheckServer) URL() string { return c.srv.URL }

func (c *ipCheckServer) hitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func (c *ipCheckServer) setStatus(code int) {
	c.mu.Lock()
	c.status = code
	c.mu.Unlock()
}

func (c *ipCheckServer) setBlock(block bool) {
	c.mu.Lock()
	c.block = block
	c.mu.Unlock()
}

// targetServer — httptest-цель (апстрим-стаб): считает коннекты, умеет «висеть»
// до отмены запроса (тесты FC-8 и таймаута чека).
type targetServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	hits    int
	hang    bool
	reached chan struct{} // сигнал «запрос дошёл до хендлера» (буферизованный)
}

func newTargetServer(t *testing.T) *targetServer {
	t.Helper()
	ts := &targetServer{reached: make(chan struct{}, 4)}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		hang := ts.hang
		if !hang {
			ts.hits++
		}
		ts.mu.Unlock()
		select {
		case ts.reached <- struct{}{}:
		default:
		}
		if hang {
			// Висим до отмены запроса клиентом: моделируем долгий апстрим.
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte("ok")); err != nil {
			return // клиент ушёл до ответа — в стабе это не ошибка теста
		}
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *targetServer) URL() string { return ts.srv.URL }

func (ts *targetServer) hitCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.hits
}

func (ts *targetServer) setHang(hang bool) {
	ts.mu.Lock()
	ts.hang = hang
	ts.mu.Unlock()
}

func (ts *targetServer) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-ts.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("запрос не дошёл до цели за 5s")
	}
}

// deadAddr — адрес закрытого локального порта: «мёртвый» SOCKS/цель для тестов.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}
