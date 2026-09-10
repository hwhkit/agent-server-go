package mota

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// resolveRoleContract
// ---------------------------------------------------------------------------

func TestResolveRoleContractPresetRoles(t *testing.T) {
	for _, tc := range []struct {
		id    string
		label string
	}{
		{"fundamental_analyst", "基本面分析师"},
		{"technical_analyst", "技术分析师"},
		{"strategist", "策略分析师"},
		{"risk_manager", "风控经理"},
		{"execution_trader", "执行交易员"},
		{"portfolio_manager", "组合经理"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			label, contract := resolveRoleContract(tc.id)
			if label != tc.label {
				t.Errorf("label = %q, want %q", label, tc.label)
			}
			if contract == "" {
				t.Fatal("contract must not be empty")
			}
			// 每份契约都要求「只输出一个 JSON 对象」。
			if !strings.HasPrefix(contract, "只输出一个 JSON 对象") {
				t.Errorf("contract should open with the JSON-only instruction, got %.40q", contract)
			}
		})
	}
}

// 首尾空白不影响命中预置岗位。
func TestResolveRoleContractTrimsInput(t *testing.T) {
	label, contract := resolveRoleContract("  risk_manager\n")
	if label != "风控经理" {
		t.Errorf("label = %q, want 风控经理", label)
	}
	if contract != riskManagerContract {
		t.Error("trimmed preset id should resolve to the risk manager contract")
	}
}

// 空 system_prompt 回退到第一个岗位(基本面)。
func TestResolveRoleContractEmptyFallsBackToFundamental(t *testing.T) {
	for _, input := range []string{"", "   ", "\n\t"} {
		label, contract := resolveRoleContract(input)
		if label != "基本面分析师" {
			t.Errorf("input %q => label %q, want 基本面分析师", input, label)
		}
		if contract != fundamentalAnalystContract {
			t.Errorf("input %q should fall back to the fundamental contract", input)
		}
	}
}

// 未知标识视为自定义提示词,原文(未 trim)直接作为契约。
func TestResolveRoleContractCustomPrompt(t *testing.T) {
	custom := "  你是一个特殊的研究助理，请输出 JSON。  "
	label, contract := resolveRoleContract(custom)
	if label != customRoleLabel {
		t.Errorf("label = %q, want %q", label, customRoleLabel)
	}
	if contract != custom {
		t.Errorf("custom contract must be passed through verbatim (untrimmed), got %q", contract)
	}
}

func TestRoleDescriptors(t *testing.T) {
	descriptors := RoleDescriptors()
	if len(descriptors) != 6 {
		t.Fatalf("len = %d, want 6", len(descriptors))
	}
	if descriptors[0].ID != "fundamental_analyst" {
		t.Errorf("first descriptor = %q, want fundamental_analyst", descriptors[0].ID)
	}
	for _, descriptor := range descriptors {
		if descriptor.ID == "" || descriptor.Label == "" {
			t.Errorf("descriptor %+v has empty fields", descriptor)
		}
	}
}

// ---------------------------------------------------------------------------
// outputLanguageInstruction
// ---------------------------------------------------------------------------

func languageInput(language any) map[string]any {
	global := map[string]any{}
	if language != nil {
		global["language"] = language
	}
	return map[string]any{"globalParameters": global}
}

func TestOutputLanguageInstructionChinese(t *testing.T) {
	for _, language := range []string{"zh-CN", "zh", "ZH-Hant", "  zh-CN  "} {
		got := outputLanguageInstruction(languageInput(language))
		if !strings.Contains(got, "必须统一使用简体中文") {
			t.Errorf("language %q should select the Chinese instruction, got %.60q", language, got)
		}
		// 回显的是 trim 后的实际值。
		if !strings.Contains(got, "globalParameters.language="+strings.TrimSpace(language)) {
			t.Errorf("instruction should echo the trimmed language %q: %.60q", language, got)
		}
	}
}

func TestOutputLanguageInstructionEnglish(t *testing.T) {
	for _, language := range []string{"en-US", "en", "EN-GB"} {
		got := outputLanguageInstruction(languageInput(language))
		if !strings.Contains(got, "must be English only") {
			t.Errorf("language %q should select the English instruction, got %.60q", language, got)
		}
	}
}

func TestOutputLanguageInstructionOtherLanguage(t *testing.T) {
	got := outputLanguageInstruction(languageInput("ja-JP"))
	if !strings.Contains(got, "must use the requested language consistently") {
		t.Errorf("an unknown language should select the generic instruction, got %.60q", got)
	}
	if !strings.Contains(got, "globalParameters.language=ja-JP") {
		t.Errorf("instruction should echo the language: %.60q", got)
	}
}

// 缺省/空白/非字符串一律回退 en-US。
func TestOutputLanguageInstructionDefaultsToEnglish(t *testing.T) {
	for name, input := range map[string]any{
		"nil input":                      nil,
		"no globalParameters":            map[string]any{},
		"no language key":                languageInput(nil),
		"blank language":                 languageInput("   "),
		"non-string language":            languageInput(42),
		"globalParameters not an object": map[string]any{"globalParameters": "zh-CN"},
	} {
		got := outputLanguageInstruction(input)
		if !strings.Contains(got, "globalParameters.language="+defaultOutputLanguage) {
			t.Errorf("%s should default to %s, got %.60q", name, defaultOutputLanguage, got)
		}
		if !strings.Contains(got, "must be English only") {
			t.Errorf("%s should select the English instruction, got %.60q", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// buildPrompt
// ---------------------------------------------------------------------------

func TestBuildPromptStructure(t *testing.T) {
	runtime := localTestRuntime("risk_manager", nil, nil)
	runtime.Config.Slug = "risk-one"
	rawInput := map[string]any{
		"runId":            "run-1",
		"globalParameters": map[string]any{"language": "zh-CN"},
	}
	prompt := buildPrompt(runtime, rawInput)

	// 开头点明 Agent slug。
	if !strings.HasPrefix(prompt, "执行 Agent `risk-one` 收到的任务。") {
		t.Errorf("prompt should open with the agent slug, got %.60q", prompt)
	}
	// 岗位中文名与契约正文。
	if !strings.Contains(prompt, "输出契约(风控经理):") {
		t.Errorf("prompt should name the role label: %.200q", prompt)
	}
	if !strings.Contains(prompt, riskManagerContract) {
		t.Error("prompt should embed the resolved contract verbatim")
	}
	// 任务输入以紧凑 JSON 内联。
	if !strings.Contains(prompt, "任务输入(JSON):") {
		t.Error("prompt should carry the task input section")
	}
	if !strings.Contains(prompt, `"runId":"run-1"`) {
		t.Errorf("prompt should embed the compact JSON input: %.400q", prompt)
	}
	// 输出约束与语言硬约束。
	if !strings.Contains(prompt, "50 字以内的纯文本摘要") {
		t.Error("prompt should carry the payload_info constraint")
	}
	if !strings.Contains(prompt, "2000 token") {
		t.Error("prompt should carry the token budget")
	}
	if !strings.Contains(prompt, "输出语言硬约束:globalParameters.language=zh-CN") {
		t.Errorf("prompt should carry the language constraint: %.400q", prompt)
	}
	// 投资范围说明。
	if !strings.Contains(prompt, "RESTRICTED(缺省)只评估 symbolPool 内的标的") {
		t.Error("prompt should explain RESTRICTED universe mode")
	}
	if !strings.Contains(prompt, "SEED 时池子只是起点") {
		t.Error("prompt should explain SEED universe mode")
	}
}

// 空 system_prompt 的提示词用基本面契约与中文名。
func TestBuildPromptEmptyRoleUsesFundamental(t *testing.T) {
	runtime := localTestRuntime("", nil, nil)
	prompt := buildPrompt(runtime, map[string]any{})
	if !strings.Contains(prompt, "输出契约(基本面分析师):") {
		t.Errorf("prompt should fall back to the fundamental role: %.200q", prompt)
	}
	if !strings.Contains(prompt, fundamentalAnalystContract) {
		t.Error("prompt should embed the fundamental contract")
	}
}

// 自定义提示词直接作为契约,岗位名显示「自定义」。
func TestBuildPromptCustomRole(t *testing.T) {
	runtime := localTestRuntime("请只输出一个 JSON 对象。", nil, nil)
	prompt := buildPrompt(runtime, map[string]any{})
	if !strings.Contains(prompt, "输出契约(自定义):") {
		t.Errorf("prompt should mark the role as custom: %.200q", prompt)
	}
	if !strings.Contains(prompt, "请只输出一个 JSON 对象。") {
		t.Error("prompt should embed the custom contract text")
	}
}

// 任务输入无法序列化时不 panic,退化为 "null"。
func TestBuildPromptToleratesUnserializableInput(t *testing.T) {
	runtime := localTestRuntime("strategist", nil, nil)
	prompt := buildPrompt(runtime, map[string]any{"bad": func() {}})
	if !strings.Contains(prompt, "任务输入(JSON):\nnull") {
		t.Errorf("unserializable input should degrade to null: %.400q", prompt)
	}
}

// ---------------------------------------------------------------------------
// 与 Rust 侧契约正文的一致性
// ---------------------------------------------------------------------------

// rustContractSource 是 Rust 侧契约常量所在文件。仓库并排放置时才做比对。
const rustContractSource = "../../mota-agent-server-rs/src/platform/executor/mod.rs"

// TestContractsMatchRustSource 直接从 Rust 源码抽出六个 *_CONTRACT 常量,
// 逐字节比对 Go 常量。
//
// 这些文本就是模型面对的协议:任何一处措辞漂移都会改变输出行为,而且不会被
// 其它测试发现。找不到 Rust 源码时跳过(例如只发布 Go 仓库的场景)。
func TestContractsMatchRustSource(t *testing.T) {
	source, err := os.ReadFile(rustContractSource)
	if err != nil {
		t.Skipf("Rust source not available at %s: %v", rustContractSource, err)
	}
	for _, tc := range []struct {
		rustName string
		goValue  string
	}{
		{"FUNDAMENTAL_ANALYST_CONTRACT", fundamentalAnalystContract},
		{"TECHNICAL_ANALYST_CONTRACT", technicalAnalystContract},
		{"STRATEGIST_CONTRACT", strategistContract},
		{"RISK_MANAGER_CONTRACT", riskManagerContract},
		{"EXECUTION_TRADER_CONTRACT", executionTraderContract},
		{"PORTFOLIO_MANAGER_CONTRACT", portfolioManagerContract},
	} {
		t.Run(tc.rustName, func(t *testing.T) {
			pattern := regexp.MustCompile(`(?s)const ` + tc.rustName + `: &str = r#"(.*?)"#;`)
			match := pattern.FindSubmatch(source)
			if match == nil {
				t.Fatalf("could not locate %s in the Rust source", tc.rustName)
			}
			if want := string(match[1]); tc.goValue != want {
				t.Errorf("contract text drifted from the Rust source.\nGo   (%d bytes)\nRust (%d bytes)",
					len(tc.goValue), len(want))
			}
		})
	}
}

// 每份契约都必须明确禁止 markdown 代码块与额外解释文字。
func TestContractsForbidMarkdownAndProse(t *testing.T) {
	for name, contract := range map[string]string{
		"fundamental": fundamentalAnalystContract,
		"technical":   technicalAnalystContract,
		"strategist":  strategistContract,
		"risk":        riskManagerContract,
		"execution":   executionTraderContract,
		"portfolio":   portfolioManagerContract,
	} {
		if !strings.Contains(contract, "不要 markdown 代码块") {
			t.Errorf("%s contract must forbid markdown fences", name)
		}
		if !strings.Contains(contract, "不要输出 DSML 或任何解释文字") {
			t.Errorf("%s contract must forbid extra prose", name)
		}
	}
}

// 契约里的 JSON 骨架必须是合法 JSON,否则模型会照着坏样例输出。
func TestContractJSONSkeletonsAreValid(t *testing.T) {
	// 骨架以 "{" 独占一行开始,到对应的 "}" 独占一行结束。
	skeleton := regexp.MustCompile(`(?ms)^\{$.*?^\}$`)
	for name, contract := range map[string]string{
		"fundamental": fundamentalAnalystContract,
		"technical":   technicalAnalystContract,
		"strategist":  strategistContract,
		"risk":        riskManagerContract,
		"execution":   executionTraderContract,
		"portfolio":   portfolioManagerContract,
	} {
		blocks := skeleton.FindAllString(contract, -1)
		if len(blocks) == 0 {
			t.Errorf("%s contract has no JSON skeleton", name)
			continue
		}
		for i, block := range blocks {
			// 样例里用 …… 占位,替换成字符串再校验结构。
			normalized := strings.ReplaceAll(block, "……", "text")
			var parsed map[string]any
			if err := json.Unmarshal([]byte(normalized), &parsed); err != nil {
				t.Errorf("%s contract skeleton %d is not valid JSON: %v", name, i, err)
				continue
			}
			if _, present := parsed["payload_result"]; !present {
				t.Errorf("%s contract skeleton %d must be wrapped in payload_result", name, i)
			}
		}
	}
}

// 组合经理契约必须同时给出两套结构,并要求按 isFinal 二选一。
func TestPortfolioManagerContractHasBothBranches(t *testing.T) {
	for _, want := range []string{
		"按 runtimeContext.isFinal 二选一输出,不要混合两套结构。",
		"【职能一:isFinal 缺省或为 false",
		"【职能二:isFinal = true",
		"硬性要求(职能一):",
		"硬性要求(职能二):",
		"两个分支字段不可混写",
	} {
		if !strings.Contains(portfolioManagerContract, want) {
			t.Errorf("portfolio manager contract missing %q", want)
		}
	}
}
