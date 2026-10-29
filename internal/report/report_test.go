package report

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ujjwalrastogi17/redislens/internal/rdbgen"
)

const fixture = "../../testdata/dump.rdb"

func analyzeFixture(t *testing.T) *Report {
	t.Helper()
	f, err := os.Open(fixture)
	if err != nil {
		t.Skip("no fixture; run scripts/gen-testdata.sh")
	}
	defer f.Close()
	rep, err := Analyze(Options{Top: 5, ToolVersion: "test"}, Input{R: f, File: "dump.rdb"})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// The JSON schema is a public contract: these field names must not change
// without bumping SchemaVersion.
func TestJSONSchema(t *testing.T) {
	rep := analyzeFixture(t)
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "tool", "source", "totals", "types", "prefixes", "patterns"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level field %q", k)
		}
	}
	if m["schema_version"] != float64(SchemaVersion) {
		t.Errorf("schema_version = %v", m["schema_version"])
	}
	row := m["patterns"].([]any)[0].(map[string]any)
	for _, k := range []string{"pattern", "type", "keys", "memory_bytes", "share", "keys_with_ttl"} {
		if _, ok := row[k]; !ok {
			t.Errorf("pattern row missing %q", k)
		}
	}
	if len(rep.Patterns) != 5 {
		t.Errorf("Top=5 gave %d patterns", len(rep.Patterns))
	}
}

// Privacy: no raw key may ever appear in the output.
func TestNoRawKeysInOutput(t *testing.T) {
	f, err := os.Open(fixture)
	if err != nil {
		t.Skip("no fixture; run scripts/gen-testdata.sh")
	}
	defer f.Close()
	rep, err := Analyze(Options{}, Input{R: f}) // Top 0 = every row
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if err := WriteText(&buf, rep); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, raw := range []string{"user:1:profile", "cart:42:items", "big:blob:1000", "sku-"} {
		if strings.Contains(out, raw) {
			t.Errorf("output contains raw key fragment %q", raw)
		}
	}
}

// The parallel pipeline must give exactly the sequential result, including
// which literals collapse into "*" (small MaxChildren forces many collapses).
func TestWorkersDoNotChangeResult(t *testing.T) {
	var buf bytes.Buffer
	if err := rdbgen.Generate(&buf, 200_000, 7); err != nil {
		t.Fatal(err)
	}
	for _, maxChildren := range []int{0, 20} {
		analyze := func(workers int) *Report {
			rep, err := Analyze(Options{MaxChildren: maxChildren, Workers: workers}, Input{R: bytes.NewReader(buf.Bytes())})
			if err != nil {
				t.Fatal(err)
			}
			return rep
		}
		want := analyze(1)
		for _, w := range []int{2, 8} {
			if got := analyze(w); !reflect.DeepEqual(got, want) {
				t.Errorf("maxChildren=%d workers=%d: report differs from workers=1\ngot  %+v\nwant %+v",
					maxChildren, w, got.Patterns, want.Patterns)
			}
		}
	}
}

func genRDB(t *testing.T, keys int, seed uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := rdbgen.Generate(&buf, keys, seed); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Shards: totals add up, every shard gets a row, and input order doesn't
// change anything but the order of the shard rows.
func TestShards(t *testing.T) {
	a, b := genRDB(t, 100_000, 1), genRDB(t, 50_000, 2)
	in := func(data []byte, name string) Input { return Input{R: bytes.NewReader(data), File: name} }
	analyze := func(inputs ...Input) *Report {
		t.Helper()
		rep, err := Analyze(Options{}, inputs...)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	repA, repB := analyze(in(a, "a")), analyze(in(b, "b"))
	ab := analyze(in(a, "a"), in(b, "b"))
	ba := analyze(in(b, "b"), in(a, "a"))

	if ab.Totals.Keys != repA.Totals.Keys+repB.Totals.Keys ||
		ab.Totals.MemoryBytes != repA.Totals.MemoryBytes+repB.Totals.MemoryBytes {
		t.Errorf("totals don't add up: %+v vs %+v + %+v", ab.Totals, repA.Totals, repB.Totals)
	}
	if len(ab.Shards) != 2 || ab.Shards[0].File != "a" || ab.Shards[0].Keys != repA.Totals.Keys ||
		ab.Shards[1].Keys != repB.Totals.Keys {
		t.Errorf("shard rows: %+v", ab.Shards)
	}
	if share := ab.Shards[0].Share + ab.Shards[1].Share; share < 0.999 || share > 1.001 {
		t.Errorf("shard shares sum to %v", share)
	}
	if ab.Source.File != "2 shards" || ab.Source.RDBVersion != 12 {
		t.Errorf("combined source: %+v", ab.Source)
	}
	// A pattern spread over both shards is one row with the keys of both.
	keys := func(r *Report) map[string]int64 {
		m := map[string]int64{}
		for _, p := range r.Patterns {
			m[p.Pattern+" "+p.Type] = p.Keys
		}
		return m
	}
	kA, kB := keys(repA), keys(repB)
	for p, n := range keys(ab) {
		if n != kA[p]+kB[p] {
			t.Errorf("%s: %d keys, want %d + %d", p, n, kA[p], kB[p])
		}
	}
	if !reflect.DeepEqual(ab.Patterns, ba.Patterns) || !reflect.DeepEqual(ab.Totals, ba.Totals) ||
		!reflect.DeepEqual(ab.Types, ba.Types) || !reflect.DeepEqual(ab.Prefixes, ba.Prefixes) {
		t.Error("shard order changed the report")
	}
}

// A broken shard fails the run, named in the error.
func TestShardErrorNamesFile(t *testing.T) {
	good := genRDB(t, 50_000, 1)
	bad := good[:len(good)/2] // truncated
	_, err := Analyze(Options{},
		Input{R: bytes.NewReader(good), File: "good.rdb"},
		Input{R: bytes.NewReader(bad), File: "bad.rdb"})
	if err == nil || !strings.HasPrefix(err.Error(), "bad.rdb: ") {
		t.Fatalf("got %v, want an error naming bad.rdb", err)
	}
}

func TestCombineSources(t *testing.T) {
	t1 := time.Unix(100, 0).UTC()
	t2 := time.Unix(50, 0).UTC()
	got := combineSources([]*shardResult{
		{src: Source{File: "a", SizeBytes: 10, RDBVersion: 12, RedisVersion: "7.4.0", SnapshotTime: &t1, UsedMemBytes: 100}},
		{src: Source{File: "b", SizeBytes: 20, RDBVersion: 11, RedisVersion: "7.4.0", SnapshotTime: &t2}},
	})
	want := Source{File: "2 shards", SizeBytes: 30, RedisVersion: "7.4.0", SnapshotTime: &t2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}
