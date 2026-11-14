package overlay

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
)

func TestLoadOverlayRules(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantIn map[string]string // finding path -> message substring
		clean  bool
	}{
		{
			name:  "minimal overlay loads clean",
			input: "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n",
			clean: true,
		},
		{
			name:   "unknown top-level key",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\npot: 1\n",
			wantIn: map[string]string{"pot": "unknown key"},
		},
		{
			name:   "unknown major",
			input:  "apiVersion: simplex.overlay/v1\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n",
			wantIn: map[string]string{"apiVersion": "unsupported apiVersion"},
		},
		{
			name:   "missing apiVersion",
			input:  "hosts:\n  spoke:\n    address: \"tailnet-spoke\"\n",
			wantIn: map[string]string{"apiVersion": "required"},
		},
		{
			name:   "missing address",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke: {}\n",
			wantIn: map[string]string{"hosts.spoke.address": "required"},
		},
		{
			name:   "empty address",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"\"\n",
			wantIn: map[string]string{"hosts.spoke.address": "required"},
		},
		{
			name:   "bad slug key",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  Spoke:\n    address: \"tailnet-spoke\"\n",
			wantIn: map[string]string{"hosts.Spoke": "not a slug"},
		},
		{
			name:   "denied port override",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    overrides:\n      - {kind: ts_reach, port: 18080}\n",
			wantIn: map[string]string{"hosts.spoke.overrides[0].port": "never dialed"},
		},
		{
			name:   "path on a non-prom_up override",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    overrides:\n      - {kind: ts_reach, path: /metrics}\n",
			wantIn: map[string]string{"hosts.spoke.overrides[0].path": "legal only on a prom_up check"},
		},
		{
			name:   "override path off the allowlist",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  edge:\n    address: \"tailnet-spoke\"\n    overrides:\n      - {kind: prom_up, path: /debug}\n",
			wantIn: map[string]string{"hosts.edge.overrides[0].path": "allowlist"},
		},
		{
			name:   "override target malformed",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    overrides:\n      - {kind: ts_reach, target: \"9100\"}\n",
			wantIn: map[string]string{"hosts.spoke.overrides[0].target": "want a target"},
		},
		{
			name:   "override kind unknown",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    overrides:\n      - {kind: ping}\n",
			wantIn: map[string]string{"hosts.spoke.overrides[0].kind": "unknown check kind"},
		},
		{
			name:   "lan_address empty",
			input:  "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    lan_address: \"\"\n",
			wantIn: map[string]string{"hosts.spoke.lan_address": "non-empty"},
		},
		{
			name:   "not valid YAML",
			input:  "apiVersion: [unclosed\n",
			wantIn: map[string]string{"": "not valid YAML"},
		},
		{
			name:   "empty document",
			input:  "",
			wantIn: map[string]string{"": "empty document"},
		},
		{
			name:   "hosts is not a mapping",
			input:  "apiVersion: simplex.overlay/v0\nhosts: [spoke]\n",
			wantIn: map[string]string{"hosts": "want a mapping"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ov, findings, err := parse([]byte(tc.input))
			if tc.clean {
				if err != nil || len(findings) != 0 {
					t.Fatalf("want clean load, got err=%v findings=%+v", err, findings)
				}
				if ov == nil {
					t.Fatal("nil overlay without an error")
				}
				return
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("want a typed error, got %T %v", err, err)
			}
			if ov != nil {
				t.Fatal("overlay returned alongside an error")
			}
			for path, substr := range tc.wantIn {
				found := false
				for _, f := range typed.Findings {
					if f.Path == path && strings.Contains(f.Message, substr) {
						found = true
					}
				}
				if !found {
					t.Fatalf("want finding on %q mentioning %q, got %+v", path, substr, typed.Findings)
				}
			}
		})
	}
}

func TestDuplicateKeysRejected(t *testing.T) {
	_, _, err := parse([]byte("apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: \"tailnet-spoke\"\n    address: \"tailnet-lab\"\n"))
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("want a typed error, got %v", err)
	}
	if !strings.Contains(typed.Error(), "duplicate") {
		t.Fatalf("want a duplicate-key finding, got %q", typed.Error())
	}
}

func TestLoadReadFailureIsTyped(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.overlay.yaml"))
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("want a typed error, got %T %v", err, err)
	}
	if !strings.Contains(typed.Error(), "cannot read overlay") {
		t.Fatalf("want a read failure message, got %q", typed.Error())
	}
}

func fixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	raw := `
apiVersion: simplex.config/v0
vantage:
  id: lab
hosts:
  - id: spoke
    checks:
      - {kind: ts_reach, expected: light}
      - {kind: lan_reach, expected: light}
      - {kind: node_exporter, expected: light}
      - {kind: kube_apiserver, expected: dark}
  - id: edge
    checks:
      - {kind: prom_up, expected: light}
  - id: lab
    checks:
      - {kind: prom_up, expected: light}
`
	cfg, findings, err := config.Parse([]byte(raw), func() time.Time { return time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC) })
	if err != nil || len(findings) != 0 {
		t.Fatalf("fixture config does not load: %v %+v", err, findings)
	}
	return cfg
}

func TestResolve(t *testing.T) {
	ov, _, err := parse([]byte("apiVersion: simplex.overlay/v0\nhosts:\n" +
		"  spoke:\n" +
		"    address: \"tailnet-spoke\"\n" +
		"    lan_address: \"lan-spoke\"\n" +
		"    overrides:\n" +
		"      - {kind: ts_reach, port: 9101}\n" +
		"      - {kind: node_exporter, port: 9102}\n" +
		"  edge:\n" +
		"    address: \"tailnet-edge\"\n" +
		"    overrides:\n" +
		"      - {kind: prom_up, target: \":9090/-/ready\", path: /metrics, port: 9091}\n" +
		"  lab:\n" +
		"    address: \"tailnet-lab\"\n"))
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	cfg := fixtureConfig(t)
	got, err := ov.Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := map[string]Resolved{
		"spoke ts_reach":       {Address: "tailnet-spoke", Port: 9101, Path: "", Target: ":9101"},
		"spoke lan_reach":      {Address: "lan-spoke", Port: 22, Path: "", Target: ":22"},
		"spoke node_exporter":  {Address: "tailnet-spoke", Port: 9102, Path: "/metrics", Target: ":9102/metrics"},
		"spoke kube_apiserver": {Address: "tailnet-spoke", Port: 6443, Path: "", Target: ":6443"},
		"edge prom_up":         {Address: "tailnet-edge", Port: 9091, Path: "/metrics", Target: ":9091/metrics"},
		"lab prom_up":          {Address: "tailnet-lab", Port: 9090, Path: "/-/ready", Target: ":9090/-/ready"},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d resolved checks, got %d: %+v", len(want), len(got), got)
	}
	for _, r := range got {
		key := r.Host + " " + string(r.Kind)
		w, ok := want[key]
		if !ok {
			t.Fatalf("unexpected resolved check %q", key)
		}
		if r.Address != w.Address || r.Port != w.Port || r.Path != w.Path || r.Target != w.Target {
			t.Fatalf("%s: got address=%q port=%d path=%q target=%q, want address=%q port=%d path=%q target=%q",
				key, r.Address, r.Port, r.Path, r.Target, w.Address, w.Port, w.Path, w.Target)
		}
	}
}

func TestResolveMissingSlugs(t *testing.T) {
	ov, _, err := parse([]byte("apiVersion: simplex.overlay/v0\nhosts:\n  edge:\n    address: \"tailnet-edge\"\n"))
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	cfg := fixtureConfig(t)
	_, rerr := ov.Resolve(cfg)
	var missing *MissingSlugsError
	if !errors.As(rerr, &missing) {
		t.Fatalf("want a missing-slugs error, got %T %v", rerr, rerr)
	}
	// Every configured host without an entry is named, sorted.
	if strings.Join(missing.Slugs, ",") != "lab,spoke" {
		t.Fatalf("missing slugs %v are not the unconfigured ones, sorted", missing.Slugs)
	}
}

func TestResolveRefusesDeniedPort(t *testing.T) {
	// The loader rejects a denied port, so Resolve can only see one if a
	// future refactor drops the load-time check. Resolve refuses it too,
	// so no code path can turn a config into a dial of that port.
	ov := &Overlay{Hosts: map[string]Host{
		"spoke": {Address: "tailnet-spoke", Overrides: []Override{{Kind: contract.KindTSReach, Port: contract.DeniedPort, HasPort: true}}},
		"edge":  {Address: "tailnet-edge"},
		"lab":   {Address: "tailnet-lab"},
	}}
	cfg := fixtureConfig(t)
	_, rerr := ov.Resolve(cfg)
	if rerr == nil {
		t.Fatal("resolve accepted the denied port")
	}
	var typed *Error
	if !errors.As(rerr, &typed) {
		t.Fatalf("want a typed error, got %T %v", rerr, rerr)
	}
}

func TestResolveLaterOverrideWins(t *testing.T) {
	ov, _, err := parse([]byte("apiVersion: simplex.overlay/v0\nhosts:\n" +
		"  spoke:\n" +
		"    address: \"tailnet-spoke\"\n" +
		"    overrides:\n" +
		"      - {kind: ts_reach, port: 9101}\n" +
		"      - {kind: ts_reach, port: 9102}\n" +
		"  edge:\n" +
		"    address: \"tailnet-edge\"\n" +
		"  lab:\n" +
		"    address: \"tailnet-lab\"\n"))
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	cfg := fixtureConfig(t)
	got, err := ov.Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Port != 9102 {
		t.Fatalf("want the last matching override to win, got port %d", got[0].Port)
	}
}

func TestResolveTargetScopedOverrideOnlyMatchesItsTarget(t *testing.T) {
	ov, _, err := parse([]byte("apiVersion: simplex.overlay/v0\nhosts:\n" +
		"  spoke:\n" +
		"    address: \"tailnet-spoke\"\n" +
		"    overrides:\n" +
		"      - {kind: prom_up, target: \":9091/-/ready\", path: /metrics}\n"))
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	cfg, _, perr := config.Parse([]byte(`
apiVersion: simplex.config/v0
vantage:
  id: lab
hosts:
  - id: spoke
    checks:
      - {kind: prom_up, expected: light}
      - {kind: prom_up, expected: light, port: 9091}
`), nil)
	if perr != nil {
		t.Fatalf("config does not load: %v", perr)
	}
	got, err := ov.Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Path != "/-/ready" {
		t.Fatalf("the :9090 check must keep its default path, got %q", got[0].Path)
	}
	if got[0].Target != ":9090/-/ready" {
		t.Fatalf("the :9090 check target drifted: %q", got[0].Target)
	}
	if got[1].Path != "/metrics" || got[1].Target != ":9091/metrics" {
		t.Fatalf("the :9091 override did not apply: %+v", got[1])
	}
}
