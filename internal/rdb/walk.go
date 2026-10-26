package rdb

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Type is the on-disk value type byte.
type Type byte

// On-disk value types (rdb.h). Encodings differ, but each maps to one logical Kind.
const (
	TypeString            Type = 0
	TypeList              Type = 1
	TypeSet               Type = 2
	TypeZSet              Type = 3
	TypeHash              Type = 4
	TypeZSet2             Type = 5
	TypeModule2           Type = 7
	TypeHashZipmap        Type = 9
	TypeListZiplist       Type = 10
	TypeSetIntset         Type = 11
	TypeZSetZiplist       Type = 12
	TypeHashZiplist       Type = 13
	TypeListQuicklist     Type = 14
	TypeStreamListpacks   Type = 15
	TypeHashListpack      Type = 16
	TypeZSetListpack      Type = 17
	TypeListQuicklist2    Type = 18
	TypeStreamListpacks2  Type = 19
	TypeSetListpack       Type = 20
	TypeStreamListpacks3  Type = 21
	TypeHashMetadataPreGA Type = 22 // Redis 7.4 RC hash-field-expiry
	TypeHashListpackExPre Type = 23
	TypeHashMetadata      Type = 24 // Redis 7.4 GA hash-field-expiry
	TypeHashListpackEx    Type = 25
)

// Kind is the logical Redis type, as shown by the TYPE command.
type Kind string

const (
	KindString Kind = "string"
	KindList   Kind = "list"
	KindSet    Kind = "set"
	KindZSet   Kind = "zset"
	KindHash   Kind = "hash"
	KindStream Kind = "stream"
	KindModule Kind = "module"
)

// Kind maps an encoding to its logical type.
func (t Type) Kind() Kind {
	switch t {
	case TypeString:
		return KindString
	case TypeList, TypeListZiplist, TypeListQuicklist, TypeListQuicklist2:
		return KindList
	case TypeSet, TypeSetIntset, TypeSetListpack:
		return KindSet
	case TypeZSet, TypeZSet2, TypeZSetZiplist, TypeZSetListpack:
		return KindZSet
	case TypeHash, TypeHashZipmap, TypeHashZiplist, TypeHashListpack,
		TypeHashMetadataPreGA, TypeHashListpackExPre, TypeHashMetadata, TypeHashListpackEx:
		return KindHash
	case TypeStreamListpacks, TypeStreamListpacks2, TypeStreamListpacks3:
		return KindStream
	case TypeModule2:
		return KindModule
	}
	return Kind(fmt.Sprintf("unknown(%d)", t))
}

// Entry describes one key. Key is only valid during the callback — copy it if
// you need to keep it (we deliberately never retain raw keys).
type Entry struct {
	DB         int
	Key        []byte
	Type       Type
	ExpireMS   int64 // absolute unix ms; 0 = no TTL
	IdleSec    int64 // LRU idle seconds; -1 = not present
	Freq       int   // LFU counter; -1 = not present
	ValueBytes int64 // serialized size of the value in the file
	MemBytes   int64 // estimated in-RAM size: key overhead + value (uncalibrated)
	Elems      int64 // elements in a collection (0 for strings)
}

// Header is everything before the first key.
type Header struct {
	Version int
	Aux     []AuxField
}

// Walk parses an RDB stream, calling onHeader once and onKey for every key.
// Values are skipped, never decoded, so memory stays flat regardless of file size.
func Walk(src io.Reader, onHeader func(Header) error, onKey func(*Entry) error) error {
	r := NewReader(src)
	ver, err := r.ReadHeader()
	if err != nil {
		return err
	}
	h := Header{Version: ver}

	e := Entry{IdleSec: -1, Freq: -1}
	headerDone := false
	for {
		op, err := r.ReadByte()
		if err != nil {
			return fmt.Errorf("offset %d: reading opcode: %w", r.Offset(), err)
		}
		// Aux fields come first; the first non-aux opcode ends the header.
		if op != OpAux && !headerDone {
			headerDone = true
			if onHeader != nil {
				if err := onHeader(h); err != nil {
					return err
				}
			}
		}
		switch op {
		case OpEOF:
			return nil // 8-byte CRC64 may follow; we don't verify it (yet).
		case OpAux:
			k, err := r.ReadString()
			if err != nil {
				return err
			}
			v, err := r.ReadString()
			if err != nil {
				return err
			}
			h.Aux = append(h.Aux, AuxField{string(k), string(v)})
		case OpSelectDB:
			n, err := r.ReadPlainLength()
			if err != nil {
				return err
			}
			e.DB = int(n)
		case OpResizeDB:
			if _, err := r.ReadPlainLength(); err != nil {
				return err
			}
			if _, err := r.ReadPlainLength(); err != nil {
				return err
			}
		case OpExpireTimeMS:
			if err := r.readFull(8); err != nil {
				return err
			}
			e.ExpireMS = int64(binary.LittleEndian.Uint64(r.buf[:8]))
		case OpExpireTime:
			if err := r.readFull(4); err != nil {
				return err
			}
			e.ExpireMS = int64(binary.LittleEndian.Uint32(r.buf[:4])) * 1000
		case OpIdle:
			n, err := r.ReadPlainLength()
			if err != nil {
				return err
			}
			e.IdleSec = int64(n)
		case OpFreq:
			b, err := r.ReadByte()
			if err != nil {
				return err
			}
			e.Freq = int(b)
		case OpSlotInfo: // slot id, slot size, expires slot size
			for i := 0; i < 3; i++ {
				if _, err := r.ReadPlainLength(); err != nil {
					return err
				}
			}
		case OpFunction2:
			if err := r.SkipString(); err != nil {
				return err
			}
		case OpModuleAux:
			if err := r.skipModuleAux(); err != nil {
				return err
			}
		default:
			// Anything else is a value type: key, then value.
			key, err := r.readStringInto(r.key)
			r.key = key
			if err != nil {
				return fmt.Errorf("offset %d: key: %w", r.Offset(), err)
			}
			start := r.Offset()
			r.acc = memAcc{}
			if err := r.skipValue(Type(op)); err != nil {
				return fmt.Errorf("offset %d: value of type %d: %w", start, op, err)
			}
			e.Key = key
			e.Type = Type(op)
			e.ValueBytes = r.Offset() - start
			e.MemBytes = keyOverhead(len(key), e.ExpireMS > 0) + valueMem(e.Type, &r.acc, e.ValueBytes)
			e.Elems = int64(r.acc.elems)
			if err := onKey(&e); err != nil {
				return err
			}
			// Reset per-key fields; DB carries over.
			e = Entry{DB: e.DB, IdleSec: -1, Freq: -1}
		}
	}
}

func (r *Reader) readFull(n int) error {
	_, err := io.ReadFull(r.br, r.buf[:n])
	return err
}

// skipStrings skips n consecutive strings.
func (r *Reader) skipStrings(n uint64) error {
	for i := uint64(0); i < n; i++ {
		if err := r.SkipString(); err != nil {
			return err
		}
	}
	return nil
}

// skipLengths skips n consecutive length-encoded integers.
func (r *Reader) skipLengths(n int) error {
	for i := 0; i < n; i++ {
		if _, err := r.ReadPlainLength(); err != nil {
			return err
		}
	}
	return nil
}

// skipValue skips a value of type t without decoding it.
func (r *Reader) skipValue(t Type) error {
	switch t {
	case TypeString:
		n, isInt, err := r.skipStr()
		r.acc.strLen, r.acc.isInt = n, isInt
		return err

	// Single-blob encodings: the whole collection is one serialized string.
	case TypeHashZipmap, TypeListZiplist, TypeSetIntset, TypeZSetZiplist,
		TypeHashZiplist, TypeHashListpack, TypeZSetListpack, TypeSetListpack,
		TypeHashListpackExPre:
		return r.blobString()

	case TypeHashListpackEx: // min-expire ms + listpack
		if err := r.readFull(8); err != nil {
			return err
		}
		return r.blobString()

	case TypeList, TypeSet:
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.elems = n
		return r.elemStrings(n)

	case TypeListQuicklist: // n ziplist nodes
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.nodes = n
		for i := uint64(0); i < n; i++ {
			if err := r.blobString(); err != nil {
				return err
			}
		}
		return nil

	case TypeHash:
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.elems = n
		return r.elemStrings(2 * n)

	case TypeZSet: // member + score as length-prefixed ASCII double
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.elems = n
		for i := uint64(0); i < n; i++ {
			if err := r.elemString(); err != nil {
				return err
			}
			b, err := r.ReadByte()
			if err != nil {
				return err
			}
			if b < 253 { // 253/254/255 = nan/+inf/-inf with no payload
				if err := r.Skip(uint64(b)); err != nil {
					return err
				}
			}
		}
		return nil

	case TypeZSet2: // member + 8-byte binary double
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.elems = n
		for i := uint64(0); i < n; i++ {
			if err := r.elemString(); err != nil {
				return err
			}
			if err := r.Skip(8); err != nil {
				return err
			}
		}
		return nil

	case TypeListQuicklist2: // per node: container type + listpack
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.nodes = n
		for i := uint64(0); i < n; i++ {
			if _, err := r.ReadPlainLength(); err != nil {
				return err
			}
			if err := r.blobString(); err != nil {
				return err
			}
		}
		return nil

	case TypeHashMetadataPreGA: // per field: 8-byte ttl, field, value
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.elems = n
		for i := uint64(0); i < n; i++ {
			if err := r.Skip(8); err != nil {
				return err
			}
			if err := r.elemStrings(2); err != nil {
				return err
			}
		}
		return nil

	case TypeHashMetadata: // min-expire ms; per field: ttl length, field, value
		if err := r.Skip(8); err != nil {
			return err
		}
		n, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		r.acc.elems = n
		for i := uint64(0); i < n; i++ {
			if _, err := r.ReadPlainLength(); err != nil {
				return err
			}
			if err := r.elemStrings(2); err != nil {
				return err
			}
		}
		return nil

	case TypeStreamListpacks, TypeStreamListpacks2, TypeStreamListpacks3:
		return r.skipStream(t)

	case TypeModule2:
		if _, err := r.ReadPlainLength(); err != nil { // module id
			return err
		}
		return r.skipModuleOpcodes()
	}
	return fmt.Errorf("unsupported value type %d", t)
}

func (r *Reader) skipStream(t Type) error {
	n, err := r.ReadPlainLength() // listpack nodes: master ID + listpack
	if err != nil {
		return err
	}
	r.acc.nodes = n
	for i := uint64(0); i < 2*n; i++ {
		if err := r.blobString(); err != nil {
			return err
		}
	}
	// length, last_id.ms, last_id.seq
	if err := r.skipLengths(3); err != nil {
		return err
	}
	if t >= TypeStreamListpacks2 {
		// first_id(2), max_deleted_id(2), entries_added
		if err := r.skipLengths(5); err != nil {
			return err
		}
	}
	groups, err := r.ReadPlainLength()
	if err != nil {
		return err
	}
	r.acc.groups = groups
	for g := uint64(0); g < groups; g++ {
		if err := r.SkipString(); err != nil { // group name
			return err
		}
		if err := r.skipLengths(2); err != nil { // last_id
			return err
		}
		if t >= TypeStreamListpacks2 {
			if err := r.skipLengths(1); err != nil { // entries_read
				return err
			}
		}
		pel, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		for i := uint64(0); i < pel; i++ {
			if err := r.Skip(16 + 8); err != nil { // raw id + delivery time
				return err
			}
			if err := r.skipLengths(1); err != nil { // delivery count
				return err
			}
		}
		consumers, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		for c := uint64(0); c < consumers; c++ {
			if err := r.SkipString(); err != nil { // name
				return err
			}
			skip := uint64(8) // seen time
			if t >= TypeStreamListpacks3 {
				skip += 8 // active time
			}
			if err := r.Skip(skip); err != nil {
				return err
			}
			cpel, err := r.ReadPlainLength()
			if err != nil {
				return err
			}
			if err := r.Skip(16 * cpel); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reader) skipModuleAux() error {
	// module id, when_opcode, when
	if err := r.skipLengths(3); err != nil {
		return err
	}
	return r.skipModuleOpcodes()
}

// skipModuleOpcodes skips self-describing module data until its EOF marker.
func (r *Reader) skipModuleOpcodes() error {
	for {
		op, err := r.ReadPlainLength()
		if err != nil {
			return err
		}
		switch op {
		case 0: // EOF
			return nil
		case 1, 2: // signed / unsigned int
			_, err = r.ReadPlainLength()
		case 3: // float
			err = r.Skip(4)
		case 4: // double
			err = r.Skip(8)
		case 5: // string
			err = r.SkipString()
		default:
			return fmt.Errorf("unknown module opcode %d", op)
		}
		if err != nil {
			return err
		}
	}
}
