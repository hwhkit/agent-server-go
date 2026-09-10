package mota

// 平台内部密钥保护与不透明 token 工具。
// 对应 Rust 侧 src/platform/crypto.rs。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// secretCipherNonceSize 与 Rust 侧固定的 12 字节 AES-GCM nonce 对齐。
const secretCipherNonceSize = 12

// SecretCipher 用 SHA-256(master_secret) 派生的 256 位密钥做 AES-256-GCM。
// 密文信封为 nonce(12B) || ciphertext,再以 URL-safe base64(无填充)编码。
type SecretCipher struct{ aead cipher.AEAD }

// NewSecretCipher 由主密钥派生 AES-256-GCM。SHA-256 恒产出 32 字节密钥,
// 因此这里不会因密钥长度失败。
func NewSecretCipher(masterSecret string) (*SecretCipher, error) {
	key := sha256.Sum256([]byte(masterSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("build AES-256 cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithNonceSize(block, secretCipherNonceSize)
	if err != nil {
		return nil, fmt.Errorf("build AES-256-GCM: %w", err)
	}
	return &SecretCipher{aead: aead}, nil
}

// Encrypt 用 context 作为 AAD 加密 plaintext。context 与解密时必须完全一致,
// 这样一条密文无法被挪用到另一个 Agent 上。
func (c *SecretCipher) Encrypt(plaintext, context string) (string, error) {
	nonce := make([]byte, secretCipherNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	envelope := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte(context))
	return base64.RawURLEncoding.EncodeToString(envelope), nil
}

// Decrypt 还原 Encrypt 产生的信封。信封被截断或 AAD 不匹配时报错。
func (c *SecretCipher) Decrypt(encoded, context string) (string, error) {
	envelope, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode encrypted secret: %w", err)
	}
	if len(envelope) <= secretCipherNonceSize {
		return "", fmt.Errorf("encrypted secret envelope is truncated")
	}
	nonce, ciphertext := envelope[:secretCipherNonceSize], envelope[secretCipherNonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(context))
	if err != nil {
		return "", fmt.Errorf("decrypt secret")
	}
	return string(plaintext), nil
}

// randomToken 返回 bytes 字节密码学随机数据的 URL-safe base64(无填充)编码。
func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand 在受支持的平台上不会失败;真失败了继续跑等于发可预测 token。
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// tokenHash 返回不透明 token 的小写十六进制 SHA-256。只存哈希,不存 token 本身。
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// bodyHash 返回请求体的小写十六进制 SHA-256,用于签名规范串与幂等请求指纹。
func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
