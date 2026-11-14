package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedInputs collects the fixture configs, the example config and a few
// hand-picked shapes as fuzz seeds. Seeds come from the frozen fixtures,
// so the fuzzer always starts from the real contract shapes.
func seedInputs() [][]byte {
	var seeds [][]byte
	if fixtures, err := os.ReadDir(filepath.Join("..", "..", "testdata")); err == nil {
		for _, e := range fixtures {
			path := filepath.Join("..", "..", "testdata", e.Name(), "config.yaml")
			if raw, err := os.ReadFile(path); err == nil {
				seeds = append(seeds, raw)
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join("..", "..", "example.yaml")); err == nil {
		seeds = append(seeds, raw)
	}
	seeds = append(seeds,
		[]byte(goodHeader+"hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n"),
		[]byte("apiVersion: simplex.config/v0\nvantage: {id: lab, canaries: [{host: spoke, kind: ts_reach, target: \":9100\"}]}\nhosts: [{id: spoke, checks: [{kind: ts_reach, expected: light}]}]\n"),
		[]byte(goodHeader+"hosts: [{id: edge, checks: [{kind: prom_up, expected: light, path: /-/healthy, port: 9091}]}]\nholds: [{host: \"*\", kind: \"*\", reason: x, until: \"2027-01-01T00:00:00Z\"}]\n"),
	)
	return seeds
}

// assertLoadProperty is the property every fuzz target pins: no panic,
// and every outcome is either a valid result or a typed *Error.
func assertLoadProperty(t *testing.T, data []byte) {
	t.Helper()
	cfg, findings, err := Parse(data, nil)
	if err != nil {
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("untyped error %T: %v", err, err)
		}
		if cfg != nil {
			t.Fatal("config returned alongside an error")
		}
		return
	}
	if cfg == nil {
		t.Fatal("nil config without an error")
	}
	for _, f := range findings {
		if f.Level != LevelWarning && f.Level != LevelError {
			t.Fatalf("finding level %q is not warning or error", f.Level)
		}
		if f.Level == LevelError {
			t.Fatalf("no error returned but an error finding exists: %+v", f)
		}
	}
	// The derived views must hold together on any accepted document.
	checks := cfg.EffectiveChecks()
	if len(cfg.Hosts) > 0 && len(checks) == 0 {
		t.Fatal("hosts with checks produced no effective checks")
	}
	for _, h := range cfg.Hosts {
		if h.ID == "" {
			t.Fatal("accepted a config with an empty host id")
		}
	}
	_ = cfg.ActiveHolds()
}

func FuzzLoadConfig(f *testing.F) {
	for _, seed := range seedInputs() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		assertLoadProperty(t, data)
	})
}

func FuzzValidateRefs(f *testing.F) {
	for _, seed := range seedInputs() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, _, err := Parse(data, nil)
		if err != nil {
			return // the structural pass already rejected it
		}
		findings := validateRefs(cfg)
		for _, fl := range findings {
			if fl.Level != LevelError {
				t.Fatalf("validateRefs returned a %q finding: %+v", fl.Level, fl)
			}
		}
	})
}
