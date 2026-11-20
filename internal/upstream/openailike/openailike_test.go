// Юнит-тесты сборки тела апстрима и распознавания форм (матрица SPEC 8.2).
// Полные e2e-сценарии провайдера — в internal/server (полный стек).
package openailike

import (
	"encoding/json"
	"strings"
	"testing"

	"llm-proxy/internal/router"
	"llm-proxy/internal/upstream"
)

func sendReq(protocol string, body []byte, stream bool) *upstream.SendRequest {
	return &upstream.SendRequest{
		Protocol: protocol,
		Body:     body,
		Stream:   stream,
		Target:   router.Target{Model: "gpt-test"},
	}
}

// passthrough должен менять ТОЛЬКО model: cache_control-маркеры и прочие
// поля переживают подмену нетронутыми (TR-9).
func TestBuildBodyOpenAIPassthrough(t *testing.T) {
	in := `{"model":"work/test","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	out, err := buildUpstreamBody(sendReq(upstream.ProtocolOpenAI, []byte(in), true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if m["model"] != "gpt-test" {
		t.Fatalf("model = %v", m["model"])
	}
	// Инжект include_usage (SPEC 7.3, клетка chat→chat, только стрим).
	opts, ok := m["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options: %v", m["stream_options"])
	}
	// z.ai-маркер кеша внутри content не вырезан.
	raw := string(out)
	if !strings.Contains(raw, "cache_control") {
		t.Fatalf("cache_control потерян: %s", raw)
	}
}

func TestBuildBodyOpenAINonStreamNoInject(t *testing.T) {
	in := `{"model":"work/test","messages":[{"role":"user","content":"hi"}]}`
	out, err := buildUpstreamBody(sendReq(upstream.ProtocolOpenAI, []byte(in), false))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(string(out), "stream_options") {
		t.Fatalf("non-stream: include_usage инжектироваться не должен: %s", out)
	}
	if !strings.Contains(string(out), `"gpt-test"`) {
		t.Fatalf("model не подменён: %s", out)
	}
}

func TestBuildBodyAnthropicConverts(t *testing.T) {
	in := `{"model":"work/test","max_tokens":50,"system":"sys","messages":[{"role":"user","content":"hi"}]}`
	out, err := buildUpstreamBody(sendReq(upstream.ProtocolAnthropic, []byte(in), true))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if m["model"] != "gpt-test" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["max_tokens"] != float64(50) {
		t.Fatalf("max_tokens = %v", m["max_tokens"])
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 { // system → role:"system", user
		t.Fatalf("messages: %v", msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "sys" {
		t.Fatalf("system-сообщение: %v", first)
	}
	// BuildChat сам включает include_usage для стрима.
	opts := m["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("stream_options: %v", opts)
	}
}

func TestBuildBodyAnthropicNoMaxTokens(t *testing.T) {
	// TR-7: ParseAnthropic отклоняет запрос без max_tokens → translate-ошибка.
	in := `{"model":"work/test","messages":[{"role":"user","content":"hi"}]}`
	_, err := buildUpstreamBody(sendReq(upstream.ProtocolAnthropic, []byte(in), false))
	if err == nil {
		t.Fatal("ожидали ошибку без max_tokens")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("ошибка: %v", err)
	}
}

func TestBuildBodyUnknownProtocol(t *testing.T) {
	if _, err := buildUpstreamBody(sendReq("codex", []byte(`{}`), false)); err == nil {
		t.Fatal("ожидали ошибку для неизвестного протокола")
	}
}

func TestUpstreamErrorOfForms(t *testing.T) {
	if _, _, ok := upstreamErrorOf(upstream.SSEEvent{Data: []byte(`{"error":{"message":"boom","type":"server_error"}}`)}); !ok {
		t.Fatal("объект error не распознан")
	}
	if _, _, ok := upstreamErrorOf(upstream.SSEEvent{Event: "error", Data: []byte(`{"x":1}`)}); !ok {
		t.Fatal("event:error не распознан")
	}
	if _, _, ok := upstreamErrorOf(upstream.SSEEvent{Data: []byte(`{"choices":[{"delta":{}}]}`)}); ok {
		t.Fatal("обычный чанк принят за ошибку")
	}
	if _, _, ok := upstreamErrorOf(upstream.SSEEvent{Data: []byte("[DONE]")}); ok {
		t.Fatal("[DONE] принят за ошибку")
	}
}

func TestChatUsageOfChunkAndBody(t *testing.T) {
	u, ok := chatUsageOf([]byte(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":2}}}`))
	if !ok || u.PromptTokens != 7 || u.CompletionTokens != 9 || u.CachedInputTokens != 2 {
		t.Fatalf("usage чанка: %+v ok=%v", u, ok)
	}
	if _, ok := chatUsageOf([]byte(`{"choices":[{"delta":{}}]}`)); ok {
		t.Fatal("чанк без usage распознан как usage")
	}
}

func TestFindAccountAndCapBytes(t *testing.T) {
	acc := findAccount(nil, "a1")
	if acc != nil {
		t.Fatal("nil-аккаунты не должны находить аккаунт")
	}
	big := make([]byte, 100)
	got := capBytes(big, 10)
	if len(got) != 10+len("...<truncated>") {
		t.Fatalf("capBytes: %d", len(got))
	}
	if len(capBytes([]byte("abc"), 10)) != 3 {
		t.Fatal("capBytes изменил маленькое тело")
	}
}
