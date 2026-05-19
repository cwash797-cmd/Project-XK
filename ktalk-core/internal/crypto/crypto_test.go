package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestSealOpen_RoundTrip(t *testing.T) {
	c := newTestCipher(t)
	plain := []byte("hello, DataChannel")
	ct := c.Seal(plain)
	seq := c.NextRecvSeq()
	got, err := c.Open(seq, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(plain, got) {
		t.Fatalf("plaintext mismatch: want %q got %q", plain, got)
	}
}

func TestSealOpen_MultipleFrames(t *testing.T) {
	c := newTestCipher(t)
	messages := [][]byte{
		[]byte("frame 0"),
		[]byte("frame 1 — longer payload here"),
		[]byte("frame 2"),
	}
	var ciphertexts [][]byte
	for _, m := range messages {
		ciphertexts = append(ciphertexts, c.Seal(m))
	}
	for i, ct := range ciphertexts {
		seq := c.NextRecvSeq()
		got, err := c.Open(seq, ct)
		if err != nil {
			t.Fatalf("Open[%d]: %v", i, err)
		}
		if !bytes.Equal(messages[i], got) {
			t.Fatalf("message[%d] mismatch", i)
		}
	}
}

func TestOpen_TamperedCiphertext(t *testing.T) {
	c := newTestCipher(t)
	ct := c.Seal([]byte("secret"))
	ct[0] ^= 0xFF // tamper
	if _, err := c.Open(0, ct); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}

func TestNewFromHex_Valid(t *testing.T) {
	key := make([]byte, KeySize)
	_, _ = rand.Read(key)
	hexKey := hex.EncodeToString(key)
	if _, err := NewFromHex(hexKey); err != nil {
		t.Fatalf("NewFromHex: %v", err)
	}
}

func TestNewFromHex_Invalid(t *testing.T) {
	if _, err := NewFromHex("tooshort"); err == nil {
		t.Fatal("expected error for short key")
	}
}
