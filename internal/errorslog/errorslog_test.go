package errorslog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestMaskedHeadersMasksSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-super-secret")
	h.Set("X-Api-Key", "key-super-secret")
	h.Set("Api-Key", "another-secret")
	h.Set("Cookie", "session=secret")
	h.Set("Content-Type", "application/json")
	h.Set("X-Request-Id", "req-1")

	b := MaskedHeaders(h)
	var got map[string][]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal дампа заголовков: %v (%s)", err, b)
	}
	// AC-2: секретные заголовки затёрты ДО записи — секретов в дампе нет вообще.
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Cookie"} {
		v := got[name]
		if len(v) != 1 || v[0] != "<masked>" {
			t.Fatalf("заголовок %s = %v, ожидался <masked>", name, v)
		}
	}
	if got["Content-Type"][0] != "application/json" || got["X-Request-Id"][0] != "req-1" {
		t.Fatalf("незасекреченные заголовки искажены: %v", got)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("секрет просочился в дамп: %s", b)
	}
	// Вход не должен быть изменён маскировкой (дамп готовится из копии).
	if h.Get("Authorization") != "Bearer sk-super-secret" {
		t.Fatalf("MaskedHeaders испортил входной Header")
	}
}

func TestMaskedHeadersEmpty(t *testing.T) {
	b := MaskedHeaders(http.Header{})
	if !bytes.Equal(b, []byte("{}")) {
		t.Fatalf("пустой заголовок → %s, ожидалось {}", b)
	}
}

func TestInsertErrorCapsBodies(t *testing.T) {
	d := openTestDB(t)
	big := bytes.Repeat([]byte("a"), maxDump+100)
	err := d.InsertError(ErrorRow{
		Ts: time.Now(), ClientKey: "opencode", Alias: "a", Provider: "p", Account: "a1",
		Stage: "upstream", Status: 500,
		Method: "POST", URL: "http://up/v1/chat/completions",
		ReqHeaders: []byte(`{"Authorization":["<masked>"]}`),
		ReqBody:    big, RespBody: big,
		Err: "boom",
	})
	if err != nil {
		t.Fatalf("InsertError: %v", err)
	}
	var reqBody, respBody []byte
	if err := d.db.QueryRow(`SELECT request_body, response_body FROM errors`).Scan(&reqBody, &respBody); err != nil {
		t.Fatalf("select: %v", err)
	}
	// AC-3: кап 1 МБ — тело не раздувает БД, маркер обрезки присутствует.
	if len(reqBody) != maxDump+len("...<truncated>") || len(respBody) != maxDump+len("...<truncated>") {
		t.Fatalf("кап не сработал: %d %d", len(reqBody), len(respBody))
	}
	if !bytes.HasSuffix(reqBody, []byte("...<truncated>")) {
		t.Fatalf("нет маркера обрезки: %q", reqBody[len(reqBody)-30:])
	}
}

func TestInsertErrorSmallBodyUntouched(t *testing.T) {
	d := openTestDB(t)
	body := []byte(`{"model":"gpt-test"}`)
	err := d.InsertError(ErrorRow{
		Ts: time.Now(), Stage: "translate", Status: 400,
		Method: "POST", URL: "/v1/messages", ReqBody: body, Err: "bad request",
	})
	if err != nil {
		t.Fatalf("InsertError: %v", err)
	}
	var got []byte
	if err := d.db.QueryRow(`SELECT request_body FROM errors`).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("маленькое тело искажено: %q", got)
	}
}

func TestCleanupOlder(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	rows := []ErrorRow{
		{Ts: now.Add(-48 * time.Hour), Stage: "upstream", Err: "old-1"},
		{Ts: now.Add(-24 * time.Hour), Stage: "upstream", Err: "old-2"},
		{Ts: now, Stage: "stream", Err: "fresh"},
	}
	for _, r := range rows {
		if err := d.InsertError(r); err != nil {
			t.Fatalf("InsertError: %v", err)
		}
	}
	// AC-9: удаляется строго старше отсечки, плейсхолдером.
	n, err := d.CleanupOlder(now.Add(-30 * time.Hour))
	if err != nil {
		t.Fatalf("CleanupOlder: %v", err)
	}
	if n != 1 {
		t.Fatalf("удалено %d строк, ожидалась 1", n)
	}
	var count int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM errors`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("осталось %d строк, ожидалось 2", count)
	}
	// Повторный cleanup с более ранней отсечкой ничего не удаляет.
	n, err = d.CleanupOlder(now.Add(-48 * time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("повторный cleanup: n=%d err=%v", n, err)
	}
}
