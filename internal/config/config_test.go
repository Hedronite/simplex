package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// fixtureClock is the clock the S0 fixtures were built against
// (docs/semantics.md section 11: as if --now 2026-11-13T18:00:00Z).
func fixtureClock(t *testing.T) func() time.Time {
	t.Helper()
	at := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

// findingKey is the (level, path) pair a table case asserts on.
type findingKey struct {
	level Level
	path  string
}

type loadCase struct {
	name   string
	input  string
	clock  string // RFC 3339, empty means the fixture clock
	want   []findingKey
	wantIn map[findingKey]string // path -> message substring
	noErr  bool                  // load must succeed with a config
	errMsg string                // substring of the typed error
}

func runLoad(t *testing.T, tc loadCase) (*Config, []Finding, error) {
	t.Helper()
	now := fixtureClock(t)
	if tc.clock != "" {
		at, err := time.Parse(time.RFC3339, tc.clock)
		if err != nil {
			t.Fatalf("bad test clock: %v", err)
		}
		now = func() time.Time { return at }
	}
	cfg, findings, err := parse([]byte(tc.input), now)
	for _, key := range tc.want {
		if !hasFinding(findings, key.level, key.path) {
			t.Fatalf("%s: want finding %s %s, got %+v", tc.name, key.level, key.path, findings)
		}
	}
	for key, substr := range tc.wantIn {
		if !hasFinding(findings, key.level, key.path) {
			t.Fatalf("%s: want finding %s %s, got %+v", tc.name, key.level, key.path, findings)
		}
		if !findingSay(findings, key.level, key.path, substr) {
			t.Fatalf("%s: finding %s %s does not mention %q: %+v", tc.name, key.level, key.path, substr, findings)
		}
	}
	if len(tc.want) == 0 && len(tc.wantIn) == 0 && len(findings) != 0 {
		t.Fatalf("%s: want no findings, got %+v", tc.name, findings)
	}
	if tc.noErr && err != nil {
		t.Fatalf("%s: want clean load, got %v (findings %+v)", tc.name, err, findings)
	}
	if !tc.noErr && err == nil {
		t.Fatalf("%s: want a typed error, got none (findings %+v)", tc.name, findings)
	}
	if err != nil {
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("%s: error is not typed: %T %v", tc.name, err, err)
		}
		if tc.errMsg != "" && !strings.Contains(typed.Error(), tc.errMsg) {
			t.Fatalf("%s: error %q does not mention %q", tc.name, typed.Error(), tc.errMsg)
		}
		if cfg != nil {
			t.Fatalf("%s: config returned alongside an error", tc.name)
		}
	}
	return cfg, findings, err
}

func hasFinding(findings []Finding, level Level, path string) bool {
	for _, f := range findings {
		if f.Level == level && f.Path == path {
			return true
		}
	}
	return false
}

func findingSay(findings []Finding, level Level, path, substr string) bool {
	for _, f := range findings {
		if f.Level == level && f.Path == path && strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

const goodHeader = "apiVersion: simplex.config/v0\nvantage:\n  id: lab\n"

func TestLoadRules(t *testing.T) {
	cases := []loadCase{
		{
			name:  "minimal config loads clean",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			noErr: true,
		},
		{
			name:  "unknown top-level key",
			input: goodHeader + "pot: 1\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			want:  []findingKey{{LevelError, "pot"}},
		},
		{
			name:   "unknown major version",
			input:  "apiVersion: simplex.config/v1\nvantage:\n  id: lab\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{{LevelError, "apiVersion"}: "simplex.config/v1"},
		},
		{
			name:  "missing apiVersion",
			input: "vantage:\n  id: lab\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			want:  []findingKey{{LevelError, "apiVersion"}},
		},
		{
			name:  "missing vantage",
			input: "apiVersion: simplex.config/v0\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			want:  []findingKey{{LevelError, "vantage"}},
		},
		{
			name:  "vantage id not a slug",
			input: "apiVersion: simplex.config/v0\nvantage:\n  id: Lab\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "vantage.id"}: "not a slug",
			},
		},
		{
			name:  "empty hosts list",
			input: goodHeader + "hosts: []\n",
			want:  []findingKey{{LevelError, "hosts"}},
		},
		{
			name:  "host without checks",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks: []\n",
			want:  []findingKey{{LevelError, "hosts[0].checks"}},
		},
		{
			name:  "missing expected",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach}\n",
			want:  []findingKey{{LevelError, "hosts[0].checks[0].expected"}},
		},
		{
			name:  "bad expected value",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: dim}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[0].expected"}: "light",
			},
		},
		{
			name:  "path on a non-prom_up kind",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light, path: /metrics}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[0].path"}: "legal only on a prom_up check",
			},
		},
		{
			name:  "path off the allowlist",
			input: goodHeader + "hosts:\n  - id: edge\n    checks:\n      - {kind: prom_up, expected: light, path: /debug}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[0].path"}: "allowlist",
			},
		},
		{
			name:  "denied port",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light, port: 18080}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[0].port"}: "never dialed",
			},
		},
		{
			name:  "port out of range",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light, port: 70000}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[0].port"}: "between 1 and 65535",
			},
		},
		{
			name:  "port is not an integer",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light, port: \"9100\"}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[0].port"}: "want an integer",
			},
		},
		{
			name:  "canary kind must be ts_reach",
			input: goodHeader + "  canaries:\n    - {host: spoke, kind: node_exporter, target: \":9100\"}\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "vantage.canaries[0].kind"}: "ts_reach",
			},
		},
		{
			name:  "canary target malformed",
			input: goodHeader + "  canaries:\n    - {host: spoke, kind: ts_reach, target: \"9100\"}\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "vantage.canaries[0].target"}: "want a target",
			},
		},
		{
			name:  "canary host unknown",
			input: goodHeader + "  canaries:\n    - {host: ghost, kind: ts_reach, target: \":9100\"}\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "vantage.canaries[0].host"}: "not configured",
			},
		},
		{
			name:  "canary target does not match the effective target",
			input: goodHeader + "  canaries:\n    - {host: spoke, kind: ts_reach, target: \":9101\"}\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "vantage.canaries[0].target"}: "answers",
			},
		},
		{
			name:  "duplicate host id",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n  - id: spoke\n    checks:\n      - {kind: node_exporter, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[1].id"}: "duplicate host id",
			},
		},
		{
			name:  "duplicate check identity on one host",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].checks[1]"}: "duplicate check",
			},
		},
		{
			name:  "same kind twice with different ports is two checks",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: prom_up, expected: light}\n      - {kind: prom_up, expected: light, port: 9091}\n",
			noErr: true,
		},
		{
			name:  "hold without until",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\nholds:\n  - {host: spoke, kind: ts_reach, reason: maintenance}\n",
			want:  []findingKey{{LevelError, "holds[0].until"}},
		},
		{
			name:  "hold with a bad until",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\nholds:\n  - {host: spoke, kind: ts_reach, reason: maintenance, until: tomorrow}\n",
			wantIn: map[findingKey]string{
				{LevelError, "holds[0].until"}: "RFC 3339",
			},
		},
		{
			name:  "hold with an unknown kind",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\nholds:\n  - {host: spoke, kind: ping, reason: maintenance, until: \"2027-01-01T00:00:00Z\"}\n",
			wantIn: map[findingKey]string{
				{LevelError, "holds[0].kind"}: "unknown check kind",
			},
		},
		{
			name:  "hold with a bad host slug",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\nholds:\n  - {host: Spoke, kind: ts_reach, reason: maintenance, until: \"2027-01-01T00:00:00Z\"}\n",
			wantIn: map[findingKey]string{
				{LevelError, "holds[0].host"}: "slug",
			},
		},
		{
			name:  "hold without a reason",
			input: goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\nholds:\n  - {host: spoke, kind: ts_reach, until: \"2027-01-01T00:00:00Z\"}\n",
			want:  []findingKey{{LevelError, "holds[0].reason"}},
		},
		{
			name:  "not valid YAML",
			input: "apiVersion: simplex.config/v0\nvantage: [unclosed\n",
			wantIn: map[findingKey]string{
				{LevelError, ""}: "not valid YAML",
			},
		},
		{
			name:  "top level is not a mapping",
			input: "just a string\n",
			wantIn: map[findingKey]string{
				{LevelError, ""}: "top-level mapping",
			},
		},
		{
			name:  "empty document",
			input: "",
			want:  []findingKey{{LevelError, ""}},
		},
		{
			name:  "a string where a slug is wanted",
			input: goodHeader + "hosts:\n  - id: 5\n    checks:\n      - {kind: ts_reach, expected: light}\n",
			wantIn: map[findingKey]string{
				{LevelError, "hosts[0].id"}: "want a string",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLoad(t, tc)
		})
	}
}

func TestLoadHolds(t *testing.T) {
	base := goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n"
	t.Run("expired hold is a warning and stays listed", func(t *testing.T) {
		cfg, findings, _ := runLoad(t, loadCase{
			name:  "expired hold",
			input: base + "holds:\n  - {host: spoke, kind: ts_reach, reason: maintenance, until: \"2026-11-13T00:00:00Z\"}\n",
			want:  []findingKey{{LevelWarning, "holds[0].until"}},
			noErr: true,
		})
		if !hasFinding(findings, LevelWarning, "holds[0].until") {
			t.Fatalf("want an expired-hold warning, got %+v", findings)
		}
		if len(cfg.Holds) != 1 || !cfg.Holds[0].Expired {
			t.Fatalf("want one expired hold, got %+v", cfg.Holds)
		}
		if len(cfg.ActiveHolds()) != 0 {
			t.Fatalf("want no active holds, got %+v", cfg.ActiveHolds())
		}
	})
	t.Run("active hold is clean", func(t *testing.T) {
		cfg, findings, _ := runLoad(t, loadCase{
			name:  "active hold",
			input: base + "holds:\n  - {host: spoke, kind: ts_reach, reason: maintenance, until: \"2027-01-01T00:00:00Z\"}\n",
			noErr: true,
		})
		if len(findings) != 0 {
			t.Fatalf("want no findings, got %+v", findings)
		}
		if len(cfg.ActiveHolds()) != 1 {
			t.Fatalf("want one active hold, got %+v", cfg.ActiveHolds())
		}
	})
	t.Run("wildcards are accepted", func(t *testing.T) {
		runLoad(t, loadCase{
			name:  "wildcard hold",
			input: base + "holds:\n  - {host: \"*\", kind: \"*\", reason: maintenance window, until: \"2027-01-01T00:00:00Z\"}\n",
			noErr: true,
		})
	})
}

func TestHoldExpiryBoundary(t *testing.T) {
	// The hold is live only while until is strictly after the run clock:
	// until == now is expired, until == now+1s is live.
	at := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	base := goodHeader + "hosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n"

	t.Run("until equal to the run clock is expired", func(t *testing.T) {
		cfg, findings, err := Parse([]byte(base+"holds:\n  - {host: spoke, kind: ts_reach, reason: maintenance, until: \"2026-11-13T18:00:00Z\"}\n"), clock)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !hasFinding(findings, LevelWarning, "holds[0].until") {
			t.Fatalf("want an expiry warning, got %+v", findings)
		}
		if len(cfg.Holds) != 1 || !cfg.Holds[0].Expired {
			t.Fatalf("want the hold marked expired, got %+v", cfg.Holds)
		}
		if len(cfg.ActiveHolds()) != 0 {
			t.Fatalf("want no active holds, got %+v", cfg.ActiveHolds())
		}
	})
	t.Run("until one second after the run clock is live", func(t *testing.T) {
		cfg, findings, err := Parse([]byte(base+"holds:\n  - {host: spoke, kind: ts_reach, reason: maintenance, until: \"2026-11-13T18:00:01Z\"}\n"), clock)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("want no findings, got %+v", findings)
		}
		if len(cfg.ActiveHolds()) != 1 {
			t.Fatalf("want one active hold, got %+v", cfg.ActiveHolds())
		}
	})
}

func TestLoadDefaultsAndTargets(t *testing.T) {
	cfg, _, _ := runLoad(t, loadCase{
		name: "defaults and effective targets",
		input: goodHeader + "hosts:\n" +
			"  - id: spoke\n    checks:\n" +
			"      - {kind: ts_reach, expected: light}\n" +
			"      - {kind: node_exporter, expected: light}\n" +
			"      - {kind: kube_apiserver, expected: dark}\n" +
			"      - {kind: ssh, expected: light}\n" +
			"  - id: edge\n    checks:\n" +
			"      - {kind: prom_up, expected: light, path: /metrics, port: 9091}\n",
		noErr: true,
	})
	got := cfg.EffectiveChecks()
	wantTargets := map[string]string{
		"spoke ts_reach":       ":9100",
		"spoke node_exporter":  ":9100/metrics",
		"spoke kube_apiserver": ":6443",
		"spoke ssh":            ":22",
		"edge prom_up":         ":9091/metrics",
	}
	if len(got) != len(wantTargets) {
		t.Fatalf("want %d effective checks, got %d", len(wantTargets), len(got))
	}
	for _, c := range got {
		key := c.Host + " " + string(c.Kind)
		if wantTargets[key] != c.Target {
			t.Fatalf("%s: effective target %q, want %q", key, c.Target, wantTargets[key])
		}
	}
}

func TestLoadS0Fixtures(t *testing.T) {
	// Every S0 fixture config loads, except the deliberate error fixture.
	// The clock is the one the fixtures were built with.
	entries, err := os.ReadDir(filepath.Join("..", "..", "testdata"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join("..", "..", "testdata", e.Name(), "config.yaml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			cfg, findings, lerr := LoadWithClock(path, fixtureClock(t))
			if e.Name() == "config-error" {
				// A canary pointing at an unconfigured host: a loader
				// rejection the schema cannot express.
				var typed *Error
				if !errors.As(lerr, &typed) {
					t.Fatalf("want a typed config error, got %v", lerr)
				}
				if cfg != nil {
					t.Fatal("config returned alongside the error")
				}
				return
			}
			if lerr != nil {
				t.Fatalf("load: %v (findings %+v)", lerr, findings)
			}
			for _, f := range findings {
				if f.Level == LevelError {
					t.Fatalf("unexpected error finding: %+v", f)
				}
			}
			if e.Name() == "expired-hold" {
				if !hasFinding(findings, LevelWarning, "holds[0].until") {
					t.Fatalf("want an expired-hold warning, got %+v", findings)
				}
			}
		})
	}
}

func TestExampleYAMLLoads(t *testing.T) {
	cfg, findings, err := LoadWithClock(filepath.Join("..", "..", "example.yaml"), fixtureClock(t))
	if err != nil {
		t.Fatalf("load example.yaml: %v (findings %+v)", err, findings)
	}
	if len(findings) != 0 {
		t.Fatalf("example.yaml must load clean against the fixture clock, got %+v", findings)
	}
	if len(cfg.ActiveHolds()) != 1 {
		t.Fatalf("want the example's one active hold, got %+v", cfg.ActiveHolds())
	}
	if len(cfg.EffectiveChecks()) == 0 {
		t.Fatal("example.yaml has no effective checks")
	}
}

func TestExampleYAMLAgainstSchemas(t *testing.T) {
	// Covered with the rest of the corpus in agreement_test.go; this test
	// only pins that the example is one of the corpus inputs.
	raw, err := os.ReadFile(filepath.Join("..", "..", "example.yaml"))
	if err != nil {
		t.Fatalf("read example.yaml: %v", err)
	}
	cfg, _, perr := parse(raw, fixtureClock(t))
	if perr != nil || cfg == nil {
		t.Fatalf("example.yaml does not parse: %v", perr)
	}
	if !slugPattern.MatchString(cfg.Vantage.ID) {
		t.Fatalf("vantage id %q is not a slug", cfg.Vantage.ID)
	}
}

func TestDeniedPortIsContractConstant(t *testing.T) {
	if contract.DeniedPort != 18080 {
		t.Fatalf("denied port drifted: %d", contract.DeniedPort)
	}
}
