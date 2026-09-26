package peer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// protocolName is the fixed identifier in the handshake.
const protocolName = "BitTorrent protocol"

// HandshakeLen is the fixed size of a handshake: 1 + 19 + 8 + 20 + 20.
const HandshakeLen = 68

// ErrInfoHashMismatch is returned when a peer answers for a different
// torrent.
var ErrInfoHashMismatch = errors.New("peer: info hash mismatch")

// Handshake is the opening message of a peer connection.
type Handshake struct {
	Pstr     string
	InfoHash [20]byte
	PeerID   [20]byte
	// Reserved carries extension flags.
	Reserved [8]byte
}

// NewHandshake returns a handshake for a torrent.
func NewHandshake(infoHash, peerID [20]byte) *Handshake {
	return &Handshake{Pstr: protocolName, InfoHash: infoHash, PeerID: peerID}
}

// Serialize encodes the handshake.
func (h *Handshake) Serialize() []byte {
	buf := make([]byte, len(h.Pstr)+49)
	buf[0] = byte(len(h.Pstr))
	n := 1
	n += copy(buf[n:], h.Pstr)
	n += copy(buf[n:], h.Reserved[:])
	n += copy(buf[n:], h.InfoHash[:])
	copy(buf[n:], h.PeerID[:])
	return buf
}

// ReadHandshake reads and validates a handshake.
func ReadHandshake(r io.Reader) (*Handshake, error) {
	var lengthBuf [1]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return nil, err
	}
	pstrLen := int(lengthBuf[0])
	if pstrLen == 0 {
		return nil, errors.New("peer: handshake protocol string is empty")
	}

	buf := make([]byte, pstrLen+48)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}

	h := &Handshake{Pstr: string(buf[:pstrLen])}
	copy(h.Reserved[:], buf[pstrLen:pstrLen+8])
	copy(h.InfoHash[:], buf[pstrLen+8:pstrLen+28])
	copy(h.PeerID[:], buf[pstrLen+28:])
	return h, nil
}

// Verify checks that the handshake is for the expected torrent and speaks
// the expected protocol.
func (h *Handshake) Verify(infoHash [20]byte) error {
	if h.Pstr != protocolName {
		return fmt.Errorf("peer: unexpected protocol %q", h.Pstr)
	}
	if !bytes.Equal(h.InfoHash[:], infoHash[:]) {
		return ErrInfoHashMismatch
	}
	return nil
}
