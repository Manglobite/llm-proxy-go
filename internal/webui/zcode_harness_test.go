package webui

// zfake — минимальный стаб z.ai для WebUI-тестов Ф7b: OAuth init/poll
// (режим poll SPEC 12) + PAT-цепочка. Логика повторяет zStub Ф7a
// (internal/upstream/zcode/harness_test.go) в объёме, нужном эндпоинтам
// /api/oauth/zcode/* и строкам статуса: полный набор поведений покрыт
// тестами пакета zcode, здесь — только проводка через HTTP API.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// zfakeJWT — трёхсегментный JWT с заданным payload (подпись фейковая —
// её никто не верифицирует).
func zfakeJWT(payload string) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(payload) + "." + b64("sig")
}

// zfake — стаб всех z.ai-эндпоинтов на одном origin (Base = ZcodeBase).
type zfake struct {
	srv *httptest.Server

	initFails   atomic.Bool  // init отвечает 500 (проверка 502 от start)
	holdPending atomic.Bool  // poll всегда pending (детерминизм теста «флоу ждёт»)
	loginOK     bool         // false → auth/z/login отвечает бизнес-кодом ≠0 (needs_relogin)
	pollN       atomic.Int64 // счётчик опросов poll: 1-2 → pending, далее ready
	initN       atomic.Int64 // счётчик init: authorize_url уникален на флоу (как у Ф7a-стаба)

	loginHit chan struct{} // первый сигнал: флоу дошёл до PAT-цепочки (state ready уже выставлен)
	patHit   chan struct{} // флоу выпустил PAT (access_tokens ответил)
}

// newZFake — стаб на httptest, закрытие через t.Cleanup.
func newZFake(t *testing.T, loginOK bool) *zfake {
	t.Helper()
	zf := &zfake{loginOK: loginOK, loginHit: make(chan struct{}, 1), patHit: make(chan struct{}, 1)}
	mux := http.NewServeMux()

	// init: готовый флоу, poll_interval 1s (минимум валидации Init).
	mux.HandleFunc("/api/v1/oauth/cli/init", func(w http.ResponseWriter, r *http.Request) {
		if zf.initFails.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":2007,"msg":"fake init failure"}`))
			return
		}
		n := strconv.FormatInt(zf.initN.Add(1), 10)
		writeZJSON(w, `{"code":0,"msg":"","data":{"flow_id":"flow-web-`+n+`","poll_token":"ptok-web-`+n+`",`+
			`"authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=client_web&state=webstate&n=`+n+`",`+
			`"expires_at":`+strconv.FormatInt(time.Now().Add(300*time.Second).Unix(), 10)+`,"poll_interval_sec":1}}`)
	})

	// poll: первые два опроса pending, далее ready с фиксированными токенами
	// (SPEC 7.4: обязательны token, user.user_id, zai.access_token);
	// holdPending форсирует вечное ожидание (тест детерминизма «флоу ждёт»).
	mux.HandleFunc("/api/v1/oauth/cli/poll/", func(w http.ResponseWriter, r *http.Request) {
		if zf.holdPending.Load() || zf.pollN.Add(1) <= 2 {
			writeZJSON(w, `{"code":0,"data":{"status":"pending"}}`)
			return
		}
		zai := zfakeJWT(`{"exp":` + strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10) + `}`)
		writeZJSON(w, `{"code":0,"data":{"status":"ready","token":"zjwt-web",`+
			`"user":{"user_id":"u-web","name":"WebUI","email":"web@z.ai"},`+
			`"zai":{"access_token":"`+zai+`"}}}`)
	})

	// PAT-цепочка (SPEC 7.4 шаги 1-4). loginHit — ПОСЛЕ записи ответа: к этому
	// моменту finalize уже выставил state ready (персист токенов → ready →
	// цепочка), тест может читать pending без гонок.
	mux.HandleFunc("/api/auth/z/login", func(w http.ResponseWriter, r *http.Request) {
		if zf.loginOK {
			biz := zfakeJWT(`{"typ":"biz","exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`)
			writeZJSON(w, `{"code":0,"msg":"","data":{"access_token":"`+biz+`","expires_in":3600}}`)
		} else {
			writeZJSON(w, `{"code":500,"msg":"Z.ai user information is invalid"}`)
		}
		zf.notify(zf.loginHit)
	})
	mux.HandleFunc("/api/biz/customer/getCustomerInfo", func(w http.ResponseWriter, r *http.Request) {
		writeZJSON(w, `{"code":0,"msg":"","data":{"organizations":[{"organizationId":"org-web",`+
			`"isDefault":true,"projects":[{"projectId":"prj-web","projectType":"1","isDefault":true}]}]}}`)
	})
	mux.HandleFunc("/api/biz/v1/organization/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeZJSON(w, `{"code":0,"msg":"","data":{"items":[]}}`) // ключа нет → POST-создание
			return
		}
		writeZJSON(w, `{"code":0,"msg":"","data":{"apiKey":"key-web"}}`)
	})
	mux.HandleFunc("/api/biz/v1/organization/org-web/projects/prj-web/api_keys/key-web/access_tokens",
		func(w http.ResponseWriter, r *http.Request) {
			pat := zfakeJWT(`{"token_use":"project_access","exp":` +
				strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`)
			writeZJSON(w, `{"code":0,"msg":"","data":{"enable":true,"accessToken":"`+pat+
				`","tokenType":"Bearer","expiresIn":3600}}`)
			zf.notify(zf.patHit)
		})

	zf.srv = httptest.NewServer(mux)
	t.Cleanup(zf.srv.Close)
	return zf
}

// notify — неблокирующая посылка сигнала (первый исход достаточно: тест ждёт
// факт «хоть раз случилось», повторные вызовы не нужны).
func (zf *zfake) notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// writeZJSON — ответ стаба JSON-конвертом.
func writeZJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(json.RawMessage(body))
}
