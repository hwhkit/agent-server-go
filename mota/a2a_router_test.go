package mota

// A2A 端到端 HTTP 测试:Agent Card 发现端点 + JSON-RPC SendMessage/GetTask。
// 对应 rs tests/a2a_card_test.rs 与 tests/a2a_rpc_test.rs;共享装配对应
// tests/a2a_support/mod.rs。
//
// 装配不构造 PlatformState(它要求 DATABASE_URL),而是直接拼 registry +
// executor + dispatcher + 内存任务存储,和 protocol_dispatch_test.go 的
// 做法一致。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const a2aTestBaseURL = "https://infoman.ai"
const a2aTestSecret = "a2a-test-signing-secret-0123456789"

func a2aAgentConfig(slug, role string, enabled bool) *AgentConfig {
	return &AgentConfig{
		ID: 1, Slug: slug, Name: "Fundamental Desk", Description: "内部基本面研究台",
		Enabled: enabled, ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeLocal,
		SigningSecret: a2aTestSecret, SignatureEncoding: "hex",
		SystemPrompt: role, WorkspacePath: ".", PermissionMode: "dontAsk",
		TimeoutSecs: 10, MaxConcurrency: 1, QueueCapacity: 4, Revision: 1,
	}
}

// a2aTestConfig 构造一份 A2A 配置。大多数用例关注协议形状,鉴权用例单独打开。
func a2aTestConfig(defaultCardSlug string) *A2AConfig {
	return &A2AConfig{
		Enabled:              true,
		BasePath:             "/a2a",
		StreamEnabled:        false,
		RequireBearerAuth:    false,
		AgentVersion:         a2aAppVersion,
		ProviderOrganization: "MOTA",
		ProviderURL:          a2aTestBaseURL,
		MaxRequestBodyBytes:  1024 * 1024,
		DefaultCardSlug:      defaultCardSlug,
	}
}

// a2aTestState 组装完整 A2A 状态。piBinary 为空时用一个不存在的路径
// (只测 card 的用例不会真的执行)。
func a2aTestState(t *testing.T, agents []*AgentConfig, config *A2AConfig, piBinary string) *A2aState {
	t.Helper()
	cfg := executorTestConfig(t)
	if piBinary == "" {
		piBinary = cfg.WorkspaceRoot + "/missing-pi"
	}
	cfg.LocalAgents.PiBinary = piBinary
	registry := NewAgentRegistry(agents)
	executor := newTestExecutor(t, cfg, PlaceholderProcessor{})
	dispatcher := NewAgentDispatcher(registry, executor, newSemaphore(4), "")
	return NewA2aState(config, a2aTestBaseURL, registry, dispatcher, NewA2AInMemoryTaskStore())
}

// a2aCardTestState 是只需要 card 的用例:不关心执行器。
func a2aCardOnlyState(t *testing.T, agents []*AgentConfig, config *A2AConfig) *A2aState {
	t.Helper()
	return a2aTestState(t, agents, config, "")
}

// a2aGet 发一次 GET 请求,返回 (状态码, Content-Type, 解码后的 JSON)。
func a2aGet(t *testing.T, handler http.Handler, path string) (int, string, map[string]any) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	resp, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, resp.Header.Get("Content-Type"), decoded
}

// ---------------------------------------------------------------------------
// Agent Card 发现端点(对应 rs tests/a2a_card_test.rs)
// ---------------------------------------------------------------------------

func TestA2ARouterPerAgentCardDescribesAV1Interface(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)},
		a2aTestConfig(""))
	status, contentType, card := a2aGet(t, A2ARouter(state), "/a2a/desk-1/agent-card.json")

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if contentType != A2AMediaType {
		t.Errorf("content-type = %q, want %q", contentType, A2AMediaType)
	}
	interfaces := card["supportedInterfaces"].([]any)
	if len(interfaces) != 1 {
		t.Fatalf("interfaces = %d, want 1", len(interfaces))
	}
	iface := interfaces[0].(map[string]any)
	if iface["url"] != "https://infoman.ai/a2a/desk-1" {
		t.Errorf("url = %v", iface["url"])
	}
	if iface["protocolBinding"] != "JSONRPC" {
		t.Errorf("protocolBinding = %v", iface["protocolBinding"])
	}
	if iface["protocolVersion"] != "1.0" {
		t.Errorf("protocolVersion = %v", iface["protocolVersion"])
	}
	if card["name"] != "Fundamental Desk" {
		t.Errorf("name = %v", card["name"])
	}
	if card["description"] != "内部基本面研究台" {
		t.Errorf("description = %v", card["description"])
	}
	provider := card["provider"].(map[string]any)
	if provider["organization"] != "MOTA" {
		t.Errorf("provider.organization = %v", provider["organization"])
	}
	if _, present := provider["name"]; present {
		t.Error("v1.0 uses organization, not v0.3's name")
	}
	capabilities := card["capabilities"].(map[string]any)
	if capabilities["streaming"] != false {
		t.Errorf("streaming = %v", capabilities["streaming"])
	}
	if capabilities["extendedAgentCard"] != false {
		t.Errorf("extendedAgentCard = %v", capabilities["extendedAgentCard"])
	}
	if capabilities["pushNotifications"] != false {
		t.Errorf("pushNotifications = %v", capabilities["pushNotifications"])
	}
}

func TestA2ARouterCardOmitsAllV03TransportFields(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)},
		a2aTestConfig(""))
	_, _, card := a2aGet(t, A2ARouter(state), "/a2a/desk-1/agent-card.json")

	for _, removed := range []string{
		"url", "preferredTransport", "additionalInterfaces", "protocolVersion",
		"supportsAuthenticatedExtendedCard",
	} {
		if _, present := card[removed]; present {
			t.Errorf("v0.3 field %q must not appear in a v1.0 card", removed)
		}
	}
	capabilities := card["capabilities"].(map[string]any)
	if _, present := capabilities["stateTransitionHistory"]; present {
		t.Error("v1.0 removed capabilities.stateTransitionHistory")
	}
	if card["version"] == "1.0" {
		t.Error("version is the Agent's own version, not the protocol version")
	}
}

func TestA2ARouterSkillsMapFromRoleContractWithRequiredTags(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("risk-1", "risk_manager", true)},
		a2aTestConfig(""))
	_, _, card := a2aGet(t, A2ARouter(state), "/a2a/risk-1/agent-card.json")

	skills := card["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("skills = %d, want 1", len(skills))
	}
	skill := skills[0].(map[string]any)
	if skill["id"] != "risk_manager" {
		t.Errorf("id = %v", skill["id"])
	}
	tags := skill["tags"].([]any)
	if len(tags) == 0 {
		t.Error("tags are required in v1.0")
	}
	description := skill["description"].(string)
	if !contains2(description, "gate") {
		t.Errorf("description = %q", description)
	}
	if !contains2(description, "checks") {
		t.Errorf("description = %q", description)
	}
	inputModes := skill["inputModes"].([]any)
	found := false
	for _, mode := range inputModes {
		if mode == "application/json" {
			found = true
		}
	}
	if !found {
		t.Error("inputModes must include application/json")
	}
}

func TestA2ARouterBearerSchemeUsesProtoJSONOneofMemberName(t *testing.T) {
	config := a2aTestConfig("")
	config.RequireBearerAuth = true
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)}, config)
	_, _, card := a2aGet(t, A2ARouter(state), "/a2a/desk-1/agent-card.json")

	schemes := card["securitySchemes"].(map[string]any)
	scheme := schemes["bearerAuth"].(map[string]any)
	httpAuth := scheme["httpAuthSecurityScheme"].(map[string]any)
	if httpAuth["scheme"] != "Bearer" {
		t.Errorf("scheme = %v", httpAuth["scheme"])
	}
	if _, present := scheme["type"]; present {
		t.Error("v0.3's flat {type: http} shape must not appear")
	}
	requirements := card["securityRequirements"].([]any)
	firstSchemes := requirements[0].(map[string]any)["schemes"].(map[string]any)
	if _, ok := firstSchemes["bearerAuth"]; !ok {
		t.Error("securityRequirements must reference bearerAuth")
	}
}

func TestA2ARouterWellKnownPathsServeTheDefaultAgentCard(t *testing.T) {
	agents := []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)}
	config := a2aTestConfig("desk-1")
	state := a2aCardOnlyState(t, agents, config)
	handler := A2ARouter(state)

	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		status, contentType, card := a2aGet(t, handler, path)
		if status != http.StatusOK {
			t.Errorf("%s: status = %d", path, status)
		}
		if contentType != A2AMediaType {
			t.Errorf("%s: content-type = %q", path, contentType)
		}
		interfaces := card["supportedInterfaces"].([]any)
		iface := interfaces[0].(map[string]any)
		if iface["url"] != "https://infoman.ai/a2a/desk-1" {
			t.Errorf("%s: url = %v", path, iface["url"])
		}
	}
}

func TestA2ARouterWellKnownCardIs404WithoutADefaultAgent(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)},
		a2aTestConfig(""))
	status, _, _ := a2aGet(t, A2ARouter(state), "/.well-known/agent-card.json")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestA2ARouterDisabledAgentDoesNotExposeACard(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", false)},
		a2aTestConfig("desk-1"))
	status, _, _ := a2aGet(t, A2ARouter(state), "/a2a/desk-1/agent-card.json")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestA2ARouterUnknownSlugIs404(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)},
		a2aTestConfig(""))
	status, _, _ := a2aGet(t, A2ARouter(state), "/a2a/nope/agent-card.json")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestA2ARouterSlugLookupIsCaseInsensitive(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)},
		a2aTestConfig(""))
	status, _, card := a2aGet(t, A2ARouter(state), "/a2a/DESK-1/agent-card.json")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	interfaces := card["supportedInterfaces"].([]any)
	iface := interfaces[0].(map[string]any)
	if iface["url"] != "https://infoman.ai/a2a/desk-1" {
		t.Errorf("url = %v", iface["url"])
	}
}

func TestA2ARouterCustomBasePathIsHonouredInRouteAndCard(t *testing.T) {
	config := &A2AConfig{
		Enabled: true, BasePath: "/protocol/a2a", StreamEnabled: false,
		RequireBearerAuth: false, AgentVersion: a2aAppVersion, MaxRequestBodyBytes: 1024 * 1024,
	}
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "strategist", true)}, config)
	status, _, card := a2aGet(t, A2ARouter(state), "/protocol/a2a/desk-1/agent-card.json")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	interfaces := card["supportedInterfaces"].([]any)
	iface := interfaces[0].(map[string]any)
	if iface["url"] != "https://infoman.ai/protocol/a2a/desk-1" {
		t.Errorf("url = %v", iface["url"])
	}
}

func TestA2ARouterCardRoundTripsThroughTheProtocolTypes(t *testing.T) {
	state := a2aCardOnlyState(t, []*AgentConfig{a2aAgentConfig("desk-1", "technical_analyst", true)},
		a2aTestConfig(""))
	server := httptest.NewServer(A2ARouter(state))
	defer server.Close()
	resp, err := http.Get(server.URL + "/a2a/desk-1/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 反序列化回强类型,确认字段名与类型和模型一致(不是只在序列化侧自洽)。
	var parsed A2AAgentCard
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("card must deserialize into A2AAgentCard: %v", err)
	}
	if parsed.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Errorf("protocolVersion = %q", parsed.SupportedInterfaces[0].ProtocolVersion)
	}
	if parsed.Skills[0].ID != "technical_analyst" {
		t.Errorf("skill id = %q", parsed.Skills[0].ID)
	}
	if len(parsed.Skills[0].Tags) == 0 {
		t.Error("tags must not be empty")
	}
}

func contains2(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
