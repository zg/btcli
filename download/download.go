// Package download coordinates fetching a torrent's pieces from peers and
// writing them to disk.
package download

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zg/btcli/metainfo"
	"github.com/zg/btcli/peer"
)

const (
	// BlockSize is the block length every client uses. Pieces are
	// requested in blocks because a piece can be megabytes.
	BlockSize = 16 * 1024
	// MaxBacklog is how many requests are kept in flight per peer.
	// Pipelining is what makes throughput acceptable: one request at a
	// time means paying a full round trip per 16 KiB.
	MaxBacklog = 5
)

// PieceWork is a piece to fetch.
type PieceWork struct {
	Index  int
	Hash   metainfo.Hash
	Length int
}

// PieceResult is a completed, hash-verified piece.
type PieceResult struct {
	Index int
	Data  []byte
}

// Progress reports download state to a caller.
type Progress struct {
	Downloaded  int64
	Total       int64
	Pieces      int
	TotalPieces int
	Peers       int
}

// Percent returns the completion percentage.
func (p Progress) Percent() float64 {
	if p.Total == 0 {
		return 0
	}
	return float64(p.Downloaded) / float64(p.Total) * 100
}

// Torrent is a download in progress.
type Torrent struct {
	Meta   *metainfo.Metainfo
	PeerID [20]byte
	Dir    string

	// Timeouts for peer operations.
	PeerTimeout time.Duration

	downloaded atomic.Int64
	donePieces atomic.Int64
	numPeers   atomic.Int64

	mu      sync.Mutex
	onPiece func(Progress)
}

// New returns a torrent ready to download into dir.
func New(meta *metainfo.Metainfo, dir string) (*Torrent, error) {
	peerID, err := GeneratePeerID()
	if err != nil {
		return nil, err
	}
	return &Torrent{
		Meta:        meta,
		PeerID:      peerID,
		Dir:         dir,
		PeerTimeout: 15 * time.Second,
	}, nil
}

// GeneratePeerID returns a random peer ID with the conventional
// "-XX0000-" client prefix.
func GeneratePeerID() ([20]byte, error) {
	var id [20]byte
	copy(id[:], "-ZG0001-")
	if _, err := rand.Read(id[8:]); err != nil {
		return id, fmt.Errorf("download: generating peer id: %w", err)
	}
	return id, nil
}

// OnProgress registers a progress callback.
func (t *Torrent) OnProgress(fn func(Progress)) {
	t.mu.Lock()
	t.onPiece = fn
	t.mu.Unlock()
}

func (t *Torrent) report() {
	t.mu.Lock()
	fn := t.onPiece
	t.mu.Unlock()
	if fn == nil {
		return
	}
	fn(Progress{
		Downloaded:  t.downloaded.Load(),
		Total:       t.Meta.Info.TotalLength(),
		Pieces:      int(t.donePieces.Load()),
		TotalPieces: t.Meta.Info.NumPieces(),
		Peers:       int(t.numPeers.Load()),
	})
}

// Download fetches every piece from the given peers and writes the result
// to disk.
func (t *Torrent) Download(ctx context.Context, peers []peer.Peer) error {
	if len(peers) == 0 {
		return errors.New("download: no peers available")
	}

	info := &t.Meta.Info
	work := make(chan *PieceWork, info.NumPieces())
	results := make(chan *PieceResult, info.NumPieces())

	for i, hash := range info.Pieces {
		work <- &PieceWork{Index: i, Hash: hash, Length: int(info.PieceSize(i))}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p peer.Peer) {
			defer wg.Done()
			t.serve(ctx, p, work, results)
		}(p)
	}

	// Close results once every peer worker has stopped, so the collector
	// below terminates even if the swarm cannot supply every piece.
	go func() {
		wg.Wait()
		close(results)
	}()

	writer, err := newWriter(t.Dir, info)
	if err != nil {
		return err
	}
	defer writer.Close()

	collected := 0
	for collected < info.NumPieces() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res, ok := <-results:
			if !ok {
				return fmt.Errorf("download: peers exhausted with %d of %d pieces",
					collected, info.NumPieces())
			}
			offset := int64(res.Index) * info.PieceLength
			if err := writer.WriteAt(res.Data, offset); err != nil {
				return err
			}
			collected++
			t.donePieces.Add(1)
			t.downloaded.Add(int64(len(res.Data)))
			t.report()
		}
	}

	cancel()
	return nil
}

// serve runs one peer: connect, then repeatedly take work and fetch it.
func (t *Torrent) serve(ctx context.Context, p peer.Peer, work chan *PieceWork, results chan<- *PieceResult) {
	c, err := peer.Dial(ctx, p, t.Meta.InfoHash, t.PeerID, t.Meta.Info.NumPieces(), t.PeerTimeout)
	if err != nil {
		return
	}
	defer c.Close()

	t.numPeers.Add(1)
	defer t.numPeers.Add(-1)

	c.SendUnchoke()
	c.SendInterested()

	for {
		select {
		case <-ctx.Done():
			return
		case pw, ok := <-work:
			if !ok {
				return
			}
			// A peer that does not have this piece cannot serve it; put it
			// back for someone else rather than failing.
			if !c.Bitfield.HasPiece(pw.Index) {
				select {
				case work <- pw:
				case <-ctx.Done():
				}
				continue
			}

			data, err := t.fetchPiece(ctx, c, pw)
			if err != nil {
				// Return the work so another peer can try it. This peer is
				// finished: an error here usually means the connection is
				// unusable.
				select {
				case work <- pw:
				case <-ctx.Done():
				}
				return
			}

			// Verify before accepting. A peer can send anything, and an
			// unverified piece would silently corrupt the file.
			if !t.Meta.Info.VerifyPiece(pw.Index, data) {
				select {
				case work <- pw:
				case <-ctx.Done():
				}
				continue
			}

			c.SendHave(pw.Index)
			select {
			case results <- &PieceResult{Index: pw.Index, Data: data}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// fetchPiece downloads one piece, pipelining block requests.
func (t *Torrent) fetchPiece(ctx context.Context, c *peer.Conn, pw *PieceWork) ([]byte, error) {
	buf := make([]byte, pw.Length)
	downloaded, requested, backlog := 0, 0, 0

	// A deadline bounds the whole piece: a peer that stops responding
	// mid-piece must not hold the slot forever.
	c.SetDeadline(time.Now().Add(t.PeerTimeout * 4))
	defer c.SetDeadline(time.Time{})

	for downloaded < pw.Length {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Only request while unchoked; a choked peer discards requests.
		if !c.Choked {
			for backlog < MaxBacklog && requested < pw.Length {
				length := BlockSize
				// The final block of the final piece is short.
				if pw.Length-requested < length {
					length = pw.Length - requested
				}
				if err := c.SendRequest(pw.Index, requested, length); err != nil {
					return nil, err
				}
				backlog++
				requested += length
			}
		}

		msg, err := c.Read()
		if err != nil {
			return nil, err
		}
		if c.HandleState(msg) {
			continue // keep-alive, choke, unchoke or have
		}
		if msg.ID == peer.MsgPiece {
			n, err := peer.ParsePiece(pw.Index, buf, msg)
			if err != nil {
				return nil, err
			}
			downloaded += n
			backlog--
		}
	}
	return buf, nil
}

// writer maps piece offsets onto the torrent's files. A multi-file torrent
// is one contiguous byte stream split across files at arbitrary offsets, so
// a single write can span two of them.
type writer struct {
	files  []*os.File
	spans  []span
	single bool
}

type span struct {
	file       *os.File
	begin, end int64 // offsets within the overall stream
}

func newWriter(dir string, info *metainfo.Info) (*writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	w := &writer{}
	if info.IsSingleFile() {
		path := filepath.Join(dir, info.Name)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		w.files = append(w.files, f)
		w.spans = append(w.spans, span{file: f, begin: 0, end: info.Length})
		w.single = true
		return w, nil
	}

	root := filepath.Join(dir, info.Name)
	var offset int64
	for _, mf := range info.Files {
		path, err := mf.FullPath(root)
		if err != nil {
			w.Close()
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			w.Close()
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			w.Close()
			return nil, err
		}
		w.files = append(w.files, f)
		w.spans = append(w.spans, span{file: f, begin: offset, end: offset + mf.Length})
		offset += mf.Length
	}
	return w, nil
}

// WriteAt writes data at a stream offset, splitting it across files where
// the write crosses a file boundary.
func (w *writer) WriteAt(data []byte, offset int64) error {
	for _, s := range w.spans {
		if offset >= s.end || offset+int64(len(data)) <= s.begin {
			continue
		}
		// The portion of data that lands in this file.
		from := max64(offset, s.begin)
		to := min64(offset+int64(len(data)), s.end)
		chunk := data[from-offset : to-offset]
		if _, err := s.file.WriteAt(chunk, from-s.begin); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) Close() error {
	var firstErr error
	for _, f := range w.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Verify checks already-downloaded data on disk, returning a bitfield of
// the pieces that are present and correct. This is what makes a download
// resumable.
func Verify(dir string, info *metainfo.Info) (peer.Bitfield, error) {
	have := peer.NewBitfield(info.NumPieces())

	r, err := newReader(dir, info)
	if err != nil {
		return have, err
	}
	defer r.Close()

	buf := make([]byte, info.PieceLength)
	for i := range info.NumPieces() {
		size := info.PieceSize(i)
		n, err := r.ReadAt(buf[:size], int64(i)*info.PieceLength)
		if err != nil && !errors.Is(err, io.EOF) {
			continue
		}
		if int64(n) != size {
			continue
		}
		if info.VerifyPiece(i, buf[:size]) {
			have.SetPiece(i)
		}
	}
	return have, nil
}

type reader struct{ w *writer }

func newReader(dir string, info *metainfo.Info) (*reader, error) {
	w, err := newWriter(dir, info)
	if err != nil {
		return nil, err
	}
	return &reader{w: w}, nil
}

func (r *reader) ReadAt(buf []byte, offset int64) (int, error) {
	total := 0
	for _, s := range r.w.spans {
		if offset >= s.end || offset+int64(len(buf)) <= s.begin {
			continue
		}
		from := max64(offset, s.begin)
		to := min64(offset+int64(len(buf)), s.end)
		n, err := s.file.ReadAt(buf[from-offset:to-offset], from-s.begin)
		total += n
		if err != nil && !errors.Is(err, io.EOF) {
			return total, err
		}
	}
	return total, nil
}

func (r *reader) Close() error { return r.w.Close() }
