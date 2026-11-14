// Package crypto 转发到通用库 hwhkit-go/secrets 与 hwhkit-go/tokengen。
//
// 公开 API 与历史实现完全一致（SecretCipher/Encrypt/Decrypt、RandomToken/
// TokenHash/BodyHash、HashPassword/VerifyPassword）；密文信封与哈希格式
// 逐字节兼容（验收向量见 hwhkit secrets 测试）。存量数据库行无需迁移。
package crypto

import (
	"github.com/hwhkit/hwhkit-go/secrets"
	"github.com/hwhkit/hwhkit-go/tokengen"
)

// SecretCipher 与 secrets.SecretCipher 同型（AES-256-GCM，AAD=上下文）。
type SecretCipher = secrets.SecretCipher

// NewSecretCipher 转发 secrets.NewSecretCipher。
func NewSecretCipher(masterSecret string) (*SecretCipher, error) {
	return secrets.NewSecretCipher(masterSecret)
}

// RandomToken 转发 tokengen.RandomToken。
func RandomToken(bytes int) string { return tokengen.RandomToken(bytes) }

// TokenHash 转发 tokengen.TokenHash。
func TokenHash(token string) string { return tokengen.TokenHash(token) }

// BodyHash 转发 tokengen.BodyHash。
func BodyHash(body []byte) string { return tokengen.BodyHash(body) }
