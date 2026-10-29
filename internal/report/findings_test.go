package report

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ujjwalrastogi17/redislens/internal/rdbgen"
)

// findingsRDB plants one case of each finding, next to keys that must not be flagged.
func findingsRDB(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := rdbgen.NewWriter(&buf)
	w.Aux("ctime", strconv.Itoa(rdbgen.CTime))
	w.SelectDB(0, 0, 0)
	now := int64(rdbgen.CTime) * 1000
	val := func(n int) []byte { return bytes.Repeat([]byte("x"), n) }
	key := func(f string, i int) []byte { return []byte(fmt.Sprintf(f, i)) }

	for i := range 1000 { // ordinary data, recently used: no finding
		w.Idle(60)
		w.String(key("user:%d:profile", i), val(80), 0)
	}
	for i := range 200 { // "cache" without TTL
		w.Idle(60)
		w.String(key("cache:%d", i), val(1000), 0)
	}
	for i := range 200 { // mostly expiring, 20 forgot EXPIRE
		w.Idle(60)
		exp := now + 3600_000
		if i%10 == 0 {
			exp = 0
		}
		w.String(key("job:%d:result", i), val(3000), exp)
	}
	for i := range 2 { // big keys
		w.Idle(60)
		w.String(key("blob:%d", i), val(1100<<10), 0)
	}
	for i := range 200 { // untouched for 30 days
		w.Idle(30 * 24 * 3600)
		w.String(key("report:%d:pdf", i), val(1000), 0)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func findingSet(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Kind+" "+f.Pattern+f.Shard)
	}
	sort.Strings(out)
	return out
}

func TestFindings(t *testing.T) {
	rep, err := Analyze(Options{}, Input{R: bytes.NewReader(findingsRDB(t)), File: "x.rdb"})
	if err != nil {
		t.Fatal(err)
	}
	got := findingSet(rep.Findings)
	want := []string{
		"big_keys blob:{num}",
		"cold report:{num}:pdf",
		"no_ttl cache:{num}",
		"no_ttl job:{num}:result",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings:\n%s\nwant:\n%s\nfull: %+v", strings.Join(got, "\n"), strings.Join(want, "\n"), rep.Findings)
	}
	for _, f := range rep.Findings {
		switch f.Kind {
		case FindingNoTTL:
			if f.Pattern == "job:{num}:result" && f.Keys != 20 {
				t.Errorf("job: %d keys without TTL, want 20", f.Keys)
			}
		case FindingBigKeys:
			if f.Keys != 2 {
				t.Errorf("big keys: %d, want 2", f.Keys)
			}
		case FindingCold:
			if f.Keys != 200 {
				t.Errorf("cold keys: %d, want 200", f.Keys)
			}
		}
	}
	// Biggest problem first: the two 1.1MB blobs.
	if rep.Findings[0].Kind != FindingBigKeys {
		t.Errorf("first finding is %s", rep.Findings[0].Kind)
	}

	// Raising the thresholds removes the big-key and cold findings.
	rep, err = Analyze(Options{BigKeyBytes: 2 << 20, ColdAfter: 60 * 24 * 3600e9},
		Input{R: bytes.NewReader(findingsRDB(t))})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Kind == FindingBigKeys || f.Kind == FindingCold {
			t.Errorf("unexpected %s finding with higher thresholds", f.Kind)
		}
	}
}

// Without LRU idle times the cold check can't run, and the report says so.
func TestFindingsNoIdleData(t *testing.T) {
	rep, err := Analyze(Options{}, Input{R: bytes.NewReader(genRDB(t, 10_000, 1))})
	if err != nil {
		t.Fatal(err)
	}
	last := rep.Findings[len(rep.Findings)-1]
	if last.Kind != FindingNoIdle || last.Severity != "info" {
		t.Errorf("want a trailing no_idle_data note, got %+v", rep.Findings)
	}
}

func TestShardSkew(t *testing.T) {
	big, small := genRDB(t, 100_000, 1), genRDB(t, 20_000, 2)
	rep, err := Analyze(Options{},
		Input{R: bytes.NewReader(big), File: "node1"},
		Input{R: bytes.NewReader(small), File: "node2"},
		Input{R: bytes.NewReader(small), File: "node3"})
	if err != nil {
		t.Fatal(err)
	}
	var skewed []string
	for _, f := range rep.Findings {
		if f.Kind == FindingShardSkew {
			skewed = append(skewed, f.Shard)
		}
	}
	if len(skewed) != 1 || skewed[0] != "node1" {
		t.Errorf("skewed shards %v, want [node1]", skewed)
	}
}

func TestVolatileWord(t *testing.T) {
	for pat, want := range map[string]string{
		"cache:{num}":           "cache",
		"api:v1/Cache/{hex}":    "cache",
		"user_session:{uuid}":   "session",
		"lock.order:{num}":      "lock",
		"user:{num}:profile":    "",
		"cachet:{num}":          "", // whole words only
		"report:{num}:tmpfiles": "",
	} {
		if got := volatileWord(pat); got != want {
			t.Errorf("volatileWord(%q) = %q, want %q", pat, got, want)
		}
	}
}
