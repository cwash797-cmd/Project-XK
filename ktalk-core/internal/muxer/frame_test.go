package muxer

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeFrame_RoundTrip(t *testing.T) {
	payload := []byte("hello encrypted payload")
	raw, err := EncodeFrame(42, CmdData, 7, payload)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	f, err := DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}

	if f.StreamID != 42 {
		t.Errorf("StreamID: want 42 got %d", f.StreamID)
	}
	if f.Cmd != CmdData {
		t.Errorf("Cmd: want DATA got %v", f.Cmd)
	}
	if f.Seq != 7 {
		t.Errorf("Seq: want 7 got %d", f.Seq)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Errorf("Payload mismatch: want %q got %q", payload, f.Payload)
	}
}

func TestEncodeFrame_PaddingVaries(t *testing.T) {
	seen := make(map[uint16]struct{})
	for i := 0; i < 200; i++ {
		raw, err := EncodeFrame(1, CmdData, 0, []byte("x"))
		if err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		f, err := DecodeFrame(raw)
		if err != nil {
			t.Fatalf("DecodeFrame: %v", err)
		}
		seen[f.PaddingLen] = struct{}{}
	}
	if len(seen) < 5 {
		t.Fatalf("padding not varying enough: only %d distinct values in 200 frames", len(seen))
	}
}

func TestDecodeFrame_TooShort(t *testing.T) {
	if _, err := DecodeFrame([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected error for too-short frame")
	}
}

func TestCmdString(t *testing.T) {
	cases := map[Cmd]string{
		CmdOpen:  "OPEN",
		CmdData:  "DATA",
		CmdClose: "CLOSE",
		CmdRST:   "RST",
		CmdPing:  "PING",
		CmdPong:  "PONG",
	}
	for cmd, want := range cases {
		if got := cmd.String(); got != want {
			t.Errorf("Cmd(%d).String() = %q, want %q", cmd, got, want)
		}
	}
}
