package dynamic

// B3 轮换宽限语义测试：窗口内旧钥签名通过、过期后仅新钥有效、未轮换
// 智能体不受影响；当前密钥始终优先。

import (
	"testing"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/platform"
)

// TestVerifyWithGrace 锁定 B3 轮换宽限语义：窗口内旧钥签名通过并记
// 观察、过期后仅新钥有效、未轮换智能体不受影响。
func TestVerifyWithGrace(t *testing.T) {
	svc := &Service{}
	rotatedNow := time.Now().UTC().Format(time.RFC3339Nano)
	rotatedOld := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	canonical := "v1\nPOST\n/agent/grace\nt\nn\nr\nk\nbodyhash"

	cases := []struct {
		name     string
		resolved *agent.ResolvedConfig
		want     bool
	}{
		{"窗口内旧钥通过", &agent.ResolvedConfig{
			Slug: "grace", SigningSecret: "new-secret-0123456789",
			PrevSigningSecret: "old-secret-0123456789", SecretRotatedAt: rotatedNow,
		}, true},
		{"当前密钥始终优先", &agent.ResolvedConfig{
			Slug: "grace", SigningSecret: "new-secret-0123456789",
			PrevSigningSecret: "old-secret-0123456789", SecretRotatedAt: rotatedNow,
		}, true},
		{"过期旧钥拒绝", &agent.ResolvedConfig{
			Slug: "grace", SigningSecret: "new-secret-0123456789",
			PrevSigningSecret: "old-secret-0123456789", SecretRotatedAt: rotatedOld,
		}, false},
		{"未轮换不受影响", &agent.ResolvedConfig{
			Slug: "grace", SigningSecret: "new-secret-0123456789",
		}, false},
	}
	// 每个用例断言：新钥签名必过；旧钥签名按语义判定。
	for _, tc := range cases {
		newSig := platform.EncodeSignature(
			platform.SignBody([]byte(tc.resolved.SigningSecret), []byte(canonical)),
			platform.EncodingHex)
		if got := svc.verifyWithGrace(tc.resolved, canonical, newSig, platform.EncodingHex); !got {
			t.Errorf("%s: current-key signature must always verify", tc.name)
		}
		oldSig := ""
		if tc.resolved.PrevSigningSecret != "" {
			oldSig = platform.EncodeSignature(
				platform.SignBody([]byte(tc.resolved.PrevSigningSecret), []byte(canonical)),
				platform.EncodingHex)
		}
		if got := svc.verifyWithGrace(tc.resolved, canonical, oldSig, platform.EncodingHex); got != tc.want {
			t.Errorf("%s: prev-key verify = %v, want %v", tc.name, got, tc.want)
		}
	}
}
