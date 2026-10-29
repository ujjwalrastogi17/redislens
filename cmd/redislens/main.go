// Command redislens analyzes Redis/Valkey RDB snapshots.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/redislens
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ue usageError
		if errors.As(err, &ue) {
			os.Exit(2) // exit code 2 = usage error (Unix convention)
		}
		os.Exit(1)
	}
}

// usageError marks bad invocations so main can exit with code 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// run takes args and streams as parameters (instead of reading os.Args /
// os.Stdout directly) so tests can call it.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.Execute()
}
