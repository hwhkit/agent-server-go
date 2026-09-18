package mota

// 本机 Agent CLI 命令协议。子进程生命周期由 executor_process.go 统一管理。
// 对应 Rust 侧 src/platform/executor/local.rs。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// LocalCliKind 是本机可调用的 agent CLI 种类。
type LocalCliKind int

const (
	LocalCliPi LocalCliKind = iota
	LocalCliCodex
	LocalCliReasonix
	LocalCliOpenCode
	LocalCliQwenCode
	LocalCliGeminiCLI
	LocalCliOpenHands
	LocalCliDeepseekHarness
)

// Label 是日志与错误信息里使用的执行器名。
func (k LocalCliKind) Label() string {
	switch k {
	case LocalCliPi:
		return "Pi"
	case LocalCliCodex:
		return "Codex"
	case LocalCliReasonix:
		return "Reasonix"
	case LocalCliOpenCode:
		return "OpenCode"
	case LocalCliQwenCode:
		return "Qwen Code"
	case LocalCliGeminiCLI:
		return "Gemini CLI"
	case LocalCliOpenHands:
		return "OpenHands"
	case LocalCliDeepseekHarness:
		return "DeepSeek Harness"
	default:
		return "unknown"
	}
}

// binary 返回该 CLI 的可执行文件路径。
func (k LocalCliKind) binary(config *PlatformConfig) string {
	switch k {
	case LocalCliPi:
		return config.LocalAgents.PiBinary
	case LocalCliCodex:
		return config.LocalAgents.CodexBinary
	case LocalCliReasonix:
		return config.LocalAgents.ReasonixBinary
	case LocalCliOpenCode:
		return config.LocalAgents.OpenCodeBinary
	case LocalCliQwenCode:
		return config.LocalAgents.QwenCodeBinary
	case LocalCliGeminiCLI:
		return config.LocalAgents.GeminiCLIBinary
	case LocalCliOpenHands:
		return config.LocalAgents.OpenHandsBinary
	case LocalCliDeepseekHarness:
		return config.LocalAgents.DeepseekHarnessBinary
	default:
		return ""
	}
}

// LocalOutputMode 决定如何解析该 CLI 的 stdout。
type LocalOutputMode int

const (
	// LocalOutputText 单份文本/JSON 结果。
	LocalOutputText LocalOutputMode = iota
	// LocalOutputPiJSONLines Pi 的 JSONL 事件流。
	LocalOutputPiJSONLines
	// LocalOutputCodexJSONLines Codex 的 JSONL 事件流。
	LocalOutputCodexJSONLines
	LocalOutputGeminiJSON
	LocalOutputOpenHandsJSONLines
)

// LocalCommand 是一条待执行的本机 CLI 调用。
type LocalCommand struct {
	Cmd          *exec.Cmd
	StdinPayload []byte
	Label        string
	OutputMode   LocalOutputMode
	// MetricsPath 非空时,执行结束后需读取并删除该文件以获取 usage(仅 Reasonix)。
	MetricsPath  string
	CleanupPaths []string
}

// localModelAliases 是管理页遗留的网关模型别名;命中则回退到本机默认模型。
var localModelAliases = map[string]bool{"": true, "deepseek-v4-flash": true, "dsk_4f": true}

// localModel 解析本机 CLI 使用的模型标识。
//
// 管理页历史上填的是网关别名(dsk_4f 等),而本机 Pi/Codex/Reasonix 需要
// provider/model 形式,因此这些别名统一回退到 [local_agents].default_model。
func localModel(config *PlatformConfig, runtime *AgentRuntime, kinds ...LocalCliKind) string {
	kind := LocalCliPi
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	fallback := config.LocalAgents.DefaultModel
	switch kind {
	case LocalCliOpenCode:
		fallback = config.LocalAgents.OpenCodeDefaultModel
	case LocalCliQwenCode:
		fallback = config.LocalAgents.QwenCodeDefaultModel
	case LocalCliGeminiCLI:
		fallback = config.LocalAgents.GeminiCLIDefaultModel
	case LocalCliOpenHands:
		fallback = config.LocalAgents.OpenHandsDefaultModel
	case LocalCliDeepseekHarness:
		fallback = config.Gateway.Model
	}
	if runtime.Config.ClaudeModel == nil {
		return fallback
	}
	configured := strings.TrimSpace(*runtime.Config.ClaudeModel)
	if kind == LocalCliDeepseekHarness && configured == "deepseek-v4-flash" {
		return configured
	}
	if localModelAliases[configured] {
		return fallback
	}
	return configured
}

// skillDirName 取 SKILL.md 所在目录名,用作该 Skill 的标识。
// 例如 /opt/skills/pi/strategies/momentum-breakout/SKILL.md → momentum-breakout。
func skillDirName(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// requestedSkills 从 allowed_tools 里取出 `skill:` 前缀声明的 Skill 名。
func requestedSkills(allowedTools []string) []string {
	var names []string
	for _, tool := range allowedTools {
		if name, ok := strings.CutPrefix(tool, "skill:"); ok {
			names = append(names, name)
		}
	}
	return names
}

// filterSkillsByName 保留目录名出现在 requested 里的 Skill 路径。
func filterSkillsByName(paths, requested []string) []string {
	var selected []string
	for _, path := range paths {
		for _, name := range requested {
			if skillDirName(path) == name {
				selected = append(selected, path)
				break
			}
		}
	}
	return selected
}

// copyAllowedEnv 只把白名单里且父进程确实存在的环境变量传给子进程。
func copyAllowedEnv(cmd *exec.Cmd, allowed []string) {
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
}

// setEnvVar 覆盖式设置子进程环境变量(先移除同名项)。
func setEnvVar(cmd *exec.Cmd, name, value string) {
	prefix := name + "="
	filtered := cmd.Env[:0]
	for _, entry := range cmd.Env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	cmd.Env = append(filtered, prefix+value)
}

// hasEnvVar 判断子进程环境里是否已有该变量。
func hasEnvVar(cmd *exec.Cmd, name string) bool {
	prefix := name + "="
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// noToolClause 是岗位契约里「不要调用工具」的措辞。启用数据扩展时必须去掉,
// 否则会与「必须调用工具取数」的数据要求自相矛盾。
const noToolClause = "、不要调用工具"

// piSkillSelection 是 Pi 一次调用可用的 Skill 与数据工具。
type piSkillSelection struct {
	strategySkills []string
	analysisSkills []string
	workflowSkills []string
	// dataTool 是 --tools 的取值;为空表示该岗位不需要数据工具。
	dataTool string
}

// selectPiSkills 按 Agent 声明的 `skill:` 工具与岗位角色挑选 Skill 与数据工具。
func selectPiSkills(config *PlatformConfig, runtime *AgentRuntime, role string) piSkillSelection {
	requested := requestedSkills(runtime.Config.AllowedTools)

	// 策略 Skill:未声明 skill: 时全部可用;声明了则只用命中的。
	var strategySkills []string
	if len(requested) == 0 {
		strategySkills = config.LocalAgents.StrategySkillPaths
	} else {
		strategySkills = filterSkillsByName(config.LocalAgents.StrategySkillPaths, requested)
	}
	// 分析/流程 Skill 必须由 Agent 显式声明才启用。
	var analysisSkills, workflowSkills []string
	if len(requested) > 0 {
		analysisSkills = filterSkillsByName(config.LocalAgents.AnalysisSkillPaths, requested)
		workflowSkills = filterSkillsByName(config.LocalAgents.WorkflowSkillPaths, requested)
	}

	selection := piSkillSelection{
		strategySkills: strategySkills,
		analysisSkills: analysisSkills,
		workflowSkills: workflowSkills,
	}
	switch role {
	case "fundamental_analyst":
		if len(analysisSkills) > 0 {
			selection.dataTool = "read,mota_finance_snapshot"
		} else {
			selection.dataTool = "mota_finance_snapshot"
		}
	case "technical_analyst":
		if len(analysisSkills) > 0 {
			selection.dataTool = "read,mota_market_history"
		} else {
			selection.dataTool = "mota_market_history"
		}
	case "strategist":
		if len(strategySkills) > 0 {
			selection.dataTool = "read,mota_market_history,mota_finance_snapshot"
		}
	case "portfolio_manager", "risk_manager", "execution_trader":
		if len(workflowSkills) > 0 {
			selection.dataTool = "read"
		}
	}
	return selection
}

// firstSkillName 返回第一个 Skill 的目录名;列表为空时返回 fallback。
func firstSkillName(paths []string, fallback string) string {
	if len(paths) == 0 {
		return fallback
	}
	return skillDirName(paths[0])
}

// piDataRequirement 生成启用数据扩展后追加到系统提示词的岗位数据要求。
func piDataRequirement(role string, selection piSkillSelection, dataContract string) string {
	switch role {
	case "fundamental_analyst":
		skillRule := ""
		if name := firstSkillName(selection.analysisSkills, ""); name != "" {
			skillRule = fmt.Sprintf("必须通过受限 read 加载并遵循本 Agent 的 `%s` Skill。", name)
		}
		return fmt.Sprintf("%s\n\n数据要求：%s在生成基本面 rating、fair_value 和 reasoning 前，必须调用 "+
			"mota_finance_snapshot 获取候选标的的公开行情、SEC 财务事实和最新申报。SEED 且 symbolPool "+
			"为空时，先选择最多三只 targetMarkets 内的高流动性美股候选，再一次性查询。必须在 reasoning "+
			"中写明数据日期和缺失项；工具失败时不得编造数据。", dataContract, skillRule)
	case "technical_analyst":
		skillRule := ""
		if name := firstSkillName(selection.analysisSkills, ""); name != "" {
			skillRule = fmt.Sprintf("必须通过受限 read 加载并遵循本 Agent 的 `%s` Skill。", name)
		}
		return fmt.Sprintf("%s\n\n数据要求：%s在生成技术面 bias、entry_zone、stop_loss、targets 和 reasoning 前，必须调用 "+
			"mota_market_history 获取任务标的的当前报价、日线 OHLCV 与指标。只能依据工具返回的 bars/indicators "+
			"判断趋势和关键位，并写明 asOf；无数据或无效代码必须保持 NEUTRAL、entry_zone 为空并解释具体错误，禁止编造行情。",
			dataContract, skillRule)
	case "portfolio_manager":
		return fmt.Sprintf("%s\n\n组合 Skill 要求：必须通过受限 read 加载并遵循本 Agent 的 `%s` Skill。"+
			"完整消费 upstream DAG、runtimeContext、账户、持仓和显式约束；禁止编造收益、协方差、价格或限制。"+
			"严格按 runtimeContext.isFinal 选择过程或最终结构，不得混写。",
			dataContract, firstSkillName(selection.workflowSkills, "portfolio-workflow"))
	case "risk_manager":
		return fmt.Sprintf("%s\n\n风控 Skill 要求：必须通过受限 read 加载并遵循本 Agent 的 `%s` Skill。"+
			"只使用任务、持仓、上游订单和显式阈值；缺失指标必须为 UNKNOWN/null，禁止用 0 伪装。"+
			"gate 必须由最差 check 推导。",
			dataContract, firstSkillName(selection.workflowSkills, "risk-workflow"))
	case "execution_trader":
		return fmt.Sprintf("%s\n\n执行 Skill 要求：必须通过受限 read 加载并遵循本 Agent 的 `%s` Skill。"+
			"必须保留上游 symbol、direction 和最大 quantity，不得在执行阶段创造投资方向；"+
			"缺少盘口、流动性或 tick 数据时禁止编造，只能使用保守的契约合法指令或空 orders。",
			dataContract, firstSkillName(selection.workflowSkills, "execution-workflow"))
	default:
		var skillNames []string
		for _, path := range selection.strategySkills {
			skillNames = append(skillNames, skillDirName(path))
		}
		var selectionRule string
		if len(skillNames) == 1 {
			selectionRule = fmt.Sprintf("本 Agent 固定使用策略 Skill `%s`。", skillNames[0])
		} else {
			selectionRule = fmt.Sprintf("必须比较至少两种适用策略 Skill（当前配置：%s）。",
				strings.Join(skillNames, "、"))
		}
		return fmt.Sprintf("%s\n\n策略 Skill 要求：必须通过受限 read 工具加载已配置的策略 SKILL.md；%s必须调用 "+
			"mota_market_history 获取所有候选标的的 OHLCV/指标，需要质量、增长或估值比较时再调用 mota_finance_snapshot。只输出 "+
			"策略 signals，不输出 orders。strategy_code、market_regime 和 rationale 必须说明采用/放弃哪些 Skill、"+
			"共同数据日期、信号合成、仓位方向和失效风险；禁止编造缺失数据。",
			dataContract, selectionRule)
	}
}

// agentCommand 为 pi / codex / reasonix 构造一条完整的本机 CLI 调用。
//
// 三者共用的收尾处理:工作目录设为 Agent workspace、清空环境、只注入白名单变量。
func agentCommand(
	config *PlatformConfig,
	runtime *AgentRuntime,
	workspace string,
	prompt string,
	systemContract string,
	kind LocalCliKind,
) *LocalCommand {
	local := &LocalCommand{Label: kind.Label(), OutputMode: LocalOutputText}

	switch kind {
	case LocalCliPi:
		local.Cmd = buildPiCommand(config, runtime, prompt, systemContract)
		local.OutputMode = LocalOutputPiJSONLines
	case LocalCliCodex:
		local.Cmd = buildCodexCommand(config, runtime)
		local.OutputMode = LocalOutputCodexJSONLines
		// Codex 以 `-` 从 stdin 读提示词。
		local.StdinPayload = []byte(prompt)
	case LocalCliReasonix:
		cmd, metricsPath := buildReasonixCommand(config, runtime, prompt)
		local.Cmd = cmd
		local.MetricsPath = metricsPath
	case LocalCliOpenCode:
		args := []string{"run", "--agent", "plan", "--format", "default"}
		if model := localModel(config, runtime, kind); model != "" {
			args = append(args, "--model", model)
		}
		args = append(args, prompt)
		local.Cmd = exec.Command(kind.binary(config), args...)
	case LocalCliQwenCode:
		args := []string{"--output-format", "text", "--safe-mode"}
		if model := localModel(config, runtime, kind); model != "" {
			args = append(args, "--model", model)
		}
		args = append(args, "--prompt", prompt)
		local.Cmd = exec.Command(kind.binary(config), args...)
	case LocalCliGeminiCLI:
		args := []string{"--output-format", "json", "--approval-mode", "plan", "--skip-trust"}
		if model := localModel(config, runtime, kind); model != "" {
			args = append(args, "--model", model)
		}
		args = append(args, "--prompt", prompt)
		local.Cmd = exec.Command(kind.binary(config), args...)
		local.OutputMode = LocalOutputGeminiJSON
	case LocalCliOpenHands:
		local.Cmd = exec.Command(kind.binary(config), "--headless", "--json", "--task", prompt)
		local.OutputMode = LocalOutputOpenHandsJSONLines
	case LocalCliDeepseekHarness:
		model := localModel(config, runtime, kind)
		if model == "" {
			model = "deepseek-v4-flash"
		}
		patchPath := filepath.Join(os.TempDir(), fmt.Sprintf("mota-dsh-model-%d-%s.yml", os.Getpid(), randomToken(12)))
		quotedModel := strconv.Quote(model)
		_ = os.WriteFile(patchPath, []byte("- id: agent-default-model\n  config:\n    provider: deepseek-official\n    model: "+quotedModel+"\n"), 0o600)
		local.Cmd = exec.Command(kind.binary(config), "--profile", "headless", "--patch", patchPath, prompt)
		local.CleanupPaths = append(local.CleanupPaths, patchPath)
	}

	local.Cmd.Dir = workspace
	// 显式清空环境:子进程只能看到白名单变量,不继承宿主的其它凭据。
	local.Cmd.Env = []string{}
	copyAllowedEnv(local.Cmd, config.LocalAgents.AllowedEnv)
	if kind == LocalCliOpenHands {
		if model := localModel(config, runtime, kind); model != "" {
			setEnvVar(local.Cmd, "LLM_MODEL", model)
			if localSliceContains(config.LocalAgents.AllowedEnv, "LLM_API_KEY") && hasEnvVar(local.Cmd, "LLM_API_KEY") {
				local.Cmd.Args = append(local.Cmd.Args, "--override-with-envs")
			}
		}
	}
	if kind == LocalCliDeepseekHarness {
		if config.Gateway.BaseURL != "" {
			setEnvVar(local.Cmd, "DEEPSEEK_BASE_URL", config.Gateway.BaseURL)
		}
		if config.Gateway.APIKey != "" {
			setEnvVar(local.Cmd, "DEEPSEEK_API_KEY", config.Gateway.APIKey)
		}
		if model := localModel(config, runtime, kind); model != "" {
			setEnvVar(local.Cmd, "DEEPSEEK_MODEL", model)
			setEnvVar(local.Cmd, "DSH_MODEL", model)
		}
	}
	return local
}

func localSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// buildPiCommand 组装 Pi 的 argv。提示词是最后一个位置参数,不走 stdin。
func buildPiCommand(config *PlatformConfig, runtime *AgentRuntime, prompt, systemContract string) *exec.Cmd {
	args := []string{"--print", "--mode", "json", "--no-session"}

	role := strings.TrimSpace(runtime.Config.SystemPrompt)
	selection := selectPiSkills(config, runtime, role)
	// 数据工具与只读金融扩展必须同时具备才启用取数模式。
	useDataExtension := selection.dataTool != "" && config.LocalAgents.FinanceExtensionPath != ""

	var piSystemPrompt string
	if useDataExtension {
		args = append(args,
			"--no-builtin-tools", "--no-extensions", "--no-skills",
			"--extension", config.LocalAgents.FinanceExtensionPath,
			"--tools", selection.dataTool)
		// 策略岗位挂策略 Skill;其余岗位挂分析 + 流程 Skill。
		if role == "strategist" {
			for _, skill := range selection.strategySkills {
				args = append(args, "--skill", skill)
			}
		} else {
			for _, skill := range selection.analysisSkills {
				args = append(args, "--skill", skill)
			}
			for _, skill := range selection.workflowSkills {
				args = append(args, "--skill", skill)
			}
		}
		dataContract := strings.ReplaceAll(systemContract, noToolClause, "")
		piSystemPrompt = piDataRequirement(role, selection, dataContract)
	} else {
		args = append(args, "--no-tools")
		piSystemPrompt = systemContract
	}

	args = append(args, "--system-prompt", piSystemPrompt)
	if model := localModel(config, runtime, LocalCliPi); model != "" {
		args = append(args, "--model", model)
	}
	if useDataExtension {
		prompt = strings.ReplaceAll(prompt, noToolClause, "")
	}
	args = append(args, prompt)

	return exec.Command(LocalCliPi.binary(config), args...)
}

// buildCodexCommand 组装 Codex 的 argv。提示词经 stdin 传入(末尾的 `-`)。
func buildCodexCommand(config *PlatformConfig, runtime *AgentRuntime) *exec.Cmd {
	args := []string{
		"exec", "--json", "--ignore-user-config",
		"--disable", "multi_agent",
		"--disable", "multi_agent_v2",
		"--skip-git-repo-check", "--ephemeral",
		"--sandbox", "read-only",
	}
	if strings.TrimSpace(config.Gateway.BaseURL) != "" {
		base := strings.TrimRight(config.Gateway.BaseURL, "/")
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		provider := fmt.Sprintf(
			`model_providers.litellm={name="LiteLLM",base_url="%s",env_key="LITELLM_API_KEY",wire_api="responses"}`,
			base)
		args = append(args, "--config", `model_provider="litellm"`, "--config", provider)
	}
	if model := localModel(config, runtime, LocalCliCodex); model != "" {
		// Codex 的 --model 只接受裸模型名,provider 由 --config 指定。
		args = append(args, "--model", strings.TrimPrefix(model, "litellm/"))
	}
	args = append(args, "-")
	return exec.Command(LocalCliCodex.binary(config), args...)
}

// buildReasonixCommand 组装 Reasonix 的 argv,并返回 usage metrics 的临时文件路径。
//
// Reasonix 1.31.x 的 run 子命令不支持 --system;岗位契约已由 buildPrompt 注入到
// 提示词里,这里只启用非交互最终输出与 metrics 侧信道。
func buildReasonixCommand(config *PlatformConfig, runtime *AgentRuntime, prompt string) (*exec.Cmd, string) {
	metricsPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("mota-reasonix-metrics-%d-%s.json", os.Getpid(), randomToken(12)))

	args := []string{"run", "--print", "--metrics", metricsPath}
	if model := localModel(config, runtime, LocalCliReasonix); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)
	return exec.Command(LocalCliReasonix.binary(config), args...), metricsPath
}

// claudeModelAliases 是 Claude Code 侧的遗留别名;注意与本机 CLI 的别名集不同
// (这里是 litellm/dsk_4f,那边是裸 dsk_4f)。
var claudeModelAliases = map[string]bool{"": true, "deepseek-v4-flash": true, "litellm/dsk_4f": true}

// claudeCommand 组装 Claude Code 的调用。提示词经 stdin 传入。
func claudeCommand(
	config *PlatformConfig,
	runtime *AgentRuntime,
	workspace string,
	prompt string,
	systemContract string,
) (*LocalCommand, error) {
	if !config.ClaudeEnabled {
		return nil, newExecutionError("executor_unavailable", "Claude Code executor is disabled")
	}

	args := []string{
		"--print", "--output-format", "json", "--no-session-persistence", "--bare",
		"--permission-mode", runtime.Config.PermissionMode,
		"--system-prompt", systemContract,
		// 注意 Claude 用的是单参数 `--tools=a,b`,与 Pi 的 `--tools a` 不同。
		"--tools=" + strings.Join(runtime.Config.AllowedTools, ","),
	}
	if runtime.Config.ClaudeModel != nil {
		model := strings.TrimSpace(*runtime.Config.ClaudeModel)
		if claudeModelAliases[model] {
			model = config.ClaudeDefaultModel
		}
		args = append(args, "--model", model)
	} else if config.ClaudeDefaultModel != "" {
		args = append(args, "--model", config.ClaudeDefaultModel)
	}

	cmd := exec.Command(config.ClaudeBinary, args...)
	cmd.Dir = workspace
	cmd.Env = []string{}
	copyAllowedEnv(cmd, config.ClaudeAllowedEnv)

	// --bare 不读取 settings.json 中的 env;显式透传受控 LiteLLM 变量,
	// 并兼容现网只配置 ANTHROPIC_AUTH_TOKEN 的部署。
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if value, ok := os.LookupEnv(name); ok {
			setEnvVar(cmd, name, value)
		}
	}
	if !hasEnvVar(cmd, "ANTHROPIC_API_KEY") {
		if _, ok := os.LookupEnv("ANTHROPIC_API_KEY"); !ok {
			if value, ok := os.LookupEnv("ANTHROPIC_AUTH_TOKEN"); ok {
				setEnvVar(cmd, "ANTHROPIC_API_KEY", value)
			}
		}
	}

	return &LocalCommand{
		Cmd:          cmd,
		StdinPayload: []byte(prompt),
		Label:        "Claude Code",
		OutputMode:   LocalOutputText,
	}, nil
}
