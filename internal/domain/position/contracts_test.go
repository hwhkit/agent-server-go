package position

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 契约正文的 golden 快照守护（随契约文本自 ir/ 迁入，语义不变）
// ---------------------------------------------------------------------------

var updateGolden = flag.Bool("update", false, "重新生成契约 golden 快照")

// TestContractsGolden 把六个预置岗位契约与仓内快照逐字节比对。
//
// 这些文本就是模型面对的协议:任何一处措辞漂移都会改变输出行为,而且不会被
// 其它测试发现。有意修改契约的流程见 testdata/contracts/README.md:
//  1. 运行 go test ./internal/domain/position -run TestContractsGolden -update 重新生成快照;
//  2. 在 CHANGELOG 记录行为变更原因。
func TestContractsGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"FUNDAMENTAL_ANALYST_CONTRACT", FundamentalAnalystContract},
		{"TECHNICAL_ANALYST_CONTRACT", TechnicalAnalystContract},
		{"STRATEGIST_CONTRACT", StrategistContract},
		{"RISK_MANAGER_CONTRACT", RiskManagerContract},
		{"EXECUTION_TRADER_CONTRACT", ExecutionTraderContract},
		{"PORTFOLIO_MANAGER_CONTRACT", PortfolioManagerContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goldenPath := filepath.Join("testdata", "contracts", tc.name+".txt")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("create golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, []byte(tc.value), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("golden 快照缺失(%v);首次生成或有意更新请运行: go test ./internal/domain/position -run TestContractsGolden -update", err)
			}
			if tc.value != string(want) {
				t.Errorf("契约正文与 golden 快照不一致。\n当前 (%d bytes)\n快照 (%d bytes)\n如果是有意修改,运行 go test ./internal/domain/position -run TestContractsGolden -update 并在 CHANGELOG 记录。",
					len(tc.value), len(want))
			}
		})
	}
}

// 每份契约都必须明确禁止 markdown 代码块与额外解释文字。
func TestContractsForbidMarkdownAndProse(t *testing.T) {
	for name, contract := range map[string]string{
		"fundamental": FundamentalAnalystContract,
		"technical":   TechnicalAnalystContract,
		"strategist":  StrategistContract,
		"risk":        RiskManagerContract,
		"execution":   ExecutionTraderContract,
		"portfolio":   PortfolioManagerContract,
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
		"fundamental": FundamentalAnalystContract,
		"technical":   TechnicalAnalystContract,
		"strategist":  StrategistContract,
		"risk":        RiskManagerContract,
		"execution":   ExecutionTraderContract,
		"portfolio":   PortfolioManagerContract,
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
		if !strings.Contains(PortfolioManagerContract, want) {
			t.Errorf("portfolio manager contract missing %q", want)
		}
	}
}
