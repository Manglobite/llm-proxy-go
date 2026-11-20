// internal/translate/sse.go — универсальный SSE-сканер (few-shot
// sse-codex-to-openai): один на все пути — Responses API, openai-chat, anthropic.
package translate

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// ScanSSE читает SSE-поток и вызывает on(event, data) на каждое событие.
// data — склеенные data:-строки (без префиксов). Возвращает ошибку чтения.
func ScanSSE(r io.Reader, on func(event string, data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // большие tool-аргументы
	var (
		event   string
		data    bytes.Buffer
		pending bool // видели event/data, ждём пустую строку
	)
	flush := func() error {
		if !pending {
			return nil
		}
		if err := on(event, data.Bytes()); err != nil {
			return err
		}
		event, pending = "", false
		data.Reset()
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // пустая строка — граница события
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"): // комментарий (: ping keepalive и т.п.)
			// игнорируем
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			pending = true
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ") // один опциональный пробел по спецификации SSE
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(chunk)
			pending = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush() // событие без финальной пустой строки
}
