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
	config string
	now    string
	json   bool
}

func newValidate(deps *Deps) *cobra.Command {
	var f validateFlags
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Load and check the config and overlay without probing",
		Long: `Load the config and the overlay, apply the cross-field rules and
report every finding. Nothing is probed.

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

	// The overlay is checked through the same resolution order the check
	// command uses. A missing overlay file is not by itself an error; a
	// check that needs an address with no entry is, and resolve reports
	// it naming every missing slug.
	overlayFile := ""
	if lerr == nil && cfg != nil {
		if path, ok := overlayPath(deps.Getenv); ok {
			overlayFile = path
			ov, lerr2 := overlay.Load(path)
			if lerr2 != nil {
				findings = append(findings, overlayFindings(lerr2)...)
			} else if _, rerr := ov.Resolve(cfg); rerr != nil {
				findings = append(findings, overlayFindings(rerr)...)
			}
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
			src := f.config
			if overlayFile != "" && i >= len(cfgFindings) {
				src = overlayFile
			}
			line := describe(src, &findings[i])
			if findings[i].Level == config.LevelWarning {
				line = "warning: " + line
			}
			fmt.Fprintln(stdout, line)
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

// overlayPath repeats the overlay resolution order the check command uses:
// $SIMPLEX_OVERLAY, then simplex.overlay.yaml when it exists. No file means
// nothing to check, which is not an error.
func overlayPath(getenv func(string) string) (string, bool) {
	path := ""
	if getenv != nil {
		path = getenv(overlayEnv)
	}
	if path == "" {
		if _, err := os.Stat(defaultOverlay); err == nil {
			path = defaultOverlay
		}
	}
	return path, path != ""
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
