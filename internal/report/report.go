// Package report runs the analysis and renders it as text or JSON.
// The output contains only patterns and numbers, never raw keys or values.
package report

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ujjwalrastogi17/redislens/internal/pattern"
	"github.com/ujjwalrastogi17/redislens/internal/rdb"
)

// SchemaVersion is bumped on any breaking change to the JSON output.
// Adding fields is not breaking; renaming/removing/retyping is.
const SchemaVersion = 1

// Report is the full analysis result. JSON field names are part of the public schema.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Tool          Tool      `json:"tool"`
	Source        Source    `json:"source"`
	Totals        Totals    `json:"totals"`
	Shards        []Shard   `json:"shards"`   // one per input, in input order
	Findings      []Finding `json:"findings"` // most important first
	Types         []TypeRow `json:"types"`
	Prefixes      []Row     `json:"prefixes"`
	Patterns      []Row     `json:"patterns"`
}

type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Source describes the analyzed snapshot. With several shards it describes
// them together (see combineSources); per-shard details are in Report.Shards.
type Source struct {
	File         string     `json:"file"`
	SizeBytes    int64      `json:"size_bytes,omitempty"`  // unknown when reading stdin
	RDBVersion   int        `json:"rdb_version,omitempty"` // 0 when shards differ
	RedisVersion string     `json:"redis_version,omitempty"`
	SnapshotTime *time.Time `json:"snapshot_time,omitempty"` // pointer: omitted when unknown
	UsedMemBytes int64      `json:"used_mem_bytes,omitempty"`
}

type Totals struct {
	Keys          int64 `json:"keys"`
	KeysWithTTL   int64 `json:"keys_with_ttl"`
	MemoryBytes   int64 `json:"memory_bytes"`   // sum of per-key estimates
	FileBytes     int64 `json:"file_bytes"`     // serialized value bytes in the RDB
	OverheadBytes int64 `json:"overhead_bytes"` // used_mem - memory_bytes (startup, buffers, ...)
	Patterns      int   `json:"patterns"`
}

// Shard is one input's own numbers. Source's fields are embedded, so in JSON
// they sit next to keys/memory_bytes rather than under a "source" key.
type Shard struct {
	Source
	Keys        int64   `json:"keys"`
	KeysWithTTL int64   `json:"keys_with_ttl"`
	MemoryBytes int64   `json:"memory_bytes"`
	Share       float64 `json:"share"` // fraction of total key memory, 0..1
}

type TypeRow struct {
	Type        string `json:"type"`
	Keys        int64  `json:"keys"`
	KeysWithTTL int64  `json:"keys_with_ttl"`
	MemoryBytes int64  `json:"memory_bytes"`
	FileBytes   int64  `json:"file_bytes"`
}

type Row struct {
	Pattern     string  `json:"pattern"`
	Type        string  `json:"type,omitempty"` // empty for prefix rollups
	Keys        int64   `json:"keys"`
	MemoryBytes int64   `json:"memory_bytes"`
	Share       float64 `json:"share"` // fraction of total key memory, 0..1
	KeysWithTTL int64   `json:"keys_with_ttl"`
	TTL         *TTL    `json:"ttl,omitempty"`
	MaxKeyBytes int64   `json:"max_key_bytes"`          // largest single key
	MaxElements int64   `json:"max_elements,omitempty"` // when known (large hash/set/zset)
}

type TTL struct {
	MinMS int64 `json:"min_ms"`
	AvgMS int64 `json:"avg_ms"`
	MaxMS int64 `json:"max_ms"`
}

// Input is one RDB stream. Several inputs are shards of one dataset (e.g. the
// nodes of a Redis Cluster) and are reported together.
type Input struct {
	R         io.Reader
	File      string // for display only
	SizeBytes int64  // 0 = unknown (stdin)
}

// Options control the analysis.
type Options struct {
	MaxChildren int // trie collapse threshold; 0 = default
	Top         int // rows to keep in prefixes/patterns; 0 = all
	ToolVersion string
	Workers     int // goroutines classifying keys, shared by all shards; 0 = one per CPU

	// Finding thresholds; zero values mean the defaults below.
	BigKeyBytes int64         // a key at least this big (estimated RAM) is a big key
	BigKeyElems int64         // ...or with at least this many elements, when known
	ColdAfter   time.Duration // idle at least this long = cold memory
}

const (
	DefaultBigKeyBytes = 1 << 20 // 1 MiB
	DefaultBigKeyElems = 10_000
	DefaultColdAfter   = 7 * 24 * time.Hour
)

func (o Options) withDefaults() Options {
	if o.BigKeyBytes <= 0 {
		o.BigKeyBytes = DefaultBigKeyBytes
	}
	if o.BigKeyElems <= 0 {
		o.BigKeyElems = DefaultBigKeyElems
	}
	if o.ColdAfter <= 0 {
		o.ColdAfter = DefaultColdAfter
	}
	return o
}

// errStopped aborts the other shards once one has failed.
var errStopped = errors.New("stopped")

// Analyze streams every input and builds one Report.
// Shards are parsed concurrently; their tries are merged before Consolidate,
// so a pattern spread over many shards is still one row.
func Analyze(opt Options, inputs ...Input) (*Report, error) {
	if len(inputs) == 0 {
		return nil, errors.New("no input")
	}
	opt = opt.withDefaults()
	workers := opt.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	// One parse goroutine keeps about 3 workers busy (see BenchmarkAnalyze), so
	// run several shards at once rather than give one shard every core.
	parallel := min(len(inputs), max(1, workers/4))
	perShard := max(1, workers/parallel)

	shards := make([]*shardResult, len(inputs))
	errs := make([]error, len(inputs))
	var stop atomic.Bool
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel) // limits how many shards run at once
	for i, in := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			shards[i], errs[i] = analyzeShard(in, opt, perShard, &stop)
			if errs[i] != nil {
				stop.Store(true)
			}
		}()
	}
	wg.Wait()
	// Report the shard that actually failed, not the ones it stopped.
	for i, err := range errs {
		if err != nil && !errors.Is(err, errStopped) {
			return nil, fmt.Errorf("%s: %w", inputs[i].File, err)
		}
	}
	return combine(shards, opt), nil
}

// shardResult is everything one RDB contributes, before combining.
type shardResult struct {
	src    Source
	totals Totals
	types  map[string]*TypeRow
	trie   *pattern.Trie
	// How many keys carried LRU idle time / LFU counters; tells the findings
	// whether a cold-memory check was possible.
	idleKeys, lfuKeys int64
}

func analyzeShard(in Input, opt Options, workers int, stop *atomic.Bool) (*shardResult, error) {
	sh := &shardResult{
		src:   Source{File: in.File, SizeBytes: in.SizeBytes},
		types: map[string]*TypeRow{},
	}
	pipe := newPipeline(workers, opt.MaxChildren)
	coldSec := int64(opt.ColdAfter / time.Second)
	var snapshotMS int64

	onHeader := func(h rdb.Header) error {
		sh.src.RDBVersion = h.Version
		for _, a := range h.Aux {
			switch a.Key {
			case "redis-ver", "valkey-ver":
				sh.src.RedisVersion = a.Value
			case "ctime":
				if sec, err := strconv.ParseInt(a.Value, 10, 64); err == nil {
					t := time.Unix(sec, 0).UTC()
					sh.src.SnapshotTime = &t
					snapshotMS = sec * 1000
				}
			case "used-mem":
				sh.src.UsedMemBytes, _ = strconv.ParseInt(a.Value, 10, 64)
			}
		}
		if snapshotMS == 0 {
			snapshotMS = time.Now().UnixMilli()
		}
		return nil
	}

	onKey := func(e *rdb.Entry) error {
		if stop.Load() {
			return errStopped
		}
		kind := string(e.Type.Kind())
		tr := sh.types[kind]
		if tr == nil {
			tr = &TypeRow{Type: kind}
			sh.types[kind] = tr
		}
		hasTTL := e.ExpireMS > 0
		tr.Keys++
		tr.MemoryBytes += e.MemBytes
		tr.FileBytes += e.ValueBytes
		sh.totals.Keys++
		sh.totals.MemoryBytes += e.MemBytes
		sh.totals.FileBytes += e.ValueBytes
		k := pattern.Key{
			Bytes:     e.MemBytes,
			Elems:     e.Elems,
			Big:       e.MemBytes >= opt.BigKeyBytes || e.Elems >= opt.BigKeyElems,
			IdleKnown: e.IdleSec >= 0,
			Cold:      e.IdleSec >= coldSec,
		}
		if hasTTL {
			tr.KeysWithTTL++
			sh.totals.KeysWithTTL++
			// TTL is relative to this shard's own snapshot time.
			k.HasTTL = true
			k.TTLMS = max(e.ExpireMS-snapshotMS, 0) // already-expired keys count as 0
		}
		if k.IdleKnown {
			sh.idleKeys++
		}
		if e.Freq >= 0 {
			sh.lfuKeys++
		}
		// Hand the key to a worker, which turns it into a template. Raw keys
		// only live in in-flight batches and are never kept.
		pipe.add(e.Key, kind, k)
		return nil
	}

	err := rdb.Walk(in.R, onHeader, onKey)
	sh.trie = pipe.finish() // always, so worker goroutines exit even on error
	if err != nil {
		return nil, err
	}
	return sh, nil
}

// combine merges shard results into the final report.
func combine(shards []*shardResult, opt Options) *Report {
	rep := &Report{
		SchemaVersion: SchemaVersion,
		Tool:          Tool{"redislens", opt.ToolVersion},
		Source:        combineSources(shards),
	}
	trie := shards[0].trie
	byType := map[string]*TypeRow{}
	for i, sh := range shards {
		if i > 0 {
			trie.Merge(sh.trie)
		}
		t := &rep.Totals
		t.Keys += sh.totals.Keys
		t.KeysWithTTL += sh.totals.KeysWithTTL
		t.MemoryBytes += sh.totals.MemoryBytes
		t.FileBytes += sh.totals.FileBytes
		for kind, r := range sh.types {
			sum := byType[kind]
			if sum == nil {
				sum = &TypeRow{Type: kind}
				byType[kind] = sum
			}
			sum.Keys += r.Keys
			sum.KeysWithTTL += r.KeysWithTTL
			sum.MemoryBytes += r.MemoryBytes
			sum.FileBytes += r.FileBytes
		}
	}
	trie.Consolidate()

	if rep.Source.UsedMemBytes > 0 {
		rep.Totals.OverheadBytes = rep.Source.UsedMemBytes - rep.Totals.MemoryBytes
	}
	for _, sh := range shards {
		row := Shard{
			Source:      sh.src,
			Keys:        sh.totals.Keys,
			KeysWithTTL: sh.totals.KeysWithTTL,
			MemoryBytes: sh.totals.MemoryBytes,
		}
		if rep.Totals.MemoryBytes > 0 {
			row.Share = float64(row.MemoryBytes) / float64(rep.Totals.MemoryBytes)
		}
		rep.Shards = append(rep.Shards, row)
	}
	for _, t := range byType {
		rep.Types = append(rep.Types, *t)
	}
	sort.Slice(rep.Types, func(i, j int) bool {
		if rep.Types[i].MemoryBytes != rep.Types[j].MemoryBytes {
			return rep.Types[i].MemoryBytes > rep.Types[j].MemoryBytes
		}
		return rep.Types[i].Type < rep.Types[j].Type
	})

	patterns := trie.Patterns()
	rep.Totals.Patterns = len(patterns)
	rep.Findings = findings(patterns, shards, rep, opt) // before Top cuts the rows
	rep.Prefixes = toRows(trie.Prefixes(1), rep.Totals.MemoryBytes, opt.Top)
	rep.Patterns = toRows(patterns, rep.Totals.MemoryBytes, opt.Top)
	return rep
}

// combineSources describes all inputs as one source. With one shard it is that
// shard's source. With several: sizes and used-mem are summed (only if every
// shard has them, so the overhead isn't computed from partial numbers), the
// snapshot time is the earliest, and versions are kept only if all agree.
func combineSources(shards []*shardResult) Source {
	if len(shards) == 1 {
		return shards[0].src
	}
	out := Source{File: fmt.Sprintf("%d shards", len(shards))}
	first := shards[0].src
	sameRDB, sameRedis, allSizes, allUsed := true, true, true, true
	for _, sh := range shards {
		s := sh.src
		out.SizeBytes += s.SizeBytes
		out.UsedMemBytes += s.UsedMemBytes
		sameRDB = sameRDB && s.RDBVersion == first.RDBVersion
		sameRedis = sameRedis && s.RedisVersion == first.RedisVersion
		allSizes = allSizes && s.SizeBytes > 0
		allUsed = allUsed && s.UsedMemBytes > 0
		if s.SnapshotTime != nil && (out.SnapshotTime == nil || s.SnapshotTime.Before(*out.SnapshotTime)) {
			out.SnapshotTime = s.SnapshotTime
		}
	}
	if sameRDB {
		out.RDBVersion = first.RDBVersion
	}
	if sameRedis {
		out.RedisVersion = first.RedisVersion
	}
	if !allSizes {
		out.SizeBytes = 0
	}
	if !allUsed {
		out.UsedMemBytes = 0
	}
	return out
}

func toRows(in []pattern.Row, total int64, top int) []Row {
	if top > 0 && len(in) > top {
		in = in[:top]
	}
	out := make([]Row, 0, len(in)) // non-nil so JSON prints [] rather than null
	for _, r := range in {
		s := r.Stats
		row := Row{
			Pattern:     r.Pattern,
			Type:        r.Kind,
			Keys:        s.Keys,
			MemoryBytes: s.Bytes,
			KeysWithTTL: s.WithTTL,
			MaxKeyBytes: s.MaxBytes,
			MaxElements: s.MaxElems,
		}
		if total > 0 {
			row.Share = float64(s.Bytes) / float64(total)
		}
		if s.WithTTL > 0 {
			row.TTL = &TTL{MinMS: s.TTLMinMS, AvgMS: s.AvgTTLMS(), MaxMS: s.TTLMaxMS}
		}
		out = append(out, row)
	}
	return out
}
