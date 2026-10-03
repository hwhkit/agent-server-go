package mota

// 智能体中心 Provider 目录、持久配置、可用性判断与岗位分配映射。
// 行为与 Rust 版 src/platform/providers.rs 保持一致。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var providerIDs = []string{
	"codex_local", "pi_local", "opencode_local", "qwen_code_local",
	"gemini_cli_local", "openhands_local", "claude_code_local", "gateway",
	"deepseek_harness", "openai_agent", "claude_managed_agent",
}

type ProviderAssignment struct {
	Label         string `json:"label"`
	ExecutorKind  string `json:"executor_kind"`
	ExecutionMode string `json:"execution_mode"`
}

type ProviderView struct {
	ID               string               `json:"id"`
	Revision         int64                `json:"revision"`
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	Category         string               `json:"category"`
	Configured       bool                 `json:"configured"`
	Editable         bool                 `json:"editable"`
	StatusDetail     string               `json:"status_detail"`
	SecretConfigured bool                 `json:"secret_configured"`
	Config           map[string]any       `json:"config"`
	Assignments      []ProviderAssignment `json:"assignments"`
}

func providerKnown(id string) bool {
	for _, candidate := range providerIDs {
		if candidate == id {
			return true
		}
	}
	return false
}

func LoadSavedProviders(ctx context.Context, store *AgentStore, config *PlatformConfig) error {
	if err := applySavedProviders(ctx, store, config); err != nil {
		return err
	}
	return config.Validate()
}

func applySavedProviders(ctx context.Context, store *AgentStore, config *PlatformConfig) error {
	for _, id := range providerIDs {
		value, err := store.GetExecutorProvider(ctx, id)
		if err != nil {
			return err
		}
		if value != nil {
			if err := applyProviderValue(config, id, value, false); err != nil {
				return fmt.Errorf("load Agent Center provider %s: %w", id, err)
			}
		}
	}
	applyProviderSecretEnvOverrides(config)
	return nil
}

// applyProviderSecretEnvOverrides keeps deployment secret injection
// authoritative over encrypted values saved by the management UI. This is
// essential for emergency rotation: a restart with a new secret must never be
// silently rolled back by an older database snapshot. Non-secret Provider
// settings remain managed by the database after first save.
func applyProviderSecretEnvOverrides(config *PlatformConfig) {
	if value, ok := lookupNonBlankEnv("AGENT_GATEWAY__API_KEY"); ok {
		config.Gateway.APIKey = value
	} else if value, ok := lookupNonBlankEnv("ANTHROPIC_AUTH_TOKEN"); ok {
		config.Gateway.APIKey = value
	}
	if value, ok := lookupNonBlankEnv("OPENAI_AGENTS__API_KEY"); ok {
		config.OpenAIAgents.APIKey = value
	}
	if value, ok := lookupNonBlankEnv("OPENAI_AGENTS__MCP_AUTHORIZATION"); ok {
		config.OpenAIAgents.MCPAuthorization = value
	}
	if value, ok := lookupNonBlankEnv("CLAUDE_MANAGED__API_KEY"); ok {
		config.ClaudeManaged.APIKey = value
	}
}

// RefreshSavedProviders 从 PostgreSQL 合并 Provider 字段，返回新的不可变快照。
// 运行快照在启动后已清掉管理口令和主密钥，因此只在校验副本中补占位值。
func RefreshSavedProviders(ctx context.Context, store *AgentStore, current *PlatformConfig) (*PlatformConfig, error) {
	next := clonePlatformConfig(current)
	if err := applySavedProviders(ctx, store, next); err != nil {
		return nil, err
	}
	validation := clonePlatformConfig(next)
	validation.AdminPassword = "provider-refresh-validation"
	validation.AdminPasswordHash = ""
	validation.AdminSessionSecret = strings.Repeat("v", 32)
	if err := validation.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// LoadConsistentProviderSnapshot 用全局 revision 栅栏把配置值和每项 revision
// 绑定到同一个数据库时刻；有并发写时最多重试三次。
func LoadConsistentProviderSnapshot(
	ctx context.Context, store *AgentStore, current *PlatformConfig,
) (*PlatformConfig, map[string]int64, int64, error) {
	for range 3 {
		before, err := store.ExecutorProviderRevision(ctx)
		if err != nil {
			return nil, nil, 0, err
		}
		next, err := RefreshSavedProviders(ctx, store, current)
		if err != nil {
			return nil, nil, 0, err
		}
		revisions, err := store.ListExecutorProviderRevisions(ctx)
		if err != nil {
			return nil, nil, 0, err
		}
		after, err := store.ExecutorProviderRevision(ctx)
		if err != nil {
			return nil, nil, 0, err
		}
		if before == after {
			return next, revisions, after, nil
		}
	}
	return nil, nil, 0, errors.New("Provider configuration changed during snapshot read")
}

func ListProviders(config *PlatformConfig) []ProviderView {
	views := make([]ProviderView, 0, len(providerIDs))
	for _, id := range providerIDs {
		views = append(views, providerView(config, id))
	}
	return views
}

func UpdateProvider(current *PlatformConfig, id string, input map[string]any) (*PlatformConfig, map[string]any, error) {
	if !providerKnown(id) {
		return nil, nil, errors.New("unknown executor provider")
	}
	known, err := storedProviderValue(current, id)
	if err != nil {
		return nil, nil, err
	}
	delete(known, "mcp_authorization_configured")
	for key := range input {
		if _, ok := known[key]; !ok {
			return nil, nil, fmt.Errorf("unknown Provider field %q", key)
		}
	}
	next := clonePlatformConfig(current)
	if err := applyProviderValue(next, id, input, true); err != nil {
		return nil, nil, err
	}
	validation := clonePlatformConfig(next)
	// 共享运行配置在启动后会清空管理密码与主密钥。Provider 热更新校验只需
	// 临时补安全占位，不能把真实凭据重新放回配置快照。
	validation.AdminPassword = "provider-update-validation"
	validation.AdminPasswordHash = ""
	validation.AdminSessionSecret = strings.Repeat("v", 32)
	if err := validation.Validate(); err != nil {
		return nil, nil, err
	}
	stored, err := storedProviderValue(next, id)
	return next, stored, err
}

func clonePlatformConfig(current *PlatformConfig) *PlatformConfig {
	next := *current
	next.AdminTrustedProxyCIDRs = append([]string(nil), current.AdminTrustedProxyCIDRs...)
	next.ClaudeAllowedEnv = append([]string(nil), current.ClaudeAllowedEnv...)
	next.LocalAgents.AllowedEnv = append([]string(nil), current.LocalAgents.AllowedEnv...)
	next.LocalAgents.StrategySkillPaths = append([]string(nil), current.LocalAgents.StrategySkillPaths...)
	next.LocalAgents.AnalysisSkillPaths = append([]string(nil), current.LocalAgents.AnalysisSkillPaths...)
	next.LocalAgents.WorkflowSkillPaths = append([]string(nil), current.LocalAgents.WorkflowSkillPaths...)
	next.ClaudeManaged.VaultIDs = append([]string(nil), current.ClaudeManaged.VaultIDs...)
	if current.ClaudeManaged.MaxListCostCents != nil {
		value := *current.ClaudeManaged.MaxListCostCents
		next.ClaudeManaged.MaxListCostCents = &value
	}
	return &next
}

func IsAssignmentConfigured(config *PlatformConfig, kind ExecutorKind, mode ExecutionMode) bool {
	// Go 版保留历史进程内 Processor API 兼容，但不在智能体中心展示新建入口。
	if kind == ExecutorKindRust {
		return true
	}
	id := ""
	switch {
	case kind == ExecutorKindCodex && mode == ExecutionModeLocal:
		id = "codex_local"
	case kind == ExecutorKindPi && mode == ExecutionModeLocal:
		id = "pi_local"
	case kind == ExecutorKindOpenCode && mode == ExecutionModeLocal:
		id = "opencode_local"
	case kind == ExecutorKindQwenCode && mode == ExecutionModeLocal:
		id = "qwen_code_local"
	case kind == ExecutorKindGeminiCLI && mode == ExecutionModeLocal:
		id = "gemini_cli_local"
	case kind == ExecutorKindOpenHands && mode == ExecutionModeLocal:
		id = "openhands_local"
	case kind == ExecutorKindClaudeCode && mode == ExecutionModeLocal:
		id = "claude_code_local"
	case kind == ExecutorKindDeepseekHarness && mode == ExecutionModeLocal:
		id = "deepseek_harness"
	case kind == ExecutorKindOpenAIAgent && mode == ExecutionModeManaged:
		id = "openai_agent"
	case kind == ExecutorKindClaudeManaged && mode == ExecutionModeManaged:
		id = "claude_managed_agent"
	case mode == ExecutionModeGateway && kind.SupportsGateway():
		id = "gateway"
	default:
		return false
	}
	return providerConfigured(config, id)
}

func providerView(config *PlatformConfig, id string) ProviderView {
	name, description, category := "Unknown", "", "remote"
	switch id {
	case "codex_local":
		name, description, category = "Codex (local)", "本机 Codex CLI，只读沙箱执行。", "local"
	case "pi_local":
		name, description, category = "Pi (local)", "本机 Pi CLI，可加载岗位 Skills 与金融扩展。", "local"
	case "opencode_local":
		name, description, category = "OpenCode (local)", "本机 OpenCode CLI，使用只读 plan 模式非交互执行。", "local"
	case "qwen_code_local":
		name, description, category = "Qwen Code (local)", "本机 Qwen Code CLI，safe mode 无头执行。", "local"
	case "gemini_cli_local":
		name, description, category = "Gemini CLI (local)", "本机 Gemini CLI，plan 权限模式与 JSON 回执。", "local"
	case "openhands_local":
		name, description, category = "OpenHands (local)", "本机 OpenHands headless；官方无头模式会自动批准动作，请使用隔离工作区。", "local"
	case "claude_code_local":
		name, description, category = "Claude Code (local)", "本机 Claude Code CLI；必须配合 OS/容器隔离。", "local"
	case "gateway":
		name, description = "LiteLLM Gateway", "Anthropic Messages 或 OpenAI Chat Completions 兼容网关。"
	case "deepseek_harness":
		name, description, category = "DeepSeek Harness", "本机启动官方 dsh headless 智能体；支持工具、会话与 Harness 循环。", "local"
	case "openai_agent":
		name, description = "OpenAI Agent", "OpenAI 托管智能体 API Session/SSE。"
	case "claude_managed_agent":
		name, description = "Claude Managed Agent", "Anthropic 托管智能体 + Environment Session/SSE。"
	}
	ready := providerConfigured(config, id)
	return ProviderView{
		ID: id, Name: name, Description: description, Category: category,
		Configured: ready, Editable: id != "", StatusDetail: providerStatus(config, id, ready),
		SecretConfigured: providerSecretConfigured(config, id), Config: publicProviderConfig(config, id),
		Assignments: providerAssignments(id),
	}
}

func commandExists(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if strings.ContainsRune(command, os.PathSeparator) {
		info, err := os.Stat(command)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(command)
	return err == nil
}

func providerConfigured(c *PlatformConfig, id string) bool {
	switch id {
	case "codex_local":
		return commandExists(c.LocalAgents.CodexBinary)
	case "pi_local":
		return commandExists(c.LocalAgents.PiBinary)
	case "opencode_local":
		return commandExists(c.LocalAgents.OpenCodeBinary)
	case "qwen_code_local":
		return commandExists(c.LocalAgents.QwenCodeBinary)
	case "gemini_cli_local":
		return commandExists(c.LocalAgents.GeminiCLIBinary)
	case "openhands_local":
		return commandExists(c.LocalAgents.OpenHandsBinary)
	case "claude_code_local":
		return c.ClaudeEnabled && commandExists(c.ClaudeBinary)
	case "gateway":
		return strings.TrimSpace(c.Gateway.BaseURL) != ""
	case "deepseek_harness":
		return commandExists(c.LocalAgents.DeepseekHarnessBinary)
	case "openai_agent":
		return c.OpenAIAgents.Enabled && c.OpenAIAgents.APIKey != ""
	case "claude_managed_agent":
		return c.ClaudeManaged.Enabled && c.ClaudeManaged.APIKey != "" && c.ClaudeManaged.AgentID != "" && c.ClaudeManaged.EnvironmentID != ""
	default:
		return false
	}
}

func providerStatus(c *PlatformConfig, id string, ready bool) string {
	if ready {
		return "已配置，可用于岗位"
	}
	switch id {
	case "codex_local", "pi_local", "opencode_local", "qwen_code_local", "gemini_cli_local", "openhands_local":
		return "未找到可执行文件"
	case "claude_code_local":
		if !c.ClaudeEnabled {
			return "服务端未启用 Claude Code"
		}
		return "未找到可执行文件"
	case "gateway":
		return "需要配置网关地址"
	case "deepseek_harness":
		return "未找到 dsh 可执行文件"
	case "openai_agent":
		return "需要启用并配置 API key"
	case "claude_managed_agent":
		return "需要 API key、Agent ID 和 Environment ID"
	default:
		return "未配置"
	}
}

func providerSecretConfigured(c *PlatformConfig, id string) bool {
	switch id {
	case "gateway", "deepseek_harness":
		return c.Gateway.APIKey != ""
	case "openai_agent":
		return c.OpenAIAgents.APIKey != ""
	case "claude_managed_agent":
		return c.ClaudeManaged.APIKey != ""
	default:
		return false
	}
}

func publicProviderConfig(c *PlatformConfig, id string) map[string]any {
	switch id {
	case "codex_local":
		return map[string]any{"binary": c.LocalAgents.CodexBinary, "default_model": c.LocalAgents.DefaultModel}
	case "pi_local":
		return map[string]any{"binary": c.LocalAgents.PiBinary, "default_model": c.LocalAgents.DefaultModel, "finance_extension_path": nilIfEmpty(c.LocalAgents.FinanceExtensionPath)}
	case "opencode_local":
		return map[string]any{"binary": c.LocalAgents.OpenCodeBinary, "default_model": c.LocalAgents.OpenCodeDefaultModel}
	case "qwen_code_local":
		return map[string]any{"binary": c.LocalAgents.QwenCodeBinary, "default_model": c.LocalAgents.QwenCodeDefaultModel}
	case "gemini_cli_local":
		return map[string]any{"binary": c.LocalAgents.GeminiCLIBinary, "default_model": c.LocalAgents.GeminiCLIDefaultModel}
	case "openhands_local":
		return map[string]any{"binary": c.LocalAgents.OpenHandsBinary, "default_model": c.LocalAgents.OpenHandsDefaultModel}
	case "claude_code_local":
		return map[string]any{"enabled": c.ClaudeEnabled, "binary": c.ClaudeBinary, "default_model": c.ClaudeDefaultModel}
	case "gateway":
		return gatewayPublicConfig(c)
	case "deepseek_harness":
		value := gatewayPublicConfig(c)
		value["binary"] = c.LocalAgents.DeepseekHarnessBinary
		return value
	case "openai_agent":
		return map[string]any{"enabled": c.OpenAIAgents.Enabled, "base_url": c.OpenAIAgents.BaseURL, "default_model": c.OpenAIAgents.DefaultModel, "reasoning_effort": c.OpenAIAgents.ReasoningEffort, "environment": c.OpenAIAgents.Environment, "mcp_server_url": nilIfEmpty(c.OpenAIAgents.MCPServerURL), "mcp_server_label": c.OpenAIAgents.MCPServerLabel, "mcp_required": c.OpenAIAgents.MCPRequired, "delete_session": c.OpenAIAgents.DeleteSession, "max_response_bytes": c.OpenAIAgents.MaxResponseBytes, "mcp_authorization_configured": c.OpenAIAgents.MCPAuthorization != ""}
	case "claude_managed_agent":
		return map[string]any{"enabled": c.ClaudeManaged.Enabled, "base_url": c.ClaudeManaged.BaseURL, "workspace_id": nilIfEmpty(c.ClaudeManaged.WorkspaceID), "agent_id": c.ClaudeManaged.AgentID, "environment_id": c.ClaudeManaged.EnvironmentID, "vault_ids": c.ClaudeManaged.VaultIDs, "max_list_cost_cents": c.ClaudeManaged.MaxListCostCents, "delete_session": c.ClaudeManaged.DeleteSession, "max_response_bytes": c.ClaudeManaged.MaxResponseBytes}
	default:
		return map[string]any{}
	}
}

func gatewayPublicConfig(c *PlatformConfig) map[string]any {
	return map[string]any{"base_url": c.Gateway.BaseURL, "model": c.Gateway.Model, "protocol": string(c.Gateway.Protocol), "max_tokens": c.Gateway.MaxTokens, "max_attempts": c.Gateway.MaxAttempts, "max_response_bytes": c.Gateway.MaxResponseBytes}
}

func storedProviderValue(c *PlatformConfig, id string) (map[string]any, error) {
	value := publicProviderConfig(c, id)
	switch id {
	case "gateway", "deepseek_harness":
		value["api_key"] = c.Gateway.APIKey
	case "openai_agent":
		value["api_key"] = c.OpenAIAgents.APIKey
		value["mcp_authorization"] = nilIfEmpty(c.OpenAIAgents.MCPAuthorization)
	case "claude_managed_agent":
		value["api_key"] = c.ClaudeManaged.APIKey
	}
	return value, nil
}

// StoredProviderUpdateGroup 返回必须原子保存的一组 Provider。部分 Provider 在
// 运行配置中共享字段；任一别名更新时要同时刷新全组，避免重启时旧记录按加载顺序
// 覆盖刚保存的值。
func StoredProviderUpdateGroup(c *PlatformConfig, id string) (map[string]map[string]any, error) {
	var ids []string
	switch id {
	case "gateway", "deepseek_harness":
		ids = []string{"gateway", "deepseek_harness"}
	case "codex_local", "pi_local":
		ids = []string{"codex_local", "pi_local"}
	default:
		if !providerKnown(id) {
			return nil, errors.New("unknown executor provider")
		}
		ids = []string{id}
	}
	values := make(map[string]map[string]any, len(ids))
	for _, providerID := range ids {
		value, err := storedProviderValue(c, providerID)
		if err != nil {
			return nil, err
		}
		values[providerID] = value
	}
	return values, nil
}

func applyProviderValue(c *PlatformConfig, id string, value map[string]any, preserveEmptySecrets bool) error {
	var err error
	switch id {
	case "codex_local":
		if c.LocalAgents.CodexBinary, err = assignString(value, "binary", c.LocalAgents.CodexBinary, false); err != nil {
			return err
		}
		c.LocalAgents.DefaultModel, err = assignString(value, "default_model", c.LocalAgents.DefaultModel, false)
	case "pi_local":
		if c.LocalAgents.PiBinary, err = assignString(value, "binary", c.LocalAgents.PiBinary, false); err != nil {
			return err
		}
		if c.LocalAgents.DefaultModel, err = assignString(value, "default_model", c.LocalAgents.DefaultModel, false); err != nil {
			return err
		}
		c.LocalAgents.FinanceExtensionPath, err = assignOptionalString(value, "finance_extension_path", c.LocalAgents.FinanceExtensionPath)
	case "opencode_local":
		c.LocalAgents.OpenCodeBinary, c.LocalAgents.OpenCodeDefaultModel, err = assignLocal(value, c.LocalAgents.OpenCodeBinary, c.LocalAgents.OpenCodeDefaultModel)
	case "qwen_code_local":
		c.LocalAgents.QwenCodeBinary, c.LocalAgents.QwenCodeDefaultModel, err = assignLocal(value, c.LocalAgents.QwenCodeBinary, c.LocalAgents.QwenCodeDefaultModel)
	case "gemini_cli_local":
		c.LocalAgents.GeminiCLIBinary, c.LocalAgents.GeminiCLIDefaultModel, err = assignLocal(value, c.LocalAgents.GeminiCLIBinary, c.LocalAgents.GeminiCLIDefaultModel)
	case "openhands_local":
		c.LocalAgents.OpenHandsBinary, c.LocalAgents.OpenHandsDefaultModel, err = assignLocal(value, c.LocalAgents.OpenHandsBinary, c.LocalAgents.OpenHandsDefaultModel)
	case "claude_code_local":
		if c.ClaudeEnabled, err = assignBool(value, "enabled", c.ClaudeEnabled); err != nil {
			return err
		}
		if c.ClaudeBinary, err = assignString(value, "binary", c.ClaudeBinary, false); err != nil {
			return err
		}
		c.ClaudeDefaultModel, err = assignString(value, "default_model", c.ClaudeDefaultModel, false)
	case "gateway", "deepseek_harness":
		if id == "deepseek_harness" {
			if c.LocalAgents.DeepseekHarnessBinary, err = assignString(value, "binary", c.LocalAgents.DeepseekHarnessBinary, false); err != nil {
				return err
			}
		}
		if c.Gateway.BaseURL, err = assignString(value, "base_url", c.Gateway.BaseURL, true); err != nil {
			return err
		}
		if c.Gateway.APIKey, err = assignSecret(value, "api_key", c.Gateway.APIKey, preserveEmptySecrets); err != nil {
			return err
		}
		if c.Gateway.Model, err = assignString(value, "model", c.Gateway.Model, false); err != nil {
			return err
		}
		if raw, ok := value["protocol"]; ok {
			protocol, ok := ParseGatewayProtocol(fmt.Sprint(raw))
			if !ok {
				return errors.New("protocol must be anthropic or openai")
			}
			c.Gateway.Protocol = protocol
		}
		if c.Gateway.MaxTokens, err = assignUint(value, "max_tokens", c.Gateway.MaxTokens); err != nil {
			return err
		}
		if c.Gateway.MaxAttempts, err = assignInt(value, "max_attempts", c.Gateway.MaxAttempts); err != nil {
			return err
		}
		c.Gateway.MaxResponseBytes, err = assignInt(value, "max_response_bytes", c.Gateway.MaxResponseBytes)
	case "openai_agent":
		if c.OpenAIAgents.Enabled, err = assignBool(value, "enabled", c.OpenAIAgents.Enabled); err != nil {
			return err
		}
		if c.OpenAIAgents.BaseURL, err = assignString(value, "base_url", c.OpenAIAgents.BaseURL, false); err != nil {
			return err
		}
		if c.OpenAIAgents.APIKey, err = assignSecret(value, "api_key", c.OpenAIAgents.APIKey, preserveEmptySecrets); err != nil {
			return err
		}
		if c.OpenAIAgents.DefaultModel, err = assignString(value, "default_model", c.OpenAIAgents.DefaultModel, false); err != nil {
			return err
		}
		if c.OpenAIAgents.ReasoningEffort, err = assignString(value, "reasoning_effort", c.OpenAIAgents.ReasoningEffort, false); err != nil {
			return err
		}
		if c.OpenAIAgents.Environment, err = assignString(value, "environment", c.OpenAIAgents.Environment, false); err != nil {
			return err
		}
		if c.OpenAIAgents.MCPServerURL, err = assignOptionalString(value, "mcp_server_url", c.OpenAIAgents.MCPServerURL); err != nil {
			return err
		}
		if c.OpenAIAgents.MCPServerLabel, err = assignString(value, "mcp_server_label", c.OpenAIAgents.MCPServerLabel, false); err != nil {
			return err
		}
		if c.OpenAIAgents.MCPAuthorization, err = assignSecret(value, "mcp_authorization", c.OpenAIAgents.MCPAuthorization, preserveEmptySecrets); err != nil {
			return err
		}
		if c.OpenAIAgents.MCPRequired, err = assignBool(value, "mcp_required", c.OpenAIAgents.MCPRequired); err != nil {
			return err
		}
		if c.OpenAIAgents.DeleteSession, err = assignBool(value, "delete_session", c.OpenAIAgents.DeleteSession); err != nil {
			return err
		}
		c.OpenAIAgents.MaxResponseBytes, err = assignInt(value, "max_response_bytes", c.OpenAIAgents.MaxResponseBytes)
	case "claude_managed_agent":
		if c.ClaudeManaged.Enabled, err = assignBool(value, "enabled", c.ClaudeManaged.Enabled); err != nil {
			return err
		}
		if c.ClaudeManaged.BaseURL, err = assignString(value, "base_url", c.ClaudeManaged.BaseURL, false); err != nil {
			return err
		}
		if c.ClaudeManaged.APIKey, err = assignSecret(value, "api_key", c.ClaudeManaged.APIKey, preserveEmptySecrets); err != nil {
			return err
		}
		if c.ClaudeManaged.WorkspaceID, err = assignOptionalString(value, "workspace_id", c.ClaudeManaged.WorkspaceID); err != nil {
			return err
		}
		if c.ClaudeManaged.AgentID, err = assignString(value, "agent_id", c.ClaudeManaged.AgentID, true); err != nil {
			return err
		}
		if c.ClaudeManaged.EnvironmentID, err = assignString(value, "environment_id", c.ClaudeManaged.EnvironmentID, true); err != nil {
			return err
		}
		if raw, ok := value["vault_ids"]; ok {
			items, ok := raw.([]any)
			if !ok {
				if direct, ok2 := raw.([]string); ok2 {
					c.ClaudeManaged.VaultIDs = direct
					break
				}
				return errors.New("vault_ids must be an array")
			}
			c.ClaudeManaged.VaultIDs = nil
			for _, item := range items {
				text, ok := item.(string)
				if !ok {
					return errors.New("vault_ids must contain strings")
				}
				c.ClaudeManaged.VaultIDs = append(c.ClaudeManaged.VaultIDs, text)
			}
		}
		if raw, ok := value["max_list_cost_cents"]; ok {
			if raw == nil {
				c.ClaudeManaged.MaxListCostCents = nil
			} else {
				n, e := numberUint(raw)
				if e != nil {
					return errors.New("max_list_cost_cents must be an integer")
				}
				c.ClaudeManaged.MaxListCostCents = &n
			}
		}
		if c.ClaudeManaged.DeleteSession, err = assignBool(value, "delete_session", c.ClaudeManaged.DeleteSession); err != nil {
			return err
		}
		c.ClaudeManaged.MaxResponseBytes, err = assignInt(value, "max_response_bytes", c.ClaudeManaged.MaxResponseBytes)
	default:
		return errors.New("unknown executor provider")
	}
	return err
}

func assignLocal(value map[string]any, binary, model string) (string, string, error) {
	var err error
	if binary, err = assignString(value, "binary", binary, false); err != nil {
		return "", "", err
	}
	model, err = assignString(value, "default_model", model, true)
	return binary, model, err
}
func assignString(value map[string]any, key, current string, allowEmpty bool) (string, error) {
	raw, ok := value[key]
	if !ok {
		return current, nil
	}
	text, ok := raw.(string)
	if !ok {
		return current, fmt.Errorf("%s must be a string", key)
	}
	text = strings.TrimSpace(text)
	if !allowEmpty && text == "" {
		return current, fmt.Errorf("%s must not be empty", key)
	}
	return text, nil
}
func assignOptionalString(value map[string]any, key, current string) (string, error) {
	raw, ok := value[key]
	if !ok {
		return current, nil
	}
	if raw == nil {
		return "", nil
	}
	text, ok := raw.(string)
	if !ok {
		return current, fmt.Errorf("%s must be a string or null", key)
	}
	return strings.TrimSpace(text), nil
}
func assignSecret(value map[string]any, key, current string, preserveEmpty bool) (string, error) {
	raw, ok := value[key]
	if !ok || raw == nil {
		return current, nil
	}
	text, ok := raw.(string)
	if !ok {
		return current, fmt.Errorf("%s must be a string or null", key)
	}
	if preserveEmpty && text == "" {
		return current, nil
	}
	return text, nil
}
func assignBool(value map[string]any, key string, current bool) (bool, error) {
	raw, ok := value[key]
	if !ok {
		return current, nil
	}
	result, ok := raw.(bool)
	if !ok {
		return current, fmt.Errorf("%s must be a boolean", key)
	}
	return result, nil
}
func numberUint(raw any) (uint64, error) {
	switch n := raw.(type) {
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return 0, errors.New("invalid integer")
		}
		return uint64(n), nil
	case json.Number:
		return strconv.ParseUint(n.String(), 10, 64)
	case int:
		if n < 0 {
			return 0, errors.New("invalid integer")
		}
		return uint64(n), nil
	case uint64:
		return n, nil
	default:
		text := fmt.Sprint(raw)
		return strconv.ParseUint(text, 10, 64)
	}
}

func assignUint(value map[string]any, key string, current uint64) (uint64, error) {
	raw, ok := value[key]
	if !ok {
		return current, nil
	}
	n, err := numberUint(raw)
	if err != nil {
		return current, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}
func assignInt(value map[string]any, key string, current int) (int, error) {
	n, err := assignUint(value, key, uint64(current))
	if err != nil {
		return current, err
	}
	if n > uint64(^uint(0)>>1) {
		return current, fmt.Errorf("%s is too large", key)
	}
	return int(n), nil
}
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func providerAssignments(id string) []ProviderAssignment {
	assignment := func(label string, kind ExecutorKind, mode ExecutionMode) ProviderAssignment {
		return ProviderAssignment{label, string(kind), string(mode)}
	}
	switch id {
	case "codex_local":
		return []ProviderAssignment{assignment("Codex (local)", ExecutorKindCodex, ExecutionModeLocal)}
	case "pi_local":
		return []ProviderAssignment{assignment("Pi (local)", ExecutorKindPi, ExecutionModeLocal)}
	case "opencode_local":
		return []ProviderAssignment{assignment("OpenCode (local)", ExecutorKindOpenCode, ExecutionModeLocal)}
	case "qwen_code_local":
		return []ProviderAssignment{assignment("Qwen Code (local)", ExecutorKindQwenCode, ExecutionModeLocal)}
	case "gemini_cli_local":
		return []ProviderAssignment{assignment("Gemini CLI (local)", ExecutorKindGeminiCLI, ExecutionModeLocal)}
	case "openhands_local":
		return []ProviderAssignment{assignment("OpenHands (local)", ExecutorKindOpenHands, ExecutionModeLocal)}
	case "claude_code_local":
		return []ProviderAssignment{assignment("Claude Code (local)", ExecutorKindClaudeCode, ExecutionModeLocal)}
	case "deepseek_harness":
		return []ProviderAssignment{assignment("DeepSeek Harness (local)", ExecutorKindDeepseekHarness, ExecutionModeLocal)}
	case "openai_agent":
		return []ProviderAssignment{assignment("OpenAI Agent", ExecutorKindOpenAIAgent, ExecutionModeManaged)}
	case "claude_managed_agent":
		return []ProviderAssignment{assignment("Claude Managed Agent", ExecutorKindClaudeManaged, ExecutionModeManaged)}
	case "gateway":
		return []ProviderAssignment{assignment("Claude via Gateway", ExecutorKindClaudeCode, ExecutionModeGateway), assignment("Codex via Gateway", ExecutorKindCodex, ExecutionModeGateway), assignment("Pi via Gateway", ExecutorKindPi, ExecutionModeGateway), assignment("Reasonix via Gateway", ExecutorKindReasonix, ExecutionModeGateway)}
	default:
		return []ProviderAssignment{}
	}
}
