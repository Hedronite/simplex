package outfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestWriteCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.json")
	if err := Write(path, []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("two\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two\n" {
		t.Fatalf("read %q, %v", got, err)
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "probe.json" {
		t.Fatalf("directory holds %v, want only probe.json", n)
	}
}

func TestWriteModeIgnoresUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	old := setUmask(0o077)
	defer setUmask(old)
	path := filepath.Join(t.TempDir(), "probe.json")
	if err := Write(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != Mode {
		t.Fatalf("mode %v, want %v", fi.Mode().Perm(), Mode)
	}
}

func TestFailedWriteKeepsThePreviousFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.json")
	if err := Write(path, []byte("good\n")); err != nil {
		t.Fatal(err)
	}
	// Make the rename fail: the target is now a non-empty directory.
	bad := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(bad, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(bad, []byte("new\n")); err == nil {
		t.Fatal("expected an error writing over a directory")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "good\n" {
		t.Fatalf("previous file changed: %q", got)
	}
	for _, n := range names(t, dir) {
		if n != "probe.json" && n != "target" {
			t.Fatalf("temp file left behind: %s", n)
		}
	}
}

func TestUnwritableDirectoryIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions do not apply")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	if err := Write(filepath.Join(dir, "probe.json"), []byte("x")); err == nil {
		t.Fatal("expected an error in a read-only directory")
	}
	if n := names(t, dir); len(n) != 0 {
		t.Fatalf("directory holds %v", n)
	}
}

func TestMissingDirectoryIsAnError(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "nope", "probe.json"), []byte("x")); err == nil {
		t.Fatal("expected an error")
	}
}
