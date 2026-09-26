package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zg/btcli/bencode"
	"github.com/zg/btcli/metainfo"
)

func testTorrent(t *testing.T, data []byte, pieceLength int) *metainfo.Metainfo {
	t.Helper()
	var hashes strings.Builder
	for off := 0; off < len(data); off += pieceLength {
		end := min(off+pieceLength, len(data))
		h := sha1.Sum(data[off:end])
		hashes.Write(h[:])
	}
	raw, err := bencode.Marshal(map[string]any{
		"announce":   "http://tracker.example.com/announce",
		"comment":    "a test torrent",
		"created by": "btcli tests",
		"info": map[string]any{
			"name":         "test.bin",
			"piece length": int64(pieceLength),
			"pieces":       hashes.String(),
			"length":       int64(len(data)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPrintInfo(t *testing.T) {
	m := testTorrent(t, bytes.Repeat([]byte("x"), 32768), 16384)

	var out bytes.Buffer
	PrintInfo(&out, m)
	got := out.String()

	for _, want := range []string{
		"test.bin", m.InfoHash.String(), "32.00 KiB", "pieces:       2",
		"tracker.example.com", "a test torrent", "btcli tests",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunVerify(t *testing.T) {
	data := bytes.Repeat([]byte("v"), 32768)
	m := testTorrent(t, data, 16384)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunVerify(&out, m, dir); err != nil {
		t.Fatalf("complete data should verify: %v", err)
	}
	if !strings.Contains(out.String(), "2 of 2") {
		t.Errorf("got %q", out.String())
	}
}

func TestRunVerifyIncomplete(t *testing.T) {
	data := bytes.Repeat([]byte("v"), 32768)
	m := testTorrent(t, data, 16384)

	dir := t.TempDir()
	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	corrupt[0] ^= 0xFF
	os.WriteFile(filepath.Join(dir, "test.bin"), corrupt, 0o644)

	var out bytes.Buffer
	if err := RunVerify(&out, m, dir); err == nil {
		t.Error("corrupt data should not verify")
	}
	if !strings.Contains(out.String(), "1 of 2") {
		t.Errorf("got %q", out.String())
	}
}

// One dead tracker must not stop the download when another works.
func TestCollectPeersTriesEveryTracker(t *testing.T) {
	body, _ := bencode.Marshal(map[string]any{
		"interval": int64(1800),
		"peers":    string([]byte{10, 0, 0, 1, 0x1A, 0xE1}),
	})

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()

	raw, _ := bencode.Marshal(map[string]any{
		"announce": bad.URL,
		"announce-list": []any{
			[]any{bad.URL},
			[]any{good.URL},
		},
		"info": map[string]any{
			"name": "f", "piece length": int64(4),
			"pieces": hashOf([]byte("aaaa")), "length": int64(4),
		},
	})
	m, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	peers, err := CollectPeers(context.Background(), m, [20]byte{}, 6881)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(peers))
	}
}

func TestCollectPeersAllTrackersFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()

	raw, _ := bencode.Marshal(map[string]any{
		"announce": bad.URL,
		"info": map[string]any{
			"name": "f", "piece length": int64(4),
			"pieces": hashOf([]byte("aaaa")), "length": int64(4),
		},
	})
	m, _ := metainfo.Parse(raw)

	if _, err := CollectPeers(context.Background(), m, [20]byte{}, 6881); err == nil {
		t.Error("expected an error when every tracker fails")
	}
}

func hashOf(data []byte) string {
	h := sha1.Sum(data)
	return string(h[:])
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.00 KiB",
		1536:       "1.50 KiB",
		1048576:    "1.00 MiB",
		1073741824: "1.00 GiB",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
