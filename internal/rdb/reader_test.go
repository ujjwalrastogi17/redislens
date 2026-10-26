package rdb

import (
	"bytes"
	"testing"
)

// Table-driven test: the idiomatic Go way to test many inputs.
func TestReadLength(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		want    uint64
		encoded bool
	}{
		{"6-bit", []byte{0x0A}, 10, false},
		{"14-bit", []byte{0x41, 0x00}, 256, false},
		{"32-bit", []byte{0x80, 0x00, 0x01, 0x00, 0x00}, 65536, false},
		{"64-bit", []byte{0x81, 0, 0, 0, 1, 0, 0, 0, 0}, 1 << 32, false},
		{"special", []byte{0xC2}, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, enc, err := NewReader(bytes.NewReader(tt.in)).ReadLength()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || enc != tt.encoded {
				t.Errorf("got (%d,%v), want (%d,%v)", got, enc, tt.want, tt.encoded)
			}
		})
	}
}

func TestReadString(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"raw", []byte{0x03, 'f', 'o', 'o'}, "foo"},
		{"int8", []byte{0xC0, 0xFB}, "-5"},
		{"int16", []byte{0xC1, 0x39, 0x30}, "12345"},
		{"int32", []byte{0xC2, 0x40, 0xE2, 0x01, 0x00}, "123456"},
		// LZF: literal "ab" (ctrl=1), then back-ref len 4 (ctrl=(2<<5)|0, off=1) -> "ababab"
		{"lzf", []byte{0xC3, 0x05, 0x06, 0x01, 'a', 'b', 0x40, 0x01}, "ababab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewReader(bytes.NewReader(tt.in)).ReadString()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadHeaderRejectsGarbage(t *testing.T) {
	_, err := NewReader(bytes.NewReader([]byte("NOTREDIS1"))).ReadHeader()
	if err != ErrNotRDB {
		t.Fatalf("got %v, want ErrNotRDB", err)
	}
}
