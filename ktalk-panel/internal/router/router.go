// Package router implements multi-room SOCKS5 routing.
//
// The Router multiplexes multiple tunnel clients behind a single SOCKS5 server.
// Each incoming SOCKS5 connection is routed to the appropriate tunnel client
// based on the target address, allowing different upstream destinations to use
// different ktalk.ru rooms.
//
// Routing rules (in priority order):
//  1. Exact host match       (e.g. "internal.example.com")
//  2. Subnet CIDR match      (e.g. "192.168.0.0/24")
//  3. Suffix domain match    (e.g. "*.example.com" matches "foo.example.com")
//  4. Default client         (fallback if configured)
//
// Usage:
//
//	r := router.New(cfg, sup, log)
//	r.SetRules([]router.Rule{
//	    {Match: "192.168.1.0/24", ClientID: "alice"},
//	    {Match: "*.internal.corp", ClientID: "bob"},
//	})
//	ln, _ := net.Listen("tcp", ":1080")
//	r.Serve(ctx, ln)
package router

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/config"
	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/supervisor"
)

// Rule maps a destination pattern to a client ID.
type Rule struct {
	// Match is the routing pattern. Formats:
	//   - IP address:   "10.0.0.1"
	//   - CIDR subnet:  "10.0.0.0/8"
	//   - Exact host:   "example.com"
	//   - Suffix glob:  "*.example.com"
	//   - Wildcard all: "*"
	Match string `json:"match"`

	// ClientID is the tunnel client to forward matching connections to.
	ClientID string `json:"client_id"`

	// Priority overrides the default match order (higher = evaluated first).
	// If omitted, rules are evaluated in list order.
	Priority int `json:"priority,omitempty"`
}

// Router routes incoming SOCKS5 connections to appropriate tunnel clients.
type Router struct {
	mu        sync.RWMutex
	rules     []compiledRule
	defaultID string
	sup       *supervisor.Supervisor
	store     *config.Store
	log       *slog.Logger
}

type compiledRule struct {
	rule     Rule
	cidr     *net.IPNet // non-nil if rule.Match is a CIDR
	ip       net.IP     // non-nil if rule.Match is a plain IP
	suffix   string     // non-nil suffix if "*.foo" pattern
	exact    string     // non-nil if exact host match
	wildcard bool       // true if match == "*"
}

// New creates a new Router.
func New(sup *supervisor.Supervisor, store *config.Store, log *slog.Logger) *Router {
	return &Router{
		sup:   sup,
		store: store,
		log:   log,
	}
}

// SetDefaultClient sets the fallback client ID used when no rule matches.
func (r *Router) SetDefaultClient(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultID = clientID
}

// SetRules replaces all routing rules.
func (r *Router) SetRules(rules []Rule) error {
	compiled := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		cr, err := compile(rule)
		if err != nil {
			return fmt.Errorf("invalid rule %q: %w", rule.Match, err)
		}
		compiled = append(compiled, cr)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = compiled
	return nil
}

// Route returns the client ID that should handle a connection to the given destination.
// Returns ("", false) if no rule matches and no default is configured.
func (r *Router) Route(host string, port int) (clientID string, ok bool) {
	r.mu.RLock()
	rules := r.rules
	defaultID := r.defaultID
	r.mu.RUnlock()

	ip := net.ParseIP(host)

	for _, cr := range rules {
		if cr.wildcard {
			r.logDebug("router: wildcard match", "host", host, "client", cr.rule.ClientID)
			return cr.rule.ClientID, true
		}

		if ip != nil {
			// IP-based matching
			if cr.ip != nil && cr.ip.Equal(ip) {
				r.logDebug("router: exact IP match", "host", host, "client", cr.rule.ClientID)
				return cr.rule.ClientID, true
			}
			if cr.cidr != nil && cr.cidr.Contains(ip) {
				r.logDebug("router: CIDR match", "host", host, "cidr", cr.rule.Match, "client", cr.rule.ClientID)
				return cr.rule.ClientID, true
			}
		} else {
			// DNS-based matching
			hostLower := strings.ToLower(host)
			if cr.exact != "" && cr.exact == hostLower {
				r.logDebug("router: exact host match", "host", host, "client", cr.rule.ClientID)
				return cr.rule.ClientID, true
			}
			if cr.suffix != "" && strings.HasSuffix(hostLower, "."+cr.suffix) {
				r.logDebug("router: suffix match", "host", host, "suffix", cr.suffix, "client", cr.rule.ClientID)
				return cr.rule.ClientID, true
			}
		}
	}

	if defaultID != "" {
		r.logDebug("router: default client", "host", host, "client", defaultID)
		return defaultID, true
	}

	r.logWarn("router: no route found", "host", host, "port", port)
	return "", false
}

// logDebug logs at DEBUG level if a logger is configured.
func (r *Router) logDebug(msg string, args ...any) {
	if r.log != nil {
		r.log.Debug(msg, args...)
	}
}

// logWarn logs at WARN level if a logger is configured.
func (r *Router) logWarn(msg string, args ...any) {
	if r.log != nil {
		r.log.Warn(msg, args...)
	}
}

// GetSocksAddr returns the SOCKS5 proxy address for a given client ID.
// Returns "" if the client is not running.
func (r *Router) GetSocksAddr(clientID string) string {
	states := r.sup.State()
	for _, s := range states {
		if s.ClientID == clientID && s.Running {
			if r.store != nil {
				if c, ok := r.store.GetClient(clientID); ok && c.SOCKS5Port > 0 {
					return fmt.Sprintf("127.0.0.1:%d", c.SOCKS5Port)
				}
			}
			// fallback for old clients without explicit port
			return fmt.Sprintf("127.0.0.1:%d", socksPortForClient(clientID))
		}
	}
	return ""
}

// TableSummary returns a human-readable routing table for the admin UI.
func (r *Router) TableSummary() []RouteSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []RouteSummary
	for i, cr := range r.rules {
		out = append(out, RouteSummary{
			Priority: i,
			Match:    cr.rule.Match,
			ClientID: cr.rule.ClientID,
		})
	}
	if r.defaultID != "" {
		out = append(out, RouteSummary{
			Priority:  len(r.rules),
			Match:     "*",
			ClientID:  r.defaultID,
			IsDefault: true,
		})
	}
	return out
}

// RouteSummary is a JSON-serialisable description of one routing rule.
type RouteSummary struct {
	Priority  int    `json:"priority"`
	Match     string `json:"match"`
	ClientID  string `json:"client_id"`
	IsDefault bool   `json:"is_default,omitempty"`
}

// compile validates and pre-processes a routing rule.
func compile(rule Rule) (compiledRule, error) {
	cr := compiledRule{rule: rule}
	pattern := strings.TrimSpace(rule.Match)

	if pattern == "*" {
		cr.wildcard = true
		return cr, nil
	}

	// Try CIDR
	if strings.Contains(pattern, "/") {
		_, ipNet, err := net.ParseCIDR(pattern)
		if err != nil {
			return cr, fmt.Errorf("invalid CIDR %q: %w", pattern, err)
		}
		cr.cidr = ipNet
		return cr, nil
	}

	// Try plain IP
	if ip := net.ParseIP(pattern); ip != nil {
		cr.ip = ip
		return cr, nil
	}

	// Suffix glob: "*.example.com"
	if strings.HasPrefix(pattern, "*.") {
		cr.suffix = strings.ToLower(pattern[2:])
		return cr, nil
	}

	// Exact host (case-insensitive)
	cr.exact = strings.ToLower(pattern)
	return cr, nil
}

// socksPortForClient is kept as a fallback for clients created before SOCKS5Port
// was stored explicitly in config. New clients always have SOCKS5Port set.
// Deprecated: use config.Client.SOCKS5Port instead.
func socksPortForClient(clientID string) int {
	h := 0
	for _, c := range clientID {
		h = h*31 + int(c)
	}
	// Map to port range 20000-29999
	port := 20000 + (h%10000+10000)%10000
	return port
}

// MultiRoomSocksHandler is a SOCKS5 DialFunc that routes through the appropriate tunnel.
// It is intended to be used with the socks5 server's DialFunc hook.
func MultiRoomSocksHandler(
	ctx context.Context,
	r *Router,
	host string, port int,
) (net.Conn, error) {
	clientID, ok := r.Route(host, port)
	if !ok {
		return nil, fmt.Errorf("no route to %s:%d", host, port)
	}

	socksAddr := r.GetSocksAddr(clientID)
	if socksAddr == "" {
		return nil, fmt.Errorf("client %q is not running (no active SOCKS5 proxy)", clientID)
	}

	// Connect to the per-client SOCKS5 proxy
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", socksAddr)
	if err != nil {
		return nil, fmt.Errorf("dial client %q SOCKS5 at %s: %w", clientID, socksAddr, err)
	}
	return conn, nil
}
