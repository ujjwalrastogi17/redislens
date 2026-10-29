package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ujjwalrastogi17/redislens/internal/rdbgen"
)

const fixture = "../../testdata/dump.rdb"

func TestRun(t *testing.T) {
	haveFixture := true
	if _, err := os.Stat(fixture); err != nil {
		haveFixture = false
	}

	tests := []struct {
		name         string
		args         []string
		needsFixture bool
		wantUsage    bool   // error should be a usageError (exit 2)
		wantErr      bool   // any other error (exit 1)
		wantOut      string // substring of stdout
	}{
		{name: "no args shows help", args: nil, wantOut: "Available Commands"},
		{name: "version", args: []string{"version"}, wantOut: "redislens dev"},
		{name: "--version", args: []string{"--version"}, wantOut: "redislens version dev"},
		{name: "bare file hints analyze", args: []string{"dump.rdb"}, wantUsage: true},
		{name: "analyze no file", args: []string{"analyze"}, wantUsage: true},
		{name: "second shard missing", args: []string{"analyze", fixture, "nope.rdb"}, wantErr: true},
		{name: "same file twice", args: []string{"analyze", fixture, "./" + fixture}, needsFixture: true, wantUsage: true},
		{name: "stdin twice", args: []string{"analyze", "-", "-"}, wantUsage: true},
		{name: "unknown flag", args: []string{"analyze", "--nope", "x"}, wantUsage: true},
		{name: "bad format", args: []string{"analyze", "--format", "xml", "x"}, wantUsage: true},
		{name: "bad big-key size", args: []string{"analyze", "--big-key", "lots", "x"}, wantUsage: true},
		{name: "bad cold-after", args: []string{"analyze", "--cold-after", "3x", "x"}, wantUsage: true},
		{name: "negative top", args: []string{"analyze", "--top", "-1", "x"}, wantUsage: true},
		{name: "missing file", args: []string{"analyze", "does-not-exist.rdb"}, wantErr: true},
		{name: "directory", args: []string{"analyze", "."}, wantErr: true},
		{name: "not an rdb", args: []string{"analyze", "main.go"}, wantErr: true},
		{name: "text report", args: []string{"analyze", "-n", "3", fixture}, needsFixture: true, wantOut: "TOP PATTERNS"},
		{name: "json report", args: []string{"analyze", "-f", "json", fixture}, needsFixture: true, wantOut: `"schema_version": 1`},
		{name: "completion", args: []string{"completion", "zsh"}, wantOut: "#compdef redislens"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsFixture && !haveFixture {
				t.Skip("no fixture; run scripts/gen-testdata.sh")
			}
			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(""), &stdout, &stderr)

			var ue usageError
			isUsage := errors.As(err, &ue)
			switch {
			case tc.wantUsage && !isUsage:
				t.Fatalf("want usage error, got %v", err)
			case tc.wantErr && (err == nil || isUsage):
				t.Fatalf("want runtime error, got %v", err)
			case !tc.wantUsage && !tc.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(stdout.String(), tc.wantOut) {
				t.Fatalf("stdout missing %q:\n%s", tc.wantOut, stdout.String())
			}
		})
	}
}

// Two shards: one combined report with a SHARDS table.
func TestRunShards(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i, n := range []int{20_000, 10_000} {
		p := filepath.Join(dir, fmt.Sprintf("node%d.rdb", i+1))
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := rdbgen.Generate(f, n, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
		f.Close()
		paths = append(paths, p)
	}
	var stdout bytes.Buffer
	if err := run(append([]string{"analyze"}, paths...), nil, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{"2 shards", "30000 keys", "SHARDS", "node1.rdb", "node2.rdb"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Stdin must work the same as a file, minus the file size.
func TestRunStdin(t *testing.T) {
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Skip("no fixture; run scripts/gen-testdata.sh")
	}
	var stdout bytes.Buffer
	if err := run([]string{"analyze", "--format", "json", "-"}, bytes.NewReader(data), &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if !strings.Contains(out, `"stdin"`) || strings.Contains(out, "size_bytes") {
		t.Fatalf("stdin report wrong:\n%.400s", out)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"1048576": 1 << 20, "1MiB": 1 << 20, "512KiB": 512 << 10, "10MB": 10e6, "1.5 mib": 3 << 19, "2gb": 2e9,
	} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-1KiB", "1XB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) should fail", in)
		}
	}
}

func TestParseDays(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseDays(in); err != nil || got != want {
			t.Errorf("parseDays(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-1d", "7days", "0h"} {
		if _, err := parseDays(in); err == nil {
			t.Errorf("parseDays(%q) should fail", in)
		}
	}
}
