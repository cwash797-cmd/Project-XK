// Package carrier implements the WebRTC PeerConnection carrier.
// It joins the room via XMPP, negotiates Jingle/ICE/DTLS, and exposes
// a DataChannel-backed multiplexed byte-stream session.
//
// State machine:
//
//	XMPP connects → OnConnected → startWebRTC → ICE gathers → sendJingleOffer
//	→ peer sends session-accept → SetRemoteDescription → DC opens → ready
//	→ (on DC failure) → ICE restart → new offer → transport-replace
//	→ (on XMPP drop) → XMPP reconnect loop (in xmpp.Client) → repeat
package carrier

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/config"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/crypto"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/jingle"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/muxer"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/names"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/roomapi"
	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/xmpp"
	"github.com/pion/webrtc/v4"
)

const (
	dcLabel   = "data"
	dcOrdered = true

	// BufferedAmountLowThreshold triggers flow-control resume.
	BufferedAmountLowThreshold uint64 = muxer.BackpressureLowWater

	// PresenceToggleInterval randomises audio/video muted flags (anti-detect).
	PresenceToggleInterval = 47 * time.Second

	// ICERestartDelay is the pause before triggering an ICE restart.
	ICERestartDelay = 2 * time.Second

	// maxICERestarts limits consecutive restart attempts before giving up.
	maxICERestarts = 5
)

// Carrier manages a single WebRTC+DataChannel session with a room.
type Carrier struct {
	cfg    *config.Config
	cipher *crypto.Cipher
	log    *slog.Logger

	xmppClient *xmpp.Client
	xmppJID    string // our full JID after bind
	focusJID   string // Jicofo JID for this session
	currentSID string // active Jingle SID

	mu          sync.RWMutex
	pc          *webrtc.PeerConnection
	dc          *webrtc.DataChannel
	session     *muxer.Session
	stunServers []xmpp.STUNServer
	connected   bool

	iceRestarts atomic.Int32
	sessionCtx  context.Context // cancelled when the whole session ends
	sessionStop context.CancelFunc

	onDCOpen      func()
	onKeyRotateFn func(string) // persisted across session rebuilds
}

// New creates a Carrier. Call Connect to start it.
func New(cfg *config.Config, cipher *crypto.Cipher, log *slog.Logger) *Carrier {
	return &Carrier{
		cfg:    cfg,
		cipher: cipher,
		log:    log,
	}
}

// SetOnDCOpen registers a callback invoked when the DataChannel first opens.
func (c *Carrier) SetOnDCOpen(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDCOpen = fn
}

// Connect starts the carrier and blocks until ctx is cancelled.
//
// Connect performs the anonymous join flow before starting XMPP:
//  1. GET /api/rooms/<roomName> → receive conferenceId + ngtoken cookie
//  2. If config.Room.ConferenceID is already set (e.g. from saved config),
//     skip the API fetch to avoid unnecessary HTTP round-trip.
//  3. Build XMPP client with the acquired cookie jar.
func (c *Carrier) Connect(ctx context.Context) error {
	sCtx, stop := context.WithCancel(ctx)
	c.sessionCtx = sCtx
	c.sessionStop = stop
	defer stop()

	nick := names.Generate()
	c.log.Info("carrier starting", "mode", c.cfg.Mode, "nick", nick,
		"room", c.cfg.Room.RoomID, "subdomain", c.cfg.Room.Subdomain)

	// --- Step 1: fetch room info and acquire cookies -------------------------
	//
	// This populates c.cfg.Room.ConferenceID (if not already set) and
	// returns an http.CookieJar pre-loaded with ngtoken/kontur_ngtoken.
	//
	// The cookie jar is then passed to xmpp.NewClient so that:
	//   - The WebSocket upgrade request carries the session cookies
	//   - keepalive goroutines (metrics + _unlock) carry them automatically
	room, jar, err := c.prepareRoom(sCtx)
	if err != nil {
		return fmt.Errorf("room prepare: %w", err)
	}

	// --- Step 2: build XMPP client with live room data ----------------------
	cb := xmpp.Callbacks{
		OnConnected: func() {
			c.log.Info("xmpp connected — starting WebRTC")
			if err := c.startWebRTC(sCtx); err != nil {
				c.log.Error("webrtc start failed", "err", err)
			}
		},
		OnDisconnected: func(err error) {
			c.log.Warn("xmpp disconnected", "err", err)
			c.teardownPC()
		},
		OnSTUNServers: func(servers []xmpp.STUNServer) {
			c.mu.Lock()
			c.stunServers = servers
			c.mu.Unlock()
			c.log.Info("ice servers received", "count", len(servers))
		},
		OnJingle: func(sess xmpp.JingleSession) {
			c.log.Debug("jingle stanza", "action", sess.Action, "sid", sess.SID)
			go c.handleJingle(sCtx, sess)
		},
		OnParticipantJoined: func(nick, jid string) {
			c.log.Info("participant joined", "nick", nick)
		},
		OnParticipantLeft: func(nick string) {
			c.log.Info("participant left", "nick", nick)
		},
	}

	c.xmppClient = xmpp.NewClient(
		room.WSSUrl(), room.JID(), room.FocusJID(), room.HTTPBase(),
		room.MetricsURL(), room.UnlockURL(),
		nick, jar, cb, c.log.With("component", "xmpp"),
	)

	go c.runPresenceToggle(sCtx)

	return c.xmppClient.Connect(sCtx)
}

// prepareRoom fetches live room data from GET /api/rooms/<roomName> and
// returns an updated RoomConfig (with ConferenceID populated) plus the
// http.CookieJar carrying ngtoken/kontur_ngtoken cookies.
//
// Anonymous join flow (confirmed via curl to live room 2026-05-19):
//
//	GET /api/rooms/<roomName>
//	  → 200 JSON {"conferenceId": "<roomName>_<sha256>", "allowAnonymous": true, ...}
//	  → Set-Cookie: ngtoken=...; Domain=.ktalk.ru; HttpOnly; SameSite=None
//	  → Set-Cookie: kontur_ngtoken=...; Domain=<shard>.ktalk.ru; SameSite=Strict
//
// If c.cfg.Room.ConferenceID is already set, the API fetch is skipped and
// a new empty jar is returned (the server will set cookies on first WS upgrade).
func (c *Carrier) prepareRoom(ctx context.Context) (config.RoomConfig, http.CookieJar, error) {
	room := c.cfg.Room

	if room.ConferenceID != "" {
		// ConferenceID already known — skip API fetch.
		// Server will issue cookies on first WebSocket upgrade via Set-Cookie.
		shortID := room.ConferenceID
		if len(shortID) > 16 {
			shortID = shortID[:16] + "..."
		}
		c.log.Debug("room conferenceId pre-set, skipping API fetch", "conferenceId", shortID)
		return room, nil, nil // nil jar → xmpp.NewClient creates a fresh one
	}

	// Fetch room info from the Kontur Talk API.
	c.log.Info("fetching room info", "url", room.RoomAPIURL())
	raClient, err := roomapi.NewClient(room.HTTPBase())
	if err != nil {
		return room, nil, fmt.Errorf("roomapi client: %w", err)
	}

	info, err := raClient.FetchRoom(ctx, room.RoomID)
	if err != nil {
		return room, nil, fmt.Errorf("fetch room %q: %w", room.RoomID, err)
	}

	// Persist ConferenceID for subsequent reconnect attempts
	// (avoids re-fetching on every XMPP reconnect).
	room.ConferenceID = info.ConferenceID
	c.cfg.Room = room

	shortID := info.ConferenceID
	if len(shortID) > 16 {
		shortID = shortID[:16] + "..."
	}
	c.log.Info("room info fetched",
		"roomName", info.RoomName,
		"conferenceId", shortID,
		"usersOnline", info.UsersOnline,
		"allowAnonymous", info.AllowAnonymous,
	)

	// Return the cookie jar — it already contains ngtoken/kontur_ngtoken
	// from the Set-Cookie headers of the GET /api/rooms/ response.
	return room, raClient.CookieJar(), nil
}

// RotateKey triggers an in-band key rotation: sends CmdKeyRotate over the active
// DataChannel and switches both sides to the new key atomically.
// Returns the new hex key so the panel can persist it.
// Returns an error if no session is active.
func (c *Carrier) RotateKey() (string, error) {
	c.mu.RLock()
	sess := c.session
	c.mu.RUnlock()
	if sess == nil {
		return "", fmt.Errorf("carrier: RotateKey: no active session")
	}
	return sess.RotateKey()
}

// SetOnKeyRotate registers a callback invoked when the peer initiates a key rotation.
// fn is called from a goroutine with the new hex key.
func (c *Carrier) SetOnKeyRotate(fn func(string)) {
	c.mu.RLock()
	sess := c.session
	c.mu.RUnlock()
	if sess != nil {
		sess.SetOnKeyRotate(fn)
	}
	// Store for future sessions too.
	c.mu.Lock()
	c.onKeyRotateFn = fn
	c.mu.Unlock()
}

// OpenStream opens a new logical tunnel stream to hostPort (e.g. "1.2.3.4:80").
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
	if c.sessionStop != nil {
		c.sessionStop()
	}
	if c.xmppClient != nil {
		c.xmppClient.Close()
	}
	c.teardownPC()
}

// --- WebRTC lifecycle ---

func (c *Carrier) startWebRTC(ctx context.Context) error {
	c.iceRestarts.Store(0)
	return c.buildPC(ctx, false)
}

// buildPC creates a new PeerConnection (and DataChannel on first build).
// When iceRestart is true it reuses the existing DC and just renegotiates ICE.
func (c *Carrier) buildPC(ctx context.Context, iceRestart bool) error {
	api := webrtc.NewAPI()
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   c.buildICEServers(),
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return fmt.Errorf("carrier: new peer connection: %w", err)
	}

	c.mu.Lock()
	c.pc = pc
	c.mu.Unlock()

	// Add fake Opus audio track so Jitsi accepts us as a full participant.
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "xk-audio",
	)
	if err == nil {
		if _, err2 := pc.AddTrack(audioTrack); err2 != nil {
			c.log.Warn("add audio track", "err", err2)
		}
	}

	// Add fake VP8 video track.
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "xk-video",
	)
	if err == nil {
		if _, err2 := pc.AddTrack(videoTrack); err2 != nil {
			c.log.Warn("add video track", "err", err2)
		}
	}

	// Create the DataChannel (only on the first buildPC call).
	if !iceRestart {
		dc, err := pc.CreateDataChannel(dcLabel, &webrtc.DataChannelInit{
			Ordered: boolPtr(dcOrdered),
		})
		if err != nil {
			pc.Close()
			return fmt.Errorf("carrier: create data channel: %w", err)
		}
		c.mu.Lock()
		c.dc = dc
		c.mu.Unlock()
		c.attachDCHandlers(ctx, dc)
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		c.log.Info("webrtc state", "state", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			c.mu.Lock()
			c.connected = true
			c.iceRestarts.Store(0)
			c.mu.Unlock()

		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected:
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()
			go c.maybeICERestart(ctx)

		case webrtc.PeerConnectionStateClosed:
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()
		}
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return // gathering complete sentinel
		}
		c.log.Debug("local ice candidate", "candidate", candidate.String())
		// Send trickle-ICE transport-info to peer via Jingle.
		go c.sendTransportInfo(candidate)
	})

	// Create offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return fmt.Errorf("carrier: create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return fmt.Errorf("carrier: set local desc: %w", err)
	}

	// Wait for full ICE gathering (non-trickle path for simplicity with Jitsi).
	select {
	case <-webrtc.GatheringCompletePromise(pc):
	case <-ctx.Done():
		pc.Close()
		return ctx.Err()
	}

	sdp := pc.LocalDescription().SDP
	c.log.Debug("sdp offer ready", "bytes", len(sdp))

	action := "session-initiate"
	if iceRestart {
		action = "transport-replace"
	}
	return c.sendJingleOffer(sdp, action)
}

// attachDCHandlers wires up DataChannel callbacks.
func (c *Carrier) attachDCHandlers(ctx context.Context, dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		c.log.Info("datachannel opened", "label", dc.Label())

		sendFn := func(raw []byte) error { return dc.Send(raw) }
		buffFn := func() uint64 { return dc.BufferedAmount() }

		sess := muxer.NewSession(sendFn, buffFn, c.cipher, c.log.With("component", "mux"))
		c.mu.Lock()
		c.session = sess
		fn := c.onKeyRotateFn
		c.mu.Unlock()
		if fn != nil {
			sess.SetOnKeyRotate(fn)
		}

		go sess.RunKeepalive(ctx)

		c.mu.RLock()
		dcOpen := c.onDCOpen
		c.mu.RUnlock()
		if dcOpen != nil {
			dcOpen()
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
}

// maybeICERestart attempts an ICE restart if under the retry limit.
func (c *Carrier) maybeICERestart(ctx context.Context) {
	n := c.iceRestarts.Add(1)
	if n > maxICERestarts {
		c.log.Error("ice restart limit reached — waiting for XMPP reconnect")
		c.teardownPC()
		return
	}

	c.log.Info("scheduling ice restart", "attempt", n)
	select {
	case <-ctx.Done():
		return
	case <-time.After(ICERestartDelay):
	}

	c.mu.RLock()
	pc := c.pc
	c.mu.RUnlock()
	if pc == nil {
		return
	}

	// Create ICE-restart offer.
	offer, err := pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		c.log.Error("ice restart create offer", "err", err)
		return
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		c.log.Error("ice restart set local desc", "err", err)
		return
	}

	select {
	case <-webrtc.GatheringCompletePromise(pc):
	case <-ctx.Done():
		return
	}

	sdp := pc.LocalDescription().SDP
	c.log.Info("sending ice restart transport-replace", "attempt", n)
	if err := c.sendJingleOffer(sdp, "transport-replace"); err != nil {
		c.log.Error("ice restart send jingle", "err", err)
	}
}

// teardownPC closes the PeerConnection and clears the session.
func (c *Carrier) teardownPC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		c.session.CloseAll()
		c.session = nil
	}
	if c.pc != nil {
		c.pc.Close()
		c.pc = nil
	}
	c.connected = false
}

// --- Jingle signaling ---

// sendJingleOffer converts the local SDP to Jingle XML and sends it via XMPP.
func (c *Carrier) sendJingleOffer(sdp, action string) error {
	c.mu.Lock()
	sid := c.currentSID
	if sid == "" {
		sid = newSID()
		c.currentSID = sid
	}
	c.mu.Unlock()

	initiator := c.xmppClient.FullJID()
	j, err := jingle.SDPToJingle(sdp, action, sid, initiator)
	if err != nil {
		return fmt.Errorf("carrier: sdp→jingle: %w", err)
	}

	xmlStr, err := j.ToXML()
	if err != nil {
		return fmt.Errorf("carrier: marshal jingle: %w", err)
	}

	c.log.Debug("sending jingle", "action", action, "sid", sid)
	return c.xmppClient.SendJingle(c.cfg.Room.FocusJID(), action, sid, xmlStr)
}

// sendTransportInfo sends a trickle-ICE candidate via Jingle transport-info.
func (c *Carrier) sendTransportInfo(candidate *webrtc.ICECandidate) {
	c.mu.RLock()
	sid := c.currentSID
	c.mu.RUnlock()
	if sid == "" {
		return
	}

	init := candidate.ToJSON()
	// Build a minimal transport-info XML fragment.
	type candidateXML struct {
		XMLName    xml.Name `xml:"candidate"`
		Component  int      `xml:"component,attr"`
		Foundation string   `xml:"foundation,attr"`
		Generation int      `xml:"generation,attr"`
		IP         string   `xml:"ip,attr"`
		Port       int      `xml:"port,attr"`
		Priority   uint32   `xml:"priority,attr"`
		Protocol   string   `xml:"protocol,attr"`
		Type       string   `xml:"type,attr"`
	}
	type transportXML struct {
		XMLName   xml.Name       `xml:"urn:xmpp:jingle:transports:ice-udp:1 transport"`
		Ufrag     string         `xml:"ufrag,attr,omitempty"`
		Candidate []candidateXML `xml:"candidate"`
	}
	type contentXML struct {
		XMLName   xml.Name     `xml:"content"`
		Creator   string       `xml:"creator,attr"`
		Name      string       `xml:"name,attr"`
		Transport transportXML `xml:"transport"`
	}

	// Parse candidate string: "candidate:foundation component protocol priority ip port typ type"
	var foundation, protocol, ip, typ string
	var component, port int
	var priority uint32
	fmt.Sscanf(init.Candidate, "candidate:%s %d %s %d %s %d typ %s",
		&foundation, &component, &protocol, &priority, &ip, &port, &typ)

	content := contentXML{
		Creator: "initiator",
		Name:    "audio", // Jitsi uses first m-line name
		Transport: transportXML{
			Candidate: []candidateXML{{
				Component:  component,
				Foundation: foundation,
				Generation: 0,
				IP:         ip,
				Port:       port,
				Priority:   priority,
				Protocol:   protocol,
				Type:       typ,
			}},
		},
	}
	raw, _ := xml.Marshal(content)

	if err := c.xmppClient.SendJingle(c.cfg.Room.FocusJID(), "transport-info", sid, string(raw)); err != nil {
		c.log.Debug("transport-info send failed", "err", err)
	}
}

// handleJingle processes an incoming Jingle stanza from the peer.
func (c *Carrier) handleJingle(ctx context.Context, sess xmpp.JingleSession) {
	switch sess.Action {
	case "session-initiate":
		// Joiner side: we receive a session-initiate from the Creator.
		c.handleSessionInitiate(ctx, sess)

	case "session-accept":
		// Creator side: peer accepted our offer.
		c.handleSessionAccept(ctx, sess)

	case "session-terminate":
		c.log.Info("session terminated by peer", "sid", sess.SID)
		c.teardownPC()

	case "transport-info":
		// Trickle-ICE candidate from peer.
		c.handleTransportInfo(sess)

	case "transport-replace":
		// ICE restart from peer — treat like session-accept.
		c.handleSessionAccept(ctx, sess)

	case "source-add", "source-remove":
		c.log.Debug("jingle source change", "action", sess.Action)
	}
}

// handleSessionInitiate handles an incoming session-initiate (Joiner role).
func (c *Carrier) handleSessionInitiate(ctx context.Context, sess xmpp.JingleSession) {
	c.mu.Lock()
	c.currentSID = sess.SID
	c.mu.Unlock()

	j, err := jingle.Parse(sess.SDP)
	if err != nil {
		c.log.Error("parse session-initiate", "err", err)
		return
	}

	sdp, err := jingle.JingleToSDP(j)
	if err != nil {
		c.log.Error("jingle→sdp", "err", err)
		return
	}

	c.mu.RLock()
	pc := c.pc
	c.mu.RUnlock()

	if pc == nil {
		// Joiner hasn't built a PC yet — do it now (answer path).
		if err := c.buildPC(ctx, false); err != nil {
			c.log.Error("build pc for joiner", "err", err)
			return
		}
		c.mu.RLock()
		pc = c.pc
		c.mu.RUnlock()
	}

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	if err := pc.SetRemoteDescription(offer); err != nil {
		c.log.Error("set remote desc (offer)", "err", err)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.log.Error("create answer", "err", err)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		c.log.Error("set local desc (answer)", "err", err)
		return
	}
	select {
	case <-webrtc.GatheringCompletePromise(pc):
	case <-ctx.Done():
		return
	}

	answerSDP := pc.LocalDescription().SDP
	if err := c.sendJingleOffer(answerSDP, "session-accept"); err != nil {
		c.log.Error("send session-accept", "err", err)
	}
}

// handleSessionAccept processes a session-accept or transport-replace.
func (c *Carrier) handleSessionAccept(ctx context.Context, sess xmpp.JingleSession) {
	j, err := jingle.Parse(sess.SDP)
	if err != nil {
		c.log.Error("parse session-accept", "err", err)
		return
	}

	sdp, err := jingle.JingleToSDP(j)
	if err != nil {
		c.log.Error("jingle→sdp (accept)", "err", err)
		return
	}

	c.mu.RLock()
	pc := c.pc
	c.mu.RUnlock()
	if pc == nil {
		return
	}

	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}
	if err := pc.SetRemoteDescription(answer); err != nil {
		c.log.Error("set remote desc (answer)", "err", err, "action", sess.Action)
	}
}

// handleTransportInfo adds a trickle-ICE candidate from the peer.
func (c *Carrier) handleTransportInfo(sess xmpp.JingleSession) {
	c.mu.RLock()
	pc := c.pc
	c.mu.RUnlock()
	if pc == nil {
		return
	}

	// Extract ice candidate string from the transport-info XML payload.
	type candidateXML struct {
		IP         string `xml:"ip,attr"`
		Port       int    `xml:"port,attr"`
		Foundation string `xml:"foundation,attr"`
		Component  int    `xml:"component,attr"`
		Priority   uint32 `xml:"priority,attr"`
		Protocol   string `xml:"protocol,attr"`
		Type       string `xml:"type,attr"`
	}
	type transportXML struct {
		XMLName    xml.Name       `xml:"transport"`
		Ufrag      string         `xml:"ufrag,attr"`
		Pwd        string         `xml:"pwd,attr"`
		Candidates []candidateXML `xml:"candidate"`
	}
	type contentXML struct {
		XMLName   xml.Name     `xml:"content"`
		Transport transportXML `xml:"transport"`
	}

	var content contentXML
	if err := xml.Unmarshal([]byte(sess.SDP), &content); err != nil {
		c.log.Debug("transport-info parse failed", "err", err)
		return
	}

	for _, cand := range content.Transport.Candidates {
		sdpCand := fmt.Sprintf(
			"candidate:%s %d %s %d %s %d typ %s",
			cand.Foundation, cand.Component, cand.Protocol,
			cand.Priority, cand.IP, cand.Port, cand.Type,
		)
		ic := webrtc.ICECandidateInit{
			Candidate:        sdpCand,
			SDPMLineIndex:    uint16Ptr(0),
			UsernameFragment: &content.Transport.Ufrag,
		}
		if err := pc.AddICECandidate(ic); err != nil {
			c.log.Debug("add ice candidate failed", "err", err)
		}
	}
}

// --- ICE server list ---

func (c *Carrier) buildICEServers() []webrtc.ICEServer {
	c.mu.RLock()
	servers := c.stunServers
	c.mu.RUnlock()

	var ice []webrtc.ICEServer
	for _, s := range servers {
		var urls []string
		switch s.Type {
		case "stun":
			urls = append(urls, fmt.Sprintf("stun:%s:%d", s.Host, s.Port))
		case "turn":
			proto := s.Transport
			if proto == "" {
				proto = "udp"
			}
			urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=%s", s.Host, s.Port, proto))
		}
		if len(urls) == 0 {
			continue
		}
		srv := webrtc.ICEServer{URLs: urls}
		if s.Username != "" {
			srv.Username = s.Username
			srv.Credential = s.Password
		}
		ice = append(ice, srv)
	}

	// Fallback public STUN (used only when extdisco hasn't responded).
	if len(ice) == 0 {
		ice = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		}
	}
	return ice
}

// --- anti-detect presence toggling ---

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
			audioMuted = !audioMuted
			if err := c.xmppClient.SendPresence(audioMuted, false); err != nil {
				c.log.Debug("presence toggle failed", "err", err)
			}
		}
	}
}

// --- Stream → net.Conn adapter (used by SOCKS5 DialFunc) ---

// StreamConn wraps a muxer.Stream as a net.Conn so the SOCKS5 server
// can call relay() on it directly.
type StreamConn struct {
	*muxer.Stream
	local  net.Addr
	remote net.Addr
}

func (s *StreamConn) LocalAddr() net.Addr                { return s.local }
func (s *StreamConn) RemoteAddr() net.Addr               { return s.remote }
func (s *StreamConn) SetDeadline(_ time.Time) error      { return nil }
func (s *StreamConn) SetReadDeadline(_ time.Time) error  { return nil }
func (s *StreamConn) SetWriteDeadline(_ time.Time) error { return nil }

// Write sends data over the tunnel stream.
func (s *StreamConn) Write(b []byte) (int, error) {
	n := len(b)
	if n == 0 {
		return 0, nil
	}
	if err := s.Stream.SendData(b); err != nil {
		return 0, err
	}
	return n, nil
}

// Read receives data from the tunnel stream (delegates to Stream.Read).
// (Stream.Read is already implemented in muxer.)

// Dial implements socks5.DialFunc — opens a muxer.Stream and returns it as net.Conn.
func (c *Carrier) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	st, err := c.OpenStream(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("carrier dial %s: %w", addr, err)
	}
	return &StreamConn{
		Stream: st,
		local:  &net.TCPAddr{},
		remote: &net.TCPAddr{},
	}, nil
}

// --- helpers ---

func boolPtr(b bool) *bool       { return &b }
func uint16Ptr(v uint16) *uint16 { return &v }

func newSID() string {
	// Use timestamp + random nibble as a simple SID.
	return fmt.Sprintf("xk-%d", time.Now().UnixNano())
}
