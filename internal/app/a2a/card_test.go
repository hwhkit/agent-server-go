package a2a

// 对应 rs src/a2a/card.rs 的单测(构造 A2AAgentCard 后直接断言字段,不经
// HTTP;端到端 HTTP 层测试见 a2a_router_test.go)。

import (
	"encoding/json"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"strings"
	"testing"
)

func a2aCardTestAgent(slug, systemPrompt, description string) *agent.ResolvedConfig {
	return &agent.ResolvedConfig{
		ID: 1, Slug: slug, Name: "Fundamental Desk", Description: description,
		Enabled: true, ExecutorKind: exec.KindPi, ExecutionMode: exec.ModeLocal,
		SigningSecret: "0123456789abcdef", SignatureEncoding: "hex",
		SystemPrompt: systemPrompt, WorkspacePath: ".", PermissionMode: "dontAsk",
		TimeoutSecs: 600, MaxConcurrency: 1, QueueCapacity: 10, Revision: 3,
	}
}

func a2aCardTestConfig() CardConfig {
	cfg := defaultCardConfig()
	return cfg
}

func TestA2ACardInterfaceCarriesURLBindingAndVersion(t *testing.T) {
	card := A2ABuildCard(
		a2aCardTestAgent("desk-1", "fundamental_analyst", "研究台"),
		a2aCardTestConfig(), "https://infoman.ai/")
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("len = %d, want 1", len(card.SupportedInterfaces))
	}
	iface := card.SupportedInterfaces[0]
	if iface.URL != "https://infoman.ai/a2a/desk-1" {
		t.Errorf("url = %q", iface.URL)
	}
	if iface.ProtocolBinding != "JSONRPC" {
		t.Errorf("protocolBinding = %q", iface.ProtocolBinding)
	}
	if iface.ProtocolVersion != "1.0" {
		t.Errorf("protocolVersion = %q", iface.ProtocolVersion)
	}
}

func TestA2ACardSerializesWithoutV03Fields(t *testing.T) {
	card := A2ABuildCard(
		a2aCardTestAgent("desk-1", "fundamental_analyst", "研究台"),
		a2aCardTestConfig(), "https://infoman.ai")
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{
		"url", "preferredTransport", "additionalInterfaces", "protocolVersion",
		"supportsAuthenticatedExtendedCard",
	} {
		if _, present := decoded[removed]; present {
			t.Errorf("v0.3 field %q must not appear in a v1.0 card", removed)
		}
	}
	if _, ok := decoded["supportedInterfaces"]; !ok {
		t.Error("supportedInterfaces must be present")
	}
	capabilities := decoded["capabilities"].(map[string]any)
	if capabilities["extendedAgentCard"] != false {
		t.Errorf("extendedAgentCard = %v, want false", capabilities["extendedAgentCard"])
	}
	if _, present := decoded["provider"]; present {
		t.Error("provider must be omitted when not configured")
	}
}

func TestA2ACardBearerSchemeUsesProtoJSONOneofMember(t *testing.T) {
	card := A2ABuildCard(
		a2aCardTestAgent("desk-1", "fundamental_analyst", ""),
		a2aCardTestConfig(), "https://infoman.ai")
	encoded, _ := json.Marshal(card)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	schemes := decoded["securitySchemes"].(map[string]any)
	bearer := schemes[A2ABearerSchemeName].(map[string]any)
	http := bearer["httpAuthSecurityScheme"].(map[string]any)
	if http["scheme"] != "Bearer" {
		t.Errorf("scheme = %v, want Bearer", http["scheme"])
	}
	requirements := decoded["securityRequirements"].([]any)
	firstSchemes := requirements[0].(map[string]any)["schemes"].(map[string]any)
	if _, ok := firstSchemes[A2ABearerSchemeName]; !ok {
		t.Error("securityRequirements must reference bearerAuth")
	}
}

func TestA2ACardDescriptionFallsBackWhenAgentHasNone(t *testing.T) {
	card := A2ABuildCard(
		a2aCardTestAgent("desk-1", "fundamental_analyst", ""),
		a2aCardTestConfig(), "https://infoman.ai")
	if card.Description == "" {
		t.Error("description must not be empty")
	}
	if !strings.Contains(card.Description, "pi") {
		t.Errorf("description = %q", card.Description)
	}
}

func TestA2ACardAuthCanBeDisabledLeavingNoSecurityDeclaration(t *testing.T) {
	cfg := a2aCardTestConfig()
	cfg.RequireBearerAuth = false
	card := A2ABuildCard(a2aCardTestAgent("desk-1", "strategist", ""), cfg, "https://x.ai")
	if len(card.SecuritySchemes) != 0 {
		t.Error("security schemes must be empty when auth is disabled")
	}
	if len(card.SecurityRequirements) != 0 {
		t.Error("security requirements must be empty when auth is disabled")
	}
}

// defaultCardConfig 与 infra/config 的 A2A 缺省值对齐（app 测试不依赖配置包）。
func defaultCardConfig() CardConfig {
	return CardConfig{
		BasePath:          "/a2a",
		RequireBearerAuth: true,
		AgentVersion:      "1.2.0",
	}
}
