package rdb

// In-RAM size model for Redis 7.x on 64-bit with jemalloc.
//
// The RDB stores values compactly (and maybe LZF-compressed), so file bytes
// say little about RAM. While skipping a value we record a few facts (memAcc),
// then estimate() applies Redis' data-structure overheads. The result is
// later calibrated against the snapshot's used-mem aux field.

// memAcc collects size facts about the value being skipped.
type memAcc struct {
	elems  uint64 // collection elements (a hash field+value counts as 1)
	strs   uint64 // allocator bytes for element strings (hashtable encodings)
	blobs  uint64 // allocator bytes for compact blobs (listpack, ziplist, intset)
	nodes  uint64 // quicklist / stream nodes
	groups uint64 // stream consumer groups
	strLen uint64 // string type: logical length
	isInt  bool   // string type: integer-encoded
}

// Struct sizes, from the Redis source (64-bit).
const (
	sizeRobj         = 16 // redisObject
	sizeDictEntry    = 24 // key, value, next pointers
	sizeDict         = 56 // dict + its two table headers
	sizeQuicklist    = 40
	sizeQuicklistNod = 32
	sizeZset         = 16 // zset{dict*, zskiplist*}
	sizeSkiplistNode = 48 // ele, score, backward + ~1.33 levels * 16, rounded by jemalloc
	sizeRaxNode      = 32 // rough per-listpack-node rax overhead
	embstrMax        = 44 // strings up to this length are embedded with their robj
	listpackMaxBytes = 8192

	// Streams: rax trees, listpack preallocation and consumer-group bookkeeping
	// are hard to model; these two are measured with MEMORY USAGE on Redis 7.4.
	sizeStream   = 536 // empty-ish stream incl. rax + preallocated listpack slack
	sizeStreamCG = 284 // per consumer group with an empty PEL
)

// jemallocSize rounds n up to jemalloc's size class. Redis builds jemalloc with
// lg-quantum=3, so classes are 8,16,24,...,64, then 4 per power of two:
// 80,96,112,128, 160,192,224,256, 320,... (verified against MEMORY USAGE).
func jemallocSize(n uint64) uint64 {
	switch {
	case n == 0:
		return 0
	case n <= 64:
		return (n + 7) &^ 7
	}
	p := uint64(1)
	for p < n {
		p <<= 1
	}
	step := p / 8
	return (n + step - 1) / step * step
}

// sdsAlloc is the allocator size of an SDS string holding n bytes.
func sdsAlloc(n uint64) uint64 {
	var hdr uint64
	switch {
	case n < 1<<5:
		hdr = 1
	case n < 1<<8:
		hdr = 3
	case n < 1<<16:
		hdr = 5
	case n < 1<<32:
		hdr = 9
	default:
		hdr = 17
	}
	return jemallocSize(hdr + n + 1) // +1 for the trailing '\0'
}

// dictAlloc is the hash table cost for n entries: header + power-of-two bucket array.
func dictAlloc(n uint64) uint64 {
	buckets := uint64(4)
	for buckets < n {
		buckets <<= 1
	}
	return sizeDict + 8*buckets
}

// keyOverhead is what the keyspace pays per key regardless of value:
// dict entry, ~1 bucket pointer, the key SDS, and an expires entry if it has a TTL.
func keyOverhead(keyLen int, hasTTL bool) int64 {
	n := sizeDictEntry + 8 + sdsAlloc(uint64(keyLen))
	if hasTTL {
		n += sizeDictEntry + 8
	}
	return int64(n)
}

// valueMem estimates RAM for a value of on-disk type t.
func valueMem(t Type, a *memAcc, fileBytes int64) int64 {
	var n uint64
	switch t {
	case TypeString:
		switch {
		case a.isInt:
			n = sizeRobj
		case a.strLen <= embstrMax:
			n = jemallocSize(sizeRobj + 3 + a.strLen + 1)
		default:
			n = sizeRobj + sdsAlloc(a.strLen)
		}

	case TypeHashZipmap, TypeListZiplist, TypeSetIntset, TypeZSetZiplist,
		TypeHashZiplist, TypeHashListpack, TypeZSetListpack, TypeSetListpack,
		TypeHashListpackExPre, TypeHashListpackEx:
		n = sizeRobj + a.blobs

	case TypeListQuicklist, TypeListQuicklist2:
		// Redis 7.2+ keeps a single small node as a plain listpack (no quicklist).
		if a.nodes == 1 && a.blobs <= listpackMaxBytes {
			n = sizeRobj + a.blobs
		} else {
			n = sizeRobj + sizeQuicklist + a.nodes*sizeQuicklistNod + a.blobs
		}

	case TypeList: // legacy linked list; Redis repacks it into listpacks on load
		n = sizeRobj + sizeQuicklist + sizeQuicklistNod + a.strs

	case TypeSet, TypeHash, TypeHashMetadataPreGA, TypeHashMetadata:
		n = sizeRobj + dictAlloc(a.elems) + a.elems*sizeDictEntry + a.strs

	case TypeZSet, TypeZSet2:
		n = sizeRobj + sizeZset + dictAlloc(a.elems) +
			a.elems*(sizeDictEntry+sizeSkiplistNode) + a.strs

	case TypeStreamListpacks, TypeStreamListpacks2, TypeStreamListpacks3:
		n = sizeRobj + sizeStream + a.nodes*sizeRaxNode + a.blobs + a.groups*sizeStreamCG

	default: // modules: opaque, fall back to file size
		return fileBytes
	}
	return int64(n)
}

// elemString skips a collection element string and accounts its SDS allocation.
func (r *Reader) elemString() error {
	n, _, err := r.skipStr()
	r.acc.strs += sdsAlloc(n)
	return err
}

// elemStrings skips n element strings.
func (r *Reader) elemStrings(n uint64) error {
	for i := uint64(0); i < n; i++ {
		if err := r.elemString(); err != nil {
			return err
		}
	}
	return nil
}

// blobString skips a compact-encoded blob, which Redis keeps in RAM as-is.
func (r *Reader) blobString() error {
	n, _, err := r.skipStr()
	r.acc.blobs += jemallocSize(n)
	return err
}
