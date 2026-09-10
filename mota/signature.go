package mota

// `X-MoTA-Signature` 的 HMAC-SHA256 计算与校验。
// 对应 Rust 侧 src/mota/signature.rs。
//
// 协议约定:
//
//	X-MoTA-Signature: <hmac-sha256(body, signing secret)>
//
// 编码方式支持 hex(小写十六进制,默认)与 base64,由配置 signature_encoding
// 控制。校验使用常数时间比较,避免时序侧信道。

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// SignatureEncoding 是签名编码方式。
type SignatureEncoding string

const (
	SignatureEncodingHex    SignatureEncoding = "hex"
	SignatureEncodingBase64 SignatureEncoding = "base64"
)

// ParseSignatureEncoding 大小写不敏感地解析编码名;无法识别时返回 false。
func ParseSignatureEncoding(value string) (SignatureEncoding, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "hex":
		return SignatureEncodingHex, true
	case "base64":
		return SignatureEncodingBase64, true
	default:
		return "", false
	}
}

func (e SignatureEncoding) String() string { return string(e) }

// ComputeSignature 计算 hmac-sha256(secret, body) 并按编码方式输出。
// 未识别的编码按 hex 处理,与 Rust 侧 SignatureEncoding 的兜底一致。
func ComputeSignature(secret string, body []byte, encoding SignatureEncoding) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sum := mac.Sum(nil)
	if encoding == SignatureEncodingBase64 {
		return base64.StdEncoding.EncodeToString(sum)
	}
	return hex.EncodeToString(sum)
}

// VerifySignature 校验请求头里的签名是否与 body 匹配。
//
// 先把外部编码解码为原始 MAC 字节,再做常数时间比较;这样不必为每个请求
// 重新编码期望签名的字符串。
func VerifySignature(secret string, body []byte, provided string, encoding SignatureEncoding) bool {
	var providedBytes []byte
	var err error
	if encoding == SignatureEncodingBase64 {
		providedBytes, err = base64.StdEncoding.DecodeString(provided)
	} else {
		providedBytes, err = hex.DecodeString(provided)
	}
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	if len(providedBytes) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(providedBytes, expected) == 1
}
