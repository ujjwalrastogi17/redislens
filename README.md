# redislens

See where your Redis memory goes, by key pattern.

`redislens` reads a Redis or Valkey RDB snapshot and reports estimated memory per
auto-detected key pattern (`cart:{num}:items`, `session:{uuid}`, ...), plus findings
worth acting on: keys that never expire, big keys, cold memory and shard skew.

- **Fast:** 20M keys (a 1.2 GB snapshot) in about 2.5 s on a laptop, in under 20 MB of RAM.
- **Private:** output contains only patterns and numbers, never raw keys or values.
- **Accurate:** per-key memory within about 1% of Redis' own `MEMORY USAGE` on large values.
- **One static binary:** no Redis connection, no runtime dependencies.

## Install

Download the archive for your platform from
[Releases](https://github.com/ujjwalrastogi17/redislens/releases), extract it and put
`redislens` on your `PATH`. Or, with Go 1.24+:

```sh
go install github.com/ujjwalrastogi17/redislens/cmd/redislens@latest
```

On macOS, a binary downloaded with a browser is blocked by Gatekeeper because it isn't
signed yet. Allow it with:

```sh
xattr -d com.apple.quarantine ./redislens
```

## Usage

```sh
redislens analyze dump.rdb
redislens analyze --format json --top 0 dump.rdb > report.json
redislens analyze node1.rdb node2.rdb node3.rdb      # cluster shards, one combined report
cat dump.rdb | redislens analyze -                   # from stdin
```

Example report (synthetic data, `--big-key 32KiB`, trimmed):

```
dump.rdb  4.3 MiB, RDB v12, redis 7.4.12, taken 2026-10-27T07:45:38Z
89279 keys (20000 with TTL), 15 patterns, est. key memory 11.3 MiB of used-mem 12.9 MiB (88%; 1.6 MiB other overhead)

FINDINGS
  !  big keys  big:blob:{num} (string)    20 keys of 32.0 KiB+ (or 10000+ elements); largest 56.1 KiB
  !  big keys  leaderboard:{date} (zset)  14 keys of 32.0 KiB+ (or 10000+ elements); largest 69.6 KiB, up to 715 elements
  i  cold      -                          no LRU idle times in the snapshot, so cold memory can't be measured; ...

TOP PATTERNS
  PATTERN             TYPE      KEYS   MEMORY  SHARE  WITH TTL  AVG TTL
  session:{uuid}      string   20000  2.4 MiB  21.5%     20000     1.5h
  user:{num}:profile  string   20000  2.0 MiB  17.5%         0        -
  feed:{num}:events   list     20000  2.0 MiB  17.5%         0        -
  cart:{num}:items    hash     20000  2.0 MiB  17.4%         0        -
  big:blob:{num}      string      20  1.1 MiB   9.7%         0        -
```

### Flags

| Flag | Default | |
|---|---|---|
| `-f, --format` | `text` | `text` or `json` |
| `-n, --top` | `20` | rows per table, `0` for all |
| `--big-key` | `1MiB` | flag keys using at least this much memory |
| `--big-key-elements` | `10000` | flag hashes, sets and sorted sets with this many elements |
| `--cold-after` | `7d` | flag memory idle at least this long |
| `--max-children` | `500` | distinct values of one key segment before it collapses into `*` |
| `-j, --workers` | CPUs | goroutines classifying keys |

## What it reports

**Patterns.** Each key is split on `:` `/` `|` `#` `,` and variable parts are replaced
with placeholders: `{num}` `{uuid}` `{hex}` `{ts}` `{date}` `{email}` `{ip}` `{tag}`. Values
the classifier can't recognize, such as usernames or slugs, collapse into `*` once a
segment has more than 500 distinct values, so memory stays bounded however many keys
there are.

**Findings.**

| Finding | When |
|---|---|
| no TTL | Keys never expire, but the pattern name suggests temporary data (`cache`, `session`, `lock`, `token`, ...), or most keys of the pattern expire and some don't |
| big keys | Single keys over `--big-key` or `--big-key-elements` |
| cold | Keys idle longer than `--cold-after`. Needs LRU idle times, which Redis only saves with an `*-lru` `maxmemory-policy` |
| shard skew | A shard holding at least 25% more memory than the average shard |

**JSON.** `--format json` output has a `schema_version`. New fields may be added within a
version; renaming or removing a field bumps it.

## How memory is estimated

The RDB file stores values compactly, often compressed, so file size says little about
RAM. While parsing, `redislens` records the facts that matter (string lengths, element
counts, encodings) and applies Redis 7's data structure overheads and jemalloc's
allocation size classes. Values are skipped, never decoded, which is why memory use stays
flat.

The per-key total is compared with the snapshot's `used-mem`. Keys usually account for
85-90% of it; the rest is Redis' fixed startup memory and buffers, which the report shows
separately instead of spreading it over patterns.

## Performance

20M keys, 1.2 GB snapshot, 8-core Apple Silicon laptop:

| Tool | Time | Peak RAM |
|---|---|---|
| `redislens analyze` | ~2.5 s | 19 MB |
| [hdt3213/rdb](https://github.com/hdt3213/rdb) `-c memory` | 20 s | 19 MB |
| hdt3213/rdb `-c prefix -prefix-sep :` | 23 s | 2.1 GB |

## Limitations

- Plain `.rdb` files only; decompress `.gz` or `.zst` snapshots first, e.g.
  `gunzip -c dump.rdb.gz | redislens analyze -`.
- Memory estimates model Redis 7.x on 64-bit Linux with jemalloc.
- Tested with Redis 7.4 snapshots (RDB v12). Older versions and Valkey are not yet
  tested.

## Building from source

```sh
go build -o redislens ./cmd/redislens
go test ./...
./scripts/gen-testdata.sh                  # synthetic fixture via Docker (needed by some tests)
./scripts/build-release.sh v0.1.0          # release archives into dist/
```

## License

[Apache 2.0](LICENSE)
