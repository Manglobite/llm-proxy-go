package codex

// Мелкая тестовая обвязка sender-тестов.

import (
	"net/http/httptest"
	"os"
)

// recorder — алиас httptest.ResponseRecorder: W провайдера пишет сюда.
type recorder = httptest.ResponseRecorder

func newRecorder() *recorder { return httptest.NewRecorder() }

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
