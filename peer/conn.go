package peer

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Peer is a peer address from a tracker.
type Peer struct {
	IP   net.IP
	Port uint16
}

func (p Peer) String() string {
	return net.JoinHostPort(p.IP.String(), fmt.Sprintf("%d", p.Port))
}

// Conn is an established peer connection with its protocol state.
type Conn struct {
	conn     net.Conn
	Peer     Peer
	Bitfield Bitfield
	PeerID   [20]byte
	infoHash [20]byte

	// Choked records whether the remote peer is choking us. A choked peer
	// will ignore requests, so there is no point sending any.
	Choked bool
}

// Dial connects to a peer and completes the handshake.
func Dial(ctx context.Context, p Peer, infoHash, peerID [20]byte, numPieces int, timeout time.Duration) (*Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", p.String())
	if err != nil {
		return nil, err
	}

	// The handshake must not hang: an unresponsive peer would otherwise
	// hold the slot indefinitely.
	conn.SetDeadline(time.Now().Add(timeout))

	if _, err := conn.Write(NewHandshake(infoHash, peerID).Serialize()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("peer: sending handshake: %w", err)
	}
	h, err := ReadHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("peer: reading handshake: %w", err)
	}
	if err := h.Verify(infoHash); err != nil {
		conn.Close()
		return nil, err
	}

	c := &Conn{
		conn:     conn,
		Peer:     p,
		PeerID:   h.PeerID,
		infoHash: infoHash,
		Choked:   true, // connections start choked until told otherwise
		Bitfield: NewBitfield(numPieces),
	}

	// A bitfield, if sent at all, must be the first message. Anything else
	// means the peer has no pieces yet, which is not an error.
	conn.SetDeadline(time.Now().Add(timeout))
	msg, err := Read(conn)
	if err == nil && msg != nil && msg.ID == MsgBitfield {
		// Accept only a correctly sized bitfield; a wrong size means the
		// peer is confused about which torrent this is.
		if len(msg.Payload) == len(c.Bitfield) {
			copy(c.Bitfield, msg.Payload)
		}
	}

	conn.SetDeadline(time.Time{})
	return c, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Read reads the next message.
func (c *Conn) Read() (*Message, error) { return Read(c.conn) }

// Send writes a message.
func (c *Conn) Send(m *Message) error {
	_, err := c.conn.Write(m.Serialize())
	return err
}

// SendUnchoke tells the peer it may request from us.
func (c *Conn) SendUnchoke() error { return c.Send(&Message{ID: MsgUnchoke}) }

// SendInterested tells the peer we want its pieces.
func (c *Conn) SendInterested() error { return c.Send(&Message{ID: MsgInterested}) }

// SendNotInterested tells the peer we do not.
func (c *Conn) SendNotInterested() error { return c.Send(&Message{ID: MsgNotInterested}) }

// SendRequest asks for a block.
func (c *Conn) SendRequest(index, begin, length int) error {
	return c.Send(FormatRequest(index, begin, length))
}

// SendHave announces that we now have a piece.
func (c *Conn) SendHave(index int) error { return c.Send(FormatHave(index)) }

// SetDeadline bounds how long a read or write may block.
func (c *Conn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// HandleState applies a message that only changes connection state,
// reporting whether it did so. Keeping this in one place means the download
// loop does not have to re-implement choke and have handling.
func (c *Conn) HandleState(msg *Message) bool {
	if msg == nil {
		return true // keep-alive
	}
	switch msg.ID {
	case MsgChoke:
		c.Choked = true
		return true
	case MsgUnchoke:
		c.Choked = false
		return true
	case MsgHave:
		if index, err := ParseHave(msg); err == nil {
			c.Bitfield.SetPiece(index)
		}
		return true
	}
	return false
}
