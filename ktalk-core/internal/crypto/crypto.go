// Package crypto provides the AEAD encryption layer for DataChannel frames.
// ChaCha20-Poly1305 is used with a monotonically-increasing per-direction nonce.
// The nonce is the 64-bit sequence number packed into a 12-byte nonce
// (8 bytes of sequence + 4 bytes of zero padding at the front).
package crypto

import (
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// Overhead is the number of bytes added per encrypted frame (AEAD tag).
	Overhead = chacha20poly1305.Overhead
	// NonceSize is the AEAD nonce length.
	NonceSize = chacha20poly1305.NonceSize
	// KeySize is the required key length in bytes.
	KeySize = chacha20poly1305.KeySize
)

// Cipher wraps a ChaCha20-Poly1305 AEAD with per-direction sequence counters.
type Cipher struct {
	aead    cipher.AEAD
	sendSeq atomic.Uint64
	recvSeq atomic.Uint64
}

// New creates a Cipher from a 32-byte raw key.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create chacha20poly1305: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// NewFromHex parses a 64-character hex string and calls New.
func NewFromHex(hexKey string) (*Cipher, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode hex key: %w", err)
	}
	return New(raw)
}

// Seal encrypts and authenticates plaintext.
// The sequence number is incremented atomically before sealing.
// Returns the ciphertext (with AEAD tag) and the sequence number used,
// which must be stored in the frame header so the receiver can derive the nonce.
func (c *Cipher) Seal(plaintext []byte) (ciphertext []byte, seq uint64) {
	seq = c.sendSeq.Add(1) - 1
	nonce := seqToNonce(seq)
	ciphertext = c.aead.Seal(nil, nonce[:], plaintext, nil)
	return
}

// Open decrypts and verifies ciphertext sealed with Seal.
// The sequence number must be provided by the caller (from the frame header).
// Returns the plaintext or an error if authentication fails.
func (c *Cipher) Open(seq uint64, ciphertext []byte) ([]byte, error) {
	nonce := seqToNonce(seq)
	plain, err := c.aead.Open(nil, nonce[:], ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: open seq=%d: %w", seq, err)
	}
	return plain, nil
}

// NextRecvSeq returns and atomically increments the receive sequence counter.
// Call this once per received frame to maintain ordering.
func (c *Cipher) NextRecvSeq() uint64 {
	return c.recvSeq.Add(1) - 1
}

// seqToNonce packs a uint64 sequence number into a 12-byte ChaCha20 nonce.
// Layout: [4 bytes zero][8 bytes seq big-endian]
func seqToNonce(seq uint64) [NonceSize]byte {
	var nonce [NonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)
	return nonce
}
