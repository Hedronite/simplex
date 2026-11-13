package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Hedronite/simplex/internal/exitcode"
)

func TestVersionPrintsOneLine(t *testing.T) {
	r := invoke(t, upDeps(), "version")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0", r.code)
	}
	line := strings.TrimSuffix(r.stdout, "\n")
	// A build without ldflags reports the fallbacks.
	if !strings.HasPrefix(line, "simplex dev (commit unknown, built unknown)") {
		t.Fatalf("want \"simplex <version> (commit <sha>, built <date>)\" with fallbacks, got %q", line)
	}
	if strings.Contains(line, "none") {
		t.Fatalf("the commit fallback is unknown, got %q", line)
	}
	if r.stderr != "" {
		t.Fatalf("version must be quiet on stderr, got %q", r.stderr)
	}
}

func TestVersionJSONFallbacks(t *testing.T) {
	r := invoke(t, upDeps(), "version", "--json")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0", r.code)
	}
	var v struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Built   string `json:"built"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", r.stdout, err)
	}
	if v.Version != "dev" || v.Commit != "unknown" || v.Built != "unknown" {
		t.Fatalf("want the dev, unknown, unknown fallbacks, got %+v", v)
	}
}

func TestVersionJSON(t *testing.T) {
	r := invoke(t, upDeps(), "version", "--json")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0", r.code)
	}
	var v struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Built   string `json:"built"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", r.stdout, err)
	}
	if v.Version == "" || v.Commit == "" || v.Built == "" {
		t.Fatalf("version object has empty fields: %+v", v)
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	r := invoke(t, upDeps(), "version", "now")
	if r.code != exitcode.Config {
		t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
	}
}

func TestRootHelpListsCommands(t *testing.T) {
	r := invoke(t, upDeps())
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0", r.code)
	}
	for _, want := range []string{"check", "validate", "version", "--config", "--json", "--now", "--out", "--overlay", "0 GREEN", "5 vantage blind"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("root help is missing %q:\n%s", want, r.stdout)
		}
	}
	if r.stderr != "" {
		t.Fatalf("help must be quiet on stderr, got %q", r.stderr)
	}
}

func TestHelpCommandListsCommands(t *testing.T) {
	r := invoke(t, upDeps(), "help")
	if r.code != exitcode.Green {
		t.Fatalf("exit %d, want 0", r.code)
	}
	if !strings.Contains(r.stdout, "validate") || !strings.Contains(r.stdout, "version") {
		t.Fatalf("help must list the commands, got %q", r.stdout)
	}
}

func TestCommandHelpFitsOneScreen(t *testing.T) {
	for _, cmd := range []string{"check", "validate", "version"} {
		r := invoke(t, upDeps(), cmd, "--help")
		if r.code != exitcode.Green {
			t.Fatalf("%s --help exit %d, want 0", cmd, r.code)
		}
		lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
		if len(lines) > 30 {
			t.Fatalf("%s --help is %d lines, want one screen or less", cmd, len(lines))
		}
		switch cmd {
		case "check":
			if !strings.Contains(r.stdout, "0 GREEN") || !strings.Contains(r.stdout, "--out") {
				t.Fatalf("check help misses exit codes or flags:\n%s", r.stdout)
			}
		case "validate":
			if !strings.Contains(r.stdout, "3") || !strings.Contains(r.stdout, "--json") {
				t.Fatalf("validate help misses exit codes or flags:\n%s", r.stdout)
			}
		}
	}
}
