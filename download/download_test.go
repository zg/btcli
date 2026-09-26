package download

import (
	"bytes"
	"context"
	"crypto/sha1"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zg/btcli/bencode"
	"github.com/zg/btcli/metainfo"
	"github.com/zg/btcli/peer"
)

// buildTorrent creates a torrent describing data, split into pieces of
// pieceLength.
func buildTorrent(t *testing.T, name string, data []byte, pieceLength int) *metainfo.Metainfo {
	t.Helper()
	var hashes strings.Builder
	for off := 0; off < len(data); off += pieceLength {
		end := min(off+pieceLength, len(data))
		h := sha1.Sum(data[off:end])
		hashes.Write(h[:])
	}
	raw, err := bencode.Marshal(map[string]any{
		"announce": "http://example.com/announce",
		"info": map[string]any{
			"name":         name,
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

// seeder is a peer that serves a complete copy of the data. It exists so
// the download path can be tested end to end over a real socket: handshake,
// bitfield, interest, requests and blocks all have to be right for it to
// work.
type seeder struct {
	listener net.Listener
	meta     *metainfo.Metainfo
	data     []byte
	t        *testing.T

	// corrupt makes the seeder serve wrong data, to test hash verification.
	corrupt bool
	// stall makes the seeder accept but never respond.
	stall bool

	mu     sync.Mutex
	served int
	closed bool
}

func newSeeder(t *testing.T, meta *metainfo.Metainfo, data []byte) *seeder {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &seeder{listener: l, meta: meta, data: data, t: t}
	t.Cleanup(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		l.Close()
	})
	go s.accept()
	return s
}

func (s *seeder) addr() peer.Peer {
	tcp := s.listener.Addr().(*net.TCPAddr)
	return peer.Peer{IP: tcp.IP, Port: uint16(tcp.Port)}
}

func (s *seeder) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *seeder) handle(conn net.Conn) {
	defer conn.Close()

	h, err := peer.ReadHandshake(conn)
	if err != nil {
		return
	}
	if err := h.Verify(s.meta.InfoHash); err != nil {
		return
	}
	var peerID [20]byte
	copy(peerID[:], "-SEEDER-000000000000")
	if _, err := conn.Write(peer.NewHandshake(s.meta.InfoHash, peerID).Serialize()); err != nil {
		return
	}

	if s.stall {
		// Accept, then never speak again.
		select {}
	}

	// Announce a complete bitfield.
	bf := peer.NewBitfield(s.meta.Info.NumPieces())
	for i := range s.meta.Info.NumPieces() {
		bf.SetPiece(i)
	}
	conn.Write((&peer.Message{ID: peer.MsgBitfield, Payload: bf}).Serialize())
	conn.Write((&peer.Message{ID: peer.MsgUnchoke}).Serialize())

	for {
		msg, err := peer.Read(conn)
		if err != nil {
			return
		}
		if msg == nil {
			continue
		}
		if msg.ID != peer.MsgRequest {
			continue
		}

		index, begin, length, err := peer.ParseRequest(msg)
		if err != nil {
			return
		}

		offset := int64(index)*s.meta.Info.PieceLength + int64(begin)
		if offset < 0 || offset >= int64(len(s.data)) {
			return
		}
		end := offset + int64(length)
		if end > int64(len(s.data)) {
			end = int64(len(s.data))
		}
		block := make([]byte, end-offset)
		copy(block, s.data[offset:end])

		if s.corrupt {
			for i := range block {
				block[i] ^= 0xFF
			}
		}

		payload := make([]byte, 8+len(block))
		payload[0] = byte(index >> 24)
		payload[1] = byte(index >> 16)
		payload[2] = byte(index >> 8)
		payload[3] = byte(index)
		payload[4] = byte(begin >> 24)
		payload[5] = byte(begin >> 16)
		payload[6] = byte(begin >> 8)
		payload[7] = byte(begin)
		copy(payload[8:], block)

		if _, err := conn.Write((&peer.Message{ID: peer.MsgPiece, Payload: payload}).Serialize()); err != nil {
			return
		}
		s.mu.Lock()
		s.served++
		s.mu.Unlock()
	}
}

// The whole point: a download must reproduce the original bytes exactly.
func TestDownloadEndToEnd(t *testing.T) {
	// Several pieces, with a short final piece so that case is exercised.
	data := bytes.Repeat([]byte("abcdefghij"), 5000) // 50,000 bytes
	meta := buildTorrent(t, "test.bin", data, 16384) // 4 pieces, last one short

	if meta.Info.NumPieces() != 4 {
		t.Fatalf("test setup: %d pieces, want 4", meta.Info.NumPieces())
	}

	s := newSeeder(t, meta, data)
	dir := t.TempDir()

	tor, err := New(meta, dir)
	if err != nil {
		t.Fatal(err)
	}
	tor.PeerTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := tor.Download(ctx, []peer.Peer{s.addr()}); err != nil {
		t.Fatalf("download: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "test.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("downloaded %d bytes, want %d; content differs", len(got), len(data))
	}
}

// A peer can send anything. Unverified data would silently corrupt the
// file, so a corrupt seeder must never produce a completed download.
func TestDownloadRejectsCorruptPieces(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 32768)
	meta := buildTorrent(t, "test.bin", data, 16384)

	s := newSeeder(t, meta, data)
	s.corrupt = true

	tor, err := New(meta, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tor.PeerTimeout = time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// It must not succeed. Either it errors, or the context expires with
	// the download still incomplete — never a clean finish with bad data.
	if err := tor.Download(ctx, []peer.Peer{s.addr()}); err == nil {
		t.Fatal("download reported success despite corrupt data")
	}
}

// Progress must be reported as pieces complete, or a UI has nothing to show.
func TestProgressReporting(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 49152) // 3 pieces
	meta := buildTorrent(t, "test.bin", data, 16384)
	s := newSeeder(t, meta, data)

	tor, err := New(meta, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tor.PeerTimeout = 5 * time.Second

	var mu sync.Mutex
	var updates []Progress
	tor.OnProgress(func(p Progress) {
		mu.Lock()
		updates = append(updates, p)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := tor.Download(ctx, []peer.Peer{s.addr()}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 3 {
		t.Fatalf("got %d progress updates, want 3", len(updates))
	}
	last := updates[len(updates)-1]
	if last.Pieces != 3 {
		t.Errorf("final pieces = %d, want 3", last.Pieces)
	}
	if last.Percent() != 100 {
		t.Errorf("final percent = %.1f, want 100", last.Percent())
	}
	// Progress must increase monotonically.
	for i := 1; i < len(updates); i++ {
		if updates[i].Downloaded < updates[i-1].Downloaded {
			t.Errorf("progress went backwards: %d then %d",
				updates[i-1].Downloaded, updates[i].Downloaded)
		}
	}
}

func TestDownloadNoPeers(t *testing.T) {
	meta := buildTorrent(t, "f", []byte("data"), 4)
	tor, _ := New(meta, t.TempDir())
	if err := tor.Download(context.Background(), nil); err == nil {
		t.Error("expected an error with no peers")
	}
}

// An unreachable peer must not hang the download forever.
func TestDownloadUnreachablePeer(t *testing.T) {
	meta := buildTorrent(t, "f", bytes.Repeat([]byte("z"), 16384), 16384)
	tor, _ := New(meta, t.TempDir())
	tor.PeerTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Port 1 on loopback refuses immediately.
	bad := peer.Peer{IP: net.ParseIP("127.0.0.1"), Port: 1}
	start := time.Now()
	if err := tor.Download(ctx, []peer.Peer{bad}); err == nil {
		t.Error("expected an error with an unreachable peer")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("took %v; should give up promptly", elapsed)
	}
}

func TestDownloadRespectsContext(t *testing.T) {
	data := bytes.Repeat([]byte("q"), 16384)
	meta := buildTorrent(t, "f", data, 16384)
	s := newSeeder(t, meta, data)
	s.stall = true

	tor, _ := New(meta, t.TempDir())
	tor.PeerTimeout = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := tor.Download(ctx, []peer.Peer{s.addr()}); err == nil {
		t.Error("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v despite a 300ms context", elapsed)
	}
}

// Verify is what makes a download resumable: it must recognise data
// already on disk.
func TestVerifyRecognisesCompleteData(t *testing.T) {
	data := bytes.Repeat([]byte("v"), 49152)
	meta := buildTorrent(t, "test.bin", data, 16384)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	have, err := Verify(dir, &meta.Info)
	if err != nil {
		t.Fatal(err)
	}
	if got := have.Count(); got != 3 {
		t.Errorf("verified %d pieces, want 3", got)
	}
}

func TestVerifyRejectsCorruptData(t *testing.T) {
	data := bytes.Repeat([]byte("v"), 32768)
	meta := buildTorrent(t, "test.bin", data, 16384)

	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	corrupt[0] ^= 0xFF // damage the first piece only

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.bin"), corrupt, 0o644)

	have, err := Verify(dir, &meta.Info)
	if err != nil {
		t.Fatal(err)
	}
	if have.HasPiece(0) {
		t.Error("corrupt piece 0 passed verification")
	}
	if !have.HasPiece(1) {
		t.Error("intact piece 1 failed verification")
	}
}

// A multi-file torrent is one byte stream split across files, so a piece
// can straddle a file boundary. Getting that mapping wrong corrupts both
// files.
func TestMultiFileWriterSpansBoundary(t *testing.T) {
	dir := t.TempDir()
	info := &metainfo.Info{
		Name:        "multi",
		PieceLength: 10,
		Files: []metainfo.File{
			{Length: 15, Path: []string{"a.txt"}},
			{Length: 15, Path: []string{"sub", "b.txt"}},
		},
	}

	w, err := newWriter(dir, info)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// This piece starts in a.txt and ends in b.txt.
	data := []byte("0123456789")
	if err := w.WriteAt(data, 10); err != nil {
		t.Fatal(err)
	}
	w.Close()

	a, err := os.ReadFile(filepath.Join(dir, "multi", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "multi", "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Bytes 10..14 go to a.txt (its last 5), 15..19 to b.txt (its first 5).
	if !bytes.Equal(a[10:15], data[:5]) {
		t.Errorf("first file got %q, want %q", a[10:15], data[:5])
	}
	if !bytes.Equal(b[0:5], data[5:]) {
		t.Errorf("second file got %q, want %q", b[0:5], data[5:])
	}
}

func TestGeneratePeerID(t *testing.T) {
	id, err := GeneratePeerID()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(id[:], []byte("-ZG0001-")) {
		t.Errorf("peer id lacks the client prefix: %q", id)
	}
	// Two IDs must differ, or peers cannot be told apart.
	other, _ := GeneratePeerID()
	if id == other {
		t.Error("two generated peer IDs are identical")
	}
}

func TestProgressPercent(t *testing.T) {
	if got := (Progress{Downloaded: 50, Total: 100}).Percent(); got != 50 {
		t.Errorf("= %v, want 50", got)
	}
	// Must not divide by zero.
	if got := (Progress{}).Percent(); got != 0 {
		t.Errorf("= %v, want 0", got)
	}
}
