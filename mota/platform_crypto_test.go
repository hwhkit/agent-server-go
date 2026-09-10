package mota

import (
	"encoding/base64"
	"strings"
	"testing"
)

func newTestCipher(t *testing.T) *SecretCipher {
	t.Helper()
	cipher, err := NewSecretCipher(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	return cipher
}

func TestSecretCipherRoundtrip(t *testing.T) {
	cipher := newTestCipher(t)
	for _, plaintext := range []string{
		"agent-signing-secret",
		"",
		"包含中文与符号 !@#$%^&*()",
		strings.Repeat("x", 512),
	} {
		encoded, err := cipher.Encrypt(plaintext, "research-one")
		if err != nil {
			t.Fatalf("encrypt %q: %v", plaintext, err)
		}
		decoded, err := cipher.Decrypt(encoded, "research-one")
		if err != nil {
			t.Fatalf("decrypt %q: %v", plaintext, err)
		}
		if decoded != plaintext {
			t.Errorf("roundtrip = %q, want %q", decoded, plaintext)
		}
	}
}

// 同一明文两次加密必须产生不同密文(nonce 随机)。
func TestSecretCipherUsesFreshNonce(t *testing.T) {
	cipher := newTestCipher(t)
	first, err := cipher.Encrypt("same-secret", "slug")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cipher.Encrypt("same-secret", "slug")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("encrypting the same plaintext twice must not produce identical ciphertext")
	}
}

// context 就是 AAD:换一个 context 必须解不开。这样一条密文无法被挪用到别的 Agent。
func TestSecretCipherContextIsAuthenticated(t *testing.T) {
	cipher := newTestCipher(t)
	encoded, err := cipher.Encrypt("agent-signing-secret", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.Decrypt(encoded, "agent-b"); err == nil {
		t.Error("decrypting with a different context must fail")
	}
	if _, err := cipher.Decrypt(encoded, ""); err == nil {
		t.Error("decrypting with an empty context must fail")
	}
}

// 换主密钥后旧密文必须解不开。
func TestSecretCipherRejectsForeignMasterKey(t *testing.T) {
	encoded, err := newTestCipher(t).Encrypt("agent-signing-secret", "slug")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewSecretCipher(strings.Repeat("m", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt(encoded, "slug"); err == nil {
		t.Error("a different master secret must not decrypt the envelope")
	}
}

func TestSecretCipherRejectsMalformedEnvelope(t *testing.T) {
	cipher := newTestCipher(t)
	for name, encoded := range map[string]string{
		"not base64":         "!!!not-base64!!!",
		"empty":              "",
		"nonce only":         base64.RawURLEncoding.EncodeToString(make([]byte, secretCipherNonceSize)),
		"shorter than nonce": base64.RawURLEncoding.EncodeToString(make([]byte, secretCipherNonceSize-1)),
	} {
		if _, err := cipher.Decrypt(encoded, "slug"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// 篡改密文任意一字节都必须被 GCM 检出。
func TestSecretCipherDetectsTampering(t *testing.T) {
	cipher := newTestCipher(t)
	encoded, err := cipher.Encrypt("agent-signing-secret", "slug")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	if _, err := cipher.Decrypt(base64.RawURLEncoding.EncodeToString(raw), "slug"); err == nil {
		t.Error("tampered ciphertext must fail authentication")
	}
}

// 解密失败的错误信息不得回显明文或密文。
func TestSecretCipherErrorsDoNotLeakSecrets(t *testing.T) {
	cipher := newTestCipher(t)
	encoded, err := cipher.Encrypt("super-secret-plaintext", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = cipher.Decrypt(encoded, "agent-b")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-plaintext") || strings.Contains(err.Error(), encoded) {
		t.Errorf("error message leaks secret material: %v", err)
	}
}

func TestRandomTokenLengthAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		token := randomToken(18)
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("token %q is not URL-safe base64: %v", token, err)
		}
		if len(raw) != 18 {
			t.Fatalf("decoded token length = %d, want 18", len(raw))
		}
		if strings.ContainsAny(token, "+/=") {
			t.Errorf("token %q must be URL-safe and unpadded", token)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q", token)
		}
		seen[token] = true
	}
}

func TestTokenHashAndBodyHash(t *testing.T) {
	// 已知向量:SHA-256("") 的十六进制。
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := tokenHash(""); got != emptySHA256 {
		t.Errorf("tokenHash(\"\") = %q, want %q", got, emptySHA256)
	}
	if got := bodyHash([]byte{}); got != emptySHA256 {
		t.Errorf("bodyHash([]) = %q, want %q", got, emptySHA256)
	}
	// tokenHash 与 bodyHash 对同一输入必须一致(都是 hex(sha256))。
	if tokenHash("abc") != bodyHash([]byte("abc")) {
		t.Error("tokenHash and bodyHash must agree on the same input")
	}
	if got := tokenHash("abc"); len(got) != 64 || strings.ToLower(got) != got {
		t.Errorf("tokenHash must be 64 lowercase hex chars, got %q", got)
	}
	if tokenHash("abc") == tokenHash("abd") {
		t.Error("different inputs must hash differently")
	}
}
