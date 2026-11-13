package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/failure"
	"github.com/Hedronite/simplex/internal/overlay"
)

type validateFlags struct {
	config  string
	overlay string
	now     string
	json    bool
}

func newValidate(deps *Deps) *cobra.Command {
	var f validateFlags
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Load and check the config and overlay without probing",
		Long: `Load the config and the overlay, apply the cross-field rules and
report every finding. Nothing is probed.

The overlay resolves like check: --overlay, then $SIMPLEX_OVERLAY,
then simplex.overlay.yaml when present. With a resolved overlay, every
check is resolved against it; a missing slug or an unreadable overlay
is an error. With no overlay path resolved, one warning is printed
("no overlay found: addresses not checked") and the resolve step is
skipped.

Human output is one finding per line, in config order:
    path: message (line N)
Warnings carry a "warning: " prefix. --json prints one document:
    {"findings":[{"level","path","message","line"}]}
Exit codes: 0 when only warnings were found, 3 when any error was. The
error line still goes to stderr.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidate(&f, deps, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", defaultConfig, "config file")
	fl.StringVar(&f.overlay, "overlay", "", "overlay file (default: $"+overlayEnv+", then "+defaultOverlay+" when present)")
	fl.StringVar(&f.now, "now", "", "pin the hold clock (RFC 3339); hold expiry uses it")
	fl.BoolVar(&f.json, "json", false, "print the findings as one JSON document")
	return cmd
}

func runValidate(f *validateFlags, deps *Deps, stdout, stderr io.Writer) error {
	now, err := runClock(f.now, deps.Now)
	if err != nil {
		return err
	}
	var findings []config.Finding
	cfg, cfgFindings, lerr := config.LoadWithClock(f.config, func() time.Time { return now })
	findings = append(findings, cfgFindings...)

	// The overlay resolves like check: --overlay, then $SIMPLEX_OVERLAY,
	// then simplex.overlay.yaml when present. With a resolved path it is
	// loaded and every check is resolved against it; a missing slug or an
	// unreadable overlay is an error. With no overlay path at all,
	// validate warns and skips the resolve step: a clean checkout cannot
	// carry a private overlay.
	overlayFile := ""
	if lerr == nil && cfg != nil {
		if path := resolveOverlayPath(f.overlay, deps.Getenv); path != "" {
			overlayFile = path
			ov, lerr2 := overlay.Load(path)
			if lerr2 != nil {
				findings = append(findings, overlayFindings(lerr2)...)
			} else if _, rerr := ov.Resolve(cfg); rerr != nil {
				findings = append(findings, overlayFindings(rerr)...)
			}
		} else {
			findings = append(findings, config.Finding{
				Level:   config.LevelWarning,
				Message: "no overlay found: addresses not checked",
			})
		}
	}

	if f.json {
		b, merr := marshalFindings(findings)
		if merr != nil {
			return failure.Runtime(merr, "encode findings")
		}
		if _, werr := stdout.Write(append(b, '\n')); werr != nil {
			return failure.Runtime(werr, "write findings to stdout")
		}
	} else {
		for i := range findings {
			line := ""
			if findings[i].Path == "" {
				// File-level findings carry no path: print the message
				// bare, so the no-overlay warning reads naturally.
				line = findings[i].Message
			} else {
				src := f.config
				if overlayFile != "" && i >= len(cfgFindings) {
					src = overlayFile
				}
				line = describe(src, &findings[i])
			}
			if findings[i].Level == config.LevelWarning {
				line = "warning: " + line
			}
			if _, err := fmt.Fprintln(stdout, line); err != nil {
				return failure.Runtime(err, "write findings to stdout")
			}
		}
	}

	if lerr != nil {
		return configFailure(f.config, lerr)
	}
	for i := range findings {
		if findings[i].Level == config.LevelError {
			return failure.Config("validate: %s", describe(f.config, &findings[i]))
		}
	}
	return nil
}

// resolveOverlayPath repeats the overlay resolution order the check
// command uses: --overlay, then $SIMPLEX_OVERLAY, then
// simplex.overlay.yaml when it exists. An empty result means no overlay
// path was resolved at all.
func resolveOverlayPath(flag string, getenv func(string) string) string {
	path := flag
	if path == "" && getenv != nil {
		path = getenv(overlayEnv)
	}
	if path == "" {
		if _, err := os.Stat(defaultOverlay); err == nil {
			path = defaultOverlay
		}
	}
	return path
}

// overlayFindings flattens an overlay failure into the shared finding
// shape, so validate prints overlay problems in the same list and the
// same --json document as config findings.
func overlayFindings(err error) []config.Finding {
	var oe *overlay.Error
	if errors.As(err, &oe) && len(oe.Findings) > 0 {
		return oe.Findings
	}
	var missing *overlay.MissingSlugsError
	if errors.As(err, &missing) {
		out := make([]config.Finding, 0, len(missing.Slugs))
		for _, slug := range missing.Slugs {
			out = append(out, config.Finding{
				Level:   config.LevelError,
				Path:    "hosts." + slug,
				Message: "no overlay entry for this host; its checks cannot run",
			})
		}
		return out
	}
	return []config.Finding{{Level: config.LevelError, Path: "", Message: err.Error()}}
}

// findingsDoc is the one --json document validate prints
// (docs/semantics.md section 10).
type findingsDoc struct {
	Findings []config.Finding `json:"findings"`
}

func marshalFindings(findings []config.Finding) ([]byte, error) {
	if findings == nil {
		findings = []config.Finding{}
	}
	return json.Marshal(findingsDoc{Findings: findings})
}
