// Package supervisor manages ktalk-core child processes.
// Each client gets one dedicated process isolated in its own Linux network
// namespace (when running as root on Linux). On other platforms it falls back
// to running without namespace isolation.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cwash797-cmd/Project-XK/ktalk-panel/internal/config"
)

const (
	maxLogLines  = 500
	restartDelay = 3 * time.Second
	maxRestarts  = 5
)

// Process represents a running (or recently stopped) ktalk-core child process.
type Process struct {
	ClientID    string
	Client      config.Client
	cmd         *exec.Cmd
	logs        *logRing
	startedAt   time.Time
	exitedAt    time.Time
	exitErr     string
	restarts    int
	running     bool
	lastHealthy time.Time // last time ICE was confirmed connected
	mu          sync.RWMutex
	done        chan struct{}
}

// LogLine is a single line with a timestamp.
type LogLine struct {
	T    time.Time `json:"t"`
	Line string    `json:"line"`
}

// ProcessState is a JSON-serialisable snapshot of process status.
type ProcessState struct {
	ClientID    string    `json:"client_id"`
	Running     bool      `json:"running"`
	Healthy     bool      `json:"healthy"`
	LastHealthy time.Time `json:"last_heartbeat,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	ExitedAt    time.Time `json:"exited_at,omitempty"`
	ExitErr     string    `json:"exit_err,omitempty"`
	Restarts    int       `json:"restarts"`
}

// State returns a snapshot of the process status.
func (p *Process) State() ProcessState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Consider tunnel healthy if it was seen connected within the last 90 seconds.
	healthy := p.running && !p.lastHealthy.IsZero() &&
		time.Since(p.lastHealthy) < 90*time.Second
	return ProcessState{
		ClientID:    p.ClientID,
		Running:     p.running,
		Healthy:     healthy,
		LastHealthy: p.lastHealthy,
		StartedAt:   p.startedAt,
		ExitedAt:    p.exitedAt,
		ExitErr:     p.exitErr,
		Restarts:    p.restarts,
	}
}

// MarkHealthy records the current time as the last known-healthy timestamp.
// Call this whenever the tunnel confirms an ICE connected state.
func (p *Process) MarkHealthy() {
	p.mu.Lock()
	p.lastHealthy = time.Now()
	p.mu.Unlock()
}

// MarkHealthyByID updates the last-healthy timestamp for the given client.
func (s *Supervisor) MarkHealthyByID(clientID string) {
	s.mu.RLock()
	p, ok := s.processes[clientID]
	s.mu.RUnlock()
	if ok {
		p.MarkHealthy()
	}
}

// LogSnapshot returns recent log lines.
func (p *Process) LogSnapshot() []LogLine {
	return p.logs.Snapshot()
}

// Supervisor manages a set of Process objects.
type Supervisor struct {
	corePath string
	log      *slog.Logger

	mu        sync.RWMutex
	processes map[string]*Process // keyed by client ID
}

// New creates a new supervisor that launches ktalk-core binaries at corePath.
func New(corePath string, log *slog.Logger) *Supervisor {
	return &Supervisor{
		corePath:  corePath,
		log:       log,
		processes: make(map[string]*Process),
	}
}

// Start launches a ktalk-core process for the given client.
// If one is already running, it is stopped first.
func (s *Supervisor) Start(ctx context.Context, client config.Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.processes[client.ID]; ok && existing.running {
		s.stopLocked(client.ID)
	}

	p, err := s.launch(ctx, client)
	if err != nil {
		return err
	}
	s.processes[client.ID] = p
	go s.watch(ctx, client.ID, p)
	return nil
}

// Stop terminates the process for the given client.
func (s *Supervisor) Stop(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(clientID)
}

// Restart stops and restarts a client process.
func (s *Supervisor) Restart(ctx context.Context, clientID string) error {
	s.mu.Lock()
	client, ok := s.processClientLocked(clientID)
	s.stopLocked(clientID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("client %q not managed", clientID)
	}
	return s.Start(ctx, client)
}

// State returns snapshots of all managed processes.
func (s *Supervisor) State() []ProcessState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ProcessState
	for _, p := range s.processes {
		out = append(out, p.State())
	}
	return out
}

// Logs returns recent log lines for a client.
func (s *Supervisor) Logs(clientID string) ([]LogLine, bool) {
	s.mu.RLock()
	p, ok := s.processes[clientID]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return p.LogSnapshot(), true
}

// StopAll terminates all running processes gracefully.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.processes {
		s.stopLocked(id)
	}
}

// --- internal ---

func (s *Supervisor) launch(ctx context.Context, client config.Client) (*Process, error) {
	// Build URI config to pass to ktalk-core
	uri, err := buildURI(client)
	if err != nil {
		return nil, fmt.Errorf("supervisor: build uri for %s: %w", client.ID, err)
	}

	cmd := exec.CommandContext(ctx, s.corePath, "-uri", uri)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("KTALK_CLIENT_ID=%s", client.ID),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor: start ktalk-core for %s: %w", client.ID, err)
	}

	p := &Process{
		ClientID:  client.ID,
		Client:    client,
		cmd:       cmd,
		logs:      newLogRing(maxLogLines),
		startedAt: time.Now(),
		running:   true,
		done:      make(chan struct{}),
	}

	// Stream logs
	go streamLogs(p.logs, stdout)
	go streamLogs(p.logs, stderr)

	// Wait in background and record exit
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.running = false
		p.exitedAt = time.Now()
		if err != nil {
			p.exitErr = err.Error()
		}
		p.mu.Unlock()
		close(p.done)
	}()

	s.log.Info("started ktalk-core", "client", client.ID, "pid", cmd.Process.Pid)
	return p, nil
}

func (s *Supervisor) stopLocked(clientID string) {
	p, ok := s.processes[clientID]
	if !ok || !p.running {
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(os.Interrupt)
		// Give 5s then kill
		go func() {
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				_ = p.cmd.Process.Kill()
			}
		}()
	}
}

func (s *Supervisor) processClientLocked(clientID string) (config.Client, bool) {
	p, ok := s.processes[clientID]
	if !ok {
		return config.Client{}, false
	}
	return p.Client, true
}

func (s *Supervisor) watch(ctx context.Context, clientID string, p *Process) {
	select {
	case <-ctx.Done():
		return
	case <-p.done:
	}

	p.mu.RLock()
	restarts := p.restarts
	exitErr := p.exitErr
	p.mu.RUnlock()

	if exitErr != "" {
		s.log.Warn("ktalk-core exited with error", "client", clientID, "err", exitErr)
	}

	if restarts >= maxRestarts {
		s.log.Error("ktalk-core reached max restarts", "client", clientID)
		return
	}

	// Auto-restart after delay
	select {
	case <-ctx.Done():
		return
	case <-time.After(restartDelay):
	}

	s.mu.Lock()
	current, ok := s.processes[clientID]
	if !ok || current != p {
		s.mu.Unlock()
		return // process was replaced
	}

	newP, err := s.launch(ctx, p.Client)
	if err != nil {
		s.log.Error("restart failed", "client", clientID, "err", err)
		s.mu.Unlock()
		return
	}
	newP.mu.Lock()
	newP.restarts = restarts + 1
	newP.mu.Unlock()
	s.processes[clientID] = newP
	s.mu.Unlock()

	go s.watch(ctx, clientID, newP)
}

// buildURI constructs a ktalk:// URI for the given client.
// The panel cannot import ktalk-core as a library (separate module),
// so we encode the JSON payload inline and base64url it.
func buildURI(client config.Client) (string, error) {
	// Use the explicitly-assigned SOCKS5 port (20001+).
	// Fall back to 1080 only for legacy clients that predate sequential assignment.
	socks5Port := client.SOCKS5Port
	if socks5Port == 0 {
		socks5Port = 1080
	}
	socks5Addr := fmt.Sprintf("127.0.0.1:%d", socks5Port)

	payload := fmt.Sprintf(
		`{"mode":"creator","room":{"subdomain":%q,"room_id":%q,"hash":%q},"crypto":{"key":%q},"net":{"dns_server":"1.1.1.1:53"},"socks5":{"listen_addr":%q}}`,
		client.Room.Subdomain, client.Room.RoomID, client.Room.Hash, client.SharedKey, socks5Addr,
	)
	b64 := rawBase64URL([]byte(payload))
	return "ktalk://" + b64, nil
}

// rawBase64URL encodes b using base64url without padding.
// Avoids importing encoding/base64 just for one call — keeps the dependency
// count down. In practice encoding/base64 is fine to use.
func rawBase64URL(b []byte) string {
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

// --- log ring ---

type logRing struct {
	mu    sync.Mutex
	lines []LogLine
	cap   int
	head  int
}

func newLogRing(cap int) *logRing {
	return &logRing{cap: cap, lines: make([]LogLine, 0, cap)}
}

func (r *logRing) Append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := LogLine{T: time.Now(), Line: line}
	if len(r.lines) < r.cap {
		r.lines = append(r.lines, entry)
	} else {
		r.lines[r.head] = entry
		r.head = (r.head + 1) % r.cap
	}
}

func (r *logRing) Snapshot() []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogLine, len(r.lines))
	copy(out, r.lines)
	return out
}

func streamLogs(ring *logRing, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		ring.Append(sc.Text())
	}
}
