package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ujjwalrastogi17/redislens/internal/pattern"
)

// Finding is one problem worth acting on. Like the rest of the report it names
// patterns and shards, never raw keys.
type Finding struct {
	Kind        string `json:"kind"`     // see the Finding* constants
	Severity    string `json:"severity"` // "warn" or "info"
	Pattern     string `json:"pattern,omitempty"`
	Type        string `json:"type,omitempty"`
	Shard       string `json:"shard,omitempty"`
	Keys        int64  `json:"keys,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"` // memory the finding is about
	Message     string `json:"message"`
}

const (
	FindingNoTTL      = "no_ttl"     // keys that should expire but don't
	FindingBigKeys    = "big_keys"   // single keys over the big-key threshold
	FindingCold       = "cold"       // memory not accessed for a long time
	FindingShardSkew  = "shard_skew" // one shard holds much more than the others
	FindingShardTimes = "shard_times"
	FindingNoIdle     = "no_idle_data" // cold check impossible for this snapshot
)

// minShare: no-TTL and cold findings must cover at least this fraction of key
// memory, so the list isn't flooded with patterns of a few keys.
const minShare = 0.01

// skewFactor: a shard this many times the average shard memory is skewed.
const skewFactor = 1.25

// shardTimeGap: shard snapshots further apart than this are flagged, because
// their numbers describe different moments.
const shardTimeGap = time.Hour

// volatileWords are pattern words that suggest data meant to expire.
var volatileWords = map[string]bool{
	"cache": true, "cached": true, "session": true, "sessions": true, "sess": true,
	"tmp": true, "temp": true, "lock": true, "locks": true, "mutex": true,
	"otp": true, "token": true, "tokens": true, "nonce": true, "ratelimit": true,
	"throttle": true, "captcha": true, "idempotency": true,
}

func findings(patterns []pattern.Row, shards []*shardResult, rep *Report, opt Options) []Finding {
	var out []Finding
	total := rep.Totals.MemoryBytes
	significant := func(bytes int64) bool { return total > 0 && float64(bytes) >= minShare*float64(total) }

	for _, p := range patterns {
		s := p.Stats
		noTTLKeys, noTTLBytes := s.Keys-s.WithTTL, s.Bytes-s.TTLBytes

		// No TTL: by name, or because most keys of the pattern do expire.
		if noTTLKeys > 0 && significant(noTTLBytes) {
			msg := ""
			if w := volatileWord(p.Pattern); w != "" {
				msg = fmt.Sprintf("%s (%s) never expire, but %q suggests temporary data",
					count(noTTLKeys, "key"), HumanBytes(noTTLBytes), w)
			} else if s.WithTTL*2 >= s.Keys {
				msg = fmt.Sprintf("%d of %s (%s) have no TTL while the rest expire; likely a missing EXPIRE",
					noTTLKeys, count(s.Keys, "key"), HumanBytes(noTTLBytes))
			}
			if msg != "" {
				out = append(out, Finding{Kind: FindingNoTTL, Severity: "warn", Pattern: p.Pattern, Type: p.Kind,
					Keys: noTTLKeys, MemoryBytes: noTTLBytes, Message: msg})
			}
		}

		// Big keys are individually large, so any count is worth reporting.
		if s.BigKeys > 0 {
			msg := fmt.Sprintf("%s of %s+ (or %d+ elements); largest %s",
				count(s.BigKeys, "key"), HumanBytes(opt.BigKeyBytes), opt.BigKeyElems, HumanBytes(s.MaxBytes))
			if s.MaxElems > 0 {
				msg += fmt.Sprintf(", up to %d elements", s.MaxElems)
			}
			out = append(out, Finding{Kind: FindingBigKeys, Severity: "warn", Pattern: p.Pattern, Type: p.Kind,
				Keys: s.BigKeys, MemoryBytes: s.BigBytes, Message: msg})
		}

		if s.ColdKeys > 0 && significant(s.ColdBytes) {
			out = append(out, Finding{Kind: FindingCold, Severity: "warn", Pattern: p.Pattern, Type: p.Kind,
				Keys: s.ColdKeys, MemoryBytes: s.ColdBytes,
				Message: fmt.Sprintf("%s (%s, %.0f%% of this pattern) not accessed for %s or more",
					count(s.ColdKeys, "key"), HumanBytes(s.ColdBytes), 100*float64(s.ColdBytes)/float64(s.Bytes),
					humanDuration(opt.ColdAfter.Milliseconds()))})
		}
	}

	out = append(out, shardFindings(rep.Shards)...)
	// Warnings first, then by memory, then by name, so the order is stable.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Severity != b.Severity {
			return a.Severity == "warn"
		}
		if a.MemoryBytes != b.MemoryBytes {
			return a.MemoryBytes > b.MemoryBytes
		}
		return a.Pattern+a.Shard+a.Kind < b.Pattern+b.Shard+b.Kind
	})
	if n := noIdleNote(shards); n != nil {
		out = append(out, *n)
	}
	if out == nil {
		out = []Finding{} // JSON [] rather than null
	}
	return out
}

// count formats "1 key" / "3 keys".
func count(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// volatileWord returns the first literal word of pat that suggests temporary data.
func volatileWord(pat string) string {
	words := strings.FieldsFunc(strings.ToLower(pat), func(r rune) bool {
		return strings.ContainsRune(":/|#,-_.", r)
	})
	for _, w := range words {
		if volatileWords[w] {
			return w
		}
	}
	return ""
}

func shardFindings(shards []Shard) []Finding {
	if len(shards) < 2 {
		return nil
	}
	var out []Finding
	var sum int64
	var first, last *time.Time
	for _, s := range shards {
		sum += s.MemoryBytes
		if t := s.SnapshotTime; t != nil {
			if first == nil || t.Before(*first) {
				first = t
			}
			if last == nil || t.After(*last) {
				last = t
			}
		}
	}
	avg := float64(sum) / float64(len(shards))
	for _, s := range shards {
		if avg > 0 && float64(s.MemoryBytes) >= skewFactor*avg {
			out = append(out, Finding{Kind: FindingShardSkew, Severity: "warn", Shard: s.File,
				Keys: s.Keys, MemoryBytes: s.MemoryBytes,
				Message: fmt.Sprintf("%s of key memory vs %s average over %d shards (+%.0f%%); check for hot keys or hash tags",
					HumanBytes(s.MemoryBytes), HumanBytes(int64(avg)), len(shards), 100*(float64(s.MemoryBytes)/avg-1))})
		}
	}
	if first != nil && last.Sub(*first) > shardTimeGap {
		out = append(out, Finding{Kind: FindingShardTimes, Severity: "info",
			Message: fmt.Sprintf("shard snapshots were taken up to %s apart, so totals mix different moments",
				humanDuration(last.Sub(*first).Milliseconds()))})
	}
	return out
}

// noIdleNote explains a missing cold check: Redis only saves LRU idle time when
// maxmemory-policy is an LRU policy.
func noIdleNote(shards []*shardResult) *Finding {
	var idle, lfu int64
	for _, sh := range shards {
		idle += sh.idleKeys
		lfu += sh.lfuKeys
	}
	if idle > 0 {
		return nil
	}
	msg := "no LRU idle times in the snapshot, so cold memory can't be measured; Redis saves them only with an *-lru maxmemory-policy"
	if lfu > 0 {
		msg = "snapshot has LFU counters, not LRU idle times, so cold memory can't be measured in time; use an *-lru maxmemory-policy for this check"
	}
	return &Finding{Kind: FindingNoIdle, Severity: "info", Message: msg}
}
