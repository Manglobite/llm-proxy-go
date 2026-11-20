package zcode

// Тесты wire-форматов OAuth init/poll (docs/zcode-oauth-network.md §3, SPEC 7.4):
// валидации init (https/expires_at/interval), статусы poll (pending/ready/
// failed/400-смерть/5xx-transient), обязательные поля ready, parse exp-клейма.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInitHappyPathAndWire(t *testing.T) {
	st := newZStub(t)
	fd, err := Init(context.Background(), directClient(), st.srv.URL)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if fd.FlowID != "flow-1" || fd.PollToken != "ptok-1" {
		t.Fatalf("flowData: %+v", fd)
	}
	if fd.AuthorizeURL != "https://chat.z.ai/api/oauth/authorize?client_id=client_x&state=srvstate" {
		t.Fatalf("authorize_url: %q", fd.AuthorizeURL)
	}
	if fd.Interval != 2*time.Second {
		t.Fatalf("interval = %v, ожидалось 2s", fd.Interval)
	}
	if !time.Now().Add(290 * time.Second).Before(fd.ExpiresAt) {
		t.Fatalf("expires_at не в будущем: %v", fd.ExpiresAt)
	}

	req := st.lastReq("POST", "/init")
	if req == nil {
		t.Fatal("init-запрос не дошёл до стаба")
	}
	if req.Body != `{"provider":"zai"}` {
		t.Fatalf("тело init = %q, ожидалось {\"provider\":\"zai\"}", req.Body)
	}
	bearer := strings.TrimPrefix(req.Auth, "Bearer ")
	if !strings.HasPrefix(req.Auth, "Bearer ") || !hex64RE.MatchString(bearer) {
		t.Fatalf("Authorization init должен быть Bearer <64 hex>, получено %q", req.Auth)
	}
}

func TestInitValidations(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "authorize_url не https",
			body:    `{"code":0,"data":{"flow_id":"f","poll_token":"p","authorize_url":"http://chat.z.ai/x","expires_at":` + futureUnix() + `,"poll_interval_sec":2}}`,
			wantSub: "не https",
		},
		{
			name:    "expires_at в прошлом",
			body:    `{"code":0,"data":{"flow_id":"f","poll_token":"p","authorize_url":"https://chat.z.ai/x","expires_at":` + pastUnix() + `,"poll_interval_sec":2}}`,
			wantSub: "в прошлом",
		},
		{
			name:    "poll_interval_sec < 1",
			body:    `{"code":0,"data":{"flow_id":"f","poll_token":"p","authorize_url":"https://chat.z.ai/x","expires_at":` + futureUnix() + `,"poll_interval_sec":0}}`,
			wantSub: "poll_interval_sec",
		},
		{
			name:    "нет flow_id",
			body:    `{"code":0,"data":{"poll_token":"p","authorize_url":"https://chat.z.ai/x","expires_at":` + futureUnix() + `,"poll_interval_sec":2}}`,
			wantSub: "flow_id",
		},
		{
			name:    "бизнес-код не 0",
			body:    `{"code":3004,"msg":"boom","data":{}}`,
			wantSub: "бизнес-код 3004",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newZStub(t)
			st.onInit = func(w http.ResponseWriter, r *hitReq) { jsonResponse(w, 200, tc.body) }
			_, err := Init(context.Background(), directClient(), st.srv.URL)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("ожидали ошибку с %q, получили: %v", tc.wantSub, err)
			}
		})
	}
}

func TestInitHTTPError(t *testing.T) {
	st := newZStub(t)
	st.onInit = func(w http.ResponseWriter, r *hitReq) { jsonResponse(w, 500, `{"code":2007,"msg":"http error"}`) }
	_, err := Init(context.Background(), directClient(), st.srv.URL)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("ожидали ошибку HTTP 500, получили: %v", err)
	}
}

// TestPollStatuses — все статусы poll и классификация HTTP-исходов.
func TestPollStatuses(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		st := newZStub(t)
		res, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if res.Status != "pending" {
			t.Fatalf("status = %q", res.Status)
		}
		// poll ходит с poll_token ИЗ ОТВЕТА init (SPEC 7.4), не с клиентским hex.
		if req := st.lastReq("GET", "/poll"); req == nil || req.Auth != "Bearer ptok-1" {
			t.Fatalf("Authorization poll = %q, ожидался Bearer ptok-1", req.Auth)
		}
	})

	t.Run("ready с обязательными полями", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 200, `{"code":0,"data":{"status":"ready","token":"zjwt-1",`+
				`"user":{"user_id":"u-1","name":"Ivan","email":"i@z.ai","avatar":"a.png"},`+
				`"zai":{"access_token":"zai-tok","refresh_token":"ignored"}}}`)
		}
		res, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if res.Status != "ready" || res.Token != "zjwt-1" || res.UserID != "u-1" ||
			res.Email != "i@z.ai" || res.Name != "Ivan" || res.ZaiAccessToken != "zai-tok" {
			t.Fatalf("ready разобран неверно: %+v", res)
		}
	})

	t.Run("failed", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 200, `{"code":0,"data":{"status":"failed"}}`)
		}
		res, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if res.Status != "failed" {
			t.Fatalf("status = %q", res.Status)
		}
	})

	t.Run("400 invalid_flow — смерть флоу", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 400, `{"code":3004,"msg":"invalid_flow"}`)
		}
		_, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "other-token")
		if !errors.Is(err, ErrFlowDead) {
			t.Fatalf("ожидали ErrFlowDead, получили: %v", err)
		}
	})

	t.Run("5xx — transient, не смерть", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			jsonResponse(w, 500, `{"error":"server down"}`)
		}
		_, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
		if err == nil || errors.Is(err, ErrFlowDead) {
			t.Fatalf("5xx должен быть transient (ретрай), получили: %v", err)
		}
	})

	t.Run("429 — ретрай, не смерть", func(t *testing.T) {
		st := newZStub(t)
		st.onPoll = func(w http.ResponseWriter, r *hitReq) {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		_, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
		if err == nil || errors.Is(err, ErrFlowDead) {
			t.Fatalf("429 должен быть ретраем, получили: %v", err)
		}
	})

	t.Run("сеть — transient", func(t *testing.T) {
		st := newZStub(t)
		st.srv.Close() // обрыв фейк-сервера
		_, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
		if err == nil || errors.Is(err, ErrFlowDead) {
			t.Fatalf("сетевой обрыв должен быть transient, получили: %v", err)
		}
	})
}

// TestPollReadyRequiresFields — docs §3.3: в ready обязательны token,
// user.user_id, zai.access_token.
func TestPollReadyRequiresFields(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"нет token", `"status":"ready","user":{"user_id":"u"},"zai":{"access_token":"z"}}`},
		{"нет user_id", `"status":"ready","token":"t","user":{},"zai":{"access_token":"z"}}`},
		{"нет zai.access_token", `"status":"ready","token":"t","user":{"user_id":"u"},"zai":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newZStub(t)
			st.onPoll = func(w http.ResponseWriter, r *hitReq) {
				jsonResponse(w, 200, `{"code":0,"data":{`+tc.data+`}}`)
			}
			_, err := Poll(context.Background(), directClient(), st.srv.URL, "flow-1", "ptok-1")
			if err == nil || errors.Is(err, ErrFlowDead) {
				t.Fatalf("неполный ready обязан быть ошибкой (не смерть), получили: %v", err)
			}
			if !strings.Contains(err.Error(), "обязательных полей") {
				t.Fatalf("текст ошибки должен называть обязательные поля: %v", err)
			}
		})
	}
}

// TestParseJWTExp — exp-клеймо без верификации (zai_expires_at — опциональная
// подсказка, SPEC 3.2а).
func TestParseJWTExp(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	tok := fakeJWT(t, `{"exp":`+strconv.FormatInt(exp.Unix(), 10)+`,"api_key":"k"}`)
	got, ok := parseJWTExp(tok)
	if !ok || !got.Equal(exp) {
		t.Fatalf("exp = %v (ok=%v), ожидалось %v", got, ok, exp)
	}
	if _, ok := parseJWTExp("не-jwt"); ok {
		t.Fatal("не-JWT не должен парситься")
	}
	if _, ok := parseJWTExp(fakeJWT(t, `{"api_key":"k"}`)); ok {
		t.Fatal("JWT без exp не должен парситься")
	}
}

func futureUnix() string { return strconv.FormatInt(time.Now().Add(300*time.Second).Unix(), 10) }
func pastUnix() string   { return strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) }
