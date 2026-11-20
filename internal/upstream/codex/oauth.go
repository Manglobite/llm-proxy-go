// Пакет upstream/codex — модуль провайдера codex (ChatGPT Codex backend,
// SPEC 7.1, ARCHITECTURE §6): OAuth PKCE (Sign in with ChatGPT), хранение
// токенов tokens.json, планировщик refresh, Sender Responses API с парсером
// квот x-codex-*.
//
// R1: все HTTP-вызовы (token exchange, refresh, апстрим /responses) идут
// через клиент аккаунта из socks.Transporter — http.Client пакет не создаёт.
// Эталон OAuth — docs/fewshots/codex-oauth.md.
package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Константы OAuth ровно из SPEC 7.1 (OA-1): публичный client codex CLI,
// пути auth-сервера и scopes. Origin'ы переопределяемы конфигом провайдера
// (auth_base_url, дефолт DefaultCodexAuthOrigin из internal/config) — стабы
// тестов/smoke подменяют auth.openai.com, прод-значения здесь не дублируются.
const (
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	oauthAuthorizePath = "/oauth/authorize"
	oauthTokenPath     = "/oauth/token"

	scopes = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

// DefaultCallbackPort — порт redirect-URI по умолчанию (SPEC 7.1: «порт по
// умолчанию 1455», конфигурируется auth.callback_port аккаунта). Дефолт
// применяется в ЕДИНСТВЕННОМ месте — RedirectURI ниже: где используется
// (построение redirect_uri OAuth-флоу), там и документировано; конфиг поле
// не трогает (0 = «не задано»).
const DefaultCallbackPort = 1455

// RedirectURI строит redirect_uri OAuth-флоу: http://127.0.0.1:{port}/auth/callback.
// callbackPort == 0 (поле не задано в конфиге) → DefaultCallbackPort —
// единственная точка применения дефолта SPEC 7.1 (F3/P3).
func RedirectURI(callbackPort int) string {
	if callbackPort == 0 {
		callbackPort = DefaultCallbackPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", callbackPort)
}

// AccountKey — ключ токенов в tokens.json (SPEC 3.2): "<provider>/<account_id>".
// Провайдер здесь — ИМЯ провайдера из config.json (как в ссылках алиасов).
func AccountKey(providerName, accountID string) string {
	return providerName + "/" + accountID
}

// NewPKCE генерирует пару verifier/challenge (S256).
func NewPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("pkce: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf) // ~86 симв., без padding
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// NewState — случайный state (base64url 32 байт).
func NewState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthorizeURL строит ссылку для браузера. authBase — origin OAuth-сервера
// (дефолт https://auth.openai.com), redirectURI = http://127.0.0.1:{port}/auth/callback.
func AuthorizeURL(authBase, redirectURI, challenge, state string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"scope":                 {scopes},
	}
	return strings.TrimRight(authBase, "/") + oauthAuthorizePath + "?" + q.Encode()
}

// TokenSet — результат обмена/refresh; то, что хранится в tokens.json.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id,omitempty"` // chatgpt_account_id
	Email        string    `json:"email,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"` // секунды; 0 = ошибка
}

// ExchangeCode обменивает authorization code на токены. client — SOCKS-клиент
// аккаунта (OA-3: обмен идёт через его путь, прямо в сети токен не ездит).
func ExchangeCode(ctx context.Context, client *http.Client, authBase, redirectURI, code, verifier string) (*TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	return doTokenRequest(ctx, client, authBase, form)
}

// Refresh обновляет access-токен. Refresh-токен ротируется: новый из ответа
// затирает старый (Store.Set перезаписывает ключ целиком).
func Refresh(ctx context.Context, client *http.Client, authBase, refreshToken string) (*TokenSet, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	return doTokenRequest(ctx, client, authBase, form)
}

func doTokenRequest(ctx context.Context, client *http.Client, authBase string, form url.Values) (*TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(authBase, "/")+oauthTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req) // через SOCKS аккаунта (fail-closed)
	if err != nil {
		return nil, fmt.Errorf("oauth token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("oauth token endpoint: status %d: %s", resp.StatusCode, body)
	}
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("oauth token response: %w", err)
	}
	if tr.ExpiresIn <= 0 {
		return nil, fmt.Errorf("oauth token response: пустой expires_in")
	}
	ts := &TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		// TTL только из expires_in ответа; expires_at считаем сами (fewshot).
		ExpiresAt: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	// Claims без верификации подписи: нам нужны только account_id и email —
	// метаданные аккаунта, на авторизационные решения они не влияют (OA-11).
	if aid, email, err := parseIDTokenClaims(tr.IDToken); err == nil {
		ts.AccountID, ts.Email = aid, email
	}
	if ts.RefreshToken == "" {
		// codex возвращает refresh_token всегда; пустой = обмен не состоялся
		// (сохранять такой TokenSet значило бы потерять доступ к аккаунту).
		return nil, fmt.Errorf("oauth token response: нет refresh_token")
	}
	if ts.AccessToken == "" {
		return nil, fmt.Errorf("oauth token response: нет access_token")
	}
	return ts, nil
}

// parseIDTokenClaims достаёт chatgpt_account_id и email из JWT payload.
// Подпись НЕ верифицируется сознательно: это данные для экрана WebUI
// (какому ChatGPT-аккаунту принадлежит токен), а не основание доверия —
// авторизационные решения принимаются по HTTP-статусам апстрима.
func parseIDTokenClaims(idToken string) (accountID, email string, err error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("id_token: не JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("id_token payload: %w", err)
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", fmt.Errorf("id_token claims: %w", err)
	}
	return claims.Auth.ChatGPTAccountID, claims.Profile.Email, nil
}

// ─── tokens.json: атомарная запись (SPEC 3.2) ───────────────────────────────

// Store — потокобезопасное хранилище tokens.json и ЕДИНСТВЕННЫЙ владелец
// файла (SPEC 3.2/13: internal/config про tokens.json не знает ничего).
//
// Жизненный цикл: Store открывается в main ОДИН раз при старте и живёт до
// остановки процесса. SIGHUP-релоад Store НЕ трогает и НЕ перечитывает файл:
// мёрж при релоаде происходит сам собой, потому что in-memory карта хранит
// все ключи, накопленные с момента старта, — ключи исчезнувших из конфига
// аккаунтов при этом НЕ удаляются молча (CF-7, ARCHITECTURE §7.3): удалить
// ключ может только явный Delete.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]TokenSet // ключ AccountKey("<provider>/<account_id>")
}

// OpenStore читает tokens.json; отсутствие файла — не ошибка (аккаунт ещё не
// проходил OAuth). Битый файл — ошибка: молча продолжать с пустой картой
// значило бы тихо потерять доступ ко всем аккаунтам.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]TokenSet{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("tokens.json: %w", err)
	}
	return s, nil
}

// Set сохраняет токены аккаунта: in-memory сразу + атомарная запись файла
// (tmp+rename на той же ФС). Мёрж-семантика: чужие ключи карты не трогаются —
// OAuth одного аккаунта не затирает токены остальных.
func (s *Store) Set(key string, ts TokenSet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = ts
	return s.writeLocked()
}

func (s *Store) Get(key string) (TokenSet, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.data[key]
	return ts, ok
}

// Delete удаляет ключ. В рантайме v1 не вызывается автоматически (CF-7:
// исчезнувшие из конфига аккаунты не чистятся молча) — точка для явных
// действий пользователя из WebUI в будущем.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return s.writeLocked()
}

// Keys — снимок списка ключей (планировщик refresh обходит аккаунты).
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	return out
}

// writeLocked — общая запись файла под mu. tmp+rename атомарны только на
// той же ФС, поэтому tmp — sibling файла (SPEC 3.2); права 0600: в файле
// лежат живые access/refresh-токены.
func (s *Store) writeLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { // токены — 0600
		return err
	}
	return os.Rename(tmp, s.path) // атомарно на той же ФС
}
