// Package muxer (continued) — session-level multiplexer over a single DataChannel.
package muxer

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/crypto"
)

// cryptoRand wraps rand.Read so we can reference it without colliding with
// the crypto/rand import name used in frame.go.
var cryptoRand = rand.Read

const (
	// PingInterval is how often keepalive pings are sent.
	PingInterval = 15 * time.Second
	// PingTimeout is how long to wait for a pong before declaring the channel dead.
	PingTimeout = 30 * time.Second
	// BackpressureThreshold is the DC buffered-amount at which we pause reading.
	BackpressureThreshold uint64 = 512 * 1024 // 512 KiB
	// BackpressureLowWater is the level at which we resume reading.
	BackpressureLowWater uint64 = 64 * 1024 // 64 KiB
)

// SendFunc is the function used to send raw bytes over a DataChannel.
type SendFunc func([]byte) error

// BufferedAmountFunc returns the current buffered amount of the DataChannel.
type BufferedAmountFunc func() uint64

// Stream is a single logical TCP connection multiplexed over the DataChannel.
type Stream struct {
	id      uint32
	target  string // remote addr:port requested by OpenStream
	recvCh  chan []byte
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	session *Session // back-reference for SendData
}

// ID returns the stream identifier.
func (s *Stream) ID() uint32 { return s.id }

// Target returns the remote addr:port this stream was opened for.
func (s *Stream) Target() string { return s.target }

// Read blocks until data is available or the stream is closed.
// It implements io.Reader so the stream can be used with io.Copy.
func (s *Stream) Read(buf []byte) (int, error) {
	select {
	case data, ok := <-s.recvCh:
		if !ok {
			return 0, fmt.Errorf("stream %d: closed", s.id)
		}
		n := copy(buf, data)
		return n, nil
	case <-s.ctx.Done():
		return 0, s.ctx.Err()
	}
}

// SendData sends a DATA frame for this stream via the owning Session.
// It implements io.Writer semantics for use with io.Copy.
func (s *Stream) SendData(data []byte) error {
	if s.session == nil {
		return fmt.Errorf("stream %d: no session", s.id)
	}
	return s.session.SendData(s.id, data)
}

// Write implements io.Writer by calling SendData.
func (s *Stream) Write(p []byte) (int, error) {
	if err := s.SendData(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close signals that this stream is done and sends a CLOSE frame.
func (s *Stream) Close() error {
	s.mu.Lock()
	already := s.closed
	if !already {
		s.closed = true
		s.cancel()
	}
	s.mu.Unlock()
	if !already && s.session != nil {
		_ = s.session.CloseStream(s.id)
	}
	return nil
}

// Session multiplexes multiple logical streams over a single DataChannel.
type Session struct {
	send     SendFunc
	buffered BufferedAmountFunc
	log      *slog.Logger

	cipherMu sync.RWMutex
	cipher   *crypto.Cipher

	mu           sync.RWMutex
	streams      map[uint32]*Stream
	nextStreamID atomic.Uint32

	lastPong atomic.Int64 // unix nano
	closed   atomic.Bool

	// onKeyRotate is called when a CmdKeyRotate frame is received.
	// The caller can use it to persist the new key.
	onKeyRotate func(newHexKey string)

	// onIncomingStream is called when the peer opens a new stream (CmdOpen).
	// The callee is responsible for serving the stream (dial target, pipe data).
	onIncomingStream func(st *Stream)
}

// NewSession creates a new multiplexer session.
func NewSession(send SendFunc, buffered BufferedAmountFunc, c *crypto.Cipher, log *slog.Logger) *Session {
	s := &Session{
		send:     send,
		buffered: buffered,
		cipher:   c,
		log:      log,
		streams:  make(map[uint32]*Stream),
	}
	s.lastPong.Store(time.Now().UnixNano())
	return s
}

// SetOnKeyRotate registers a callback invoked when the peer sends a key rotation frame.
func (s *Session) SetOnKeyRotate(fn func(newHexKey string)) {
	s.onKeyRotate = fn
}

// SetOnIncomingStream registers a callback invoked when the peer opens a new stream.
// The callback is called in a new goroutine for each incoming CmdOpen frame.
// Use this on the Joiner (responder) side to accept tunnel connections.
func (s *Session) SetOnIncomingStream(fn func(st *Stream)) {
	s.onIncomingStream = fn
}

// RotateKey generates a new random 32-byte key, sends CmdKeyRotate to the peer,
// and immediately switches the local cipher to the new key.
// The new hex key is returned so the caller can persist it.
func (s *Session) RotateKey() (string, error) {
	newKey := make([]byte, crypto.KeySize)
	if _, err := cryptoRand(newKey); err != nil {
		return "", fmt.Errorf("session: generate new key: %w", err)
	}
	newCipher, err := crypto.New(newKey)
	if err != nil {
		return "", fmt.Errorf("session: build new cipher: %w", err)
	}

	// Send the raw key as payload (sealed with the OLD cipher — last frame on old key).
	if err := s.sendFrame(0, CmdKeyRotate, 0, newKey); err != nil {
		return "", fmt.Errorf("session: send key rotate frame: %w", err)
	}

	// Switch local cipher.
	s.cipherMu.Lock()
	s.cipher = newCipher
	s.cipherMu.Unlock()

	hexKey := fmt.Sprintf("%x", newKey)
	s.log.Info("key rotated (initiator)")
	return hexKey, nil
}

// OpenStream allocates a new stream ID and sends a CmdOpen frame.
func (s *Session) OpenStream(ctx context.Context, hostPort string) (*Stream, error) {
	id := s.nextStreamID.Add(1)
	st := s.newStream(ctx, id)

	if err := s.sendFrame(id, CmdOpen, 0, []byte(hostPort)); err != nil {
		st.Close()
		return nil, fmt.Errorf("session: open stream %d: %w", id, err)
	}
	s.log.Debug("stream opened", "stream_id", id, "target", hostPort)
	return st, nil
}

// Deliver is called by the carrier when a raw DataChannel message arrives.
func (s *Session) Deliver(raw []byte) {
	f, err := DecodeFrame(raw)
	if err != nil {
		s.log.Warn("decode frame failed", "err", err)
		return
	}

	// Decrypt payload using current cipher.
	var plaintext []byte
	if len(f.Payload) > 0 {
		s.cipherMu.RLock()
		c := s.cipher
		s.cipherMu.RUnlock()
		plaintext, err = c.Open(f.Seq, f.Payload)
		if err != nil {
			s.log.Warn("decrypt frame failed", "stream_id", f.StreamID, "seq", f.Seq, "err", err)
			return
		}
	}

	switch f.Cmd {
	case CmdKeyRotate:
		// Peer initiated key rotation — plaintext is the new 32-byte raw key.
		if len(plaintext) != crypto.KeySize {
			s.log.Warn("key rotate: bad payload length", "len", len(plaintext))
			return
		}
		newCipher, err := crypto.New(plaintext)
		if err != nil {
			s.log.Warn("key rotate: build cipher", "err", err)
			return
		}
		s.cipherMu.Lock()
		s.cipher = newCipher
		s.cipherMu.Unlock()
		hexKey := fmt.Sprintf("%x", plaintext)
		s.log.Info("key rotated (responder)")
		if s.onKeyRotate != nil {
			go s.onKeyRotate(hexKey)
		}

	case CmdPing:
		_ = s.sendFrame(0, CmdPong, 0, plaintext)
	case CmdPong:
		s.lastPong.Store(time.Now().UnixNano())
		if len(plaintext) == 8 {
			sentAt := int64(binary.BigEndian.Uint64(plaintext))
			rtt := time.Since(time.Unix(0, sentAt))
			s.log.Debug("pong received", "rtt", rtt)
		}
	case CmdOpen:
		// Responder side: peer opened a new stream.
		target := string(plaintext)
		st := s.newStreamWithTarget(context.Background(), f.StreamID, target)
		s.log.Debug("incoming stream", "stream_id", f.StreamID, "target", target)
		if s.onIncomingStream != nil {
			go s.onIncomingStream(st)
		}
	case CmdData:
		s.deliverData(f.StreamID, plaintext)
	case CmdClose, CmdRST:
		s.closeStream(f.StreamID)
	}
}

// SendData sends a DATA frame for an existing stream.
func (s *Session) SendData(streamID uint32, data []byte) error {
	return s.sendFrame(streamID, CmdData, 0, data)
}

// CloseStream sends a CLOSE frame and removes the stream from the map.
func (s *Session) CloseStream(streamID uint32) error {
	if err := s.sendFrame(streamID, CmdClose, 0, nil); err != nil {
		return err
	}
	s.closeStream(streamID)
	return nil
}

// RunKeepalive starts the PING loop. Call in a goroutine.
func (s *Session) RunKeepalive(ctx context.Context) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.closed.Load() {
				return
			}
			// Check last pong
			last := time.Unix(0, s.lastPong.Load())
			if time.Since(last) > PingTimeout {
				s.log.Warn("keepalive timeout — channel appears dead")
				return
			}
			ts := make([]byte, 8)
			binary.BigEndian.PutUint64(ts, uint64(time.Now().UnixNano()))
			if err := s.sendFrame(0, CmdPing, 0, ts); err != nil {
				s.log.Warn("ping send failed", "err", err)
			}
		}
	}
}

// CloseAll closes all streams and marks the session as closed.
func (s *Session) CloseAll() {
	s.closed.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.streams {
		st.Close()
	}
	s.streams = make(map[uint32]*Stream)
}

// --- internal helpers ---

func (s *Session) newStream(ctx context.Context, id uint32) *Stream {
	return s.newStreamWithTarget(ctx, id, "")
}

func (s *Session) newStreamWithTarget(ctx context.Context, id uint32, target string) *Stream {
	sCtx, cancel := context.WithCancel(ctx)
	st := &Stream{
		id:      id,
		target:  target,
		recvCh:  make(chan []byte, 64),
		ctx:     sCtx,
		cancel:  cancel,
		session: s,
	}
	s.mu.Lock()
	s.streams[id] = st
	s.mu.Unlock()
	return st
}

func (s *Session) deliverData(streamID uint32, data []byte) {
	s.mu.RLock()
	st, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case st.recvCh <- data:
	default:
		s.log.Warn("stream recv buffer full, dropping data", "stream_id", streamID)
	}
}

func (s *Session) closeStream(streamID uint32) {
	s.mu.Lock()
	st, ok := s.streams[streamID]
	if ok {
		delete(s.streams, streamID)
	}
	s.mu.Unlock()
	if ok {
		st.Close()
	}
}

func (s *Session) sendFrame(streamID uint32, cmd Cmd, seq uint64, plaintext []byte) error {
	// Backpressure check
	if s.buffered != nil && s.buffered() > BackpressureThreshold {
		return fmt.Errorf("muxer: backpressure: buffered amount too high")
	}

	var encrypted []byte
	var usedSeq uint64
	if len(plaintext) > 0 {
		s.cipherMu.RLock()
		encrypted, usedSeq = s.cipher.Seal(plaintext)
		s.cipherMu.RUnlock()
	}

	raw, err := EncodeFrame(streamID, cmd, usedSeq, encrypted)
	if err != nil {
		return err
	}
	return s.send(raw)
}
