// Package loadtest — benchmark and integration tests.
//
// Run:
//
//	go test ./loadtest -v -timeout 300s -run TestLoadSmoke
//	go test ./loadtest -v -timeout 300s -run TestLoad100
//	go test ./loadtest -bench=BenchmarkMuxerThroughput -benchtime=30s
package loadtest

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// TestLoadSmoke runs 5 sessions as a quick sanity check.
func TestLoadSmoke(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sessions = 5
	cfg.BytesPerSession = 64 * 1024 // 64 KiB per session
	cfg.Timeout = 60 * time.Second

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("load test error: %v", err)
	}

	t.Logf("result: %s", result)

	if result.Failed > 0 {
		t.Errorf("got %d failed sessions (want 0)", result.Failed)
	}
	if result.Completed != cfg.Sessions {
		t.Errorf("got %d completed sessions (want %d)", result.Completed, cfg.Sessions)
	}
	if result.ThroughputMBps < 0.1 {
		t.Errorf("throughput too low: %.3f MB/s (want ≥ 0.1)", result.ThroughputMBps)
	}
}

// TestLoad100 runs the full 100-session load test.
// This test takes ~60-120 seconds and is skipped in short mode.
func TestLoad100(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100-session load test in short mode")
	}

	cfg := DefaultConfig()
	cfg.Sessions = 100
	cfg.BytesPerSession = 1 * 1024 * 1024 // 1 MiB per session
	cfg.Timeout = 300 * time.Second

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Record baseline memory
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	result, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("load test error: %v", err)
	}

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	peakMB := float64(memAfter.HeapInuse-memBefore.HeapInuse) / (1024 * 1024)
	result.PeakMemoryMB = peakMB

	t.Logf("=== LOAD TEST RESULTS ===")
	t.Logf("%s", result)
	t.Logf("peak heap delta: %.1f MB", peakMB)
	t.Logf("goroutines at end: %d", runtime.NumGoroutine())

	// Acceptance criteria:
	maxFailRate := 0.05 // allow up to 5% failure rate (network flakiness in CI)
	failRate := float64(result.Failed) / float64(cfg.Sessions)
	if failRate > maxFailRate {
		t.Errorf("fail rate %.1f%% exceeds threshold %.1f%%",
			failRate*100, maxFailRate*100)
	}

	if result.ThroughputMBps < 10.0 {
		t.Errorf("aggregate throughput %.2f MB/s below 10 MB/s threshold", result.ThroughputMBps)
	}

	if result.P99LatencyMicro > 50_000 { // 50ms p99 for loopback
		t.Errorf("p99 send latency %dµs exceeds 50ms threshold", result.P99LatencyMicro)
	}

	// Memory: 100 sessions should fit in <512 MB heap delta
	if peakMB > 512 {
		t.Errorf("peak heap delta %.1f MB exceeds 512 MB limit", peakMB)
	}
}

// BenchmarkMuxerThroughput measures single-session DataChannel throughput.
func BenchmarkMuxerThroughput(b *testing.B) {
	cfg := DefaultConfig()
	cfg.Sessions = 1
	cfg.BytesPerSession = int64(b.N) * 4096
	cfg.ChunkSize = 4096
	cfg.Timeout = 10 * time.Minute

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	b.SetBytes(4096)
	b.ResetTimer()

	result, err := Run(context.Background(), cfg, log)
	if err != nil {
		b.Fatalf("bench error: %v", err)
	}
	if result.Failed > 0 {
		b.Fatalf("session failed")
	}
}
