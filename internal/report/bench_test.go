package report

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/ujjwalrastogi17/redislens/internal/pattern"
	"github.com/ujjwalrastogi17/redislens/internal/rdb"
	"github.com/ujjwalrastogi17/redislens/internal/rdbgen"
)

// The benchmarks read a generated 1M-key snapshot from memory, so they
// measure parsing and analysis, not the disk. For the 20M-key end-to-end
// number see "Benchmarks" in CLAUDE.md.
const benchKeys = 1_000_000

var benchData = sync.OnceValue(func() []byte {
	var buf bytes.Buffer
	if err := rdbgen.Generate(&buf, benchKeys, 1); err != nil {
		panic(err)
	}
	return buf.Bytes()
})

func reportKeysPerSec(b *testing.B) {
	b.ReportMetric(float64(benchKeys)*float64(b.N)/b.Elapsed().Seconds(), "keys/s")
}

// BenchmarkWalk: parsing only (values skipped, keys discarded).
func BenchmarkWalk(b *testing.B) {
	data := benchData()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if err := rdb.Walk(bytes.NewReader(data), nil, func(*rdb.Entry) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
	reportKeysPerSec(b)
}

// BenchmarkWalkNormalize: parsing plus key -> template.
func BenchmarkWalkNormalize(b *testing.B) {
	var tmpl []byte
	data := benchData()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		err := rdb.Walk(bytes.NewReader(data), nil, func(e *rdb.Entry) error {
			tmpl = pattern.AppendNormalized(tmpl[:0], e.Key)
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	reportKeysPerSec(b)
}

// BenchmarkAnalyze: the full pipeline behind "redislens analyze", by worker count.
func BenchmarkAnalyze(b *testing.B) {
	data := benchData()
	for _, w := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("workers=%d", w), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Analyze(Options{Workers: w}, Input{R: bytes.NewReader(data)}); err != nil {
					b.Fatal(err)
				}
			}
			reportKeysPerSec(b)
		})
	}
}
