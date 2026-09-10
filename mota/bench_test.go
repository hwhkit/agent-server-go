package mota

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

func BenchmarkSignature(b *testing.B) {
	body := bytes.Repeat([]byte{'x'}, 512)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ComputeSignature("bench-secret", body, "hex")
	}
}

func BenchmarkVerifySignature(b *testing.B) {
	body := bytes.Repeat([]byte{'x'}, 512)
	sig := ComputeSignature("bench-secret", body, "hex")
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if !VerifySignature("bench-secret", body, sig, "hex") {
			b.Fatal("unexpected mismatch")
		}
	}
}

func BenchmarkPushHandler(b *testing.B) {
	cfg := testConfig()
	cfg.SigningSecret = "bench-secret"
	r := chi.NewRouter()
	r.Post(cfg.PushPath, NewPushHandler(cfg, PlaceholderProcessor{}))
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := []byte(pushTaskBody("bench"))
	sig := ComputeSignature("bench-secret", body, "hex")

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL+cfg.PushPath, bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SignatureHeader, sig)
		req.Header.Set(RunIDHeader, "bench")
		req.Header.Set(IdempotencyKeyHeader, fmt.Sprintf("bench-%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	}
}

func BenchmarkPushHandlerConcurrent(b *testing.B) {
	cfg := testConfig()
	cfg.SigningSecret = "bench-secret"
	r := chi.NewRouter()
	r.Post(cfg.PushPath, NewPushHandler(cfg, PlaceholderProcessor{}))
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := []byte(pushTaskBody("bench"))
	sig := ComputeSignature("bench-secret", body, "hex")

	client := &http.Client{}
	var counter atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, err := http.NewRequest(http.MethodPost, srv.URL+cfg.PushPath, bytes.NewReader(body))
			if err != nil {
				b.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(SignatureHeader, sig)
			req.Header.Set(RunIDHeader, "bench")
			req.Header.Set(IdempotencyKeyHeader, fmt.Sprintf("bench-%d", counter.Add(1)))
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b.Fatalf("expected 200, got %d", resp.StatusCode)
			}
		}
	})
}
