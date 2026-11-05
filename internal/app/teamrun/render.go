package teamrun

// 步骤输入模板渲染。v1 支持两种占位符（蓝图 §3.3）：
//
//	{{team.input}}          团队级输入原文
//	{{steps.<id>.output}}   上游步骤的输出（子运行的 V2 结果信封）
//
// 模板本身必须是合法 JSON：占位符出现在 JSON 字符串值内。占位符独占整个
// 字符串时以原始 JSON 值替换（保持类型）；内嵌在长字符串中时以紧凑 JSON
// 文本替换。已知模式但取值缺失（上游无输出）渲染为 null；未知模式的占位符
// 原样保留（向前兼容）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// RenderContext 是渲染上游依赖的取值环境。
type RenderContext struct {
	TeamInput   json.RawMessage
	StepOutputs map[string]json.RawMessage // stepID → 输出 JSON
}

// Render 渲染步骤输入模板。模板为空时回退为团队级输入。
func Render(template json.RawMessage, rc RenderContext) (json.RawMessage, error) {
	if len(template) == 0 || string(template) == "null" {
		if len(rc.TeamInput) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return rc.TeamInput, nil
	}
	var parsed any
	if err := json.Unmarshal(template, &parsed); err != nil {
		return nil, fmt.Errorf("input template is not valid JSON: %w", err)
	}
	rendered, err := renderValue(parsed, rc)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rendered)
}

func renderValue(value any, rc RenderContext) (any, error) {
	switch typed := value.(type) {
	case string:
		return renderString(typed, rc), nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			rendered, err := renderValue(item, rc)
			if err != nil {
				return nil, err
			}
			out[key] = rendered
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			rendered, err := renderValue(item, rc)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered)
		}
		return out, nil
	default:
		return value, nil
	}
}

// renderString 处理占位符：整串占位返回原始 JSON 值（解码进 any），
// 内嵌占位做紧凑文本替换。
func renderString(text string, rc RenderContext) any {
	trimmed := strings.TrimSpace(text)
	if raw, ok := lookupPlaceholder(trimmed, rc); ok {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err == nil {
			return decoded
		}
		return string(raw)
	}
	// 内嵌替换：扫描所有 {{...}} 片段。
	var out strings.Builder
	rest := text
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			out.WriteString(rest)
			break
		}
		placeholder := strings.TrimSpace(rest[start : start+end+2])
		out.WriteString(rest[:start])
		if raw, ok := lookupPlaceholder(placeholder, rc); ok {
			out.Write(compactJSON(raw))
		} else {
			out.WriteString(placeholder)
		}
		rest = rest[start+end+2:]
	}
	return out.String()
}

// lookupPlaceholder 解析 {{team.input}} / {{steps.<id>.output}}。
func lookupPlaceholder(placeholder string, rc RenderContext) (json.RawMessage, bool) {
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(placeholder, "{{"), "}}"))
	if inner == "team.input" {
		if len(rc.TeamInput) == 0 {
			return json.RawMessage("null"), true
		}
		return rc.TeamInput, true
	}
	if stepID, found := strings.CutPrefix(inner, "steps."); found {
		if suffix, ok := strings.CutSuffix(stepID, ".output"); ok {
			if raw, exists := rc.StepOutputs[suffix]; exists {
				return raw, true
			}
			return json.RawMessage("null"), true
		}
	}
	return nil, false
}

func compactJSON(raw json.RawMessage) []byte {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return []byte(raw)
	}
	return buffer.Bytes()
}
