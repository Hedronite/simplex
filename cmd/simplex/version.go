package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Build stamps. Release builds override them with -ldflags; a build from
// source without them reports the placeholders.
var (
	version = "dev"
	commit  = "none"
	built   = "unknown"
)

func newVersion() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Long: `Print the version.

Human output is one line: simplex <semver>. --json prints one object:
    {"version": "...", "commit": "...", "built": "..."}
Always exits 0.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if asJSON {
				b, err := json.Marshal(struct {
					Version string `json:"version"`
					Commit  string `json:"commit"`
					Built   string `json:"built"`
				}{Version: version, Commit: commit, Built: built})
				if err != nil {
					// Only three strings, so this cannot fail.
					return fmt.Errorf("encode version: %w", err)
				}
				fmt.Fprintln(w, string(b))
				return nil
			}
			fmt.Fprintf(w, "simplex %s\n", version)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print a JSON version object")
	return cmd
}
