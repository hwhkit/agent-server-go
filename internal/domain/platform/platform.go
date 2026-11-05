// Package platform 定义平台协议域：bound-task 信封、签名规范与 callback
// 回执分类。全部是纯值对象与纯函数——协议行为可脱离 HTTP 单测。
package platform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// Push 入口的请求头（对上游平台 agent-center-go 的线协议契约）。
const (
	HeaderSignature      = "X-IR-Signature"
	HeaderRunID          = "X-IR-Run-ID"
	HeaderIdempotencyKey = "X-IR-Idempotency-Key"
)

// 动态 Agent 入口 / callback v2 的请求头。
const (
	HeaderAgentSignature      = "X-Agent-Signature"
	HeaderAgentTimestamp      = "X-Agent-Timestamp"
	HeaderAgentNonce          = "X-Agent-Nonce"
	HeaderAgentRunID          = "X-Agent-Run-Id"
	HeaderIdempotencyKeyAgent = "X-Idempotency-Key"
)

// 信封 schema 标识。
const (
	SchemaBoundTaskV1 = "ir-agent-bound-task/v1"
	SchemaPayloadV1   = "ir-agent-payload/v1"
	SchemaUsageV2     = "ir-bound-usage-v2"
)

// SignatureEncoding 是签名编码。
type SignatureEncoding string

// String 返回编码名。
func (e SignatureEncoding) String() string { return string(e) }

const (
	EncodingHex    SignatureEncoding = "hex"
	EncodingBase64 SignatureEncoding = "base64"
)

// ParseEncoding 严格解析编码，大小写不敏感。
func ParseEncoding(v string) (SignatureEncoding, error) {
	switch SignatureEncoding(strings.ToLower(strings.TrimSpace(v))) {
	case EncodingHex, EncodingBase64:
		return SignatureEncoding(strings.ToLower(v)), nil
	}
	return "", errors.New("signature encoding must be hex or base64")
}

// SignBody 计算 body-only HMAC-SHA256（push 兼容签名路径的 canonical bytes
// 就是原始请求体）。
func SignBody(secret, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return mac.Sum(nil)
}

// EncodeSignature 按编码输出签名字符串。
func EncodeSignature(sig []byte, enc SignatureEncoding) string {
	if enc == EncodingBase64 {
		return base64.StdEncoding.EncodeToString(sig)
	}
	return hex.EncodeToString(sig)
}

// VerifyBodySignature 恒定时间比较 body-only 签名。
func VerifyBodySignature(secret, body []byte, presented string, enc SignatureEncoding) bool {
	expected := EncodeSignature(SignBody(secret, body), enc)
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(presented)))
}

// Receipt 是一次 callback 投递的回执分类。分类语义与既有系统一致：
// 只有 HTTP 200 + code=OK + accepted=true 才算投递成功；明确拒绝、确定
// 投递失败与无法确认分别记录，不把不确定结果当作可安全重试。
type Receipt struct {
	HTTPStatus int
	Code       string
	Accepted   bool
	RunStatus  string
	// Classification 是回执裁决。
	Classification ReceiptClass
}

// ReceiptClass 是回执分类。
type ReceiptClass string

const (
	// ReceiptAccepted 投递成功（200 + OK + accepted=true）。
	ReceiptAccepted ReceiptClass = "accepted"
	// ReceiptRejected 平台明确拒绝（不可重试）。
	ReceiptRejected ReceiptClass = "rejected"
	// ReceiptDeliveryFailed 确定投递失败（可有限重试）。
	ReceiptDeliveryFailed ReceiptClass = "delivery_failed"
	// ReceiptUncertain 无法确认（保守保持 202 重放，不自动重试副作用）。
	ReceiptUncertain ReceiptClass = "uncertain"
)

// Classify 按既有回调协议裁决回执。
func Classify(httpStatus int, body []byte) Receipt {
	r := Receipt{HTTPStatus: httpStatus}
	if httpStatus == 200 && len(body) > 0 {
		var doc struct {
			Code      string `json:"code"`
			Accepted  bool   `json:"accepted"`
			RunStatus string `json:"runStatus"`
		}
		if err := json.Unmarshal(body, &doc); err == nil {
			r.Code, r.Accepted, r.RunStatus = doc.Code, doc.Accepted, doc.RunStatus
			if doc.Code == "OK" && doc.Accepted {
				r.Classification = ReceiptAccepted
				return r
			}
			r.Classification = ReceiptRejected
			return r
		}
	}
	switch {
	case httpStatus == 408, httpStatus == 429, httpStatus >= 500:
		r.Classification = ReceiptDeliveryFailed
	default:
		r.Classification = ReceiptUncertain
	}
	return r
}
