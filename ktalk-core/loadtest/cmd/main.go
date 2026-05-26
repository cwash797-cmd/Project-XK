// Command loadtest runs the muxer load test with configurable parameters.
//
// Usage:
//
//	go run ./ktalk-core/loadtest/cmd -n 100 -bytes 1048576 -duration 120s
//	go run ./ktalk-core/loadtest/cmd -n 10 -bytes 65536 -verbose
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/loadtest"
)

func main() {
	n := flag.Int("n", 10, "Number of concurrent sessions")
	bytesPerSession := flag.Int64("bytes", 1*1024*1024, "Bytes per session")
	duration := flag.Duration("duration", 120*time.Second, "Test timeout")
	chunkSize := flag.Int("chunk", 4096, "Write chunk size in bytes")
	verbose := flag.Bool("verbose", false, "Verbose per-session logging")
	logLevel := flag.String("log", "warn", "Log level (debug|info|warn|error)")
	flag.Parse()

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	default:
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg := loadtest.Config{
		Sessions:        *n,
		BytesPerSession: *bytesPerSession,
		Timeout:         *duration,
		ChunkSize:       *chunkSize,
		Verbose:         *verbose,
	}

	fmt.Fprintf(os.Stdout, "Starting load test: sessions=%d bytes_per_session=%d chunk=%d timeout=%v\n",
		cfg.Sessions, cfg.BytesPerSession, cfg.ChunkSize, cfg.Timeout)

	result, err := loadtest.Run(context.Background(), cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("=== LOAD TEST RESULTS ===")
	fmt.Printf("Sessions:         %d\n", result.Sessions)
	fmt.Printf("Completed:        %d\n", result.Completed)
	fmt.Printf("Failed:           %d\n", result.Failed)
	fmt.Printf("Total bytes:      %d (%.1f MB)\n", result.TotalBytesXfer,
		float64(result.TotalBytesXfer)/(1024*1024))
	fmt.Printf("Duration:         %v\n", result.Duration.Round(time.Millisecond))
	fmt.Printf("Throughput:       %.2f MB/s\n", result.ThroughputMBps)
	fmt.Printf("Avg latency:      %dµs\n", result.AvgLatencyMicro)
	fmt.Printf("P99 latency:      %dµs\n", result.P99LatencyMicro)

	if result.Failed > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %d sessions failed\n", result.Failed)
		os.Exit(1)
	}
	fmt.Println("\nPASS")
}
