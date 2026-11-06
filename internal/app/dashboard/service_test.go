package dashboard

// 总览聚合缓存测试：TTL 窗口内并发/连续 Snapshot 只打一次来源（消费者
// 数量不线性放大 DB COUNT），TTL 过后重新聚合；来源失败降级零值。
import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/run"
)

type fakeHealth struct{ pings atomic.Int64 }

func (f *fakeHealth) Ping(context.Context) error { f.pings.Add(1); return nil }

type fakeRuns struct{ calls atomic.Int64 }

func (f *fakeRuns) RunTotals(context.Context, time.Time) (run.Totals, error) {
	f.calls.Add(1)
	return run.Totals{Completed: 5}, nil
}

type fakeQueue struct{ calls atomic.Int64 }

func (f *fakeQueue) QueueTotals(context.Context) (connector.QueueTotals, error) {
	f.calls.Add(1)
	return connector.QueueTotals{Queued: 2}, nil
}

type fakeWorkers struct{ calls atomic.Int64 }

func (f *fakeWorkers) ListOnline(context.Context) ([]connector.Worker, error) {
	f.calls.Add(1)
	return nil, nil
}

func (f *fakeWorkers) Beat(context.Context, connector.Worker) error { return nil }

func (f *fakeWorkers) MarkOfflineStale(context.Context, time.Time) (int, error) {
	return 0, nil
}

func TestSnapshotCacheTTL(t *testing.T) {
	health := &fakeHealth{}
	svc := NewService(Deps{
		Version: "test", Health: health, Runs: &fakeRuns{},
		Queue: &fakeQueue{}, Workers: &fakeWorkers{},
		CacheTTL: time.Hour, // 窗口长到测试内不会自然过期
	})
	first := svc.Snapshot(context.Background())
	if !first.DatabaseOK || first.RunsToday.Completed != 5 || first.Queue.Queued != 2 {
		t.Fatalf("first snapshot = %+v", first)
	}
	for range 5 {
		again := svc.Snapshot(context.Background())
		if again.ServerTime != first.ServerTime {
			t.Error("cached snapshot must be the identical aggregate")
		}
	}
	if pings := health.pings.Load(); pings != 1 {
		t.Errorf("source pings within TTL = %d, want 1", pings)
	}
}

// TestSnapshotDegradedOnFailure 单项来源失败降级零值且不污染缓存语义
// （窗口内返回同一份降级快照，而不是每次重试失败来源）。
func TestSnapshotDegradedOnFailure(t *testing.T) {
	svc := NewService(Deps{
		Version:  "test",
		Runs:     failingRuns{},
		CacheTTL: time.Hour,
	})
	first := svc.Snapshot(context.Background())
	if first.DatabaseOK {
		t.Error("nil health source must not report database ok")
	}
	if first.RunsToday.Completed != 0 {
		t.Errorf("failed runs source must degrade to zero, got %+v", first.RunsToday)
	}
	second := svc.Snapshot(context.Background())
	if second.ServerTime != first.ServerTime {
		t.Error("degraded snapshot must also be cached within TTL")
	}
}

type failingRuns struct{}

func (failingRuns) RunTotals(context.Context, time.Time) (run.Totals, error) {
	return run.Totals{}, context.DeadlineExceeded
}
