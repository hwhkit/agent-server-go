package crypto_test

import (
	"strings"
	"testing"

	"ir-agent-server/internal/infra/crypto"
)

// 旧栈兼容 golden vector：以下两个常量由删除前的 ir/ 包实际生成
// （ir.NewSecretCipher("test-master-secret").Encrypt 与
// ir.HashAdminPassword，2026-11-05 捕获）。它们把「存量数据库行必须
// 永远可读可验」的保证固化下来——旧栈代码删除后，兼容性守护不再依赖
// 双实现互验，而是依赖这对固定向量。
const (
	legacyGoldenCiphertext = "5WEyS7dxI5f2E0P2-Q64TgQifqXwRR0vamcVwV0-Md4W0y6LtWTKoNJgIwpoIe8Reg4"
	legacyGoldenPassword   = "golden-vector-password"
	legacyGoldenHash       = "$argon2id$v=19$m=19456,t=2,p=1$/Ki2Hs2zO7ZquYqBIMZ7Dw$gdp3R9D3+fCeQSo4OEd0z2QNpT1uzQVBKsaUtacUAok"
)

// TestSecretCipherLegacyCiphertext 证明新实现能解密旧栈密文（存量数据库行
// 在切换后必须可读），且新实现产出的密文信封形态一致（可被同一算法族解析）。
func TestSecretCipherLegacyCiphertext(t *testing.T) {
	t.Parallel()
	newCipher, err := crypto.NewSecretCipher("test-master-secret")
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	const plaintext = "sk-secret-value-测试"
	const context = "demo-agent"

	got, err := newCipher.Decrypt(legacyGoldenCiphertext, context)
	if err != nil || got != plaintext {
		t.Fatalf("decrypt of legacy ciphertext = (%q, %v)", got, err)
	}

	// 往返自洽 + 信封长度一致（nonce 12B + 密文，URL-safe base64 无填充）。
	newEncrypted, err := newCipher.Encrypt(plaintext, context)
	if err != nil {
		t.Fatalf("new encrypt: %v", err)
	}
	if roundTrip, err := newCipher.Decrypt(newEncrypted, context); err != nil || roundTrip != plaintext {
		t.Fatalf("round trip = (%q, %v)", roundTrip, err)
	}
	if len(newEncrypted) != len(legacyGoldenCiphertext) {
		t.Errorf("envelope length drifted: new %d vs legacy %d",
			len(newEncrypted), len(legacyGoldenCiphertext))
	}
}

func TestSecretCipherAADBinding(t *testing.T) {
	t.Parallel()
	cipher, err := crypto.NewSecretCipher("master")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	encrypted, err := cipher.Encrypt("secret", "agent-a")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := cipher.Decrypt(encrypted, "agent-b"); err == nil {
		t.Error("ciphertext must not decrypt under a different AAD context")
	}
	if _, err := cipher.Decrypt(encrypted[:8], "agent-a"); err == nil {
		t.Error("truncated envelope must be rejected")
	}
}

// TestPasswordHashCompatibleWithLegacy 校验旧栈生成的 PHC 哈希（golden
// vector）在新实现下必须可验——存量 admin_users.password_hash 行在切换后
// 不能失效；同时校验新实现的参数与 PHC 前缀未漂移。
func TestPasswordHashCompatibleWithLegacy(t *testing.T) {
	t.Parallel()
	if !crypto.VerifyPassword(legacyGoldenHash, legacyGoldenPassword) {
		t.Error("legacy golden hash must verify with new implementation")
	}
	if crypto.VerifyPassword(legacyGoldenHash, "wrong password") {
		t.Error("wrong password must not verify against legacy golden hash")
	}

	password := "correct horse battery staple"
	newHash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(newHash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("unexpected PHC prefix: %s", newHash[:40])
	}
	if !crypto.VerifyPassword(newHash, password) {
		t.Error("new hash must verify with new implementation")
	}
	if crypto.VerifyPassword(newHash, "wrong password") {
		t.Error("wrong password must not verify")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "$$", "$argon2id$v=19$m=19456,t=2,p=1$!!$!!", "bcrypt-hash"} {
		if crypto.VerifyPassword(bad, "x") {
			t.Errorf("malformed hash %q must not verify", bad)
		}
	}
}

func TestTokenHelpers(t *testing.T) {
	t.Parallel()
	token := crypto.RandomToken(32)
	if len(token) < 40 {
		t.Errorf("random token too short: %d chars", len(token))
	}
	if crypto.TokenHash(token) != crypto.TokenHash(token) {
		t.Error("token hash must be deterministic")
	}
	if crypto.TokenHash(token) == crypto.TokenHash(token+"x") {
		t.Error("different tokens must hash differently")
	}
	if crypto.BodyHash([]byte("a")) == crypto.BodyHash([]byte("b")) {
		t.Error("different bodies must hash differently")
	}
}
