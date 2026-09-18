package mota

// 对应 rs tests/scheduling_test.rs 里不依赖外部数据库的部分:执行器能力矩阵与
// AgentInput 校验。这些函数目前只在 platform_store_test.go(需要真实 PostgreSQL)
// 的 happy path 里被间接跑过,从未直接单测过拒绝分支 —— 这里补齐。

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ExecutorKind / ExecutionMode:解析与能力矩阵
// ---------------------------------------------------------------------------

// 对应 rs executor_kind_roundtrip_all_variants。
func TestParseExecutorKindRoundtripsAllVariants(t *testing.T) {
	for _, kind := range []ExecutorKind{
		ExecutorKindRust, ExecutorKindClaudeCode, ExecutorKindCodex,
		ExecutorKindReasonix, ExecutorKindPi, ExecutorKindOpenCode, ExecutorKindQwenCode,
		ExecutorKindGeminiCLI, ExecutorKindOpenHands, ExecutorKindDeepseekHarness,
		ExecutorKindGoose, ExecutorKindOpenAIAgent, ExecutorKindClaudeManaged,
	} {
		got, err := ParseExecutorKind(kind.String())
		if err != nil {
			t.Errorf("ParseExecutorKind(%q): %v", kind, err)
		}
		if got != kind {
			t.Errorf("ParseExecutorKind(%q) = %q", kind, got)
		}
	}
	if _, err := ParseExecutorKind("not-an-executor"); err == nil {
		t.Error("expected an error for an unknown executor kind")
	}
}

// 对应 rs execution_mode_capability_matrix_is_enforced。
func TestExecutionModeCapabilityMatrixIsEnforced(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionModeGateway, ExecutionModeLocal, ExecutionModeManaged} {
		got, err := ParseExecutionMode(mode.String())
		if err != nil || got != mode {
			t.Errorf("ParseExecutionMode(%q) = (%q, %v)", mode, got, err)
		}
	}

	for _, kind := range []ExecutorKind{
		ExecutorKindClaudeCode, ExecutorKindPi, ExecutorKindCodex, ExecutorKindReasonix,
	} {
		if !kind.SupportsLocalCLI() {
			t.Errorf("%s should support local CLI", kind)
		}
		if !kind.SupportsGateway() {
			t.Errorf("%s should support gateway", kind)
		}
	}
	if !ExecutorKindDeepseekHarness.SupportsLocalCLI() {
		t.Error("deepseek_harness should support local CLI")
	}
	if !ExecutorKindDeepseekHarness.SupportsGateway() {
		t.Error("deepseek_harness should support gateway")
	}
	for _, kind := range []ExecutorKind{ExecutorKindOpenCode, ExecutorKindQwenCode, ExecutorKindGeminiCLI, ExecutorKindOpenHands} {
		if !kind.SupportsLocalCLI() || kind.SupportsGateway() {
			t.Errorf("%s must be local-only", kind)
		}
	}
	for _, kind := range []ExecutorKind{ExecutorKindOpenAIAgent, ExecutorKindClaudeManaged} {
		if !kind.SupportsExecutionMode(ExecutionModeManaged) || kind.SupportsLocalCLI() || kind.SupportsGateway() {
			t.Errorf("%s must be managed-only", kind)
		}
	}
	if ExecutorKindRust.SupportsLocalCLI() || ExecutorKindRust.SupportsGateway() {
		t.Error("rust (embedded processor) supports neither local CLI nor gateway proper")
	}
	// 但 rust 通过 SupportsExecutionMode 的特判在两种执行方式下都可用
	// (它走进程内 Processor,不是真的本机 CLI 或网关直调)。
	if !ExecutorKindRust.SupportsExecutionMode(ExecutionModeGateway) {
		t.Error("rust should be usable under gateway execution mode (embedded processor)")
	}
	if !ExecutorKindRust.SupportsExecutionMode(ExecutionModeLocal) {
		t.Error("rust should be usable under local execution mode (embedded processor)")
	}
}

// AgentInput 默认执行方式是 gateway;Goose 声称 local 执行方式不受支持,必须拒绝。
// 对应 rs execution_mode_capability_matrix_is_enforced 的后半段。
func TestAgentInputDefaultsToGatewayAndRejectsUnsupportedCombination(t *testing.T) {
	if (&AgentInput{}).ExecutionMode != "" {
		t.Fatal("sanity: a zero-value AgentInput must not already carry an execution mode")
	}

	secret := "test-signing-secret-0123456789"
	invalid := &AgentInput{
		Slug: "invalid-local", Name: "invalid local",
		ExecutorKind: ExecutorKindGoose, ExecutionMode: ExecutionModeLocal,
		SigningSecret: &secret,
	}
	if err := invalid.Validate(true); err == nil {
		t.Fatal("goose does not support local execution and must be rejected")
	}
}

func TestAgentNameIsLimitedToThirtyCharacters(t *testing.T) {
	secret := "test-signing-secret-0123456789"
	valid := &AgentInput{Slug: "name-limit-agent", Name: strings.Repeat("a", 30), ExecutorKind: ExecutorKindRust, ExecutionMode: ExecutionModeLocal, SigningSecret: &secret}
	if err := valid.Validate(true); err != nil {
		t.Fatalf("30 character name: %v", err)
	}
	invalid := &AgentInput{Slug: "name-limit-agent", Name: strings.Repeat("a", 31), ExecutorKind: ExecutorKindRust, ExecutionMode: ExecutionModeLocal, SigningSecret: &secret}
	if err := invalid.Validate(true); err == nil {
		t.Fatal("31 character name must be rejected")
	}
}

// ---------------------------------------------------------------------------
// AgentInput.Validate:字段级拒绝分支
// ---------------------------------------------------------------------------

func validAgentInput() *AgentInput {
	secret := "a-valid-signing-secret-0123456789"
	return &AgentInput{
		Slug: "valid-agent", Name: "Valid Agent",
		ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeGateway,
		SigningSecret: &secret,
	}
}

func TestAgentInputValidateAcceptsMinimalValidInput(t *testing.T) {
	input := validAgentInput()
	if err := input.Validate(true); err != nil {
		t.Fatalf("expected valid input, got %v", err)
	}
	// applyDefaults 必须把可选字段填上文档化的默认值。
	if input.SignatureEncoding != "hex" {
		t.Errorf("signature_encoding default = %q, want hex", input.SignatureEncoding)
	}
	if input.WorkspacePath != "." {
		t.Errorf("workspace_path default = %q, want .", input.WorkspacePath)
	}
	if input.PermissionMode != "dontAsk" {
		t.Errorf("permission_mode default = %q, want dontAsk", input.PermissionMode)
	}
}

func TestAgentInputValidateRequiresSecretOnCreate(t *testing.T) {
	input := validAgentInput()
	input.SigningSecret = nil
	if err := input.Validate(true); err == nil {
		t.Error("creating an Agent without a signing_secret must fail")
	}
	// 更新场景允许留空(保留原密钥)。
	if err := input.Validate(false); err != nil {
		t.Errorf("updating without a signing_secret should be allowed, got %v", err)
	}
}

func TestAgentInputValidateRejectsShortOrOverlongSecret(t *testing.T) {
	for _, length := range []int{1, 15, 513, 1000} {
		input := validAgentInput()
		secret := strings.Repeat("s", length)
		input.SigningSecret = &secret
		if err := input.Validate(true); err == nil {
			t.Errorf("secret length %d should be rejected", length)
		}
	}
	for _, length := range []int{16, 32, 512} {
		input := validAgentInput()
		secret := strings.Repeat("s", length)
		input.SigningSecret = &secret
		if err := input.Validate(true); err != nil {
			t.Errorf("secret length %d should be accepted, got %v", length, err)
		}
	}
}

func TestAgentInputValidateRejectsBadSlug(t *testing.T) {
	for _, slug := range []string{
		"", "a", "-leading", "trailing-", "has--double", "Has_Underscore", "has space",
		strings.Repeat("a", 65),
	} {
		input := validAgentInput()
		input.Slug = slug
		if err := input.Validate(true); err == nil {
			t.Errorf("slug %q should be rejected", slug)
		}
	}
}

func TestAgentInputValidateNormalizesSlugCase(t *testing.T) {
	input := validAgentInput()
	input.Slug = "  Mixed-Case-Slug  "
	if err := input.Validate(true); err != nil {
		t.Fatalf("expected valid input, got %v", err)
	}
	if input.Slug != "mixed-case-slug" {
		t.Errorf("slug = %q, want mixed-case-slug", input.Slug)
	}
}

func TestAgentInputValidateRejectsOverlongName(t *testing.T) {
	input := validAgentInput()
	input.Name = strings.Repeat("n", 51)
	if err := input.Validate(true); err == nil {
		t.Error("name over 50 characters should be rejected")
	}
}

func TestAgentInputValidateRejectsOverlongDescription(t *testing.T) {
	input := validAgentInput()
	input.Description = strings.Repeat("d", 501)
	if err := input.Validate(true); err == nil {
		t.Error("description over 500 characters should be rejected")
	}
}

func TestAgentInputValidateRejectsUnknownSignatureEncoding(t *testing.T) {
	input := validAgentInput()
	input.SignatureEncoding = "rot13"
	if err := input.Validate(true); err == nil {
		t.Error("unknown signature_encoding should be rejected")
	}
}

func TestAgentInputValidateRejectsUnknownExecutorOrMode(t *testing.T) {
	input := validAgentInput()
	input.ExecutorKind = "not-a-real-executor"
	if err := input.Validate(true); err == nil {
		t.Error("unknown executor_kind should be rejected")
	}

	input = validAgentInput()
	input.ExecutionMode = "not-a-real-mode"
	if err := input.Validate(true); err == nil {
		t.Error("unknown execution_mode should be rejected")
	}
}

func TestAgentInputValidateRejectsBadWorkspacePath(t *testing.T) {
	// 空字符串不在拒绝列表里:applyDefaults 会先把它填成 "."(合法默认值),
	// 校验阶段根本看不到空字符串。
	for _, path := range []string{"/etc/passwd", "a/../b", "../escape", `C:\Windows`} {
		input := validAgentInput()
		input.WorkspacePath = path
		if err := input.Validate(true); err == nil {
			t.Errorf("workspace_path %q should be rejected", path)
		}
	}
}

func TestAgentInputValidateRejectsBadPermissionMode(t *testing.T) {
	input := validAgentInput()
	input.PermissionMode = "yolo"
	if err := input.Validate(true); err == nil {
		t.Error("unknown permission_mode should be rejected")
	}
	for _, mode := range []string{"dontAsk", "plan", "acceptEdits", "manual"} {
		input := validAgentInput()
		input.PermissionMode = mode
		if err := input.Validate(true); err != nil {
			t.Errorf("permission_mode %q should be accepted, got %v", mode, err)
		}
	}
}

func TestAgentInputValidateRejectsOutOfRangeNumerics(t *testing.T) {
	tooLowTimeout := uint64(4)
	input := validAgentInput()
	input.TimeoutSecs = &tooLowTimeout
	if err := input.Validate(true); err == nil {
		t.Error("timeout_secs below 5 should be rejected")
	}

	tooHighTimeout := uint64(3601)
	input = validAgentInput()
	input.TimeoutSecs = &tooHighTimeout
	if err := input.Validate(true); err == nil {
		t.Error("timeout_secs above 3600 should be rejected")
	}

	tooLowConcurrency := 0
	input = validAgentInput()
	input.MaxConcurrency = &tooLowConcurrency
	if err := input.Validate(true); err == nil {
		t.Error("max_concurrency below 1 should be rejected")
	}

	tooHighConcurrency := 33
	input = validAgentInput()
	input.MaxConcurrency = &tooHighConcurrency
	if err := input.Validate(true); err == nil {
		t.Error("max_concurrency above 32 should be rejected")
	}

	tooHighQueue := 1001
	input = validAgentInput()
	input.QueueCapacity = &tooHighQueue
	if err := input.Validate(true); err == nil {
		t.Error("queue_capacity above 1000 should be rejected")
	}
}

func TestAgentInputValidateRejectsTooManyOrInvalidAllowedTools(t *testing.T) {
	input := validAgentInput()
	tools := make([]string, 65)
	for i := range tools {
		tools[i] = "read"
	}
	input.AllowedTools = tools
	if err := input.Validate(true); err == nil {
		t.Error("more than 64 allowed_tools should be rejected")
	}

	input = validAgentInput()
	input.AllowedTools = []string{"read,write"}
	if err := input.Validate(true); err == nil {
		t.Error("a tool entry containing a comma should be rejected")
	}

	input = validAgentInput()
	input.AllowedTools = []string{strings.Repeat("t", 201)}
	if err := input.Validate(true); err == nil {
		t.Error("an overlong tool entry should be rejected")
	}
}

// applyDefaults 必须在校验之前跑,否则空字符串字段会被误判为非法值而不是「使用默认值」。
func TestAgentInputValidateAppliesDefaultsBeforeChecking(t *testing.T) {
	secret := "a-valid-signing-secret-0123456789"
	input := &AgentInput{Slug: "defaults-agent", Name: "Defaults Agent", SigningSecret: &secret}
	if err := input.Validate(true); err != nil {
		t.Fatalf("expected valid input relying on defaults, got %v", err)
	}
	if input.ExecutorKind != ExecutorKindPi {
		t.Errorf("executor_kind default = %q, want pi", input.ExecutorKind)
	}
	if input.ExecutionMode != ExecutionModeGateway {
		t.Errorf("execution_mode default = %q, want gateway", input.ExecutionMode)
	}
}
