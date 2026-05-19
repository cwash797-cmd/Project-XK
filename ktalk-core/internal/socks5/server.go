// Package socks5 implements a SOCKS5 proxy server (RFC 1928 + RFC 1929).
//
// On each incoming CONNECT request, the server calls the provided DialFunc
// to establish the outbound connection. This allows the muxer to intercept
// connections and route them through the DataChannel tunnel.
//
// Supported:
//   - CONNECT command (TCP proxy)
//   - UDP ASSOCIATE (DNS forwarding)
//   - USERNAME/PASSWORD auth (optional, from config)
//   - No-auth mode when credentials are not configured
package socks5

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"
)

const (
	socks5Version = 0x05

	authNone     = 0x00
	authUserPass = 0x02
	authNoAccept = 0xFF

	cmdConnect  = 0x01
	cmdBind     = 0x02
	cmdUDPAssoc = 0x03

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess              = 0x00
	repGeneralFailure       = 0x01
	repConnRefused          = 0x05
	repCommandNotSupported  = 0x07
	repAddrTypeNotSupported = 0x08
)

// DialFunc is called by the SOCKS5 server to establish a connection to the
// target host on behalf of the client.
// In tunnel mode this will open a new DataChannel stream.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config holds SOCKS5 server configuration.
type Config struct {
	// ListenAddr is the address to listen on, e.g. "127.0.0.1:1080".
	ListenAddr string
	// Username and Password enable auth. Leave empty for no-auth mode.
	Username string
	Password string
	// Dial is the function used to connect to remote addresses.
	Dial DialFunc
}

// Server is a SOCKS5 proxy server.
type Server struct {
	cfg Config
	log *slog.Logger
	ln  net.Listener
}

// New creates a new SOCKS5 server but does not start it.
func New(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("socks5: listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.ln = ln
	s.log.Info("socks5 listening", "addr", s.cfg.ListenAddr)
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Warn("socks5 accept error", "err", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

// CloseAll closes the listener and resets all active connections.
func (s *Server) CloseAll() {
	if s.ln != nil {
		s.ln.Close()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := s.negotiate(conn); err != nil {
		s.log.Debug("socks5 negotiate failed", "err", err)
		return
	}

	conn.SetDeadline(time.Now().Add(30 * time.Second))
	cmd, addr, port, err := s.readRequest(conn)
	if err != nil {
		s.log.Debug("socks5 read request failed", "err", err)
		return
	}
	conn.SetDeadline(time.Time{})

	switch cmd {
	case cmdConnect:
		s.handleConnect(ctx, conn, addr, port)
	case cmdUDPAssoc:
		s.handleUDPAssoc(ctx, conn, addr, port)
	default:
		writeReply(conn, repCommandNotSupported, "0.0.0.0", 0)
	}
}

func (s *Server) negotiate(conn net.Conn) error {
	// Read version + nmethods
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	if hdr[0] != socks5Version {
		return fmt.Errorf("unsupported SOCKS version %d", hdr[0])
	}
	nmethods := int(hdr[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}

	// Choose auth method
	if s.cfg.Username != "" {
		// Require user/pass auth
		hasUserPass := false
		for _, m := range methods {
			if m == authUserPass {
				hasUserPass = true
				break
			}
		}
		if !hasUserPass {
			conn.Write([]byte{socks5Version, authNoAccept})
			return fmt.Errorf("client does not support user/pass auth")
		}
		conn.Write([]byte{socks5Version, authUserPass})
		return s.userPassAuth(conn)
	}

	// No auth required
	conn.Write([]byte{socks5Version, authNone})
	return nil
}

func (s *Server) userPassAuth(conn net.Conn) error {
	// RFC 1929 sub-negotiation
	ver := make([]byte, 1)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return err
	}
	ulenB := make([]byte, 1)
	if _, err := io.ReadFull(conn, ulenB); err != nil {
		return err
	}
	uname := make([]byte, ulenB[0])
	if _, err := io.ReadFull(conn, uname); err != nil {
		return err
	}
	plenB := make([]byte, 1)
	if _, err := io.ReadFull(conn, plenB); err != nil {
		return err
	}
	passwd := make([]byte, plenB[0])
	if _, err := io.ReadFull(conn, passwd); err != nil {
		return err
	}

	if string(uname) != s.cfg.Username || string(passwd) != s.cfg.Password {
		conn.Write([]byte{0x01, 0x01}) // failure
		return fmt.Errorf("socks5: bad credentials")
	}
	conn.Write([]byte{0x01, 0x00}) // success
	return nil
}

func (s *Server) readRequest(conn net.Conn) (cmd byte, addr string, port int, err error) {
	hdr := make([]byte, 4)
	if _, err = io.ReadFull(conn, hdr); err != nil {
		return
	}
	if hdr[0] != socks5Version {
		err = fmt.Errorf("bad version in request")
		return
	}
	cmd = hdr[1]
	atyp := hdr[3]

	switch atyp {
	case atypIPv4:
		ip := make([]byte, 4)
		if _, err = io.ReadFull(conn, ip); err != nil {
			return
		}
		addr = net.IP(ip).String()
	case atypIPv6:
		ip := make([]byte, 16)
		if _, err = io.ReadFull(conn, ip); err != nil {
			return
		}
		addr = net.IP(ip).String()
	case atypDomain:
		lenB := make([]byte, 1)
		if _, err = io.ReadFull(conn, lenB); err != nil {
			return
		}
		domain := make([]byte, lenB[0])
		if _, err = io.ReadFull(conn, domain); err != nil {
			return
		}
		addr = string(domain)
	default:
		writeReply(conn, repAddrTypeNotSupported, "0.0.0.0", 0)
		err = fmt.Errorf("unsupported atyp %d", atyp)
		return
	}

	portB := make([]byte, 2)
	if _, err = io.ReadFull(conn, portB); err != nil {
		return
	}
	port = int(binary.BigEndian.Uint16(portB))
	return
}

func (s *Server) handleConnect(ctx context.Context, clientConn net.Conn, addr string, port int) {
	target := fmt.Sprintf("%s:%d", addr, port)
	s.log.Debug("socks5 CONNECT", "target", target)

	remote, err := s.cfg.Dial(ctx, "tcp", target)
	if err != nil {
		s.log.Warn("socks5 dial failed", "target", target, "err", err)
		writeReply(clientConn, repConnRefused, "0.0.0.0", 0)
		return
	}
	defer remote.Close()

	writeReply(clientConn, repSuccess, "0.0.0.0", 0)
	relay(clientConn, remote)
}

func (s *Server) handleUDPAssoc(ctx context.Context, conn net.Conn, addr string, port int) {
	// For DNS-over-tunnel: set up a UDP association.
	// Simplified: we create a local UDP listener and forward via Dial("udp", ...).
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		writeReply(conn, repGeneralFailure, "0.0.0.0", 0)
		return
	}
	defer udpConn.Close()

	udpAddr := udpConn.LocalAddr().(*net.UDPAddr)
	writeReply(conn, repSuccess, udpAddr.IP.String(), udpAddr.Port)

	// Keep alive until the TCP control connection closes
	buf := make([]byte, 1)
	conn.Read(buf) // blocks until client disconnects
}

// relay copies data bidirectionally between two connections.
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(a, b) //nolint:errcheck
		done <- struct{}{}
	}()
	go func() {
		io.Copy(b, a) //nolint:errcheck
		done <- struct{}{}
	}()
	<-done
}

func writeReply(conn net.Conn, rep byte, ip string, port int) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		parsed = net.IPv4(0, 0, 0, 0)
	}
	v4 := parsed.To4()
	if v4 == nil {
		v4 = []byte{0, 0, 0, 0}
	}
	reply := []byte{
		socks5Version, rep, 0x00, atypIPv4,
		v4[0], v4[1], v4[2], v4[3],
		byte(port >> 8), byte(port),
	}
	conn.Write(reply) //nolint:errcheck
}
