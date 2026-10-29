package rdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hdt3213/rdb/parser"

	"github.com/ujjwalrastogi17/redislens/internal/rdbgen"
)

const fixture = "../../testdata/dump.rdb"

// TestAgainstOracle parses the synthetic snapshot with both our parser and
// hdt3213/rdb, and checks every key agrees on db, type and expiry.
// Generate the fixture with scripts/gen-testdata.sh.
func TestAgainstOracle(t *testing.T) {
	if _, err := os.Stat(fixture); err != nil {
		t.Skip("no fixture; run scripts/gen-testdata.sh")
	}
	compareWithOracle(t, fixture)
}

// TestGeneratorAgainstOracle checks that rdbgen (used for the 20M-key
// benchmark) writes files that a third-party parser reads the same way we do.
func TestGeneratorAgainstOracle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gen.rdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdbgen.Generate(f, 50_000, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	compareWithOracle(t, path)
}

func compareWithOracle(t *testing.T, path string) {
	t.Helper()

	type info struct {
		db       int
		kind     string
		expireMS int64
	}
	ours := map[string]info{}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = Walk(f, nil, func(e *Entry) error {
		ours[string(e.Key)] = info{e.DB, string(e.Type.Kind()), e.ExpireMS}
		return nil
	})
	if err != nil {
		t.Fatalf("our parser: %v", err)
	}

	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	theirs := map[string]info{}
	err = parser.NewDecoder(f2).Parse(func(o parser.RedisObject) bool {
		switch o.GetType() {
		case parser.StringType, parser.ListType, parser.SetType, parser.HashType,
			parser.ZSetType, parser.StreamType:
		default:
			return true // aux, db size, etc.
		}
		var exp int64
		if e := o.GetExpiration(); e != nil {
			exp = e.UnixMilli()
		}
		theirs[o.GetKey()] = info{o.GetDBIndex(), o.GetType(), exp}
		return true
	})
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}

	if len(ours) != len(theirs) {
		t.Errorf("key count: ours %d, oracle %d", len(ours), len(theirs))
	}
	mismatches := 0
	for k, want := range theirs {
		got, ok := ours[k]
		if !ok || got != want {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("key %q: ours %+v (present=%v), oracle %+v", k, got, ok, want)
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("%d mismatched keys", mismatches)
	}
	t.Logf("compared %d keys", len(theirs))
}
