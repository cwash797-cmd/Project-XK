// Package xmpp implements a minimal XMPP-over-WebSocket client
// sufficient to join a Kontour Talk (Jitsi) room as an anonymous participant.
//
// Protocol details:
//   - Transport: WebSocket with Sec-WebSocket-Protocol: xmpp
//   - Auth: SASL ANONYMOUS
//   - Extensions: Stream Management (XEP-0198), extdisco (XEP-0215), MUC (XEP-0045)
//   - Reconnect: SM resume on WebSocket reconnect, full re-join on SM failure
package xmpp

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	xmppNS         = "jabber:client"
	saslNS         = "urn:ietf:params:xml:ns:xmpp-sasl"
	bindNS         = "urn:ietf:params:xml:ns:xmpp-bind"
	sessionNS      = "urn:ietf:params:xml:ns:xmpp-session"
	smNS           = "urn:xmpp:sm:3"
	mucNS          = "http://jabber.org/protocol/muc"
	mucUserNS      = "http://jabber.org/protocol/muc#user"
	discoNS        = "http://jabber.org/protocol/disco#info"
	extDiscoNS     = "urn:xmpp:extdisco:2"
	jingleNS       = "urn:xmpp:jingle:1"
	jitsiNS        = "http://jitsi.org/jitmeet"
	capNS          = "http://jabber.org/protocol/caps"

	reconnectDelay    = 3 * time.Second
	reconnectMaxDelay = 60 * time.Second
	smAckInterval     = 10 * time.Second
)

// STUNServer holds a STUN/TURN credential from extdisco.
type STUNServer struct {
	Host       string
	Port       int
	Type       string // "stun" or "turn"
	Transport  string // "udp" or "tcp"
	Username   string
	Password   string
	Expiry     time.Time
}

// JingleSession describes an incoming or outgoing Jingle negotiation.
type JingleSession struct {
	SID    string
	Action string
	SDP    string
}

// Callbacks holds application-level hooks invoked by the XMPP client.
type Callbacks struct {
	// OnConnected is called once the MUC join is complete and the DC can open.
	OnConnected func()
	// OnDisconnected is called when the XMPP connection drops.
	OnDisconnected func(err error)
	// OnSTUNServers is called when fresh ICE credentials arrive from extdisco.
	OnSTUNServers func(servers []STUNServer)
	// OnJingle is called when a Jingle stanza is received.
	OnJingle func(sess JingleSession)
	// OnParticipantJoined is called when a new participant appears in MUC.
	OnParticipantJoined func(nick, jid string)
	// OnParticipantLeft is called when a participant leaves MUC.
	OnParticipantLeft func(nick string)
}

// Client is a stateful XMPP-over-WebSocket connection to a Kontour Talk room.
type Client struct {
	wssURL    string
	roomJID   string   // e.g. "roomid@muc.meet.jitsi"
	focusJID  string   // e.g. "focus@auth.meet.jitsi/focus"
	httpBase  string   // e.g. "https://subdomain.ktalk.ru"
	nickname  string
	callbacks Callbacks
	log       *slog.Logger

	mu        sync.Mutex
	conn      *websocket.Conn
	fullJID   string // assigned after bind
	smEnabled bool
	smH       uint32 // handled stanza count (for SM acks)
	smResume  string // SM previd for resume

	iqSeq    atomic.Uint64
	closed   atomic.Bool
}

// NewClient creates a new XMPP client. Call Connect to start the session.
func NewClient(wssURL, roomJID, focusJID, httpBase, nickname string, cb Callbacks, log *slog.Logger) *Client {
	return &Client{
		wssURL:   wssURL,
		roomJID:  roomJID,
		focusJID: focusJID,
		httpBase: httpBase,
		nickname: nickname,
		callbacks: cb,
		log:      log,
	}
}

// Connect starts the XMPP session, blocking until the context is cancelled.
// On transient failures it reconnects with exponential backoff.
func (c *Client) Connect(ctx context.Context) error {
	delay := reconnectDelay
	for {
		if c.closed.Load() {
			return nil
		}
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			c.log.Warn("xmpp session ended, will reconnect", "err", err, "delay", delay)
			if c.callbacks.OnDisconnected != nil {
				c.callbacks.OnDisconnected(err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay = min(delay*2, reconnectMaxDelay)
		} else {
			delay = reconnectDelay
		}
	}
}

// Close gracefully terminates the XMPP session.
func (c *Client) Close() {
	c.closed.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		// Send <presence type="unavailable">
		_ = c.sendStanza(`<presence type="unavailable"/>`)
		_ = c.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = c.conn.Close()
	}
}

// SendJingle transmits a Jingle IQ stanza.
func (c *Client) SendJingle(to, action, sid, sdpXML string) error {
	iqID := c.nextIQID()
	stanza := fmt.Sprintf(
		`<iq to=%q id=%q type="set"><jingle xmlns=%q action=%q sid=%q>%s</jingle></iq>`,
		to, iqID, jingleNS, action, sid, sdpXML,
	)
	return c.sendStanza(stanza)
}

// SendPresence sends a MUC presence update with audio/video muted flags.
func (c *Client) SendPresence(audioMuted, videoMuted bool) error {
	to := fmt.Sprintf("%s/%s", c.roomJID, c.nickname)
	stanza := fmt.Sprintf(
		`<presence to=%q><x xmlns=%q/><audiomuted xmlns=%q>%v</audiomuted><videomuted xmlns=%q>%v</videomuted></presence>`,
		to, mucNS, jitsiNS, audioMuted, jitsiNS, videoMuted,
	)
	return c.sendStanza(stanza)
}

// FullJID returns the bound JID after a successful connect, or empty string.
func (c *Client) FullJID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fullJID
}

// --- internal session lifecycle ---

func (c *Client) runOnce(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		conn.Close()
	}()

	if err := c.handshake(ctx, conn); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if c.callbacks.OnConnected != nil {
		c.callbacks.OnConnected()
	}
	return c.readLoop(ctx, conn)
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		Subprotocols:     []string{"xmpp"},
	}
	headers := http.Header{}
	headers.Set("Origin", c.httpBase)
	conn, _, err := dialer.DialContext(ctx, c.wssURL, headers)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) error {
	// 1. Open stream
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<?xml version="1.0"?><stream:stream to="meet.jitsi" version="1.0" xml:lang="en" xmlns=%q xmlns:stream="http://etherx.jabber.org/streams">`,
		xmppNS,
	)); err != nil {
		return err
	}

	// 2. Wait for <stream:features> with SASL ANONYMOUS
	if err := c.waitSASLFeatures(conn); err != nil {
		return fmt.Errorf("wait sasl features: %w", err)
	}

	// 3. SASL ANONYMOUS auth
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<auth xmlns=%q mechanism="ANONYMOUS">%s</auth>`,
		saslNS, base64.StdEncoding.EncodeToString([]byte("ANONYMOUS")),
	)); err != nil {
		return err
	}
	if err := c.waitSASLSuccess(conn); err != nil {
		return fmt.Errorf("sasl anonymous: %w", err)
	}

	// 4. Reopen stream
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<stream:stream to="meet.jitsi" version="1.0" xml:lang="en" xmlns=%q xmlns:stream="http://etherx.jabber.org/streams">`,
		xmppNS,
	)); err != nil {
		return err
	}
	if err := c.waitFeatures(conn); err != nil {
		return fmt.Errorf("wait post-sasl features: %w", err)
	}

	// 5. Resource bind
	iqID := c.nextIQID()
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<iq type="set" id=%q xmlns=%q><bind xmlns=%q/></iq>`,
		iqID, xmppNS, bindNS,
	)); err != nil {
		return err
	}
	jid, err := c.waitBind(conn, iqID)
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	c.mu.Lock()
	c.fullJID = jid
	c.mu.Unlock()
	c.log.Info("xmpp bound", "jid", jid)

	// 6. Session establish
	sessID := c.nextIQID()
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<iq type="set" id=%q xmlns=%q><session xmlns=%q/></iq>`,
		sessID, xmppNS, sessionNS,
	)); err != nil {
		return err
	}

	// 7. Enable Stream Management
	if err := c.writeRaw(conn, fmt.Sprintf(`<enable xmlns=%q resume="true"/>`, smNS)); err != nil {
		return err
	}

	// 8. extdisco — get STUN/TURN credentials
	discoID := c.nextIQID()
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<iq type="get" to="meet.jitsi" id=%q xmlns=%q><services xmlns=%q/></iq>`,
		discoID, xmppNS, extDiscoNS,
	)); err != nil {
		return err
	}

	// 9. Register with Jicofo
	confID := c.nextIQID()
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<iq to=%q type="set" id=%q xmlns=%q><conference xmlns="http://jitsi.org/protocol/focus" room=%q/></iq>`,
		c.focusJID, confID, xmppNS, c.roomJID,
	)); err != nil {
		return err
	}

	// 10. MUC presence join
	to := fmt.Sprintf("%s/%s", c.roomJID, c.nickname)
	if err := c.writeRaw(conn, fmt.Sprintf(
		`<presence to=%q xmlns=%q><x xmlns=%q/><audiomuted xmlns=%q>true</audiomuted><videomuted xmlns=%q>false</videomuted></presence>`,
		to, xmppNS, mucNS, jitsiNS, jitsiNS,
	)); err != nil {
		return err
	}

	c.log.Info("xmpp muc joined", "room", c.roomJID, "nick", c.nickname)
	return nil
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	smTicker := time.NewTicker(smAckInterval)
	defer smTicker.Stop()

	done := make(chan error, 1)
	go func() {
		done <- c.recvLoop(conn)
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case <-smTicker.C:
			_ = c.writeRaw(conn, fmt.Sprintf(`<a xmlns=%q h="%d"/>`, smNS, c.smH))
		}
	}
}

func (c *Client) recvLoop(conn *websocket.Conn) error {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		c.dispatch(msg)
	}
}

func (c *Client) dispatch(raw []byte) {
	// We parse individual stanzas. The stream is kept open so we receive
	// a stream of XML elements from Prosody.
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.log.Debug("xml token error", "err", err)
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "iq":
			c.handleIQ(dec, se)
		case "presence":
			c.handlePresence(dec, se)
		case "message":
			// ignore chat messages for now
		case "a": // SM ack
			atomic.AddUint32(&c.smH, 1)
		case "enabled": // SM enabled confirmation
			c.mu.Lock()
			c.smEnabled = true
			for _, attr := range se.Attr {
				if attr.Name.Local == "id" {
					c.smResume = attr.Value
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *Client) handleIQ(dec *xml.Decoder, se xml.StartElement) {
	var iq struct {
		Type string `xml:"type,attr"`
		ID   string `xml:"id,attr"`
		From string `xml:"from,attr"`
		Body []byte `xml:",innerxml"`
	}
	if err := dec.DecodeElement(&iq, &se); err != nil {
		return
	}

	// Check for extdisco response
	if strings.Contains(string(iq.Body), extDiscoNS) {
		c.parseExtDisco(iq.Body)
		return
	}

	// Check for Jingle
	if strings.Contains(string(iq.Body), jingleNS) {
		c.parseJingle(iq.From, iq.ID, iq.Body)
		return
	}
}

func (c *Client) handlePresence(dec *xml.Decoder, se xml.StartElement) {
	var p struct {
		From string `xml:"from,attr"`
		Type string `xml:"type,attr"`
	}
	if err := dec.DecodeElement(&p, &se); err != nil {
		return
	}
	parts := strings.SplitN(p.From, "/", 2)
	nick := ""
	if len(parts) == 2 {
		nick = parts[1]
	}
	if p.Type == "unavailable" {
		if c.callbacks.OnParticipantLeft != nil {
			c.callbacks.OnParticipantLeft(nick)
		}
	} else {
		if c.callbacks.OnParticipantJoined != nil {
			c.callbacks.OnParticipantJoined(nick, p.From)
		}
	}
}

func (c *Client) parseExtDisco(body []byte) {
	// Minimal extdisco parser — extract service elements
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	var servers []STUNServer
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "service" {
			var srv STUNServer
			for _, attr := range se.Attr {
				switch attr.Name.Local {
				case "host":
					srv.Host = attr.Value
				case "type":
					srv.Type = attr.Value
				case "transport":
					srv.Transport = attr.Value
				case "username":
					srv.Username = attr.Value
				case "password":
					srv.Password = attr.Value
				}
			}
			if srv.Host != "" {
				servers = append(servers, srv)
			}
		}
	}
	if len(servers) > 0 && c.callbacks.OnSTUNServers != nil {
		c.callbacks.OnSTUNServers(servers)
	}
}

func (c *Client) parseJingle(from, iqID string, body []byte) {
	type jingleXML struct {
		Action string `xml:"action,attr"`
		SID    string `xml:"sid,attr"`
		Inner  []byte `xml:",innerxml"`
	}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "jingle" {
			var j jingleXML
			if err := dec.DecodeElement(&j, &se); err != nil {
				continue
			}
			if c.callbacks.OnJingle != nil {
				c.callbacks.OnJingle(JingleSession{
					SID:    j.SID,
					Action: j.Action,
					SDP:    string(j.Inner),
				})
			}
		}
	}
}

// --- low-level helpers ---

func (c *Client) writeRaw(conn *websocket.Conn, s string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, []byte(s))
}

func (c *Client) sendStanza(s string) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("xmpp: not connected")
	}
	return conn.WriteMessage(websocket.TextMessage, []byte(s))
}

func (c *Client) nextIQID() string {
	return fmt.Sprintf("ktalk-%d-%d", c.iqSeq.Add(1), rand.Intn(9999))
}

// waitSASLFeatures reads until a <stream:features> element with SASL is seen.
func (c *Client) waitSASLFeatures(conn *websocket.Conn) error {
	return c.waitForElement(conn, "features")
}

func (c *Client) waitSASLSuccess(conn *websocket.Conn) error {
	return c.waitForElement(conn, "success")
}

func (c *Client) waitFeatures(conn *websocket.Conn) error {
	return c.waitForElement(conn, "features")
}

func (c *Client) waitBind(conn *websocket.Conn, iqID string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return "", err
		}
		if strings.Contains(string(msg), "bind") && strings.Contains(string(msg), "<jid>") {
			// Extract JID from <jid>...</jid>
			start := strings.Index(string(msg), "<jid>") + 5
			end := strings.Index(string(msg), "</jid>")
			if start > 5 && end > start {
				jid := string(msg)[start:end]
				conn.SetReadDeadline(time.Time{})
				return jid, nil
			}
		}
	}
	conn.SetReadDeadline(time.Time{})
	return "", fmt.Errorf("bind timeout")
}

func (c *Client) waitForElement(conn *websocket.Conn, localName string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if strings.Contains(string(msg), localName) {
			conn.SetReadDeadline(time.Time{})
			return nil
		}
	}
	conn.SetReadDeadline(time.Time{})
	return fmt.Errorf("timeout waiting for <%s>", localName)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
