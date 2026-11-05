package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// ---------------------------------------------------------------------------
// 动态 Agent 入口（/agent/{slug}）的协议值对象与纯函数
// ---------------------------------------------------------------------------

// 签名请求的时间窗与 nonce 保留期（秒）。
const (
	SignedRequestWindowSecs = 300
	RequestNonceRetention   = 600
	// GatewayIdempotencyStaleAfter 是 processing 记录被判定为陈旧、
	// 可被接管的阈值。
	GatewayIdempotencyStaleAfter = 2 * 60 * 60
)

// 动态入口的请求头常量在 platform.go（HeaderAgent* / HeaderIdempotencyKeyAgent）。

// GatewayDecisionKind 是网关幂等租约的决策种类。
type GatewayDecisionKind int

const (
	// GatewayNew 拿到了新租约，调用方负责执行并最终 Complete/Release。
	GatewayNew GatewayDecisionKind = iota
	// GatewayCached 已有完成结果，直接回放 ResponseBody。
	GatewayCached
	// GatewayInProgress 同键请求正在处理中（409）。
	GatewayInProgress
	// GatewayConflict 键已用于不同请求元数据（409）。
	GatewayConflict
)

// GatewayDecision 是一次幂等租约申请的结果。
type GatewayDecision struct {
	Kind GatewayDecisionKind
	// LeaseID 仅 GatewayNew 时有值。
	LeaseID string
	// RunID 在 GatewayNew 与 GatewayCached 时有值（缓存归属的运行）。
	RunID string
	// ResponseBody 仅 GatewayCached 时有值（可能是成功信封，也可能是
	// 缓存的错误信封——由 CachedErrorStatus 判别）。
	ResponseBody string
}

// GatewayFinalizationKind 是随运行终态提交的幂等迁移种类。
type GatewayFinalizationKind int

const (
	// GatewayFinalizeComplete 缓存响应体（同键重放返回它）。
	GatewayFinalizeComplete GatewayFinalizationKind = iota
	// GatewayFinalizeRelease 释放租约（允许同键重试重新执行）。
	GatewayFinalizeRelease
)

// GatewayFinalization 是一次幂等状态迁移（与运行终态同事务提交）。
type GatewayFinalization struct {
	Kind GatewayFinalizationKind
	Key  string
	// LeaseID 是迁移必须匹配的租约：已被接管/落定的记录不能被过期请求改写。
	LeaseID string
	// ResponseBody 仅 Complete 时有值。
	ResponseBody string
}

// CompleteGatewayIdempotency 构造「缓存响应」迁移。
func CompleteGatewayIdempotency(key, leaseID, responseBody string) *GatewayFinalization {
	return &GatewayFinalization{Kind: GatewayFinalizeComplete, Key: key,
		LeaseID: leaseID, ResponseBody: responseBody}
}

// ReleaseGatewayIdempotency 构造「释放租约」迁移。
func ReleaseGatewayIdempotency(key, leaseID string) *GatewayFinalization {
	return &GatewayFinalization{Kind: GatewayFinalizeRelease, Key: key, LeaseID: leaseID}
}

// GatewayRunFinalization 是一次运行终态落库（含随行的幂等迁移）。
type GatewayRunFinalization struct {
	AgentSlug          string
	RunID              string
	Status             string // completed | failed | canceled
	DurationMs         int64
	ErrorCode          *string
	ResponseCode       *int64
	ResultJSON         *string
	ExecutorResultJSON *string
	Idempotency        *GatewayFinalization
}

// GatewayIdempotencyRecordKey 生成幂等记录键。缓存的响应只属于产生它的
// 那个 Agent 修订版本。
func GatewayIdempotencyRecordKey(agentID, revision int64, idempotencyKey string) string {
	return fmt.Sprintf("%d:%d:%s", agentID, revision, idempotencyKey)
}

// AgentSignatureV1Canonical 构造 X-Agent-Signature v1 的 canonical 串：
// 方法 + 路径 + 时间戳 + nonce + runId + 幂等键 + body 摘要。
// 路径固定为 /agent/{slug}（/agent-center-go 别名同样按此计算）。
func AgentSignatureV1Canonical(slug, timestampText, nonce, runID, idempotencyKey string, body []byte) string {
	return fmt.Sprintf("v1\nPOST\n/agent/%s\n%s\n%s\n%s\n%s\n%s",
		slug, timestampText, nonce, runID, idempotencyKey, BodyHashHex(body))
}

// BodyHashHex 是 body 的小写十六进制 SHA-256（canonical 串的摘要段）。
func BodyHashHex(body []byte) string { return BodyHash(body) }

// ValidRequestNonce 校验 nonce 形态：16-128 个 URL-safe 字符。
func ValidRequestNonce(nonce string) bool {
	if len(nonce) < 16 || len(nonce) > 128 {
		return false
	}
	for i := 0; i < len(nonce); i++ {
		c := nonce[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// SerializedGatewayError 是动态入口的错误信封 {"error":{code,message}}。
func SerializedGatewayError(code, message string) string {
	encoded, err := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return `{"error":{"code":"serialization_failed"}}`
	}
	return string(encoded)
}

// CachedGatewayErrorStatus 判别缓存的响应体是否为错误信封，并给出应回放
// 的 HTTP 状态。非错误信封返回 (0, false)。
func CachedGatewayErrorStatus(body string) (int, bool) {
	var envelope struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil || envelope.Error == nil ||
		envelope.Error.Code == "" {
		return 0, false
	}
	switch envelope.Error.Code {
	case "callback_rejected", "callback_delivery_failed":
		return 502, true
	case "outcome_uncertain":
		return 503, true
	default:
		return ExecutionErrorStatus(envelope.Error.Code), true
	}
}

// ExecutionErrorStatus 把执行/分发错误码映射成 HTTP 状态。
func ExecutionErrorStatus(code string) int {
	switch code {
	case "execution_timeout":
		return 504
	case "executor_unavailable", "agent_disabled", "agent_not_found":
		return 503
	case "agent_overloaded":
		return 429
	case "output_too_large", "input_too_large", "invalid_executor_output", "usage_unavailable":
		return 502
	default:
		return 500
	}
}

// GatewayMetadataValid 校验 runId/幂等键：1..limit 个可打印 ASCII，无首尾空白。
func GatewayMetadataValid(value string, limit int) bool {
	if value == "" || len(value) > limit || value != trimSpace(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t' ||
		value[start] == '\n' || value[start] == '\r') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' ||
		value[end-1] == '\n' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}

// FormatRevision 便于响应头渲染。
func FormatRevision(revision int64) string { return strconv.FormatInt(revision, 10) }

// BodyHash 返回请求体的小写十六进制 SHA-256。
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
