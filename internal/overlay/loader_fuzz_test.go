package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedInputs collects the overlay-shaped seeds. The S0 fixtures carry no
// overlay (it is private and untracked), so the seeds here are hand-built
// shapes plus the ones the agreement corpus exercises.
func seedInputs() [][]byte {
	seeds := [][]byte{
		[]byte("apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n"),
		[]byte("apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    lan_address: \"lan-spoke\"\n    overrides:\n      - {kind: ts_reach, port: 9101}\n"),
		[]byte("apiVersion: simplex.overlay/v0\nhosts:\n  edge:\n    address: \"tailnet-edge\"\n    overrides:\n      - {kind: prom_up, target: \":9090/-/ready\", path: /metrics, port: 9091}\n"),
		[]byte("apiVersion: simplex.overlay/v0\nhosts: {}\n"),
		[]byte(""),
	}
	// Agreement corpus inputs are seeds too: they cover the known reject
	// and accept shapes in both directions.
	if entries, err := os.ReadDir(filepath.Join("testdata", "agreement")); err == nil {
		for _, e := range entries {
			path := filepath.Join("testdata", "agreement", e.Name(), "input.yaml")
			if raw, err := os.ReadFile(path); err == nil {
				seeds = append(seeds, raw)
			}
		}
	}
	return seeds
}

// assertOverlayProperty pins: no panic, and every outcome is either a
// valid overlay or a typed *Error whose findings are well-formed.
func assertOverlayProperty(t *testing.T, data []byte) {
	t.Helper()
	ov, findings, err := Parse(data)
	if err != nil {
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("untyped error %T: %v", err, err)
		}
		if ov != nil {
			t.Fatal("overlay returned alongside an error")
		}
		for _, f := range typed.Findings {
			if f.Level != LevelError {
				t.Fatalf("error carries a %q finding: %+v", f.Level, f)
			}
		}
		return
	}
	if ov == nil {
		t.Fatal("nil overlay without an error")
	}
	for _, f := range findings {
		if f.Level != LevelWarning && f.Level != LevelError {
			t.Fatalf("finding level %q is not warning or error", f.Level)
		}
	}
}

func FuzzLoadOverlay(f *testing.F) {
	for _, seed := range seedInputs() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		assertOverlayProperty(t, data)
	})
}
