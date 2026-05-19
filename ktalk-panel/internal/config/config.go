// Package config manages the ktalk-panel persistent configuration (JSON on disk).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// PanelConfig is the top-level configuration stored in /etc/ktalk-panel/config.json.
type PanelConfig struct {
	// Version is bumped when breaking schema changes are introduced.
	Version int `json:"version"`
	// Port is the HTTP listen port for the panel (before Caddy/nginx).
	Port int `json:"port"`
	// ListenAddr is the bind address.
	ListenAddr string `json:"listen_addr"`
	// Clients is the list of managed tunnel clients.
	Clients []Client `json:"clients"`
	// AdminHash is the bcrypt hash of the admin password.
	AdminHash string `json:"admin_hash,omitempty"`
}

// Client represents one managed tunnel endpoint (one ktalk-core process).
type Client struct {
	// ID is a short random slug, e.g. "alice".
	ID string `json:"id"`
	// Name is a human-readable label shown in the UI.
	Name string `json:"name"`
	// Comment is an optional note for the admin.
	Comment string `json:"comment,omitempty"`
	// Room is the Kontour Talk room parameters.
	Room RoomParams `json:"room"`
	// SharedKey is the 32-byte hex ChaCha20 key for this client.
	SharedKey string `json:"shared_key"`
	// SubToken is the secret token for the /sub/:id/:token endpoint.
	SubToken string `json:"sub_token"`
	// Quota holds bandwidth and traffic limits.
	Quota Quota `json:"quota"`
	// CreatedAt is an ISO-8601 timestamp.
	CreatedAt string `json:"created_at"`
}

// RoomParams describes which ktalk.ru room this client uses.
type RoomParams struct {
	Subdomain string `json:"subdomain"`
	RoomID    string `json:"room_id"`
	Hash      string `json:"hash,omitempty"`
}

// Quota holds per-client resource limits.
type Quota struct {
	// SpeedMbps is the egress speed cap in Mbit/s. 0 = unlimited.
	SpeedMbps int `json:"speed_mbps,omitempty"`
	// TrafficGB is the monthly traffic cap in GiB. 0 = unlimited.
	TrafficGB int `json:"traffic_gb,omitempty"`
	// UsedBytes is the accumulated bytes for the current period.
	UsedBytes uint64 `json:"used_bytes,omitempty"`
	// ExpiresAt is an ISO-8601 date-time after which the client is auto-stopped.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Status returns the quota status string.
func (q Quota) Status() string {
	if q.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, q.ExpiresAt)
		if err == nil && time.Now().After(exp) {
			return "expired"
		}
	}
	if q.TrafficGB > 0 {
		usedGB := float64(q.UsedBytes) / (1 << 30)
		if usedGB >= float64(q.TrafficGB) {
			return "traffic_exceeded"
		}
	}
	return "active"
}

// Store is a thread-safe wrapper around PanelConfig that persists to disk.
type Store struct {
	mu   sync.RWMutex
	cfg  PanelConfig
	path string
}

// Load reads config from the given path, creating a default if absent.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.cfg = defaultConfig()
		return s, s.save()
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if s.cfg.Port == 0 {
		s.cfg.Port = 8888
	}
	if s.cfg.ListenAddr == "" {
		s.cfg.ListenAddr = "127.0.0.1"
	}
	return s, nil
}

// Get returns a deep copy of the current config.
func (s *Store) Get() PanelConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.cfg)
	var out PanelConfig
	_ = json.Unmarshal(b, &out)
	return out
}

// Update applies a mutation function and persists the result.
func (s *Store) Update(fn func(*PanelConfig)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.save()
}

// AddClient appends a new client with generated credentials.
func (s *Store) AddClient(name, comment, subdomain, roomID, hash string, quota Quota) (Client, error) {
	key, err := randHex(32)
	if err != nil {
		return Client{}, fmt.Errorf("generate key: %w", err)
	}
	tok, err := randHex(16)
	if err != nil {
		return Client{}, fmt.Errorf("generate token: %w", err)
	}
	id, err := randHex(4)
	if err != nil {
		return Client{}, fmt.Errorf("generate id: %w", err)
	}

	c := Client{
		ID:      id,
		Name:    name,
		Comment: comment,
		Room: RoomParams{
			Subdomain: subdomain,
			RoomID:    roomID,
			Hash:      hash,
		},
		SharedKey: key,
		SubToken:  tok,
		Quota:     quota,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.Update(func(cfg *PanelConfig) {
		cfg.Clients = append(cfg.Clients, c)
	}); err != nil {
		return Client{}, err
	}
	return c, nil
}

// GetClient returns the client with the given ID, or false if not found.
func (s *Store) GetClient(id string) (Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.cfg.Clients {
		if c.ID == id {
			return c, true
		}
	}
	return Client{}, false
}

// DeleteClient removes a client by ID.
func (s *Store) DeleteClient(id string) error {
	return s.Update(func(cfg *PanelConfig) {
		clients := cfg.Clients[:0]
		for _, c := range cfg.Clients {
			if c.ID != id {
				clients = append(clients, c)
			}
		}
		cfg.Clients = clients
	})
}

// RotateKey generates a new SharedKey for the given client.
func (s *Store) RotateKey(id string) error {
	key, err := randHex(32)
	if err != nil {
		return err
	}
	return s.Update(func(cfg *PanelConfig) {
		for i, c := range cfg.Clients {
			if c.ID == id {
				cfg.Clients[i].SharedKey = key
				return
			}
		}
	})
}

// RotateRoom generates a new RoomID for the given client.
func (s *Store) RotateRoom(id string) error {
	roomID, err := randHex(12)
	if err != nil {
		return err
	}
	return s.Update(func(cfg *PanelConfig) {
		for i, c := range cfg.Clients {
			if c.ID == id {
				cfg.Clients[i].Room.RoomID = roomID
				return
			}
		}
	})
}

// AddUsedBytes increments the traffic counter for a client.
func (s *Store) AddUsedBytes(id string, n uint64) error {
	return s.Update(func(cfg *PanelConfig) {
		for i, c := range cfg.Clients {
			if c.ID == id {
				cfg.Clients[i].Quota.UsedBytes += n
				return
			}
		}
	})
}

// SetAdminHash stores the bcrypt-hashed admin password.
func (s *Store) SetAdminHash(hash string) error {
	return s.Update(func(cfg *PanelConfig) {
		cfg.AdminHash = hash
	})
}

// GetAdminHash returns the stored admin password hash.
func (s *Store) GetAdminHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.AdminHash
}

// save writes the config to disk (must be called with s.mu held).
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("config: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("config: rename: %w", err)
	}
	return nil
}

func defaultConfig() PanelConfig {
	return PanelConfig{
		Version:    1,
		Port:       8888,
		ListenAddr: "127.0.0.1",
		Clients:    []Client{},
	}
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
