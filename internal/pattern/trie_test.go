package pattern

import (
	"reflect"
	"testing"
)

func TestTrie(t *testing.T) {
	tr := NewTrie(0)
	tr.Insert([]byte("user:{num}:profile"), "string", Key{Bytes: 100})
	tr.Insert([]byte("user:{num}:profile"), "string", Key{Bytes: 50})
	tr.Insert([]byte("user:{num}:settings"), "hash", Key{Bytes: 30, HasTTL: true, TTLMS: 1000})
	tr.Insert([]byte("user:{num}:settings"), "hash", Key{Bytes: 10, HasTTL: true, TTLMS: 3000})
	tr.Insert([]byte("user:{num}"), "string", Key{Bytes: 5}) // ends at an inner node
	tr.Insert([]byte("config"), "string", Key{Bytes: 7})
	tr.Insert([]byte("user:{num}:profile"), "hash", Key{Bytes: 1}) // same pattern, other type

	t.Run("patterns", func(t *testing.T) {
		got := tr.Patterns()
		want := []Row{
			{"user:{num}:profile", "string", Stats{Keys: 2, Bytes: 150, MaxBytes: 100}},
			{"user:{num}:settings", "hash", Stats{Keys: 2, Bytes: 40, WithTTL: 2, TTLMinMS: 1000, TTLMaxMS: 3000, TTLSumMS: 4000, TTLBytes: 40, MaxBytes: 30}},
			{"config", "string", Stats{Keys: 1, Bytes: 7, MaxBytes: 7}},
			{"user:{num}", "string", Stats{Keys: 1, Bytes: 5, MaxBytes: 5}},
			{"user:{num}:profile", "hash", Stats{Keys: 1, Bytes: 1, MaxBytes: 1}},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d rows %+v, want %d", len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("prefixes depth 1", func(t *testing.T) {
		got := tr.Prefixes(1)
		if len(got) != 2 || got[0].Pattern != "user:" || got[0].Stats.Keys != 6 || got[0].Stats.Bytes != 196 {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("total", func(t *testing.T) {
		s := tr.Total()
		if s.Keys != 7 || s.Bytes != 203 || s.WithTTL != 2 || s.AvgTTLMS() != 2000 {
			t.Errorf("got %+v", s)
		}
	})
}

func TestCollapse(t *testing.T) {
	tr := NewTrie(3)
	// 10 distinct usernames: the classifier can't tell these are IDs.
	for _, name := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy"} {
		tr.Insert([]byte("user:"+name+":cart"), "hash", Key{Bytes: 10})
		tr.Insert([]byte("user:"+name+":profile"), "string", Key{Bytes: 1})
	}
	tr.Insert([]byte("user:{num}:cart"), "hash", Key{Bytes: 100}) // placeholders are never collapsed
	tr.Insert([]byte("user:*:cart"), "hash", Key{Bytes: 5})       // pattern already containing a wildcard
	tr.Insert([]byte("user:zed"), "string", Key{Bytes: 2})        // literal leaf after collapse

	got := map[string]Stats{}
	for _, r := range tr.Patterns() {
		got[r.Pattern+" "+r.Kind] = r.Stats
	}
	want := map[string]Stats{
		"user:*:cart hash":      {Keys: 11, Bytes: 105, MaxBytes: 10},
		"user:*:profile string": {Keys: 10, Bytes: 10, MaxBytes: 1},
		"user:{num}:cart hash":  {Keys: 1, Bytes: 100, MaxBytes: 100},
		"user:* string":         {Keys: 1, Bytes: 2, MaxBytes: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}
	if s := tr.Total(); s.Keys != 23 {
		t.Errorf("total keys %d, want 23 (collapse must not lose keys)", s.Keys)
	}
}

// Nested collapse: collapsing a parent merges children that are themselves wide.
func TestCollapseMergesWideSubtrees(t *testing.T) {
	tr := NewTrie(3)
	for i, a := range []string{"a", "b", "c", "d"} {
		for _, b := range []string{"p", "q", "r", "s", "u"} {
			tr.Insert([]byte(a+":"+b+string(rune('0'+i))), "string", Key{Bytes: 1})
		}
	}
	rows := tr.Patterns()
	if len(rows) != 1 || rows[0].Pattern != "*:*" || rows[0].Stats.Keys != 20 {
		t.Errorf("got %+v", rows)
	}
}

// Splitting keys across tries and merging them must give the same patterns as
// inserting everything into one trie, whichever trie collapses first.
func TestMergeMatchesSingleTrie(t *testing.T) {
	var keys []string
	for i := 0; i < 40; i++ {
		name := string(rune('a'+i%26)) + string(rune('a'+i/26)) // 40 distinct literals
		keys = append(keys, "user:"+name+":cart", "user:"+name+":"+name, "grp:"+name)
	}
	keys = append(keys, "user:{num}:cart", "solo")

	want := NewTrie(5)
	for _, k := range keys {
		want.Insert([]byte(k), "hash", Key{Bytes: 1})
	}

	splits := map[string]func(i int) int{
		"round-robin": func(i int) int { return i % 3 },
		"one-heavy":   func(i int) int { return min(i/100, 2) }, // trie 0 collapses, others may not
		"few-in-each": func(i int) int { return i % 30 },        // no trie collapses on its own
	}
	for name, which := range splits {
		t.Run(name, func(t *testing.T) {
			var parts []*Trie
			for i, k := range keys {
				for which(i) >= len(parts) {
					parts = append(parts, NewTrie(5))
				}
				parts[which(i)].Insert([]byte(k), "hash", Key{Bytes: 1})
			}
			got := parts[0]
			for _, p := range parts[1:] {
				got.Merge(p)
			}
			if g, w := got.Patterns(), want.Patterns(); !reflect.DeepEqual(g, w) {
				t.Errorf("merged:\n%v\nsingle:\n%v", g, w)
			}
		})
	}
}

func TestConsolidate(t *testing.T) {
	tr := NewTrie(0)
	add := func(tmpl string, n int) {
		for range n {
			tr.Insert([]byte(tmpl), "string", Key{Bytes: 1})
		}
	}
	add("user:{num}:profile", 1000)
	add("user:{date}:profile", 10) // 8-digit IDs that look like dates
	add("user:{ts}:profile", 5)
	add("user:{uuid}:profile", 10) // different kind of ID: kept
	add("user:{date}", 3)          // different separator: kept
	add("evt:{ts}", 100)
	add("evt:{num}", 100) // not dominated: kept
	add("cache/{hex}", 1000)
	add("cache/{num}", 20)
	add("cache/bafeecafbcba", 1) // hex ID with no digits: folded, so the raw value disappears
	add("cache/config", 1)       // ordinary word: kept
	add("cfg:{num}", 1000)
	add("cfg:deadbeef", 1) // a hex word only folds into {hex}, not {num}
	tr.Consolidate()

	got := map[string]int64{}
	for _, r := range tr.Patterns() {
		got[r.Pattern] = r.Stats.Keys
	}
	want := map[string]int64{
		"user:{num}:profile":  1015,
		"user:{uuid}:profile": 10,
		"user:{date}":         3,
		"evt:{ts}":            100,
		"evt:{num}":           100,
		"cache/{hex}":         1021,
		"cache/config":        1,
		"cfg:{num}":           1000,
		"cfg:deadbeef":        1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func BenchmarkTrieInsert(b *testing.B) {
	tr := NewTrie(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tr.Insert([]byte("session:{uuid}:cart:{num}:items"), "hash", Key{Bytes: 100})
	}
}
