package execengine

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (e *AgentExecutor) executeClaudeManaged(parent context.Context, runtime *AgentConfig, rawInput any, timeout time.Duration, policy *outputPolicy) (*ProcessedOutput, *ExecutionError) {
	config := e.config.ClaudeManaged
	if !config.Enabled || config.APIKey == "" || config.AgentID == "" || config.EnvironmentID == "" {
		return nil, NewExecutionError("executor_unavailable", "Claude Managed Agents executor is disabled or missing credentials/resource IDs")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	prompt := buildPrompt(runtime, rawInput)
	if len(prompt) > e.config.LocalAgents.MaxPromptBytes {
		return nil, NewExecutionError("input_too_large", "Claude Managed Agent prompt exceeded the configured byte limit")
	}
	headers := map[string]string{"x-api-key": config.APIKey, "anthropic-version": "2023-06-01", "anthropic-beta": "managed-agents-2026-04-01"}
	if config.WorkspaceID != "" {
		headers["anthropic-workspace-id"] = config.WorkspaceID
	}
	agent := map[string]any{"type": "agent_with_overrides", "id": config.AgentID, "system": fmt.Sprintf("You are the Claude Managed Agent for Intelligent Router role `%s`. Follow the role contract in the user message exactly and return one JSON object without markdown fences.", runtime.SystemPrompt)}
	if model := claudeManagedModel(runtime.ClaudeModel); model != "" {
		agent["model"] = map[string]any{"id": model}
	}
	if tools := claudeManagedTools(runtime); tools != nil {
		agent["tools"] = tools
	}
	operationID := runtime.Slug
	if value, ok := rawInput.(map[string]any); ok {
		if runID, ok := value["runId"].(string); ok && runID != "" {
			operationID = runID
		}
	}
	body := map[string]any{"agent": agent, "environment_id": config.EnvironmentID, "title": "IR " + runtime.Slug + " " + operationID, "metadata": map[string]any{"ir_agent_slug": runtime.Slug, "ir_agent_revision": fmt.Sprint(runtime.Revision), "ir_run_id": operationID}}
	if len(config.VaultIDs) > 0 {
		body["vault_ids"] = config.VaultIDs
	}
	if config.MaxListCostCents != nil {
		body["budget"] = map[string]any{"type": "limit", "max_list_cost": map[string]any{"amount": fmt.Sprint(*config.MaxListCostCents), "currency": "USD"}}
	}
	created, execErr := managedJSON(ctx, e.http, http.MethodPost, config.BaseURL+"/sessions", headers, body, config.MaxResponseBytes)
	if execErr != nil {
		return nil, execErr
	}
	sessionID, _ := created["id"].(string)
	if sessionID == "" {
		return nil, NewExecutionError("invalid_executor_output", "Claude Managed Agents session response omitted id")
	}
	base := config.BaseURL + "/sessions/" + sessionID
	stream, execErr := managedRequest(ctx, e.http, http.MethodGet, base+"/events/stream", mergeHeaders(headers, map[string]string{"Accept": "text/event-stream"}), nil)
	if execErr != nil {
		managedCleanup(e.http, headers, "", nil, base, config.DeleteSession)
		return nil, execErr
	}
	eventBody := map[string]any{"events": []any{map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": prompt}}}}}
	if response, submitErr := managedRequest(ctx, e.http, http.MethodPost, base+"/events", headers, eventBody); submitErr != nil {
		stream.Body.Close()
		managedCleanup(e.http, headers, base+"/events", map[string]any{"events": []any{map[string]any{"type": "user.interrupt"}}}, base, config.DeleteSession)
		return nil, submitErr
	} else {
		response.Body.Close()
	}
	var finalText, stopReason string
	var rawUsage any
	invocations := uint64(0)
	execErr = readSSE(ctx, stream, config.MaxResponseBytes, func(event map[string]any) (bool, *ExecutionError) {
		typeName, _ := event["type"].(string)
		switch typeName {
		case "agent.message":
			if blocks, ok := event["content"].([]any); ok {
				var texts []string
				for _, block := range blocks {
					if item, ok := block.(map[string]any); ok && item["type"] == "text" {
						if text, ok := item["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
				if len(texts) > 0 {
					finalText = strings.Join(texts, "\n")
				}
			}
		case "session.usage":
			rawUsage = event["usage"]
		case "span.model_request_end":
			invocations++
		case "session.error", "session.status_terminated":
			return false, NewExecutionError("executor_failed", "Claude Managed Agent session failed: "+typeName)
		case "session.status_idle":
			stopReason = claudeStopReason(event)
			if stopReason != "end_turn" {
				return false, NewExecutionError("executor_failed", "Claude Managed Agent stopped with reason "+stopReason)
			}
			if strings.TrimSpace(finalText) == "" {
				return false, NewExecutionError("invalid_executor_output", "Claude Managed Agent completed without an agent.message")
			}
			return true, nil
		}
		return false, nil
	})
	if execErr != nil {
		managedCleanup(e.http, headers, base+"/events", map[string]any{"events": []any{map[string]any{"type": "user.interrupt"}}}, base, config.DeleteSession)
		return nil, execErr.WithExecutorOutput(map[string]any{"provider": "claude_managed_agents", "sessionId": sessionID})
	}
	managedCleanup(e.http, headers, "", nil, base, config.DeleteSession)
	candidate := parseTextCandidate(finalText)
	if missing := missingRoleFields(candidate, runtime.SystemPrompt, policy.isFinal); len(missing) > 0 {
		return nil, NewExecutionError("invalid_executor_output", "Claude Managed Agent output missed required role fields: "+strings.Join(missing, ", ")).WithExecutorOutput(candidate)
	}
	output, normalizeErr := outputFromClaude(candidate, runtime, policy)
	if normalizeErr != nil {
		return nil, normalizeErr
	}
	if invocations == 0 {
		invocations = 1
	}
	output.Usage = claudeManagedUsage(rawUsage, invocations)
	output.ExecutorOutput = map[string]any{"provider": "claude_managed_agents", "sessionId": sessionID, "output": candidate, "usage": rawUsage, "stopReason": stopReason, "modelInvocations": invocations}
	return output, nil
}

func claudeManagedModel(value *string) string {
	if value == nil {
		return ""
	}
	model := strings.TrimSpace(*value)
	switch model {
	case "", "deepseek-v4-flash", "dsk_4f":
		return ""
	case "claude-opus5":
		return "claude-opus-5"
	case "claude-opus4.8":
		return "claude-opus-4-8"
	case "claude-sonnet5":
		return "claude-sonnet-5"
	}
	return model
}
func claudeManagedTools(runtime *AgentConfig) any {
	for _, tool := range runtime.AllowedTools {
		if strings.HasPrefix(tool, "mcp:") {
			return nil
		}
	}
	mapping := map[string]string{"bash": "bash", "read": "read", "write": "write", "edit": "edit", "glob": "glob", "grep": "grep", "websearch": "web_search", "web_search": "web_search", "webfetch": "web_fetch", "web_fetch": "web_fetch"}
	names := map[string]bool{}
	for _, tool := range runtime.AllowedTools {
		if name := mapping[strings.ToLower(strings.TrimSpace(tool))]; name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	if runtime.PermissionMode == "plan" {
		for name := range names {
			if name != "read" && name != "glob" && name != "grep" && name != "web_search" && name != "web_fetch" {
				delete(names, name)
			}
		}
	}
	names["read"] = true
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	policy := "always_allow"
	if runtime.PermissionMode == "manual" {
		policy = "always_ask"
	}
	configs := make([]any, 0, len(ordered))
	for _, name := range ordered {
		configs = append(configs, map[string]any{"name": name, "enabled": true, "permission_policy": map[string]any{"type": policy}})
	}
	return []any{map[string]any{"type": "agent_toolset_20260401", "default_config": map[string]any{"enabled": false}, "configs": configs}}
}
func claudeStopReason(event map[string]any) string {
	if value, ok := event["stop_reason"].(string); ok {
		return value
	}
	if object, ok := event["stop_reason"].(map[string]any); ok {
		if value, ok := object["type"].(string); ok {
			return value
		}
	}
	return "unknown"
}
func claudeManagedUsage(raw any, invocations uint64) *BoundRunUsage {
	usage, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	input, ok1 := jsonUint64(usage["input_tokens"])
	output, ok2 := jsonUint64(usage["output_tokens"])
	if !ok1 || !ok2 {
		return nil
	}
	cacheRead, _ := jsonUint64(usage["cache_read_input_tokens"])
	cacheCreate := uint64(0)
	if detail, ok := usage["cache_creation"].(map[string]any); ok {
		five, _ := jsonUint64(detail["ephemeral_5m_input_tokens"])
		hour, _ := jsonUint64(detail["ephemeral_1h_input_tokens"])
		cacheCreate = five + hour
	}
	prompt := input + cacheRead + cacheCreate
	var cached *uint64
	if cacheRead > 0 {
		cached = &cacheRead
	}
	return FinalReportedUsage(prompt, cached, output, nil, invocations)
}
