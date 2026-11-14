package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/exitcode"
	"github.com/Hedronite/simplex/internal/overlay"
	"github.com/Hedronite/simplex/internal/probe"
)

type result struct {
	code           int
	stdout, stderr string
}

func invoke(t *testing.T, deps *Deps, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb, deps)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// splitTarget turns ":9090/-/ready" into port "9090" and path "/-/ready".
func splitTarget(t *testing.T, target string) (port, path string) {
	t.Helper()
	rest := strings.TrimPrefix(target, ":")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i], rest[i:]
	}
	return rest, ""
}

// writeOverlay writes an overlay giving every configured host a placeholder
// address. When the case carries its own overlay data (private.yaml), that text
// is kept and only the hosts it does not mention are appended, so its overrides
// still apply.
func writeOverlay(t *testing.T, dir, cfgPath, base string) string {
	t.Helper()
	var b strings.Builder
	have := map[string]bool{}
	if base != "" {
		b.Write(readFile(t, base))
		ov, err := overlay.Load(base)
		if err != nil {
			t.Fatalf("load %s: %v", base, err)
		}
		for slug := range ov.Hosts {
			have[slug] = true
		}
	} else {
		b.WriteString("apiVersion: simplex.overlay/v0\nhosts:\n")
	}
	// An invalid config has no hosts to list; the run fails before the overlay is read.
	if cfg, _, err := config.Load(cfgPath); err == nil {
		for _, h := range cfg.Hosts {
			if !have[h.ID] {
				fmt.Fprintf(&b, "  %s:\n    address: %s.invalid\n", h.ID, h.ID)
			}
		}
	}
	p := filepath.Join(dir, "overlay.generated.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fixtureDeps builds fake seams from a case's document, so the real probe code
// classifies what they return and the engine derives the verdict from that.
func fixtureDeps(t *testing.T, doc *document.Document, ovPath string) *Deps {
	t.Helper()
	ov, err := overlay.Load(ovPath)
	if err != nil {
		t.Fatalf("load %s: %v", ovPath, err)
	}
	dials := map[string]plan{}
	https := map[string]plan{}
	for _, c := range doc.Checks {
		port, path := splitTarget(t, c.Target)
		var outcome contract.Reason
		for _, r := range c.Reasons {
			for _, s := range contract.ReasonSpecs {
				if s.Code == r && s.Class == contract.ClassOutcome {
					outcome = r
				}
			}
		}
		p := plan{kind: c.Kind, result: c.Result, outcome: outcome, path: path}
		addr := ov.Hosts[c.Host].Address + ":" + port
		switch c.Kind {
		case contract.KindNodeExporter, contract.KindPromUp:
			https[addr+path] = p
		default:
			dials[addr] = p
		}
	}
	runner := probe.New(&probe.Options{
		Dialer:    fakeDialer{plans: dials},
		Transport: fakeTransport{plans: https},
		Clock:     instantClock{},
	})
	socket := fixtureLocalAPI(t, doc, ov)
	return &Deps{Runner: runner, Lister: fakeLister(doc.Vantage.Status != contract.VantageBlind), Getenv: func(string) string { return "" }, Now: time.Now, TailscaleSocket: socket}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func oneErrorLine(t *testing.T, stderr string, wantCode int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr has %d lines, want exactly one error line: %q", len(lines), stderr)
	}
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("stderr is not one JSON error object: %q (%v)", lines[0], err)
	}
	if e.Error.Code != wantCode || e.Error.Message == "" {
		t.Fatalf("error line %+v, want code %d and a message", e, wantCode)
	}
}

// Every S0 fixture case reproduces through `check --json`, byte for byte, with
// its exit code. The seams are derived from each case's document; the shipped
// binary has no way to feed it observations.
func TestFixturesReproduceThroughCheck(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			wantCode, err := strconv.Atoi(strings.TrimSpace(string(readFile(t, filepath.Join(dir, "exit")))))
			if err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(dir, "config.yaml")
			tmp := t.TempDir()
			base := filepath.Join(dir, "private.yaml")
			if _, err := os.Stat(base); err != nil {
				base = ""
			}
			ovPath := writeOverlay(t, tmp, cfgPath, base)

			docPath := filepath.Join(dir, "document.json")
			if !exitcode.WritesDocument(wantCode) {
				// Error cases: no document, exactly one error line.
				deps := &Deps{Runner: probe.New(&probe.Options{
					Dialer: fakeDialer{plans: map[string]plan{"spoke.invalid:9100": {kind: contract.KindTSReach, result: contract.ResultUp, outcome: contract.ReasonTCPConnectOK}}},
					Clock:  instantClock{},
				}), Lister: fakeLister(true), Now: time.Now, Getenv: func(string) string { return "" }}
				args := []string{"check", "--json", "--config", cfgPath, "--overlay", ovPath, "--now", "2026-11-13T18:00:00Z"}
				if wantCode == exitcode.Runtime {
					// An I/O error: the output directory does not exist.
					args = append(args, "--out", filepath.Join(tmp, "missing", "probe.json"))
				}
				r := invoke(t, deps, args...)
				if r.code != wantCode {
					t.Fatalf("exit %d, want %d (stderr %q)", r.code, wantCode, r.stderr)
				}
				if r.stdout != "" {
					t.Fatalf("stdout must be empty on exit %d, got %q", wantCode, r.stdout)
				}
				oneErrorLine(t, r.stderr, wantCode)
				return
			}

			raw := readFile(t, docPath)
			var doc document.Document
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			deps := fixtureDeps(t, &doc, ovPath)
			r := invoke(t, deps, "check", "--json", "--config", cfgPath, "--overlay", ovPath, "--now", doc.GeneratedAt)
			if r.code != wantCode {
				t.Fatalf("exit %d, want %d (stderr %q)\nstdout:\n%s", r.code, wantCode, r.stderr, r.stdout)
			}
			if r.stdout != string(raw) {
				t.Fatalf("stdout differs from %s\n--- got\n%s--- want\n%s", docPath, r.stdout, raw)
			}
		})
	}
}

func fakeRunner(plans map[string]plan) *probe.Runner {
	return probe.New(&probe.Options{Dialer: fakeDialer{plans: plans}, Clock: instantClock{}})
}

func tinyFiles(t *testing.T) (cfg, ov string) {
	t.Helper()
	dir := t.TempDir()
	cfg = filepath.Join(dir, "c.yaml")
	ov = filepath.Join(dir, "o.txt")
	if err := os.WriteFile(cfg, []byte("apiVersion: simplex.config/v0\nvantage:\n  id: lab\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: spoke.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, ov
}

func upDeps() *Deps {
	return &Deps{
		Runner: fakeRunner(map[string]plan{"spoke.invalid:9100": {kind: contract.KindTSReach, result: contract.ResultUp, outcome: contract.ReasonTCPConnectOK}}),
		Lister: fakeLister(true),
		Now:    func() time.Time { return time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC) },
		Getenv: func(string) string { return "" },
	}
}

func TestUsageErrorsExitThreeWithOneErrorLine(t *testing.T) {
	cfg, ov := tinyFiles(t)
	cases := map[string][]string{
		"unknown flag":    {"check", "--json", "--nope"},
		"unknown command": {"frobnicate", "--json"},
		"extra argument":  {"check", "--json", "--config", cfg, "stray"},
		"bad now":         {"check", "--json", "--config", cfg, "--overlay", ov, "--now", "yesterday"},
		"missing config":  {"check", "--json", "--config", filepath.Join(t.TempDir(), "absent.yaml"), "--overlay", ov},
		"missing overlay": {"check", "--json", "--config", cfg, "--overlay", filepath.Join(t.TempDir(), "absent.txt")},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := invoke(t, upDeps(), args...)
			if r.code != exitcode.Config {
				t.Fatalf("exit %d, want 3 (stderr %q)", r.code, r.stderr)
			}
			if r.stdout != "" {
				t.Fatalf("stdout not empty: %q", r.stdout)
			}
			if wantsJSON(args) {
				oneErrorLine(t, r.stderr, exitcode.Config)
			} else if !strings.HasPrefix(r.stderr, "simplex: ") {
				t.Fatalf("plain error expected, got %q", r.stderr)
			}
		})
	}
}

func TestNoErrorPathExitsWithASeverityCode(t *testing.T) {
	cfg, ov := tinyFiles(t)
	bad := [][]string{
		{"check", "--json", "--bogus"}, {"nope"}, {"check", "--json", "--config", "/nonexistent/x.yaml", "--overlay", ov},
		{"check", "--json", "--config", cfg, "--overlay", ov, "--out", filepath.Join(t.TempDir(), "no", "dir", "p.json")},
	}
	for _, args := range bad {
		if c := invoke(t, upDeps(), args...).code; c < exitcode.Config {
			t.Fatalf("%v exited %d, an error path must exit 3 or 4", args, c)
		}
	}
}

func TestMissingOverlayFailsClosedNamingSlugs(t *testing.T) {
	cfg, _ := tinyFiles(t)
	t.Chdir(t.TempDir())
	r := invoke(t, upDeps(), "check", "--json", "--config", cfg)
	if r.code != exitcode.Config || !strings.Contains(r.stderr, "spoke") || r.stdout != "" {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestOverlayResolutionOrder(t *testing.T) {
	cfg, ov := tinyFiles(t)
	t.Chdir(t.TempDir())
	deps := upDeps()
	deps.Getenv = func(k string) string {
		if k == overlayEnv {
			return ov
		}
		return ""
	}
	if r := invoke(t, deps, "check", "--json", "--config", cfg); r.code != exitcode.Green {
		t.Fatalf("env overlay: exit %d stderr %q", r.code, r.stderr)
	}
	// The default file is used when neither flag nor env is set.
	deps.Getenv = func(string) string { return "" }
	if err := os.WriteFile(defaultOverlay, readFile(t, ov), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, deps, "check", "--json", "--config", cfg); r.code != exitcode.Green {
		t.Fatalf("default overlay: exit %d stderr %q", r.code, r.stderr)
	}
}

func TestOutFileMatchesStdoutAndFollowsTheWriteRules(t *testing.T) {
	cfg, ov := tinyFiles(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "probe.json")

	r := invoke(t, upDeps(), "check", "--json", "--config", cfg, "--overlay", ov, "--out", out)
	if r.code != exitcode.Green {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	if got := string(readFile(t, out)); got != r.stdout {
		t.Fatalf("--out file differs from stdout:\n%s\nvs\n%s", got, r.stdout)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v err %v, want 0644", fi.Mode().Perm(), err)
	}

	// Exit 3 leaves the previous file byte-identical.
	before := readFile(t, out)
	if r := invoke(t, upDeps(), "check", "--json", "--config", filepath.Join(dir, "absent.yaml"), "--overlay", ov, "--out", out); r.code != exitcode.Config {
		t.Fatalf("exit %d", r.code)
	}
	if !bytes.Equal(before, readFile(t, out)) {
		t.Fatal("exit 3 changed the --out file")
	}

	// An unwritable target is exit 4 with nothing on stdout, and leaves no temp file.
	r = invoke(t, upDeps(), "check", "--json", "--config", cfg, "--overlay", ov, "--out", filepath.Join(dir, "no", "such", "probe.json"))
	if r.code != exitcode.Runtime || r.stdout != "" {
		t.Fatalf("exit %d stdout %q", r.code, r.stdout)
	}
	oneErrorLine(t, r.stderr, exitcode.Runtime)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".simplex-out-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestBlindWritesTheDocumentAndExitsFive(t *testing.T) {
	cfg, ov := tinyFiles(t)
	deps := upDeps()
	deps.Lister = fakeLister(false)
	out := filepath.Join(t.TempDir(), "probe.json")
	r := invoke(t, deps, "check", "--json", "--config", cfg, "--overlay", ov, "--out", out)
	if r.code != exitcode.Blind {
		t.Fatalf("exit %d, want 5", r.code)
	}
	var d document.Document
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatal(err)
	}
	if d.Vantage.Status != contract.VantageBlind || d.Overall != contract.SeverityRed || d.Checks[0].Result != contract.ResultUnknown {
		t.Fatalf("blind document wrong: %+v", d)
	}
	if string(readFile(t, out)) != r.stdout {
		t.Fatal("exit 5 must write the --out file")
	}
}

// Canaries from the config become real probes through the same runner: one
// that answers makes the vantage ok, and one that does not makes it blind and
// exits 5. Only the interface lister and the dial results are faked.
func TestConfigCanariesDriveTheVantage(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.yaml")
	ov := filepath.Join(dir, "o.txt")
	if err := os.WriteFile(cfg, []byte("apiVersion: simplex.config/v0\nvantage:\n  id: lab\n  canaries:\n    - {host: spoke, kind: ts_reach, target: \":9100\"}\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: spoke.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	answering := map[string]plan{"spoke.invalid:9100": {kind: contract.KindTSReach, result: contract.ResultUp, outcome: contract.ReasonTCPConnectOK}}
	silent := map[string]plan{"spoke.invalid:9100": {kind: contract.KindTSReach, result: contract.ResultUnknown, outcome: contract.ReasonTCPConnectTimeout}}
	for name, tc := range map[string]struct {
		plans  map[string]plan
		lister bool
		status contract.VantageStatus
		code   int
	}{
		"canary answers":     {answering, true, contract.VantageOK, exitcode.Green},
		"canary silent":      {silent, true, contract.VantageBlind, exitcode.Blind},
		"no tailnet address": {answering, false, contract.VantageBlind, exitcode.Blind},
	} {
		t.Run(name, func(t *testing.T) {
			deps := &Deps{Runner: fakeRunner(tc.plans), Lister: fakeLister(tc.lister), Now: time.Now, Getenv: func(string) string { return "" }}
			r := invoke(t, deps, "check", "--json", "--config", cfg, "--overlay", ov, "--now", "2026-11-13T18:00:00Z")
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d (stderr %q)", r.code, tc.code, r.stderr)
			}
			var d document.Document
			if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
				t.Fatal(err)
			}
			if d.Vantage.Status != tc.status {
				t.Fatalf("status %s, want %s", d.Vantage.Status, tc.status)
			}
		})
	}
}

// The real dialer path, with no fakes in the seams that matter: a loopback
// listener answers, so the check is up with tcp_connect_ok.
func TestRealDialerAgainstALoopbackListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.yaml")
	ov := filepath.Join(dir, "o.txt")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("apiVersion: simplex.config/v0\nvantage:\n  id: lab\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light, port: %d}\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := &Deps{
		Runner: probe.Default(),
		Lister: fakeLister(true),
		Now:    time.Now,
		Getenv: func(string) string { return "" },
	}
	r := invoke(t, deps, "check", "--json", "--config", cfg, "--overlay", ov)
	if r.code != exitcode.Green {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, `"result": "up"`) || !strings.Contains(r.stdout, "tcp_connect_ok") || !strings.Contains(r.stdout, `"status": "unchecked"`) {
		t.Fatalf("unexpected document:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "127.0.0.1") {
		t.Fatalf("the overlay address reached the document:\n%s", r.stdout)
	}
}

// fixtureLocalAPI starts a fake tailnet daemon for a case whose document carries
// enrichment codes, with one peer per matched ts_reach check built from the codes
// (documentation values only). It returns the socket path, or "" when the case
// has no enrichment.
func fixtureLocalAPI(t *testing.T, doc *document.Document, ov *overlay.Overlay) string {
	t.Helper()
	var peers []string
	enriched := false
	for _, c := range doc.Checks {
		if c.Kind != contract.KindTSReach {
			continue
		}
		has := func(r contract.Reason) bool {
			for _, x := range c.Reasons {
				if x == r {
					return true
				}
			}
			return false
		}
		if !has(contract.ReasonTSPeerOnline) && !has(contract.ReasonTSPeerOffline) && !has(contract.ReasonTSPeerUnmatched) {
			continue
		}
		enriched = true
		if has(contract.ReasonTSPeerUnmatched) {
			continue
		}
		cur, relay := "", ""
		if has(contract.ReasonTSPathDirect) {
			cur = "198.51.100.1:41641"
		}
		if has(contract.ReasonTSPathRelayed) {
			relay = "mia"
		}
		peers = append(peers, fmt.Sprintf(`"nodekey:%s":{"DNSName":"%s.","Online":%t,"CurAddr":"%s","Relay":"%s","PublicKey":"nodekey:fixture","Addrs":["198.51.100.2:1"]}`,
			c.Host, ov.Hosts[c.Host].Address, has(contract.ReasonTSPeerOnline), cur, relay))
	}
	if !enriched {
		return ""
	}
	dir, err := os.MkdirTemp("/tmp", "sx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"BackendState":"Running","Self":{"TailscaleIPs":["203.0.113.7"],"DNSName":"vantage.example."},"Peer":{` + strings.Join(peers, ",") + `}}`
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}), ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}
