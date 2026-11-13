package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hedronite/simplex/internal/check"
	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/failure"
	"github.com/Hedronite/simplex/internal/outfile"
	"github.com/Hedronite/simplex/internal/severity"
	"github.com/Hedronite/simplex/internal/table"
)

// The names the commands share with the pipeline.
const (
	defaultConfig  = check.DefaultConfig
	defaultOverlay = check.DefaultOverlay
	overlayEnv     = check.OverlayEnv
)

func describe(file string, f *config.Finding) string { return check.Describe(file, f) }

func configFailure(file string, err error) error { return check.ConfigFailure(file, err) }

func resolveOverlayPath(flag string, getenv func(string) string) string {
	return check.ResolveOverlayPath(flag, getenv)
}

type checkFlags struct {
	config  string
	overlay string
	now     string
	out     string
	json    bool
}

func newCheck(deps *Deps) *cobra.Command {
	var f checkFlags
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Probe every configured check once and report the verdict",
		Long: `Probe every configured check once from this vantage and report the
simplex.probe/v0 document.

Exit codes: 0 GREEN, 1 YELLOW, 2 RED, 3 config or usage error,
4 runtime or I/O error, 5 vantage blind. Exits 0, 1, 2 and 5 write the
document; exits 3 and 4 never do.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCheck(cmd.Context(), &f, deps, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", defaultConfig, "config file")
	fl.StringVar(&f.overlay, "overlay", "", "overlay file (default: $"+overlayEnv+", then "+defaultOverlay+" when present)")
	fl.StringVar(&f.now, "now", "", "pin the run clock (RFC 3339); generated_at and hold expiry use it")
	fl.StringVar(&f.out, "out", "", "also write the document to this file (temp file, fsync, atomic rename)")
	fl.BoolVar(&f.json, "json", false, "print the simplex.probe/v0 document")
	return cmd
}

func runCheck(ctx context.Context, f *checkFlags, deps *Deps, stdout, stderr io.Writer) error {
	now, err := runClock(f.now, deps.Now)
	if err != nil {
		return err
	}

	res, err := check.Run(ctx,
		&check.Params{Config: f.config, Overlay: f.overlay, Now: now},
		&check.Deps{Runner: deps.Runner, Lister: deps.Lister, Getenv: deps.Getenv, TailscaleSocket: deps.TailscaleSocket})
	for i := range res.Warnings {
		_, _ = fmt.Fprintf(stderr, "warning: %s\n", describe(f.config, &res.Warnings[i]))
	}
	if err != nil {
		return err
	}

	// The file is written first, so a failed write exits 4 with nothing on stdout.
	if f.out != "" {
		if err := outfile.Write(f.out, res.Body); err != nil {
			return failure.Runtime(err, "write --out file %s", f.out)
		}
	}
	if f.json {
		if _, err := stdout.Write(res.Body); err != nil {
			return failure.Runtime(err, "write document to stdout")
		}
	} else {
		// The table is a renderer of the same document; only the
		// severity word is colored, and only on a terminal without
		// NO_COLOR.
		if _, err := fmt.Fprint(stdout, table.Render(&res.Doc, colorFor(stdout, deps.Getenv))); err != nil {
			return failure.Runtime(err, "write table to stdout")
		}
	}
	if code := severity.ExitCode(&res.Doc); code != 0 {
		return &statusExit{code: code}
	}
	return nil
}

// colorFor decides whether the table may paint the severity word: only
// on a character device (a terminal) with NO_COLOR unset.
func colorFor(w io.Writer, getenv func(string) string) bool {
	if getenv != nil && getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runClock resolves the run clock: --now when given, else the injected clock.
func runClock(flag string, fallback func() time.Time) (time.Time, error) {
	if flag == "" {
		return fallback().UTC().Truncate(time.Second), nil
	}
	t, err := time.Parse(time.RFC3339, flag)
	if err != nil {
		return time.Time{}, failure.Config("--now %q: not an RFC 3339 time (for example 2026-11-13T18:00:00Z)", flag)
	}
	return t.UTC(), nil
}
