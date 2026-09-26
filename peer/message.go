// Package peer implements the BitTorrent peer wire protocol.
package peer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MessageID identifies a peer wire message type.
type MessageID uint8

const (
	MsgChoke MessageID = iota
	MsgUnchoke
	MsgInterested
	MsgNotInterested
	MsgHave
	MsgBitfield
	MsgRequest
	MsgPiece
	MsgCancel
	MsgPort
)

func (m MessageID) String() string {
	names := [...]string{
		"choke", "unchoke", "interested", "not interested",
		"have", "bitfield", "request", "piece", "cancel", "port",
	}
	if int(m) < len(names) {
		return names[m]
	}
	return fmt.Sprintf("unknown(%d)", uint8(m))
}

// MaxMessageSize bounds an accepted message. A peer is untrusted and can
// claim any length; without a cap, a single claimed length would let it
// force an arbitrary allocation.
const MaxMessageSize = 1 << 20 // 1 MiB, comfortably above a 16 KiB block

// ErrMessageTooLarge is returned when a peer claims an oversized message.
var ErrMessageTooLarge = errors.New("peer: message exceeds the size limit")

// Message is a peer wire message. A nil Message is a keep-alive, which has
// no ID and a zero length.
type Message struct {
	ID      MessageID
	Payload []byte
}

// Serialize encodes the message with its four-byte big-endian length
// prefix. A nil message encodes as a keep-alive.
func (m *Message) Serialize() []byte {
	if m == nil {
		return make([]byte, 4) // length 0
	}
	length := uint32(len(m.Payload) + 1) // +1 for the ID
	buf := make([]byte, 4+length)
	binary.BigEndian.PutUint32(buf[0:4], length)
	buf[4] = byte(m.ID)
	copy(buf[5:], m.Payload)
	return buf
}

// Read reads one message. It returns (nil, nil) for a keep-alive, which is
// a valid message rather than an error or EOF.
func Read(r io.Reader) (*Message, error) {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lengthBuf[:])

	if length == 0 {
		return nil, nil // keep-alive
	}
	if length > MaxMessageSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, length)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return &Message{ID: MessageID(buf[0]), Payload: buf[1:]}, nil
}

// FormatRequest builds a request for a block within a piece.
func FormatRequest(index, begin, length int) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))
	return &Message{ID: MsgRequest, Payload: payload}
}

// FormatHave builds a have message.
func FormatHave(index int) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(index))
	return &Message{ID: MsgHave, Payload: payload}
}

// ParseHave reads the piece index from a have message.
func ParseHave(msg *Message) (int, error) {
	if msg == nil || msg.ID != MsgHave {
		return 0, fmt.Errorf("peer: expected have, got %v", msg)
	}
	if len(msg.Payload) != 4 {
		return 0, fmt.Errorf("peer: have payload is %d bytes, want 4", len(msg.Payload))
	}
	return int(binary.BigEndian.Uint32(msg.Payload)), nil
}

// ParseRequest reads index, begin and length from a request message.
func ParseRequest(msg *Message) (index, begin, length int, err error) {
	if msg == nil || msg.ID != MsgRequest {
		return 0, 0, 0, fmt.Errorf("peer: expected request, got %v", msg)
	}
	if len(msg.Payload) != 12 {
		return 0, 0, 0, fmt.Errorf("peer: request payload is %d bytes, want 12", len(msg.Payload))
	}
	index = int(binary.BigEndian.Uint32(msg.Payload[0:4]))
	begin = int(binary.BigEndian.Uint32(msg.Payload[4:8]))
	length = int(binary.BigEndian.Uint32(msg.Payload[8:12]))
	return index, begin, length, nil
}

// ParsePiece copies a received block into buf, returning how many bytes
// were written.
//
// Every field is validated against what was actually requested. A peer can
// send any index, offset or length it likes, and an unchecked copy into a
// fixed buffer is a straightforward buffer overflow.
func ParsePiece(index int, buf []byte, msg *Message) (int, error) {
	if msg == nil || msg.ID != MsgPiece {
		return 0, fmt.Errorf("peer: expected piece, got %v", msg)
	}
	if len(msg.Payload) < 8 {
		return 0, fmt.Errorf("peer: piece payload is %d bytes, want at least 8", len(msg.Payload))
	}

	gotIndex := int(binary.BigEndian.Uint32(msg.Payload[0:4]))
	if gotIndex != index {
		return 0, fmt.Errorf("peer: got piece %d, expected %d", gotIndex, index)
	}

	begin := int(binary.BigEndian.Uint32(msg.Payload[4:8]))
	if begin < 0 || begin >= len(buf) {
		return 0, fmt.Errorf("peer: offset %d is outside the piece (%d bytes)", begin, len(buf))
	}

	data := msg.Payload[8:]
	if begin+len(data) > len(buf) {
		return 0, fmt.Errorf("peer: block of %d bytes at offset %d overflows the piece (%d bytes)",
			len(data), begin, len(buf))
	}
	copy(buf[begin:], data)
	return len(data), nil
}

// Bitfield records which pieces a peer has.
type Bitfield []byte

// HasPiece reports whether the peer has the given piece.
func (b Bitfield) HasPiece(index int) bool {
	if index < 0 {
		return false
	}
	byteIndex := index / 8
	offset := index % 8
	if byteIndex >= len(b) {
		return false
	}
	// Bit 0 is the most significant bit of the first byte.
	return b[byteIndex]>>(7-offset)&1 != 0
}

// SetPiece marks a piece as present, ignoring an out-of-range index rather
// than growing the bitfield — its size is fixed by the torrent.
func (b Bitfield) SetPiece(index int) {
	if index < 0 {
		return
	}
	byteIndex := index / 8
	offset := index % 8
	if byteIndex >= len(b) {
		return
	}
	b[byteIndex] |= 1 << (7 - offset)
}

// Count returns how many pieces the bitfield marks as present.
func (b Bitfield) Count() int {
	n := 0
	for _, by := range b {
		for i := range 8 {
			if by>>(7-i)&1 != 0 {
				n++
			}
		}
	}
	return n
}

// NewBitfield returns a bitfield sized for numPieces.
func NewBitfield(numPieces int) Bitfield {
	return make(Bitfield, (numPieces+7)/8)
}
