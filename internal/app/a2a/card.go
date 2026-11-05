package a2a

// Agent Card 生成。对应 rs 侧 src/a2a/card.rs。
//
// v1.0 结构要点(与 v0.3 不兼容):
//   - 顶层 url / preferredTransport / additionalInterfaces / protocolVersion
//     四个字段合并为有序数组 supportedInterfaces,第一项为首选接口。
//   - version 是 Agent 自身版本,协议版本在每个 interface 里。
//   - supportsAuthenticatedExtendedCard 移到 capabilities.extendedAgentCard。
//   - provider.name 改名 provider.organization。
//   - 每个 skill 的 tags 必填非空。

import (
	"strings"

	"ir-agent-server/internal/domain/agent"
)

// A2ABearerSchemeName 是 Bearer 安全方案在 card 里的键名。
const A2ABearerSchemeName = "bearerAuth"

// A2ABuildCard 为单个 Agent 构造 v1.0 Agent Card。
func A2ABuildCard(agentCfg *agent.ResolvedConfig, config CardConfig, publicBaseURL string) A2AAgentCard {
	base := strings.TrimRight(publicBaseURL, "/")
	endpoint := base + config.AgentPath(agentCfg.Slug)

	securitySchemes := map[string]A2ASecurityScheme{}
	var securityRequirements []A2ASecurityRequirement
	if config.RequireBearerAuth {
		securitySchemes[A2ABearerSchemeName] = A2ASecuritySchemeBearer(
			"Per-Agent bearer token issued by the Intelligent Router agent platform administrator.")
		securityRequirements = append(securityRequirements,
			A2ASecurityRequirementScheme(A2ABearerSchemeName))
	}

	var provider *A2AAgentProvider
	if config.ProviderOrganization != "" {
		provider = &A2AAgentProvider{
			Organization: config.ProviderOrganization,
			URL:          config.ProviderURL,
		}
	}

	var documentationURL *string
	if config.DocumentationURL != "" {
		documentationURL = &config.DocumentationURL
	}

	// SendStreamingMessage / SubscribeToTask are not implemented. Keep the
	// public card truthful even if a caller constructed A2AConfig without
	// passing it through validation.
	streaming := false
	// 推送通知订阅尚未实现;声明 false,避免调用方发起后被拒。
	pushNotifications := false
	extendedAgentCard := false

	return A2AAgentCard{
		Name:        agentCfg.Name,
		Description: a2aCardDescription(agentCfg),
		// 有序数组,当前只暴露 JSON-RPC 绑定。
		SupportedInterfaces: []A2AAgentInterface{
			{
				URL:             endpoint,
				ProtocolBinding: A2AProtocolBindingJSONRPC,
				ProtocolVersion: A2AProtocolVersion,
			},
		},
		Provider:         provider,
		Version:          config.AgentVersion,
		DocumentationURL: documentationURL,
		Capabilities: A2AAgentCapabilities{
			Streaming:         &streaming,
			PushNotifications: &pushNotifications,
			ExtendedAgentCard: &extendedAgentCard,
		},
		SecuritySchemes:      securitySchemes,
		SecurityRequirements: securityRequirements,
		DefaultInputModes:    append([]string{}, A2AInputModes...),
		DefaultOutputModes:   append([]string{}, A2AOutputModes...),
		Skills:               A2ASkillsFor(agentCfg.SystemPrompt, agentCfg.Description),
		// Card 签名(JWS + RFC 8785)尚未实现。
	}
}

// a2aCardDescription 是 AgentCard.description,必填。管理端没填说明时用
// 岗位与执行方式兜底。
func a2aCardDescription(agentCfg *agent.ResolvedConfig) string {
	description := strings.TrimSpace(agentCfg.Description)
	if description != "" {
		return description
	}
	return "Self-hosted " + agentCfg.ExecutorKind.String() + " agent executed in " +
		agentCfg.ExecutionMode.String() + " mode by the Intelligent Router agent platform."
}
