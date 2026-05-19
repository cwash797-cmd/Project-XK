// Package roomapi implements the Kontur Talk rooms REST client.
//
// # Anonymous join flow (confirmed via curl 2026-05-19)
//
//  1. GET https://<shard>.ktalk.ru/api/rooms/<roomName>
//     → 200 JSON with "conferenceId" field
//     → Set-Cookie: ngtoken=<token>; Domain=.ktalk.ru; HttpOnly; SameSite=None
//     → Set-Cookie: kontur_ngtoken=<token>; Domain=<shard>.ktalk.ru; SameSite=Strict
//
//  2. All subsequent requests (WebSocket upgrade, _unlock, /api/metrics/connection)
//     must carry both cookies — the http.Client.Jar handles this automatically.
//
// # Key findings from live testing
//
//   - No authentication required for allowAnonymous=true rooms
//   - conferenceId format: "<roomName>_<sha256-hex>"  (64 hex chars after underscore)
//   - _unlock does NOT return x-jitsi-shard in this deployment (returns HTML, 3701 bytes)
//   - POST /api/metrics/connection accepts {} and returns 200 — minimal payload is fine
//   - Room has no TTL — same room has been accessible for 3+ months continuously
//   - Free tier has no call duration limit — sessions run indefinitely
package roomapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"time"
)

// RoomInfo is the JSON response from GET /api/rooms/<roomName>.
// Only fields relevant to the tunnel client are included.
type RoomInfo struct {
	// RoomName is the short room identifier used in URL paths.
	RoomName string `json:"roomName"`

	// ConferenceID is the full conference identifier with HMAC suffix.
	// Format: "<roomName>_<sha256-hex>"
	// CRITICAL: This is the room= parameter in the XMPP WebSocket URL.
	// It is NOT the same as RoomName.
	ConferenceID string `json:"conferenceId"`

	// StageConferenceID is the stage room variant (not used by tunnel).
	StageConferenceID string `json:"stageConferenceId"`

	// AllowAnonymous indicates whether anonymous guests can join.
	// Must be true for headless client to proceed.
	AllowAnonymous bool `json:"allowAnonymous"`

	// AudioPolicy / VideoPolicy / ScreenSharePolicy — media restrictions.
	// All are "none" for this room (no restrictions).
	AudioPolicy       string `json:"audioPolicy"`
	VideoPolicy       string `json:"videoPolicy"`
	ScreenSharePolicy string `json:"screenSharePolicy"`

	// UsersOnline is the current occupant count.
	UsersOnline int `json:"usersOnline"`

	// OnlineUsers lists current participants (informational).
	OnlineUsers []OnlineUser `json:"onlineUsers"`

	// ChatChannelSettings — chat availability.
	ChatChannelSettings struct {
		Enabled bool `json:"enabled"`
	} `json:"chatChannelSettings"`

	// MaskingSettings — name masking mode (UI only, not protocol-relevant).
	MaskingSettings struct {
		NameMaskingMode    string `json:"nameMaskingMode"`
		PostMaskingMode    string `json:"postMaskingMode"`
		ShowAdditionalInfo bool   `json:"showAdditionalInfo"`
	} `json:"maskingSettings"`
}

// OnlineUser is a single participant entry in RoomInfo.OnlineUsers.
type OnlineUser struct {
	AnonymousName string `json:"anonymousName"`
	AnonymousID   string `json:"anonymousId"`
	IsAnonymous   bool   `json:"isAnonymous"`
}

// Client fetches room info and manages the HTTP cookie jar for subsequent
// XMPP WebSocket and keepalive requests.
type Client struct {
	httpClient *http.Client
	baseURL    string // e.g. "https://ilte0310.ktalk.ru"
}

// NewClient creates a room API client.
// baseURL is the shard base URL, e.g. "https://ilte0310.ktalk.ru".
// A fresh cookie jar is created; cookies are populated by FetchRoom.
func NewClient(baseURL string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookiejar: %w", err)
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
			// Follow redirects but collect cookies from each hop.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}, nil
}

// NewClientWithJar creates a room API client using an existing cookie jar.
// Use this when you want to share cookies with an existing http.Client.
func NewClientWithJar(baseURL string, jar http.CookieJar) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
		},
	}
}

// CookieJar returns the underlying cookie jar.
// Pass this to xmpp.NewClient so it inherits the ngtoken/kontur_ngtoken cookies.
func (c *Client) CookieJar() http.CookieJar {
	return c.httpClient.Jar
}

// FetchRoom calls GET /api/rooms/<roomName> and returns the parsed response.
//
// Side effects:
//   - Populates the cookie jar with ngtoken and kontur_ngtoken cookies
//     (both are set by the server via Set-Cookie on this endpoint).
//   - After this call, c.CookieJar() carries valid session cookies for the
//     XMPP WebSocket upgrade and all keepalive requests.
//
// Returns ErrAnonymousNotAllowed if the room requires authentication.
// Returns ErrRoomNotFound if the room does not exist.
func (c *Client) FetchRoom(ctx context.Context, roomName string) (*RoomInfo, error) {
	url := fmt.Sprintf("%s/api/rooms/%s", c.baseURL, roomName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ktalk-core/0.1 (headless tunnel client)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// OK — fall through to parse
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrRoomNotFound, roomName)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", ErrUnauthorized, roomName)
	default:
		return nil, fmt.Errorf("unexpected status %d for room %s", resp.StatusCode, roomName)
	}

	var info RoomInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode room response: %w", err)
	}

	if info.RoomName == "" {
		return nil, fmt.Errorf("room response missing roomName field")
	}
	if info.ConferenceID == "" {
		return nil, fmt.Errorf("room response missing conferenceId field — cannot build WebSocket URL")
	}
	if !info.AllowAnonymous {
		return nil, fmt.Errorf("%w: room %s requires authentication (allowAnonymous=false)", ErrAnonymousNotAllowed, roomName)
	}

	return &info, nil
}

// Sentinel errors for the caller to distinguish failure modes.
var (
	// ErrRoomNotFound is returned when the room does not exist (404).
	ErrRoomNotFound = fmt.Errorf("room not found")

	// ErrUnauthorized is returned when the room requires a valid session (401/403).
	ErrUnauthorized = fmt.Errorf("unauthorized")

	// ErrAnonymousNotAllowed is returned when allowAnonymous=false.
	ErrAnonymousNotAllowed = fmt.Errorf("anonymous access not allowed")
)
