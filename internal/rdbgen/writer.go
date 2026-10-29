// Package rdbgen writes synthetic RDB files for tests and benchmarks.
// It covers the encodings Redis 7.x emits for small values (listpacks,
// intsets, single-node quicklists), which is what most real keys use.
package rdbgen

import (
	"bufio"
	"encoding/binary"
	"io"
)

// Writer emits RDB v12. Errors are sticky: check Err (or Close) once at the end.
type Writer struct {
	w   *bufio.Writer
	err error
	lp  []byte // reused listpack buffer
	tmp [9]byte

	pendingIdle uint64 // idle seconds + 1 for the next key; 0 = none
}

func NewWriter(w io.Writer) *Writer {
	wr := &Writer{w: bufio.NewWriterSize(w, 1<<20)}
	wr.raw([]byte("REDIS0012"))
	return wr
}

func (w *Writer) Err() error { return w.err }

func (w *Writer) raw(b []byte) {
	if w.err == nil {
		_, w.err = w.w.Write(b)
	}
}

func (w *Writer) byte(b byte) {
	if w.err == nil {
		w.err = w.w.WriteByte(b)
	}
}

// length writes an RDB length encoding (see rdb.Reader.ReadLength).
func (w *Writer) length(n uint64) {
	switch {
	case n < 1<<6:
		w.byte(byte(n))
	case n < 1<<14:
		w.raw([]byte{0x40 | byte(n>>8), byte(n)})
	case n <= 1<<32-1:
		w.tmp[0] = 0x80
		binary.BigEndian.PutUint32(w.tmp[1:5], uint32(n))
		w.raw(w.tmp[:5])
	default:
		w.tmp[0] = 0x81
		binary.BigEndian.PutUint64(w.tmp[1:9], n)
		w.raw(w.tmp[:9])
	}
}

func (w *Writer) str(s []byte) {
	w.length(uint64(len(s)))
	w.raw(s)
}

// intStr writes v as an integer-encoded string, as Redis does for numeric values.
func (w *Writer) intStr(v int32) {
	switch {
	case v >= -1<<7 && v < 1<<7:
		w.raw([]byte{0xC0, byte(int8(v))})
	case v >= -1<<15 && v < 1<<15:
		w.raw([]byte{0xC1, byte(v), byte(v >> 8)})
	default:
		w.tmp[0] = 0xC2
		binary.LittleEndian.PutUint32(w.tmp[1:5], uint32(v))
		w.raw(w.tmp[:5])
	}
}

// Aux writes a metadata field (redis-ver, ctime, ...).
func (w *Writer) Aux(k, v string) {
	w.byte(0xFA)
	w.str([]byte(k))
	w.str([]byte(v))
}

// SelectDB starts database db, announcing its key and expires counts.
func (w *Writer) SelectDB(db, keys, expires uint64) {
	w.byte(0xFE)
	w.length(db)
	w.byte(0xFB)
	w.length(keys)
	w.length(expires)
}

// Idle records LRU idle seconds for the next key, as Redis does under an
// *-lru maxmemory-policy. Call it right before String/Hash/....
func (w *Writer) Idle(sec uint64) {
	w.pendingIdle = sec + 1 // +1 so zero means "none"
}

// keyHeader writes the optional expiry, the value type and the key.
func (w *Writer) keyHeader(typ byte, key []byte, expireMS int64) {
	if expireMS > 0 {
		w.byte(0xFC)
		binary.LittleEndian.PutUint64(w.tmp[:8], uint64(expireMS))
		w.raw(w.tmp[:8])
	}
	if w.pendingIdle > 0 { // Redis writes expiry, then LRU/LFU info, then the key
		w.byte(0xF8)
		w.length(w.pendingIdle - 1)
		w.pendingIdle = 0
	}
	w.byte(typ)
	w.str(key)
}

// String writes a string key. expireMS is absolute unix ms; 0 = no TTL.
func (w *Writer) String(key, val []byte, expireMS int64) {
	w.keyHeader(0, key, expireMS)
	w.str(val)
}

// IntString writes a string key whose value is a number.
func (w *Writer) IntString(key []byte, val int32, expireMS int64) {
	w.keyHeader(0, key, expireMS)
	w.intStr(val)
}

// Hash writes a small hash (listpack encoding). kv alternates field, value.
func (w *Writer) Hash(key []byte, kv [][]byte, expireMS int64) {
	w.keyHeader(16, key, expireMS)
	w.str(w.listpack(kv))
}

// List writes a small list (quicklist with a single packed listpack node).
func (w *Writer) List(key []byte, items [][]byte, expireMS int64) {
	w.keyHeader(18, key, expireMS)
	w.length(1) // nodes
	w.length(2) // container: PACKED
	w.str(w.listpack(items))
}

// Set writes a small set of strings (listpack encoding).
func (w *Writer) Set(key []byte, members [][]byte, expireMS int64) {
	w.keyHeader(20, key, expireMS)
	w.str(w.listpack(members))
}

// ZSet writes a small sorted set (listpack). ms alternates member, score.
func (w *Writer) ZSet(key []byte, ms [][]byte, expireMS int64) {
	w.keyHeader(17, key, expireMS)
	w.str(w.listpack(ms))
}

// IntSet writes a set of small integers (intset encoding). vals must be sorted and unique.
func (w *Writer) IntSet(key []byte, vals []int16, expireMS int64) {
	w.keyHeader(11, key, expireMS)
	b := w.lp[:0]
	b = binary.LittleEndian.AppendUint32(b, 2) // 2 bytes per element
	b = binary.LittleEndian.AppendUint32(b, uint32(len(vals)))
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	w.lp = b
	w.str(b)
}

// listpack encodes entries as strings shorter than 64 bytes (the 6-bit
// string encoding), which is all the generator needs.
//
//	<total bytes u32><count u16> { 0x80|len, data, backlen } ... 0xFF
func (w *Writer) listpack(entries [][]byte) []byte {
	b := append(w.lp[:0], 0, 0, 0, 0, 0, 0)
	for _, e := range entries {
		if len(e) >= 64 {
			panic("rdbgen: listpack entries must be < 64 bytes")
		}
		b = append(b, 0x80|byte(len(e)))
		b = append(b, e...)
		b = append(b, byte(1+len(e))) // backlen: entry size, fits one byte below 128
	}
	b = append(b, 0xFF)
	binary.LittleEndian.PutUint32(b[0:4], uint32(len(b)))
	binary.LittleEndian.PutUint16(b[4:6], uint16(len(entries)))
	w.lp = b
	return b
}

// Close writes EOF and a zero checksum (Redis treats 0 as "not computed") and flushes.
func (w *Writer) Close() error {
	w.byte(0xFF)
	w.raw(make([]byte, 8))
	if w.err == nil {
		w.err = w.w.Flush()
	}
	return w.err
}
