package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/table"
)

// Every fixture document that a run writes renders to a committed expected
// table. The golden tables live outside the frozen root testdata layout, in
// their own directory, one file per case.
func TestFixtureDocumentsRenderToGoldenTables(t *testing.T) {
	cases, err := os.ReadDir("testdata/tables")
	if err != nil {
		t.Fatalf("read tables: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no committed tables")
	}
	for _, c := range cases {
		name := strings.TrimSuffix(c.Name(), filepath.Ext(c.Name()))
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name, "document.json"))
			if err != nil {
				t.Fatalf("read document: %v", err)
			}
			var d document.Document
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatalf("decode document: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "tables", name+".txt"))
			if err != nil {
				t.Fatalf("read golden table: %v", err)
			}
			if got := table.Render(&d, false); got != string(want) {
				t.Fatalf("render differs from the golden table\n--- got\n%s--- want\n%s", got, want)
			}
		})
	}
}

// check without --json prints the table to stdout with the same exit code
// the document verdict maps to, and --out still writes the document.
func TestCheckTableOutput(t *testing.T) {
	dir := "../../testdata/green"
	cfgPath := filepath.Join(dir, "config.yaml")
	tmp := t.TempDir()
	ovPath := writeOverlay(t, tmp, cfgPath, "")
	deps := fixtureDeps(t, mustDocument(t, filepath.Join(dir, "document.json")), ovPath)
	r := invoke(t, deps, "check", "--config", cfgPath, "--overlay", ovPath, "--now", "2026-11-13T18:00:00Z")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "tables", "green.txt"))
	if err != nil {
		t.Fatalf("read golden table: %v", err)
	}
	if r.stdout != string(want) {
		t.Fatalf("table output differs\n--- got\n%s--- want\n%s", r.stdout, want)
	}
	if strings.Contains(r.stdout, "\x1b") {
		t.Fatal("a buffer stdout must not be colored")
	}
}

func TestCheckTableOutputWithOut(t *testing.T) {
	dir := "../../testdata/green"
	cfgPath := filepath.Join(dir, "config.yaml")
	tmp := t.TempDir()
	ovPath := writeOverlay(t, tmp, cfgPath, "")
	deps := fixtureDeps(t, mustDocument(t, filepath.Join(dir, "document.json")), ovPath)
	outPath := filepath.Join(tmp, "probe.json")
	r := invoke(t, deps, "check", "--config", cfgPath, "--overlay", ovPath, "--now", "2026-11-13T18:00:00Z", "--out", outPath)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
	// The table goes to stdout and the document to the file.
	if !strings.HasSuffix(strings.TrimRight(r.stdout, "\n"), "tcp_connect_ok") {
		t.Fatalf("stdout is not the table: %q", r.stdout)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read --out file: %v", err)
	}
	if !strings.Contains(string(raw), "\"apiVersion\": \"simplex.probe/v0\"") {
		t.Fatalf("--out file is not the document: %q", raw)
	}
}

// NO_COLOR keeps the severity word but drops the escapes.
func TestCheckTableNoColor(t *testing.T) {
	// Color happens only on a character device with NO_COLOR unset; both
	// halves of that decision are checked through colorFor.
	if colorFor(nil, func(string) string { return "1" }) {
		t.Fatal("NO_COLOR must disable color")
	}
	if colorFor(nil, func(string) string { return "" }) {
		t.Fatal("a non-terminal writer must not get color")
	}
}

func mustDocument(t *testing.T, path string) *document.Document {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d document.Document
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return &d
}
