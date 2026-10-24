#!/usr/bin/env bash
# Generates a SYNTHETIC RDB snapshot at testdata/dump.rdb.
# Starts a throwaway Redis in Docker, loads ~100K patterned keys, BGSAVEs, copies the file out.
# Usage: scripts/gen-testdata.sh [num_users]   (default 20000 -> ~100K keys)
set -euo pipefail

N="${1:-20000}"
NAME="redislens-testdata"
IMAGE="redis:7"
OUT_DIR="$(cd "$(dirname "$0")/.." && pwd)/testdata"
mkdir -p "$OUT_DIR"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" "$IMAGE" >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

until docker exec "$NAME" redis-cli ping >/dev/null 2>&1; do sleep 0.2; done

echo "loading synthetic keys for $N users..."
awk -v n="$N" 'BEGIN {
  srand(42)
  for (i = 1; i <= n; i++) {
    printf "SET user:%d:profile name=user%d;plan=free\n", i, i
    printf "HSET cart:%d:items sku-%d %d sku-%d %d\n", i, i % 500, int(rand()*5)+1, (i*7) % 500, 1
    printf "SET session:%08x-%04x-4%03x-a%03x-%012x %d EX %d\n", int(rand()*2^31), int(rand()*65535), int(rand()*4095), int(rand()*4095), int(rand()*2^40), i, 3600 + int(rand()*3600)
    printf "RPUSH feed:%d:events login view click\n", i
    if (i % 2 == 0) printf "ZADD leaderboard:2026-10-%02d %d user:%d\n", (i % 28) + 1, int(rand()*10000), i
    if (i % 4 == 0) printf "SADD tags:%d red green blue\n", i
    if (i % 1000 == 0) printf "SET big:blob:%d %s\n", i, sprintf("%050000d", 0)
    if (i % 100 == 0) printf "SADD ids:%d %d %d %d\n", i, i, i+1, i+2
    if (i % 500 == 0) printf "XADD stream:%d * event signup user %d\n", i, i
    if (i % 5000 == 0) printf "XGROUP CREATE stream:%d workers 0\n", i - (i % 500)
    # Letters-only slugs: the classifier cannot detect these, so the trie must collapse them to *.
    if (i % 5 == 0) {
      slug = ""
      for (c = 0; c < 6; c++) slug = slug substr("abcdefghijklmnopqrstuvwxyz", int(rand()*26)+1, 1)
      printf "SET article:%s:views %d\n", slug, i
    }
  }
  # Large collections force non-compact encodings (hashtable/skiplist/multi-node quicklist).
  for (j = 0; j < 600; j++) {
    printf "HSET bighash:1 f%d v%d\n", j, j
    printf "SADD bigset:1 m%d\n", j
    printf "ZADD bigzset:1 %d m%d\n", j, j
    printf "RPUSH biglist:1 item-%d-padding-padding-padding-padding\n", j
  }
  printf "SELECT 1\nSET other:db:key 1\n"
}' | docker exec -i "$NAME" redis-cli --pipe

docker exec "$NAME" redis-cli info keyspace | grep '^db'
docker exec "$NAME" redis-cli info memory | grep -E '^used_memory:'
docker exec "$NAME" redis-cli save >/dev/null
docker cp "$NAME:/data/dump.rdb" "$OUT_DIR/dump.rdb"
echo "wrote $OUT_DIR/dump.rdb ($(wc -c < "$OUT_DIR/dump.rdb") bytes)"
