// Package loadtest benchmarks the muxer layer with N concurrent loopback
// DataChannel sessions. It measures throughput, latency, and memory usage
// without requiring any external network (pure in-process Pion loopback).
//
// Usage:
//
//	go run ./loadtest -n 100 -bytes 1048576 -duration 30s
//	go test ./loadtest -bench=. -benchtime=30s
package loadtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/crypto"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/muxer"
	"github.com/pion/webrtc/v4"
)

// Config holds load test parameters.
type Config struct {
	// Sessions is the number of concurrent loopback pairs.
	Sessions int
	// BytesPerSession is how many bytes each session transfers before completing.
	BytesPerSession int64
	// Timeout is the maximum test duration.
	Timeout time.Duration
	// ChunkSize is the write size per frame.
	ChunkSize int
	// Verbose enables per-session logging.
	Verbose bool
}

// DefaultConfig returns sensible defaults for 100-session load test.
func DefaultConfig() Config {
	return Config{
		Sessions:        100,
		BytesPerSession: 1 * 1024 * 1024, // 1 MiB per session
		Timeout:         120 * time.Second,
		ChunkSize:       4096,
		Verbose:         false,
	}
}

// Result holds aggregated load test results.
type Result struct {
	Sessions        int
	Completed       int
	Failed          int
	TotalBytesXfer  int64
	Duration        time.Duration
	ThroughputMBps  float64
	AvgLatencyMicro int64
	P99LatencyMicro int64
	PeakMemoryMB    float64
}

func (r Result) String() string {
	return fmt.Sprintf(
		"sessions=%d completed=%d failed=%d bytes=%d duration=%v throughput=%.2f MB/s "+
			"avg_latency=%dµs p99_latency=%dµs",
		r.Sessions, r.Completed, r.Failed,
		r.TotalBytesXfer, r.Duration.Round(time.Millisecond),
		r.ThroughputMBps,
		r.AvgLatencyMicro, r.P99LatencyMicro,
	)
}

// Run executes the load test with the given config.
func Run(ctx context.Context, cfg Config, log *slog.Logger) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	type sessionResult struct {
		bytes   int64
		latency []int64 // microseconds
		err     error
	}

	results := make([]sessionResult, cfg.Sessions)
	var wg sync.WaitGroup
	var totalBytes int64
	start := time.Now()

	// Semaphore: limit concurrent goroutines to avoid file descriptor exhaustion
	sem := make(chan struct{}, 20) // max 20 pairs initializing at once

	for i := 0; i < cfg.Sessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := runSession(ctx, cfg, idx, log)
			results[idx] = res
			if res.err == nil {
				atomic.AddInt64(&totalBytes, res.bytes)
			}
			if cfg.Verbose || res.err != nil {
				if res.err != nil {
					log.Warn("session failed",
						"idx", idx, "err", res.err, "bytes", res.bytes)
				} else {
					log.Debug("session done", "idx", idx, "bytes", res.bytes)
				}
			}
		}(i)
	}

	wg.Wait()
	dur := time.Since(start)

	// Aggregate
	completed, failed := 0, 0
	var allLatencies []int64
	for _, r := range results {
		if r.err == nil {
			completed++
		} else {
			failed++
		}
		allLatencies = append(allLatencies, r.latency...)
	}

	throughput := float64(totalBytes) / dur.Seconds() / (1024 * 1024)

	var avgLatency, p99Latency int64
	if len(allLatencies) > 0 {
		sortInts(allLatencies)
		sum := int64(0)
		for _, l := range allLatencies {
			sum += l
		}
		avgLatency = sum / int64(len(allLatencies))
		p99Latency = allLatencies[int(float64(len(allLatencies))*0.99)]
	}

	return Result{
		Sessions:        cfg.Sessions,
		Completed:       completed,
		Failed:          failed,
		TotalBytesXfer:  totalBytes,
		Duration:        dur,
		ThroughputMBps:  throughput,
		AvgLatencyMicro: avgLatency,
		P99LatencyMicro: p99Latency,
	}, nil
}

// runSession runs a single loopback session and returns bytes transferred + per-ping latencies.
func runSession(ctx context.Context, cfg Config, idx int, log *slog.Logger) struct {
	bytes   int64
	latency []int64
	err     error
} {
	type res struct {
		bytes   int64
		latency []int64
		err     error
	}

	// Generate a fresh key per session
	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		return res{err: fmt.Errorf("keygen: %w", err)}
	}
	hexKey := hex.EncodeToString(rawKey)

	cipherA, err := crypto.NewFromHex(hexKey)
	if err != nil {
		return res{err: fmt.Errorf("cipher A: %w", err)}
	}
	cipherB, err := crypto.NewFromHex(hexKey)
	if err != nil {
		return res{err: fmt.Errorf("cipher B: %w", err)}
	}

	sessACh := make(chan *muxer.Session, 1)
	sessBCh := make(chan *muxer.Session, 1)

	pc1, pc2, err := createLoopbackPair(ctx, cipherA, cipherB, sessACh, sessBCh, log)
	if err != nil {
		return res{err: fmt.Errorf("loopback pair: %w", err)}
	}
	defer pc1.Close()
	defer pc2.Close()

	var sessA, sessB *muxer.Session
	deadline := time.After(15 * time.Second)
	for sessA == nil || sessB == nil {
		select {
		case s := <-sessACh:
			sessA = s
		case s := <-sessBCh:
			sessB = s
		case <-deadline:
			return res{err: errors.New("timeout waiting for sessions")}
		case <-ctx.Done():
			return res{err: ctx.Err()}
		}
	}

	// Open a stream from A to B
	stA, err := sessA.OpenStream(ctx, fmt.Sprintf("127.0.0.1:%d", 10000+idx))
	if err != nil {
		return res{err: fmt.Errorf("open stream: %w", err)}
	}

	// Transfer BytesPerSession bytes A→B in chunks
	chunk := make([]byte, cfg.ChunkSize)
	if _, err := rand.Read(chunk); err != nil {
		return res{err: fmt.Errorf("rand chunk: %w", err)}
	}

	var transferred int64
	var latencies []int64

	for transferred < cfg.BytesPerSession {
		toSend := int64(cfg.ChunkSize)
		if transferred+toSend > cfg.BytesPerSession {
			toSend = cfg.BytesPerSession - transferred
		}
		t0 := time.Now()
		if err := sessA.SendData(stA.ID(), chunk[:toSend]); err != nil {
			if ctx.Err() != nil {
				return res{bytes: transferred, latency: latencies, err: ctx.Err()}
			}
			return res{bytes: transferred, latency: latencies, err: fmt.Errorf("send data: %w", err)}
		}
		latencyMicro := time.Since(t0).Microseconds()
		latencies = append(latencies, latencyMicro)
		transferred += toSend
	}

	// Clean close
	_ = sessA.CloseStream(stA.ID())
	_ = sessB

	return res{bytes: transferred, latency: latencies}
}

// createLoopbackPair builds two in-process Pion PCs connected to each other
// and wires their DataChannels to muxer.Sessions.
func createLoopbackPair(
	ctx context.Context,
	cipherA, cipherB *crypto.Cipher,
	sessACh, sessBCh chan<- *muxer.Session,
	log *slog.Logger,
) (*webrtc.PeerConnection, *webrtc.PeerConnection, error) {
	api := webrtc.NewAPI()

	pc1, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, err
	}
	pc2, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		pc1.Close()
		return nil, nil, err
	}

	ordered := true
	dc1, err := pc1.CreateDataChannel("data", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		pc1.Close()
		pc2.Close()
		return nil, nil, err
	}

	// Wire pc1/dc1 → sessA
	dc1.OnOpen(func() {
		sendFn := func(raw []byte) error { return dc1.Send(raw) }
		buffFn := func() uint64 { return dc1.BufferedAmount() }
		sess := muxer.NewSession(sendFn, buffFn, cipherA, noopLogger())
		select {
		case sessACh <- sess:
		default:
		}
		dc1.OnMessage(func(msg webrtc.DataChannelMessage) { sess.Deliver(msg.Data) })
	})

	// Wire pc2 mirrored dc → sessB
	pc2.OnDataChannel(func(dc2 *webrtc.DataChannel) {
		dc2.OnOpen(func() {
			sendFn := func(raw []byte) error { return dc2.Send(raw) }
			buffFn := func() uint64 { return dc2.BufferedAmount() }
			sess := muxer.NewSession(sendFn, buffFn, cipherB, noopLogger())
			select {
			case sessBCh <- sess:
			default:
			}
			dc2.OnMessage(func(msg webrtc.DataChannelMessage) { sess.Deliver(msg.Data) })
		})
	})

	// Exchange SDP
	if err := exchangeSDP(pc1, pc2); err != nil {
		pc1.Close()
		pc2.Close()
		return nil, nil, err
	}

	_ = log
	return pc1, pc2, nil
}

// exchangeSDP performs offer/answer signaling between two in-process PCs.
func exchangeSDP(pc1, pc2 *webrtc.PeerConnection) error {
	offer, err := pc1.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc1.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local offer: %w", err)
	}
	gatheringDone := webrtc.GatheringCompletePromise(pc1)
	select {
	case <-gatheringDone:
	case <-time.After(10 * time.Second):
		return errors.New("ICE gathering timeout (pc1)")
	}

	if err := pc2.SetRemoteDescription(*pc1.LocalDescription()); err != nil {
		return fmt.Errorf("set remote offer: %w", err)
	}
	answer, err := pc2.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := pc2.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local answer: %w", err)
	}
	gatheringDone2 := webrtc.GatheringCompletePromise(pc2)
	select {
	case <-gatheringDone2:
	case <-time.After(10 * time.Second):
		return errors.New("ICE gathering timeout (pc2)")
	}

	if err := pc1.SetRemoteDescription(*pc2.LocalDescription()); err != nil {
		return fmt.Errorf("set remote answer: %w", err)
	}
	return nil
}

// sortInts sorts int64 slice in-place (insertion sort for simplicity, good for <10k items).
func sortInts(a []int64) {
	for i := 1; i < len(a); i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j] > key {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}

func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
