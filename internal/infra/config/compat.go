package config

// 签名编码与 origin 校验：与 domain/platform 及旧栈行为逐字一致的兼容层。

import (
	"ir-agent-server/internal/domain/platform"
)

// SignatureEncoding 是签名编码方式（domain/platform 的同源别名）。
type SignatureEncoding = platform.SignatureEncoding

const (
	SignatureEncodingHex    = platform.EncodingHex
	SignatureEncodingBase64 = platform.EncodingBase64
)

// ParseSignatureEncoding 大小写不敏感地解析编码名；无法识别时返回 false。
func ParseSignatureEncoding(value string) (SignatureEncoding, bool) {
	enc, err := platform.ParseEncoding(value)
	return enc, err == nil
}

// Callback origin 规则已归位 domain/platform（纯领域规则）；此处保留兼容别名。
var (
	CallbackOriginKey           = platform.CallbackOriginKey
	ValidateCallbackTokenOrigin = platform.ValidateCallbackTokenOrigin
)

// IsLoopbackHost 已归位 domain/platform（纯领域规则）；此处保留兼容别名。
var IsLoopbackHost = platform.IsLoopbackHost
