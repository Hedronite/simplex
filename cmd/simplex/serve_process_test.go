//go:build unix

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildBinary compiles the real command once per test.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "simplex")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runBinary(t *testing.T, bin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

// The real process: exit 3 for a bad start, exit 4 for a taken port, with
// nothing on stdout and exactly one error line.
func TestServeProcessStartupExitCodes(t *testing.T) {
	bin := buildBinary(t)
	cfg, ov := serveFiles(t, "127.0.0.1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	for name, tc := range map[string]struct {
		args []string
		code int
	}{
		"non-loopback without the flag": {[]string{"serve", "--config", cfg, "--overlay", ov, "--listen", "0.0.0.0:0"}, 3},
		"missing config":                {[]string{"serve", "--config", filepath.Join(t.TempDir(), "absent.yaml"), "--overlay", ov, "--listen", "127.0.0.1:0"}, 3},
		"taken port":                    {[]string{"serve", "--config", cfg, "--overlay", ov, "--listen", ln.Addr().String()}, 4},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runBinary(t, bin, tc.args...)
			if code != tc.code || stdout != "" {
				t.Fatalf("exit %d stdout %q stderr %q, want exit %d", code, stdout, stderr, tc.code)
			}
			oneErrorText(t, stderr)
		})
	}
}

// The real process end to end: a loopback listener stands in for a mesh host,
// serve probes it, serves /metrics, and exits 0 on SIGTERM.
func TestServeProcessServesAndStopsOnSIGTERM(t *testing.T) {
	bin := buildBinary(t)

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.yaml")
	ov := filepath.Join(dir, "o.txt")
	port := target.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("apiVersion: simplex.config/v0\nvantage:\n  id: lathe\nhosts:\n  - id: tower\n    checks:\n      - {kind: ts_reach, expected: light, port: %d}\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("apiVersion: simplex.overlay/v0\nhosts:\n  tower:\n    address: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pick a free loopback port for the server.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	cmd := exec.Command(bin, "serve", "--config", cfg, "--overlay", ov, "--listen", addr, "--interval", "100ms", "--deadline", "80ms")
	var errb syncBuffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var body string
	waitFor(t, "metrics with a finished run", func() bool {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return strings.Contains(body, "simplex_last_run_timestamp_seconds ")
	})
	if !strings.Contains(body, "simplex_check_severity{") || strings.Contains(body, "127.0.0.1") {
		t.Fatalf("unexpected metrics:\n%s", body)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serve did not exit 0 on SIGTERM: %v\n%s", err, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop on SIGTERM")
	}
	if !strings.Contains(errb.String(), "stopped") {
		t.Fatalf("no stop line: %q", errb.String())
	}
}
