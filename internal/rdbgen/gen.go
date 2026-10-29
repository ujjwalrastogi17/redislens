package rdbgen

import (
	"io"
	"math/rand/v2"
	"strconv"
)

// CTime is the snapshot time written to generated files (2026-10-01T00:00:00Z),
// fixed so TTL stats are reproducible.
const CTime = 1790812800

// Generate writes a deterministic synthetic snapshot with n keys to out.
// The mix mirrors scripts/gen-testdata.sh (sessions with TTLs, profiles,
// carts, feeds, ...), plus keys the classifier can't detect (article slugs)
// so the trie's collapse path is exercised at scale.
func Generate(out io.Writer, n int, seed uint64) error {
	w := NewWriter(out)
	w.Aux("redis-ver", "7.4.0")
	w.Aux("redis-bits", "64")
	w.Aux("ctime", strconv.Itoa(CTime))
	w.Aux("aof-base", "0")
	w.SelectDB(0, uint64(n), 0)

	g := gen{rng: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))}
	const nowMS = CTime * 1000
	for i := 0; i < n && w.Err() == nil; i++ {
		g.key = g.key[:0]
		switch p := g.rng.IntN(100); {
		case p < 25:
			g.lit("session:").uuid()
			w.String(g.key, g.val("uid=", i), nowMS+int64(1800_000+g.rng.IntN(5400_000)))
		case p < 45:
			g.lit("user:").num(i).lit(":profile")
			w.String(g.key, g.val("name=user", i, ";plan=free;country=IN"), 0)
		case p < 60:
			g.lit("cart:").num(i).lit(":items")
			w.Hash(g.key, g.pairs(1+g.rng.IntN(3), "sku-", 500), 0)
		case p < 72:
			g.lit("feed:").num(i).lit(":events")
			w.List(g.key, g.pick(3+g.rng.IntN(4), events), 0)
		case p < 79:
			g.lit("tags:").num(i)
			w.Set(g.key, g.pick(2+g.rng.IntN(3), colors), 0)
		case p < 83:
			g.lit("ids:").num(i)
			x := int16(i % 997) // keeps the three values sorted and distinct
			w.IntSet(g.key, []int16{1, 2 + x, 1000 + x}, 0)
		case p < 88:
			g.lit("article:").slug(6).lit(":views")
			w.IntString(g.key, int32(g.rng.IntN(100000)), 0)
		case p < 94:
			g.lit("rate:").ip().lit(":").num(CTime + g.rng.IntN(60))
			w.IntString(g.key, int32(1+g.rng.IntN(100)), nowMS+int64(1000+g.rng.IntN(59_000)))
		case p < 97:
			g.lit("leaderboard:2026-09-").num2(1 + g.rng.IntN(30))
			w.ZSet(g.key, g.pairs(5, "user:", 1_000_000), 0)
		case p < 99:
			g.lit("order:").hex(16).lit(":status")
			w.String(g.key, []byte("shipped"), 0)
		default:
			if i%100 == 0 {
				g.lit("big:blob:").num(i)
				w.String(g.key, g.blob(50_000), 0)
				continue
			}
			g.lit("api:v1/cache/").hex(12)
			w.String(g.key, g.blob(200), nowMS+3600_000)
		}
	}
	return w.Close()
}

var (
	events = [][]byte{[]byte("login"), []byte("view"), []byte("click"), []byte("cart"), []byte("buy"), []byte("logout")}
	colors = [][]byte{[]byte("red"), []byte("green"), []byte("blue"), []byte("black"), []byte("white")}
)

// gen builds keys and values in reused buffers; nothing allocates per key.
type gen struct {
	rng   *rand.Rand
	key   []byte
	v     []byte
	elems [][]byte
	ebuf  []byte
}

func (g *gen) lit(s string) *gen { g.key = append(g.key, s...); return g }
func (g *gen) num(n int) *gen    { g.key = strconv.AppendInt(g.key, int64(n), 10); return g }

func (g *gen) num2(n int) *gen {
	if n < 10 {
		g.key = append(g.key, '0')
	}
	return g.num(n)
}

const hexDigits = "0123456789abcdef"

func (g *gen) hex(n int) *gen {
	for range n {
		g.key = append(g.key, hexDigits[g.rng.IntN(16)])
	}
	return g
}

func (g *gen) uuid() *gen {
	return g.hex(8).lit("-").hex(4).lit("-4").hex(3).lit("-a").hex(3).lit("-").hex(12)
}

func (g *gen) slug(n int) *gen {
	for range n {
		g.key = append(g.key, byte('a'+g.rng.IntN(26)))
	}
	return g
}

func (g *gen) ip() *gen {
	return g.lit("10.").num(g.rng.IntN(256)).lit(".").num(g.rng.IntN(256)).lit(".").num(1 + g.rng.IntN(254))
}

func (g *gen) val(prefix string, n int, suffix ...string) []byte {
	g.v = strconv.AppendInt(append(g.v[:0], prefix...), int64(n), 10)
	for _, s := range suffix {
		g.v = append(g.v, s...)
	}
	return g.v
}

func (g *gen) blob(n int) []byte {
	g.v = g.v[:0]
	for range n {
		g.v = append(g.v, byte('a'+g.rng.IntN(26)))
	}
	return g.v
}

// pairs returns n (prefix+id, small number) pairs, flattened.
func (g *gen) pairs(n int, prefix string, ids int) [][]byte {
	g.ebuf, g.elems = g.ebuf[:0], g.elems[:0]
	var offs []int
	for range n {
		start := len(g.ebuf)
		g.ebuf = strconv.AppendInt(append(g.ebuf, prefix...), int64(g.rng.IntN(ids)), 10)
		mid := len(g.ebuf)
		g.ebuf = strconv.AppendInt(g.ebuf, int64(1+g.rng.IntN(9999)), 10)
		offs = append(offs, start, mid, len(g.ebuf))
	}
	// Slice after appending: ebuf may have moved while growing.
	for j := 0; j < len(offs); j += 3 {
		g.elems = append(g.elems, g.ebuf[offs[j]:offs[j+1]], g.ebuf[offs[j+1]:offs[j+2]])
	}
	return g.elems
}

// pick returns n distinct entries from choices (n <= len(choices)).
func (g *gen) pick(n int, choices [][]byte) [][]byte {
	g.elems = g.elems[:0]
	start := g.rng.IntN(len(choices))
	for j := range n {
		g.elems = append(g.elems, choices[(start+j)%len(choices)])
	}
	return g.elems
}
