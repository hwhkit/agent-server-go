package execengine

import (
	"strings"
	"testing"
)

func TestExecutorLogRoleNeverReturnsCustomPrompt(t *testing.T) {
	secretPrompt := "custom role; authorization=Bearer secret-upstream-token"
	got := executorLogRole(secretPrompt)
	if got != "custom" {
		t.Fatalf("custom role log value = %q, want custom", got)
	}
	if strings.Contains(got, "secret-upstream-token") {
		t.Fatal("custom SystemPrompt leaked into its log-safe role value")
	}
}

func TestExecutorLogRoleKeepsStablePresetIdentifier(t *testing.T) {
	if got := executorLogRole("technical_analyst"); got != "technical_analyst" {
		t.Fatalf("preset role log value = %q", got)
	}
	if got := executorLogRole(""); got != "fundamental_analyst" {
		t.Fatalf("default role log value = %q", got)
	}
}
