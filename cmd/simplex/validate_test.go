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

// validate resolves the overlay from the working directory or the
// environment, so the tests chdir into a directory that carries the
// overlay file the config needs.
func chdirWithOverlay(t *testing.T, cfgPath string) string {
	t.Helper()
	tmp := t.TempDir()
	ovText := overlayForConfig(t, cfgPath)
	if err := os.WriteFile(filepath.Join(tmp, defaultOverlay), []byte(ovText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmp)
	return tmp
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

// The fixture config paths are absolute, because the tests chdir before
// running the command.
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
	// Anchored on this test file's directory, so a chdir inside the test
	// cannot move where the fixtures resolve.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find this file")
	}
	dir := filepath.Dir(thisFile)
	return filepath.Join(dir, "..", "..", "testdata", name, "config.yaml")
}

func TestValidateCleanConfig(t *testing.T) {
	chdirWithOverlay(t, greenConfig(t))
	r := invoke(t, upDeps(), "validate", "--config", greenConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Fatalf("a clean config prints no findings, got %q", r.stdout)
	}
}

func TestValidateExpiredHoldWarns(t *testing.T) {
	chdirWithOverlay(t, holdConfig(t))
	r := invoke(t, upDeps(), "validate", "--config", holdConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "warning: ") || !strings.Contains(r.stdout, "holds[0].until") {
		t.Fatalf("want an expired-hold warning on stdout, got %q", r.stdout)
	}
}

func TestValidateConfigErrorExitsThree(t *testing.T) {
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

func TestValidateMissingOverlaySlugsExitThree(t *testing.T) {
	// No overlay file and nothing in the environment: every configured
	// host is missing its entry, and resolve names them all.
	r := invoke(t, upDeps(), "validate", "--config", greenConfig(t), "--now", "2026-11-13T18:00:00Z")
	if r.code != exitcode.Config {
		t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
	}
	for _, slug := range []string{"box", "lathe", "tower"} {
		if !strings.Contains(r.stdout, "hosts."+slug) {
			t.Fatalf("want a finding for host %q, got %q", slug, r.stdout)
		}
	}
}

func TestValidateJSONFindingsShape(t *testing.T) {
	chdirWithOverlay(t, holdConfig(t))
	r := invoke(t, upDeps(), "validate", "--json", "--config", holdConfig(t), "--now", "2026-11-13T18:00:00Z")
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
		t.Fatalf("want one finding, got %+v", doc.Findings)
	}
	f := doc.Findings[0]
	if f.Level != "warning" || f.Path != "holds[0].until" || f.Line <= 0 || f.Message == "" {
		t.Fatalf("finding shape wrong: %+v", f)
	}
}

func TestValidateJSONErrorFindings(t *testing.T) {
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
	r := invoke(t, upDeps(), "validate", "--config", greenConfig(t), "--now", "yesterday")
	if r.code != exitcode.Config {
		t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
	}
}
