package dispatch

// 准入闸热路径基准：TryAcquire/Release 是每次执行的必经路径，无锁竞争
// 时的开销决定单实例吞吐上限。
import "testing"

func BenchmarkSemaphoreTryAcquireRelease(b *testing.B) {
	s := newSemaphore(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !s.TryAcquire() {
			b.Fatal("acquire failed with ample capacity")
		}
		s.Release()
	}
}

func BenchmarkSlotRegistryGetOrCreate(b *testing.B) {
	registry := slotRegistry{entries: map[string]*slotEntry{}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		registry.get("bench-agent", 4, 10)
	}
}
