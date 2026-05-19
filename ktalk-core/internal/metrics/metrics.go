// Package metrics exposes runtime counters for ktalk-core.
//
// Two endpoints are served on the configured listen address:
//
//	GET /health   — JSON liveness probe {"ok":true,"uptime":"3m2s","version":"..."}
//	GET /metrics  — Prometheus text format
//
// No external dependencies: counters are plain sync/atomic values;
// the Prometheus format is hand-serialised (no prometheus/client_golang).
package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Counters holds all runtime statistics for a single ktalk-core process.
// All fields are 64-bit counters incremented atomically.
type Counters struct {
	BytesIn           atomic.Uint64
	BytesOut          atomic.Uint64
	StreamsOpened     atomic.Uint64
	StreamsClosed     atomic.Uint64
	FramesSent        atomic.Uint64
	FramesReceived    atomic.Uint64
	ICERestarts       atomic.Uint64
	XMPPReconnects    atomic.Uint64
	KeepaliveTimeouts atomic.Uint64
}

// Global is the process-wide counter set, ready to use immediately.
var Global = &Counters{}

// Server serves /health and /metrics over HTTP.
type Server struct {
	version   string
	startTime time.Time
	counters  *Counters
}

// New creates a metrics Server. version is a human-readable release string.
func New(version string, counters *Counters) *Server {
	return &Server{
		version:   version,
		startTime: time.Now(),
		counters:  counters,
	}
}

// Handler returns an http.Handler for /health and /metrics.
// Mount it on a dedicated port (e.g. 7071) or under an existing mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	return mux
}

// handleHealth writes a simple JSON liveness response.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.startTime).Truncate(time.Second).String()
	resp := map[string]interface{}{
		"ok":      true,
		"uptime":  uptime,
		"version": s.version,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// handleMetrics writes Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	c := s.counters
	uptime := time.Since(s.startTime).Seconds()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP xk_uptime_seconds Seconds since process start.\n")
	fmt.Fprintf(w, "# TYPE xk_uptime_seconds gauge\n")
	fmt.Fprintf(w, "xk_uptime_seconds %.3f\n\n", uptime)

	writeCounter(w, "xk_bytes_in_total",
		"Total bytes received from the DataChannel.", c.BytesIn.Load())
	writeCounter(w, "xk_bytes_out_total",
		"Total bytes sent over the DataChannel.", c.BytesOut.Load())
	writeCounter(w, "xk_streams_opened_total",
		"Total logical streams opened.", c.StreamsOpened.Load())
	writeCounter(w, "xk_streams_closed_total",
		"Total logical streams closed.", c.StreamsClosed.Load())
	writeCounter(w, "xk_frames_sent_total",
		"Total DataChannel frames sent.", c.FramesSent.Load())
	writeCounter(w, "xk_frames_received_total",
		"Total DataChannel frames received.", c.FramesReceived.Load())
	writeCounter(w, "xk_ice_restarts_total",
		"Total ICE restart attempts.", c.ICERestarts.Load())
	writeCounter(w, "xk_xmpp_reconnects_total",
		"Total XMPP reconnection attempts.", c.XMPPReconnects.Load())
	writeCounter(w, "xk_keepalive_timeouts_total",
		"Total keepalive timeout events.", c.KeepaliveTimeouts.Load())

	// Gauge: active streams = opened - closed
	opened := c.StreamsOpened.Load()
	closed := c.StreamsClosed.Load()
	active := int64(opened) - int64(closed)
	if active < 0 {
		active = 0
	}
	fmt.Fprintf(w, "# HELP xk_active_streams Current number of open logical streams.\n")
	fmt.Fprintf(w, "# TYPE xk_active_streams gauge\n")
	fmt.Fprintf(w, "xk_active_streams %d\n", active)
}

func writeCounter(w http.ResponseWriter, name, help string, value uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	fmt.Fprintf(w, "%s %d\n\n", name, value)
}

// Run starts the HTTP server and blocks until ctx cancellation.
func (s *Server) Run(addr string) error {
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}
