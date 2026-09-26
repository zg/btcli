// Command btcli downloads a torrent, or inspects a .torrent file.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/zg/btcli/download"
	"github.com/zg/btcli/metainfo"
	"github.com/zg/btcli/peer"
	"github.com/zg/btcli/tracker"
)

func main() {
	var (
		out     = flag.String("o", ".", "download directory")
		info    = flag.Bool("info", false, "print torrent information and exit")
		verify  = flag.Bool("verify", false, "verify existing data and exit")
		port    = flag.Int("port", 6881, "port to advertise to the tracker")
		timeout = flag.Duration("timeout", 30*time.Minute, "overall download timeout")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] file.torrent\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	meta, err := metainfo.ParseFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "btcli:", err)
		os.Exit(1)
	}

	switch {
	case *info:
		PrintInfo(os.Stdout, meta)
		return
	case *verify:
		if err := RunVerify(os.Stdout, meta, *out); err != nil {
			fmt.Fprintln(os.Stderr, "btcli:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := Run(ctx, os.Stdout, meta, *out, uint16(*port)); err != nil {
		fmt.Fprintln(os.Stderr, "btcli:", err)
		os.Exit(1)
	}
}

// PrintInfo shows what a .torrent describes.
func PrintInfo(w io.Writer, m *metainfo.Metainfo) {
	fmt.Fprintf(w, "name:         %s\n", m.Info.Name)
	fmt.Fprintf(w, "info hash:    %s\n", m.InfoHash)
	fmt.Fprintf(w, "total size:   %s\n", humanBytes(m.Info.TotalLength()))
	fmt.Fprintf(w, "piece length: %s\n", humanBytes(m.Info.PieceLength))
	fmt.Fprintf(w, "pieces:       %d\n", m.Info.NumPieces())
	fmt.Fprintf(w, "private:      %v\n", m.Info.Private)
	if m.Comment != "" {
		fmt.Fprintf(w, "comment:      %s\n", m.Comment)
	}
	if m.CreatedBy != "" {
		fmt.Fprintf(w, "created by:   %s\n", m.CreatedBy)
	}

	fmt.Fprintf(w, "trackers:\n")
	for _, t := range m.Trackers() {
		fmt.Fprintf(w, "  %s\n", t)
	}

	if m.Info.IsSingleFile() {
		fmt.Fprintf(w, "files:\n  %s (%s)\n", m.Info.Name, humanBytes(m.Info.Length))
		return
	}
	fmt.Fprintf(w, "files:\n")
	for _, f := range m.Info.Files {
		fmt.Fprintf(w, "  %s (%s)\n", strings.Join(f.Path, "/"), humanBytes(f.Length))
	}
}

// RunVerify checks data already on disk.
func RunVerify(w io.Writer, m *metainfo.Metainfo, dir string) error {
	have, err := download.Verify(dir, &m.Info)
	if err != nil {
		return err
	}
	got, total := have.Count(), m.Info.NumPieces()
	fmt.Fprintf(w, "%d of %d pieces present (%.1f%%)\n",
		got, total, float64(got)/float64(total)*100)
	if got != total {
		return fmt.Errorf("incomplete: %d pieces missing", total-got)
	}
	return nil
}

// Run announces to the trackers and downloads the torrent.
func Run(ctx context.Context, w io.Writer, m *metainfo.Metainfo, dir string, port uint16) error {
	tor, err := download.New(m, dir)
	if err != nil {
		return err
	}

	peers, err := CollectPeers(ctx, m, tor.PeerID, port)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return fmt.Errorf("no peers returned by any tracker")
	}
	fmt.Fprintf(w, "found %d peers, downloading %s (%s)\n",
		len(peers), m.Info.Name, humanBytes(m.Info.TotalLength()))

	// Throttle progress output: a piece can complete many times a second.
	var lastPrint time.Time
	tor.OnProgress(func(p download.Progress) {
		if time.Since(lastPrint) < 500*time.Millisecond && p.Pieces != p.TotalPieces {
			return
		}
		lastPrint = time.Now()
		fmt.Fprintf(w, "\r%6.2f%%  %s / %s  %d/%d pieces  %d peers    ",
			p.Percent(), humanBytes(p.Downloaded), humanBytes(p.Total),
			p.Pieces, p.TotalPieces, p.Peers)
	})

	if err := tor.Download(ctx, peers); err != nil {
		fmt.Fprintln(w)
		return err
	}
	fmt.Fprintf(w, "\ndone: %s\n", m.Info.Name)
	return nil
}

// CollectPeers announces to every tracker, returning the union of the peers
// they report. Trying them all means one dead tracker does not stop the
// download.
func CollectPeers(ctx context.Context, m *metainfo.Metainfo, peerID [20]byte, port uint16) ([]peer.Peer, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	seen := map[string]bool{}
	var peers []peer.Peer
	var lastErr error

	for _, url := range m.Trackers() {
		resp, err := tracker.Announce(ctx, client, tracker.Request{
			URL:      url,
			InfoHash: m.InfoHash,
			PeerID:   peerID,
			Port:     port,
			Left:     m.Info.TotalLength(),
			Event:    tracker.EventStarted,
		})
		if err != nil {
			lastErr = err
			continue
		}
		for _, p := range resp.Peers {
			if key := p.String(); !seen[key] {
				seen[key] = true
				peers = append(peers, p)
			}
		}
	}

	if len(peers) == 0 && lastErr != nil {
		return nil, fmt.Errorf("no tracker responded: %w", lastErr)
	}
	return peers, nil
}

// humanBytes formats a byte count.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
