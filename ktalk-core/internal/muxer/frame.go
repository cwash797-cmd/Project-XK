// Package muxer implements the DataChannel framing protocol.
//
// Frame layout over a WebRTC DataChannel:
//
//	┌────────────────────────────────────────────────────────────┐
//	│ u32  stream_id   (4 bytes, big-endian)                     │
//	│ u8   cmd         (1 byte)                                  │
//	│ u64  seq         (8 bytes, big-endian) — AEAD nonce input  │
//	│ u16  padding_len (2 bytes, big-endian)                     │
//	│ u16  payload_len (2 bytes, big-endian)                     │
//	│ ...  padding     (padding_len bytes, random)               │
//	│ ...  payload     (payload_len bytes, AEAD-encrypted)       │
//	└────────────────────────────────────────────────────────────┘
//
// The payload field contains AEAD-sealed bytes. The AEAD nonce is derived
// from the seq field. Padding is never encrypted — it is pure random noise
// whose sole purpose is to perturb the packet-size distribution.
package muxer

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// Cmd is a single-byte command identifier.
type Cmd byte

const (
	// CmdOpen opens a new logical stream. Payload is "host:port".
	CmdOpen Cmd = 0x01
	// CmdData carries proxied TCP data for an existing stream.
	CmdData Cmd = 0x02
	// CmdClose gracefully closes a stream.
	CmdClose Cmd = 0x03
	// CmdRST forcefully resets a stream.
	CmdRST Cmd = 0x04
	// CmdPing is a keepalive ping. Payload is an 8-byte timestamp (ns).
	CmdPing Cmd = 0x05
	// CmdPong is the response to a ping. Payload mirrors the ping payload.
	CmdPong Cmd = 0x06
	// CmdKeyRotate signals a key rotation. Payload is the new 32-byte raw key.
	// The sender switches to the new key immediately after sending this frame.
	// The receiver must switch to the new key before sending any reply.
	CmdKeyRotate Cmd = 0x07
)

// HeaderSize is the number of bytes in the fixed-size frame header.
const HeaderSize = 4 + 1 + 8 + 2 + 2 // stream_id + cmd + seq + padding_len + payload_len

// MaxPadding is the maximum random padding per frame (bytes).
// Chosen to effectively smear DPI packet-size histograms.
const MaxPadding = 1024

// Frame is a decoded DataChannel frame.
type Frame struct {
	StreamID   uint32
	Cmd        Cmd
	Seq        uint64
	PaddingLen uint16
	Payload    []byte // plaintext after AEAD decryption
}

// String returns a human-readable representation for logging.
func (f *Frame) String() string {
	return fmt.Sprintf("Frame{stream=%d cmd=%s seq=%d payload=%dB}",
		f.StreamID, f.Cmd, f.Seq, len(f.Payload))
}

func (c Cmd) String() string {
	switch c {
	case CmdOpen:
		return "OPEN"
	case CmdData:
		return "DATA"
	case CmdClose:
		return "CLOSE"
	case CmdRST:
		return "RST"
	case CmdPing:
		return "PING"
	case CmdPong:
		return "PONG"
	case CmdKeyRotate:
		return "KEY_ROTATE"
	default:
		return fmt.Sprintf("CMD(0x%02x)", byte(c))
	}
}

// EncodeFrame serialises a Frame into a byte slice ready to send over DC.
// The payload is expected to already be AEAD-encrypted by the caller.
// Random padding of 0…MaxPadding bytes is added automatically.
func EncodeFrame(streamID uint32, cmd Cmd, seq uint64, encryptedPayload []byte) ([]byte, error) {
	paddingLen, err := randomPaddingLen()
	if err != nil {
		return nil, fmt.Errorf("muxer: random padding: %w", err)
	}
	padding := make([]byte, paddingLen)
	if _, err := rand.Read(padding); err != nil {
		return nil, fmt.Errorf("muxer: fill padding: %w", err)
	}

	payloadLen := len(encryptedPayload)
	if payloadLen > 0xFFFF {
		return nil, fmt.Errorf("muxer: payload too large: %d bytes", payloadLen)
	}

	buf := make([]byte, HeaderSize+int(paddingLen)+payloadLen)
	off := 0

	binary.BigEndian.PutUint32(buf[off:], streamID)
	off += 4
	buf[off] = byte(cmd)
	off++
	binary.BigEndian.PutUint64(buf[off:], seq)
	off += 8
	binary.BigEndian.PutUint16(buf[off:], paddingLen)
	off += 2
	binary.BigEndian.PutUint16(buf[off:], uint16(payloadLen))
	off += 2

	copy(buf[off:], padding)
	off += int(paddingLen)
	copy(buf[off:], encryptedPayload)

	return buf, nil
}

// DecodeFrame parses a byte slice into a Frame.
// The payload is still AEAD-encrypted; the caller must decrypt it.
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("muxer: frame too short: %d bytes", len(data))
	}

	off := 0
	streamID := binary.BigEndian.Uint32(data[off:])
	off += 4
	cmd := Cmd(data[off])
	off++
	seq := binary.BigEndian.Uint64(data[off:])
	off += 8
	paddingLen := binary.BigEndian.Uint16(data[off:])
	off += 2
	payloadLen := binary.BigEndian.Uint16(data[off:])
	off += 2

	required := HeaderSize + int(paddingLen) + int(payloadLen)
	if len(data) < required {
		return nil, fmt.Errorf("muxer: frame body too short: have %d need %d", len(data), required)
	}

	off += int(paddingLen) // skip padding
	payload := make([]byte, payloadLen)
	copy(payload, data[off:off+int(payloadLen)])

	return &Frame{
		StreamID:   streamID,
		Cmd:        cmd,
		Seq:        seq,
		PaddingLen: paddingLen,
		Payload:    payload,
	}, nil
}

// ReadFrame reads exactly one frame from r (used for streaming, not DC).
func ReadFrame(r io.Reader) (*Frame, error) {
	hdr := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("muxer: read header: %w", err)
	}

	off := 0
	streamID := binary.BigEndian.Uint32(hdr[off:])
	off += 4
	cmd := Cmd(hdr[off])
	off++
	seq := binary.BigEndian.Uint64(hdr[off:])
	off += 8
	paddingLen := binary.BigEndian.Uint16(hdr[off:])
	off += 2
	payloadLen := binary.BigEndian.Uint16(hdr[off:])

	body := make([]byte, int(paddingLen)+int(payloadLen))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("muxer: read body: %w", err)
	}

	payload := make([]byte, payloadLen)
	copy(payload, body[paddingLen:])

	return &Frame{
		StreamID:   streamID,
		Cmd:        cmd,
		Seq:        seq,
		PaddingLen: paddingLen,
		Payload:    payload,
	}, nil
}

func randomPaddingLen() (uint16, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]) % (MaxPadding + 1), nil
}
