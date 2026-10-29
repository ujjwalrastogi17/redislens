package rdb

import (
	"os"
	"testing"
)

// TestPrintEstimates prints our estimate for a few known keys so they can be
// compared with `redis-cli MEMORY USAGE <key> SAMPLES 0`. Run with:
//
//	REDISLENS_DEBUG_KEYS=1 go test -run PrintEstimates -v ./internal/rdb
func TestPrintEstimates(t *testing.T) {
	if os.Getenv("REDISLENS_DEBUG_KEYS") == "" {
		t.Skip("set REDISLENS_DEBUG_KEYS=1")
	}
	want := map[string]bool{
		"user:1:profile": true, "cart:1:items": true, "feed:1:events": true,
		"tags:4": true, "leaderboard:2026-10-02": true, "big:blob:1000": true,
		"bighash:1": true, "bigset:1": true, "bigzset:1": true, "biglist:1": true,
		"ids:100": true, "stream:500": true, "stream:5000": true,
	}
	f, err := os.Open(fixture)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	err = Walk(f, nil, func(e *Entry) error {
		k := string(e.Key)
		if want[k] || (len(k) == 44 && k[:8] == "session:" && want["session"] == false) {
			if k[:3] == "ses" {
				want["session"] = true
			}
			t.Logf("%-26s type=%-2d file=%-6d est=%d", k, e.Type, e.ValueBytes, e.MemBytes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
