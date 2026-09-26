// Package metainfo parses .torrent files.
package metainfo

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zg/btcli/bencode"
)

// HashSize is the length of a SHA-1 hash in bytes.
const HashSize = 20

// Hash is a SHA-1 hash.
type Hash [HashSize]byte

func (h Hash) String() string { return fmt.Sprintf("%x", h[:]) }

// File is one file within a multi-file torrent.
type File struct {
	Length int64
	Path   []string
}

// FullPath joins the file's path components under root, rejecting any
// component that would escape it.
func (f File) FullPath(root string) (string, error) {
	parts := append([]string{root}, f.Path...)
	joined := filepath.Join(parts...)

	// A torrent is untrusted input. A path component of ".." would let a
	// malicious file be written outside the download directory — the
	// classic "zip slip" traversal. Verify the result stays under root.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if absJoined != absRoot && !strings.HasPrefix(absJoined, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("metainfo: path %q escapes the download directory", strings.Join(f.Path, "/"))
	}
	return joined, nil
}

// Info is the info dictionary of a torrent.
type Info struct {
	Name        string
	PieceLength int64
	Pieces      []Hash
	Length      int64  // single-file mode
	Files       []File // multi-file mode
	Private     bool
}

// IsSingleFile reports whether the torrent describes one file.
func (i *Info) IsSingleFile() bool { return len(i.Files) == 0 }

// TotalLength returns the total size of all files.
func (i *Info) TotalLength() int64 {
	if i.IsSingleFile() {
		return i.Length
	}
	var total int64
	for _, f := range i.Files {
		total += f.Length
	}
	return total
}

// NumPieces returns the piece count.
func (i *Info) NumPieces() int { return len(i.Pieces) }

// PieceSize returns the length of piece index, which is shorter for the
// last piece unless the total divides evenly.
func (i *Info) PieceSize(index int) int64 {
	if index < 0 || index >= len(i.Pieces) {
		return 0
	}
	total := i.TotalLength()
	begin := int64(index) * i.PieceLength
	end := begin + i.PieceLength
	if end > total {
		end = total
	}
	return end - begin
}

// Metainfo is a parsed .torrent file.
type Metainfo struct {
	Announce     string
	AnnounceList [][]string
	Info         Info
	InfoHash     Hash
	Comment      string
	CreatedBy    string
}

// Trackers returns every tracker URL, preferring the announce-list when
// present and falling back to the single announce URL.
func (m *Metainfo) Trackers() []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, tier := range m.AnnounceList {
		for _, u := range tier {
			add(u)
		}
	}
	add(m.Announce)
	return out
}

// Errors returned when a torrent is malformed.
var (
	ErrNotDict     = errors.New("metainfo: torrent is not a dictionary")
	ErrMissingInfo = errors.New("metainfo: missing info dictionary")
	ErrBadPieces   = errors.New("metainfo: pieces field is not a multiple of 20 bytes")
	ErrNoLength    = errors.New("metainfo: info has neither length nor files")
)

// ParseFile reads a .torrent from disk.
func ParseFile(path string) (*Metainfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse reads a .torrent from memory.
func Parse(data []byte) (*Metainfo, error) {
	decoded, err := bencode.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("metainfo: %w", err)
	}
	dict, ok := decoded.(map[string]any)
	if !ok {
		return nil, ErrNotDict
	}

	m := &Metainfo{}
	m.Announce, _ = dict["announce"].(string)
	m.Comment, _ = dict["comment"].(string)
	m.CreatedBy, _ = dict["created by"].(string)

	if list, ok := dict["announce-list"].([]any); ok {
		for _, tierAny := range list {
			tier, ok := tierAny.([]any)
			if !ok {
				continue
			}
			var urls []string
			for _, u := range tier {
				if s, ok := u.(string); ok {
					urls = append(urls, s)
				}
			}
			if len(urls) > 0 {
				m.AnnounceList = append(m.AnnounceList, urls)
			}
		}
	}

	infoAny, ok := dict["info"]
	if !ok {
		return nil, ErrMissingInfo
	}
	infoDict, ok := infoAny.(map[string]any)
	if !ok {
		return nil, ErrMissingInfo
	}

	if err := parseInfo(infoDict, &m.Info); err != nil {
		return nil, err
	}

	// The info hash is the SHA-1 of the *re-encoded* info dictionary. This
	// only matches what trackers and peers expect because the encoder
	// sorts dictionary keys canonically.
	encoded, err := bencode.Marshal(infoDict)
	if err != nil {
		return nil, fmt.Errorf("metainfo: re-encoding info: %w", err)
	}
	m.InfoHash = sha1.Sum(encoded)

	return m, nil
}

func parseInfo(dict map[string]any, info *Info) error {
	info.Name, _ = dict["name"].(string)
	if info.Name == "" {
		info.Name = "unnamed"
	}
	// A name must not be usable to escape the download directory.
	if strings.Contains(info.Name, "/") || strings.Contains(info.Name, "\\") || info.Name == ".." {
		return fmt.Errorf("metainfo: unsafe name %q", info.Name)
	}

	pieceLength, ok := dict["piece length"].(int64)
	if !ok || pieceLength <= 0 {
		return fmt.Errorf("metainfo: invalid piece length")
	}
	info.PieceLength = pieceLength

	piecesStr, ok := dict["pieces"].(string)
	if !ok {
		return ErrBadPieces
	}
	if len(piecesStr)%HashSize != 0 {
		return ErrBadPieces
	}
	pieces := []byte(piecesStr)
	info.Pieces = make([]Hash, len(pieces)/HashSize)
	for i := range info.Pieces {
		copy(info.Pieces[i][:], pieces[i*HashSize:(i+1)*HashSize])
	}

	if p, ok := dict["private"].(int64); ok {
		info.Private = p == 1
	}

	// Single-file mode has `length`; multi-file mode has `files`.
	if length, ok := dict["length"].(int64); ok {
		info.Length = length
		return validateLength(info)
	}

	filesAny, ok := dict["files"].([]any)
	if !ok {
		return ErrNoLength
	}
	for _, fAny := range filesAny {
		fDict, ok := fAny.(map[string]any)
		if !ok {
			return fmt.Errorf("metainfo: file entry is not a dictionary")
		}
		length, ok := fDict["length"].(int64)
		if !ok || length < 0 {
			return fmt.Errorf("metainfo: file has an invalid length")
		}
		pathAny, ok := fDict["path"].([]any)
		if !ok || len(pathAny) == 0 {
			return fmt.Errorf("metainfo: file has no path")
		}
		var parts []string
		for _, p := range pathAny {
			s, ok := p.(string)
			if !ok {
				return fmt.Errorf("metainfo: path component is not a string")
			}
			// Reject traversal components at parse time rather than
			// relying on every caller to check.
			if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
				return fmt.Errorf("metainfo: unsafe path component %q", s)
			}
			parts = append(parts, s)
		}
		info.Files = append(info.Files, File{Length: length, Path: parts})
	}
	return validateLength(info)
}

// validateLength checks that the piece count matches the declared total
// size, which catches a truncated or inconsistent torrent early.
func validateLength(info *Info) error {
	total := info.TotalLength()
	if total <= 0 {
		return ErrNoLength
	}
	want := int((total + info.PieceLength - 1) / info.PieceLength)
	if want != len(info.Pieces) {
		return fmt.Errorf("metainfo: %d pieces declared but %d needed for %d bytes",
			len(info.Pieces), want, total)
	}
	return nil
}

// VerifyPiece reports whether data hashes to the expected piece hash.
func (i *Info) VerifyPiece(index int, data []byte) bool {
	if index < 0 || index >= len(i.Pieces) {
		return false
	}
	sum := sha1.Sum(data)
	return bytes.Equal(sum[:], i.Pieces[index][:])
}
