package platform

// Callback 回传协议（纯逻辑）：v2 签名、严格回执解析、origin 规则。
// HTTP 投递机制在 infra/httpx，重试策略在 app/callback。

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// CreatorSignatureHeader 是 callback 回传的签名头（v2 时间戳+HMAC）。
const CreatorSignatureHeader = "X-Creator-Signature"

// CallbackSignatureV2 用指定时间戳生成生产 v2 签名。
// canonical bytes = "v2\n" + timestamp + "\n" + runId + "\n" + body。
// 导出这个纯函数便于互操作测试（平台侧可用同一份 canonical 复算）。
func CallbackSignatureV2(signingSecret, runID string, body []byte, timestamp int64) string {
	stamp := strconv.FormatInt(timestamp, 10)
	canonical := make([]byte, 0, len(body)+len(runID)+len(stamp)+8)
	canonical = append(canonical, "v2\n"...)
	canonical = append(canonical, stamp...)
	canonical = append(canonical, '\n')
	canonical = append(canonical, runID...)
	canonical = append(canonical, '\n')
	canonical = append(canonical, body...)
	digest := EncodeSignature(SignBody([]byte(signingSecret), canonical), EncodingHex)
	return "t=" + stamp + ",v2=" + digest
}

// CallbackReceipt 是平台 callback 的结果层回执。
//
// HTTP 200 与 code=OK 只代表**传输**成功；结果是否被采纳必须读 Accepted。
type CallbackReceipt struct {
	Code      string  `json:"code"`
	Accepted  bool    `json:"accepted"`
	RunStatus *string `json:"runStatus,omitempty"`
	RunMsg    *string `json:"runMsg,omitempty"`
}

// callbackReceiptWire 用指针字段区分「字段缺失」与「零值」。
//
// rs 侧 CallbackReceipt 的 code / accepted 是必填字段，serde 遇到缺失会解析
// 失败；Go 的 json 包会静默填零值，所以这里显式检查存在性，保持两侧一致。
type callbackReceiptWire struct {
	Code      *string `json:"code"`
	Accepted  *bool   `json:"accepted"`
	RunStatus *string `json:"runStatus"`
	RunMsg    *string `json:"runMsg"`
}

// ParseCallbackReceipt 严格解析回执：code 与 accepted 必须存在且 code 合法。
func ParseCallbackReceipt(text string) (*CallbackReceipt, error) {
	var wire callbackReceiptWire
	if err := json.Unmarshal([]byte(text), &wire); err != nil {
		return nil, err
	}
	if wire.Code == nil {
		return nil, errors.New("missing field `code`")
	}
	if wire.Accepted == nil {
		return nil, errors.New("missing field `accepted`")
	}
	if SafeCallbackCode(*wire.Code) == "invalid code" {
		return nil, errors.New("field `code` is invalid")
	}
	return &CallbackReceipt{
		Code:      *wire.Code,
		Accepted:  *wire.Accepted,
		RunStatus: wire.RunStatus,
		RunMsg:    wire.RunMsg,
	}, nil
}

// CallbackResponseCode 取响应体里的 code 字段；读不到时返回 "no code"
// （与 rs 的 unwrap_or("no code") 一致，这个字符串会进错误文案）。
func CallbackResponseCode(text string) string {
	var decoded map[string]any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return "no code"
	}
	if code, ok := decoded["code"].(string); ok {
		return SafeCallbackCode(code)
	}
	return "no code"
}

// SafeCallbackCode 防止不受信对端把超长或带控制字符的值写进结构化日志
// 与审计详情。
func SafeCallbackCode(code string) string {
	if code == "" || len(code) > 128 {
		return "invalid code"
	}
	for _, char := range code {
		if char < 0x20 || char > 0x7e {
			return "invalid code"
		}
	}
	return code
}

// CallbackOriginKey 把回调 URL 归一化为无凭据的 origin 键（scheme://host 小写）。
func CallbackOriginKey(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("invalid callback origin")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

// ValidateCallbackTokenOrigin 校验配置的 legacy callback token 允许 origin：
// 必须是仅含 scheme+host 的绝对 HTTP(S) origin，非回环强制 HTTPS。
func ValidateCallbackTokenOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("must be an absolute HTTP(S) origin without credentials")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must contain only scheme and host")
	}
	if parsed.Scheme == "http" && !IsLoopbackHost(parsed.Hostname()) {
		return "", errors.New("must use HTTPS except for loopback development")
	}
	return CallbackOriginKey(raw)
}

// AppendCallbackToken 在 callback URL 上附加可选的回调令牌；token 为空时原样
// 返回。使用 net/url 编码 token，避免特殊字符改变 query 边界，并保留 fragment。
// 目标 origin 必须与配置的允许 origin 完全一致，否则拒绝（令牌只发给受信主机）。
func AppendCallbackToken(callbackURL, callbackToken, allowedOrigin string) (string, error) {
	if strings.TrimSpace(callbackToken) == "" {
		return callbackURL, nil
	}
	targetOrigin, err := CallbackOriginKey(callbackURL)
	if err != nil {
		return "", errors.New("callbackUrl is not an absolute URL")
	}
	normalizedAllowed, err := ValidateCallbackTokenOrigin(allowedOrigin)
	if err != nil {
		return "", errors.New("callback token origin is not configured")
	}
	if targetOrigin != normalizedAllowed {
		return "", errors.New("callbackUrl origin is not allowed to receive the callback token")
	}
	parsed, err := url.Parse(callbackURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", errors.New("callbackUrl is not an absolute URL")
	}
	query := parsed.Query()
	query.Add("token", callbackToken)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// CallbackOrigin 返回无凭据的日志/审计目标。
func CallbackOrigin(target string) string {
	parsed, err := url.Parse(target)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "<invalid callback URL>"
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// MaskCallbackToken 把 URL 里的 token 查询参数掩码，避免明文令牌进日志。
func MaskCallbackToken(target string) string {
	var out strings.Builder
	rest := target
	for {
		index := strings.Index(strings.ToLower(rest), "token=")
		if index < 0 {
			break
		}
		out.WriteString(rest[:index])
		out.WriteString("token=***")
		valueStart := index + len("token=")
		end := len(rest)
		if cut := strings.IndexAny(rest[valueStart:], "&#"); cut >= 0 {
			end = valueStart + cut
		}
		rest = rest[end:]
	}
	out.WriteString(rest)
	return out.String()
}
