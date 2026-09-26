package peer

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestMessageSerializeRoundTrip(t *testing.T) {
	tests := []*Message{
		{ID: MsgChoke},
		{ID: MsgUnchoke},
		{ID: MsgInterested},
		{ID: MsgHave, Payload: []byte{0, 0, 0, 5}},
		{ID: MsgBitfield, Payload: []byte{0xff, 0x00, 0xaa}},
		{ID: MsgPiece, Payload: bytes.Repeat([]byte{1}, 100)},
	}
	for _, want := range tests {
		t.Run(want.ID.String(), func(t *testing.T) {
			got, err := Read(bytes.NewReader(want.Serialize()))
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != want.ID {
				t.Errorf("ID = %v, want %v", got.ID, want.ID)
			}
			if !bytes.Equal(got.Payload, want.Payload) {
				t.Errorf("payload = %v, want %v", got.Payload, want.Payload)
			}
		})
	}
}

// A zero-length message is a keep-alive, not an error or EOF.
func TestKeepAlive(t *testing.T) {
	var nilMsg *Message
	encoded := nilMsg.Serialize()
	if len(encoded) != 4 {
		t.Fatalf("keep-alive is %d bytes, want 4", len(encoded))
	}
	got, err := Read(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("keep-alive should not be an error: %v", err)
	}
	if got != nil {
		t.Errorf("keep-alive should decode to nil, got %v", got)
	}
}

// A peer is untrusted and can claim any length. Without a cap, one claimed
// length forces an arbitrary allocation.
func TestReadRejectsOversizedMessage(t *testing.T) {
	// Claim 4 GiB.
	buf := []byte{0xff, 0xff, 0xff, 0xff}
	_, err := Read(bytes.NewReader(buf))
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestReadTruncated(t *testing.T) {
	// Declares 100 bytes but supplies 10.
	buf := append([]byte{0, 0, 0, 100}, bytes.Repeat([]byte{1}, 10)...)
	if _, err := Read(bytes.NewReader(buf)); err == nil {
		t.Error("expected an error on truncated input")
	}
	// Empty input is EOF.
	if _, err := Read(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want EOF", err)
	}
}

func TestFormatAndParseRequest(t *testing.T) {
	msg := FormatRequest(1, 2, 3)
	index, begin, length, err := ParseRequest(msg)
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 || begin != 2 || length != 3 {
		t.Errorf("= (%d, %d, %d), want (1, 2, 3)", index, begin, length)
	}
}

func TestFormatAndParseHave(t *testing.T) {
	got, err := ParseHave(FormatHave(42))
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("= %d, want 42", got)
	}
}

func TestParseHaveErrors(t *testing.T) {
	if _, err := ParseHave(nil); err == nil {
		t.Error("nil should fail")
	}
	if _, err := ParseHave(&Message{ID: MsgChoke}); err == nil {
		t.Error("wrong ID should fail")
	}
	if _, err := ParseHave(&Message{ID: MsgHave, Payload: []byte{1}}); err == nil {
		t.Error("short payload should fail")
	}
}

func TestParsePiece(t *testing.T) {
	buf := make([]byte, 100)
	data := []byte("hello")
	payload := append([]byte{0, 0, 0, 1, 0, 0, 0, 10}, data...)

	n, err := ParsePiece(1, buf, &Message{ID: MsgPiece, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Errorf("wrote %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(buf[10:15], data) {
		t.Errorf("data landed at the wrong offset: %v", buf[10:15])
	}
}

// A peer can send any index, offset or length. Copying without checking is
// a buffer overflow, so every field must be validated.
func TestParsePieceRejectsMaliciousInput(t *testing.T) {
	buf := make([]byte, 100)

	tests := []struct {
		name    string
		index   int
		payload []byte
	}{
		{
			"wrong piece index",
			1,
			append([]byte{0, 0, 0, 99, 0, 0, 0, 0}, []byte("data")...),
		},
		{
			"offset beyond the buffer",
			1,
			append([]byte{0, 0, 0, 1, 0, 0, 1, 0}, []byte("data")...), // offset 256
		},
		{
			"block overflows the buffer",
			1,
			append([]byte{0, 0, 0, 1, 0, 0, 0, 98}, bytes.Repeat([]byte{1}, 50)...),
		},
		{
			"payload too short for a header",
			1,
			[]byte{0, 0, 0},
		},
		{
			"huge offset",
			1,
			append([]byte{0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff}, []byte("x")...),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePiece(tt.index, buf, &Message{ID: MsgPiece, Payload: tt.payload})
			if err == nil {
				t.Error("malicious payload was accepted")
			}
		})
	}

	// The buffer must be untouched by every rejected message.
	if !bytes.Equal(buf, make([]byte, 100)) {
		t.Error("a rejected piece message still wrote into the buffer")
	}
}

func TestBitfield(t *testing.T) {
	// 0b10101010, 0b01010101
	bf := Bitfield{0xAA, 0x55}
	want := []bool{
		true, false, true, false, true, false, true, false,
		false, true, false, true, false, true, false, true,
	}
	for i, w := range want {
		if got := bf.HasPiece(i); got != w {
			t.Errorf("HasPiece(%d) = %v, want %v", i, got, w)
		}
	}
	// Out of range is false, not a panic.
	if bf.HasPiece(100) || bf.HasPiece(-1) {
		t.Error("out-of-range index should be false")
	}
	if got := bf.Count(); got != 8 {
		t.Errorf("Count = %d, want 8", got)
	}
}

func TestBitfieldSetPiece(t *testing.T) {
	bf := NewBitfield(16)
	if bf.Count() != 0 {
		t.Errorf("new bitfield has %d pieces set", bf.Count())
	}
	for _, i := range []int{0, 7, 8, 15} {
		bf.SetPiece(i)
		if !bf.HasPiece(i) {
			t.Errorf("SetPiece(%d) did not take effect", i)
		}
	}
	if got := bf.Count(); got != 4 {
		t.Errorf("Count = %d, want 4", got)
	}
	// Out of range is ignored rather than growing or panicking.
	bf.SetPiece(1000)
	bf.SetPiece(-1)
	if got := bf.Count(); got != 4 {
		t.Errorf("out-of-range SetPiece changed the count to %d", got)
	}
}

func TestNewBitfieldSize(t *testing.T) {
	tests := map[int]int{0: 0, 1: 1, 8: 1, 9: 2, 16: 2, 17: 3}
	for pieces, wantBytes := range tests {
		if got := len(NewBitfield(pieces)); got != wantBytes {
			t.Errorf("NewBitfield(%d) is %d bytes, want %d", pieces, got, wantBytes)
		}
	}
}

func TestHandshakeRoundTrip(t *testing.T) {
	var infoHash, peerID [20]byte
	copy(infoHash[:], "12345678901234567890")
	copy(peerID[:], "abcdefghijklmnopqrst")

	h := NewHandshake(infoHash, peerID)
	encoded := h.Serialize()
	if len(encoded) != HandshakeLen {
		t.Fatalf("handshake is %d bytes, want %d", len(encoded), HandshakeLen)
	}

	got, err := ReadHandshake(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if got.Pstr != protocolName {
		t.Errorf("pstr = %q", got.Pstr)
	}
	if got.InfoHash != infoHash {
		t.Errorf("info hash = %x", got.InfoHash)
	}
	if got.PeerID != peerID {
		t.Errorf("peer id = %x", got.PeerID)
	}
}

// A peer answering for a different torrent must be rejected.
func TestHandshakeVerifyRejectsWrongInfoHash(t *testing.T) {
	var ours, theirs [20]byte
	copy(ours[:], "aaaaaaaaaaaaaaaaaaaa")
	copy(theirs[:], "bbbbbbbbbbbbbbbbbbbb")

	h := NewHandshake(theirs, [20]byte{})
	if err := h.Verify(ours); !errors.Is(err, ErrInfoHashMismatch) {
		t.Errorf("err = %v, want ErrInfoHashMismatch", err)
	}
	if err := h.Verify(theirs); err != nil {
		t.Errorf("matching hash should verify: %v", err)
	}
}

func TestHandshakeVerifyRejectsWrongProtocol(t *testing.T) {
	h := &Handshake{Pstr: "Something Else"}
	if err := h.Verify([20]byte{}); err == nil {
		t.Error("wrong protocol string should be rejected")
	}
}

func TestReadHandshakeErrors(t *testing.T) {
	// Zero-length protocol string.
	if _, err := ReadHandshake(bytes.NewReader([]byte{0})); err == nil {
		t.Error("zero pstr length should fail")
	}
	// Truncated.
	if _, err := ReadHandshake(bytes.NewReader([]byte{19, 'B'})); err == nil {
		t.Error("truncated handshake should fail")
	}
	// Empty.
	if _, err := ReadHandshake(bytes.NewReader(nil)); err == nil {
		t.Error("empty input should fail")
	}
}

func TestConnHandleState(t *testing.T) {
	c := &Conn{Choked: true, Bitfield: NewBitfield(16)}

	if !c.HandleState(nil) {
		t.Error("keep-alive should be handled")
	}
	if !c.HandleState(&Message{ID: MsgUnchoke}) || c.Choked {
		t.Error("unchoke was not applied")
	}
	if !c.HandleState(&Message{ID: MsgChoke}) || !c.Choked {
		t.Error("choke was not applied")
	}
	if !c.HandleState(FormatHave(5)) || !c.Bitfield.HasPiece(5) {
		t.Error("have was not applied")
	}
	// A piece message is not state and must be left to the caller.
	if c.HandleState(&Message{ID: MsgPiece}) {
		t.Error("piece should not be handled as state")
	}
}

func TestMessageIDString(t *testing.T) {
	if got := MsgChoke.String(); got != "choke" {
		t.Errorf("= %q", got)
	}
	if got := MessageID(99).String(); got != "unknown(99)" {
		t.Errorf("= %q", got)
	}
}
