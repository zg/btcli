package bencode

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want any
	}{
		{"integer", "i42e", int64(42)},
		{"negative integer", "i-42e", int64(-42)},
		{"zero", "i0e", int64(0)},
		{"string", "5:hello", "hello"},
		{"empty string", "0:", ""},
		{"string with colon", "5:a:b:c", "a:b:c"},
		{"empty list", "le", []any{}},
		{"list", "li1ei2ei3ee", []any{int64(1), int64(2), int64(3)}},
		{"mixed list", "li42e5:helloe", []any{int64(42), "hello"}},
		{"nested list", "lli1eeli2eee", []any{[]any{int64(1)}, []any{int64(2)}}},
		{"empty dict", "de", map[string]any{}},
		{"dict", "d3:bar4:spam3:fooi42ee", map[string]any{"bar": "spam", "foo": int64(42)}},
		{"nested dict", "d1:ad1:bi1eee", map[string]any{"a": map[string]any{"b": int64(1)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Unmarshal([]byte(tt.in))
			if err != nil {
				t.Fatalf("decode %q: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("= %#v, want %#v", got, tt.want)
			}
		})
	}
}

// The spec forbids these forms; accepting them would make encoding
// non-canonical, and the info hash depends on canonical encoding.
func TestDecodeRejectsNonCanonicalIntegers(t *testing.T) {
	for _, in := range []string{"i03e", "i-0e", "i00e", "i-03e", "ie", "i-e"} {
		t.Run(in, func(t *testing.T) {
			if _, err := Unmarshal([]byte(in)); err == nil {
				t.Error("expected a syntax error")
			}
		})
	}
}

// Dictionary keys must be sorted. An unsorted file would produce a
// different info hash than the one peers expect.
func TestDecodeRejectsUnsortedKeys(t *testing.T) {
	for _, in := range []string{
		"d3:foo1:a3:bar1:be", // foo before bar
		"d1:a1:x1:a1:ye",     // duplicate key
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := Unmarshal([]byte(in)); err == nil {
				t.Error("expected an out-of-order key error")
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := []string{
		"",
		"x",
		"i42",      // unterminated
		"5:abc",    // string shorter than declared
		"l",        // unterminated list
		"d",        // unterminated dict
		"d1:ae",    // key with no value
		"-1:abc",   // negative length
		"di1ei2ee", // non-string key
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if _, err := Unmarshal([]byte(in)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// A hostile file must not be able to exhaust the stack.
func TestDecodeDepthLimit(t *testing.T) {
	deep := strings.Repeat("l", 1000) + strings.Repeat("e", 1000)
	_, err := Unmarshal([]byte(deep))
	if !errors.Is(err, ErrDepthLimit) {
		t.Errorf("err = %v, want ErrDepthLimit", err)
	}
}

// Nor to force an enormous allocation by claiming a huge string length.
func TestDecodeRejectsHugeString(t *testing.T) {
	if _, err := Unmarshal([]byte("999999999999:x")); err == nil {
		t.Error("expected a length limit error")
	}
}

func TestEncode(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"integer", int64(42), "i42e"},
		{"negative", int64(-42), "i-42e"},
		{"int", 42, "i42e"},
		{"string", "hello", "5:hello"},
		{"bytes", []byte("hello"), "5:hello"},
		{"empty string", "", "0:"},
		{"list", []any{int64(1), "two"}, "li1e3:twoe"},
		{"empty list", []any{}, "le"},
		{"dict", map[string]any{"foo": int64(42), "bar": "spam"}, "d3:bar4:spam3:fooi42ee"},
		{"empty dict", map[string]any{}, "de"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
		})
	}
}

// Key order must be canonical regardless of Go's map iteration order,
// because the info hash is computed over the re-encoded dictionary.
func TestEncodeSortsKeysDeterministically(t *testing.T) {
	dict := map[string]any{
		"zebra": int64(1), "apple": int64(2), "mango": int64(3),
		"banana": int64(4), "cherry": int64(5),
	}
	want := "d5:apple i2e"
	_ = want

	first, err := Marshal(dict)
	if err != nil {
		t.Fatal(err)
	}
	// Repeat: Go randomises map iteration, so an unsorted encoder would
	// produce different bytes between runs.
	for range 50 {
		again, err := Marshal(dict)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("encoding is not deterministic:\n %q\n %q", first, again)
		}
	}
	if !strings.HasPrefix(string(first), "d5:apple") {
		t.Errorf("keys are not sorted: %q", first)
	}
}

func TestEncodeUnsupportedType(t *testing.T) {
	if _, err := Marshal(3.14); err == nil {
		t.Error("expected an error for an unsupported type")
	}
}

// Every value must survive encode/decode unchanged.
func TestRoundTrip(t *testing.T) {
	values := []any{
		int64(0), int64(42), int64(-1),
		"", "hello", "with spaces and :colons",
		[]any{},
		[]any{int64(1), "two", []any{int64(3)}},
		map[string]any{},
		map[string]any{"a": int64(1), "b": "two", "c": []any{int64(3)}},
	}
	for _, v := range values {
		encoded, err := Marshal(v)
		if err != nil {
			t.Fatalf("encode %#v: %v", v, err)
		}
		decoded, err := Unmarshal(encoded)
		if err != nil {
			t.Fatalf("decode %q: %v", encoded, err)
		}
		if !reflect.DeepEqual(decoded, v) {
			t.Errorf("round trip changed %#v into %#v", v, decoded)
		}
	}
}

// Binary data, including NUL bytes, must survive — piece hashes are raw
// SHA-1 values and routinely contain them.
func TestRoundTripBinaryData(t *testing.T) {
	binary := string([]byte{0, 1, 2, 255, 254, 0, 128})
	encoded, err := Marshal(binary)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Unmarshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.(string) != binary {
		t.Errorf("binary data was corrupted: %q -> %q", binary, decoded)
	}
}
