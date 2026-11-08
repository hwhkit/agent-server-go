package agent

// C5 领域不变量测试：运行约束上限在 Validate 收口——管理 API、批量导入
// 与任何未来写入路径共享同一防线（拦异常值，不拦合理调优）。
import "testing"

import "ir-agent-server/internal/domain/exec"

func TestValidateRuntimeCaps(t *testing.T) {
	base := func() *Agent {
		a := &Agent{Slug: "cap-agent", Name: "上限", Enabled: false,
			Binding:       exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
			SigningSecret: []byte("cap-agent-secret-0123456789")}
		a.ApplyDefaults()
		return a
	}
	ok := base()
	ok.TimeoutSecs = MaxTimeoutSecs
	ok.MaxConcurrency = MaxConcurrency
	ok.QueueCapacity = MaxQueueCapacity
	if err := ok.Validate(false); err != nil {
		t.Errorf("boundary values must pass: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Agent)
	}{
		{"timeout over cap", func(a *Agent) { a.TimeoutSecs = MaxTimeoutSecs + 1 }},
		{"concurrency over cap", func(a *Agent) { a.MaxConcurrency = MaxConcurrency + 1 }},
		{"queue over cap", func(a *Agent) { a.QueueCapacity = MaxQueueCapacity + 1 }},
	} {
		a := base()
		tc.mutate(a)
		if err := a.Validate(false); err == nil {
			t.Errorf("%s: must be rejected by domain Validate", tc.name)
		}
	}
}
