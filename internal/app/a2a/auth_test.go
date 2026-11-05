package a2a

// 对应 rs src/a2a/auth.rs 的单测。

import (
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"strings"
	"testing"
)

func a2aTestAgent(slug, secret string) *agent.ResolvedConfig {
	return &agent.ResolvedConfig{
		ID: 1, Slug: slug, Name: "Desk", Enabled: true,
		ExecutorKind: exec.KindPi, ExecutionMode: exec.ModeLocal,
		SigningSecret: secret, SignatureEncoding: "hex",
		SystemPrompt: "strategist", WorkspacePath: ".",
		PermissionMode: "dontAsk", TimeoutSecs: 600,
		MaxConcurrency: 1, QueueCapacity: 10, Revision: 1,
	}
}

func TestA2ADerivedTokenIsNotTheSigningSecret(t *testing.T) {
	secret := "0123456789abcdef"
	token := A2ADeriveToken(secret, "desk")
	if token[:4] != "a2a_" {
		t.Fatalf("token should start with a2a_, got %q", token)
	}
	if strings.Contains(token, secret) {
		t.Fatal("derived token must not contain the signing secret")
	}
	// HMAC-SHA256 的 hex 是 64 字符。
	if want := len("a2a_") + 64; len(token) != want {
		t.Errorf("token length = %d, want %d", len(token), want)
	}
}

func TestA2ATokenIsBoundToSlugAndSecret(t *testing.T) {
	if A2ADeriveToken("0123456789abcdef", "desk-a") == A2ADeriveToken("0123456789abcdef", "desk-b") {
		t.Error("different slugs must derive different tokens")
	}
	if A2ADeriveToken("0123456789abcdef", "desk") == A2ADeriveToken("fedcba9876543210", "desk") {
		t.Error("different secrets must derive different tokens")
	}
}

func TestA2ACorrectTokenIsAcceptedRegardlessOfSchemeCase(t *testing.T) {
	agent := a2aTestAgent("desk", "0123456789abcdef")
	token := A2ADeriveToken(agent.SigningSecret, agent.Slug)
	for _, header := range []string{
		"Bearer " + token,
		"bearer " + token,
		"BEARER  " + token + "  ",
	} {
		if got := A2AVerifyAuth(agent, header); got != A2AAuthOK {
			t.Errorf("header %q: got %v, want OK", header, got)
		}
	}
}

func TestA2AMissingAndWrongSchemeAreReportedAsMissing(t *testing.T) {
	agent := a2aTestAgent("desk", "0123456789abcdef")
	for _, header := range []string{"", "Basic abc", "Bearer", "Bearer   "} {
		if got := A2AVerifyAuth(agent, header); got != A2AAuthMissing {
			t.Errorf("header %q: got %v, want Missing", header, got)
		}
	}
}

func TestA2AWrongTokenIsInvalid(t *testing.T) {
	agent := a2aTestAgent("desk", "0123456789abcdef")
	if got := A2AVerifyAuth(agent, "Bearer a2a_deadbeef"); got != A2AAuthInvalid {
		t.Errorf("got %v, want Invalid", got)
	}
	// 另一个 Agent 的 token 不能横向使用。
	other := A2ADeriveToken("0123456789abcdef", "other-desk")
	if got := A2AVerifyAuth(agent, "Bearer "+other); got != A2AAuthInvalid {
		t.Errorf("got %v, want Invalid", got)
	}
}
