// Command ktalk-core is the tunnel process for the ktalk private relay service.
//
// Usage (Creator side — runs on a VPS outside RU):
//
//	ktalk-core -config /etc/ktalk-panel/clients/alice.yaml
//
// Usage (Joiner side — runs on the user's device):
//
//	ktalk-core -uri "ktalk://<base64>"
//
// The URI can also be loaded from a subscription URL:
//
//	ktalk-core -sub "https://panel.example.com/sub/alice/secret-token"
//
// All references to upstream projects have been removed from this binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/private/ktalk-core/internal/carrier"
	"github.com/private/ktalk-core/internal/config"
	"github.com/private/ktalk-core/internal/crypto"
	"github.com/private/ktalk-core/internal/socks5"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ktalk-core: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		flagConfig string
		flagURI    string
		flagSub    string
		flagDebug  bool
	)
	flag.StringVar(&flagConfig, "config", "", "path to YAML config file (Creator mode)")
	flag.StringVar(&flagURI, "uri", "", "ktalk:// URI config (Joiner mode)")
	flag.StringVar(&flagSub, "sub", "", "subscription URL (fetches URI on startup)")
	flag.BoolVar(&flagDebug, "debug", false, "enable verbose logging")
	flag.Parse()

	// Configure structured logger
	logLevel := slog.LevelInfo
	if flagDebug {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	// Load config
	cfg, err := loadConfig(flagConfig, flagURI, flagSub, log)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}

	// Initialise cipher
	cipher, err := crypto.NewFromHex(cfg.Crypto.Key)
	if err != nil {
		return fmt.Errorf("crypto init: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := carrier.New(cfg, cipher, log.With("component", "carrier"))

	// Joiner mode: start SOCKS5 proxy
	if cfg.Mode == config.ModeJoiner {
		socksCfg := socks5.Config{
			ListenAddr: cfg.SOCKS5.ListenAddr,
			Username:   cfg.SOCKS5.Username,
			Password:   cfg.SOCKS5.Password,
			Dial: func(dialCtx context.Context, network, addr string) (interface {
				io.Reader
				io.Writer
				io.Closer
			}, error) {
				return c.OpenStream(dialCtx, addr)
			},
		}
		_ = socksCfg
		// SOCKS5 server construction — Dial signature reconciled when muxer.Stream
		// implements net.Conn (Sprint 4 → Sprint 5 integration).
		// For now, start a basic server without tunnel dial.
		basicCfg := socks5.Config{
			ListenAddr: cfg.SOCKS5.ListenAddr,
			Username:   cfg.SOCKS5.Username,
			Password:   cfg.SOCKS5.Password,
		}
		sock5 := socks5.New(basicCfg, log.With("component", "socks5"))
		go func() {
			if err := sock5.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("socks5 server error", "err", err)
			}
		}()
		log.Info("socks5 proxy started", "addr", cfg.SOCKS5.ListenAddr)
	}

	log.Info("ktalk-core starting",
		"mode", cfg.Mode,
		"room", cfg.Room.RoomID,
		"subdomain", cfg.Room.Subdomain,
	)

	return c.Connect(ctx)
}

func loadConfig(cfgPath, uri, subURL string, log *slog.Logger) (*config.Config, error) {
	switch {
	case subURL != "":
		log.Info("fetching subscription", "url", subURL)
		cfg, _, err := config.FetchSubscription(subURL, httpGet)
		if err != nil {
			return nil, fmt.Errorf("fetch subscription %s: %w", subURL, err)
		}
		return cfg, nil

	case uri != "":
		return config.DecodeURI(uri)

	case cfgPath != "":
		// TODO: implement YAML loading (requires gopkg.in/yaml.v3 in go.mod)
		return nil, fmt.Errorf("YAML config loading not yet implemented — use -uri flag")

	default:
		// Try env variable
		if envURI := os.Getenv("KTALK_URI"); envURI != "" {
			return config.DecodeURI(envURI)
		}
		if envSub := os.Getenv("KTALK_SUB"); envSub != "" {
			cfg, _, err := config.FetchSubscription(envSub, httpGet)
			return cfg, err
		}
		return nil, fmt.Errorf("one of -config, -uri, or -sub is required")
	}
}

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return string(body), nil
}
