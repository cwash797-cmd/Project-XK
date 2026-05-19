// Package tests contains end-to-end integration tests for ktalk-core.
//
// # Scenario
//
// TestEchoTunnel — "nc localhost 2222 → echo-server on peer" (Sprint 1):
//  1. An in-process TCP echo server starts on a random port.
//  2. Two Pion PeerConnections are looped back in-process (no real network).
//  3. A muxer.Session sits on each DC end (same AES-256-GCM key).
//  4. Side A opens a stream; side B receives it via OnIncomingStream callback,
//     dials the echo server, signals readiness, then pipes data bidirectionally.
//  5. A writes bytes → B → echo server → echo mirrors → B → A. Round-trip OK.
//
// TestMultiStreamEcho — five concurrent streams over one DataChannel.
//
// TestKeyRotationLive — RotateKey() mid-session; post-rotation data still flows.
//
// All tests are purely in-process and -short safe.
package tests

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/private/ktalk-core/internal/crypto"
	"github.com/private/ktalk-core/internal/muxer"
)

const (
	testHexKey   = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	setupTimeout = 15 * time.Second
)

// ─── echo server ──────────────────────────────────────────────────────────────

// startEchoServer starts a TCP echo server on a random loopback port.
func startEchoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) //nolint:errcheck
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// ─── loopback pair ────────────────────────────────────────────────────────────

// loopbackPair creates two muxer.Sessions wired over an in-process Pion loopback.
// proxyFn is invoked in a goroutine for each incoming stream on the B side.
func loopbackPair(t *testing.T, hexKey string, proxyFn func(*muxer.Stream)) *muxer.Session {
	t.Helper()

	cA, err := crypto.NewFromHex(hexKey)
	if err != nil {
		t.Fatalf("cipherA: %v", err)
	}
	cB, err := crypto.NewFromHex(hexKey)
	if err != nil {
		t.Fatalf("cipherB: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	api := webrtc.NewAPI()
	pc1, _ := api.NewPeerConnection(webrtc.Configuration{})
	pc2, _ := api.NewPeerConnection(webrtc.Configuration{})

	t.Cleanup(func() { pc1.Close(); pc2.Close() }) //nolint:errcheck

	sessACh := make(chan *muxer.Session, 1)

	// pc1 (offerer): creates DataChannel → sessA.
	dc1, err := pc1.CreateDataChannel("data", &webrtc.DataChannelInit{Ordered: boolp(true)})
	if err != nil {
		t.Fatalf("create dc: %v", err)
	}
	dc1.OnOpen(func() {
		sess := muxer.NewSession(
			func(b []byte) error { return dc1.Send(b) },
			func() uint64 { return dc1.BufferedAmount() },
			cA, log,
		)
		sessACh <- sess
		dc1.OnMessage(func(m webrtc.DataChannelMessage) { sess.Deliver(m.Data) })
	})

	// pc2 (answerer): receives mirrored DC → sessB.
	pc2.OnDataChannel(func(dc2 *webrtc.DataChannel) {
		dc2.OnOpen(func() {
			sess := muxer.NewSession(
				func(b []byte) error { return dc2.Send(b) },
				func() uint64 { return dc2.BufferedAmount() },
				cB, log,
			)
			sess.SetOnIncomingStream(func(st *muxer.Stream) {
				go proxyFn(st)
			})
			dc2.OnMessage(func(m webrtc.DataChannelMessage) { sess.Deliver(m.Data) })
		})
	})

	// In-process SDP exchange (no real network).
	offer, _ := pc1.CreateOffer(nil)
	_ = pc1.SetLocalDescription(offer)
	gatherDone(t, pc1)
	_ = pc2.SetRemoteDescription(*pc1.LocalDescription())
	answer, _ := pc2.CreateAnswer(nil)
	_ = pc2.SetLocalDescription(answer)
	gatherDone(t, pc2)
	_ = pc1.SetRemoteDescription(*pc2.LocalDescription())

	select {
	case sess := <-sessACh:
		return sess
	case <-time.After(setupTimeout):
		t.Fatal("timeout waiting for sessA")
		return nil
	}
}

func gatherDone(t *testing.T, pc *webrtc.PeerConnection) {
	t.Helper()
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
	case <-time.After(setupTimeout):
		t.Fatal("ICE gathering timeout")
	}
}

func boolp(b bool) *bool { return &b }

// ─── echo proxy ───────────────────────────────────────────────────────────────

// echoProxy dials st.Target() and pipes bidirectionally, simulating the Joiner.
// ready is closed once the TCP connection is established (so caller can send).
func echoProxy(t *testing.T, st *muxer.Stream, ready chan<- struct{}) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", st.Target(), 5*time.Second)
	if err != nil {
		t.Logf("proxy: dial %s: %v", st.Target(), err)
		st.Close() //nolint:errcheck
		if ready != nil {
			close(ready)
		}
		return
	}
	if ready != nil {
		close(ready) // signal: TCP up, safe to write from A
	}
	go func() {
		io.Copy(conn, st) //nolint:errcheck
		conn.Close()      //nolint:errcheck
	}()
	io.Copy(st, conn) //nolint:errcheck
	st.Close()        //nolint:errcheck
}

// ─── tests ────────────────────────────────────────────────────────────────────

// TestEchoTunnel is the Sprint 1 acceptance test:
//
//	Initiator opens a tunnel stream → Joiner dials echo server →
//	data flows A→echo→A; verified round-trip.
func TestEchoTunnel(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	// readyCh is closed by echoProxy once TCP to echo server is established.
	readyCh := make(chan struct{})

	sessA := loopbackPair(t, testHexKey, func(st *muxer.Stream) {
		echoProxy(t, st, readyCh)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st, err := sessA.OpenStream(ctx, echoAddr)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	t.Logf("stream %d opened → %s", st.ID(), echoAddr)

	// Wait until B has dialed the echo server before sending data.
	select {
	case <-readyCh:
		t.Log("proxy ready — sending data")
	case <-time.After(8 * time.Second):
		t.Fatal("proxy never became ready")
	}

	want := []byte("hello from ktalk-core integration test")
	if _, err := st.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := mustRead(t, st, len(want), 8*time.Second)
	if string(got) != string(want) {
		t.Fatalf("echo mismatch:\n got  %q\n want %q", got, want)
	}
	t.Logf("✓ echo round-trip: %q", got)
	st.Close() //nolint:errcheck
}

// TestMultiStreamEcho verifies N concurrent streams over one DataChannel.
func TestMultiStreamEcho(t *testing.T) {
	const N = 5

	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	// Each stream gets its own readyCh so we can wait per-stream.
	type entry struct {
		ready chan struct{}
	}
	var mu sync.Mutex
	queue := make([]entry, 0, N)
	cond := sync.NewCond(&mu)

	sessA := loopbackPair(t, testHexKey, func(st *muxer.Stream) {
		ready := make(chan struct{})
		mu.Lock()
		queue = append(queue, entry{ready})
		cond.Broadcast()
		mu.Unlock()
		echoProxy(t, st, ready)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			st, err := sessA.OpenStream(ctx, echoAddr)
			if err != nil {
				t.Errorf("stream %d: OpenStream: %v", i, err)
				return
			}

			// Wait for our queue slot's readyCh.
			var e entry
			deadline := time.Now().Add(10 * time.Second)
			mu.Lock()
			for len(queue) == 0 && time.Now().Before(deadline) {
				cond.Wait()
			}
			if len(queue) > 0 {
				e = queue[0]
				queue = queue[1:]
			}
			mu.Unlock()

			if e.ready == nil {
				t.Errorf("stream %d: no proxy entry", i)
				return
			}
			select {
			case <-e.ready:
			case <-time.After(8 * time.Second):
				t.Errorf("stream %d: proxy ready timeout", i)
				return
			}

			msg := []byte(fmt.Sprintf("concurrent-stream-%d-payload", i))
			if _, err := st.Write(msg); err != nil {
				t.Errorf("stream %d: Write: %v", i, err)
				return
			}
			got := mustRead(t, st, len(msg), 8*time.Second)
			if string(got) != string(msg) {
				t.Errorf("stream %d echo mismatch:\n got  %q\n want %q", i, got, msg)
			}
			st.Close() //nolint:errcheck
		}(i)
	}
	wg.Wait()
	t.Logf("✓ all %d concurrent streams passed", N)
}

// TestKeyRotationLive verifies E2E key rotation mid-session.
func TestKeyRotationLive(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	readyCh := make(chan chan struct{}, 4) // buffered: multiple streams
	sessA := loopbackPair(t, testHexKey, func(st *muxer.Stream) {
		r := make(chan struct{})
		readyCh <- r // pass channel to test goroutine
		echoProxy(t, st, r)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	doRoundTrip := func(label string) {
		t.Helper()
		st, err := sessA.OpenStream(ctx, echoAddr)
		if err != nil {
			t.Fatalf("[%s] OpenStream: %v", label, err)
		}
		// Wait for proxy ready
		var r chan struct{}
		select { //nolint:gosimple
		case r = <-readyCh:
		case <-time.After(8 * time.Second):
			t.Fatalf("[%s] proxy ready channel timeout", label)
		}
		select {
		case <-r:
		case <-time.After(8 * time.Second):
			t.Fatalf("[%s] proxy dial timeout", label)
		}

		msg := []byte(label + ": round-trip payload")
		if _, err := st.Write(msg); err != nil {
			t.Fatalf("[%s] Write: %v", label, err)
		}
		got := mustRead(t, st, len(msg), 8*time.Second)
		if string(got) != string(msg) {
			t.Fatalf("[%s] echo mismatch: %q vs %q", label, got, msg)
		}
		st.Close() //nolint:errcheck
		t.Logf("✓ [%s] round-trip OK", label)
	}

	doRoundTrip("pre-rotation")

	newKey, err := sessA.RotateKey()
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	t.Logf("key rotated → %s…", newKey[:16])
	time.Sleep(200 * time.Millisecond) // let B process CmdKeyRotate

	doRoundTrip("post-rotation")
}

// ─── helper ───────────────────────────────────────────────────────────────────

// mustRead reads exactly wantLen bytes from r within timeout.
func mustRead(t *testing.T, r io.Reader, wantLen int, timeout time.Duration) []byte {
	t.Helper()
	buf := make([]byte, wantLen)
	ch := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, buf)
		ch <- err
	}()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return buf
	case <-time.After(timeout):
		t.Fatalf("read timeout after %v (want %d bytes)", timeout, wantLen)
		return nil
	}
}
