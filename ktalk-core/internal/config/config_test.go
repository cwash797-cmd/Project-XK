package config_test

import (
	"strings"
	"testing"

	"github.com/private/ktalk-core/internal/config"
)

// baseRoom returns a RoomConfig populated with data confirmed from DevTools 2026-05-19.
func baseRoom() config.RoomConfig {
	return config.RoomConfig{
		Subdomain:    "ilte0310",
		RoomID:       "cb140blkff7i",
		ConferenceID: "cb140blkff7i_3074b65d29905f8e4418e2113a329f487fcadc8e4ed58df7b108624d199a4110",
	}
}

func TestRoomConfig_Domain(t *testing.T) {
	r := baseRoom()
	want := "ilte0310.ktalk.ru"
	if got := r.Domain(); got != want {
		t.Errorf("Domain() = %q, want %q", got, want)
	}
}

func TestRoomConfig_WSSUrl_WithConferenceID(t *testing.T) {
	r := baseRoom()
	got := r.WSSUrl()

	// Confirmed format from DevTools:
	// wss://ilte0310.ktalk.ru/cb140blkff7i/xmpp-websocket?room=cb140blkff7i_3074b65d...
	wantPrefix := "wss://ilte0310.ktalk.ru/cb140blkff7i/xmpp-websocket?room="
	wantSuffix := r.ConferenceID

	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("WSSUrl() = %q\nwant prefix %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("WSSUrl() = %q\nwant suffix (conferenceId) %q", got, wantSuffix)
	}
}

func TestRoomConfig_WSSUrl_FallbackToRoomID(t *testing.T) {
	// When ConferenceID is not set yet (before API fetch), falls back to RoomID.
	r := config.RoomConfig{
		Subdomain: "ilte0310",
		RoomID:    "cb140blkff7i",
		// ConferenceID intentionally empty
	}
	got := r.WSSUrl()
	// Should use roomName as room= param (degraded mode, still correct path)
	if !strings.Contains(got, "/cb140blkff7i/xmpp-websocket?room=cb140blkff7i") {
		t.Errorf("WSSUrl() fallback = %q, expected roomName in both path and room= param", got)
	}
}

func TestRoomConfig_MUCDomain(t *testing.T) {
	r := baseRoom()
	got := r.MUCDomain()
	// Format: conference.<roomName>.<domain>
	want := "conference.cb140blkff7i.ilte0310.ktalk.ru"
	if got != want {
		t.Errorf("MUCDomain() = %q, want %q", got, want)
	}
}

func TestRoomConfig_JID(t *testing.T) {
	r := baseRoom()
	got := r.JID()
	// Should use conferenceId as local part, MUCDomain as domain
	wantPrefix := "cb140blkff7i_3074b65d"
	wantSuffix := "@conference.cb140blkff7i.ilte0310.ktalk.ru"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("JID() = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("JID() = %q, want suffix %q", got, wantSuffix)
	}
}

func TestRoomConfig_FocusJID(t *testing.T) {
	r := baseRoom()
	got := r.FocusJID()
	want := "focus@ilte0310.ktalk.ru/focus"
	if got != want {
		t.Errorf("FocusJID() = %q, want %q", got, want)
	}
}

func TestRoomConfig_UnlockURL(t *testing.T) {
	r := baseRoom()
	got := r.UnlockURL()
	// Format: https://<domain>/<roomName>/_unlock?room=<conferenceId>
	if !strings.HasPrefix(got, "https://ilte0310.ktalk.ru/cb140blkff7i/_unlock?room=") {
		t.Errorf("UnlockURL() = %q", got)
	}
	if !strings.Contains(got, r.ConferenceID) {
		t.Errorf("UnlockURL() missing conferenceId: %q", got)
	}
}

func TestRoomConfig_MetricsURL(t *testing.T) {
	r := baseRoom()
	got := r.MetricsURL()
	want := "https://ilte0310.ktalk.ru/api/metrics/connection"
	if got != want {
		t.Errorf("MetricsURL() = %q, want %q", got, want)
	}
}

func TestRoomConfig_HTTPBase(t *testing.T) {
	r := baseRoom()
	got := r.HTTPBase()
	want := "https://ilte0310.ktalk.ru"
	if got != want {
		t.Errorf("HTTPBase() = %q, want %q", got, want)
	}
}

func TestRoomConfig_RoomAPIURL(t *testing.T) {
	r := baseRoom()
	got := r.RoomAPIURL()
	want := "https://ilte0310.ktalk.ru/api/rooms/cb140blkff7i"
	if got != want {
		t.Errorf("RoomAPIURL() = %q, want %q", got, want)
	}
}

func TestConfig_Validate_OK(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeCreator,
		Room: config.RoomConfig{
			Subdomain:    "ilte0310",
			RoomID:       "cb140blkff7i",
			ConferenceID: "cb140blkff7i_abc",
		},
		Crypto: config.CryptoConfig{
			Key: strings.Repeat("a", 64),
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

func TestConfig_Validate_MissingSubdomain(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeCreator,
		Room: config.RoomConfig{RoomID: "abc"},
		Crypto: config.CryptoConfig{
			Key: strings.Repeat("a", 64),
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for missing subdomain")
	}
}

func TestConfig_Validate_MissingRoomID(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeCreator,
		Room: config.RoomConfig{Subdomain: "ilte0310"},
		Crypto: config.CryptoConfig{
			Key: strings.Repeat("a", 64),
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for missing room_id")
	}
}

func TestConfig_Validate_BadCryptoKey(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeCreator,
		Room: config.RoomConfig{Subdomain: "ilte0310", RoomID: "abc"},
		Crypto: config.CryptoConfig{
			Key: "tooshort",
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for short crypto key")
	}
}

func TestConfig_URIRoundtrip(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeCreator,
		Room: config.RoomConfig{
			Subdomain:    "ilte0310",
			RoomID:       "cb140blkff7i",
			ConferenceID: "cb140blkff7i_3074b65d29905f8e4418e2113a329f487fcadc8e4ed58df7b108624d199a4110",
		},
		Crypto: config.CryptoConfig{
			Key: strings.Repeat("f", 64),
		},
	}
	uri, err := cfg.EncodeURI()
	if err != nil {
		t.Fatalf("EncodeURI: %v", err)
	}
	if !strings.HasPrefix(uri, "ktalk://") {
		t.Fatalf("URI must start with ktalk://, got %q", uri)
	}

	decoded, err := config.DecodeURI(uri)
	if err != nil {
		t.Fatalf("DecodeURI: %v", err)
	}
	if decoded.Room.ConferenceID != cfg.Room.ConferenceID {
		t.Errorf("ConferenceID roundtrip: got %q, want %q",
			decoded.Room.ConferenceID, cfg.Room.ConferenceID)
	}
	if decoded.Room.RoomID != cfg.Room.RoomID {
		t.Errorf("RoomID roundtrip: got %q, want %q",
			decoded.Room.RoomID, cfg.Room.RoomID)
	}
}
