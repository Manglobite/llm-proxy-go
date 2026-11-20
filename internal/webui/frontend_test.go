// Фронтовые grep-гейты (REVIEW §10 WU-1/2/5/6, CODING 2.2/2.5/2.6) как Go-тест:
// WU-1 — оффлайн (литерал http(s):// только в комментариях); WU-2 — каждая
// интерполяция ${...} в .js проходит esc()/badgeHtml() и не содержит вложенных
// backtick-шаблонов; WU-6 — нет alert(/prompt(; импорты FSD — строго вниз;
// мёртвых модулей нет.
package webui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// webRoot — корень статики относительно пакета (internal/webui → ../../web).
const webRoot = "../../web"

// readWebFiles — все текстовые файлы web/ (пути относительные: "index.html", "src/...").
func readWebFiles(t *testing.T) map[string][]string {
	t.Helper()
	files := map[string][]string{}
	err := filepath.WalkDir(webRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(webRoot, p)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = strings.Split(string(raw), "\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk web/: %v", err)
	}
	return files
}

// stripComment — срезает часть строки после первого "//" (для гейтов мы
// намеренно консервативны: строка в кавычках с "//" обрежется тоже — для
// проверяемых правил это только ослабляет ложных срабатываний).
func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

// TestWebOffline — WU-1: литерал http:// / https:// только в комментариях
// (CDN/внешних зависимостей нет, WebUI работает оффлайн в контейнере).
func TestWebOffline(t *testing.T) {
	for name, lines := range readWebFiles(t) {
		for i, line := range lines {
			code := stripComment(line)
			if strings.Contains(code, "http://") || strings.Contains(code, "https://") {
				t.Fatalf("WU-1 %s:%d: сетевой литерал вне комментария: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestWebNoAlertPrompt — WU-6: alert(/prompt( заменены диалогами эталона.
func TestWebNoAlertPrompt(t *testing.T) {
	for name, lines := range readWebFiles(t) {
		for i, line := range lines {
			code := stripComment(line)
			if strings.Contains(code, "alert(") || strings.Contains(code, "prompt(") {
				t.Fatalf("WU-6 %s:%d: alert/prompt в коде: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// extractInterpolations — все ${...} выражения строки (вложенные backticks
// запрещены отдельной проверкой, поэтому баланс только по фигурным скобкам).
func extractInterpolations(code string) []string {
	var out []string
	for {
		i := strings.Index(code, "${")
		if i < 0 {
			return out
		}
		depth := 0
		j := i + 2
		for ; j < len(code); j++ {
			switch code[j] {
			case '{':
				depth++
			case '}':
				if depth == 0 {
					out = append(out, code[i+2:j])
					code = code[j+1:]
					j = len(code) // выход из внутреннего цикла
				} else {
					depth--
				}
			}
		}
		if j >= len(code) && depth != 0 {
			return out // незакрытая интерполяция — уйдёт в синтаксическую ошибку
		}
	}
}

// TestWebEscapedInterpolations — WU-2: в .js нет интерполяции вне esc()/badgeHtml()
// и нет вложенных backtick-шаблонов внутри ${...} (санитарная зона XSS: в дампы
// ошибок попадает чужой HTML — P0 по REVIEW §10).
func TestWebEscapedInterpolations(t *testing.T) {
	for name, lines := range readWebFiles(t) {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		for i, line := range lines {
			code := stripComment(line)
			for _, expr := range extractInterpolations(code) {
				if strings.Contains(expr, "`") {
					t.Fatalf("WU-2 %s:%d: вложенный backtick-шаблон в ${%s}", name, i+1, expr)
				}
				if !strings.Contains(expr, "esc(") && !strings.Contains(expr, "badgeHtml(") {
					t.Fatalf("WU-2 %s:%d: интерполяция вне esc()/badgeHtml(): ${%s}", name, i+1, expr)
				}
			}
		}
	}
}

// fsdLayer — индекс слоя FSD по первому сегменту пути под src/ (CODING 2.2:
// app → pages → widgets → features → entities → shared; импорты только вниз).
var fsdLayer = map[string]int{
	"app": 0, "pages": 1, "widgets": 2, "features": 3, "entities": 4, "shared": 5,
}

// TestWebFSDImports — импорты только вниз по слоям (горизонталь внутри слоя
// разрешена в объёме shared/* — так устроен эталон: badge→lib, api→api).
func TestWebFSDImports(t *testing.T) {
	for name, lines := range readWebFiles(t) {
		if !strings.HasSuffix(name, ".js") || !strings.HasPrefix(name, "src/") {
			continue
		}
		srcLayer := layerOf(name)
		for i, line := range lines {
			code := strings.TrimSpace(stripComment(line))
			if !strings.HasPrefix(code, "import ") || !strings.Contains(code, "from ") {
				continue
			}
			spec := importSpec(code)
			if !strings.HasPrefix(spec, ".") {
				t.Fatalf("FSD %s:%d: импорт не из дерева web/src: %q", name, i+1, spec)
			}
			target := filepath.ToSlash(filepath.Join(filepath.Dir(name), spec))
			targetLayer := layerOf(target)
			if targetLayer < srcLayer {
				t.Fatalf("FSD %s:%d: импорт вверх по слоям: %q", name, i+1, spec)
			}
		}
	}
}

// importSpec — спецификатор из строки `import … from "spec";` — текст между
// последними двумя кавычками (хвост ';/пробелы обрезаны).
func importSpec(code string) string {
	closing := strings.LastIndexByte(code, '"')
	if closing < 1 {
		return ""
	}
	opening := strings.LastIndexByte(code[:closing], '"')
	if opening < 0 {
		return ""
	}
	return code[opening+1 : closing]
}

func layerOf(relPath string) int {
	rest := strings.TrimPrefix(relPath, "src/")
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	return fsdLayer[seg]
}

// TestWebNoDeadModules — каждый .js модуль импортируется кем-то (или это main.js).
func TestWebNoDeadModules(t *testing.T) {
	files := readWebFiles(t)
	imported := map[string]bool{}
	for name, lines := range files {
		if !strings.HasSuffix(name, ".js") || !strings.HasPrefix(name, "src/") {
			continue
		}
		for _, line := range lines {
			code := strings.TrimSpace(stripComment(line))
			if !strings.HasPrefix(code, "import ") || !strings.Contains(code, "from ") {
				continue
			}
			imported[filepath.ToSlash(filepath.Join(filepath.Dir(name), importSpec(code)))] = true
		}
	}
	var dead []string
	for name := range files {
		if !strings.HasSuffix(name, ".js") || !strings.HasPrefix(name, "src/") || strings.HasSuffix(name, "app/main.js") {
			continue
		}
		if !imported[name] {
			dead = append(dead, name)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("мёртвые модули (никто не импортирует): %v", dead)
	}
}

// TestWebMockFormsSync — mock.js остаётся контрактом форм: ключевые литералы
// DEMO_* на месте (данные его форм = golden'ы API-тестов).
func TestWebMockFormsSync(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(webRoot, "src/shared/api/mock.js"))
	if err != nil {
		t.Fatalf("mock.js: %v", err)
	}
	mock := string(raw)
	for _, marker := range []string{
		"DEMO_STATUS_META", "DEMO_SOCKS_PATHS", "DEMO_CLIENT_KEYS", "DEMO_ACCOUNTS",
		"DEMO_ALIASES", "DEMO_USAGE", "DEMO_QUOTAS", "DEMO_RATE_LIMITS", "DEMO_OAUTH", "DEMO_ERRORS",
	} {
		if !strings.Contains(mock, marker) {
			t.Fatalf("mock.js потерял литерал %s", marker)
		}
	}
	// DEMO-флаг остаётся выключенным: рабочие вызовы идут через fetch (SPEC 12).
	client, err := os.ReadFile(filepath.Join(webRoot, "src/shared/api/client.js"))
	if err != nil {
		t.Fatalf("client.js: %v", err)
	}
	if !strings.Contains(string(client), "export const DEMO = false;") {
		t.Fatalf("client.js: DEMO должен быть false в рабочей версии")
	}
	// Все экраны потребляют свои эндпоинты (SPEC 12 ①–⑤ + OAuth codex/zcode).
	for _, want := range []struct{ file, endpoint string }{
		{"src/pages/providers.js", "/api/status"},
		{"src/pages/aliases.js", "/api/aliases"},
		{"src/pages/quotas.js", "/api/quotas"},
		{"src/pages/dashboard.js", "/api/usage"},
		{"src/pages/errors.js", "/api/errors"},
		{"src/features/oauth-flow.js", "/api/oauth/start"},
		{"src/features/oauth-flow.js", "/api/oauth/pending"},
		{"src/features/oauth-flow.js", "/api/oauth/paste"},
		{"src/features/oauth-flow.js", "/api/oauth/zcode/start"},
		{"src/features/oauth-flow.js", "/api/oauth/zcode/pending"},
		{"src/pages/providers.js", "/api/oauth/refresh"},
	} {
		raw, err := os.ReadFile(filepath.Join(webRoot, want.file))
		if err != nil {
			t.Fatalf("%s: %v", want.file, err)
		}
		if !strings.Contains(string(raw), want.endpoint) {
			t.Fatalf("%s не потребляет %s", want.file, want.endpoint)
		}
	}
	// Статика index.html держит каркас эталона (WU-4).
	index, err := os.ReadFile(filepath.Join(webRoot, "index.html"))
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	for _, marker := range []string{`src/shared/ui/tokens.css`, `src/shared/ui/ui.css`, `src/app/main.js`} {
		if !strings.Contains(string(index), marker) {
			t.Fatalf("index.html потерял %s", marker)
		}
	}
}
