// Command genrdb writes a large synthetic RDB for benchmarks:
//
//	go run ./scripts/genrdb -keys 20000000 -o testdata/big.rdb
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ujjwalrastogi17/redislens/internal/rdbgen"
)

func main() {
	keys := flag.Int("keys", 1_000_000, "number of keys")
	out := flag.String("o", "testdata/big.rdb", "output file")
	seed := flag.Uint64("seed", 1, "random seed (same seed = same file)")
	flag.Parse()

	f, err := os.Create(*out)
	if err == nil {
		err = rdbgen.Generate(f, *keys, *seed)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genrdb:", err)
		os.Exit(1)
	}
}
