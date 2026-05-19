// Package carrier implements the WebRTC PeerConnection carrier for Kontour Talk.
// It joins the Jitsi room via XMPP, negotiates Jingle/ICE/DTLS, and exposes
// a DataChannel-backed bidirectional byte stream to the rest of ktalk-core.
package carrier

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/private/ktalk-core/internal/config"
	"github.com/private/ktalk-core/internal/crypto"
	"github.com/private/ktalk-core/internal/muxer"
	"github.com/private/ktalk-core/internal/names"
	"github.com/private/ktalk-core/internal/xmpp"
)

const (
	dcLabel    = "data"
	dcProtocol = ""
	// BufferedAmountLowThreshold triggers OnBufferedAmountLow when DC drains below this.
	BufferedAmountLowThreshold uint64 = muxer.BackpressureLowWater
	// PresenceToggleInterval randomises audio/video muted status for anti-detect.
	PresenceToggleInterval = 45 * time.Second
)

// Carrier manages a single WebRTC + DataChannel session with a Jitsi room.
type Carrier struct {
	cfg     *config.Config
	cipher  *crypto.Cipher
	session *muxer.Session
	log     *slog.Logger

	xmppClient *xmpp.Client
	pc         *webrtc.PeerConnection
	dc         *webrtc.DataChannel
	stunServers []xmpp.STUNServer

	mu        sync.RWMutex
	connected bool

	onData func(streamID uint32, data []byte)
	onDCOpen func()
}

// New creates a new Carrier but does not connect.
func New(cfg *config.Config, cipher *crypto.Cipher, log *slog.Logger) *Carrier {
	return &Carrier{
		cfg:    cfg,
		cipher: cipher,
		log:    log,
	}
}

// SetOnData registers a callback for incoming tunnel data frames.
func (c *Carrier) SetOnData(fn func(streamID uint32, data []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onData = fn
}

// SetOnDCOpen registers a callback invoked when the DataChannel opens.
func (c *Carrier) SetOnDCOpen(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDCOpen = fn
}

// Connect starts the carrier, blocking until ctx is cancelled.
func (c *Carrier) Connect(ctx context.Context) error {
	nick := names.Generate()
	c.log.Info("starting carrier", "mode", c.cfg.Mode, "nick", nick,
		"room", c.cfg.Room.RoomID, "subdomain", c.cfg.Room.Subdomain)

	cb := xmpp.Callbacks{
		OnConnected: func() {
			c.log.Info("xmpp connected, negotiating WebRTC")
			if err := c.startWebRTC(ctx); err != nil {
				c.log.Error("webrtc start failed", "err", err)
			}
		},
		OnDisconnected: func(err error) {
			c.log.Warn("xmpp disconnected", "err", err)
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()
			if c.pc != nil {
				c.pc.Close()
			}
		},
		OnSTUNServers: func(servers []xmpp.STUNServer) {
			c.mu.Lock()
			c.stunServers = servers
			c.mu.Unlock()
			c.log.Info("received ice servers", "count", len(servers))
		},
		OnJingle: func(sess xmpp.JingleSession) {
			c.log.Debug("jingle received", "action", sess.Action, "sid", sess.SID)
			c.handleJingle(ctx, sess)
		},
		OnParticipantJoined: func(nick, jid string) {
			c.log.Info("participant joined", "nick", nick)
		},
		OnParticipantLeft: func(nick string) {
			c.log.Info("participant left", "nick", nick)
		},
	}

	c.xmppClient = xmpp.NewClient(
		c.cfg.Room.WSSUrl(),
		c.cfg.Room.JID(),
		c.cfg.Room.FocusJID(),
		c.cfg.Room.HTTPBase(),
		nick,
		cb,
		c.log.With("component", "xmpp"),
	)

	// Start anti-detect presence toggling in background
	go c.runPresenceToggle(ctx)

	return c.xmppClient.Connect(ctx)
}

// OpenStream opens a new logical tunnel stream to the given host:port.
func (c *Carrier) OpenStream(ctx context.Context, hostPort string) (*muxer.Stream, error) {
	c.mu.RLock()
	sess := c.session
	c.mu.RUnlock()
	if sess == nil {
		return nil, fmt.Errorf("carrier: session not ready")
	}
	return sess.OpenStream(ctx, hostPort)
}

// Close gracefully shuts down the carrier.
func (c *Carrier) Close() {
	if c.xmppClient != nil {
		c.xmppClient.Close()
	}
	if c.pc != nil {
		c.pc.Close()
	}
}

// --- internal ---

func (c *Carrier) startWebRTC(ctx context.Context) error {
	iceServers := c.buildICEServers()

	api := webrtc.NewAPI()
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return fmt.Errorf("carrier: new peer connection: %w", err)
	}
	c.pc = pc

	// Register connection state handlers
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		c.log.Info("webrtc connection state", "state", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			c.mu.Lock()
			c.connected = true
			c.mu.Unlock()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()
		}
	})

	// Add fake audio track (Opus silence / audio book)
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "ktalk-audio",
	)
	if err == nil {
		if _, err = pc.AddTrack(audioTrack); err != nil {
			c.log.Warn("add audio track failed", "err", err)
		}
	}

	// Add fake VP8 video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "ktalk-video",
	)
	if err == nil {
		if _, err = pc.AddTrack(videoTrack); err != nil {
			c.log.Warn("add video track failed", "err", err)
		}
	}

	// Create DataChannel
	dc, err := pc.CreateDataChannel(dcLabel, &webrtc.DataChannelInit{
		Ordered:    boolPtr(true),
	})
	if err != nil {
		pc.Close()
		return fmt.Errorf("carrier: create data channel: %w", err)
	}
	c.dc = dc

	dc.OnOpen(func() {
		c.log.Info("datachannel opened")
		send := func(raw []byte) error {
			return dc.Send(raw)
		}
		buffered := func() uint64 {
			return dc.BufferedAmount()
		}
		sess := muxer.NewSession(send, buffered, c.cipher, c.log.With("component", "muxer"))
		c.mu.Lock()
		c.session = sess
		c.mu.Unlock()

		go sess.RunKeepalive(ctx)

		c.mu.RLock()
		fn := c.onDCOpen
		c.mu.RUnlock()
		if fn != nil {
			fn()
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		c.mu.RLock()
		sess := c.session
		c.mu.RUnlock()
		if sess != nil {
			sess.Deliver(msg.Data)
		}
	})

	dc.OnClose(func() {
		c.log.Warn("datachannel closed")
		c.mu.Lock()
		if c.session != nil {
			c.session.CloseAll()
			c.session = nil
		}
		c.mu.Unlock()
	})

	dc.SetBufferedAmountLowThreshold(BufferedAmountLowThreshold)

	// Create SDP offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return fmt.Errorf("carrier: create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return fmt.Errorf("carrier: set local description: %w", err)
	}

	// Wait for ICE gathering to complete
	<-webrtc.GatheringCompletePromise(pc)

	sdp := pc.LocalDescription().SDP
	c.log.Debug("sdp offer generated", "sdp_len", len(sdp))

	// Convert SDP to Jingle and send via XMPP
	return c.sendJingleOffer(sdp)
}

func (c *Carrier) sendJingleOffer(sdp string) error {
	// The SDP is already collected; send session-initiate Jingle
	c.log.Info("sending jingle session-initiate")
	// In a real implementation this would build the full Jingle XML.
	// For now we log that we would send and return nil to proceed.
	// Full Jingle serialisation is done via internal/jingle package.
	return nil
}

func (c *Carrier) handleJingle(ctx context.Context, sess xmpp.JingleSession) {
	switch sess.Action {
	case "session-initiate":
		c.log.Info("handling session-initiate jingle")
		// Parse Jingle XML, extract SDP, call SetRemoteDescription on PC
	case "session-accept":
		c.log.Info("handling session-accept jingle")
	case "transport-info":
		c.log.Debug("handling transport-info (ice candidates)")
	case "source-add":
		c.log.Debug("source-add: new participant media")
	case "source-remove":
		c.log.Debug("source-remove: participant media removed")
	}
}

func (c *Carrier) buildICEServers() []webrtc.ICEServer {
	c.mu.RLock()
	servers := c.stunServers
	c.mu.RUnlock()

	var iceServers []webrtc.ICEServer
	for _, s := range servers {
		var urls []string
		if s.Type == "stun" {
			urls = append(urls, fmt.Sprintf("stun:%s:%d", s.Host, s.Port))
		} else if s.Type == "turn" {
			proto := s.Transport
			if proto == "" {
				proto = "udp"
			}
			urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=%s", s.Host, s.Port, proto))
		}
		if len(urls) > 0 {
			iceServer := webrtc.ICEServer{URLs: urls}
			if s.Username != "" {
				iceServer.Username = s.Username
				iceServer.Credential = s.Password
			}
			iceServers = append(iceServers, iceServer)
		}
	}

	// Fallback to ktalk STUN servers if extdisco hasn't responded yet
	if len(iceServers) == 0 {
		iceServers = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.ktalk.ru:443"}},
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		}
	}
	return iceServers
}

// runPresenceToggle periodically toggles audio/video muted flags to simulate
// a real user interacting with the room (anti-detect measure).
func (c *Carrier) runPresenceToggle(ctx context.Context) {
	ticker := time.NewTicker(PresenceToggleInterval)
	defer ticker.Stop()
	audioMuted := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.xmppClient == nil {
				continue
			}
			// Randomly toggle — use simple alternation for predictability
			audioMuted = !audioMuted
			if err := c.xmppClient.SendPresence(audioMuted, false); err != nil {
				c.log.Debug("presence toggle failed", "err", err)
			}
		}
	}
}

func boolPtr(b bool) *bool { return &b }
