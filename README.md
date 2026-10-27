# btcli

[![ci](https://github.com/zg/btcli/actions/workflows/ci.yml/badge.svg)](https://github.com/zg/btcli/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/zg/btcli.svg)](https://pkg.go.dev/github.com/zg/btcli) [![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An application intended for use with the BitTorrent protocol.

Implements bencode, `.torrent` parsing, HTTP tracker announces, the peer
wire protocol, and a piece downloader that verifies every piece before
writing it.

## Install

```sh
go install github.com/zg/btcli/cmd/btcli@latest
```

## Use

```sh
btcli file.torrent              # download into the current directory
btcli -o ~/Downloads file.torrent
btcli -info file.torrent        # show what the torrent describes
btcli -verify file.torrent      # check data already on disk
```

```
$ btcli -info ubuntu.torrent
name:         ubuntu-24.04.iso
info hash:    74a5f8a80e75a1b72995e0004f6928ce4d2f54a4
total size:   5.70 GiB
piece length: 512.00 KiB
pieces:       11673
trackers:
  https://torrent.ubuntu.com/announce
```

## Packages

| package | role |
|---|---|
| `bencode` | the encoding used by torrents and trackers |
| `metainfo` | `.torrent` parsing and the info hash |
| `tracker` | HTTP announce and peer list parsing |
| `peer` | handshake, wire messages, bitfields |
| `download` | piece scheduling, verification, file writing |

## Design notes

**Bencode dictionary keys must be sorted, and it matters.** The info hash is
the SHA-1 of the *re-encoded* info dictionary, so an encoder that emits keys
in Go's randomised map order produces a different hash on every run — and
one no tracker or peer recognises. The encoder sorts; a test re-encodes the
same dictionary fifty times and asserts the bytes are identical. The decoder
also *rejects* unsorted keys and non-canonical integers (`i03e`, `i-0e`)
rather than accepting input it could not reproduce.

**A torrent is untrusted input.** A file path of `..` would let a torrent
write outside the download directory — the "zip slip" traversal. Path
components are rejected at parse time, and `FullPath` re-checks that the
resolved path stays under the root. Both have tests.

**So is a peer.** `ParsePiece` validates the index, offset and block length
against the piece actually requested before copying anything: an unchecked
copy into a fixed buffer is a straightforward overflow. Message lengths are
capped, because a peer can claim any length it likes and a 4 GiB claim would
otherwise force a 4 GiB allocation. A test feeds it wrong indexes, huge
offsets and overflowing blocks, and asserts the destination buffer is never
touched.

**Every piece is verified before it is written.** A peer can send anything;
without the SHA-1 check the file is silently corrupted. A test runs a
deliberately corrupt seeder and asserts the download never reports success.

**Requests are pipelined.** Up to five block requests stay in flight per
peer. One at a time would pay a full round trip per 16 KiB block, which
makes throughput latency-bound rather than bandwidth-bound.

**Work is returned to the queue, not lost.** A peer that lacks a piece, or
fails mid-transfer, puts the work back for another peer rather than
abandoning it.

**Multi-file torrents are one byte stream.** A piece can straddle a file
boundary, so the writer maps stream offsets onto files and splits a write
across two of them where needed. Getting this wrong corrupts both files at
the seam, so it has a dedicated test.

## Tests

The download path is tested end to end against a **real seeder over a real
socket** — handshake, bitfield, interest, pipelined requests, blocks, hash
verification and file writing all have to be correct for the test to pass.
Variants of that seeder serve corrupt data or stall entirely, to check that
verification rejects the first and timeouts handle the second.

```sh
go test -race ./...
```

## Not implemented

UDP trackers, DHT, peer exchange, magnet links, and uploading beyond
announcing `have`. Tracker support is HTTP/HTTPS only, and an unsupported
scheme is reported rather than silently skipped.
