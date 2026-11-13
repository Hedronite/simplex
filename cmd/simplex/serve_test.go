package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/exitcode"
	"github.com/Hedronite/simplex/internal/metrics"
)

// Sentinels are reserved documentation values written as plain literals.
const (
	sentinelAddr = "192.0.2.55"
	sentinelHost = "box.tail.example"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type server struct {
	addr   string
	cancel context.CancelFunc
	done   chan int
	stderr *syncBuffer
}

// startServe runs `simplex serve` in process on a loopback port chosen by the
// kernel and returns once it is listening.
func startServe(t *testing.T, deps *Deps, extra ...string) *server {
	t.Helper()
	ready := make(chan net.Addr, 1)
	deps.Ready = func(a net.Addr) { ready <- a }
	if deps.Now == nil {
		deps.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	sv := &server{cancel: cancel, done: make(chan int, 1), stderr: &syncBuffer{}}
	args := append([]string{"serve", "--listen", "127.0.0.1:0"}, extra...)
	go func() { sv.done <- run(ctx, args, io.Discard, sv.stderr, deps) }()
	select {
	case a := <-ready:
		sv.addr = a.String()
	case code := <-sv.done:
		cancel()
		t.Fatalf("serve exited %d before listening: %s", code, sv.stderr.String())
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("serve did not start listening")
	}
	t.Cleanup(func() { cancel() })
	return sv
}

func (s *server) stop(t *testing.T) int {
	t.Helper()
	s.cancel()
	select {
	case code := <-s.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
		return -1
	}
}

func (s *server) get(t *testing.T, method, path string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+s.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// oneErrorText asserts stderr is exactly one plain error line. serve has no
// --json flag, so its startup errors are text.
func oneErrorText(t *testing.T, stderr string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "simplex: ") {
		t.Fatalf("want exactly one \"simplex: ...\" line, got %q", stderr)
	}
}

func serveFiles(t *testing.T, addr string) (cfg, ov string) {
	t.Helper()
	dir := t.TempDir()
	cfg = filepath.Join(dir, "c.yaml")
	ov = filepath.Join(dir, "o.txt")
	if err := os.WriteFile(cfg, []byte("apiVersion: simplex.config/v0\nvantage:\n  id: lathe\nhosts:\n  - id: tower\n    checks:\n      - {kind: ts_reach, expected: light}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  tower:\n    address: "+addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, ov
}

func serveDeps(addr string, outcome contract.Reason) *Deps {
	res := contract.ResultUp
	if outcome != contract.ReasonTCPConnectOK && outcome != contract.ReasonTCPConnectRefused {
		res = contract.ResultUnknown
	}
	return &Deps{
		Runner: fakeRunner(map[string]plan{addr + ":9100": {kind: contract.KindTSReach, result: res, outcome: outcome}}),
		Lister: fakeLister(true),
		Getenv: func(string) string { return "" },
	}
}

var fast = []string{"--interval", "80ms", "--deadline", "60ms"}

func TestServeRefusesBadFlagsWithExitThree(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	cases := map[string][]string{
		"zero interval":            {"serve", "--config", cfg, "--overlay", ov, "--interval", "0s"},
		"deadline beyond interval": {"serve", "--config", cfg, "--overlay", ov, "--interval", "1s", "--deadline", "2s"},
		"malformed listen":         {"serve", "--config", cfg, "--overlay", ov, "--listen", "nonsense"},
		"wildcard needs the flag":  {"serve", "--config", cfg, "--overlay", ov, "--listen", "0.0.0.0:0"},
		"ipv6 wildcard":            {"serve", "--config", cfg, "--overlay", ov, "--listen", "[::]:0"},
		"documentation address":    {"serve", "--config", cfg, "--overlay", ov, "--listen", "192.0.2.1:0"},
		"a name that is not local": {"serve", "--config", cfg, "--overlay", ov, "--listen", "metrics.example:0"},
		"stray argument":           {"serve", "--config", cfg, "--overlay", ov, "stray"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			ready := false
			deps := serveDeps(sentinelAddr, contract.ReasonTCPConnectOK)
			deps.Ready = func(net.Addr) { ready = true }
			deps.Now = time.Now
			r := invoke(t, deps, args...)
			if r.code != exitcode.Config || ready || r.stdout != "" {
				t.Fatalf("exit %d ready=%v stdout=%q stderr=%q", r.code, ready, r.stdout, r.stderr)
			}
			oneErrorText(t, r.stderr)
		})
	}
}

func TestServeAcceptsLoopbackForms(t *testing.T) {
	for _, l := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		ok, err := listenAllowed(l)
		if err != nil || !ok {
			t.Errorf("%s: allowed=%v err=%v", l, ok, err)
		}
	}
	for _, l := range []string{"0.0.0.0:0", "[::]:0", "192.0.2.1:80", "metrics.example:80"} {
		ok, err := listenAllowed(l)
		if err != nil || ok {
			t.Errorf("%s: allowed=%v err=%v, want refused", l, ok, err)
		}
	}
}

func TestServeUnsafeFlagAllowsAndWarnsOnEveryStart(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	for i := 0; i < 2; i++ {
		deps := serveDeps(sentinelAddr, contract.ReasonTCPConnectOK)
		ready := make(chan net.Addr, 1)
		deps.Ready = func(a net.Addr) { ready <- a }
		deps.Now = time.Now
		ctx, cancel := context.WithCancel(context.Background())
		errb := &syncBuffer{}
		done := make(chan int, 1)
		go func() {
			done <- run(ctx, append([]string{"serve", "--config", cfg, "--overlay", ov, "--listen", "0.0.0.0:0", "--" + unsafeListenFlag}, fast...), io.Discard, errb, deps)
		}()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("not listening")
		}
		cancel()
		if code := <-done; code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		if !strings.Contains(errb.String(), "warning: --"+unsafeListenFlag) {
			t.Fatalf("start %d printed no unsafe warning: %q", i, errb.String())
		}
	}
}

func TestServeStartupErrorsExitBeforeListening(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: simplex.config/v0\nvantage:\n  id: lathe\n  canaries:\n    - {host: ghost, kind: ts_reach, target: \":9100\"}\nhosts:\n  - id: tower\n    checks:\n      - {kind: ts_reach, expected: light}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"invalid config":  {"serve", "--config", bad, "--overlay", ov},
		"missing config":  {"serve", "--config", filepath.Join(t.TempDir(), "absent.yaml"), "--overlay", ov},
		"missing overlay": {"serve", "--config", cfg, "--overlay", filepath.Join(t.TempDir(), "absent.txt")},
	} {
		t.Run(name, func(t *testing.T) {
			listened := false
			deps := serveDeps(sentinelAddr, contract.ReasonTCPConnectOK)
			deps.Ready = func(net.Addr) { listened = true }
			deps.Now = time.Now
			r := invoke(t, deps, append(args, "--listen", "127.0.0.1:0")...)
			if r.code != exitcode.Config || listened {
				t.Fatalf("exit %d listened=%v stderr=%q", r.code, listened, r.stderr)
			}
			oneErrorText(t, r.stderr)
			if strings.Contains(r.stderr, "ghost") {
				t.Fatalf("a startup error repeated a value from the file: %q", r.stderr)
			}
		})
	}
}

func TestServeTakenPortExitsFourNamingTheAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cfg, ov := serveFiles(t, sentinelAddr)
	r := invoke(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK), "serve", "--config", cfg, "--overlay", ov, "--listen", ln.Addr().String())
	if r.code != exitcode.Runtime {
		t.Fatalf("exit %d, want 4 (stderr %q)", r.code, r.stderr)
	}
	oneErrorText(t, r.stderr)
	if !strings.Contains(r.stderr, ln.Addr().String()) {
		t.Fatalf("the bind error must name the address: %q", r.stderr)
	}
}

func TestServeServesTheSnapshotAndTheDocumentFile(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	out := filepath.Join(t.TempDir(), "probe.json")
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK),
		append([]string{"--config", cfg, "--overlay", ov, "--out", out}, fast...)...)

	var body string
	waitFor(t, "the first snapshot", func() bool {
		_, _, b := sv.get(t, http.MethodGet, "/metrics")
		body = b
		return strings.Contains(b, "simplex_last_run_timestamp_seconds ")
	})
	status, hdr, _ := sv.get(t, http.MethodGet, "/metrics")
	if status != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status %d content type %q", status, hdr.Get("Content-Type"))
	}
	if !strings.Contains(body, "simplex_build_info{") || !strings.Contains(body, "simplex_run_errors_total 0") {
		t.Fatalf("missing process series:\n%s", body)
	}

	// The document file and /metrics come from the same snapshot: the check
	// series rendered from the file match what is served.
	raw := readFile(t, out)
	var doc document.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want, err := metrics.String(&metrics.Snapshot{Doc: &doc, RunEnded: time.Now(), Version: version, Commit: commit}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range []string{"simplex_check_severity", "simplex_check_result", "simplex_check_reason", "simplex_host_severity", "simplex_overall_severity", "simplex_vantage_status"} {
		if got, wantLines := seriesOf(body, fam), seriesOf(want, fam); got != wantLines {
			t.Fatalf("family %s differs between /metrics and the --out document\n--- served\n%s--- from file\n%s", fam, got, wantLines)
		}
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("out file mode %v err %v", fi.Mode().Perm(), err)
	}
	if strings.Contains(body, sentinelAddr) || strings.Contains(string(raw), sentinelAddr) {
		t.Fatal("the overlay address reached the metrics or the document file")
	}
	if code := sv.stop(t); code != 0 {
		t.Fatalf("exit %d on stop", code)
	}
}

func seriesOf(body, family string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, family+"{") || strings.HasPrefix(line, family+" ") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func TestServeHandlerServesOnlyGetMetrics(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK), append([]string{"--config", cfg, "--overlay", ov}, fast...)...)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodPut, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodHead, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/", http.StatusNotFound},
		{http.MethodGet, "/metrics/extra", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusNotFound},
	} {
		status, hdr, _ := sv.get(t, c.method, c.path)
		if status != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, status, c.want)
		}
		if c.want == http.StatusMethodNotAllowed && hdr.Get("Allow") != http.MethodGet {
			t.Errorf("%s %s: Allow = %q", c.method, c.path, hdr.Get("Allow"))
		}
	}
	sv.stop(t)
}

func TestServeColdStartHasOnlyTheProcessSeries(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	// A long interval and a dialer that blocks until cancelled keep the first
	// pass from finishing, so the scrape sees the cold state.
	deps := serveDeps(sentinelAddr, contract.ReasonTCPConnectOK)
	sv := startServe(t, deps, "--config", cfg, "--overlay", ov, "--interval", "1m", "--deadline", "50s")
	_, _, body := sv.get(t, http.MethodGet, "/metrics")
	if strings.Contains(body, "go_") || strings.Contains(body, "process_") {
		t.Fatalf("runtime series are not promised:\n%s", body)
	}
	if !strings.Contains(body, "simplex_build_info{") {
		t.Fatalf("no build info:\n%s", body)
	}
	sv.stop(t)
}

func TestServeFailureAfterStartupIsCountedAndNonFatal(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK), append([]string{"--config", cfg, "--overlay", ov}, fast...)...)
	waitFor(t, "the first good snapshot", func() bool {
		_, _, b := sv.get(t, http.MethodGet, "/metrics")
		return strings.Contains(b, "simplex_last_run_timestamp_seconds ")
	})
	// Break the overlay with a key that is a value, so the journal could leak it.
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\n"+sentinelAddr+": x\nhosts:\n  tower:\n    address: "+sentinelAddr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var body string
	waitFor(t, "a counted failure", func() bool {
		_, _, body = sv.get(t, http.MethodGet, "/metrics")
		return !strings.Contains(body, "simplex_run_errors_total 0")
	})
	if !strings.Contains(body, "simplex_check_severity{") || !strings.Contains(body, "simplex_document_age_seconds ") {
		t.Fatalf("the last good snapshot must stay served:\n%s", body)
	}
	if strings.Contains(sv.stderr.String(), sentinelAddr) || strings.Contains(body, sentinelAddr) {
		t.Fatalf("a value from the private overlay reached the journal or the metrics:\njournal: %s\nmetrics: %s", sv.stderr.String(), body)
	}
	if !strings.Contains(sv.stderr.String(), "run failed source=overlay") {
		t.Fatalf("the failure was not logged by field path: %q", sv.stderr.String())
	}
	// Still running, and a fixed file recovers.
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  tower:\n    address: "+sentinelAddr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recovery", func() bool {
		return strings.Contains(sv.stderr.String(), "run ok") && strings.Count(sv.stderr.String(), "run ok") >= 2
	})
	if code := sv.stop(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Whatever fails, nothing from the private overlay reaches the journal, the
// metrics or the document file.
func TestServeNeverEchoesOverlayValues(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	dir := t.TempDir()
	out := filepath.Join(dir, "probe.json")
	// Dial failures with the address in the dialer's own error text.
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectTimeout), append([]string{"--config", cfg, "--overlay", ov, "--out", out}, fast...)...)
	waitFor(t, "a snapshot with a failing dial", func() bool {
		_, _, b := sv.get(t, http.MethodGet, "/metrics")
		return strings.Contains(b, "simplex_last_run_timestamp_seconds ")
	})
	// An --out target that cannot be written, then an unknown overlay key naming a host.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  tower:\n    address: "+sentinelAddr+"\n    "+sentinelHost+": y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an overlay failure in the journal", func() bool {
		return strings.Contains(sv.stderr.String(), "run failed source=overlay")
	})
	_, _, body := sv.get(t, http.MethodGet, "/metrics")
	files := ""
	if b, err := os.ReadFile(out); err == nil {
		files = string(b)
	}
	for _, where := range map[string]string{"journal": sv.stderr.String(), "metrics": body, "document file": files} {
		for _, secret := range []string{sentinelAddr, sentinelHost} {
			if strings.Contains(where, secret) {
				t.Fatalf("%q reached an output:\n%s", secret, where)
			}
		}
	}
	sv.stop(t)
}

func TestServeOutFailureCountsAndNamesOnlyTheOperation(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	out := filepath.Join(t.TempDir(), "missing-dir", "probe.json")
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK), append([]string{"--config", cfg, "--overlay", ov, "--out", out}, fast...)...)
	waitFor(t, "a counted --out failure", func() bool {
		_, _, b := sv.get(t, http.MethodGet, "/metrics")
		return strings.Contains(b, "simplex_run_errors_total ") && !strings.Contains(b, "simplex_run_errors_total 0")
	})
	if !strings.Contains(sv.stderr.String(), "run failed source=out") || strings.Contains(sv.stderr.String(), "no such file") {
		t.Fatalf("journal: %q", sv.stderr.String())
	}
	sv.stop(t)
}

func TestServeStopsCleanlyAndReleasesThePort(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK), append([]string{"--config", cfg, "--overlay", ov}, fast...)...)
	if code := sv.stop(t); code != exitcode.Green {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, err := net.DialTimeout("tcp", sv.addr, 500*time.Millisecond); err == nil {
		t.Fatal("the port is still open after stop")
	}
	if !strings.Contains(sv.stderr.String(), "stopped") {
		t.Fatalf("no stop line: %q", sv.stderr.String())
	}
}

func TestServeNeverExitsOnSeverity(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	// A dial that times out makes the check YELLOW; serve keeps running.
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectTimeout), append([]string{"--config", cfg, "--overlay", ov}, fast...)...)
	waitFor(t, "a YELLOW snapshot", func() bool {
		_, _, b := sv.get(t, http.MethodGet, "/metrics")
		return strings.Contains(b, "simplex_overall_severity 1")
	})
	select {
	case code := <-sv.done:
		t.Fatalf("serve exited %d on a YELLOW verdict", code)
	case <-time.After(200 * time.Millisecond):
	}
	sv.stop(t)
}
