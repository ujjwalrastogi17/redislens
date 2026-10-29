# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands
```sh
go build -ldflags "-X main.version=v0.1.0" -o redislens ./cmd/redislens
./redislens analyze testdata/dump.rdb            # or: go run ./cmd/redislens analyze ...
go test ./...                                     # tests needing testdata/dump.rdb skip if absent
go test -run TestAgainstOracle -v ./internal/rdb  # single test
go vet ./... && gofmt -l .
./scripts/gen-testdata.sh [num_users]             # regenerate testdata/dump.rdb (needs Docker)
REDISLENS_DEBUG_KEYS=1 go test -run PrintEstimates -v ./internal/rdb  # per-key memory estimates
go run ./scripts/genrdb -keys 20000000 -o testdata/big.rdb   # 1.2GB synthetic RDB, no Docker
for i in 1 2 3 4; do go run ./scripts/genrdb -keys 5000000 -seed $i -o testdata/shards/node$i.rdb; done
go test -run '^$' -bench . ./internal/report                  # in-memory 1M-key benchmarks
./redislens analyze --cpuprofile cpu.prof --memprofile mem.prof testdata/big.rdb  # hidden flags
go test -race ./internal/report ./internal/pattern            # after touching the pipeline
./scripts/build-release.sh v0.1.0                             # release archives into dist/
```
Profile with `go test -bench ... -cpuprofile`, not the binary on a file: on macOS the
binary's CPU profile wrongly attributes most samples to `syscall.syscall`.

## What this is
An open-source Go CLI that analyzes Redis/Valkey RDB snapshots and reports memory usage
by auto-detected key pattern (e.g. `cart:{num}:items`), plus TTL hygiene, big keys,
cold memory (LRU idle time), and shard skew. Later: a paid SaaS that runs it continuously
(scheduled ElastiCache exports, trends, alerts, cost attribution).

## Key decisions (already made)
- Language: Go. Single static binary, no cgo.
- Input: local `.rdb` file first; `s3://` prefix later (SDK `manager.Downloader` to a 0700
  temp dir, delete on exit incl. Ctrl+C); stdin (`-`) supported from early on.
- Stream-parse; never hold raw keys in memory. Normalize each key to a template, aggregate
  into a pattern trie (counts, bytes, TTL stats), collapse high-cardinality nodes into `*`.
- Privacy: output contains only patterns and numbers, never raw keys or values.
- Write our own RDB parser (for learning); use github.com/hdt3213/rdb as a test oracle.
- Memory estimate = in-RAM estimate with per-key overhead, calibrated against `used-mem`.
- DuckDB/Parquet only as an optional `--export parquet` later, not the core path.
- License: Apache-2.0.

## How to work with me
- I'm new to Go. Go step by step; one concept per step; every step must end with
  something that runs.
- Split of work: Claude writes everything (plumbing and core: parser, classifier, trie)
  and explains the concepts and idiomatic Go as it goes. (Changed from the original
  "I write the core" split at the user's request.)
- Never use employer data or snapshots. Test only with synthetic data.

## Plan
Phase 1 – Go foundations
1. hello CLI: version + file path arg (modules, packages, go run/build, os.Args)
2. Test-data script: load ~100K patterned keys into local Redis, BGSAVE
3. Open RDB, read and print header (os.Open, defer, io.Reader, errors)

Phase 2 – RDB parser
4. Length encoding + strings; print aux fields (bufio, bit ops)
5. Walk file: SELECTDB, RESIZEDB, expiry, type, key; count keys by type
6. Skip values for every type (list/set/hash/zset/listpack/intset/stream)
7. Compare counts with hdt3213/rdb (table-driven tests)

Phase 3 – Pattern detection
8. Tokenizer + classifier ({num} {uuid} {hex} {ts} {date} {email} {ip} {tag})
9. Pattern trie with counts, bytes, TTL stats
10. High-cardinality collapse into `*` (online, bounded memory)

Phase 4 – Report (tag v0.1 here)
11. Per-key memory estimation, calibrated vs used-mem
12. Terminal table + `--format json` (stable schema_version)
13. Cobra subcommands and flags

Phase 5 – Production-grade
14. 20M-key benchmark, pprof, remove hot-path allocations
15. Parallel pipeline: reader -> workers -> merged tries
16. Multi-shard + S3 download
17. Findings: no-TTL, big keys, cold memory, shard skew
18. CI (GitHub Actions), GoReleaser, Homebrew tap, README with benchmarks

## Current status
Phases 1–2 done (Steps 1–7). internal/rdb: Reader primitives, LZF, Walk() that skips
every value type; oracle_test.go matches hdt3213/rdb on all 85,279 synthetic keys.
Step 8 done: internal/pattern (Normalize/Classify), main prints top patterns via a flat map.
Step 9 done: pattern.Trie (Insert/Patterns/Prefixes/Total, TTL stats relative to ctime),
main prints prefix rollup + pattern table.
Step 10 done: per-node cap (DefaultMaxChildren=500) on literal children; overflow collapses
into "*" (keeping separator), recursive subtree merge. Test data has article:{slug}:views.
Step 11 done: internal/rdb/memory.go estimates per-key RAM (jemalloc lg-quantum=3 classes,
SDS headers, embstr, listpack/quicklist/dict/skiplist/stream overheads) while skipping values.
Verified within ~1.5% of MEMORY USAGE per key; keys sum to ~88% of used-mem (rest = fixed
startup/dict overhead). Report ranks patterns by estimated memory with a SHARE column.
Note: when checking with a Redis container, never mount testdata/ (Redis overwrites dump.rdb).
Step 12 done: internal/report (Analyze -> Report struct; WriteText via tabwriter; WriteJSON
with schema_version=1). main uses stdlib flag: --format text|json, --top N, --max-children N,
`-` for stdin; usage errors exit 2. Tests lock the JSON field names and check no raw keys leak.
Step 13 done: Cobra CLI in cmd/redislens (main.go exit codes, root.go tree, analyze.go).
`redislens analyze [-f text|json] [-n N] [--max-children N] <file|->`, `version`, `--version`,
`completion <shell>` (analyze completes *.rdb). Bare `redislens file.rdb` is a usage error
that suggests `analyze`. usageError -> exit 2, other errors -> exit 1. main_test.go covers it.
Step 14 done: internal/rdbgen writes synthetic RDBs directly (listpack/intset/quicklist2
encodings; TestGeneratorAgainstOracle checks it with hdt3213). Hot path is allocation-free:
Reader reuses a key buffer, pattern.AppendNormalized + generic classifiers over
string|[]byte, Trie.Insert takes []byte (map lookups via m[string(b)] don't allocate).
Step 15 done: internal/report/pipeline.go: Walk goroutine batches keys (4096) -> N workers,
each with its own trie -> Trie.Merge. `--workers/-j` (0 = GOMAXPROCS). Trie.merge now
collapses dst when src is collapsed (bug that only parallel merging hits).
TestWorkersDoNotChangeResult checks reports are identical for 1/2/8 workers.

Benchmarks (M-series, 8 cores; 20M keys, 1.2GB, `redislens analyze testdata/big.rdb`):
| Stage                 | Time   | Peak RSS |
|-----------------------|--------|----------|
| Before Step 14        | 13.0s  | 12MB     |
| Step 14 (no allocs)   | 7.6s   | 6MB      |
| Step 15 (8 workers)   | 2.5s   | 19MB     |
In memory (1M keys): Walk alone ~64ms; Analyze saturates at 4 workers (~96ms), so the
single parse goroutine is now the bottleneck.
Post-15 fix: Trie.Consolidate() (called once in Analyze after merging) folds rare ID
variants into a dominant sibling with >= 20x the keys: {num}/{date}/{ts} into each other,
and digits or hex-letter-only literals into {hex}. Fixes 8-digit IDs read as {date} and
raw hex values leaking as literals. 20M-key report: 21 -> 12 patterns.

Comparison with hdt3213/rdb v1.3.2 CLI (same 20M file): redislens analyze ~2-3.6s / 19MB;
`rdb -c memory` 20s / 19MB; `-c bigkey` 10.5s; `-c prefix -prefix-sep :` 23s / 2.1GB;
`-c prefix` (radix) 97s / 3.7GB. Per-key RAM vs Redis MEMORY USAGE on the fixture:
ours within 1% on large values (+8B on small keys, intentional: bucket pointer counted
in used-mem); hdt3213 off by 13-15% on hashtable/skiplist encodings, +81% on a small
quicklist, and reports LZF-compressed size for strings (50KB blob -> 56B).
Step 16 done (local files only; S3 deferred, input stays plain .rdb, no compressed formats):
`redislens analyze a.rdb b.rdb ...` -> report.Analyze(opt, inputs...). Each shard parsed by
analyzeShard (own pipeline, TTL relative to its own ctime); shards run concurrently
(parallel = workers/4, workers split between them); tries merged, then Consolidate once.
Report.Shards (JSON "shards", Source fields embedded) + SHARDS text table when >1 input.
Combined Source: "N shards", sizes/used-mem summed only if every shard has them, earliest
snapshot time, versions only if all agree. cmd/redislens/input.go opens and checks all
files first; same file twice (os.SameFile) or "-" twice = usage error. 4x5M shards: ~1.4-2.3s.
Step 17 done: internal/report/findings.go -> Report.Findings (JSON "findings", text FINDINGS
section first). pattern.Insert takes a pattern.Key (Bytes, HasTTL/TTLMS, Elems, Big, Cold);
Stats gained TTLBytes, MaxBytes, MaxElems, BigKeys/Bytes, IdleKnown, ColdKeys/Bytes.
Checks: no_ttl (volatile word in pattern, or >=50% of the pattern expires), big_keys
(--big-key 1MiB / --big-key-elements 10000; elements only known for hashtable/skiplist
encodings), cold (--cold-after 7d; needs OpIdle, only saved under *-lru policies; else an
info note), shard_skew (>=1.25x average), shard_times (snapshots >1h apart). no_ttl and cold
need >=1% of key memory. rdbgen.Writer.Idle() plants idle times for tests. Cold check
verified on a real Redis 7.4 allkeys-lru snapshot.
Packaging (manual, no CI yet): scripts/build-release.sh vX.Y.Z -> dist/vX.Y.Z/ archives for
darwin/linux amd64+arm64 and windows amd64, CGO_ENABLED=0, plus SHA256SUMS.
Next: Step 18 (CI, GoReleaser, Homebrew, README) when wanted.