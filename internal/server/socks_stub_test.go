// Минимальный no-auth SOCKS5-стаб для тестов сервера: только CONNECT + pipe
// (по образцу стаба internal/socks, но самодостаточный — тот живёт в другом
// пакете). Нужен, чтобы transporter аккаунта шёл через настоящий путь socks:
// только там работают gate/needCheck, ассертить которые требует FC-9.
package server

import (
	"io"
	"net"
	"strconv"
	"testing"
)

type socksStub struct {
	ln  net.Listener
	url string
}

// newSocksStub поднимает no-auth SOCKS5 на случайном порту localhost.
func newSocksStub(t *testing.T) *socksStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks stub listen: %v", err)
	}
	s := &socksStub{ln: ln, url: "socks5h://" + ln.Addr().String()}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *socksStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // слушатель закрыт — стаб остановлен
		}
		go s.handle(conn)
	}
}

// handle — рукопожатие no-auth (VER5/METHOD0) + CONNECT + двусторонний pipe.
func (s *socksStub) handle(conn net.Conn) {
	defer conn.Close()
	hdr := make([]byte, 2) // VER, NMETHODS
	if _, err := io.ReadFull(conn, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, int(hdr[1]))); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil { // выбор no-auth
		return
	}
	head := make([]byte, 4) // VER CMD RSV ATYP
	if _, err := io.ReadFull(conn, head); err != nil || head[1] != 1 {
		return // поддерживаем только CONNECT
	}
	var host string
	switch head[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3: // домен (socks5h)
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = string(b)
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1]))))
	if err != nil {
		// REP=1 (general failure): клиент увидит сетевую ошибку диалера.
		_, _ = conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // REP=0 — success
	go func() {
		_, _ = io.Copy(target, conn)
		target.Close()
	}()
	_, _ = io.Copy(conn, target)
}
