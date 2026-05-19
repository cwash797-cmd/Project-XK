// Package jingle implements conversion between Jingle XML stanzas and SDP.
//
// Supported XEPs:
//   - XEP-0166: Jingle (base)
//   - XEP-0167: Jingle RTP Sessions
//   - XEP-0176: Jingle ICE-UDP Transport Method
//   - XEP-0294: Jingle RTP Header Extensions Negotiation
//   - XEP-0339: Source-Specific Media Attributes in Jingle
//
// The conversion is intentionally minimal — we only need enough to
// satisfy Jitsi Videobridge for a headless participant that sends fake
// audio/video and tunnels data through the DataChannel.
package jingle

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// --- XML types ---

// Jingle is the top-level <jingle> element.
type Jingle struct {
	XMLName     xml.Name    `xml:"urn:xmpp:jingle:1 jingle"`
	Action      string      `xml:"action,attr"`
	SID         string      `xml:"sid,attr"`
	Initiator   string      `xml:"initiator,attr,omitempty"`
	Responder   string      `xml:"responder,attr,omitempty"`
	Contents    []Content   `xml:"content"`
}

// Content is a single media or data content block.
type Content struct {
	Creator     string      `xml:"creator,attr"`
	Name        string      `xml:"name,attr"`
	Senders     string      `xml:"senders,attr,omitempty"`
	Description *Description `xml:"description,omitempty"`
	Transport   *Transport   `xml:"transport,omitempty"`
}

// Description holds RTP codec info (XEP-0167).
type Description struct {
	XMLName    xml.Name    `xml:"urn:xmpp:jingle:apps:rtp:1 description"`
	Media      string      `xml:"media,attr"`
	PayloadTypes []PayloadType `xml:"payload-type"`
	Sources    []Source    `xml:"urn:ietf:rfc:5576 source"`
	RTPHdrExts []RTPHdrExt `xml:"urn:xmpp:jingle:apps:rtp:rtp-hdrext:0 rtp-hdrext"`
}

// PayloadType describes a codec.
type PayloadType struct {
	ID        int    `xml:"id,attr"`
	Name      string `xml:"name,attr"`
	ClockRate int    `xml:"clockrate,attr,omitempty"`
	Channels  int    `xml:"channels,attr,omitempty"`
}

// Source describes an SSRC (XEP-0339).
type Source struct {
	SSRC       uint32      `xml:"ssrc,attr"`
	Parameters []Parameter `xml:"parameter"`
}

// Parameter is a key-value attribute of an SSRC source.
type Parameter struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr,omitempty"`
}

// RTPHdrExt is an RTP header extension (XEP-0294).
type RTPHdrExt struct {
	ID  int    `xml:"id,attr"`
	URI string `xml:"uri,attr"`
}

// Transport holds ICE candidates (XEP-0176).
type Transport struct {
	XMLName    xml.Name    `xml:"urn:xmpp:jingle:transports:ice-udp:1 transport"`
	Ufrag      string      `xml:"ufrag,attr,omitempty"`
	Pwd        string      `xml:"pwd,attr,omitempty"`
	Candidates []Candidate `xml:"candidate"`
	Fingerprint *Fingerprint `xml:"urn:xmpp:jingle:apps:dtls:0 fingerprint,omitempty"`
}

// Candidate is an ICE candidate.
type Candidate struct {
	Component  int    `xml:"component,attr"`
	Foundation string `xml:"foundation,attr"`
	Generation int    `xml:"generation,attr"`
	ID         string `xml:"id,attr,omitempty"`
	IP         string `xml:"ip,attr"`
	Network    int    `xml:"network,attr,omitempty"`
	Port       int    `xml:"port,attr"`
	Priority   uint32 `xml:"priority,attr"`
	Protocol   string `xml:"protocol,attr"`
	RelAddr    string `xml:"rel-addr,attr,omitempty"`
	RelPort    int    `xml:"rel-port,attr,omitempty"`
	Type       string `xml:"type,attr"`
}

// Fingerprint holds the DTLS fingerprint.
type Fingerprint struct {
	Hash    string `xml:"hash,attr"`
	Setup   string `xml:"setup,attr,omitempty"`
	Value   string `xml:",chardata"`
}

// --- SDP conversion ---

// SDPToJingle converts a Pion-generated SDP offer/answer to a Jingle XML fragment.
// The result is the inner XML of the <jingle> element (the <content> children).
func SDPToJingle(sdp string, action, sid, initiator string) (*Jingle, error) {
	j := &Jingle{
		Action:    action,
		SID:       sid,
		Initiator: initiator,
	}

	var currentMedia string
	var currentContent *Content

	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 2 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "m="):
			// e.g. m=audio 9 UDP/TLS/RTP/SAVPF 111
			parts := strings.Fields(line[2:])
			if len(parts) == 0 {
				continue
			}
			currentMedia = parts[0]
			c := Content{
				Creator: "initiator",
				Name:    currentMedia,
				Senders: "both",
				Description: &Description{
					Media: currentMedia,
				},
				Transport: &Transport{},
			}
			j.Contents = append(j.Contents, c)
			currentContent = &j.Contents[len(j.Contents)-1]

		case strings.HasPrefix(line, "a=rtpmap:") && currentContent != nil:
			// a=rtpmap:111 opus/48000/2
			var id int
			var name string
			var clock int
			var ch int
			fmt.Sscanf(strings.TrimPrefix(line, "a=rtpmap:"), "%d %[^/]/%d/%d", &id, &name, &clock, &ch)
			if ch == 0 {
				ch = 1
			}
			currentContent.Description.PayloadTypes = append(
				currentContent.Description.PayloadTypes,
				PayloadType{ID: id, Name: name, ClockRate: clock, Channels: ch},
			)

		case strings.HasPrefix(line, "a=extmap:") && currentContent != nil:
			// a=extmap:1 urn:ietf:params:rtp-hdrext:ssrc-audio-level
			var id int
			var uri string
			fmt.Sscanf(strings.TrimPrefix(line, "a=extmap:"), "%d %s", &id, &uri)
			currentContent.Description.RTPHdrExts = append(
				currentContent.Description.RTPHdrExts,
				RTPHdrExt{ID: id, URI: uri},
			)

		case strings.HasPrefix(line, "a=ice-ufrag:") && currentContent != nil:
			currentContent.Transport.Ufrag = strings.TrimPrefix(line, "a=ice-ufrag:")

		case strings.HasPrefix(line, "a=ice-pwd:") && currentContent != nil:
			currentContent.Transport.Pwd = strings.TrimPrefix(line, "a=ice-pwd:")

		case strings.HasPrefix(line, "a=fingerprint:") && currentContent != nil:
			// a=fingerprint:sha-256 AA:BB:...
			rest := strings.TrimPrefix(line, "a=fingerprint:")
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 {
				currentContent.Transport.Fingerprint = &Fingerprint{
					Hash:  parts[0],
					Value: parts[1],
					Setup: "active",
				}
			}

		case strings.HasPrefix(line, "a=candidate:") && currentContent != nil:
			cand := parseCandidate(strings.TrimPrefix(line, "a=candidate:"))
			if cand != nil {
				currentContent.Transport.Candidates = append(
					currentContent.Transport.Candidates, *cand,
				)
			}
		}
		_ = currentMedia
	}

	return j, nil
}

// JingleToSDP converts a received Jingle session-initiate to an SDP offer string.
// This is needed on the Joiner side to pass to pion/webrtc.
func JingleToSDP(j *Jingle) (string, error) {
	var sb strings.Builder

	sb.WriteString("v=0\r\n")
	sb.WriteString("o=- 0 0 IN IP4 0.0.0.0\r\n")
	sb.WriteString("s=-\r\n")
	sb.WriteString("t=0 0\r\n")

	for _, c := range j.Contents {
		if c.Description == nil || c.Transport == nil {
			continue
		}
		media := c.Description.Media
		port := 9
		if len(c.Transport.Candidates) > 0 {
			port = c.Transport.Candidates[0].Port
		}

		var ptIDs []string
		for _, pt := range c.Description.PayloadTypes {
			ptIDs = append(ptIDs, fmt.Sprintf("%d", pt.ID))
		}
		sb.WriteString(fmt.Sprintf("m=%s %d UDP/TLS/RTP/SAVPF %s\r\n", media, port, strings.Join(ptIDs, " ")))
		sb.WriteString("c=IN IP4 0.0.0.0\r\n")
		sb.WriteString("a=rtcp:9 IN IP4 0.0.0.0\r\n")

		if c.Transport.Ufrag != "" {
			sb.WriteString(fmt.Sprintf("a=ice-ufrag:%s\r\n", c.Transport.Ufrag))
		}
		if c.Transport.Pwd != "" {
			sb.WriteString(fmt.Sprintf("a=ice-pwd:%s\r\n", c.Transport.Pwd))
		}
		if c.Transport.Fingerprint != nil {
			sb.WriteString(fmt.Sprintf("a=fingerprint:%s %s\r\n",
				c.Transport.Fingerprint.Hash, c.Transport.Fingerprint.Value))
			sb.WriteString(fmt.Sprintf("a=setup:%s\r\n", c.Transport.Fingerprint.Setup))
		}

		for _, pt := range c.Description.PayloadTypes {
			ch := ""
			if pt.Channels > 1 {
				ch = fmt.Sprintf("/%d", pt.Channels)
			}
			sb.WriteString(fmt.Sprintf("a=rtpmap:%d %s/%d%s\r\n", pt.ID, pt.Name, pt.ClockRate, ch))
		}

		for _, hdrext := range c.Description.RTPHdrExts {
			sb.WriteString(fmt.Sprintf("a=extmap:%d %s\r\n", hdrext.ID, hdrext.URI))
		}

		for _, cand := range c.Transport.Candidates {
			sb.WriteString(fmt.Sprintf(
				"a=candidate:%s %d %s %d %s %d typ %s\r\n",
				cand.Foundation, cand.Component, cand.Protocol,
				cand.Priority, cand.IP, cand.Port, cand.Type,
			))
		}
	}

	return sb.String(), nil
}

// MarshalXML returns the XML representation of the Jingle stanza.
func (j *Jingle) MarshalXML() (string, error) {
	raw, err := xml.Marshal(j)
	if err != nil {
		return "", fmt.Errorf("jingle marshal: %w", err)
	}
	return string(raw), nil
}

// Parse parses a raw XML fragment containing a <jingle> element.
func Parse(raw string) (*Jingle, error) {
	var j Jingle
	if err := xml.Unmarshal([]byte(raw), &j); err != nil {
		return nil, fmt.Errorf("jingle parse: %w", err)
	}
	return &j, nil
}

// --- helpers ---

func parseCandidate(s string) *Candidate {
	// a=candidate:foundation component protocol priority ip port typ type
	// e.g. "1 1 udp 2113937151 192.168.1.1 12345 typ host"
	var c Candidate
	var typ string
	n, err := fmt.Sscanf(s, "%s %d %s %d %s %d typ %s",
		&c.Foundation, &c.Component, &c.Protocol,
		&c.Priority, &c.IP, &c.Port, &typ,
	)
	if n < 7 || err != nil {
		return nil
	}
	c.Type = typ
	return &c
}
