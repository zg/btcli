// Package bencode implements the encoding used by BitTorrent metainfo files
// and tracker responses.
//
// Bencode has four types: integers (i42e), byte strings (5:hello), lists
// (l...e) and dictionaries (d...e). Dictionary keys are byte strings and
// must be sorted, which matters because the info hash is computed over the
// re-encoded info dictionary — an encoder that does not sort produces a
// hash no tracker or peer will recognise.
package bencode

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Errors returned while decoding.
var (
	ErrSyntax     = errors.New("bencode: syntax error")
	ErrDepthLimit = errors.New("bencode: nesting too deep")
)

// maxDepth bounds recursion. A hostile torrent file of deeply nested lists
// would otherwise exhaust the stack.
const maxDepth = 100

// Decode reads one bencoded value from r.
//
// The result is one of: int64, string, []any, or map[string]any.
func Decode(r io.Reader) (any, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return decode(br, 0)
}

// Unmarshal decodes a byte slice.
func Unmarshal(data []byte) (any, error) {
	return Decode(bytes.NewReader(data))
}

func decode(r *bufio.Reader, depth int) (any, error) {
	if depth > maxDepth {
		return nil, ErrDepthLimit
	}

	b, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	switch {
	case b == 'i':
		return decodeInt(r)
	case b == 'l':
		return decodeList(r, depth)
	case b == 'd':
		return decodeDict(r, depth)
	case b >= '0' && b <= '9':
		if err := r.UnreadByte(); err != nil {
			return nil, err
		}
		return decodeString(r)
	default:
		return nil, fmt.Errorf("%w: unexpected byte %q", ErrSyntax, string(b))
	}
}

func decodeInt(r *bufio.Reader) (int64, error) {
	s, err := readUntil(r, 'e')
	if err != nil {
		return 0, err
	}
	// Reject the representations the spec forbids: leading zeros and
	// negative zero. Accepting them would make encoding non-canonical, and
	// the info hash depends on canonical encoding.
	if s == "" || s == "-" {
		return 0, fmt.Errorf("%w: empty integer", ErrSyntax)
	}
	if s == "-0" {
		return 0, fmt.Errorf("%w: negative zero", ErrSyntax)
	}
	if len(s) > 1 && (s[0] == '0' || (s[0] == '-' && s[1] == '0')) {
		return 0, fmt.Errorf("%w: leading zero in %q", ErrSyntax, s)
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not an integer", ErrSyntax, s)
	}
	return n, nil
}

func decodeString(r *bufio.Reader) (string, error) {
	lengthStr, err := readUntil(r, ':')
	if err != nil {
		return "", err
	}
	length, err := strconv.ParseInt(lengthStr, 10, 64)
	if err != nil || length < 0 {
		return "", fmt.Errorf("%w: bad string length %q", ErrSyntax, lengthStr)
	}
	// A hostile file could claim a huge length; cap it so decoding cannot
	// be turned into an allocation bomb.
	const maxString = 1 << 28 // 256 MiB
	if length > maxString {
		return "", fmt.Errorf("%w: string length %d exceeds the limit", ErrSyntax, length)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func decodeList(r *bufio.Reader, depth int) ([]any, error) {
	list := []any{}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 'e' {
			return list, nil
		}
		if err := r.UnreadByte(); err != nil {
			return nil, err
		}
		item, err := decode(r, depth+1)
		if err != nil {
			return nil, err
		}
		list = append(list, item)
	}
}

func decodeDict(r *bufio.Reader, depth int) (map[string]any, error) {
	dict := map[string]any{}
	prevKey := ""
	first := true

	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 'e' {
			return dict, nil
		}
		if err := r.UnreadByte(); err != nil {
			return nil, err
		}

		key, err := decodeString(r)
		if err != nil {
			return nil, fmt.Errorf("%w: dictionary key: %v", ErrSyntax, err)
		}
		// Keys must be sorted. Enforcing it on decode catches a malformed
		// file early rather than letting it produce a wrong info hash.
		if !first && key <= prevKey {
			return nil, fmt.Errorf("%w: dictionary keys out of order: %q after %q", ErrSyntax, key, prevKey)
		}
		first, prevKey = false, key

		value, err := decode(r, depth+1)
		if err != nil {
			return nil, err
		}
		dict[key] = value
	}
}

// readUntil reads bytes up to but not including delim.
func readUntil(r *bufio.Reader, delim byte) (string, error) {
	s, err := r.ReadString(delim)
	if err != nil {
		return "", err
	}
	return s[:len(s)-1], nil
}

// Encode writes v to w in bencode form.
func Encode(w io.Writer, v any) error {
	switch val := v.(type) {
	case int:
		_, err := fmt.Fprintf(w, "i%de", val)
		return err
	case int64:
		_, err := fmt.Fprintf(w, "i%de", val)
		return err
	case string:
		_, err := fmt.Fprintf(w, "%d:%s", len(val), val)
		return err
	case []byte:
		if _, err := fmt.Fprintf(w, "%d:", len(val)); err != nil {
			return err
		}
		_, err := w.Write(val)
		return err
	case []any:
		if _, err := w.Write([]byte{'l'}); err != nil {
			return err
		}
		for _, item := range val {
			if err := Encode(w, item); err != nil {
				return err
			}
		}
		_, err := w.Write([]byte{'e'})
		return err
	case map[string]any:
		if _, err := w.Write([]byte{'d'}); err != nil {
			return err
		}
		// Sorting is required, not cosmetic: the info hash is the SHA-1 of
		// the re-encoded info dictionary, so a different key order gives a
		// different hash and the torrent becomes unrecognisable.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			if err := Encode(w, k); err != nil {
				return err
			}
			if err := Encode(w, val[k]); err != nil {
				return err
			}
		}
		_, err := w.Write([]byte{'e'})
		return err
	default:
		return fmt.Errorf("bencode: cannot encode %T", v)
	}
}

// Marshal encodes v to a byte slice.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := Encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
