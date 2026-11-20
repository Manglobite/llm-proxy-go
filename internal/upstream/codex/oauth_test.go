package codex

// Тесты OAuth-ядра (OA-1/2/3/5/11): PKCE-математика, AuthorizeURL, claims
// id_token, Store (атомарность/0600/мёрж) и обмен/refresh строго через
// клиент-Transporter аккаунта (socks.Direct — R1/I1: ни одного http.Client
// вне internal/socks).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/socks"
)

// fixVerifier/fixChallenge — фикстура OA-2: challenge = base64url(sha256(verifier)),
// ожидание посчитано независимо (захардкожено, не выводится тестируемым кодом).
const (
	fixVerifier  = "fixed-verifier-for-test-0123456789abcdef0123456789abcdef"
	fixChallenge = "eSMmUHGV2Ll5qYoTTiBRpkeo1a5DNnEIXFG5X_6NYCM"
)

func TestNewPKCEChallengeIsBase64SHA256OfVerifier(t *testing.T) {
	// Формула на фиксированной паре.
	h := sha256.Sum256([]byte(fixVerifier))
	if got := base64.RawURLEncoding.EncodeToString(h[:]); got != fixChallenge {
		t.Fatalf("challenge фикстуры = %q, ожидался %q", got, fixChallenge)
	}
	verifier, challenge, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(verifier) < 80 {
		t.Fatalf("verifier слишком короткий: %d симв. (ожидалось ~86 из 64 байт)", len(verifier))
	}
	if strings.ContainsAny(verifier, "+/=") {
		t.Fatalf("verifier содержит padding/алфавит не base64url: %q", verifier)
	}
	// Воспроизводимость формулы на сгенерированном verifier.
	h2 := sha256.Sum256([]byte(verifier))
	if got := base64.RawURLEncoding.EncodeToString(h2[:]); got != challenge {
		t.Fatalf("challenge не равен base64url(sha256(verifier))")
	}
}

func TestAuthorizeURLParameters(t *testing.T) {
	raw := AuthorizeURL("https://auth.openai.com", "http://127.0.0.1:1455/auth/callback", fixChallenge, "st-1")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" {
		t.Fatalf("base authorize url = %s", u)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             "app_EMoamEEZ73f0CkXaXp7hrann",
		"redirect_uri":          "http://127.0.0.1:1455/auth/callback",
		"code_challenge":        fixChallenge,
		"code_challenge_method": "S256",
		"state":                 "st-1",
		"scope":                 "openid profile email offline_access api.connectors.read api.connectors.invoke",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("параметр %s = %q, ожидался %q", k, q.Get(k), v)
		}
	}
}

// F3/P3: дефолт callback-порта 1455 (SPEC 7.1) применяется в ЕДИНСТВЕННОМ
// месте — RedirectURI; конфиг не резолвит поле (0 = не задано).
func TestRedirectURIDefaultAndOverride(t *testing.T) {
	if got := RedirectURI(0); got != "http://127.0.0.1:1455/auth/callback" {
		t.Fatalf("RedirectURI(0) = %q, ожидался порт 1455 (DefaultCallbackPort)", got)
	}
	if got := RedirectURI(1500); got != "http://127.0.0.1:1500/auth/callback" {
		t.Fatalf("RedirectURI(1500) = %q", got)
	}
}

func TestNewStateRandomAndDecodable(t *testing.T) {
	s1, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	s2, _ := NewState()
	if s1 == s2 {
		t.Fatal("state повторился")
	}
	b, err := base64.RawURLEncoding.DecodeString(s1)
	if err != nil || len(b) != 32 {
		t.Fatalf("state не base64url(32 байта): len=%d err=%v", len(b), err)
	}
}

// fakeJWT собирает трёхсегментный JWT с заданным payload (подпись фейковая —
// верификация и не требуется, OA-11).
func fakeJWT(t *testing.T, payload string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString([]byte("fake-signature"))
}

const claimsPayload = `{"https://api.openai.com/auth":{"chatgpt_account_id":"acc_9wBk"},` +
	`"https://api.openai.com/profile":{"email":"work@corp.ru"},"sub":"user-1"}`

func TestParseIDTokenClaims(t *testing.T) {
	aid, email, err := parseIDTokenClaims(fakeJWT(t, claimsPayload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if aid != "acc_9wBk" || email != "work@corp.ru" {
		t.Fatalf("claims = (%q, %q)", aid, email)
	}

	// Не-JWT и битый payload — ошибка, а не молчаливые пустые строки.
	for name, tok := range map[string]string{
		"не JWT":       "abc",
		"битый base64": "h.p.s",
		"битый JSON":   fakeJWT(t, "{not json"),
	} {
		if _, _, err := parseIDTokenClaims(tok); err == nil {
			t.Fatalf("%s: ожидалась ошибка", name)
		}
	}
}

// tokenStub — httptest-стаб auth-сервера; проверяет форму запроса (OA-3:
// маршрут через Transporter) и отвечает токенами.
type tokenStub struct {
	srv      *httptest.Server
	grants   []string // grant_type по порядку запросов
	hits     int
	verifier string
	code     string
}

func newTokenStub(t *testing.T, respond func(st *tokenStub, w http.ResponseWriter, r *http.Request)) *tokenStub {
	t.Helper()
	st := &tokenStub{}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		st.hits++
		st.grants = append(st.grants, r.FormValue("grant_type"))
		st.verifier = r.FormValue("code_verifier")
		st.code = r.FormValue("code")
		respond(st, w, r)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func tokenJSON(t *testing.T, access, refresh, idToken string, expiresIn int) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"access_token": access, "refresh_token": refresh, "id_token": idToken, "expires_in": expiresIn,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// directClient — клиент аккаунта для тестов: socks.Direct (маршрут через
// Transporter, R1). Для локального стаба это легальный прямой путь (I5).
func directClient() *http.Client { return socks.Direct{}.Client() }

func TestExchangeCodeThroughAccountClient(t *testing.T) {
	st := newTokenStub(t, func(st *tokenStub, w http.ResponseWriter, r *http.Request) {
		// Форма обмена — ровно SPEC 7.1.
		if r.FormValue("client_id") != "app_EMoamEEZ73f0CkXaXp7hrann" {
			t.Errorf("client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("redirect_uri") != "http://127.0.0.1:1455/auth/callback" {
			t.Errorf("redirect_uri = %q", r.FormValue("redirect_uri"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tokenJSON(t, "at-1", "rt-1", fakeJWT(t, claimsPayload), 3600))
	})
	ts, err := ExchangeCode(context.Background(), directClient(), st.srv.URL,
		"http://127.0.0.1:1455/auth/callback", "auth-code-1", fixVerifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if len(st.grants) != 1 || st.grants[0] != "authorization_code" {
		t.Fatalf("grants = %v", st.grants)
	}
	if st.code != "auth-code-1" || st.verifier != fixVerifier {
		t.Fatalf("code=%q verifier=%q", st.code, st.verifier)
	}
	if ts.AccessToken != "at-1" || ts.RefreshToken != "rt-1" {
		t.Fatalf("tokens = %+v", ts)
	}
	// expires_at из expires_in; claims из id_token.
	if !ts.ExpiresAt.After(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expires_at = %v, ожидалось ~now+1h", ts.ExpiresAt)
	}
	if ts.AccountID != "acc_9wBk" || ts.Email != "work@corp.ru" {
		t.Fatalf("claims = (%q, %q)", ts.AccountID, ts.Email)
	}
}

func TestRefreshRotatesRefreshToken(t *testing.T) {
	st := newTokenStub(t, func(st *tokenStub, w http.ResponseWriter, r *http.Request) {
		if r.FormValue("refresh_token") != "rt-old" {
			t.Errorf("refresh_token = %q", r.FormValue("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tokenJSON(t, "at-2", "rt-new", fakeJWT(t, claimsPayload), 600))
	})
	ts, err := Refresh(context.Background(), directClient(), st.srv.URL, "rt-old")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(st.grants) != 1 || st.grants[0] != "refresh_token" {
		t.Fatalf("grants = %v", st.grants)
	}
	// Ротация: новый refresh_token затирает старый.
	if ts.RefreshToken != "rt-new" {
		t.Fatalf("refresh_token не ротирован: %q", ts.RefreshToken)
	}
}

func TestExchangeRejectsMissingRefreshToken(t *testing.T) {
	st := newTokenStub(t, func(_ *tokenStub, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":60}`))
	})
	if _, err := ExchangeCode(context.Background(), directClient(), st.srv.URL, "u", "c", "v"); err == nil {
		t.Fatal("ответ без refresh_token обязан быть ошибкой (иначе теряем доступ)")
	}
}

func TestTokenEndpointErrorStatus(t *testing.T) {
	st := newTokenStub(t, func(_ *tokenStub, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	_, err := ExchangeCode(context.Background(), directClient(), st.srv.URL, "u", "c", "v")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("ожидалась ошибка со статусом, получено: %v", err)
	}
}

// ─── Store (OA-5) ───────────────────────────────────────────────────────────

func TestStoreKeyFormat(t *testing.T) {
	if got := AccountKey("codex", "myWork"); got != "codex/myWork" {
		t.Fatalf("ключ = %q, ожидался codex/myWork (SPEC 3.2)", got)
	}
}

func TestStoreAtomicWrite0600AndMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")

	// Предзапись чужого ключа: Set не теряет ключи других аккаунтов (мёрж).
	first, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Set("codex/other", TokenSet{AccessToken: "a-other", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("set other: %v", err)
	}
	if err := first.Set("codex/myWork", TokenSet{AccessToken: "a-1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("set myWork: %v", err)
	}

	// Файл валиден, tmp-файл не остался, права 0600.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var onDisk map[string]TokenSet
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("битый tokens.json: %v", err)
	}
	if len(onDisk) != 2 {
		t.Fatalf("мёрж сломан: в файле %d ключей: %v", len(onDisk), onDisk)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp-файл остался после rename")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("права tokens.json = %o, ожидались 600", perm)
	}

	// Перезапуск: Store перечитывает оба ключа; Get по формату ключа.
	second, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if ts, ok := second.Get("codex/myWork"); !ok || ts.AccessToken != "a-1" {
		t.Fatalf("Get после reopen: %+v ok=%v", ts, ok)
	}
}

func TestStoreMissingFileIsNotError(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("отсутствующий tokens.json — не ошибка, получено: %v", err)
	}
	if len(s.Keys()) != 0 {
		t.Fatalf("Keys на пустом Store: %v", s.Keys())
	}
}

func TestStoreCorruptFileIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("битый tokens.json обязан быть ошибкой старта")
	}
}
