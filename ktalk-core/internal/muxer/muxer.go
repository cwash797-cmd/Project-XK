// Package muxer (continued) — session-level multiplexer over a single DataChannel.
package muxer

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/private/ktalk-core/internal/crypto"
)

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
	id       uint32
	recvCh   chan []byte
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
}

// Read blocks until data is available or the stream is closed.
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

// Close signals that this stream is done.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
}

// Session multiplexes multiple logical streams over a single DataChannel.
type Session struct {
	send         SendFunc
	buffered     BufferedAmountFunc
	cipher       *crypto.Cipher
	log          *slog.Logger

	mu           sync.RWMutex
	streams      map[uint32]*Stream
	nextStreamID atomic.Uint32

	lastPong   atomic.Int64 // unix nano
	closed     atomic.Bool
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

	// Decrypt payload
	var plaintext []byte
	if len(f.Payload) > 0 {
		plaintext, err = s.cipher.Open(f.Seq, f.Payload)
		if err != nil {
			s.log.Warn("decrypt frame failed", "stream_id", f.StreamID, "seq", f.Seq, "err", err)
			return
		}
	}

	switch f.Cmd {
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
		// Creator side: accept an incoming stream from Joiner.
		st := s.newStream(context.Background(), f.StreamID)
		_ = st
		s.log.Debug("incoming stream", "stream_id", f.StreamID, "target", string(plaintext))
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
	sCtx, cancel := context.WithCancel(ctx)
	st := &Stream{
		id:     id,
		recvCh: make(chan []byte, 64),
		ctx:    sCtx,
		cancel: cancel,
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
	if len(plaintext) > 0 {
		encrypted = s.cipher.Seal(plaintext)
		// Use the send sequence counter from cipher for the frame seq field
	}

	raw, err := EncodeFrame(streamID, cmd, seq, encrypted)
	if err != nil {
		return err
	}
	return s.send(raw)
}
