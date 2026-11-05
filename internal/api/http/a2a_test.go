package http_test

// A2A 路由传输层测试（无 DB）：well-known 发现、每 Agent card、404/401、
// JSON-RPC 信封错误（解析/协议版本/方法分派经真实 app/a2a.Service）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apihttp "ir-agent-server/internal/api/http"
	"ir-agent-server/internal/app/a2a"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
)

type fakeA2AAgents struct{ agents map[string]*agent.Agent }

func (f fakeA2AAgents) GetBySlug(_ context.Context, slug string) (*agent.Agent, error) {
	if stored, ok := f.agents[slug]; ok {
		return stored, nil
	}
	return nil, agent.ErrNotFound
}

type memA2ATasks struct{ finished map[string]string }

func (m *memA2ATasks) Start(context.Context, string, string, int64, string, string,
	[]byte) (bool, error) {
	return true, nil
}

func (m *memA2ATasks) Finish(_ context.Context, _, taskID, status string, _ int64,
	_ *string, _ []byte, _ *string) error {
	m.finished[taskID] = status
	return nil
}

func (m *memA2ATasks) Get(context.Context, string, string) (string, []byte, bool, error) {
	return "", nil, false, nil
}

type stubA2ADispatcher struct{}

func (stubA2ADispatcher) Execute(context.Context, string, *platform.TaskContext,
	any, time.Duration) (*platform.ProcessedOutput, error) {
	return &platform.ProcessedOutput{Summary: "a2a-ok"}, nil
}

func a2aTestAgent(slug string, enabled bool) *agent.Agent {
	return &agent.Agent{
		Slug: slug, Name: "A2A 测试台", Enabled: enabled,
		Binding:       exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
		SigningSecret: []byte("0123456789abcdef-a2a-test-secret"),
		TimeoutSecs:   60,
	}
}

func newA2AServer(t *testing.T, requireAuth bool, defaultSlug string) *httptest.Server {
	t.Helper()
	service := &a2a.Service{
		Tasks:      &memA2ATasks{finished: map[string]string{}},
		Dispatcher: stubA2ADispatcher{},
	}
	router := apihttp.A2ARouter(apihttp.A2ADeps{
		Service: service,
		Agents: fakeA2AAgents{agents: map[string]*agent.Agent{
			"it-a2a": a2aTestAgent("it-a2a", true),
			"it-off": a2aTestAgent("it-off", false),
		}},
		Card:                a2a.CardConfig{BasePath: "/a2a", RequireBearerAuth: requireAuth},
		RequireBearerAuth:   requireAuth,
		MaxRequestBodyBytes: 1 << 20,
		DefaultCardSlug:     defaultSlug,
		PublicBaseURL:       "https://a2a.example.test",
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func postRPC(t *testing.T, url string, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	request, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response, decoded
}

func TestA2ACardDiscovery(t *testing.T) {
	server := newA2AServer(t, false, "it-a2a")

	// root well-known（新旧两个路径）指向默认 Agent。
	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			t.Errorf("%s = %d", path, response.StatusCode)
		}
		if ct := response.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
			t.Errorf("%s content-type = %s", path, ct)
		}
		var card map[string]any
		if err := json.NewDecoder(response.Body).Decode(&card); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if card["name"] != "A2A 测试台" {
			t.Errorf("%s card = %v", path, card)
		}
	}

	// 每 Agent card 路径。
	response, err := http.Get(server.URL + "/a2a/it-a2a/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Errorf("per-agent card = %d", response.StatusCode)
	}

	// 停用 / 未知 Agent → 404。
	for _, slug := range []string{"it-off", "ghost"} {
		response, err := http.Get(server.URL + "/a2a/" + slug + "/agent-card.json")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Errorf("card %s = %d", slug, response.StatusCode)
		}
	}

	// 未配置默认 Agent 时 root well-known → 404（错误信息指引配置项）。
	noDefault := newA2AServer(t, false, "")
	response, err = http.Get(noDefault.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 404 {
		t.Errorf("no-default well-known = %d", response.StatusCode)
	}
}

func TestA2ARPCTransportErrors(t *testing.T) {
	server := newA2AServer(t, false, "it-a2a")
	rpcURL := server.URL + "/a2a/it-a2a"

	// 非法 JSON → HTTP 200 + JSON-RPC 解析错误信封。
	response, envelope := postRPC(t, rpcURL, "{not-json", nil)
	if response.StatusCode != 200 {
		t.Errorf("bad json status = %d", response.StatusCode)
	}
	rpcErr, _ := envelope["error"].(map[string]any)
	if rpcErr == nil || rpcErr["code"] == nil {
		t.Fatalf("bad json envelope = %v", envelope)
	}
	if int(rpcErr["code"].(float64)) != platform.A2AErrJSONParse.Code() {
		t.Errorf("bad json code = %v", rpcErr["code"])
	}
	if envelope["id"] != nil {
		t.Errorf("parse error id must be null: %v", envelope["id"])
	}

	// jsonrpc != "2.0" → invalid request。
	_, envelope = postRPC(t, rpcURL, `{"jsonrpc":"1.0","id":1,"method":"GetTask"}`, nil)
	rpcErr, _ = envelope["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != platform.A2AErrInvalidRequest.Code() {
		t.Errorf("invalid request envelope = %v", envelope)
	}
	if id, _ := envelope["id"].(float64); id != 1 {
		t.Errorf("invalid request id = %v", envelope["id"])
	}

	// 未知方法 → method not found（经真实 Service 分派）。
	_, envelope = postRPC(t, rpcURL, `{"jsonrpc":"2.0","id":2,"method":"Bogus"}`, nil)
	rpcErr, _ = envelope["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != platform.A2AErrMethodNotFound.Code() {
		t.Errorf("method not found envelope = %v", envelope)
	}

	// A2A-Version 主版本不同 → 拒绝。
	_, envelope = postRPC(t, rpcURL, `{"jsonrpc":"2.0","id":3,"method":"Bogus"}`,
		map[string]string{"A2A-Version": "0.3"})
	rpcErr, _ = envelope["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != platform.A2AErrVersionNotSupported.Code() {
		t.Errorf("version envelope = %v", envelope)
	}

	// 同主版本放行（"1" 与 "1.0" 等价）。
	_, envelope = postRPC(t, rpcURL, `{"jsonrpc":"2.0","id":4,"method":"Bogus"}`,
		map[string]string{"A2A-Version": "1"})
	rpcErr, _ = envelope["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) == platform.A2AErrVersionNotSupported.Code() {
		t.Errorf("same-major version rejected: %v", envelope)
	}
}

func TestA2ARPCAuthGate(t *testing.T) {
	server := newA2AServer(t, true, "it-a2a")
	rpcURL := server.URL + "/a2a/it-a2a"

	// 缺 Authorization → 401 + WWW-Authenticate，且不触发执行。
	response, _ := postRPC(t, rpcURL, `{"jsonrpc":"2.0","id":1,"method":"Bogus"}`, nil)
	if response.StatusCode != 401 {
		t.Errorf("missing auth = %d", response.StatusCode)
	}
	if challenge := response.Header.Get("WWW-Authenticate"); !strings.HasPrefix(challenge, "Bearer") {
		t.Errorf("challenge = %q", challenge)
	}

	// 错误令牌 → 401。
	response, _ = postRPC(t, rpcURL, `{"jsonrpc":"2.0","id":1,"method":"Bogus"}`,
		map[string]string{"Authorization": "Bearer wrong-token"})
	if response.StatusCode != 401 {
		t.Errorf("bad token = %d", response.StatusCode)
	}

	// 认证先于协议解析：非法 JSON + 无凭据仍是 401（不是解析错误信封）。
	response, _ = postRPC(t, rpcURL, "{not-json", nil)
	if response.StatusCode != 401 {
		t.Errorf("auth-before-parse = %d", response.StatusCode)
	}
}
