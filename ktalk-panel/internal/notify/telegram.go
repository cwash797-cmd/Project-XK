// Package notify provides alert delivery via Telegram Bot API.
//
// Usage:
//
//	bot, err := notify.NewTelegramBot(token, chatID)
//	if err != nil { ... }
//	bot.Alert(ctx, notify.Alert{Level: notify.LevelError, Title: "Tunnel failed", Body: "..."})
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Level represents the severity of an alert.
type Level int

const (
	LevelInfo  Level = iota
	LevelWarn        // ⚠️
	LevelError       // 🔴
	LevelCrit        // 🚨
)

func (l Level) Emoji() string {
	switch l {
	case LevelInfo:
		return "ℹ️"
	case LevelWarn:
		return "⚠️"
	case LevelError:
		return "🔴"
	case LevelCrit:
		return "🚨"
	default:
		return "•"
	}
}

func (l Level) String() string {
	switch l {
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelCrit:
		return "CRIT"
	default:
		return "UNKNOWN"
	}
}

// Alert is a notification message.
type Alert struct {
	// Level is the severity.
	Level Level
	// Title is the short summary (displayed in bold).
	Title string
	// Body is optional multi-line detail text.
	Body string
	// ClientID is the tunnel client ID that generated this alert (optional).
	ClientID string
	// Timestamp is when the event occurred (zero = time.Now()).
	Timestamp time.Time
}

// Notifier is the interface for sending alerts.
type Notifier interface {
	// Alert sends an alert. Returns nil on success or delivery error.
	Alert(ctx context.Context, a Alert) error
	// Close shuts down the notifier, flushing any pending alerts.
	Close() error
}

// NoopNotifier discards all alerts (for use when notifications are not configured).
type NoopNotifier struct{}

func (NoopNotifier) Alert(_ context.Context, _ Alert) error { return nil }
func (NoopNotifier) Close() error                           { return nil }

// TelegramBot sends alerts via Telegram Bot API.
type TelegramBot struct {
	token     string
	chatID    string
	client    *http.Client
	log       *slog.Logger
	rateLimit *rateLimiter // avoid flooding
}

// TelegramConfig holds Telegram bot configuration.
type TelegramConfig struct {
	// Token is the Telegram bot token from @BotFather.
	Token string `json:"token"`
	// ChatID is the target chat/channel ID (e.g. "-1001234567890" for a channel).
	ChatID string `json:"chat_id"`
	// MinLevel is the minimum alert level to deliver (default: LevelWarn).
	MinLevel Level `json:"min_level"`
	// RateLimit is the minimum interval between messages (default: 30s).
	// Set to 0 to disable rate limiting.
	RateLimit time.Duration `json:"rate_limit"`
}

// NewTelegramBot creates a new Telegram notifier.
func NewTelegramBot(cfg TelegramConfig, log *slog.Logger) (*TelegramBot, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram token is required")
	}
	if cfg.ChatID == "" {
		return nil, fmt.Errorf("telegram chat_id is required")
	}

	rateInterval := cfg.RateLimit
	if rateInterval == 0 {
		rateInterval = 30 * time.Second
	}

	bot := &TelegramBot{
		token:  cfg.Token,
		chatID: cfg.ChatID,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
		rateLimit: &rateLimiter{
			interval: rateInterval,
		},
	}

	return bot, nil
}

// Alert formats and sends a Telegram message.
func (b *TelegramBot) Alert(ctx context.Context, a Alert) error {
	if a.Timestamp.IsZero() {
		a.Timestamp = time.Now()
	}

	// Rate limit: suppress if too soon after last message of same level
	if !b.rateLimit.Allow(a.Level) {
		b.log.Debug("alert rate-limited", "level", a.Level, "title", a.Title)
		return nil
	}

	text := b.formatMessage(a)

	return b.sendMessage(ctx, text)
}

// formatMessage builds the Telegram message text in MarkdownV2 format.
func (b *TelegramBot) formatMessage(a Alert) string {
	var sb strings.Builder

	// Header line
	sb.WriteString(fmt.Sprintf("%s *%s* — %s\n",
		a.Level.Emoji(),
		escapeMarkdown(a.Level.String()),
		escapeMarkdown(a.Title),
	))

	// Optional client ID
	if a.ClientID != "" {
		sb.WriteString(fmt.Sprintf("Client: `%s`\n", escapeMarkdown(a.ClientID)))
	}

	// Timestamp
	sb.WriteString(fmt.Sprintf("Time: `%s`\n",
		escapeMarkdown(a.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC")),
	))

	// Optional body
	if a.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(escapeMarkdown(a.Body))
	}

	return sb.String()
}

// sendMessage sends a message to the configured chat.
func (b *TelegramBot) sendMessage(ctx context.Context, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)

	payload := map[string]interface{}{
		"chat_id":    b.chatID,
		"text":       text,
		"parse_mode": "MarkdownV2",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		if jsonErr := json.NewDecoder(resp.Body).Decode(&apiErr); jsonErr == nil && apiErr.Description != "" {
			return fmt.Errorf("telegram API error %d: %s", resp.StatusCode, apiErr.Description)
		}
		return fmt.Errorf("telegram API returned HTTP %d", resp.StatusCode)
	}

	b.log.Debug("telegram alert sent", "text_len", len(text))
	return nil
}

// Close is a no-op for TelegramBot (stateless HTTP calls).
func (b *TelegramBot) Close() error { return nil }

// escapeMarkdown escapes special characters for Telegram MarkdownV2.
func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(s)
}

// rateLimiter prevents alert flooding per level.
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	lastSent map[Level]time.Time
}

// Allow returns true if an alert of the given level can be sent now.
func (r *rateLimiter) Allow(level Level) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastSent == nil {
		r.lastSent = make(map[Level]time.Time)
	}
	last, ok := r.lastSent[level]
	if !ok || time.Since(last) >= r.interval {
		r.lastSent[level] = time.Now()
		return true
	}
	return false
}

// AlertManager fans alerts out to multiple notifiers and enforces deduplication.
type AlertManager struct {
	notifiers []Notifier
	log       *slog.Logger
	dedup     *dedupCache
}

// NewAlertManager creates an AlertManager with the given notifiers.
func NewAlertManager(log *slog.Logger, notifiers ...Notifier) *AlertManager {
	return &AlertManager{
		notifiers: notifiers,
		log:       log,
		dedup:     &dedupCache{ttl: 5 * time.Minute},
	}
}

// Alert delivers an alert to all registered notifiers.
// Duplicate alerts (same title + client within 5 minutes) are suppressed.
func (m *AlertManager) Alert(ctx context.Context, a Alert) {
	key := fmt.Sprintf("%s|%s|%d", a.Title, a.ClientID, a.Level)
	if m.dedup.IsDuplicate(key) {
		m.log.Debug("alert deduplicated", "title", a.Title, "client", a.ClientID)
		return
	}

	for _, n := range m.notifiers {
		if err := n.Alert(ctx, a); err != nil {
			m.log.Warn("alert delivery failed",
				"notifier", fmt.Sprintf("%T", n),
				"title", a.Title,
				"err", err)
		}
	}
}

// Close shuts down all notifiers.
func (m *AlertManager) Close() {
	for _, n := range m.notifiers {
		if err := n.Close(); err != nil {
			m.log.Warn("notifier close error", "err", err)
		}
	}
}

// TunnelEventAlerter wraps AlertManager with tunnel-specific alert constructors.
type TunnelEventAlerter struct {
	mgr *AlertManager
}

// NewTunnelEventAlerter creates a TunnelEventAlerter backed by the given AlertManager.
func NewTunnelEventAlerter(mgr *AlertManager) *TunnelEventAlerter {
	return &TunnelEventAlerter{mgr: mgr}
}

// OnTunnelFailed fires when a tunnel reaches the error state.
func (a *TunnelEventAlerter) OnTunnelFailed(ctx context.Context, clientID, reason string) {
	a.mgr.Alert(ctx, Alert{
		Level:    LevelError,
		Title:    "Tunnel failed",
		Body:     reason,
		ClientID: clientID,
	})
}

// OnICEFailed fires when ICE fails after all restart attempts.
func (a *TunnelEventAlerter) OnICEFailed(ctx context.Context, clientID string, attempts int) {
	a.mgr.Alert(ctx, Alert{
		Level:    LevelError,
		Title:    "ICE connection failed",
		Body:     fmt.Sprintf("All %d ICE restart attempts exhausted", attempts),
		ClientID: clientID,
	})
}

// OnFocusLeft fires when Jicofo disconnects from the room.
func (a *TunnelEventAlerter) OnFocusLeft(ctx context.Context, clientID string) {
	a.mgr.Alert(ctx, Alert{
		Level:    LevelWarn,
		Title:    "Jicofo (focus) left conference",
		Body:     "Server-side Jitsi focus server disconnected. Reconnecting...",
		ClientID: clientID,
	})
}

// OnShardChanged fires when the ktalk.ru load balancer moves us to a new shard.
func (a *TunnelEventAlerter) OnShardChanged(ctx context.Context, clientID, oldShard, newShard string) {
	a.mgr.Alert(ctx, Alert{
		Level:    LevelWarn,
		Title:    "Server shard changed",
		Body:     fmt.Sprintf("Shard changed from %q to %q. Reconnecting...", oldShard, newShard),
		ClientID: clientID,
	})
}

// OnSessionExpired fires when the server session token expires (401 response).
func (a *TunnelEventAlerter) OnSessionExpired(ctx context.Context, clientID string) {
	a.mgr.Alert(ctx, Alert{
		Level:    LevelCrit,
		Title:    "Session token expired",
		Body:     "ktalk.ru session token expired (401). Manual intervention may be required.",
		ClientID: clientID,
	})
}

// OnKeyRotated fires on successful 24h key rotation.
func (a *TunnelEventAlerter) OnKeyRotated(ctx context.Context, clientID string) {
	a.mgr.Alert(ctx, Alert{
		Level:    LevelInfo,
		Title:    "E2E key rotated",
		Body:     "ChaCha20 key rotated successfully (24h cycle).",
		ClientID: clientID,
	})
}

// dedupCache prevents duplicate alerts within a TTL window.
type dedupCache struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	ttl     time.Duration
	cleaned time.Time
}

// IsDuplicate returns true if this key was seen within the TTL window.
// If not a duplicate, records the key.
func (c *dedupCache) IsDuplicate(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.seen == nil {
		c.seen = make(map[string]time.Time)
	}

	// Periodic cleanup every minute
	if time.Since(c.cleaned) > time.Minute {
		now := time.Now()
		for k, t := range c.seen {
			if now.Sub(t) > c.ttl {
				delete(c.seen, k)
			}
		}
		c.cleaned = now
	}

	if last, ok := c.seen[key]; ok && time.Since(last) < c.ttl {
		return true
	}
	c.seen[key] = time.Now()
	return false
}
