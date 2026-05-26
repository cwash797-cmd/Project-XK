// ktalk-core — tunnel daemon.
//
// Usage:
//
//	ktalk-core serve   -config /etc/xk/config.yaml          # Creator side (egress)
//	ktalk-core connect -config /etc/xk/config.yaml          # Joiner side (proxy)
//	ktalk-core version
//
// Config file is YAML; see internal/config for field docs.
// The API server always starts on cfg.API.ListenAddr (default 127.0.0.1:7070).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/carrier"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/config"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/crypto"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/metrics"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/socks5"
)

const version = "0.1.0-sprint1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runMode("creator", os.Args[2:])
	case "connect":
		runMode("joiner", os.Args[2:])
	case "version":
		fmt.Println("ktalk-core", version)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `ktalk-core %s

Commands:
  serve    -config FILE   Start Creator (egress) side
  connect  -config FILE   Start Joiner (SOCKS5 proxy) side
  version                 Print version

`, version)
}

// runMode starts the daemon in either creator or joiner mode.
func runMode(mode string, args []string) {
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	cfgPath := fs.String("config", "", "Path to YAML config file (required)")
	debug := fs.Bool("debug", false, "Enable debug logging")
	metricsAddr := fs.String("metrics-addr", "127.0.0.1:7071", "Address for /health and /metrics (empty to disable)")
	fs.Parse(args) //nolint:errcheck

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "error: -config is required")
		os.Exit(1)
	}

	// Logging
	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(log)

	// Load config
	cfg, err := config.LoadFile(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	cfg.Mode = config.Mode(mode)
	if *debug {
		cfg.Debug = true
	}

	log.Info("starting", "mode", mode, "version", version,
		"room", cfg.Room.RoomID, "subdomain", cfg.Room.Subdomain)

	// Build cipher
	cipher, err := crypto.NewFromHex(cfg.Crypto.Key)
	if err != nil {
		log.Error("init cipher", "err", err)
		os.Exit(1)
	}

	// Context with graceful shutdown on SIGTERM / SIGINT
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Start metrics server if requested.
	if *metricsAddr != "" {
		metSrv := metrics.New(version, metrics.Global)
		go func() {
			log.Info("metrics server starting", "addr", *metricsAddr)
			if err := metSrv.Run(*metricsAddr); err != nil {
				log.Warn("metrics server stopped", "err", err)
			}
		}()
	}

	// Create carrier
	c := carrier.New(cfg, cipher, log.With("component", "carrier"))

	switch mode {
	case "creator":
		runCreator(ctx, cfg, c, log)
	case "joiner":
		runJoiner(ctx, cfg, c, log)
	}

	log.Info("shutdown complete")
}

// runCreator starts the Creator side: carrier + API server.
// The Creator receives SOCKS5 CONNECT requests from the Joiner over the DataChannel
// and proxies them out to the real internet.
func runCreator(ctx context.Context, cfg *config.Config, c *carrier.Carrier, log *slog.Logger) {
	errCh := make(chan error, 1)
	go func() { errCh <- c.Connect(ctx) }()

	log.Info("creator running — waiting for joiner connections")
	select {
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			log.Error("carrier error", "err", err)
		}
	case <-ctx.Done():
		log.Info("shutdown signal received")
		c.Close()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}
}

// runJoiner starts the Joiner side: carrier + local SOCKS5 listener.
// The Joiner exposes a SOCKS5 proxy on cfg.SOCKS5.ListenAddr that forwards
// all connections through the DataChannel tunnel to the Creator.
func runJoiner(ctx context.Context, cfg *config.Config, c *carrier.Carrier, log *slog.Logger) {
	addr := cfg.SOCKS5.ListenAddr
	if addr == "" {
		addr = "127.0.0.1:1080"
	}

	// Wait until the DataChannel is open before starting the proxy listener.
	dcReady := make(chan struct{})
	c.SetOnDCOpen(func() {
		select {
		case dcReady <- struct{}{}:
		default:
		}
	})

	carrierErrCh := make(chan error, 1)
	go func() { carrierErrCh <- c.Connect(ctx) }()

	log.Info("joiner: waiting for datachannel to open…")
	select {
	case <-dcReady:
		log.Info("datachannel ready — starting SOCKS5", "addr", addr)
	case err := <-carrierErrCh:
		if err != nil && ctx.Err() == nil {
			log.Error("carrier failed before DC opened", "err", err)
		}
		return
	case <-ctx.Done():
		c.Close()
		return
	}

	// Build SOCKS5 server using carrier.Dial as the tunnel DialFunc.
	s5cfg := socks5.Config{
		ListenAddr: addr,
		Username:   cfg.SOCKS5.Username,
		Password:   cfg.SOCKS5.Password,
		Dial:       c.Dial,
	}
	srv := socks5.New(s5cfg, log.With("component", "socks5"))

	s5ErrCh := make(chan error, 1)
	go func() { s5ErrCh <- srv.Run(ctx) }()

	log.Info("joiner ready", "socks5", addr)

	select {
	case err := <-carrierErrCh:
		if err != nil && ctx.Err() == nil {
			log.Error("carrier error", "err", err)
		}
		srv.CloseAll()
	case err := <-s5ErrCh:
		if err != nil && ctx.Err() == nil {
			log.Error("socks5 error", "err", err)
		}
		c.Close()
	case <-ctx.Done():
		log.Info("shutdown signal")
		c.Close()
		srv.CloseAll()
		select {
		case <-carrierErrCh:
		case <-time.After(5 * time.Second):
		}
	}
}
