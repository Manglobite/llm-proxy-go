// Тест микропатча N1 (Ф2b): успешный non-stream ответ уходит клиенту ДО
// SQLite-вставок сервера (AC-4) — Flush после записи тела в nonStreamToClient,
// по образцу TestErrorEnvelopeNotDelayedBySQLite для конвертов ошибок.
package server

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSuccessBodyNotDelayedBySQLite(t *testing.T) {
	e := newEnv(t, envOptions{})

	// Держим write-lock на БД: INSERT'ы прокси упрутся в busy_timeout(5s).
	lockDB, err := sql.Open("sqlite", e.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("lock db: %v", err)
	}
	defer lockDB.Close()
	tx, err := lockDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS _n1_lock(x INTEGER)`); err != nil {
		t.Fatalf("create lock table: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO _n1_lock VALUES (1)`); err != nil {
		t.Fatalf("take write lock: %v", err)
	}
	defer func() {
		_ = tx.Rollback() // отпускаем лок: зависшие INSERT'ы прокси доедут
	}()

	start := time.Now()
	resp := e.postJSON("/v1/chat/completions", "sk-test-key", []byte(openaiNonStreamBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d (%s)", resp.StatusCode, e.readBody(resp))
	}
	// Успешное тело обязано доехать, ПОКА лок ещё удержан: без Flush оно
	// ждало бы возврата хендлера (после INSERT'ов, ~5s) — регрессия N1.
	// Читаем БАЙТЫ, а не строки: тело — один чанк без завершающего '\n',
	// строчный сканер вернул бы токен только по EOF (EOF приходит после
	// возврата хендлера и маскировал бы факт ранней доставки байтов).
	var got []byte
	buf := make([]byte, 1024)
	for !strings.Contains(string(got), "привет") {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			t.Fatalf("чтение тела: %v (прочитано %q)", err, got)
		}
		if len(got) > 64<<10 {
			t.Fatal("тело больше ожидаемого, контент не найден")
		}
	}
	elapsed := time.Since(start)
	// Лок держим до конца чтения: фиксируем факт доставки при удержанном локе.
	if err := resp.Body.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("успешное тело ждало %v — Flush после записи тела отсутствует (N1/AC-4)", elapsed)
	}
	// defer tx.Rollback() отпускает лок: вставки прокси завершатся, хендлер
	// выйдет, cleanup не зависнет.
}
