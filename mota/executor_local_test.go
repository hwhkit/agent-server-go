package mota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localTestConfig 返回一份带默认值的平台配置,供 argv 断言使用。
func localTestConfig(t *testing.T) *PlatformConfig {
	t.Helper()
	cfg := testPlatformConfig(t)
	return cfg
}

// localTestRuntime 构造一个最小 AgentRuntime。
func localTestRuntime(role string, allowedTools []string, claudeModel *string) *AgentRuntime {
	return newAgentRuntime(&AgentConfig{
		Slug: "local-agent", Name: "local", Enabled: true,
		ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeLocal,
		SystemPrompt: role, AllowedTools: allowedTools, ClaudeModel: claudeModel,
		PermissionMode: "dontAsk", TimeoutSecs: 600, MaxConcurrency: 1, QueueCapacity: 10,
	})
}

// writeSkill 建一个 <dir>/<name>/SKILL.md 并返回其路径。
func writeSkill(t *testing.T, dir, name string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte("# "+name), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// argsAfter 返回 flag 之后紧跟的那个参数。
func argsAfter(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// allArgsAfter 返回 flag 每次出现后紧跟的参数(用于可重复的 --skill)。
func allArgsAfter(args []string, flag string) []string {
	var values []string
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			values = append(values, args[i+1])
		}
	}
	return values
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// envValue 从 cmd.Env 里取变量值。
func envValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 模型别名
// ---------------------------------------------------------------------------

func TestLocalModelAliasFallback(t *testing.T) {
	cfg := localTestConfig(t)
	for _, alias := range []string{"", "deepseek-v4-flash", "dsk_4f"} {
		value := alias
		runtime := localTestRuntime("", nil, &value)
		if got := localModel(cfg, runtime); got != cfg.LocalAgents.DefaultModel {
			t.Errorf("alias %q => %q, want the local default %q", alias, got, cfg.LocalAgents.DefaultModel)
		}
	}
	// nil 也回退默认模型。
	if got := localModel(cfg, localTestRuntime("", nil, nil)); got != cfg.LocalAgents.DefaultModel {
		t.Errorf("nil claude_model => %q, want the local default", got)
	}
	// 真实模型名原样透传(并去掉首尾空白)。
	explicit := "  provider/custom-model  "
	if got := localModel(cfg, localTestRuntime("", nil, &explicit)); got != "provider/custom-model" {
		t.Errorf("explicit model => %q, want provider/custom-model", got)
	}
}

// ---------------------------------------------------------------------------
// Pi
// ---------------------------------------------------------------------------

// 无金融扩展时走 --no-tools,系统提示词即原始岗位契约。
func TestPiCommandWithoutDataExtensionUsesNoTools(t *testing.T) {
	cfg := localTestConfig(t)
	runtime := localTestRuntime("fundamental_analyst", nil, nil)
	local := agentCommand(cfg, runtime, t.TempDir(), "任务提示词", "岗位契约、不要调用工具", LocalCliPi)

	args := local.Cmd.Args[1:]
	for _, want := range []string{"--print", "--mode", "json", "--no-session", "--no-tools"} {
		if !hasArg(args, want) {
			t.Errorf("Pi argv missing %q: %v", want, args)
		}
	}
	if hasArg(args, "--extension") {
		t.Errorf("no finance extension configured, must not pass --extension: %v", args)
	}
	systemPrompt, ok := argsAfter(args, "--system-prompt")
	if !ok {
		t.Fatalf("missing --system-prompt: %v", args)
	}
	// 未启用取数时保留「不要调用工具」。
	if systemPrompt != "岗位契约、不要调用工具" {
		t.Errorf("system prompt = %q, want the contract verbatim", systemPrompt)
	}
	// 提示词是最后一个位置参数。
	if args[len(args)-1] != "任务提示词" {
		t.Errorf("last arg = %q, want the prompt", args[len(args)-1])
	}
	if local.StdinPayload != nil {
		t.Error("Pi passes the prompt positionally, stdin must stay empty")
	}
	if local.OutputMode != LocalOutputPiJSONLines {
		t.Errorf("output mode = %v, want PiJSONLines", local.OutputMode)
	}
	if local.Label != "Pi" {
		t.Errorf("label = %q, want Pi", local.Label)
	}
}

// 配了金融扩展的基本面岗位启用取数模式,并去掉「不要调用工具」。
func TestPiCommandFundamentalAnalystWithDataExtension(t *testing.T) {
	dir := t.TempDir()
	extension := filepath.Join(dir, "mota-finance-data.ts")
	if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := localTestConfig(t)
	cfg.LocalAgents.FinanceExtensionPath = extension

	runtime := localTestRuntime("fundamental_analyst", nil, nil)
	local := agentCommand(cfg, runtime, dir, "分析、不要调用工具", "契约、不要调用工具", LocalCliPi)
	args := local.Cmd.Args[1:]

	for _, want := range []string{"--no-builtin-tools", "--no-extensions", "--no-skills"} {
		if !hasArg(args, want) {
			t.Errorf("Pi argv missing %q: %v", want, args)
		}
	}
	if got, _ := argsAfter(args, "--extension"); got != extension {
		t.Errorf("--extension = %q, want %q", got, extension)
	}
	// 无 analysis skill 时只给数据工具,不给 read。
	if got, _ := argsAfter(args, "--tools"); got != "mota_finance_snapshot" {
		t.Errorf("--tools = %q, want mota_finance_snapshot", got)
	}
	systemPrompt, _ := argsAfter(args, "--system-prompt")
	if strings.Contains(systemPrompt, noToolClause) {
		t.Errorf("data mode must strip %q from the contract: %q", noToolClause, systemPrompt)
	}
	if !strings.Contains(systemPrompt, "mota_finance_snapshot") {
		t.Errorf("system prompt should carry the data requirement: %q", systemPrompt)
	}
	// 位置提示词同样要去掉该措辞。
	if last := args[len(args)-1]; strings.Contains(last, noToolClause) {
		t.Errorf("prompt must strip %q: %q", noToolClause, last)
	}
}

// 技术面岗位用 mota_market_history;声明 skill: 后加上 read 并挂载该 Skill。
func TestPiCommandTechnicalAnalystSkillSelection(t *testing.T) {
	dir := t.TempDir()
	extension := filepath.Join(dir, "ext.ts")
	if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisDir := filepath.Join(dir, "analysis")
	wanted := writeSkill(t, analysisDir, "technical-factor-model")
	other := writeSkill(t, analysisDir, "value-gap-catalyst")

	cfg := localTestConfig(t)
	cfg.LocalAgents.FinanceExtensionPath = extension
	cfg.LocalAgents.AnalysisSkillPaths = []string{wanted, other}

	runtime := localTestRuntime("technical_analyst", []string{"skill:technical-factor-model"}, nil)
	args := agentCommand(cfg, runtime, dir, "p", "c", LocalCliPi).Cmd.Args[1:]

	if got, _ := argsAfter(args, "--tools"); got != "read,mota_market_history" {
		t.Errorf("--tools = %q, want read,mota_market_history", got)
	}
	skills := allArgsAfter(args, "--skill")
	if len(skills) != 1 || skills[0] != wanted {
		t.Errorf("--skill = %v, want only the requested skill %q", skills, wanted)
	}
	systemPrompt, _ := argsAfter(args, "--system-prompt")
	if !strings.Contains(systemPrompt, "`technical-factor-model` Skill") {
		t.Errorf("system prompt should name the selected skill: %q", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "mota_market_history") {
		t.Errorf("system prompt should carry the market-history requirement: %q", systemPrompt)
	}
}

// 未声明 skill: 时分析/流程 Skill 一个都不启用。
func TestPiCommandAnalysisSkillsRequireExplicitRequest(t *testing.T) {
	dir := t.TempDir()
	extension := filepath.Join(dir, "ext.ts")
	if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := localTestConfig(t)
	cfg.LocalAgents.FinanceExtensionPath = extension
	cfg.LocalAgents.AnalysisSkillPaths = []string{writeSkill(t, filepath.Join(dir, "a"), "some-skill")}

	runtime := localTestRuntime("technical_analyst", nil, nil)
	args := agentCommand(cfg, runtime, dir, "p", "c", LocalCliPi).Cmd.Args[1:]

	if skills := allArgsAfter(args, "--skill"); len(skills) != 0 {
		t.Errorf("--skill = %v, want none without an explicit skill: request", skills)
	}
	if got, _ := argsAfter(args, "--tools"); got != "mota_market_history" {
		t.Errorf("--tools = %q, want mota_market_history (no read without a skill)", got)
	}
}

// 策略岗位:未声明 skill: 时全部策略 Skill 可用,并要求比较至少两种。
func TestPiCommandStrategistUsesAllStrategySkillsByDefault(t *testing.T) {
	dir := t.TempDir()
	extension := filepath.Join(dir, "ext.ts")
	if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
		t.Fatal(err)
	}
	strategyDir := filepath.Join(dir, "strategies")
	first := writeSkill(t, strategyDir, "momentum-breakout")
	second := writeSkill(t, strategyDir, "mean-reversion")

	cfg := localTestConfig(t)
	cfg.LocalAgents.FinanceExtensionPath = extension
	cfg.LocalAgents.StrategySkillPaths = []string{first, second}

	runtime := localTestRuntime("strategist", nil, nil)
	args := agentCommand(cfg, runtime, dir, "p", "c", LocalCliPi).Cmd.Args[1:]

	if got, _ := argsAfter(args, "--tools"); got != "read,mota_market_history,mota_finance_snapshot" {
		t.Errorf("--tools = %q", got)
	}
	skills := allArgsAfter(args, "--skill")
	if len(skills) != 2 {
		t.Errorf("--skill = %v, want both strategy skills", skills)
	}
	systemPrompt, _ := argsAfter(args, "--system-prompt")
	if !strings.Contains(systemPrompt, "必须比较至少两种适用策略 Skill") {
		t.Errorf("system prompt should demand comparing skills: %q", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "momentum-breakout、mean-reversion") {
		t.Errorf("system prompt should list the configured skills: %q", systemPrompt)
	}
}

// 策略岗位只声明一个 Skill 时改为「固定使用」。
func TestPiCommandStrategistSingleSkillIsFixed(t *testing.T) {
	dir := t.TempDir()
	extension := filepath.Join(dir, "ext.ts")
	if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
		t.Fatal(err)
	}
	only := writeSkill(t, filepath.Join(dir, "strategies"), "momentum-breakout")

	cfg := localTestConfig(t)
	cfg.LocalAgents.FinanceExtensionPath = extension
	cfg.LocalAgents.StrategySkillPaths = []string{only}

	runtime := localTestRuntime("strategist", []string{"skill:momentum-breakout"}, nil)
	systemPrompt, _ := argsAfter(agentCommand(cfg, runtime, dir, "p", "c", LocalCliPi).Cmd.Args[1:], "--system-prompt")
	if !strings.Contains(systemPrompt, "本 Agent 固定使用策略 Skill `momentum-breakout`") {
		t.Errorf("system prompt = %q", systemPrompt)
	}
}

// 组合/风控/执行三个流程岗位:有 workflow Skill 时工具为 read,且提示词带默认 Skill 名。
func TestPiCommandWorkflowRoles(t *testing.T) {
	for _, tc := range []struct {
		role            string
		fallbackName    string
		requirementText string
	}{
		{"portfolio_manager", "portfolio-workflow", "组合 Skill 要求"},
		{"risk_manager", "risk-workflow", "风控 Skill 要求"},
		{"execution_trader", "execution-workflow", "执行 Skill 要求"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			dir := t.TempDir()
			extension := filepath.Join(dir, "ext.ts")
			if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
				t.Fatal(err)
			}
			skill := writeSkill(t, filepath.Join(dir, "workflow"), "custom-flow")
			cfg := localTestConfig(t)
			cfg.LocalAgents.FinanceExtensionPath = extension
			cfg.LocalAgents.WorkflowSkillPaths = []string{skill}

			runtime := localTestRuntime(tc.role, []string{"skill:custom-flow"}, nil)
			args := agentCommand(cfg, runtime, dir, "p", "c", LocalCliPi).Cmd.Args[1:]

			if got, _ := argsAfter(args, "--tools"); got != "read" {
				t.Errorf("--tools = %q, want read", got)
			}
			systemPrompt, _ := argsAfter(args, "--system-prompt")
			if !strings.Contains(systemPrompt, tc.requirementText) {
				t.Errorf("system prompt should carry %q: %q", tc.requirementText, systemPrompt)
			}
			if !strings.Contains(systemPrompt, "`custom-flow` Skill") {
				t.Errorf("system prompt should name the selected skill: %q", systemPrompt)
			}
		})
	}
}

// 流程岗位没有 workflow Skill 时不启用取数模式。
func TestPiCommandWorkflowRoleWithoutSkillHasNoDataTool(t *testing.T) {
	dir := t.TempDir()
	extension := filepath.Join(dir, "ext.ts")
	if err := os.WriteFile(extension, []byte("// ext"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := localTestConfig(t)
	cfg.LocalAgents.FinanceExtensionPath = extension

	runtime := localTestRuntime("risk_manager", nil, nil)
	args := agentCommand(cfg, runtime, dir, "p", "c", LocalCliPi).Cmd.Args[1:]
	if !hasArg(args, "--no-tools") {
		t.Errorf("expected --no-tools without a workflow skill: %v", args)
	}
}

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

func TestCodexCommandBaseArgs(t *testing.T) {
	cfg := localTestConfig(t)
	cfg.Gateway.BaseURL = ""
	local := agentCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "提示词", "契约", LocalCliCodex)
	args := local.Cmd.Args[1:]

	want := []string{
		"exec", "--json", "--ignore-user-config",
		"--disable", "multi_agent",
		"--disable", "multi_agent_v2",
		"--skip-git-repo-check", "--ephemeral",
		"--sandbox", "read-only",
	}
	for i, value := range want {
		if i >= len(args) || args[i] != value {
			t.Fatalf("Codex argv prefix mismatch at %d: got %v, want prefix %v", i, args, want)
		}
	}
	// 末尾必须是 `-`,表示从 stdin 读提示词。
	if args[len(args)-1] != "-" {
		t.Errorf("last arg = %q, want -", args[len(args)-1])
	}
	if string(local.StdinPayload) != "提示词" {
		t.Errorf("stdin payload = %q, want the prompt", local.StdinPayload)
	}
	if local.OutputMode != LocalOutputCodexJSONLines {
		t.Errorf("output mode = %v, want CodexJSONLines", local.OutputMode)
	}
	// 未配网关时不注入 provider 配置。
	if hasArg(args, "--config") {
		t.Errorf("no gateway configured, must not pass --config: %v", args)
	}
}

// 配了网关时注入 litellm provider,并补齐 /v1 后缀。
func TestCodexCommandInjectsLiteLLMProvider(t *testing.T) {
	for _, tc := range []struct {
		baseURL  string
		wantBase string
	}{
		{"https://litellm.internal", "https://litellm.internal/v1"},
		{"https://litellm.internal/", "https://litellm.internal/v1"},
		{"https://litellm.internal/v1", "https://litellm.internal/v1"},
		{"https://litellm.internal/v1/", "https://litellm.internal/v1"},
	} {
		t.Run(tc.baseURL, func(t *testing.T) {
			cfg := localTestConfig(t)
			cfg.Gateway.BaseURL = tc.baseURL
			args := agentCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c", LocalCliCodex).Cmd.Args[1:]

			if !hasArg(args, `model_provider="litellm"`) {
				t.Errorf("missing model_provider config: %v", args)
			}
			wantProvider := `model_providers.litellm={name="LiteLLM",base_url="` + tc.wantBase +
				`",env_key="LITELLM_API_KEY",wire_api="responses"}`
			if !hasArg(args, wantProvider) {
				t.Errorf("provider config mismatch.\n got: %v\nwant: %s", args, wantProvider)
			}
		})
	}
}

// Codex 的 --model 必须去掉 litellm/ 前缀(provider 由 --config 指定)。
func TestCodexCommandStripsLiteLLMModelPrefix(t *testing.T) {
	cfg := localTestConfig(t)
	if !strings.HasPrefix(cfg.LocalAgents.DefaultModel, "litellm/") {
		t.Fatalf("precondition: default model should carry the litellm/ prefix, got %q",
			cfg.LocalAgents.DefaultModel)
	}
	args := agentCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c", LocalCliCodex).Cmd.Args[1:]
	model, ok := argsAfter(args, "--model")
	if !ok {
		t.Fatalf("missing --model: %v", args)
	}
	if strings.HasPrefix(model, "litellm/") {
		t.Errorf("--model = %q, want the bare model name", model)
	}
	if model != strings.TrimPrefix(cfg.LocalAgents.DefaultModel, "litellm/") {
		t.Errorf("--model = %q, want %q", model, strings.TrimPrefix(cfg.LocalAgents.DefaultModel, "litellm/"))
	}
}

// ---------------------------------------------------------------------------
// Reasonix
// ---------------------------------------------------------------------------

func TestReasonixCommandArgsAndMetricsPath(t *testing.T) {
	cfg := localTestConfig(t)
	local := agentCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "提示词", "契约", LocalCliReasonix)
	args := local.Cmd.Args[1:]

	if args[0] != "run" || !hasArg(args, "--print") {
		t.Errorf("Reasonix argv = %v, want it to start with run --print", args)
	}
	metrics, ok := argsAfter(args, "--metrics")
	if !ok {
		t.Fatalf("missing --metrics: %v", args)
	}
	if metrics != local.MetricsPath {
		t.Errorf("--metrics = %q, want the reported metrics path %q", metrics, local.MetricsPath)
	}
	if !strings.Contains(filepath.Base(metrics), "mota-reasonix-metrics-") {
		t.Errorf("metrics file name = %q, want the mota-reasonix-metrics- prefix", filepath.Base(metrics))
	}
	if !strings.HasSuffix(metrics, ".json") {
		t.Errorf("metrics path = %q, want a .json suffix", metrics)
	}
	// 提示词是最后一个位置参数,不走 stdin。
	if args[len(args)-1] != "提示词" {
		t.Errorf("last arg = %q, want the prompt", args[len(args)-1])
	}
	if local.StdinPayload != nil {
		t.Error("Reasonix passes the prompt positionally, stdin must stay empty")
	}
	// usage 只能来自 metrics 文件,所以输出按纯文本解析。
	if local.OutputMode != LocalOutputText {
		t.Errorf("output mode = %v, want Text", local.OutputMode)
	}
	// run 子命令不支持 --system。
	if hasArg(args, "--system") {
		t.Errorf("Reasonix run does not support --system: %v", args)
	}
}

// 每次调用的 metrics 路径都不同,避免并发互相覆盖。
func TestReasonixMetricsPathIsUnique(t *testing.T) {
	cfg := localTestConfig(t)
	runtime := localTestRuntime("", nil, nil)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		path := agentCommand(cfg, runtime, t.TempDir(), "p", "c", LocalCliReasonix).MetricsPath
		if seen[path] {
			t.Fatalf("duplicate metrics path %q", path)
		}
		seen[path] = true
	}
}

// ---------------------------------------------------------------------------
// 工作目录与环境白名单
// ---------------------------------------------------------------------------

func TestLocalCommandSetsWorkspaceAndClearsEnv(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("MOTA_FINANCE_USER_AGENT", "mota-test/1.0")
	t.Setenv("MOTA_SECRET_NOT_ALLOWED", "must-not-leak")

	workspace := t.TempDir()
	cfg := localTestConfig(t)
	for _, kind := range []LocalCliKind{LocalCliPi, LocalCliCodex, LocalCliReasonix} {
		t.Run(kind.Label(), func(t *testing.T) {
			local := agentCommand(cfg, localTestRuntime("", nil, nil), workspace, "p", "c", kind)
			if local.Cmd.Dir != workspace {
				t.Errorf("Dir = %q, want %q", local.Cmd.Dir, workspace)
			}
			if value, ok := envValue(local.Cmd.Env, "HOME"); !ok || value != "/home/tester" {
				t.Errorf("HOME = %q (present=%t), want it forwarded", value, ok)
			}
			if value, ok := envValue(local.Cmd.Env, "MOTA_FINANCE_USER_AGENT"); !ok || value != "mota-test/1.0" {
				t.Errorf("MOTA_FINANCE_USER_AGENT = %q (present=%t), want it forwarded", value, ok)
			}
			if _, ok := envValue(local.Cmd.Env, "MOTA_SECRET_NOT_ALLOWED"); ok {
				t.Error("a variable outside the whitelist must not reach the child")
			}
		})
	}
}

// 白名单里但父进程未设置的变量不会凭空出现。
func TestLocalCommandSkipsUnsetWhitelistedEnv(t *testing.T) {
	cfg := localTestConfig(t)
	cfg.LocalAgents.AllowedEnv = []string{"MOTA_DEFINITELY_UNSET_VAR"}
	os.Unsetenv("MOTA_DEFINITELY_UNSET_VAR")

	local := agentCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c", LocalCliPi)
	if _, ok := envValue(local.Cmd.Env, "MOTA_DEFINITELY_UNSET_VAR"); ok {
		t.Error("an unset whitelisted variable must not be injected")
	}
	if len(local.Cmd.Env) != 0 {
		t.Errorf("Env = %v, want empty", local.Cmd.Env)
	}
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

func TestClaudeCommandDisabledIsExecutorUnavailable(t *testing.T) {
	cfg := localTestConfig(t)
	cfg.ClaudeEnabled = false
	_, err := claudeCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c")
	execErr := asExecutionError(t, err)
	if execErr.Code != "executor_unavailable" {
		t.Fatalf("code = %q, want executor_unavailable", execErr.Code)
	}
	if execErr.Detail != "Claude Code executor is disabled" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

func TestClaudeCommandArgs(t *testing.T) {
	cfg := localTestConfig(t)
	cfg.ClaudeEnabled = true
	runtime := localTestRuntime("", []string{"read", "grep"}, nil)
	runtime.Config.PermissionMode = "plan"

	workspace := t.TempDir()
	local, err := claudeCommand(cfg, runtime, workspace, "提示词", "岗位契约")
	if err != nil {
		t.Fatalf("claudeCommand: %v", err)
	}
	args := local.Cmd.Args[1:]

	for _, want := range []string{"--print", "--output-format", "json", "--no-session-persistence", "--bare"} {
		if !hasArg(args, want) {
			t.Errorf("Claude argv missing %q: %v", want, args)
		}
	}
	if got, _ := argsAfter(args, "--permission-mode"); got != "plan" {
		t.Errorf("--permission-mode = %q, want plan", got)
	}
	if got, _ := argsAfter(args, "--system-prompt"); got != "岗位契约" {
		t.Errorf("--system-prompt = %q, want the contract", got)
	}
	// Claude 用单参数 --tools=a,b(与 Pi 的 --tools a 不同)。
	if !hasArg(args, "--tools=read,grep") {
		t.Errorf("expected a single --tools=read,grep argument: %v", args)
	}
	if local.Cmd.Dir != workspace {
		t.Errorf("Dir = %q, want %q", local.Cmd.Dir, workspace)
	}
	if string(local.StdinPayload) != "提示词" {
		t.Errorf("stdin payload = %q, want the prompt", local.StdinPayload)
	}
	if local.Label != "Claude Code" {
		t.Errorf("label = %q, want 'Claude Code'", local.Label)
	}
	if local.OutputMode != LocalOutputText {
		t.Errorf("output mode = %v, want Text", local.OutputMode)
	}
	if local.MetricsPath != "" {
		t.Errorf("Claude has no metrics side channel, got %q", local.MetricsPath)
	}
}

// Claude 的别名集与本机 CLI 不同:litellm/dsk_4f 也回退默认模型。
func TestClaudeCommandModelAliases(t *testing.T) {
	cfg := localTestConfig(t)
	cfg.ClaudeEnabled = true
	cfg.ClaudeDefaultModel = "dsk_4f"

	for _, alias := range []string{"", "deepseek-v4-flash", "litellm/dsk_4f"} {
		value := alias
		runtime := localTestRuntime("", nil, &value)
		local, err := claudeCommand(cfg, runtime, t.TempDir(), "p", "c")
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := argsAfter(local.Cmd.Args[1:], "--model"); got != "dsk_4f" {
			t.Errorf("alias %q => --model %q, want dsk_4f", alias, got)
		}
	}

	// 真实模型名原样透传。
	explicit := "claude-sonnet-4"
	local, err := claudeCommand(cfg, localTestRuntime("", nil, &explicit), t.TempDir(), "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := argsAfter(local.Cmd.Args[1:], "--model"); got != "claude-sonnet-4" {
		t.Errorf("--model = %q, want claude-sonnet-4", got)
	}

	// claude_model 为 nil 时用默认模型。
	local, err = claudeCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := argsAfter(local.Cmd.Args[1:], "--model"); got != "dsk_4f" {
		t.Errorf("nil claude_model => --model %q, want dsk_4f", got)
	}

	// 默认模型为空且未指定时不传 --model,让 Claude 用内置默认。
	cfg.ClaudeDefaultModel = ""
	local, err = claudeCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if hasArg(local.Cmd.Args[1:], "--model") {
		t.Errorf("no model configured, must not pass --model: %v", local.Cmd.Args[1:])
	}
}

// --bare 不读 settings.json,因此要显式透传 ANTHROPIC_BASE_URL / AUTH_TOKEN。
func TestClaudeCommandForwardsAnthropicEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://litellm.internal")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-auth-token")
	os.Unsetenv("ANTHROPIC_API_KEY")

	cfg := localTestConfig(t)
	cfg.ClaudeEnabled = true
	local, err := claudeCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := envValue(local.Cmd.Env, "ANTHROPIC_BASE_URL"); !ok || value != "https://litellm.internal" {
		t.Errorf("ANTHROPIC_BASE_URL = %q (present=%t)", value, ok)
	}
	if value, ok := envValue(local.Cmd.Env, "ANTHROPIC_AUTH_TOKEN"); !ok || value != "sk-auth-token" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q (present=%t)", value, ok)
	}
	// 现网常只配 AUTH_TOKEN;此时要补出 API_KEY。
	if value, ok := envValue(local.Cmd.Env, "ANTHROPIC_API_KEY"); !ok || value != "sk-auth-token" {
		t.Errorf("ANTHROPIC_API_KEY = %q (present=%t), want it derived from the auth token", value, ok)
	}
}

// 父进程已有 ANTHROPIC_API_KEY 时不被 AUTH_TOKEN 覆盖。
func TestClaudeCommandKeepsExistingAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-real-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-auth-token")

	cfg := localTestConfig(t)
	cfg.ClaudeEnabled = true
	local, err := claudeCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := envValue(local.Cmd.Env, "ANTHROPIC_API_KEY"); value != "sk-real-key" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the pre-existing key", value)
	}
}

// 环境白名单同样约束 Claude。
func TestClaudeCommandRespectsEnvWhitelist(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("MOTA_SECRET_NOT_ALLOWED", "must-not-leak")

	cfg := localTestConfig(t)
	cfg.ClaudeEnabled = true
	local, err := claudeCommand(cfg, localTestRuntime("", nil, nil), t.TempDir(), "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := envValue(local.Cmd.Env, "AWS_REGION"); !ok || value != "us-east-1" {
		t.Errorf("AWS_REGION = %q (present=%t), want it forwarded", value, ok)
	}
	if _, ok := envValue(local.Cmd.Env, "MOTA_SECRET_NOT_ALLOWED"); ok {
		t.Error("a variable outside the whitelist must not reach Claude")
	}
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func TestSkillDirName(t *testing.T) {
	if got := skillDirName("/opt/skills/pi/strategies/momentum-breakout/SKILL.md"); got != "momentum-breakout" {
		t.Errorf("skillDirName = %q, want momentum-breakout", got)
	}
}

func TestRequestedSkills(t *testing.T) {
	got := requestedSkills([]string{"read", "skill:alpha", "grep", "skill:beta", "skillnoprefix"})
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("requestedSkills = %v, want [alpha beta]", got)
	}
	if len(requestedSkills(nil)) != 0 {
		t.Error("nil allowed_tools should yield no skills")
	}
}

func TestFilterSkillsByName(t *testing.T) {
	paths := []string{"/a/alpha/SKILL.md", "/a/beta/SKILL.md", "/a/gamma/SKILL.md"}
	got := filterSkillsByName(paths, []string{"beta", "gamma"})
	if len(got) != 2 || got[0] != "/a/beta/SKILL.md" || got[1] != "/a/gamma/SKILL.md" {
		t.Errorf("filterSkillsByName = %v", got)
	}
	if len(filterSkillsByName(paths, nil)) != 0 {
		t.Error("no requested names should select nothing")
	}
}
