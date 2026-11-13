package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hedronite/simplex/internal/exitcode"
	"github.com/Hedronite/simplex/internal/failure"
	"github.com/Hedronite/simplex/internal/probe"
	"github.com/Hedronite/simplex/internal/vantage"
)

// Deps are the seams a run is built from. realDeps wires only real ones; the
// in-process fixture test supplies fakes. No flag, environment variable or
// build tag swaps any of them in the shipped binary.
type Deps struct {
	// Runner probes the resolved checks.
	Runner *probe.Runner
	// Lister answers whether this machine has a tailnet address. Nil means
	// the real interface reader. The canaries always come from the config.
	Lister vantage.Lister
	// Now is the clock used when --now is not given.
	Now func() time.Time
	// Getenv reads the environment (SIMPLEX_OVERLAY).
	Getenv func(string) string
}

func realDeps() *Deps {
	return &Deps{Runner: probe.Default(), Now: time.Now, Getenv: os.Getenv}
}

// statusExit carries a severity exit code (1, 2 or 5) out of a command that
// succeeded: the document was written, and the code is its verdict.
type statusExit struct{ code int }

func (s *statusExit) Error() string { return fmt.Sprintf("exit %d", s.code) }

// run executes the command line and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps *Deps) int {
	root := newRoot(deps, stdout, stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitcode.Green
	}
	var st *statusExit
	if errors.As(err, &st) {
		return st.code
	}
	failure.Report(stderr, err, wantsJSON(args))
	return failure.CodeOf(err)
}

// wantsJSON reports whether --json was given, even when the command line
// failed to parse, so the error line has the right shape.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	return false
}

func newRoot(deps *Deps, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "simplex",
		Short:         "Probe a mesh from one vantage and report structured health",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return failure.Config("unknown command %q (try: check)", args[0])
			}
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return failure.Config("%s", err.Error())
	})
	root.AddCommand(newCheck(deps))
	return root
}

// noArgs rejects positional arguments as a usage error (exit 3).
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return failure.Config("%s: unexpected argument %q", cmd.Name(), strings.TrimSpace(args[0]))
	}
	return nil
}
