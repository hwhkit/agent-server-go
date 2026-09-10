package mota

import (
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ExtractTargetCount
// ---------------------------------------------------------------------------

func TestExtractTargetCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		brief any
		want  int
		ok    bool
	}{
		{"nil brief", nil, 0, false},
		{"empty object", map[string]any{}, 0, false},
		{"no count in text", map[string]any{"userCustomInstructions": "请给出评估"}, 0, false},
		{"ascii digits with 只", map[string]any{"userCustomInstructions": "精选 5 只标的"}, 5, true},
		{"ascii digits no space", map[string]any{"userCustomInstructions": "恰好3条信号"}, 3, true},
		{"han numeral", map[string]any{"userCustomInstructions": "覆盖全部五只"}, 5, true},
		{"han 两", map[string]any{"userCustomInstructions": "两个方向"}, 2, true},
		{"han 十", map[string]any{"userCustomInstructions": "十只标的"}, 10, true},
		{"维度 suffix", map[string]any{"userCustomInstructions": "六维度检查"}, 6, true},
		{"个维度", map[string]any{"userCustomInstructions": "6 个维度"}, 6, true},
		{"项", map[string]any{"userCustomInstructions": "4 项检查"}, 4, true},
		{"plain string brief", "精选 7 只", 7, true},
		{"upstreamRequirements key", map[string]any{"upstreamRequirements": "输出 8 条订单"}, 8, true},
		{"unit not recognised", map[string]any{"userCustomInstructions": "5 天均线"}, 0, false},
		{"zero rejected", map[string]any{"userCustomInstructions": "0 只"}, 0, false},
		{"over 100 rejected", map[string]any{"userCustomInstructions": "101 只"}, 0, false},
		{"exactly 100 accepted", map[string]any{"userCustomInstructions": "100 只"}, 100, true},
		{"non-string field ignored", map[string]any{"userCustomInstructions": 5}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractTargetCount(tc.brief)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ExtractTargetCount(%v) = (%d, %t), want (%d, %t)", tc.brief, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// 两个键的文本会被拼接后一起扫描。
func TestExtractTargetCountConcatenatesBothKeys(t *testing.T) {
	brief := map[string]any{
		"userCustomInstructions": "无数量要求",
		"upstreamRequirements":   "请输出 9 条",
	}
	got, ok := ExtractTargetCount(brief)
	if !ok || got != 9 {
		t.Errorf("got (%d, %t), want (9, true)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// ExtractFailureReason / ExtractFailurePath
// ---------------------------------------------------------------------------

func TestExtractFailureReason(t *testing.T) {
	for _, keyword := range failurePathKeywords {
		brief := map[string]any{"userCustomInstructions": "现有信息" + keyword + "，请给保守结果"}
		reason, ok := ExtractFailureReason(brief)
		if !ok {
			t.Fatalf("keyword %q should be detected", keyword)
		}
		if !strings.Contains(reason, keyword) {
			t.Errorf("reason %q should name the matched keyword %q", reason, keyword)
		}
		if !ExtractFailurePath(brief) {
			t.Errorf("ExtractFailurePath should agree for %q", keyword)
		}
	}
}

// 原因只回显命中的规则,不回显完整用户输入。
func TestExtractFailureReasonDoesNotEchoUserInput(t *testing.T) {
	secret := "SENSITIVE-INTERNAL-NOTE-12345"
	brief := map[string]any{"userCustomInstructions": secret + " 无法完成"}
	reason, ok := ExtractFailureReason(brief)
	if !ok {
		t.Fatal("expected a failure reason")
	}
	if strings.Contains(reason, secret) {
		t.Errorf("reason must not echo the user input: %q", reason)
	}
}

func TestExtractFailureReasonNegative(t *testing.T) {
	for _, brief := range []any{
		nil,
		map[string]any{},
		map[string]any{"userCustomInstructions": "正常任务，请给出完整评估"},
		"一切正常",
		42,
	} {
		if _, ok := ExtractFailureReason(brief); ok {
			t.Errorf("brief %v must not be a failure path", brief)
		}
		if ExtractFailurePath(brief) {
			t.Errorf("ExtractFailurePath(%v) must be false", brief)
		}
	}
}

// 字符串形态的 taskBrief 也参与关键词检测。
func TestExtractFailureReasonFromStringBrief(t *testing.T) {
	if _, ok := ExtractFailureReason("数据不足以支撑结论"); !ok {
		t.Error("string brief should be scanned for failure keywords")
	}
}

// ---------------------------------------------------------------------------
// ExtractUniverseMode / ExtractSymbolPool
// ---------------------------------------------------------------------------

func TestExtractUniverseMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global any
		want   string
	}{
		{"nil", nil, ""},
		{"missing key", map[string]any{}, ""},
		{"restricted", map[string]any{"universeMode": "RESTRICTED"}, "RESTRICTED"},
		{"seed", map[string]any{"universeMode": "SEED"}, "SEED"},
		{"lowercase normalised", map[string]any{"universeMode": "seed"}, "SEED"},
		{"whitespace trimmed", map[string]any{"universeMode": "  restricted  "}, "RESTRICTED"},
		{"unknown rejected", map[string]any{"universeMode": "OPEN"}, ""},
		{"non-string rejected", map[string]any{"universeMode": 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractUniverseMode(tc.global)
			if tc.want == "" {
				if got != nil {
					t.Errorf("got %q, want nil", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Errorf("got %v, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractSymbolPool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global any
		want   []string
	}{
		{"nil", nil, nil},
		{"missing key", map[string]any{}, nil},
		{"empty array", map[string]any{"symbolPool": []any{}}, nil},
		{"strings", map[string]any{"symbolPool": []any{"AAPL.US", "0700.HK"}}, []string{"AAPL.US", "0700.HK"}},
		{"non-strings skipped", map[string]any{"symbolPool": []any{"AAPL.US", 42, nil}}, []string{"AAPL.US"}},
		{"not an array", map[string]any{"symbolPool": "AAPL.US"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractSymbolPool(tc.global)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ExtractUpstreamSymbols
// ---------------------------------------------------------------------------

func TestExtractUpstreamSymbolsFromUpstreamPayload(t *testing.T) {
	upstream := []any{
		map[string]any{
			"agentInstanceId": "a1",
			"payloadResult": map[string]any{
				"orders": []any{
					map[string]any{"symbol": "aapl.us"},
					map[string]any{"symbol": " 0700.hk "},
					map[string]any{"symbol": "AAPL.US"}, // 去重
				},
			},
		},
		map[string]any{
			"agentInstanceId": "a2",
			"payloadResult":   map[string]any{"orders": []any{map[string]any{"symbol": "MSFT.US"}}},
		},
	}
	got := ExtractUpstreamSymbols(upstream, nil)
	want := []string{"AAPL.US", "0700.HK", "MSFT.US"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractUpstreamSymbolsFromDAGNodes(t *testing.T) {
	upstream := map[string]any{
		"edges": []any{},
		"nodes": []any{
			map[string]any{
				"agentInstanceId": "n1",
				"output":          map[string]any{"orders": []any{map[string]any{"symbol": "tsla.us"}}},
			},
			map[string]any{"agentInstanceId": "n2", "output": map[string]any{"orders": []any{}}},
		},
		"self": map[string]any{},
	}
	got := ExtractUpstreamSymbols(upstream, nil)
	if !reflect.DeepEqual(got, []string{"TSLA.US"}) {
		t.Errorf("got %v, want [TSLA.US]", got)
	}
}

// target_allocation 不参与提取:CASH 等占位不是可成交订单。
func TestExtractUpstreamSymbolsIgnoresTargetAllocation(t *testing.T) {
	upstream := []any{
		map[string]any{
			"payloadResult": map[string]any{
				"target_allocation": []any{
					map[string]any{"symbol": "CASH"},
					map[string]any{"symbol": "SPY.US"},
				},
			},
		},
	}
	if got := ExtractUpstreamSymbols(upstream, nil); len(got) != 0 {
		t.Errorf("got %v, want no symbols from target_allocation", got)
	}
}

func TestExtractUpstreamSymbolsEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream any
	}{
		{"nil", nil},
		{"empty array", []any{}},
		{"missing payloadResult", []any{map[string]any{"agentInstanceId": "a"}}},
		{"orders not an array", []any{map[string]any{"payloadResult": map[string]any{"orders": "x"}}}},
		{"order missing symbol", []any{map[string]any{"payloadResult": map[string]any{"orders": []any{map[string]any{}}}}}},
		{"blank symbol", []any{map[string]any{"payloadResult": map[string]any{"orders": []any{map[string]any{"symbol": "   "}}}}}},
		{"scalar upstream", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractUpstreamSymbols(tc.upstream, nil); len(got) != 0 {
				t.Errorf("got %v, want empty", got)
			}
		})
	}
}

// upstreamPayload 有结果时不再回落到 DAG 分支。
func TestExtractUpstreamSymbolsPrefersPayloadOverDAG(t *testing.T) {
	upstream := []any{
		map[string]any{"payloadResult": map[string]any{"orders": []any{map[string]any{"symbol": "FROM.PAYLOAD"}}}},
	}
	got := ExtractUpstreamSymbols(upstream, nil)
	if !reflect.DeepEqual(got, []string{"FROM.PAYLOAD"}) {
		t.Errorf("got %v, want [FROM.PAYLOAD]", got)
	}
}
