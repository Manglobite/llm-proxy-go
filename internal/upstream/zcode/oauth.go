// OAuth-провайдера zcode: init → браузер → poll (SPEC 7.4, wire-форматы —
// живой прогон docs/zcode-oauth-network.md §3/§8). PKCE нет, state серверный;
// локальный callback не нужен — код авторизации уходит серверному флоу
// zcode.z.ai. Все запросы — через клиент аккаунта (его SOCKS, R1).
package zcode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Пути OAuth поверх origin zcode_base (дефолт https://zcode.z.ai) и имя
// провайдера входа (в бандле ZCode активен ровно один — "zai").
const (
	oauthInitPath   = "/api/v1/oauth/cli/init"
	oauthPollPrefix = "/api/v1/oauth/cli/poll/"
	oauthProvider   = "zai"

	// maxOAuthBody — кап тел OAuth-ответов (токены крупные JWT, 1 МБ с запасом).
	maxOAuthBody = 1 << 20
)

// flowData — валидированный ответ init (SPEC 7.4 шаг 1).
type flowData struct {
	FlowID       string
	PollToken    string
	AuthorizeURL string
	ExpiresAt    time.Time     // unix из ответа; TTL потока 300s (docs §3.1)
	Interval     time.Duration // poll_interval_sec из ответа
}

// NewHexToken — 64 hex-символа из 32 байт crypto/rand: клиентский идентификатор
// init-запроса. Сервер его не выдаёт и не проверяет — генерируется на месте
// (docs/zcode-oauth-network.md §3.1, randomBytes(32).toString("hex")).
func NewHexToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth init: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// initResponse — конверт ответа init: {code, msg, data{...}}.
type initResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		FlowID          string `json:"flow_id"`
		PollToken       string `json:"poll_token"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"`
		PollIntervalSec int    `json:"poll_interval_sec"`
	} `json:"data"`
}

// Init — шаг 1 флоу: POST {zcodeBase}/api/v1/oauth/cli/init с
// Authorization: Bearer <64 hex> и телом {"provider":"zai"}. Валидации ответа —
// как у клиента ZCode (parseInitData, docs §3.1): authorize_url обязан быть
// https, expires_at в будущем, poll_interval_sec ≥ 1s.
func Init(ctx context.Context, client *http.Client, zcodeBase string) (*flowData, error) {
	tok, err := NewHexToken()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"provider": oauthProvider})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(zcodeBase, "/")+oauthInitPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("oauth init: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) // через SOCKS аккаунта (fail-closed)
	if err != nil {
		return nil, fmt.Errorf("oauth init: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
		return nil, fmt.Errorf("oauth init: HTTP %d: %s", resp.StatusCode, errPreview(b, maxErrPreview))
	}
	var ir initResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(&ir); err != nil {
		return nil, fmt.Errorf("oauth init: ответ не парсится: %w", err)
	}
	if ir.Code != 0 {
		return nil, fmt.Errorf("oauth init: бизнес-код %d: %s", ir.Code, ir.Msg)
	}
	d := ir.Data
	if d.FlowID == "" || d.PollToken == "" {
		return nil, errors.New("oauth init: в ответе нет flow_id/poll_token")
	}
	// authorize_url обязан быть https (docs §3.1 — клиент ZCode это же требует).
	u, err := url.Parse(d.AuthorizeURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("oauth init: authorize_url %q не https", d.AuthorizeURL)
	}
	expiresAt := time.Unix(d.ExpiresAt, 0)
	if !time.Now().Before(expiresAt) {
		return nil, fmt.Errorf("oauth init: expires_at %d в прошлом", d.ExpiresAt)
	}
	if d.PollIntervalSec < 1 {
		return nil, fmt.Errorf("oauth init: poll_interval_sec %d < 1s", d.PollIntervalSec)
	}
	return &flowData{
		FlowID:       d.FlowID,
		PollToken:    d.PollToken,
		AuthorizeURL: d.AuthorizeURL,
		ExpiresAt:    expiresAt,
		Interval:     time.Duration(d.PollIntervalSec) * time.Second,
	}, nil
}

// pollResult — результат одного опроса (SPEC 7.4 шаг 3, docs §3.3).
type pollResult struct {
	Status          string // pending | ready | failed
	Token           string // zcodejwttoken (HS256, без exp) — только ready
	UserID          string // user.user_id — обязателен в ready
	Name            string // user.name
	Email           string // user.email
	Avatar          string // user.avatar
	ZaiAccessToken  string // zai.access_token — обязателен в ready
	ZaiRefreshToken string // zai.refresh_token: у zai consumer'а нет (research §1 #1371), не персистится
}

// ErrFlowDead — poll ответил 4xx (кроме 408/429): флоу умер (чужой poll_token
// вида {"code":3004,"msg":"invalid_flow"}, истёкший/несуществующий flow_id) —
// дальнейшие опросы бессмысленны, флоу закрывается с ошибкой.
var ErrFlowDead = errors.New("oauth poll: флоу умер")

// pollEnvelope — конверт ответа poll.
type pollEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		User   struct {
			UserID string `json:"user_id"`
			Name   string `json:"name"`
			Email  string `json:"email"`
			Avatar string `json:"avatar"`
		} `json:"user"`
		Zai struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"zai"`
	} `json:"data"`
}

// Poll — один опрос GET {zcodeBase}/api/v1/oauth/cli/poll/{flow_id} с
// Authorization: Bearer <poll_token ИЗ ОТВЕТА init> (SPEC 7.4: не клиентский
// 64-hex). Возвраты:
//   - (result, nil) — pending | failed | ready (ready валидирован: token,
//     user.user_id, zai.access_token непусты — иначе ответ недействителен);
//   - (nil, ErrFlowDead) — HTTP 4xx кроме 408/429: смерть флоу;
//   - (nil, err) — сеть / 5xx / 429 / 408 / неожиданный ответ: transient,
//     ретрай на следующем тике.
func Poll(ctx context.Context, client *http.Client, zcodeBase, flowID, pollToken string) (*pollResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(zcodeBase, "/")+oauthPollPrefix+flowID, nil)
	if err != nil {
		return nil, fmt.Errorf("oauth poll: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)

	resp, err := client.Do(req) // через SOCKS аккаунта (fail-closed)
	if err != nil {
		return nil, fmt.Errorf("oauth poll: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		// SPEC 7.4/docs §3.3: 408/429 из правила «4xx — смерть» исключены —
		// ретрай на следующем тике.
		return nil, fmt.Errorf("oauth poll: HTTP %d — ретрай на следующем тике", resp.StatusCode)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrFlowDead, resp.StatusCode, errPreview(b, maxErrPreview))
	case resp.StatusCode >= 500:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
		return nil, fmt.Errorf("oauth poll: HTTP %d — ретрай на следующем тике: %s", resp.StatusCode, errPreview(b, maxErrPreview))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("oauth poll: неожиданный HTTP %d", resp.StatusCode)
	}

	var pe pollEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(&pe); err != nil {
		return nil, fmt.Errorf("oauth poll: ответ не парсится: %w", err)
	}
	// Бизнес-код ≠0 при HTTP 200 — бизнес-сбой (например 3004 из issue #718):
	// не классифицируем как смерть (клиент ZCode судит по HTTP), тикер повторит,
	// TTL потока ограничит бесконечный ретрай.
	if pe.Code != 0 {
		return nil, fmt.Errorf("oauth poll: бизнес-код %d: %s", pe.Code, pe.Msg)
	}

	res := &pollResult{
		Status:          pe.Data.Status,
		Token:           pe.Data.Token,
		UserID:          pe.Data.User.UserID,
		Name:            pe.Data.User.Name,
		Email:           pe.Data.User.Email,
		Avatar:          pe.Data.User.Avatar,
		ZaiAccessToken:  pe.Data.Zai.AccessToken,
		ZaiRefreshToken: pe.Data.Zai.RefreshToken,
	}
	switch res.Status {
	case "pending", "failed":
		return res, nil
	case "ready":
		// В ready обязательны три поля (docs §3.3): иначе ответ недействителен.
		// Ошибка не ErrFlowDead: недействительный ready — не смерть флоу,
		// ретрай на следующем тике (TTL ограничит).
		if res.Token == "" || res.UserID == "" || res.ZaiAccessToken == "" {
			return nil, errors.New("oauth poll: ready без обязательных полей (token/user_id/zai.access_token)")
		}
		return res, nil
	default:
		return nil, fmt.Errorf("oauth poll: неизвестный статус %q", res.Status)
	}
}

// parseJWTExp — exp-клеймо payload JWT без верификации подписи (образец codex
// parseIDTokenClaims: это подсказка для упреждающего refresh, не авторизация —
// авторизационные решения принимаются по HTTP-статусам апстрима).
func parseJWTExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
