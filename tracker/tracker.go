// Package tracker announces to BitTorrent trackers and parses their peer
// lists.
package tracker

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/zg/btcli/bencode"
	"github.com/zg/btcli/peer"
)

// Event is the announce event type.
type Event string

const (
	EventStarted   Event = "started"
	EventStopped   Event = "stopped"
	EventCompleted Event = "completed"
	EventNone      Event = ""
)

// Request describes an announce.
type Request struct {
	URL        string
	InfoHash   [20]byte
	PeerID     [20]byte
	Port       uint16
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      Event
	NumWant    int
}

// Response is a tracker's reply.
type Response struct {
	Interval    time.Duration
	MinInterval time.Duration
	Peers       []peer.Peer
	Complete    int64 // seeders
	Incomplete  int64 // leechers
	TrackerID   string
	WarningMsg  string
}

// Announce contacts the tracker and returns its response.
func Announce(ctx context.Context, client *http.Client, req Request) (*Response, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	u, err := buildURL(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("User-Agent", "btcli/1.0")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker: HTTP %d", resp.StatusCode)
	}

	// Cap the body: a tracker is not fully trusted, and an unbounded read
	// is an easy way to be handed a very large response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("tracker: reading response: %w", err)
	}
	return ParseResponse(body)
}

// buildURL constructs the announce URL.
func buildURL(req Request) (string, error) {
	base, err := url.Parse(req.URL)
	if err != nil {
		return "", fmt.Errorf("tracker: invalid announce URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("tracker: unsupported scheme %q (only HTTP is implemented)", base.Scheme)
	}

	numWant := req.NumWant
	if numWant <= 0 {
		numWant = 50
	}

	// info_hash and peer_id are raw 20-byte values, not text. Building the
	// query with url.Values is what escapes them correctly; concatenating
	// them into a string would corrupt any non-ASCII byte.
	q := base.Query()
	q.Set("info_hash", string(req.InfoHash[:]))
	q.Set("peer_id", string(req.PeerID[:]))
	q.Set("port", strconv.Itoa(int(req.Port)))
	q.Set("uploaded", strconv.FormatInt(req.Uploaded, 10))
	q.Set("downloaded", strconv.FormatInt(req.Downloaded, 10))
	q.Set("left", strconv.FormatInt(req.Left, 10))
	q.Set("numwant", strconv.Itoa(numWant))
	q.Set("compact", "1")
	if req.Event != EventNone {
		q.Set("event", string(req.Event))
	}
	base.RawQuery = q.Encode()
	return base.String(), nil
}

// ParseResponse decodes a tracker response.
func ParseResponse(body []byte) (*Response, error) {
	decoded, err := bencode.Unmarshal(body)
	if err != nil {
		return nil, fmt.Errorf("tracker: decoding response: %w", err)
	}
	dict, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tracker: response is not a dictionary")
	}

	// A tracker reports refusal in-band rather than by HTTP status.
	if failure, ok := dict["failure reason"].(string); ok {
		return nil, fmt.Errorf("tracker: %s", failure)
	}

	r := &Response{}
	if warning, ok := dict["warning message"].(string); ok {
		r.WarningMsg = warning
	}
	if interval, ok := dict["interval"].(int64); ok {
		r.Interval = time.Duration(interval) * time.Second
	}
	if minInterval, ok := dict["min interval"].(int64); ok {
		r.MinInterval = time.Duration(minInterval) * time.Second
	}
	r.Complete, _ = dict["complete"].(int64)
	r.Incomplete, _ = dict["incomplete"].(int64)
	r.TrackerID, _ = dict["tracker id"].(string)

	peers, err := parsePeers(dict["peers"])
	if err != nil {
		return nil, err
	}
	r.Peers = peers
	return r, nil
}

// parsePeers handles both peer list encodings: the compact form (6 bytes
// per peer) and the original dictionary form.
func parsePeers(v any) ([]peer.Peer, error) {
	switch peers := v.(type) {
	case string:
		return parseCompactPeers([]byte(peers))
	case []any:
		var out []peer.Peer
		for _, pAny := range peers {
			pDict, ok := pAny.(map[string]any)
			if !ok {
				continue
			}
			ipStr, _ := pDict["ip"].(string)
			port, _ := pDict["port"].(int64)
			ip := net.ParseIP(ipStr)
			if ip == nil || port <= 0 || port > 65535 {
				continue
			}
			out = append(out, peer.Peer{IP: ip, Port: uint16(port)})
		}
		return out, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("tracker: unexpected peers field of type %T", v)
	}
}

// parseCompactPeers reads the compact form: four bytes of IPv4 followed by
// two bytes of big-endian port, repeated.
func parseCompactPeers(data []byte) ([]peer.Peer, error) {
	const peerSize = 6
	if len(data)%peerSize != 0 {
		return nil, fmt.Errorf("tracker: compact peer list is %d bytes, not a multiple of %d", len(data), peerSize)
	}
	out := make([]peer.Peer, 0, len(data)/peerSize)
	for i := 0; i < len(data); i += peerSize {
		out = append(out, peer.Peer{
			IP:   net.IP(data[i : i+4]),
			Port: binary.BigEndian.Uint16(data[i+4 : i+6]),
		})
	}
	return out, nil
}
