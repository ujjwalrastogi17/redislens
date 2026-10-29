package report

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// WriteJSON writes the report as indented JSON.
func WriteJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// WriteText writes a human-readable report. tabwriter aligns columns to the
// widest cell, so long patterns don't break the layout.
func WriteText(w io.Writer, rep *Report) error {
	s := rep.Source
	fmt.Fprint(w, s.File)
	if s.SizeBytes > 0 {
		fmt.Fprintf(w, "  %s", HumanBytes(s.SizeBytes))
	}
	if s.RDBVersion > 0 {
		fmt.Fprintf(w, ", RDB v%d", s.RDBVersion)
	}
	if s.RedisVersion != "" {
		fmt.Fprintf(w, ", redis %s", s.RedisVersion)
	}
	if s.SnapshotTime != nil {
		taken := "taken"
		if len(rep.Shards) > 1 {
			taken = "earliest taken"
		}
		fmt.Fprintf(w, ", %s %s", taken, s.SnapshotTime.Format(time.RFC3339))
	}
	fmt.Fprintln(w)

	t := rep.Totals
	fmt.Fprintf(w, "%d keys (%d with TTL), %d patterns, est. key memory %s",
		t.Keys, t.KeysWithTTL, t.Patterns, HumanBytes(t.MemoryBytes))
	if s.UsedMemBytes > 0 {
		fmt.Fprintf(w, " of used-mem %s (%.0f%%; %s other overhead)",
			HumanBytes(s.UsedMemBytes), 100*float64(t.MemoryBytes)/float64(s.UsedMemBytes),
			HumanBytes(t.OverheadBytes))
	}
	fmt.Fprintln(w)

	writeFindings(w, rep.Findings)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "TYPE    \tKEYS\tMEMORY\tIN FILE\tWITH TTL\t")
	for _, r := range rep.Types {
		fmt.Fprintf(tw, "%-8s\t%d\t%s\t%s\t%d\t\n", r.Type, r.Keys, HumanBytes(r.MemoryBytes), HumanBytes(r.FileBytes), r.KeysWithTTL)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(rep.Shards) > 1 {
		writeShards(w, rep.Shards)
	}
	writeRows(w, "TOP PREFIXES", "PREFIX", rep.Prefixes, false)
	writeRows(w, "TOP PATTERNS", "PATTERN", rep.Patterns, true)
	return nil
}

// findingLabels are the short names shown in the text report.
var findingLabels = map[string]string{
	FindingNoTTL:      "no TTL",
	FindingBigKeys:    "big keys",
	FindingCold:       "cold",
	FindingShardSkew:  "shard skew",
	FindingShardTimes: "shard times",
	FindingNoIdle:     "cold",
}

// writeFindings comes first in the report: it says what to act on, the
// tables below say why.
func writeFindings(w io.Writer, fs []Finding) {
	fmt.Fprintln(w, "\nFINDINGS")
	if len(fs) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range fs {
		mark := "!"
		if f.Severity != "warn" {
			mark = "i"
		}
		subject := f.Pattern
		if f.Shard != "" {
			subject = f.Shard
		}
		if f.Type != "" {
			subject += " (" + f.Type + ")"
		}
		if subject == "" {
			subject = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", mark, findingLabels[f.Kind], subject, f.Message)
	}
	tw.Flush()
}

func writeShards(w io.Writer, shards []Shard) {
	fmt.Fprintln(w, "\nSHARDS")
	width := len("SHARD")
	for _, s := range shards {
		width = max(width, len(s.File))
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%-*s\tKEYS\tMEMORY\tSHARE\tWITH TTL\tUSED-MEM\tTAKEN               \t\n", width, "SHARD")
	for _, s := range shards {
		used, taken := "-", "-"
		if s.UsedMemBytes > 0 {
			used = HumanBytes(s.UsedMemBytes)
		}
		if s.SnapshotTime != nil {
			taken = s.SnapshotTime.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%-*s\t%d\t%s\t%.1f%%\t%d\t%s\t%-20s\t\n",
			width, s.File, s.Keys, HumanBytes(s.MemoryBytes), 100*s.Share, s.KeysWithTTL, used, taken)
	}
	tw.Flush()
}

func writeRows(w io.Writer, title, col string, rows []Row, withType bool) {
	fmt.Fprintf(w, "\n%s\n", title)
	// Left-align text columns, right-align numbers: two writers would misalign,
	// so pad text manually via %-*s using the widest cell.
	width := len(col)
	for _, r := range rows {
		width = max(width, len(r.Pattern))
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	head := fmt.Sprintf("%-*s\t", width, col)
	if withType {
		head += "TYPE   \t"
	}
	fmt.Fprintln(tw, head+"KEYS\tMEMORY\tSHARE\tWITH TTL\tAVG TTL\t")
	for _, r := range rows {
		line := fmt.Sprintf("%-*s\t", width, r.Pattern)
		if withType {
			line += fmt.Sprintf("%-7s\t", r.Type)
		}
		avg := "-"
		if r.TTL != nil {
			avg = humanDuration(r.TTL.AvgMS)
		}
		fmt.Fprintf(tw, "%s%d\t%s\t%.1f%%\t%d\t%s\t\n",
			line, r.Keys, HumanBytes(r.MemoryBytes), 100*r.Share, r.KeysWithTTL, avg)
	}
	tw.Flush()
}

// HumanBytes formats n using binary units (KiB = 1024).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit && n > -unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= unit
		if f < unit && f > -unit {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", f/unit)
}

func humanDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}
