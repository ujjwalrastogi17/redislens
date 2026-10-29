package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRootCmd builds the command tree. A constructor (instead of package-level
// command variables) gives every test a fresh tree with fresh flag values.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "redislens",
		Short: "Memory usage by key pattern for Redis/Valkey RDB snapshots",
		Long: `redislens reads an RDB snapshot and reports estimated memory by key pattern
(e.g. cart:{num}:items). Output contains only patterns and numbers, never raw
keys or values.`,
		Version: version,
		// Errors are printed once, by main; usage is shown only for usage errors.
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		// Without RunE, Cobra would treat "redislens dump.rdb" as an unknown
		// command and return a plain error (exit 1). Handling it here lets us
		// point at the right command and exit 2.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usageError{fmt.Sprintf("unknown command %q; did you mean \"redislens analyze %s\"?", args[0], args[0])}
		},
	}
	// Bad flags anywhere in the tree are usage errors.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageError{fmt.Sprintf("%v (see %q)", err, cmd.CommandPath()+" --help")}
	})

	root.AddCommand(newAnalyzeCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the redislens version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "redislens", version)
		},
	}
}
