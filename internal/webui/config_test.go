package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llm-proxy/internal/config"
	"llm-proxy/internal/server"
	"llm-proxy/internal/upstream/openailike"
)

func TestModelCatalog(t *testing.T) {
	const token = "fixture-admin"
	var requests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-upstream" {
			t.Error("unexpected upstream method or account key")
		}
		switch r.URL.Path {
		case "/gateway/v1/models", "/api/coding/paas/v4/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"z"},{"id":"a"},{"id":"a"},{"id":""}]}`))
		case "/redirect/v1/models":
			http.Redirect(w, r, "/unexpected", http.StatusFound)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	e := newEnv(t, token)
	ollama := e.snap.Cfg.Providers["ollama"].OpenAILike
	ollama.BaseURL = upstream.URL + "/gateway/"
	ollama.Accounts[0].Auth.Key = "fixture-upstream"
	glm := e.snap.Cfg.Providers["glm"].GLM
	glm.BaseURL = upstream.URL
	glm.Accounts[0].Auth.Key = "fixture-upstream"
	const route = "/api/config/models?account="
	for _, tc := range []struct {
		account string
		token   string
		status  int
	}{
		{"ollama/o1", "", http.StatusUnauthorized},
		{"ollama/missing", token, http.StatusNotFound},
		{"codex/c1", token, http.StatusNotImplemented},
		{"ollama/o1", token, http.StatusOK},
		{"glm/g1", token, http.StatusOK},
	} {
		res := e.get(route+tc.account, tc.token)
		if res.Code != tc.status || (res.Code == http.StatusOK && strings.TrimSpace(res.Body.String()) != `{"models":["a","z"]}`) {
			t.Fatalf("%s: got %d %s", tc.account, res.Code, res.Body.String())
		}
	}
	ollama.BaseURL = upstream.URL + "/redirect"
	if res := e.get(route+"ollama/o1", token); res.Code != http.StatusBadGateway {
		t.Fatalf("redirect should be rejected, got %d", res.Code)
	}
	e.snap.Cfg.Server.WebUIToken = ""
	if res := e.get(route+"ollama/o1", ""); res.Code != http.StatusForbidden {
		t.Fatalf("missing configured token: got %d", res.Code)
	}
	if requests != 3 {
		t.Fatalf("expected three upstream requests, got %d", requests)
	}
}

func TestCreateClientKey(t *testing.T) {
	const admin = "fixture-admin"
	const endpoint = "/api/config/clients"
	const payload = `{"name":"new-client"}`
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"Server":{"listen":"127.0.0.1:8787","Clients":{"existing":"fixture-client"}}}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := config.NewManager(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d := newEnv(t, "").ui.deps
	d.Snapshot, d.AddClient = mgr.Snapshot, mgr.AddClient
	ui, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Params{Snapshot: mgr.Snapshot, Sender: openailike.Sender{}, WebUI: ui})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	request := func(method, path, token, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got status %d, want %d", method, path, w.Code, status)
		}
		return w
	}
	request(http.MethodPost, endpoint, "", payload, 403)
	write(`{"Server":{"listen":"127.0.0.1:8787","webui_token":"` + admin + `","Clients":{"existing":"fixture-client"}}}`)
	if err := mgr.Reload(path); err != nil {
		t.Fatal(err)
	}
	before := mgr.Snapshot()
	request(http.MethodPost, endpoint, "fixture-client", payload, 401)
	request(http.MethodPost, endpoint, admin, `{"name":"invalid name"}`, 400)
	request(http.MethodPost, endpoint, admin, `{"name":"new-client","key":"custom"}`, 400)
	request(http.MethodPost, endpoint, admin, `{"name":"new-client","alias":null}`, 400)
	request(http.MethodPost, endpoint, admin, `{"name":"existing"}`, 409)
	if mgr.Snapshot() != before {
		t.Fatal("failed requests changed live configuration")
	}
	w := request(http.MethodPost, endpoint, admin, payload, 200)
	var result struct{ Key string }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !strings.HasPrefix(result.Key, "sk-") || len(result.Key) != 67 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("expected a generated key and a non-cacheable response")
	}
	request(http.MethodGet, "/v1/models", result.Key, "", 200)
	request(http.MethodGet, "/v1/models", "fixture-client", "", 200)
	request(http.MethodGet, "/v1/models", admin, "", 401)
	for _, rec := range []*httptest.ResponseRecorder{
		request(http.MethodPost, endpoint, admin, payload, 409),
		request(http.MethodGet, endpoint, admin, "", 405),
		request(http.MethodGet, "/api/status", admin, "", 200),
	} {
		if strings.Contains(rec.Body.String(), result.Key) {
			t.Fatal("client key disclosed after creation")
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Clients["new-client"] != result.Key || cfg.Server.Clients["existing"] != "fixture-client" || cfg.Server.WebUIToken != admin {
		t.Fatal("key not persisted or unrelated settings lost")
	}
	write(`{"server":{"listen":"127.0.0.1:8787","webui_token":"` + admin + `"}}`)
	request(http.MethodPost, endpoint, admin, `{"name":"other-client"}`, 409)
	if err := mgr.Reload(path); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, endpoint, admin, payload, 200)
	if key := mgr.Snapshot().Cfg.Server.Clients["new-client"]; key == "" || key == result.Key {
		t.Fatal("expected a fresh key when adding to a config without clients")
	}
}

func TestConfigCreate(t *testing.T) {
	const admin = "fixture-admin"
	const key = "fixture-upstream"
	const socksPassword = "fixture-socks-password"
	const socks = `{"name":"work","socks":{"url":"socks5h://local:` + socksPassword + `@127.0.0.1:1080","expected_ip":"192.0.2.1","check_interval":"75s","check_url":"http://127.0.0.1:1"}}`
	const provider = `{"name":"local","provider":{"type":"openai-like","base_url":"http://127.0.0.1:11434","accounts":[{"id":"main","socks":"none","auth":{"method":"api_key","key":"` + key + `"}}]}}`
	const alias = `{"name":"local/test","alias":{"account":"local/main","model":"upstream-model"}}`
	e := newEnv(t, "")
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"server":{"listen":"127.0.0.1:8787","clients":{"test":"fixture-client"}},"providers":null}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := config.NewManager(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d := e.ui.deps
	d.Snapshot, d.AddProvider, d.AddAlias, d.AddSocks = mgr.Snapshot, mgr.AddProvider, mgr.AddAlias, mgr.AddSocks
	d.UpdateSocks = mgr.UpdateSocks
	e.ui, err = New(d)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, endpoint, token, body string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, "/api/config/"+endpoint, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ui.ServeHTTP(w, r)
		if w.Code != want || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), socksPassword) {
			t.Fatalf("%s: got status %d, want %d (or response contains a secret)", endpoint, w.Code, want)
		}
	}
	post := func(endpoint, token, body string, want int) {
		t.Helper()
		request(http.MethodPost, endpoint, token, body, want)
	}
	post("providers", "", provider, 403)
	post("socks/check", "", socks, 403)
	post("socks/recheck", "", `{"name":"work"}`, 403)
	post("socks", "", socks, 403)
	write(`{"server":{"listen":"127.0.0.1:8787","webui_token":"` + admin + `","clients":{"test":"fixture-client"}},"providers":null}`)
	if err := mgr.Reload(path); err != nil {
		t.Fatal(err)
	}
	before := mgr.Snapshot()
	post("providers", "wrong", provider, 401)
	post("socks", "wrong", socks, 401)
	for _, invalid := range []string{
		strings.Replace(socks, `"work"`, `"none"`, 1),
		strings.Replace(socks, "socks5h:", "socks5:", 1),
		strings.Replace(socks, ":1080", "", 1),
		strings.Replace(socks, "192.0.2.1", "not-an-ip", 1),
		strings.Replace(socks, "75s", "-75s", 1),
		strings.Replace(socks, "http://127.0.0.1:1", "file:///tmp/test", 1),
	} {
		post("socks", admin, invalid, 400)
	}
	post("aliases", admin, alias, 400)
	post("providers", admin, strings.Replace(provider, "11434", "11434/v1", 1), 400)
	post("providers", admin, strings.Replace(provider, "11434", "11434/inference/go/v1/", 1), 400)
	post("providers", admin, strings.Replace(provider, "11434", "11434/inference/go?secret=fixture", 1), 400)
	post("providers", admin, provider+` {}`, 400)
	post("providers", admin, strings.Repeat(" ", 64*1024)+provider, 413)
	if mgr.Snapshot() != before {
		t.Fatal("failed request changed live config")
	}
	post("socks/check", "wrong", socks, 401)
	post("socks/recheck", "wrong", `{"name":"work"}`, 401)
	post("socks/recheck", admin, `{"name":"missing"}`, 404)
	post("socks/check", admin, strings.Replace(socks, ":1080", "", 1), 400)
	beforePreview, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	post("socks/check", admin, strings.Replace(socks, ":1080", ":1", 1), 200)
	if after, err := os.ReadFile(path); err != nil || string(after) != string(beforePreview) || mgr.Snapshot() != before {
		t.Fatal("preview changed the saved or live configuration")
	}
	request(http.MethodPut, "socks", admin, socks, 409)
	post("socks", admin, strings.Replace(socks, `"expected_ip"`, `"login":"ignored","password":"ignored","expected_ip"`, 1), 200)
	post("socks", admin, socks, 409)
	if sc := mgr.Snapshot().Cfg.Socks["work"]; !strings.Contains(sc.URL, "local:"+socksPassword) {
		t.Fatal("separate credentials overrode the URL")
	}
	updated := strings.Replace(socks, "socks5h://local:"+socksPassword+"@127.0.0.1:1080", "127.0.0.1:1", 1)
	updated = strings.Replace(updated, `"expected_ip"`, `"login":"updated","password":"`+socksPassword+`","expected_ip"`, 1)
	request(http.MethodPut, "socks", admin, updated, 200)
	if rec := e.get("/api/status", admin); rec.Code != 200 || !strings.Contains(rec.Body.String(), "socks5h://updated:"+socksPassword) || !strings.Contains(rec.Body.String(), `"state":"unchecked"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("admin status should include the editable URL and unchecked state")
	}
	if rec := e.get("/api/status", ""); rec.Code != 401 || strings.Contains(rec.Body.String(), socksPassword) {
		t.Fatal("unauthenticated status exposed credentials")
	}
	beforeRecheck, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	post("socks/recheck", admin, `{"name":"work"}`, 200)
	if status := mgr.Snapshot().Socks["work"].Status(); status.State != "blocked" || status.LastCheck == "" {
		t.Fatalf("manual check should update the live path: %+v", status)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(beforeRecheck) {
		t.Fatal("manual check changed the config file")
	}
	prefixed := strings.Replace(provider, "11434", "11434/inference/go", 1)
	post("providers", admin, strings.Replace(prefixed, `"socks":"none"`, `"socks":"work"`, 1), 200)
	post("providers", admin, provider, 409)
	post("aliases", admin, alias, 200)
	post("aliases", admin, alias, 409)
	if rec := e.get("/api/status", admin); rec.Code != 200 || strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), `"provider":"local"`) {
		t.Fatal("status should include the provider without exposing its API key")
	}
	if rec := e.get("/api/aliases", admin); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"local/test"`) {
		t.Fatal("alias missing from live API")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Clients["test"] != "fixture-client" || cfg.Aliases["local/test"].Model != "upstream-model" || cfg.Providers["local"].OpenAILike.Accounts[0].Auth.Key != key {
		t.Fatal("config changes not persisted or unrelated settings lost")
	}
	if saved := cfg.Socks["work"]; saved.ExpectedIP != "192.0.2.1" || saved.CheckInterval.String() != "1m15s" || !strings.Contains(saved.URL, socksPassword) || cfg.Providers["local"].OpenAILike.Accounts[0].Socks != "work" {
		t.Fatal("SOCKS settings or provider reference not persisted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manual := string(data) + "\n"
	write(manual)
	post("aliases", admin, strings.Replace(alias, "local/test", "local/second", 1), 409)
	if actual, err := os.ReadFile(path); err != nil || string(actual) != manual {
		t.Fatal("manual change was overwritten")
	}
}

func TestConfigCreateProviderTypes(t *testing.T) {
	const token = "test-admin"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"listen":"127.0.0.1:8787","webui_token":"test-admin"},"socks":{"work":{"url":"socks5h://127.0.0.1:1","check_url":"http://127.0.0.1:1"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := config.NewManager(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{"glm", `{"type":"glm","accounts":[{"id":"main","socks":"work","auth":{"method":"api_key","key":"fixture"},"coding_plan":true}]}`, nil},
		{"codex", `{"type":"codex","accounts":[{"id":"main","socks":"work","auth":{"method":"oauth"}}]}`, nil},
		{"zcode", `{"type":"zcode","accounts":[{"id":"main","socks":"work","auth":{"method":"oauth"}}]}`, nil},
		{"zcode-other", `{"type":"zcode","accounts":[{"id":"main","socks":"work","auth":{"method":"oauth"}}]}`, config.ErrConfigConflict},
		{"codex-direct", `{"type":"codex","accounts":[{"id":"main","socks":"none","auth":{"method":"oauth"}}]}`, config.ErrConfigInvalid},
		{"codex-custom", `{"type":"codex","auth_base_url":"https://example.invalid","accounts":[{"id":"main","socks":"work","auth":{"method":"oauth"}}]}`, config.ErrConfigInvalid},
	} {
		if err := mgr.AddProvider(token, tc.name, json.RawMessage(tc.body)); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}
