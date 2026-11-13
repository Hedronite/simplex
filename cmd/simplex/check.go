package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/failure"
	"github.com/Hedronite/simplex/internal/outfile"
	"github.com/Hedronite/simplex/internal/overlay"
	"github.com/Hedronite/simplex/internal/probe"
	"github.com/Hedronite/simplex/internal/severity"
	"github.com/Hedronite/simplex/internal/vantage"
)

const (
	defaultConfig  = "simplex.yaml"
	defaultOverlay = "simplex.overlay.yaml"
	overlayEnv     = "SIMPLEX_OVERLAY"
)

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
	if !f.json {
		return failure.Config("check: the table output arrives with the CLI slice (pass --json for now)")
	}
	now, err := runClock(f.now, deps.Now)
	if err != nil {
		return err
	}

	cfg, findings, err := config.LoadWithClock(f.config, func() time.Time { return now })
	for i := range findings {
		if findings[i].Level == config.LevelWarning {
			_, _ = fmt.Fprintf(stderr, "warning: %s\n", describe(f.config, &findings[i]))
		}
	}
	if err != nil {
		return configFailure(f.config, err)
	}

	ov, err := loadOverlay(f.overlay, deps.Getenv)
	if err != nil {
		return err
	}
	resolved, err := ov.Resolve(cfg)
	if err != nil {
		return overlayFailure(err)
	}

	instances := make([]probe.Instance, len(resolved))
	for i := range resolved {
		r := &resolved[i]
		instances[i] = probe.Instance{
			Host: r.Host, Kind: r.Kind, Target: r.Target,
			Address: r.Address, Port: r.Port, Path: r.Path,
		}
	}

	// The vantage is derived from the config's canaries; only the interface
	// lister is a seam, so a test cannot inject a canary verdict.
	status := vantage.New(vantage.Options{
		Lister:   deps.Lister,
		Canaries: canaries(cfg, instances, deps.Runner),
	}).Status(ctx)

	// A blind vantage cannot trust anything it would see, so nothing is dialed.
	observed := make([]probe.Observation, len(instances))
	if status == contract.VantageBlind {
		for i := range instances {
			observed[i] = probe.Observation{
				Host: instances[i].Host, Kind: instances[i].Kind, Target: instances[i].Target,
				Result: contract.ResultUnknown,
			}
		}
	} else {
		observed = deps.Runner.Run(ctx, instances)
	}

	in := severity.Input{
		Now:           now,
		VantageID:     cfg.Vantage.ID,
		VantageStatus: status,
		Checks:        make([]severity.Observation, len(observed)),
	}
	for i := range observed {
		in.Checks[i] = severity.Observation{
			Host: observed[i].Host, Kind: observed[i].Kind, Target: observed[i].Target,
			Expected: resolved[i].Expected,
			Result:   observed[i].Result, Outcome: observed[i].Outcome,
		}
	}
	for _, h := range cfg.ActiveHolds() {
		in.Holds = append(in.Holds, severity.Hold{
			Host: h.Host, Kind: string(h.Kind), Reason: h.Reason, Until: h.Until,
		})
	}
	doc, err := severity.Evaluate(&in)
	if err != nil {
		return failure.Runtime(err, "evaluate checks")
	}
	body := doc.Encode()

	// The file is written first, so a failed write exits 4 with nothing on stdout.
	if f.out != "" {
		if err := outfile.Write(f.out, body); err != nil {
			return failure.Runtime(err, "write --out file %s", f.out)
		}
	}
	if _, err := stdout.Write(body); err != nil {
		return failure.Runtime(err, "write document to stdout")
	}
	if code := severity.ExitCode(&doc); code != 0 {
		return &statusExit{code: code}
	}
	return nil
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

// loadOverlay resolves the overlay path: --overlay, then $SIMPLEX_OVERLAY, then
// simplex.overlay.yaml when it exists. With none, the overlay is empty, so any
// check that needs an address fails closed naming the missing slugs.
func loadOverlay(flag string, getenv func(string) string) (*overlay.Overlay, error) {
	path := flag
	if path == "" && getenv != nil {
		path = getenv(overlayEnv)
	}
	if path == "" {
		if _, err := os.Stat(defaultOverlay); err == nil {
			path = defaultOverlay
		}
	}
	if path == "" {
		return &overlay.Overlay{}, nil
	}
	ov, err := overlay.Load(path)
	if err != nil {
		return nil, overlayFailure(err)
	}
	return ov, nil
}

// canaries builds one vantage canary per configured canary reference. Each runs
// the referenced ts_reach instance through the same runner as the real checks.
func canaries(cfg *config.Config, instances []probe.Instance, runner *probe.Runner) []vantage.Canary {
	var out []vantage.Canary
	for _, c := range cfg.Vantage.Canaries {
		for i := range instances {
			inst := instances[i]
			if inst.Host == c.Host && inst.Kind == contract.KindTSReach && inst.Target == c.Target {
				out = append(out, func(ctx context.Context) bool {
					obs := runner.Run(ctx, []probe.Instance{inst})
					return len(obs) == 1 && obs[0].Result == contract.ResultUp
				})
				break
			}
		}
	}
	return out
}

func describe(file string, f *config.Finding) string {
	msg := file
	if f.Path != "" && f.Path != file {
		msg += ": " + f.Path
	}
	msg += ": " + f.Message
	if f.Line > 0 {
		msg += fmt.Sprintf(" (line %d)", f.Line)
	}
	return msg
}

func configFailure(file string, err error) error {
	var ce *config.Error
	if errors.As(err, &ce) && len(ce.Findings) > 0 {
		for i := range ce.Findings {
			if ce.Findings[i].Level == config.LevelError {
				return failure.Config("config %s", describe(file, &ce.Findings[i]))
			}
		}
	}
	return failure.Config("config %s: %s", file, err.Error())
}

func overlayFailure(err error) error {
	var missing *overlay.MissingSlugsError
	if errors.As(err, &missing) {
		return failure.Config("%s (pass --overlay, set %s, or create %s)", err.Error(), overlayEnv, defaultOverlay)
	}
	var oe *overlay.Error
	if errors.As(err, &oe) && len(oe.Findings) > 0 {
		f := oe.Findings[0]
		msg := f.Message
		if f.Path != "" {
			msg = f.Path + ": " + msg
		}
		if f.Line > 0 {
			msg += fmt.Sprintf(" (line %d)", f.Line)
		}
		return failure.Config("overlay %s", msg)
	}
	return failure.Config("overlay: %s", err.Error())
}
