// Package check is the whole probe pipeline behind one run: load the config
// and overlay, resolve every check, decide the vantage, probe, and derive the
// document. The check command runs it once; serve runs it on an interval.
package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/failure"
	"github.com/Hedronite/simplex/internal/overlay"
	"github.com/Hedronite/simplex/internal/probe"
	"github.com/Hedronite/simplex/internal/severity"
	"github.com/Hedronite/simplex/internal/vantage"
)

// Defaults for the config and overlay locations.
const (
	DefaultConfig  = "simplex.yaml"
	DefaultOverlay = "simplex.overlay.yaml"
	OverlayEnv     = "SIMPLEX_OVERLAY"
)

// Params names what one run reads. Now is the single run clock.
type Params struct {
	// Config is the config file path.
	Config string
	// Overlay is the --overlay value; empty falls back to $SIMPLEX_OVERLAY,
	// then simplex.overlay.yaml when it exists.
	Overlay string
	// Now is the run clock: generated_at and hold expiry both use it.
	Now time.Time
}

// Deps are the seams of a run. Only real ones are wired by the shipped binary.
type Deps struct {
	Runner *probe.Runner
	Lister vantage.Lister
	Getenv func(string) string
}

// Result is what a completed run produced. Warnings is filled even when Run
// returns an error, so a caller can report what the loaders said on the way.
type Result struct {
	Doc      document.Document
	Body     []byte
	Warnings []config.Finding
}

// RunError is a failed run. Err carries the message and exit code the command
// line prints; Source and Path say where, using field paths only, so a long
// running caller can log it without repeating any value from a private file.
type RunError struct {
	Err    *failure.Error
	Source string
	Path   string
}

func (e *RunError) Error() string { return e.Err.Error() }

// Unwrap exposes the failure so failure.CodeOf maps it to exit 3 or 4.
func (e *RunError) Unwrap() error { return e.Err }

// Fields is the loggable summary: where the run failed, with no message text.
func (e *RunError) Fields() string {
	if e.Path == "" {
		return e.Source
	}
	return e.Source + " " + e.Path
}

// Run executes one full pass.
func Run(ctx context.Context, p *Params, d *Deps) (*Result, error) {
	res := &Result{}

	cfg, findings, err := config.LoadWithClock(p.Config, func() time.Time { return p.Now })
	for i := range findings {
		if findings[i].Level == config.LevelWarning {
			res.Warnings = append(res.Warnings, findings[i])
		}
	}
	if err != nil {
		return res, ConfigFailure(p.Config, err)
	}

	ov, err := loadOverlay(p.Overlay, d.Getenv)
	if err != nil {
		return res, err
	}
	resolved, err := ov.Resolve(cfg)
	if err != nil {
		return res, OverlayFailure(err)
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
		Lister:   d.Lister,
		Canaries: canaries(cfg, instances, d.Runner),
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
		observed = d.Runner.Run(ctx, instances)
	}

	in := severity.Input{
		Now:           p.Now,
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
		return res, &RunError{Err: failure.Runtime(err, "evaluate checks"), Source: "evaluate"}
	}
	res.Doc = doc
	res.Body = doc.Encode()
	return res, nil
}

// ResolveOverlayPath is the overlay resolution order: the flag, then
// $SIMPLEX_OVERLAY, then simplex.overlay.yaml when it exists. Empty means none.
func ResolveOverlayPath(flag string, getenv func(string) string) string {
	path := flag
	if path == "" && getenv != nil {
		path = getenv(OverlayEnv)
	}
	if path == "" {
		if _, err := os.Stat(DefaultOverlay); err == nil {
			path = DefaultOverlay
		}
	}
	return path
}

// loadOverlay resolves and loads the overlay. With none, the overlay is empty,
// so any check that needs an address fails closed naming the missing slugs.
func loadOverlay(flag string, getenv func(string) string) (*overlay.Overlay, error) {
	path := ResolveOverlayPath(flag, getenv)
	if path == "" {
		return &overlay.Overlay{}, nil
	}
	ov, err := overlay.Load(path)
	if err != nil {
		return nil, OverlayFailure(err)
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

// Describe formats one finding for an operator's terminal:
// file: path: message (line N).
func Describe(file string, f *config.Finding) string {
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

// ConfigFailure turns a config load error into a run error (exit 3).
func ConfigFailure(file string, err error) error {
	var ce *config.Error
	if errors.As(err, &ce) && len(ce.Findings) > 0 {
		for i := range ce.Findings {
			if ce.Findings[i].Level == config.LevelError {
				return &RunError{
					Err:    failure.Config("config %s", Describe(file, &ce.Findings[i])),
					Source: "config", Path: safePath(ce.Findings[i].Path),
				}
			}
		}
	}
	return &RunError{Err: failure.Config("config %s: %s", file, err.Error()), Source: "config"}
}

// OverlayFailure turns an overlay load or resolve error into a run error
// (exit 3). Missing slugs are public host identifiers, so they may be logged.
func OverlayFailure(err error) error {
	var missing *overlay.MissingSlugsError
	if errors.As(err, &missing) {
		return &RunError{
			Err:    failure.Config("%s (pass --overlay, set %s, or create %s)", err.Error(), OverlayEnv, DefaultOverlay),
			Source: "overlay", Path: "hosts",
		}
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
		return &RunError{Err: failure.Config("overlay %s", msg), Source: "overlay", Path: safePath(f.Path)}
	}
	return &RunError{Err: failure.Config("overlay: %s", err.Error()), Source: "overlay"}
}

// knownFields are the field names the config and overlay formats define.
var knownFields = map[string]bool{
	"apiVersion": true, "vantage": true, "id": true, "canaries": true,
	"hosts": true, "checks": true, "holds": true, "host": true, "kind": true,
	"target": true, "expected": true, "port": true, "path": true, "reason": true,
	"until": true, "address": true, "lan_address": true, "overrides": true,
}

var (
	slugRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	indexRe = regexp.MustCompile(`^\[[0-9]+\]$`)
)

// safePath returns path only when every part of it is a field the formats
// define, an index, or a host slug under "hosts". A loader reports an unknown
// key by using the key itself as the path, and a mistyped key could be an
// address or a name; such a path is dropped, so what a long-running caller logs
// is always a field path and never a value from a private file.
func safePath(path string) string {
	if path == "" {
		return ""
	}
	prev := ""
	for _, seg := range strings.Split(path, ".") {
		name := seg
		var idx string
		if i := strings.Index(seg, "["); i >= 0 {
			name, idx = seg[:i], seg[i:]
		}
		if idx != "" && !indexRe.MatchString(idx) {
			return ""
		}
		switch {
		case name == "" && idx != "":
		case knownFields[name]:
		case prev == "hosts" && slugRe.MatchString(name):
		default:
			return ""
		}
		prev = name
	}
	return path
}
