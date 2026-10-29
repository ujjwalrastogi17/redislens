package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ujjwalrastogi17/redislens/internal/report"
)

// analyzeFlags holds the flag values; Cobra writes into the fields directly.
type analyzeFlags struct {
	format      string
	top         int
	maxChildren int
	workers     int
	bigKey      string
	bigElems    int64
	coldAfter   string
	cpuProfile  string
	memProfile  string
}

func newAnalyzeCmd() *cobra.Command {
	var f analyzeFlags
	cmd := &cobra.Command{
		Use:   "analyze <file.rdb | -> [more shards...]",
		Short: "Report memory usage by key pattern",
		Long: `Report memory usage by key pattern.

Pass several RDB files (e.g. one per Redis Cluster node) to get one combined
report with a per-shard breakdown.`,
		Example: `  redislens analyze dump.rdb
  redislens analyze --format json --top 0 dump.rdb > report.json
  redislens analyze node1.rdb node2.rdb node3.rdb
  cat dump.rdb | redislens analyze -`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError{"analyze needs at least one RDB file; use - for stdin"}
			}
			return nil
		},
		// Shell completion: suggest only .rdb files.
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return []string{"rdb"}, cobra.ShellCompDirectiveFilterFileExt
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.run(args, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&f.format, "format", "f", "text", "output format: text or json")
	fl.IntVarP(&f.top, "top", "n", 20, "rows per table (0 = all)")
	fl.IntVar(&f.maxChildren, "max-children", 0, "collapse a key segment into * after this many distinct values (0 = default 500)")
	fl.IntVarP(&f.workers, "workers", "j", 0, "goroutines classifying keys (0 = one per CPU)")
	fl.StringVar(&f.bigKey, "big-key", "1MiB", "flag keys using at least this much memory (e.g. 512KiB, 10MB)")
	fl.Int64Var(&f.bigElems, "big-key-elements", report.DefaultBigKeyElems, "flag hashes/sets/zsets with at least this many elements")
	fl.StringVar(&f.coldAfter, "cold-after", "7d", "flag memory idle at least this long (e.g. 36h, 30d); needs LRU idle times in the snapshot")
	// Profiling flags are for development, so they stay out of --help.
	fl.StringVar(&f.cpuProfile, "cpuprofile", "", "write a CPU profile to this file")
	fl.StringVar(&f.memProfile, "memprofile", "", "write an allocation profile to this file")
	_ = fl.MarkHidden("cpuprofile")
	_ = fl.MarkHidden("memprofile")
	_ = cmd.RegisterFlagCompletionFunc("format", cobra.FixedCompletions([]string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func (f *analyzeFlags) run(paths []string, stdin io.Reader, stdout io.Writer) error {
	if f.format != "text" && f.format != "json" {
		return usageError{fmt.Sprintf("unknown --format %q (want text or json)", f.format)}
	}
	if f.top < 0 || f.maxChildren < 0 || f.workers < 0 || f.bigElems <= 0 {
		return usageError{"--top, --max-children and --workers must be >= 0, --big-key-elements > 0"}
	}
	bigKey, err := parseSize(f.bigKey)
	if err != nil {
		return usageError{"--big-key: " + err.Error()}
	}
	coldAfter, err := parseDays(f.coldAfter)
	if err != nil {
		return usageError{"--cold-after: " + err.Error()}
	}

	if f.cpuProfile != "" {
		pf, err := os.Create(f.cpuProfile)
		if err != nil {
			return err
		}
		defer pf.Close()
		if err := pprof.StartCPUProfile(pf); err != nil {
			return err
		}
		defer pprof.StopCPUProfile() // deferred calls run last-in first-out: stop, then close
	}
	if f.memProfile != "" {
		defer writeMemProfile(f.memProfile)
	}

	inputs, closeAll, err := openInputs(paths, stdin)
	if err != nil {
		return err
	}
	defer closeAll()

	opt := report.Options{
		MaxChildren: f.maxChildren, Top: f.top, ToolVersion: version, Workers: f.workers,
		BigKeyBytes: bigKey, BigKeyElems: f.bigElems, ColdAfter: coldAfter,
	}
	rep, err := report.Analyze(opt, inputs...)
	if err != nil {
		return err
	}
	if f.format == "json" {
		return report.WriteJSON(stdout, rep)
	}
	return report.WriteText(stdout, rep)
}

// parseSize reads "1048576", "512KiB", "10MB" (1000-based) or "1MiB" (1024-based).
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{ // longest suffixes first, so "KiB" isn't read as "B"
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1},
	}
	num, mult := strings.TrimSpace(s), int64(1)
	for _, u := range units {
		if strings.HasSuffix(strings.ToUpper(num), strings.ToUpper(u.suffix)) {
			num, mult = strings.TrimSpace(num[:len(num)-len(u.suffix)]), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (want e.g. 1MiB, 512KiB, 10MB)", s)
	}
	return int64(n * float64(mult)), nil
}

// parseDays is time.ParseDuration plus a "d" (day) unit, which Go's parser lacks.
func parseDays(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q (want e.g. 7d, 36h)", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (want e.g. 7d, 36h)", s)
	}
	return d, nil
}

// writeMemProfile records allocations made over the whole run ("alloc_space").
func writeMemProfile(path string) {
	pf, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memprofile:", err)
		return
	}
	defer pf.Close()
	runtime.GC()
	if err := pprof.Lookup("allocs").WriteTo(pf, 0); err != nil {
		fmt.Fprintln(os.Stderr, "memprofile:", err)
	}
}
