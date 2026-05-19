package supervisor

import (
	"context"
	"log/slog"
	"time"

	"github.com/private/ktalk-panel/internal/config"
)

// KeyRotationInterval is how often each client's encryption key is automatically
// rotated. The rotation is done out-of-band: the supervisor restarts the process
// with a freshly generated key (in-band DataChannel rotation is handled by
// ktalk-core when both sides are connected; here we take the safe offline path
// so that the panel always has the canonical key on disk).
const KeyRotationInterval = 24 * time.Hour

// KeyRotator runs a background goroutine that rotates all client keys every
// KeyRotationInterval and restarts their processes.
type KeyRotator struct {
	store *config.Store
	sup   *Supervisor
	log   *slog.Logger
}

// NewKeyRotator creates a KeyRotator.
func NewKeyRotator(store *config.Store, sup *Supervisor, log *slog.Logger) *KeyRotator {
	return &KeyRotator{store: store, sup: sup, log: log}
}

// Run starts the rotation loop. It blocks until ctx is cancelled.
// Call in a goroutine: go rotator.Run(ctx).
func (r *KeyRotator) Run(ctx context.Context) {
	ticker := time.NewTicker(KeyRotationInterval)
	defer ticker.Stop()

	r.log.Info("key rotator started", "interval", KeyRotationInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			r.log.Info("scheduled key rotation tick", "at", t.Format(time.RFC3339))
			r.rotateAll(ctx)
		}
	}
}

// rotateAll rotates keys for every client and restarts the affected processes.
func (r *KeyRotator) rotateAll(ctx context.Context) {
	cfg := r.store.Get()
	for _, c := range cfg.Clients {
		if err := r.rotateOne(ctx, c); err != nil {
			r.log.Error("key rotation failed", "client", c.ID, "err", err)
		}
	}
}

func (r *KeyRotator) rotateOne(ctx context.Context, c config.Client) error {
	// Generate new key on disk.
	if err := r.store.RotateKey(c.ID); err != nil {
		return err
	}
	// Reload fresh client record (has new key).
	fresh, ok := r.store.GetClient(c.ID)
	if !ok {
		return nil
	}
	r.log.Info("key rotated, restarting process", "client", c.ID)
	// Restart process with new config. Ignore "not managed" — process may not
	// have been running; Start will launch it fresh.
	return r.sup.Start(ctx, fresh)
}
