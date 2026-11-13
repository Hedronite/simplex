package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Hedronite/simplex/internal/exitcode"
)

// The fixture config paths are absolute, anchored on this test file, so a
// chdir inside the test cannot move where the fixtures resolve.
func greenConfig(t *testing.T) string {
	t.Helper()
	return fixtureConfig(t, "green")
}

func holdConfig(t *testing.T) string {
	t.Helper()
	return fixtureConfig(t, "expired-hold")
}

func errorConfig(t *testing.T) string {
	t.Helper()
	return fixtureConfig(t, "config-error")
}

func fixtureConfig(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find this test file")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", name, "config.yaml")
}

// overlayForConfig builds an overlay text that covers every host the
// config names, with placeholder addresses.
func overlayForConfig(t *testing.T, cfgPath string) string {
	t.Helper()
	p := writeOverlay(t, t.TempDir(), cfgPath, "")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestValidateTableDrivenOverlay(t *testing.T) {
	cfg := greenConfig(t)
	fullOverlay := overlayForConfig(t, cfg)
	partialOverlay := strings.Replace(fullOverlay, "  box:\n    address: box.invalid\n", "", 1)

	dirtyStdout := func(r result) bool {
		return r.stdout != ""
	}
	contains := func(substr string) func(result) bool {
		return func(r result) bool { return strings.Contains(r.stdout, substr) }
	}
	tmp := t.TempDir()
	cases := []struct {
		name   string
		args   []string
		env    string
		stdout func(result) bool
		code   int
		setup  func()
		undo   func()
	}{
		{
			name:   "overlay via the default file is resolved",
			args:   []string{"validate", "--config", cfg},
			stdout: func(r result) bool { return r.stdout == "" },
			code:   exitcode.Green,
		},
		{
			name:   "overlay via the flag is resolved",
			args:   []string{"validate", "--config", cfg, "--overlay", "-"},
			stdout: func(r result) bool { return r.stdout == "" },
			code:   exitcode.Green,
		},
		{
			name:   "overlay via the environment is resolved",
			args:   []string{"validate", "--config", cfg},
			env:    "SIMPLEX_OVERLAY",
			stdout: func(r result) bool { return r.stdout == "" },
			code:   exitcode.Green,
		},
		{
			name:   "no overlay at all is one warning and exit 0",
			args:   []string{"validate", "--config", cfg},
			stdout: contains("warning: no overlay found: addresses not checked"),
			code:   exitcode.Green,
			setup: func() {
				// The default file must be gone for no path to resolve.
				_ = os.Remove(filepath.Join(tmp, defaultOverlay))
			},
			undo: func() {
				_ = os.WriteFile(filepath.Join(tmp, defaultOverlay), []byte(fullOverlay), 0o600)
			},
		},
		{
			name:   "an incomplete overlay names the missing slugs",
			args:   []string{"validate", "--config", cfg, "--overlay", "PARTIAL"},
			stdout: contains("hosts.box"),
			code:   exitcode.Config,
		},
		{
			name:   "an unreadable overlay file is exit 3",
			args:   []string{"validate", "--config", cfg, "--overlay", "-"},
			stdout: dirtyStdout,
			code:   exitcode.Config,
		},
		{
			name:   "a named overlay that does not exist is exit 3",
			args:   []string{"validate", "--config", cfg, "--overlay", "-"},
			stdout: dirtyStdout,
			code:   exitcode.Config,
		},
	}
	files := map[string]string{
		"full.overlay.txt":    fullOverlay,
		"partial.overlay.txt": partialOverlay,
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(tmp)
	if err := os.WriteFile(filepath.Join(tmp, defaultOverlay), []byte(fullOverlay), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range cases {
		c := &cases[i]
		if c.args != nil {
			for j, a := range c.args {
				if a == "PARTIAL" {
					c.args[j] = filepath.Join(tmp, "partial.overlay.txt")
					continue
				}
				if a == "-" {
					switch c.name {
					case "overlay via the flag is resolved":
						c.args[j] = filepath.Join(tmp, "full.overlay.txt")
					case "an incomplete overlay names the missing slugs":
						c.args[j] = filepath.Join(tmp, "partial.overlay.txt")
					case "an unreadable overlay file is exit 3":
						c.args[j] = filepath.Join(tmp, "defaultOverlay")
					case "a named overlay that does not exist is exit 3":
						c.args[j] = filepath.Join(tmp, "absent.overlay.txt")
					}
				}
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			if tc.undo != nil {
				defer tc.undo()
			}
			deps := upDeps()
			if tc.env != "" {
				deps.Getenv = func(key string) string {
					if key == overlayEnv {
						return filepath.Join(tmp, "full.overlay.txt")
					}
					return ""
				}
			}
			r := invoke(t, deps, tc.args...)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d (stderr %q)", r.code, tc.code, r.stderr)
			}
			if !tc.stdout(r) {
				t.Fatalf("stdout check failed:\n%s", r.stdout)
			}
		})
	}
}

func TestValidateNoOverlayJSONWarning(t *testing.T) {
	t.Chdir(t.TempDir())
	r := invoke(t, upDeps(), "validate", "--json", "--config", greenConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
	var doc struct {
		Findings []struct {
			Level   string `json:"level"`
			Path    string `json:"path"`
			Message string `json:"message"`
			Line    int    `json:"line"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one findings document: %q (%v)", r.stdout, err)
	}
	if len(doc.Findings) != 1 {
		t.Fatalf("want exactly the no-overlay warning, got %+v", doc.Findings)
	}
	f := doc.Findings[0]
	if f.Level != "warning" || f.Message != "no overlay found: addresses not checked" || f.Path != "" || f.Line != 0 {
		t.Fatalf("warning shape wrong: %+v", f)
	}
}

func TestValidateExpiredHoldWarns(t *testing.T) {
	t.Chdir(t.TempDir())
	r := invoke(t, upDeps(), "validate", "--config", holdConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "warning: ") || !strings.Contains(r.stdout, "holds[0].until") {
		t.Fatalf("want an expired-hold warning on stdout, got %q", r.stdout)
	}
}

func TestValidateConfigErrorExitsThree(t *testing.T) {
	t.Chdir(t.TempDir())
	r := invoke(t, upDeps(), "validate", "--config", errorConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Config {
		t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
	}
	// Findings go to stdout even on the error exit.
	if !strings.Contains(r.stdout, "vantage.canaries[0].host") {
		t.Fatalf("want the canary finding on stdout, got %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "simplex: ") {
		t.Fatalf("want the error line on stderr, got %q", r.stderr)
	}
}

func TestValidateJSONErrorFindings(t *testing.T) {
	t.Chdir(t.TempDir())
	r := invoke(t, upDeps(), "validate", "--json", "--config", errorConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Config {
		t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
	}
	var doc struct {
		Findings []struct {
			Level string `json:"level"`
			Path  string `json:"path"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one findings document: %q (%v)", r.stdout, err)
	}
	found := false
	for _, f := range doc.Findings {
		if f.Level == "error" && strings.Contains(f.Path, "vantage.canaries[0].host") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want the error finding in the document, got %+v", doc.Findings)
	}
	// The error line still goes to stderr.
	oneErrorLine(t, r.stderr, exitcode.Config)
}

func TestValidateBadClockExitsThree(t *testing.T) {
	t.Chdir(t.TempDir())
	r := invoke(t, upDeps(), "validate", "--config", greenConfig(t), "--now", "yesterday")
	if r.code != exitcode.Config {
		t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
	}
}
