// Package rdb is a streaming parser for Redis/Valkey RDB snapshot files.
package rdb

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Opcodes that can appear where a value type byte is expected.
const (
	OpSlotInfo     = 0xF4 // Valkey/Redis cluster slot info
	OpFunction2    = 0xF5 // Redis 7 functions
	OpModuleAux    = 0xF7
	OpIdle         = 0xF8 // LRU idle time
	OpFreq         = 0xF9 // LFU frequency
	OpAux          = 0xFA // metadata key/value
	OpResizeDB     = 0xFB
	OpExpireTimeMS = 0xFC
	OpExpireTime   = 0xFD
	OpSelectDB     = 0xFE
	OpEOF          = 0xFF
)

// ErrNotRDB is returned when the file doesn't start with "REDIS".
var ErrNotRDB = errors.New("not an RDB file (missing REDIS magic)")

// Reader reads RDB primitives from a buffered stream.
type Reader struct {
	br  *bufio.Reader
	cr  *countingReader
	acc memAcc  // size facts gathered while skipping the current value
	buf [8]byte // scratch space so small reads don't allocate
	key []byte  // reused for every key; see Entry.Key
}

// countingReader counts bytes pulled from the underlying reader.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// NewReader wraps any io.Reader (file, stdin, network stream).
func NewReader(r io.Reader) *Reader {
	cr := &countingReader{r: r}
	return &Reader{br: bufio.NewReaderSize(cr, 64*1024), cr: cr}
}

// Offset is the number of bytes consumed so far (read minus still-buffered).
func (r *Reader) Offset() int64 {
	return r.cr.n - int64(r.br.Buffered())
}

// Skip discards n bytes without allocating.
func (r *Reader) Skip(n uint64) error {
	for n > 0 {
		chunk := n
		if chunk > 1<<30 {
			chunk = 1 << 30
		}
		d, err := r.br.Discard(int(chunk))
		n -= uint64(d)
		if err != nil {
			return err
		}
	}
	return nil
}

// SkipString skips a string of any encoding without allocating or decompressing.
func (r *Reader) SkipString() error {
	_, _, err := r.skipStr()
	return err
}

// skipStr skips a string and reports its logical (uncompressed) length.
// isInt is true for integer-encoded strings, which Redis keeps as a number in RAM.
func (r *Reader) skipStr() (length uint64, isInt bool, err error) {
	n, enc, err := r.ReadLength()
	if err != nil {
		return 0, false, err
	}
	if !enc {
		return n, false, r.Skip(n)
	}
	switch n {
	case encInt8, encInt16, encInt32:
		return 8, true, r.Skip(1 << n)
	case encLZF:
		clen, err := r.ReadPlainLength()
		if err != nil {
			return 0, false, err
		}
		ulen, err := r.ReadPlainLength()
		if err != nil {
			return 0, false, err
		}
		return ulen, false, r.Skip(clen)
	}
	return 0, false, fmt.Errorf("unknown string encoding %d", n)
}

// ReadHeader reads "REDIS" + 4 ASCII digits and returns the version.
func (r *Reader) ReadHeader() (int, error) {
	var hdr [9]byte
	if _, err := io.ReadFull(r.br, hdr[:]); err != nil {
		return 0, fmt.Errorf("reading header: %w", err)
	}
	if string(hdr[:5]) != "REDIS" {
		return 0, ErrNotRDB
	}
	v, err := strconv.Atoi(string(hdr[5:]))
	if err != nil {
		return 0, fmt.Errorf("bad version %q: %w", hdr[5:], err)
	}
	return v, nil
}

// ReadByte reads one byte (opcodes, value types).
func (r *Reader) ReadByte() (byte, error) {
	return r.br.ReadByte()
}

// ReadLength reads a length-encoded integer.
// The top 2 bits of the first byte select the format:
//
//	00xxxxxx            -> 6-bit length
//	01xxxxxx xxxxxxxx   -> 14-bit length
//	10000000 + 4 bytes  -> 32-bit big-endian
//	10000001 + 8 bytes  -> 64-bit big-endian
//	11xxxxxx            -> special encoding; returns encoded=true, value = low 6 bits
func (r *Reader) ReadLength() (length uint64, encoded bool, err error) {
	first, err := r.br.ReadByte()
	if err != nil {
		return 0, false, err
	}
	switch first >> 6 {
	case 0:
		return uint64(first & 0x3F), false, nil
	case 1:
		next, err := r.br.ReadByte()
		if err != nil {
			return 0, false, err
		}
		return uint64(first&0x3F)<<8 | uint64(next), false, nil
	case 2:
		switch first {
		case 0x80:
			if _, err := io.ReadFull(r.br, r.buf[:4]); err != nil {
				return 0, false, err
			}
			return uint64(binary.BigEndian.Uint32(r.buf[:4])), false, nil
		case 0x81:
			if _, err := io.ReadFull(r.br, r.buf[:8]); err != nil {
				return 0, false, err
			}
			return binary.BigEndian.Uint64(r.buf[:8]), false, nil
		}
		return 0, false, fmt.Errorf("unknown length prefix 0x%02x", first)
	default: // 3
		return uint64(first & 0x3F), true, nil
	}
}

// ReadPlainLength is ReadLength that rejects special encodings.
func (r *Reader) ReadPlainLength() (uint64, error) {
	n, enc, err := r.ReadLength()
	if err == nil && enc {
		err = errors.New("unexpected encoded length")
	}
	return n, err
}

// String encodings used when ReadLength returns encoded=true.
const (
	encInt8  = 0
	encInt16 = 1
	encInt32 = 2
	encLZF   = 3
)

// maxStringLen guards against corrupt files asking us to allocate huge buffers.
const maxStringLen = 1 << 31

// ReadString reads a string, which may be stored raw, as an integer, or LZF-compressed.
func (r *Reader) ReadString() ([]byte, error) {
	return r.readStringInto(nil)
}

// readStringInto is ReadString that reuses buf's memory when it is big enough.
func (r *Reader) readStringInto(buf []byte) ([]byte, error) {
	n, enc, err := r.ReadLength()
	if err != nil {
		return nil, err
	}
	if !enc {
		if n > maxStringLen {
			return nil, fmt.Errorf("string length %d too large", n)
		}
		out := buf[:0]
		if uint64(cap(out)) < n {
			out = make([]byte, n)
		}
		out = out[:n]
		_, err := io.ReadFull(r.br, out)
		return out, err
	}
	switch n {
	case encInt8, encInt16, encInt32:
		size := 1 << n // 1, 2, 4 bytes, little-endian
		if _, err := io.ReadFull(r.br, r.buf[:size]); err != nil {
			return nil, err
		}
		var v int64
		switch size {
		case 1:
			v = int64(int8(r.buf[0]))
		case 2:
			v = int64(int16(binary.LittleEndian.Uint16(r.buf[:2])))
		case 4:
			v = int64(int32(binary.LittleEndian.Uint32(r.buf[:4])))
		}
		return strconv.AppendInt(buf[:0], v, 10), nil
	case encLZF:
		clen, err := r.ReadPlainLength()
		if err != nil {
			return nil, err
		}
		ulen, err := r.ReadPlainLength()
		if err != nil {
			return nil, err
		}
		if clen > maxStringLen || ulen > maxStringLen {
			return nil, errors.New("lzf length too large")
		}
		comp := make([]byte, clen)
		if _, err := io.ReadFull(r.br, comp); err != nil {
			return nil, err
		}
		return lzfDecompress(comp, int(ulen))
	}
	return nil, fmt.Errorf("unknown string encoding %d", n)
}

// AuxField is one metadata entry from the file header area (redis-ver, used-mem, ...).
type AuxField struct {
	Key, Value string
}

// ReadAux reads AUX fields until a non-AUX opcode, which it returns so the
// caller can continue parsing from there.
func (r *Reader) ReadAux() (fields []AuxField, next byte, err error) {
	for {
		op, err := r.br.ReadByte()
		if err != nil {
			return fields, 0, err
		}
		if op != OpAux {
			return fields, op, nil
		}
		k, err := r.ReadString()
		if err != nil {
			return fields, 0, fmt.Errorf("aux key: %w", err)
		}
		v, err := r.ReadString()
		if err != nil {
			return fields, 0, fmt.Errorf("aux %q value: %w", k, err)
		}
		fields = append(fields, AuxField{string(k), string(v)})
	}
}
