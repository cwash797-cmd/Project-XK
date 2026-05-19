package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	rl := &rateLimiter{interval: 100 * time.Millisecond}

	// First call should always be allowed
	if !rl.Allow(LevelError) {
		t.Fatal("first call should be allowed")
	}

	// Immediate second call should be rate-limited
	if rl.Allow(LevelError) {
		t.Fatal("immediate repeat should be rate-limited")
	}

	// Different level should be allowed independently
	if !rl.Allow(LevelWarn) {
		t.Fatal("different level should be allowed independently")
	}

	// Wait for interval to expire
	time.Sleep(150 * time.Millisecond)
	if !rl.Allow(LevelError) {
		t.Fatal("call after interval should be allowed")
	}
}

func TestDedupCache(t *testing.T) {
	c := &dedupCache{ttl: 100 * time.Millisecond}

	// First occurrence: not a duplicate
	if c.IsDuplicate("key-a") {
		t.Fatal("first occurrence should not be a duplicate")
	}
	// Second occurrence within TTL: is a duplicate
	if !c.IsDuplicate("key-a") {
		t.Fatal("second occurrence within TTL should be a duplicate")
	}
	// Different key: not a duplicate
	if c.IsDuplicate("key-b") {
		t.Fatal("different key should not be a duplicate")
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)
	if c.IsDuplicate("key-a") {
		t.Fatal("occurrence after TTL expiry should not be a duplicate")
	}
}

func TestEscapeMarkdown(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"hello", "hello"},
		{"hello_world", "hello\\_world"},
		{"a*b", "a\\*b"},
		{"1+1=2", "1\\+1\\=2"},
		{"a.b", "a\\.b"},
		{"(test)", "\\(test\\)"},
	}
	for _, tc := range cases {
		got := escapeMarkdown(tc.in)
		if got != tc.out {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestNoopNotifier(t *testing.T) {
	n := NoopNotifier{}
	if err := n.Alert(context.Background(), Alert{Level: LevelError, Title: "test"}); err != nil {
		t.Fatalf("NoopNotifier.Alert returned error: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("NoopNotifier.Close returned error: %v", err)
	}
}

func TestAlertManagerDedup(t *testing.T) {
	type delivered struct {
		count int
	}
	d := &delivered{}

	mock := &mockNotifier{fn: func(_ Alert) {
		d.count++
	}}

	noopLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewAlertManager(noopLog, mock)
	// Override dedup TTL for test
	mgr.dedup = &dedupCache{ttl: 100 * time.Millisecond}

	ctx := context.Background()
	a := Alert{Level: LevelError, Title: "ICE failed", ClientID: "test-1"}

	mgr.Alert(ctx, a)
	mgr.Alert(ctx, a) // should be deduped
	mgr.Alert(ctx, a) // should be deduped

	if d.count != 1 {
		t.Errorf("expected 1 delivery, got %d", d.count)
	}

	time.Sleep(150 * time.Millisecond)

	mgr.Alert(ctx, a) // after TTL expiry, should deliver
	if d.count != 2 {
		t.Errorf("expected 2 deliveries after TTL, got %d", d.count)
	}
}

func TestLevelEmoji(t *testing.T) {
	cases := []struct {
		l Level
		e string
	}{
		{LevelInfo, "ℹ️"},
		{LevelWarn, "⚠️"},
		{LevelError, "🔴"},
		{LevelCrit, "🚨"},
	}
	for _, tc := range cases {
		if got := tc.l.Emoji(); got != tc.e {
			t.Errorf("Level(%d).Emoji() = %q, want %q", tc.l, got, tc.e)
		}
	}
}

func TestFormatMessage(t *testing.T) {
	bot := &TelegramBot{}
	a := Alert{
		Level:     LevelError,
		Title:     "Tunnel failed",
		Body:      "ICE restart failed after 5 attempts",
		ClientID:  "abc123",
		Timestamp: time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}
	msg := bot.formatMessage(a)
	if msg == "" {
		t.Fatal("formatMessage returned empty string")
	}
	t.Logf("formatted message:\n%s", msg)
}

// mockNotifier is a test helper.
type mockNotifier struct {
	fn func(Alert)
}

func (m *mockNotifier) Alert(_ context.Context, a Alert) error {
	m.fn(a)
	return nil
}

func (m *mockNotifier) Close() error { return nil }
