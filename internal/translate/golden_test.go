package translate

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update — перезапись golden-фикстур фактическим выводом (CODING 1.9):
//
//	go test ./internal/translate -update
var update = flag.Bool("update", false, "перезаписать golden-фикстуры фактическим выводом")

// golden сравнивает got с testdata/<name>. Сравнение после TrimSpace: хвостовые
// переводы строк не должны ломать тест при редактировании фикстуры вручную.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("запись golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (сгенерируйте: go test ./internal/translate -update)", path, err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Fatalf("golden %s расходится:\n--- фикстура ---\n%s\n--- факт ---\n%s", name, want, got)
	}
}

// goldenJSON маршалит v (indent для читаемых диффов) и сравнивает с фикстурой.
func goldenJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	golden(t, name, append(b, '\n'))
}

// goldenSSE — для уже сформированных SSE-тел.
func goldenSSE(t *testing.T, name string, got string) {
	t.Helper()
	golden(t, name, []byte(got))
}

// Формат SSE-фикстур (*.sse): по одному событию на строку "event|data";
// пустое имя до "|" — событие без имени (data-only, как в openai-стримах);
// строки с "#" — комментарии, пустые строки игнорируются. Формат выбран
// однострочным сознательно: JSON события умещается в строку, и дифф файла
// читается так же легко, как последовательность событий.
type sseFixture struct {
	event string
	data  string
}

// loadSSE читает testdata/<name>.
func loadSSE(t *testing.T, name string) []sseFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("фикстура %s: %v", name, err)
	}
	var out []sseFixture
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ev, data, ok := strings.Cut(line, "|")
		if !ok {
			t.Fatalf("фикстура %s: строка без разделителя |: %q", name, line)
		}
		out = append(out, sseFixture{event: ev, data: data})
	}
	if len(out) == 0 {
		t.Fatalf("фикстура %s пуста", name)
	}
	return out
}

// feedAll прогоняет фикстуру через транслятор/агрегатор.
func feedAll(t *testing.T, f Feeder, events []sseFixture) {
	t.Helper()
	for _, ev := range events {
		if err := f.Feed(ev.event, []byte(ev.data)); err != nil {
			t.Fatalf("Feed(%q): %v", ev.event, err)
		}
	}
}
