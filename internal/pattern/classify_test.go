package pattern

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"cart:123:items", "cart:{num}:items"},
		{"user:42:profile", "user:{num}:profile"},
		{"session:3f2a1b4c-0d9e-4a7b-a1c2-123456789abc", "session:{uuid}"},
		{"leaderboard:2026-10-27", "leaderboard:{date}"},
		{"daily:20261027", "daily:{date}"},
		{"evt:1793037950", "evt:{ts}"},
		{"evt:1793037950123", "evt:{ts}"},
		{"user:alice@example.com", "user:{email}"},
		{"rate:10.0.0.1", "rate:{ip}"},
		{"{user42}:cart", "{tag}:cart"},
		{"blob:ab12cd34ef56", "blob:{hex}"},
		{"sku-42", "sku-{num}"},
		{"api/v1/users/7", "api/v1/users/{num}"},
		{"config", "config"},
		{"a::b", "a::b"},
		{"deadbeef", "deadbeef"}, // no digits: treated as a word
		{"ver:-1", "ver:{num}"},
		{"", ""},
		{"x:1.2.3.4.5", "x:1.2.3.4.5"}, // too many octets
		{"x:1.2.3.", "x:1.2.3."},
		{"x:1..2.3", "x:1..2.3"},
		{"x:256.1.1.1", "x:256.1.1.1"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := Normalize([]byte(tt.in)); got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSegmentsRoundTrip(t *testing.T) {
	for _, s := range []string{"cart:{num}:items", "a/b|c", "plain", "x:"} {
		if got := strings.Join(Segments(s), ""); got != s {
			t.Errorf("Segments(%q) joined = %q", s, got)
		}
	}
}

// Benchmarks: run with `go test -bench . ./internal/pattern`.
func BenchmarkNormalize(b *testing.B) {
	key := []byte("session:3f2a1b4c-0d9e-4a7b-a1c2-123456789abc:cart:123:items")
	b.ReportAllocs()
	var buf []byte
	for b.Loop() {
		buf = AppendNormalized(buf[:0], key)
	}
}
