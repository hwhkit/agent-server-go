package teamrun

// 模板渲染基准：团队 DAG 每步派发前渲染一次输入模板；多占位符场景覆盖
// 替换循环的常态形态（{{team.input…}} 与 {{steps.<id>.output…}} 各占半）。
import (
	"encoding/json"
	"testing"
)

func BenchmarkRender(b *testing.B) {
	template, _ := json.Marshal(map[string]any{
		"brief":   "{{team.input.topic}}",
		"up1":     "{{steps.up.output.summary}}",
		"up2":     "{{steps.up.output.metrics.orders}}",
		"context": "{{team.input.context}}",
	})
	rc := RenderContext{
		TeamInput: json.RawMessage(`{"topic":"benchmark","context":"ctx"}`),
		StepOutputs: map[string]json.RawMessage{
			"up": json.RawMessage(`{"summary":"ok","metrics":{"orders":42}}`),
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := Render(template, rc); err != nil {
			b.Fatal(err)
		}
	}
}
