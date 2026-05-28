// Command ktalk-panel is the admin web panel for the ktalk private relay service.
//
// Usage:
//
//	ktalk-panel -config /etc/ktalk-panel/config.json [-addr 127.0.0.1] [-port 8888]
//
// The panel:
//   - Serves the embedded SvelteKit frontend at /admin
//   - Exposes a REST API at /api/* (requires session cookie)
//   - Serves per-client subscription configs at /sub/:id/:token
//   - Manages ktalk-core child processes via the supervisor
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/auth"
	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/config"
	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/sse"
	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/supervisor"
)

//go:embed all:web/dist
var webFS embed.FS

func init() {
	// Register MIME types explicitly so Go's http.FileServer serves correct
	// Content-Type headers on minimal Linux systems that lack /etc/mime.types.
	// Without this, CSS and JS files are served as "text/plain", causing
	// browsers with X-Content-Type-Options: nosniff to reject them.
	_ = mime.AddExtensionType(".js", "application/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".mjs", "application/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
	_ = mime.AddExtensionType(".html", "text/html; charset=utf-8")
	_ = mime.AddExtensionType(".json", "application/json")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
	_ = mime.AddExtensionType(".png", "image/png")
	_ = mime.AddExtensionType(".ico", "image/x-icon")
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".woff", "font/woff")
}

var (
	sessions = auth.NewSessionStore()
	limiter  = auth.NewLimiter()
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ktalk-panel: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		flagConfig string
		flagAddr   string
		flagPort   int
		flagDebug  bool
	)
	flag.StringVar(&flagConfig, "config", "/etc/ktalk-panel/config.json", "path to config.json")
	flag.StringVar(&flagAddr, "addr", "127.0.0.1", "listen address")
	flag.IntVar(&flagPort, "port", 0, "listen port (overrides config)")
	flag.BoolVar(&flagDebug, "debug", false, "verbose logging")
	flag.Parse()

	logLevel := slog.LevelInfo
	if flagDebug {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	store, err := config.Load(flagConfig)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	cfg := store.Get()
	port := cfg.Port
	if flagPort != 0 {
		port = flagPort
	}
	if flagAddr != "127.0.0.1" {
		cfg.ListenAddr = flagAddr
	}

	// Find ktalk-core binary
	corePath, err := findCoreBinary()
	if err != nil {
		log.Warn("ktalk-core binary not found, process management disabled", "err", err)
		corePath = ""
	}

	sup := supervisor.New(corePath, log.With("component", "supervisor"))
	defer sup.StopAll()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	broker := sse.New(log.With("component", "sse"))
	go broker.Run(func() any { return sup.State() })

	rotator := supervisor.NewKeyRotator(store, sup, log.With("component", "rotator"))
	go rotator.Run(ctx)

	mux := buildRouter(store, sup, broker, log)

	addr := net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("ktalk-panel listening", "addr", addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		} else {
			errc <- nil
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errc:
		return err
	}
}

func buildRouter(store *config.Store, sup *supervisor.Supervisor, broker *sse.Broker, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Static files (embedded SvelteKit build)
	staticFS, err := fs.Sub(webFS, "web/dist")
	if err == nil {
		mux.Handle("/", http.FileServer(http.FS(staticFS)))
	}

	// Admin SPA — serve index.html for /admin (SPA catch-all)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		f, err := staticFS.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		http.ServeContent(w, r, "index.html", fi.ModTime(), f.(interface {
			fs.File
			io.ReadSeeker
		}))
	})

	// Auth endpoints (no session required)
	mux.HandleFunc("/api/auth/setup", handleSetup(store))
	mux.HandleFunc("/api/auth/login", handleLogin(store))
	mux.HandleFunc("/api/auth/me", handleMe(store))

	// Protected endpoints
	protected := func(h http.HandlerFunc) http.Handler {
		return auth.Middleware(sessions, h)
	}

	mux.Handle("/api/auth/logout", protected(handleLogout))
	mux.Handle("/api/auth/password", protected(handleChangePassword(store)))
	mux.Handle("/api/clients", protected(handleClients(store, sup, log)))
	mux.Handle("/api/clients/", protected(handleClient(store, sup, log)))
	mux.Handle("/api/state", protected(handleState(sup)))
	mux.Handle("/api/logs/", protected(handleLogs(sup)))
	mux.Handle("/api/events", auth.Middleware(sessions, broker))

	// Subscription endpoint (public but secret-token gated)
	mux.HandleFunc("/sub/", handleSubscription(store))
	// QR code endpoint for subscription URL
	mux.HandleFunc("/qr/", handleQRSub(store))

	// Health + metrics (unauthenticated — safe for internal monitoring)
	startTime := time.Now()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "ok",
			"uptime": time.Since(startTime).Truncate(time.Second).String(),
		})
	})
	mux.HandleFunc("/metrics", handleMetrics(sup, startTime))

	return securityHeaders(mux)
}

// --- auth handlers ---

func handleSetup(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store.GetAdminHash() != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already set up"})
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 8 characters"})
			return
		}
		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "hash failed"})
			return
		}
		if err := store.SetAdminHash(hash); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		tok, err := sessions.Create()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		auth.SetSessionCookie(w, tok)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func handleLogin(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ip := clientIP(r)
		if !limiter.Allow(ip) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts"})
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		hash := store.GetAdminHash()
		if hash == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not configured"})
			return
		}
		if !auth.CheckPassword(hash, req.Password) {
			limiter.RecordFail(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
			return
		}
		limiter.RecordSuccess(ip)
		tok, err := sessions.Create()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		auth.SetSessionCookie(w, tok)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func handleMe(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		configured := store.GetAdminHash() != ""
		writeJSON(w, http.StatusOK, map[string]bool{"configured": configured})
	}
}

var handleLogout http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("ktalk_session"); err == nil {
		sessions.Delete(c.Value)
	}
	auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleChangePassword(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		if !auth.CheckPassword(store.GetAdminHash(), req.Current) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong current password"})
			return
		}
		if len(req.New) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "new password too short"})
			return
		}
		hash, err := auth.HashPassword(req.New)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = store.SetAdminHash(hash)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// --- client handlers ---

func handleClients(store *config.Store, sup *supervisor.Supervisor, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg := store.Get()
			writeJSON(w, http.StatusOK, cfg.Clients)
		case http.MethodPost:
			var req struct {
				Name      string       `json:"name"`
				Comment   string       `json:"comment"`
				Subdomain string       `json:"subdomain"`
				RoomID    string       `json:"room_id"`
				Hash      string       `json:"hash"`
				Quota     config.Quota `json:"quota"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if req.Name == "" || req.Subdomain == "" || req.RoomID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, subdomain and room_id are required"})
				return
			}
			client, err := store.AddClient(req.Name, req.Comment, req.Subdomain, req.RoomID, req.Hash, req.Quota)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			// Start the process if core binary is available
			if sup != nil {
				ctx := r.Context()
				if err := sup.Start(ctx, client); err != nil {
					log.Warn("auto-start failed for new client", "id", client.ID, "err", err)
				}
			}
			writeJSON(w, http.StatusCreated, clientWithSubURL(client, r.Host))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func handleClient(store *config.Store, sup *supervisor.Supervisor, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/clients/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		id := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		switch {
		case action == "start" && r.Method == http.MethodPost:
			c, ok := store.GetClient(id)
			if !ok {
				http.NotFound(w, r)
				return
			}
			if err := sup.Start(r.Context(), c); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		case action == "stop" && r.Method == http.MethodPost:
			sup.Stop(id)
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		case action == "restart" && r.Method == http.MethodPost:
			if err := sup.Restart(r.Context(), id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		case action == "rotate-key" && r.Method == http.MethodPost:
			if err := store.RotateKey(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			// Restart process with new key
			if c, ok := store.GetClient(id); ok {
				_ = sup.Restart(r.Context(), c.ID)
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		case action == "rotate-room" && r.Method == http.MethodPost:
			if err := store.RotateRoom(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if c, ok := store.GetClient(id); ok {
				_ = sup.Restart(r.Context(), c.ID)
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		case action == "" && r.Method == http.MethodDelete:
			sup.Stop(id)
			if err := store.DeleteClient(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case action == "qr" && r.Method == http.MethodGet:
			handleQR(store)(w, r)

		case action == "" && r.Method == http.MethodGet:
			c, ok := store.GetClient(id)
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, clientWithSubURL(c, r.Host))

		default:
			http.Error(w, "method not allowed or unknown action", http.StatusMethodNotAllowed)
		}
	}
}

func handleState(sup *supervisor.Supervisor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sup.State())
	}
}

func handleLogs(sup *supervisor.Supervisor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/logs/")
		id = strings.TrimSuffix(id, "/")
		lines, ok := sup.Logs(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"logs": lines})
	}
}

// clientResponse is the JSON shape returned by POST /api/clients and GET /api/clients/:id.
// It augments config.Client with the computed sub_url field.
type clientResponse struct {
	config.Client
	SubURL     string `json:"sub_url"`
	SOCKS5Addr string `json:"socks5_addr"`
}

// clientWithSubURL wraps a Client with its computed subscription URL.
func clientWithSubURL(c config.Client, host string) clientResponse {
	socksPort := c.SOCKS5Port
	if socksPort == 0 {
		socksPort = 1080 // sensible default for display
	}
	return clientResponse{
		Client:     c,
		SubURL:     buildSubURL(c, host),
		SOCKS5Addr: fmt.Sprintf("127.0.0.1:%d", socksPort),
	}
}

// buildSubURL returns the HTTPS subscription URL for a client.
func buildSubURL(c config.Client, host string) string {
	scheme := "https"
	// Use http for localhost/plain-IP dev setups
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.") {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/sub/%s/%s", scheme, host, c.ID, c.SubToken)
}

// --- subscription endpoint ---

func handleSubscription(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// URL: /sub/<client-id>/<secret-token>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sub/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		clientID, token := parts[0], parts[1]

		c, ok := store.GetClient(clientID)
		if !ok || c.SubToken != token {
			http.NotFound(w, r)
			return
		}

		status := c.Quota.Status()
		usedGB := float64(c.Quota.UsedBytes) / (1 << 30)

		socksPort := c.SOCKS5Port
		if socksPort == 0 {
			socksPort = 1080
		}
		socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)

		// P0-2: dual-format response — JSON for kvas-client, text/plain legacy for old clients
		accept := r.Header.Get("Accept")
		if strings.Contains(accept, "application/json") {
			writeJSON(w, http.StatusOK, map[string]string{
				"sub_url":     buildSubURL(c, r.Host),
				"room_url":    fmt.Sprintf("https://%s.ktalk.ru/%s", c.Room.Subdomain, c.Room.RoomID),
				"shared_key":  c.SharedKey,
				"socks5_addr": socksAddr,
				"label":       c.Name,
				"status":      status,
			})
			return
		}

		// Legacy text/plain — ktalk:// URI for old clients / manual use
		uri := buildJoinerURI(c)

		var sb strings.Builder
		fmt.Fprintf(&sb, "#ktalk-speed-mbps:%d\n", c.Quota.SpeedMbps)
		fmt.Fprintf(&sb, "#ktalk-traffic-gb:%d\n", c.Quota.TrafficGB)
		fmt.Fprintf(&sb, "#ktalk-used-gb:%.3f\n", usedGB)
		fmt.Fprintf(&sb, "#ktalk-expires-at:%s\n", c.Quota.ExpiresAt)
		fmt.Fprintf(&sb, "#ktalk-status:%s\n", status)
		fmt.Fprintf(&sb, "%s\n", uri)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sb.String()))
	}
}

// handleQR serves a QR code PNG for the subscription URL of a client.
// Route: GET /api/clients/:id/qr  (auth-protected, handled by handleClient)
func handleQR(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Path: /api/clients/<id>/qr
		path := strings.TrimPrefix(r.URL.Path, "/api/clients/")
		id := strings.TrimSuffix(path, "/qr")
		id = strings.TrimSuffix(id, "/")

		c, ok := store.GetClient(id)
		if !ok {
			http.NotFound(w, r)
			return
		}

		subURL := buildSubURL(c, r.Host)
		png, err := qrcode.Encode(subURL, qrcode.Medium, 256)
		if err != nil {
			http.Error(w, "qr encode error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	}
}

// handleQRSub serves a QR code for the subscription URL at /qr/:id/:token.
// This is publicly accessible (no auth) because the token acts as the secret.
func handleQRSub(store *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/qr/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		clientID, token := parts[0], parts[1]
		c, ok := store.GetClient(clientID)
		if !ok || c.SubToken != token {
			http.NotFound(w, r)
			return
		}
		subURL := buildSubURL(c, r.Host)
		png, err := qrcode.Encode(subURL, qrcode.Medium, 256)
		if err != nil {
			http.Error(w, "qr encode error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	}
}

// buildJoinerURI constructs the ktalk:// URI for the Joiner (client) side.
func buildJoinerURI(c config.Client) string {
	payload := fmt.Sprintf(
		`{"mode":"joiner","room":{"subdomain":%q,"room_id":%q,"hash":%q},"crypto":{"key":%q},"net":{"dns_server":"1.1.1.1:53"},"socks5":{"listen_addr":"127.0.0.1:1080"}}`,
		c.Room.Subdomain, c.Room.RoomID, c.Room.Hash, c.SharedKey,
	)
	return "ktalk://" + encodeBase64URL([]byte(payload))
}

// encodeBase64URL encodes b as base64url without padding (RFC 4648 §5).
func encodeBase64URL(b []byte) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		b0 := b[i]
		var b1, b2 byte
		if i+1 < len(b) {
			b1 = b[i+1]
		}
		if i+2 < len(b) {
			b2 = b[i+2]
		}
		out = append(out, alpha[b0>>2])
		out = append(out, alpha[(b0&3)<<4|b1>>4])
		if i+1 < len(b) {
			out = append(out, alpha[(b1&0xf)<<2|b2>>6])
		}
		if i+2 < len(b) {
			out = append(out, alpha[b2&0x3f])
		}
	}
	return string(out)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.Split(xff, ",")[0]
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// handleMetrics serves a Prometheus-compatible text exposition of panel metrics.
//
// Exposed metrics:
//
//	ktalk_tunnels_active          — number of currently running processes
//	ktalk_tunnels_total           — total processes ever started (running + stopped)
//	ktalk_restarts_total          — sum of all per-process restart counters
//	ktalk_panel_uptime_seconds    — panel uptime in seconds
func handleMetrics(sup *supervisor.Supervisor, startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		states := sup.State()

		var active, total, restarts int
		for _, s := range states {
			total++
			if s.Running {
				active++
			}
			restarts += s.Restarts
		}
		uptime := time.Since(startTime).Seconds()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "# HELP ktalk_tunnels_active Number of currently running tunnel processes\n")
		fmt.Fprintf(w, "# TYPE ktalk_tunnels_active gauge\n")
		fmt.Fprintf(w, "ktalk_tunnels_active %d\n", active)
		fmt.Fprintf(w, "# HELP ktalk_tunnels_total Total tunnel clients configured\n")
		fmt.Fprintf(w, "# TYPE ktalk_tunnels_total gauge\n")
		fmt.Fprintf(w, "ktalk_tunnels_total %d\n", total)
		fmt.Fprintf(w, "# HELP ktalk_restarts_total Total process restart events across all tunnels\n")
		fmt.Fprintf(w, "# TYPE ktalk_restarts_total counter\n")
		fmt.Fprintf(w, "ktalk_restarts_total %d\n", restarts)
		fmt.Fprintf(w, "# HELP ktalk_panel_uptime_seconds Panel process uptime in seconds\n")
		fmt.Fprintf(w, "# TYPE ktalk_panel_uptime_seconds gauge\n")
		fmt.Fprintf(w, "ktalk_panel_uptime_seconds %.1f\n", uptime)
	}
}

func findCoreBinary() (string, error) {
	// Look for ktalk-core in standard locations
	candidates := []string{
		"/usr/local/bin/ktalk-core",
		"./ktalk-core",
		filepath.Join(filepath.Dir(os.Args[0]), "ktalk-core"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("ktalk-core binary not found in PATH or standard locations")
}
