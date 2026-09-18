package mota

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (e *AgentExecutor) executeOpenAIAgent(parent context.Context, runtime *AgentRuntime, rawInput any, timeout time.Duration, policy *outputPolicy) (*ProcessedOutput, *ExecutionError) {
	config := e.config.OpenAIAgents
	if !config.Enabled || config.APIKey == "" {
		return nil, newExecutionError("executor_unavailable", "OpenAI Agents API executor is disabled or missing its API key")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	prompt := buildPrompt(runtime, rawInput)
	if len(prompt) > e.config.LocalAgents.MaxPromptBytes {
		return nil, newExecutionError("input_too_large", "OpenAI Agent prompt exceeded the configured byte limit")
	}
	headers := map[string]string{"Authorization": "Bearer " + config.APIKey, "OpenAI-Beta": "agents=v1"}
	model := config.DefaultModel
	if runtime.Config.ClaudeModel != nil && strings.TrimSpace(*runtime.Config.ClaudeModel) != "" {
		model = strings.TrimSpace(*runtime.Config.ClaudeModel)
	}
	operationID := runtime.Config.Slug
	if value, ok := rawInput.(map[string]any); ok {
		if runID, ok := value["runId"].(string); ok && runID != "" {
			operationID = runID
		}
	}
	agent := map[string]any{
		"model":        model,
		"instructions": fmt.Sprintf("You are the managed OpenAI Agent for MOTA role `%s`. Follow the role contract in the user message and return only one JSON object.", runtime.Config.SystemPrompt),
		"reasoning":    map[string]any{"effort": config.ReasoningEffort, "summary": "auto"},
	}
	if tools := openAITools(config, runtime); len(tools) > 0 {
		agent["tools"] = tools
		prompt = strings.ReplaceAll(prompt, noToolClause, "")
	}
	created, execErr := managedJSON(ctx, e.http, http.MethodPost, config.BaseURL+"/agents/sessions", headers,
		map[string]any{"agent": agent, "environment": map[string]any{"type": "none"}, "metadata": map[string]any{"mota_agent_slug": runtime.Config.Slug, "mota_agent_revision": fmt.Sprint(runtime.Config.Revision), "mota_run_id": operationID}}, config.MaxResponseBytes)
	if execErr != nil {
		return nil, execErr
	}
	sessionID, _ := created["id"].(string)
	if sessionID == "" {
		return nil, newExecutionError("invalid_executor_output", "OpenAI Agents API session response omitted id")
	}
	base := config.BaseURL + "/agents/sessions/" + sessionID
	stream, execErr := managedRequest(ctx, e.http, http.MethodGet, base+"/events?stream=true", mergeHeaders(headers, map[string]string{"Accept": "text/event-stream"}), nil)
	if execErr != nil {
		managedCleanup(e.http, headers, "", nil, base, config.DeleteSession)
		return nil, execErr
	}
	input := map[string]any{
		"events": []any{map[string]any{
			"type": "agent.session.input.message",
			"input": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": prompt}},
			}},
		}},
		"idempotency_key": fmt.Sprintf("mota:%d:%s", runtime.Config.Revision, operationID),
	}
	if response, submitErr := managedRequest(ctx, e.http, http.MethodPost, base+"/events", headers, input); submitErr != nil {
		stream.Body.Close()
		managedCleanup(e.http, headers, base+"/events", map[string]any{"events": []any{map[string]any{"type": "agent.session.input.cancel"}}}, base, config.DeleteSession)
		return nil, submitErr
	} else {
		response.Body.Close()
	}
	var finalText, turnID string
	var rawUsage any
	execErr = readSSE(ctx, stream, config.MaxResponseBytes, func(event map[string]any) (bool, *ExecutionError) {
		typeName, _ := event["type"].(string)
		switch typeName {
		case "agent.session.turn.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				finalText += delta
			}
		case "agent.session.turn.output_text.done":
			if text, ok := event["text"].(string); ok {
				finalText = text
			}
		case "agent.session.requires_action":
			return false, newExecutionError("executor_failed", "OpenAI Agent requested an application-side action")
		case "error", "agent.session.failed", "agent.session.environment.failed", "agent.session.turn.failed", "agent.session.turn.cancelled":
			return false, newExecutionError("executor_failed", "OpenAI Agent session failed: "+typeName)
		case "agent.session.turn.completed":
			if turn, ok := event["turn"].(map[string]any); ok {
				turnID, _ = turn["id"].(string)
				rawUsage = turn["usage"]
			}
			if strings.TrimSpace(finalText) == "" {
				return false, newExecutionError("invalid_executor_output", "OpenAI Agent completed without output text")
			}
			return true, nil
		}
		return false, nil
	})
	if execErr != nil {
		managedCleanup(e.http, headers, base+"/events", map[string]any{"events": []any{map[string]any{"type": "agent.session.input.cancel"}}}, base, config.DeleteSession)
		return nil, execErr.WithExecutorOutput(map[string]any{"provider": "openai_agents", "sessionId": sessionID})
	}
	managedCleanup(e.http, headers, "", nil, base, config.DeleteSession)
	candidate := parseTextCandidate(finalText)
	if missing := missingRoleFields(candidate, runtime.Config.SystemPrompt, policy.isFinal); len(missing) > 0 {
		return nil, newExecutionError("invalid_executor_output", "OpenAI Agent output missed required role fields: "+strings.Join(missing, ", ")).WithExecutorOutput(candidate)
	}
	output, normalizeErr := outputFromClaude(candidate, runtime, policy)
	if normalizeErr != nil {
		return nil, normalizeErr
	}
	output.Usage = nil
	output.ExecutorOutput = map[string]any{"provider": "openai_agents", "sessionId": sessionID, "turnId": turnID, "output": candidate, "usage": rawUsage}
	return output, nil
}

func openAITools(config OpenAIAgentsConfig, runtime *AgentRuntime) []any {
	var tools []any
	for _, tool := range runtime.Config.AllowedTools {
		if tool == "web_search" {
			tools = append(tools, map[string]any{"type": "web_search"})
		}
	}
	var allowed []string
	for _, tool := range runtime.Config.AllowedTools {
		if value, ok := strings.CutPrefix(tool, "mcp:"); ok && value != "" {
			allowed = append(allowed, value)
		}
	}
	if config.MCPServerURL != "" && len(allowed) > 0 {
		transport := map[string]any{"type": "http", "server_url": config.MCPServerURL}
		if config.MCPAuthorization != "" {
			transport["authorization"] = config.MCPAuthorization
		}
		tools = append(tools, map[string]any{"type": "mcp", "server_label": config.MCPServerLabel, "transport": transport, "connection_origin": "service", "required": config.MCPRequired, "allowed_tools": allowed})
	}
	return tools
}

func mergeHeaders(left, right map[string]string) map[string]string {
	merged := map[string]string{}
	for k, v := range left {
		merged[k] = v
	}
	for k, v := range right {
		merged[k] = v
	}
	return merged
}
