package tracker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zg/btcli/bencode"
)

func TestParseCompactPeers(t *testing.T) {
	// 127.0.0.1:6881 and 192.168.1.1:51413
	compact := []byte{
		127, 0, 0, 1, 0x1A, 0xE1,
		192, 168, 1, 1, 0xC8, 0xD5,
	}
	body, err := bencode.Marshal(map[string]any{
		"interval": int64(1800),
		"peers":    string(compact),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(resp.Peers))
	}
	if got := resp.Peers[0].String(); got != "127.0.0.1:6881" {
		t.Errorf("peer 0 = %q", got)
	}
	if got := resp.Peers[1].String(); got != "192.168.1.1:51413" {
		t.Errorf("peer 1 = %q", got)
	}
	if resp.Interval != 30*time.Minute {
		t.Errorf("interval = %v, want 30m", resp.Interval)
	}
}

// A compact list must be a multiple of six bytes; anything else is
// malformed and must be reported rather than silently truncated.
func TestParseCompactPeersRejectsBadLength(t *testing.T) {
	body, _ := bencode.Marshal(map[string]any{"peers": "12345"}) // 5 bytes
	if _, err := ParseResponse(body); err == nil {
		t.Error("expected an error for a malformed compact peer list")
	}
}

// The original dictionary form must also be supported.
func TestParseDictionaryPeers(t *testing.T) {
	body, err := bencode.Marshal(map[string]any{
		"interval": int64(900),
		"peers": []any{
			map[string]any{"ip": "10.0.0.1", "port": int64(6881)},
			map[string]any{"ip": "10.0.0.2", "port": int64(6882)},
			// Invalid entries are skipped rather than failing the whole
			// response.
			map[string]any{"ip": "not-an-ip", "port": int64(1)},
			map[string]any{"ip": "10.0.0.3", "port": int64(0)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Peers) != 2 {
		t.Fatalf("got %d peers, want 2 (invalid entries skipped): %v", len(resp.Peers), resp.Peers)
	}
}

// A tracker reports refusal in the body, not by HTTP status.
func TestParseFailureReason(t *testing.T) {
	body, _ := bencode.Marshal(map[string]any{
		"failure reason": "torrent not registered with this tracker",
	})
	_, err := ParseResponse(body)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error does not carry the reason: %v", err)
	}
}

func TestParseResponseFields(t *testing.T) {
	body, _ := bencode.Marshal(map[string]any{
		"complete":        int64(10),
		"incomplete":      int64(5),
		"interval":        int64(1800),
		"min interval":    int64(900),
		"tracker id":      "abc123",
		"warning message": "watch out",
		"peers":           "",
	})
	resp, err := ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Complete != 10 || resp.Incomplete != 5 {
		t.Errorf("seeders/leechers = %d/%d, want 10/5", resp.Complete, resp.Incomplete)
	}
	if resp.MinInterval != 15*time.Minute {
		t.Errorf("min interval = %v", resp.MinInterval)
	}
	if resp.TrackerID != "abc123" {
		t.Errorf("tracker id = %q", resp.TrackerID)
	}
	if resp.WarningMsg != "watch out" {
		t.Errorf("warning = %q", resp.WarningMsg)
	}
}

func TestParseResponseErrors(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("not bencode"),
		[]byte("li1ee"), // not a dictionary
		[]byte(""),
	} {
		if _, err := ParseResponse(body); err == nil {
			t.Errorf("%q should fail to parse", body)
		}
	}
}

// info_hash and peer_id are raw 20-byte values, not text. They must be
// percent-encoded correctly or the tracker will not recognise the torrent.
func TestBuildURLEscapesBinaryFields(t *testing.T) {
	var infoHash, peerID [20]byte
	// Deliberately include bytes that must be escaped.
	for i := range infoHash {
		infoHash[i] = byte(i)
	}
	copy(peerID[:], "-ZG0001-\x00\xff\x01\x02abcdef")

	raw, err := buildURL(Request{
		URL:      "http://tracker.example.com/announce",
		InfoHash: infoHash,
		PeerID:   peerID,
		Port:     6881,
		Left:     1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("produced an unparseable URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("info_hash"); got != string(infoHash[:]) {
		t.Errorf("info_hash did not survive escaping: %q", got)
	}
	if got := q.Get("peer_id"); got != string(peerID[:]) {
		t.Errorf("peer_id did not survive escaping: %q", got)
	}
	if q.Get("compact") != "1" {
		t.Error("compact should be requested")
	}
	if q.Get("port") != "6881" {
		t.Errorf("port = %q", q.Get("port"))
	}
}

func TestBuildURLRejectsUnsupportedScheme(t *testing.T) {
	for _, u := range []string{"udp://tracker.example.com:6969", "ftp://x/y", "://bad"} {
		if _, err := buildURL(Request{URL: u}); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestBuildURLPreservesExistingQuery(t *testing.T) {
	raw, err := buildURL(Request{URL: "http://t.example.com/announce?key=value"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if u.Query().Get("key") != "value" {
		t.Errorf("existing query parameter was lost: %s", raw)
	}
}

func TestAnnounceAgainstServer(t *testing.T) {
	compact := []byte{127, 0, 0, 1, 0x1A, 0xE1}
	body, _ := bencode.Marshal(map[string]any{
		"interval": int64(1800),
		"peers":    string(compact),
	})

	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Write(body)
	}))
	defer server.Close()

	resp, err := Announce(context.Background(), server.Client(), Request{
		URL:   server.URL,
		Port:  6881,
		Left:  1000,
		Event: EventStarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(resp.Peers))
	}
	if gotQuery.Get("event") != "started" {
		t.Errorf("event = %q", gotQuery.Get("event"))
	}
	if gotQuery.Get("left") != "1000" {
		t.Errorf("left = %q", gotQuery.Get("left"))
	}
}

func TestAnnounceHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer server.Close()

	_, err := Announce(context.Background(), server.Client(), Request{URL: server.URL})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "410") {
		t.Errorf("error should carry the status: %v", err)
	}
}

func TestAnnounceRespectsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Announce(ctx, server.Client(), Request{URL: server.URL}); err == nil {
		t.Error("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v despite a 50ms context", elapsed)
	}
}

func TestPeerString(t *testing.T) {
	// Covered indirectly elsewhere, but pin the format directly.
	body, _ := bencode.Marshal(map[string]any{
		"peers": string([]byte{10, 0, 0, 1, 0x1A, 0xE1}),
	})
	resp, err := ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Peers[0].String(); got != "10.0.0.1:6881" {
		t.Errorf("= %q", got)
	}
	if !resp.Peers[0].IP.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("IP = %v", resp.Peers[0].IP)
	}
}
