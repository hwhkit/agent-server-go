// Package token 提供不透明 token 的生成与哈希（domain 层端口，纯标准库）。
//
// 格式与 hwhkit-go/tokengen 及存量实现逐字节兼容：URL-safe base64（无填充）
// token、小写十六进制 SHA-256 哈希。infra/crypto 是其与 secrets/tokengen
// 库的转发汇聚点；app 层一律 import 本包（分层规则：app 只依赖 domain）。
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// Random 返回 bytes 字节密码学随机数据的 URL-safe base64(无填充)编码。
func Random(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand 在受支持的平台上不会失败；真失败了继续跑等于发可预测 token。
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Hash 返回不透明 token 的小写十六进制 SHA-256。只存哈希，不存 token 本身。
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// BodyHash 返回请求体的小写十六进制 SHA-256（签名规范串/幂等指纹）。
// 与 Hash(token) 输入维度不同但算法一致，便于审计对照。
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
