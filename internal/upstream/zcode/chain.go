// PAT-цепочка и единый PAT-кеш (SPEC 7.4 «Токены/refresh (единый PAT-кеш,
// single-flight)»). PAT — короткоживущий токен модельных запросов, только в
// памяти (SPEC 3.2а); бизнес-JWT — в zcode-tokens.json через Store.
//
// Цепочка (research-zcode-oauth.md §3.1 шаги 4–8, все запросы через клиент
// аккаунта = его SOCKS):
//  1. POST /api/auth/z/login {"token": <zai>} → бизнес-JWT + expires_in;
//     бизнес-код ≠0 → needs_relogin (интерактивный OAuth); HTTP≠200/сеть →
//     transient.
//  2. GET /api/biz/customer/getCustomerInfo с РАВНЫМ бизнес-JWT (без Bearer)
//     → дефолтные org/project, иначе первый personal (projectType≠"2");
//     только team-проекты → ошибка «team-аккаунты не поддерживаются».
//  3. GET /api/biz/v1/organization/{org}/projects/{proj}/api_keys (Bearer)
//     → найти имя «zcode-api-key», иначе POST создать
//     {"name":"zcode-api-key","usageScene":1} → apiKey (Key ID, не секрет).
//  4. POST .../api_keys/{apiKeyId}/access_tokens (Bearer)
//     {"clientType":"zcode","clientVersion":...} → PAT + expiresIn/expiresAt;
//     enable:false / 401/403/404 → fatal (мгновенная очистка PAT-кеша).
//
// Классификация сбоев — SPEC 7.4: transient (сеть/timeout/429/5xx) — старые
// токены живут до своего expiry, ретрай тикером; fatal — только провал
// access_tokens; needs_relogin — бизнес-код ≠0 от auth/z/login.
package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// patMinLife — запас «живого» PAT/бизнес-JWT: до expiry должно оставаться
// больше этого, иначе пере-issue/ре-логин (SPEC 7.4: «за 120s до expiry»).
const patMinLife = 120 * time.Second

// Шаги цепочки — в текстах ошибок и логов.
const (
	stepLogin        = "auth/z/login"
	stepCustomerInfo = "customer/getCustomerInfo"
	stepAPIKeys      = "api_keys"
	stepAccessTokens = "access_tokens"
)

// maxChainBody — кап тел ответов бизнес-API.
const maxChainBody = 1 << 20

// maxBizCache — TTL бизнес-JWT без метаданных (research §3.2: «без метаданных
// — максимум 5 минут кэша»): expires_in в ответе отсутствует/нулевый.
const maxBizCache = 5 * time.Minute

// chainKind — класс ошибки цепочки (SPEC 7.4 «Классификация сбоев цепочки»).
type chainKind int

const (
	chainTransient    chainKind = iota // сеть/timeout/429/5xx: ретрай тикером
	chainFatal                         // провал access_tokens: очистить PAT-кеш
	chainNeedsRelogin                  // бизнес-код ≠0 от auth/z/login: OAuth заново
)

// chainError — классифицированная ошибка одного шага цепочки.
type chainError struct {
	kind chainKind
	step string
	err  error
}

func (e *chainError) Error() string { return e.step + ": " + e.err.Error() }
func (e *chainError) Unwrap() error { return e.err }

// IsFatalChainErr — ошибка НЕ ретраится сама по себе: fatal (очистка PAT) или
// needs_relogin (интерактивный OAuth). OAuth-флоу показывает её в pending
// как failed; тикер логирует и ретраит (состояние сервера могло измениться).
func IsFatalChainErr(err error) bool {
	var ce *chainError
	return errors.As(err, &ce) && ce.kind != chainTransient
}

// patEntry — кеш PAT одного аккаунта. mu — single-flight ВСЕЙ цепочки:
// тикер, sender (в т.ч. 401-ретрай) и финализация OAuth-флоу сходятся в один
// EnsurePAT и ждут одну execution, вторую цепочку не запускают (SPEC 7.4).
// bizExp — expiry бизнес-JWT, известный кешу после ЕГО последней цепочки
// (копии rec у параллельных вызывающих могут быть устаревшими).
type patEntry struct {
	mu     sync.Mutex
	pat    string
	patExp time.Time
	bizExp time.Time
}

// PATCache — единый PAT-кеш на аккаунт (SPEC 7.4): в памяти, общий для
// тикера, sender'а и финализации OAuth-флоу.
type PATCache struct {
	store   *Store
	mu      sync.Mutex
	entries map[string]*patEntry
}

// NewPATCache собирает кеш поверх Store (записи бизнес-JWT делает через него).
func NewPATCache(store *Store) *PATCache {
	return &PATCache{store: store, entries: map[string]*patEntry{}}
}

// entry — запись кеша аккаунта (создаётся лениво под mu кеша).
func (c *PATCache) entry(accountKey string) *patEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[accountKey]
	if !ok {
		e = &patEntry{}
		c.entries[accountKey] = e
	}
	return e
}

// Clear — очистка PAT-кеша аккаунта: 401 от модельного эндпоинта (форс ре-issue
// на следующем EnsurePAT — иначе вернулся бы тот же «свежий» PAT). Берёт
// мьютекс записи сам (внешний вызов); EnsurePAT использует clearLocked.
func (c *PATCache) Clear(accountKey string) {
	e := c.entry(accountKey)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clearLocked()
}

// clearLocked — сброс полей записи под уже взятым мьютексом (внутренний хелпер:
// Clear изнутри EnsurePAT упёрся бы в его же deferred Lock).
func (e *patEntry) clearLocked() {
	e.pat, e.patExp, e.bizExp = "", time.Time{}, time.Time{}
}

// PATInfo — снимок кеша аккаунта: pat, expiry, есть ли живая запись. Хелпер
// тестов (и будущей проекции статуса в WebUI).
func (c *PATCache) PATInfo(accountKey string) (pat string, exp time.Time, ok bool) {
	e := c.entry(accountKey)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pat == "" {
		return "", time.Time{}, false
	}
	return e.pat, e.patExp, true
}

// EnsurePAT — единая точка получения PAT (SPEC 7.4): живой PAT (>now+120s)
// возвращается без сети; иначе вся цепочка шагов 1–4 под per-account
// мьютексом (single-flight). rec — копия записи из Store: цепочка мутирует её
// (business JWT, локация ключа) и персистит ТОЧЕЧНО через UpdateChainState,
// чтобы не затереть возможные свежие OAuth-токены.
func (c *PATCache) EnsurePAT(ctx context.Context, deps AcctDeps, rec *TokenRec) (string, error) {
	if deps.AccountKey == "" {
		return "", errors.New("pat: пустой ключ аккаунта в deps")
	}
	if deps.Client == nil {
		return "", errors.New("pat: клиент аккаунта не задан в deps")
	}
	if rec == nil || rec.ZaiAccessToken == "" {
		return "", errors.New("pat: нет zai-токена — выполните OAuth")
	}
	e := c.entry(deps.AccountKey)
	e.mu.Lock()
	defer e.mu.Unlock()
	// PAT жив (>now+120s) И бизнес-JWT не истекает → вернуть без сети. Свежесть
	// бизнес-JWT известна кешу (после его последней цепочки) либо копии
	// вызывающего; иначе цепочка — она сама сделает ре-логин (шаг 1) и
	// пере-issue PAT (шаг 4).
	now := time.Now()
	patFresh := e.pat != "" && now.Add(patMinLife).Before(e.patExp)
	bizFresh := (!e.bizExp.IsZero() && now.Add(patMinLife).Before(e.bizExp)) ||
		(rec.BusinessJWT != "" && now.Add(patMinLife).Before(rec.BusinessExpiresAt))
	if patFresh && bizFresh {
		return e.pat, nil
	}
	pat, exp, err := c.runChain(ctx, deps, rec)
	if err != nil {
		var ce *chainError
		if errors.As(err, &ce) {
			switch ce.kind {
			case chainFatal:
				// SPEC 7.4: fatal от access_tokens — мгновенная очистка кеша,
				// ретрай тикером. clearLocked, не Clear: мьютекс записи уже
				// держит этот же вызов (deferred Lock).
				e.clearLocked()
			case chainNeedsRelogin:
				// SPEC 7.4: needs_relogin — статус аккаунта (память Store),
				// сбрасывается успешным OAuth. Сверка с Store: пока эта цепочка
				// (со старой копией записи) ждала мьютекс, параллельный
				// OAuth-флоу мог записать СВЕЖИЙ zai-токен — тогда её
				// needs_relogin ложный: ставился бы поверх свежего OAuth и жил
				// до следующего входа (sender отказывает, тикер пропускает).
				// Статус не ставим, ошибка transient — следующий EnsurePAT
				// возьмёт из Store актуальный токен.
				if stored, _ := c.store.Get(deps.AccountKey); stored.ZaiAccessToken != rec.ZaiAccessToken {
					err = &chainError{chainTransient, stepLogin,
						errors.New("zai-токен ротирован параллельным OAuth, повторите")}
				} else {
					c.store.setStatus(deps.AccountKey, StatusNeedsRelogin, ce.Error())
				}
			}
		}
		return "", err
	}
	e.pat, e.patExp, e.bizExp = pat, exp, rec.BusinessExpiresAt
	return pat, nil
}

// runChain — шаги 1–4 (см. godoc пакета). Возвращает PAT и его expiry
// (приоритет expiresAt (unix) из ответа, иначе now+expiresIn — SPEC 7.4).
func (c *PATCache) runChain(ctx context.Context, deps AcctDeps, rec *TokenRec) (string, time.Time, error) {
	base := strings.TrimRight(deps.Base, "/")

	// Шаг 1: бизнес-JWT — ре-логин, если пуст или истекает раньше запаса.
	if rec.BusinessJWT == "" || !time.Now().Add(patMinLife).Before(rec.BusinessExpiresAt) {
		biz, bizExp, err := businessLogin(ctx, deps.Client, base, rec.ZaiAccessToken)
		if err != nil {
			return "", time.Time{}, err
		}
		rec.BusinessJWT, rec.BusinessExpiresAt = biz, bizExp
		if err := c.store.UpdateChainState(deps.AccountKey, func(r *TokenRec) {
			r.BusinessJWT, r.BusinessExpiresAt = biz, bizExp
		}); err != nil {
			// Токен в памяти обновлён; провал файла не валит цепочку: при
			// рестарте login повторится (zai-токен в файле уже есть).
			slog.Warn("zcode chain: запись business_jwt не удалась", "account", deps.AccountKey, "err", err)
		}
	}

	// Шаги 2–3: локация ключа и Key ID «zcode-api-key» — кэшируются в записи
	// (research §3.1 шаг 5: location кэшируется), отсутствие — выбор заново.
	org, proj, keyID := rec.Org, rec.Project, rec.APIKeyID
	if org == "" || proj == "" {
		var err error
		org, proj, err = locateProject(ctx, deps.Client, base, rec.BusinessJWT)
		if err != nil {
			return "", time.Time{}, err
		}
	}
	if keyID == "" {
		var err error
		keyID, err = ensureAPIKey(ctx, deps.Client, base, rec.BusinessJWT, org, proj)
		if err != nil {
			// Локация могла устареть (аккаунт переехал): сброс кэша — следующий
			// тик выберет org/project заново.
			if err := c.store.UpdateChainState(deps.AccountKey, func(r *TokenRec) { r.Org, r.Project = "", "" }); err != nil {
				slog.Warn("zcode chain: сброс кэша локации не удался", "account", deps.AccountKey, "err", err)
			}
			return "", time.Time{}, err
		}
	}
	if err := c.store.UpdateChainState(deps.AccountKey, func(r *TokenRec) {
		r.Org, r.Project, r.APIKeyID = org, proj, keyID
	}); err != nil {
		slog.Warn("zcode chain: запись локации ключа не удалась", "account", deps.AccountKey, "err", err)
	}

	// Шаг 4: PAT. 404 мог означать устаревший apiKeyID (ключ удалён на сервере):
	// сбрасываем кэш Key ID, следующий тик пересоздаст/перевыберет.
	pat, exp, err := issuePAT(ctx, deps.Client, base, rec.BusinessJWT, org, proj, keyID, deps.ClientVersion)
	if err != nil {
		var ce *chainError
		if errors.As(err, &ce) && ce.kind == chainFatal && strings.Contains(ce.Error(), "HTTP 404") {
			if err := c.store.UpdateChainState(deps.AccountKey, func(r *TokenRec) { r.APIKeyID = "" }); err != nil {
				slog.Warn("zcode chain: сброс кэша Key ID не удался", "account", deps.AccountKey, "err", err)
			}
		}
		return "", time.Time{}, err
	}
	return pat, exp, nil
}

// bizRequest — один запрос бизнес-API через клиент аккаунта. Authorization
// передаётся ГОТОВЫМ значением заголовка (шаг 2 требует равный бизнес-JWT без
// Bearer, шаги 3–4 — Bearer). Не-2xx не классифицирует (правила различаются
// по шагам) — возвращает статус и тело.
func bizRequest(ctx context.Context, client *http.Client, method, rawURL, authHeader string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := client.Do(req) // через SOCKS аккаунта (fail-closed)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxChainBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// bizEnvelope — общий конверт бизнес-API {code, msg, data}.
type bizEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// businessLogin — шаг 1: обмен zai-токена на бизнес-JWT (research §3.1 шаг 4).
// Провал-детектор SPEC 7.4: бизнес-код ≠0 (например 500 «Z.ai user
// information is invalid», research #1001) → needs_relogin; HTTP≠200/сеть/5xx →
// transient. TTL — expires_in ответа (SPEC 3.2а), ограниченный сверху exp-клеймом
// самого JWT (research §3.2: «TTL = min(JWT exp, expires_in)»); без метаданных —
// максимум maxBizCache.
func businessLogin(ctx context.Context, client *http.Client, base, zaiToken string) (string, time.Time, error) {
	body, _ := json.Marshal(map[string]string{"token": zaiToken})
	status, raw, err := bizRequest(ctx, client, http.MethodPost, base+"/api/auth/z/login", "", body)
	if err != nil {
		return "", time.Time{}, &chainError{chainTransient, stepLogin, err}
	}
	if status != http.StatusOK {
		return "", time.Time{}, &chainError{chainTransient, stepLogin,
			fmt.Errorf("HTTP %d: %s", status, errPreview(raw, maxErrPreview))}
	}
	var env struct {
		bizEnvelope
		Data struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", time.Time{}, &chainError{chainTransient, stepLogin, fmt.Errorf("ответ не парсится: %w", err)}
	}
	if env.Code != 0 {
		// needs_relogin: zai-токен сервер не признал — интерактивный OAuth заново
		// (refresh-эндпоинта у z.ai нет, research §5.4).
		return "", time.Time{}, &chainError{chainNeedsRelogin, stepLogin,
			fmt.Errorf("бизнес-код %d: %s", env.Code, env.Msg)}
	}
	if env.Data.AccessToken == "" {
		return "", time.Time{}, &chainError{chainTransient, stepLogin, errors.New("в ответе нет access_token")}
	}
	exp := time.Now().Add(maxBizCache)
	if env.Data.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second)
	}
	if e, ok := parseJWTExp(env.Data.AccessToken); ok && e.Before(exp) {
		exp = e
	}
	return env.Data.AccessToken, exp, nil
}

// Формы ответа getCustomerInfo (research §3.1 шаг 5: organizations[] →
// projects[], выбор дефолтных или первого personal projectType≠"2"). Точные
// имена ID-полей доками не зафиксированы (⚠ проверка) — парсим оба
// распространённых написания (organizationId|id, projectId|id, projectType|
// project_type, isDefault|is_default).
type customerOrg struct {
	OrganizationID string        `json:"organizationId"`
	ID             string        `json:"id"`
	IsDefault      bool          `json:"isDefault"`
	DefaultAlt     bool          `json:"is_default"`
	Projects       []customerPrj `json:"projects"`
}

type customerPrj struct {
	ProjectID   string `json:"projectId"`
	ID          string `json:"id"`
	ProjectType string `json:"projectType"`
	TypeAlt     string `json:"project_type"`
	IsDefault   bool   `json:"isDefault"`
}

// orgID/prjID/personal/projectType — унификация альтернативных написаний полей.
func (o *customerOrg) orgID() string {
	if o.OrganizationID != "" {
		return o.OrganizationID
	}
	return o.ID
}

func (o *customerOrg) defaultOrg() bool { return o.IsDefault || o.DefaultAlt }

func (p *customerPrj) prjID() string {
	if p.ProjectID != "" {
		return p.ProjectID
	}
	return p.ID
}

func (p *customerPrj) personal() bool { return p.projectType() != "2" }

func (p *customerPrj) projectType() string {
	if p.ProjectType != "" {
		return p.ProjectType
	}
	return p.TypeAlt
}

// locateProject — шаг 2: org/project из getCustomerInfo с РАВНЫМ бизнес-JWT
// (research §3.1 шаг 5: «customerInfo 要求裸业务 token» — без Bearer). Выбор:
// дефолтные org/project, иначе первый personal; только team → ошибка
// «team-аккаунты не поддерживаются» (SPEC 7.4, бэклог team-ветки).
func locateProject(ctx context.Context, client *http.Client, base, bizJWT string) (string, string, error) {
	status, raw, err := bizRequest(ctx, client, http.MethodGet, base+"/api/biz/customer/getCustomerInfo", bizJWT, nil)
	if err != nil {
		return "", "", &chainError{chainTransient, stepCustomerInfo, err}
	}
	if status != http.StatusOK {
		return "", "", &chainError{chainTransient, stepCustomerInfo,
			fmt.Errorf("HTTP %d: %s", status, errPreview(raw, maxErrPreview))}
	}
	var env struct {
		bizEnvelope
		Data struct {
			Organizations []customerOrg `json:"organizations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", "", &chainError{chainTransient, stepCustomerInfo, fmt.Errorf("ответ не парсится: %w", err)}
	}
	if env.Code != 0 {
		return "", "", &chainError{chainTransient, stepCustomerInfo,
			fmt.Errorf("бизнес-код %d: %s", env.Code, env.Msg)}
	}
	org, proj, ok := pickProject(env.Data.Organizations)
	if !ok {
		return "", "", &chainError{chainFatal, stepCustomerInfo,
			errors.New("team-аккаунты не поддерживаются: personal-проектов нет (projectType≠2)")}
	}
	return org, proj, nil
}

// pickProject — выбор дефолтных org/project, иначе первый personal
// (projectType≠"2"), research §3.1 шаг 5. Первый проход — по дефолтным org,
// второй — по любым.
func pickProject(orgs []customerOrg) (orgID, prjID string, ok bool) {
	for _, preferDefault := range []bool{true, false} {
		for i := range orgs {
			o := &orgs[i]
			if preferDefault && !o.defaultOrg() {
				continue
			}
			if prj, found := pickProjectOf(o); found {
				return o.orgID(), prj, true
			}
		}
	}
	return "", "", false
}

// pickProjectOf — проект внутри org: дефолтный personal, иначе первый personal.
func pickProjectOf(o *customerOrg) (string, bool) {
	var firstPersonal string
	for i := range o.Projects {
		p := &o.Projects[i]
		if !p.personal() {
			continue
		}
		if p.IsDefault {
			return p.prjID(), true
		}
		if firstPersonal == "" {
			firstPersonal = p.prjID()
		}
	}
	return firstPersonal, firstPersonal != ""
}

// apiKeyItem — элемент списка ключей (research §3.1 шаг 6: возвращаемый apiKey
// — не секрет, а Key ID).
type apiKeyItem struct {
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
}

// parseAPIKeys — форма списка ключей (⚠ точная форма доками не зафиксирована):
// пробуем data.items[] | data.list[] | data[]; ok=false — форма не распознана
// (создавать ключ вслепую при нераспознанном ответе значило бы плодить дубли).
func parseAPIKeys(raw []byte) (items []apiKeyItem, ok bool) {
	var obj struct {
		Data struct {
			Items []apiKeyItem `json:"items"`
			List  []apiKeyItem `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return append(obj.Data.Items, obj.Data.List...), true
	}
	var arr struct {
		Data []apiKeyItem `json:"data"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr.Data, true
	}
	return nil, false
}

// ensureAPIKey — шаг 3: Key ID существующего «zcode-api-key», иначе создание
// POST {"name":"zcode-api-key","usageScene":1} (research §3.1 шаг 6).
func ensureAPIKey(ctx context.Context, client *http.Client, base, bizJWT, org, proj string) (string, error) {
	// org/proj — ID с сервера; эскейпим как сегменты пути на всякий случай.
	u := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys",
		base, urlPathEscape(org), urlPathEscape(proj))

	status, raw, err := bizRequest(ctx, client, http.MethodGet, u, "Bearer "+bizJWT, nil)
	if err != nil {
		return "", &chainError{chainTransient, stepAPIKeys, err}
	}
	if status == http.StatusOK {
		items, ok := parseAPIKeys(raw)
		if !ok {
			return "", &chainError{chainTransient, stepAPIKeys, errors.New("форма списка api_keys не распознана")}
		}
		for _, it := range items {
			if it.Name == "zcode-api-key" && it.APIKey != "" {
				return it.APIKey, nil
			}
		}
	} else {
		// Не-2xx списка — transient (SPEC 7.4 fatal оговорён только для
		// access_tokens); устаревшая локация уже сброшена в runChain.
		return "", &chainError{chainTransient, stepAPIKeys,
			fmt.Errorf("HTTP %d: %s", status, errPreview(raw, maxErrPreview))}
	}

	// Ключа нет — создать.
	body, _ := json.Marshal(map[string]any{"name": "zcode-api-key", "usageScene": 1})
	status, raw, err = bizRequest(ctx, client, http.MethodPost, u, "Bearer "+bizJWT, body)
	if err != nil {
		return "", &chainError{chainTransient, stepAPIKeys, err}
	}
	if status != http.StatusOK {
		return "", &chainError{chainTransient, stepAPIKeys,
			fmt.Errorf("HTTP %d: %s", status, errPreview(raw, maxErrPreview))}
	}
	var env struct {
		bizEnvelope
		Data struct {
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", &chainError{chainTransient, stepAPIKeys, fmt.Errorf("ответ не парсится: %w", err)}
	}
	if env.Code != 0 {
		return "", &chainError{chainTransient, stepAPIKeys,
			fmt.Errorf("бизнес-код %d: %s", env.Code, env.Msg)}
	}
	if env.Data.APIKey == "" {
		return "", &chainError{chainTransient, stepAPIKeys, errors.New("в ответе создания нет apiKey")}
	}
	return env.Data.APIKey, nil
}

// urlPathEscape — эскейп одного сегмента пути.
func urlPathEscape(s string) string { return strings.ReplaceAll(s, "/", "%2F") }

// issuePAT — шаг 4: POST access_tokens {"clientType":"zcode","clientVersion":...}
// (research §3.1 шаг 7). enable:false / HTTP 401/403/404 → fatal (SPEC 7.4:
// «мгновенная очистка PAT-кеша»); остальное — transient.
func issuePAT(ctx context.Context, client *http.Client, base, bizJWT, org, proj, keyID, clientVersion string) (string, time.Time, error) {
	u := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys/%s/access_tokens",
		base, urlPathEscape(org), urlPathEscape(proj), urlPathEscape(keyID))
	body, _ := json.Marshal(map[string]string{
		"clientType":    "zcode",
		"clientVersion": clientVersion,
	})

	status, raw, err := bizRequest(ctx, client, http.MethodPost, u, "Bearer "+bizJWT, body)
	if err != nil {
		return "", time.Time{}, &chainError{chainTransient, stepAccessTokens, err}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		return "", time.Time{}, &chainError{chainFatal, stepAccessTokens,
			fmt.Errorf("HTTP %d: %s", status, errPreview(raw, maxErrPreview))}
	case status != http.StatusOK:
		return "", time.Time{}, &chainError{chainTransient, stepAccessTokens,
			fmt.Errorf("HTTP %d: %s", status, errPreview(raw, maxErrPreview))}
	}
	var env struct {
		bizEnvelope
		Data struct {
			Enable      bool   `json:"enable"`
			AccessToken string `json:"accessToken"`
			ExpiresIn   int    `json:"expiresIn"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", time.Time{}, &chainError{chainTransient, stepAccessTokens, fmt.Errorf("ответ не парсится: %w", err)}
	}
	if !env.Data.Enable {
		// SPEC 7.4: enable:false — fatal; повтор — статус-ошибка аккаунта
		// (классификация fatal, запись ключа в Store сохраняется для диагностики).
		return "", time.Time{}, &chainError{chainFatal, stepAccessTokens,
			fmt.Errorf("enable:false (бизнес-код %d: %s)", env.Code, env.Msg)}
	}
	if env.Data.AccessToken == "" {
		return "", time.Time{}, &chainError{chainTransient, stepAccessTokens, errors.New("в ответе нет accessToken")}
	}
	// Expiry: приоритет expiresAt (unix), иначе now+expiresIn (SPEC 7.4).
	var exp time.Time
	if env.Data.ExpiresAt > 0 {
		exp = time.Unix(env.Data.ExpiresAt, 0)
	} else if env.Data.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second)
	} else {
		return "", time.Time{}, &chainError{chainTransient, stepAccessTokens,
			errors.New("в ответе нет ни expiresAt, ни expiresIn")}
	}
	return env.Data.AccessToken, exp, nil
}
