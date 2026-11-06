package platform

// 签名摘要热路径基准：BodyHash 在动态入口每个请求的 canonical 串与幂等
// 记录键上各执行一次；SignBody 在 push 签名校验执行一次。
import "testing"

var benchBody = []byte(`{"schemaVersion":"1.0","runId":"run-bench","templateInputData":[{"name":"topic","value":"benchmark"}]}`)

func BenchmarkBodyHash(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = BodyHash(benchBody)
	}
}

func BenchmarkSignBody(b *testing.B) {
	secret := []byte("bench-signing-secret-0123456789")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = SignBody(secret, benchBody)
	}
}
