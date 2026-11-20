package zcode

// Тестовая обвязка пакета: один httptest-стаб со ВСЕМИ эндпоинтами
// (OAuth init/poll, PAT-цепочка, модельные пути — origin'ы base и zcode_base
// совпадают с URL стаба, как в smoke) и мелкие хелперы. Внешних вызовов в
// тестах быть не должно (CODING 1.9); транспорт провайдера — socks.Direct{}
// (как в glm-тестах: http.Client создаёт только internal/socks, I1).

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-proxy/internal/socks"
)

// recorder — алиас httptest.ResponseRecorder: W провайдера пишет сюда.
type recorder = httptest.ResponseRecorder

func newRecorder() *recorder { return httptest.NewRecorder() }

func directClient() *http.Client { return socks.Direct{}.Client() }

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// uuidV4RE — форма device_mid (RFC 4122, версия 4).
var uuidV4RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// hex64RE — форма клиентского 64-hex токена init-запроса.
var hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fakeJWT собирает трёхсегментный JWT с заданным payload (подпись фейковая —
// её никто не верифицирует).
func fakeJWT(t *testing.T, payload string) string {
	t.Helper()
	return fakeJWTStatic(payload)
}

// fakeJWTStatic — то же без testing.T (для тел ответов стаба).
func fakeJWTStatic(payload string) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(payload) + "." + b64("sig")
}

// hitReq — что увидел стаб на одном вызове.
type hitReq struct {
	Method string
	Path   string // нормализованный путь (см. normPath)
	Auth   string // сырой заголовок Authorization
	APIKey string // x-api-key
	AppVer string // x-zcode-app-version
	Plat   string // x-platform
	DevMid string // x-device-mid
	Body   string
}

// zStub — httptest-сервер со всеми эндпоинтами zcode/z.ai; поведение каждого
// переопределяется on*-колбэком (nil → дефолт «всё хорошо»), вызовы считаются
// по «METHOD путь» с нормализацией переменных сегментов.
type zStub struct {
	srv *httptest.Server

	mu    sync.Mutex
	hits  map[string]int
	last  map[string]*hitReq
	order []string

	// управляемое поведение
	onInit         func(w http.ResponseWriter, r *hitReq)
	onPoll         func(w http.ResponseWriter, r *hitReq)
	onLogin        func(w http.ResponseWriter, r *hitReq)
	onCustomer     func(w http.ResponseWriter, r *hitReq)
	onKeysGET      func(w http.ResponseWriter, r *hitReq)
	onKeysPOST     func(w http.ResponseWriter, r *hitReq)
	onAccessTokens func(w http.ResponseWriter, r *hitReq)
	onModel        func(w http.ResponseWriter, r *hitReq) // модельные пути
}

func newZStub(t *testing.T) *zStub {
	st := &zStub{hits: map[string]int{}, last: map[string]*hitReq{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/oauth/cli/init", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := &hitReq{Method: r.Method, Path: "/init", Auth: r.Header.Get("Authorization"), Body: string(b)}
		st.record(req)
		if st.onInit != nil {
			st.onInit(w, req)
			return
		}
		st.defaultResponse(w, req)
	})
	mux.HandleFunc("/api/v1/oauth/cli/poll/", func(w http.ResponseWriter, r *http.Request) {
		req := &hitReq{Method: r.Method, Path: "/poll", Auth: r.Header.Get("Authorization")}
		st.record(req)
		if st.onPoll != nil {
			st.onPoll(w, req)
			return
		}
		st.defaultResponse(w, req)
	})
	mux.HandleFunc("/api/auth/z/login", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := &hitReq{Method: r.Method, Path: "/api/auth/z/login", Auth: r.Header.Get("Authorization"), Body: string(b)}
		st.record(req)
		if st.onLogin != nil {
			st.onLogin(w, req)
			return
		}
		st.defaultResponse(w, req)
	})
	mux.HandleFunc("/api/biz/customer/getCustomerInfo", func(w http.ResponseWriter, r *http.Request) {
		req := &hitReq{Method: r.Method, Path: "/customer", Auth: r.Header.Get("Authorization")}
		st.record(req)
		if st.onCustomer != nil {
			st.onCustomer(w, req)
			return
		}
		st.defaultResponse(w, req)
	})
	mux.HandleFunc("/api/biz/v1/organization/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := &hitReq{
			Method: r.Method,
			Path:   normPath(r.URL.Path),
			Auth:   r.Header.Get("Authorization"),
			Body:   string(b),
		}
		st.record(req)
		switch {
		case req.Path == "/access_tokens" && st.onAccessTokens != nil:
			st.onAccessTokens(w, req)
		case req.Path == "/api_keys" && r.Method == http.MethodPost && st.onKeysPOST != nil:
			st.onKeysPOST(w, req)
		case req.Path == "/api_keys" && st.onKeysGET != nil:
			st.onKeysGET(w, req)
		default:
			st.defaultResponse(w, req)
		}
	})
	for _, model := range []string{"/api/anthropic/v1/messages", "/api/paas/v4/chat/completions",
		"/api/v1/zcode-plan", "/api/v1/zcode-plan/anthropic"} {
		mux.HandleFunc(model, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			req := &hitReq{
				Method: r.Method,
				Path:   r.URL.Path,
				Auth:   r.Header.Get("Authorization"),
				APIKey: r.Header.Get("x-api-key"),
				AppVer: r.Header.Get("x-zcode-app-version"),
				Plat:   r.Header.Get("x-platform"),
				DevMid: r.Header.Get("x-device-mid"),
				Body:   string(b),
			}
			st.record(req)
			if st.onModel != nil {
				st.onModel(w, req)
				return
			}
			st.defaultResponse(w, req)
		})
	}

	st.srv = httptest.NewServer(mux)
	t.Cleanup(st.srv.Close)
	return st
}

func (st *zStub) record(req *hitReq) {
	st.mu.Lock()
	defer st.mu.Unlock()
	key := req.Method + " " + req.Path
	st.hits[key]++
	st.last[key] = req
	st.order = append(st.order, key)
}

// normPath — нормализация переменных сегментов для счётчиков.
func normPath(p string) string {
	switch {
	case strings.HasPrefix(p, "/api/v1/oauth/cli/poll/"):
		return "/poll"
	case strings.HasSuffix(p, "/access_tokens"):
		return "/access_tokens"
	case strings.HasSuffix(p, "/api_keys"):
		return "/api_keys"
	}
	return p
}

// count — число вызовов «METHOD путь» (путь нормализован).
func (st *zStub) count(method, path string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hits[method+" "+path]
}

// lastReq — последний запрос «METHOD путь».
func (st *zStub) lastReq(method, path string) *hitReq {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.last[method+" "+path]
}

// jsonResponse — ответ JSON-конвертом.
func jsonResponse(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// defaultResponse — «всё хорошо» для каждого эндпоинта (формы — SPEC 7.4).
func (st *zStub) defaultResponse(w http.ResponseWriter, r *hitReq) {
	switch r.Path {
	case "/init":
		jsonResponse(w, 200, `{"code":0,"msg":"","data":{"flow_id":"flow-1","poll_token":"ptok-1",`+
			`"authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=client_x&state=srvstate",`+
			`"expires_at":`+strconv.FormatInt(time.Now().Add(300*time.Second).Unix(), 10)+`,"poll_interval_sec":2}}`)
	case "/poll":
		jsonResponse(w, 200, `{"code":0,"data":{"status":"pending"}}`)
	case "/api/auth/z/login":
		jsonResponse(w, 200, `{"code":0,"msg":"","data":{"access_token":"`+
			fakeJWTStatic(`{"typ":"biz","exp":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+`}`)+
			`","expires_in":3600}}`)
	case "/customer":
		jsonResponse(w, 200, `{"code":0,"msg":"","data":{"organizations":[{"organizationId":"org-1",`+
			`"isDefault":true,"projects":[{"projectId":"prj-1","projectType":"1","isDefault":true}]}]}}`)
	case "/api_keys":
		if r.Method == http.MethodGet {
			// Дефолт — ключа нет: полный прогон цепочки делает GET→POST
			// (создание), что и считает тест полного успеха.
			jsonResponse(w, 200, `{"code":0,"msg":"","data":{"items":[]}}`)
			return
		}
		jsonResponse(w, 200, `{"code":0,"msg":"","data":{"apiKey":"key-1"}}`)
	case "/access_tokens":
		jsonResponse(w, 200, `{"code":0,"msg":"","data":{"enable":true,"accessToken":"`+
			fakeJWTStatic(`{"token_use":"project_access"}`)+`","tokenType":"Bearer","expiresIn":3600}}`)
	default:
		// Модельные пути: дефолт — openai chat SSE (тесты переопределяют onModel).
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\n" +
			"data: [DONE]\n\n"))
	}
}
