package metainfo

import (
	"crypto/sha1"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zg/btcli/bencode"
)

// buildTorrent constructs a valid .torrent from an info dictionary.
func buildTorrent(t *testing.T, info map[string]any) []byte {
	t.Helper()
	data, err := bencode.Marshal(map[string]any{
		"announce": "http://tracker.example.com/announce",
		"info":     info,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// pieceHashes returns concatenated SHA-1 hashes for the given piece data.
func pieceHashes(pieces ...[]byte) string {
	var sb strings.Builder
	for _, p := range pieces {
		h := sha1.Sum(p)
		sb.Write(h[:])
	}
	return sb.String()
}

func TestParseSingleFile(t *testing.T) {
	piece := []byte("hello world, this is piece data")
	data := buildTorrent(t, map[string]any{
		"name":         "test.txt",
		"piece length": int64(len(piece)),
		"pieces":       pieceHashes(piece),
		"length":       int64(len(piece)),
	})

	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Info.Name != "test.txt" {
		t.Errorf("name = %q", m.Info.Name)
	}
	if !m.Info.IsSingleFile() {
		t.Error("should be single-file mode")
	}
	if m.Info.NumPieces() != 1 {
		t.Errorf("pieces = %d, want 1", m.Info.NumPieces())
	}
	if m.Info.TotalLength() != int64(len(piece)) {
		t.Errorf("total = %d, want %d", m.Info.TotalLength(), len(piece))
	}
	if m.Announce == "" {
		t.Error("announce URL not parsed")
	}
}

func TestParseMultiFile(t *testing.T) {
	a, b := []byte("aaaa"), []byte("bbbb")
	data := buildTorrent(t, map[string]any{
		"name":         "mydir",
		"piece length": int64(4),
		"pieces":       pieceHashes(a, b),
		"files": []any{
			map[string]any{"length": int64(4), "path": []any{"sub", "a.txt"}},
			map[string]any{"length": int64(4), "path": []any{"b.txt"}},
		},
	})

	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Info.IsSingleFile() {
		t.Error("should be multi-file mode")
	}
	if len(m.Info.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(m.Info.Files))
	}
	if m.Info.TotalLength() != 8 {
		t.Errorf("total = %d, want 8", m.Info.TotalLength())
	}
	if got := strings.Join(m.Info.Files[0].Path, "/"); got != "sub/a.txt" {
		t.Errorf("path = %q", got)
	}
}

// The info hash must be the SHA-1 of the re-encoded info dictionary. It is
// the torrent's identity, so getting it wrong makes the torrent
// unrecognisable to every tracker and peer.
func TestInfoHashMatchesReEncodedInfo(t *testing.T) {
	piece := []byte("some piece data here")
	info := map[string]any{
		"name":         "f.bin",
		"piece length": int64(len(piece)),
		"pieces":       pieceHashes(piece),
		"length":       int64(len(piece)),
	}
	data := buildTorrent(t, info)

	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	want := sha1.Sum(encoded)
	if m.InfoHash != want {
		t.Errorf("info hash = %x, want %x", m.InfoHash, want)
	}
}

// Parsing the same torrent twice must give the same hash — it would not if
// the encoder did not sort keys.
func TestInfoHashIsStable(t *testing.T) {
	piece := []byte("data")
	data := buildTorrent(t, map[string]any{
		"name": "f", "piece length": int64(4),
		"pieces": pieceHashes(piece), "length": int64(4),
		// Extra keys, deliberately not in alphabetical order in the source.
		"private": int64(1), "source": "test",
	})

	first, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		if again.InfoHash != first.InfoHash {
			t.Fatalf("info hash varies between parses: %x vs %x", first.InfoHash, again.InfoHash)
		}
	}
}

func TestPieceSize(t *testing.T) {
	// 10 bytes in pieces of 4: sizes are 4, 4, 2.
	a, b, c := []byte("aaaa"), []byte("bbbb"), []byte("cc")
	data := buildTorrent(t, map[string]any{
		"name": "f", "piece length": int64(4),
		"pieces": pieceHashes(a, b, c), "length": int64(10),
	})
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{4, 4, 2}
	for i, w := range want {
		if got := m.Info.PieceSize(i); got != w {
			t.Errorf("PieceSize(%d) = %d, want %d", i, got, w)
		}
	}
	// Out of range returns zero rather than panicking.
	if got := m.Info.PieceSize(99); got != 0 {
		t.Errorf("PieceSize(99) = %d, want 0", got)
	}
}

func TestVerifyPiece(t *testing.T) {
	piece := []byte("the quick brown fox")
	data := buildTorrent(t, map[string]any{
		"name": "f", "piece length": int64(len(piece)),
		"pieces": pieceHashes(piece), "length": int64(len(piece)),
	})
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Info.VerifyPiece(0, piece) {
		t.Error("correct data failed verification")
	}
	if m.Info.VerifyPiece(0, []byte("wrong data here...")) {
		t.Error("wrong data passed verification")
	}
	if m.Info.VerifyPiece(99, piece) {
		t.Error("out-of-range index should not verify")
	}
}

// A torrent is untrusted input: a path of ".." must not let a file be
// written outside the download directory.
func TestRejectsPathTraversal(t *testing.T) {
	tests := []struct {
		name string
		path []any
	}{
		{"parent", []any{"..", "escaped.txt"}},
		{"nested parent", []any{"sub", "..", "..", "escaped.txt"}},
		{"slash in component", []any{"a/b"}},
		{"backslash", []any{`a\b`}},
		{"empty component", []any{""}},
		{"dot", []any{"."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildTorrent(t, map[string]any{
				"name": "dir", "piece length": int64(4),
				"pieces": pieceHashes([]byte("aaaa")),
				"files":  []any{map[string]any{"length": int64(4), "path": tt.path}},
			})
			if _, err := Parse(data); err == nil {
				t.Errorf("path %v should be rejected", tt.path)
			}
		})
	}
}

func TestRejectsUnsafeName(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", ".."} {
		t.Run(name, func(t *testing.T) {
			data := buildTorrent(t, map[string]any{
				"name": name, "piece length": int64(4),
				"pieces": pieceHashes([]byte("aaaa")), "length": int64(4),
			})
			if _, err := Parse(data); err == nil {
				t.Error("unsafe name should be rejected")
			}
		})
	}
}

// FullPath is the last line of defence, so test it directly too.
func TestFullPathRejectsEscape(t *testing.T) {
	root := t.TempDir()
	safe := File{Path: []string{"sub", "file.txt"}}
	got, err := safe.FullPath(root)
	if err != nil {
		t.Fatalf("safe path rejected: %v", err)
	}
	if !strings.HasPrefix(got, root) {
		t.Errorf("path %q is not under %q", got, root)
	}

	escaping := File{Path: []string{"..", "..", "etc", "passwd"}}
	if _, err := escaping.FullPath(root); err == nil {
		t.Error("escaping path should be rejected")
	}
}

// A torrent whose piece count disagrees with its declared length is
// inconsistent and must be rejected rather than silently mis-downloaded.
func TestRejectsInconsistentPieceCount(t *testing.T) {
	data := buildTorrent(t, map[string]any{
		"name": "f", "piece length": int64(4),
		"pieces": pieceHashes([]byte("aaaa")), // one piece
		"length": int64(100),                  // but 25 pieces' worth of data
	})
	if _, err := Parse(data); err == nil {
		t.Error("inconsistent piece count should be rejected")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"not bencode", []byte("not a torrent")},
		{"not a dict", []byte("li1ee")},
		{"no info", []byte("d8:announce3:urle")},
		{"empty", []byte("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.data); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestRejectsBadPiecesLength(t *testing.T) {
	data := buildTorrent(t, map[string]any{
		"name": "f", "piece length": int64(4),
		"pieces": "not a multiple of twenty", "length": int64(4),
	})
	if _, err := Parse(data); err == nil {
		t.Error("pieces not a multiple of 20 should be rejected")
	}
}

func TestTrackers(t *testing.T) {
	data, err := bencode.Marshal(map[string]any{
		"announce": "http://primary.example.com/announce",
		"announce-list": []any{
			[]any{"http://a.example.com/announce"},
			[]any{"http://b.example.com/announce", "http://primary.example.com/announce"},
		},
		"info": map[string]any{
			"name": "f", "piece length": int64(4),
			"pieces": pieceHashes([]byte("aaaa")), "length": int64(4),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	trackers := m.Trackers()
	if len(trackers) != 3 {
		t.Errorf("got %d trackers, want 3 (duplicates removed): %v", len(trackers), trackers)
	}
	// The announce-list comes first, and duplicates are dropped.
	if trackers[0] != "http://a.example.com/announce" {
		t.Errorf("first tracker = %q", trackers[0])
	}
}

func TestParseFileMissing(t *testing.T) {
	if _, err := ParseFile(filepath.Join(t.TempDir(), "nope.torrent")); err == nil {
		t.Error("expected an error for a missing file")
	}
}
