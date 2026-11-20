// Decode тел запросов (SPEC 4 / TR-11): Content-Encoding identity|gzip|zstd
// распаковывается ДО парсинга; кап на РАСПАКОВАННОЕ тело — анти-декомпрессионная
// бомба (превышение → 413).
package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
)

// httpError — ошибка с готовым HTTP-статусом и code для конверта протокола.
type httpError struct {
	status int
	code   string
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// decodeBody читает и распаковывает тело запроса.
//
// Кап ставится дважды, и это не дублирование: сжатое тело ограничиваем на
// чтении из сети (сжатое > капа ⇒ распакованное тем больше), распакованное —
// на декодировании (маленький архив может развернуться в гигабайт).
func (s *Server) decodeBody(r *http.Request) ([]byte, *httpError) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, s.params.MaxBodyBytes+1))
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, "invalid_request", "не удалось прочитать тело запроса: " + err.Error()}
	}
	if int64(len(raw)) > s.params.MaxBodyBytes {
		return nil, &httpError{http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("тело запроса превышает %d байт", s.params.MaxBodyBytes)}
	}

	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
		return raw, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, &httpError{http.StatusBadRequest, "invalid_request", "битый gzip: " + err.Error()}
		}
		return s.readDecoded(zr)
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, &httpError{http.StatusBadRequest, "invalid_request", "битый zstd: " + err.Error()}
		}
		defer zr.Close()
		return s.readDecoded(zr)
	default:
		return nil, &httpError{http.StatusUnsupportedMediaType, "unsupported_encoding",
			fmt.Sprintf("Content-Encoding %q не поддерживается (identity|gzip|zstd)", enc)}
	}
}

// readDecoded дочитывает распакованный поток с капом (вызывается после
// успешного создания декомпрессора).
func (s *Server) readDecoded(r io.Reader) ([]byte, *httpError) {
	body, err := io.ReadAll(io.LimitReader(r, s.params.MaxBodyBytes+1))
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, "invalid_request", "не удалось распаковать тело: " + err.Error()}
	}
	if int64(len(body)) > s.params.MaxBodyBytes {
		return nil, &httpError{http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("распакованное тело превышает %d байт", s.params.MaxBodyBytes)}
	}
	return body, nil
}
