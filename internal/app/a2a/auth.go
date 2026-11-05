package a2a

// A2A 端点认证。对应 rs 侧 src/a2a/auth.rs。
//
// A2A 客户端不会计算本项目自有的 X-Agent-Signature HMAC,规范里能声明的是
// securitySchemes,因此这里用 Bearer token。
//
// token 不是 Agent 的 signing secret 本身,而是从它派生出来的:
//
//	token = "a2a_" + hex(HMAC-SHA256(signing_secret, "a2a-bearer-v1\n{slug}"))
//
// 这样做的原因:bearer token 会出现在每个请求头里,比只用于签名的密钥暴露面
// 大得多(反向代理日志、客户端配置)。派生后即使 token 泄漏,也不能用来伪造
// X-Agent-Signature。派生只绑定 slug 不绑定 revision,所以改提示词、超时等
// 配置不会让调用方的 token 失效,只有轮换 signing secret 才会。

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"ir-agent-server/internal/domain/agent"
	"strings"
)

const a2aTokenPrefix = "a2a_"
const a2aDerivationLabel = "a2a-bearer-v1"

// A2ADeriveToken 派生某个 Agent 的 A2A bearer token。
func A2ADeriveToken(signingSecret, slug string) string {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(a2aDerivationLabel))
	mac.Write([]byte("\n"))
	mac.Write([]byte(slug))
	return a2aTokenPrefix + hex.EncodeToString(mac.Sum(nil))
}

// A2AAuthOutcome 是认证结果。
type A2AAuthOutcome int

const (
	A2AAuthOK A2AAuthOutcome = iota
	// A2AAuthMissing 表示没带 Authorization 头,或者不是 Bearer 方案。
	A2AAuthMissing
	// A2AAuthInvalid 表示带了但对不上。
	A2AAuthInvalid
)

// A2AVerifyAuth 校验 Authorization: Bearer <token>。
func A2AVerifyAuth(agentCfg *agent.ResolvedConfig, authorization string) A2AAuthOutcome {
	presented, ok := a2aStripBearer(authorization)
	if !ok {
		return A2AAuthMissing
	}
	expected := A2ADeriveToken(agentCfg.SigningSecret, agentCfg.Slug)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1 {
		return A2AAuthOK
	}
	return A2AAuthInvalid
}

// a2aStripBearer 取出 Bearer 凭证。方案名按 RFC 9110 §11.1 大小写不敏感。
func a2aStripBearer(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	scheme, credentials, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	credentials = strings.TrimSpace(credentials)
	if credentials == "" {
		return "", false
	}
	return credentials, true
}
