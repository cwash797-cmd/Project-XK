// Package sse implements a Server-Sent Events broker for the admin panel.
//
// The broker pushes JSON events to all connected browser clients whenever
// the supervisor state or process logs change.
//
// Event types:
//
//	state  — full []ProcessState snapshot (every 2 s)
//	log    — single LogLine for a specific client {"client_id":"...","t":"...","line":"..."}
//	ping   — keepalive comment line (every 15 s)
package sse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	stateInterval = 2 * time.Second
	pingInterval  = 15 * time.Second
	chanBuf       = 64
)

// Event is a single SSE message.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// subscriber holds the per-connection send channel.
type subscriber struct {
	ch chan string
}

// Broker fans out events to all connected SSE subscribers.
type Broker struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
	log  *slog.Logger
}

// New creates a new Broker. Call Run() to start the state-push loop.
func New(log *slog.Logger) *Broker {
	return &Broker{
		subs: make(map[*subscriber]struct{}),
		log:  log,
	}
}

// Run starts the periodic state broadcaster.
// stateFn is called every stateInterval and the result is broadcast as a "state" event.
func (b *Broker) Run(stateFn func() any) {
	stateTicker := time.NewTicker(stateInterval)
	pingTicker := time.NewTicker(pingInterval)
	defer stateTicker.Stop()
	defer pingTicker.Stop()

	for {
		select {
		case <-stateTicker.C:
			b.Publish(Event{Type: "state", Data: stateFn()})
		case <-pingTicker.C:
			b.publishRaw(": ping\n\n")
		}
	}
}

// Publish encodes an event to JSON and broadcasts it to all subscribers.
func (b *Broker) Publish(e Event) {
	data, err := json.Marshal(e)
	if err != nil {
		b.log.Warn("sse marshal failed", "err", err)
		return
	}
	b.publishRaw(fmt.Sprintf("data: %s\n\n", data))
}

// PublishLog broadcasts a single log line for a specific client.
func (b *Broker) PublishLog(clientID, line string) {
	b.Publish(Event{
		Type: "log",
		Data: map[string]string{
			"client_id": clientID,
			"t":         time.Now().UTC().Format(time.RFC3339),
			"line":      line,
		},
	})
}

// publishRaw sends raw SSE text to all subscribers.
func (b *Broker) publishRaw(msg string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		select {
		case s.ch <- msg:
		default:
			// Subscriber is slow — drop the event.
		}
	}
}

// ServeHTTP implements http.Handler — each request becomes a long-lived SSE stream.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sub := &subscriber{ch: make(chan string, chanBuf)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	b.log.Debug("sse subscriber connected", "remote", r.RemoteAddr)

	defer func() {
		b.mu.Lock()
		delete(b.subs, sub)
		b.mu.Unlock()
		b.log.Debug("sse subscriber disconnected", "remote", r.RemoteAddr)
	}()

	// Send initial retry hint
	fmt.Fprintf(w, "retry: 3000\n\n")
	flusher.Flush()

	for {
		select {
		case msg := <-sub.ch:
			fmt.Fprint(w, msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
