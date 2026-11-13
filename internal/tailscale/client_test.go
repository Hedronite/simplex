package tailscale

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture is a status payload built from documentation values: TEST-NET
// addresses and names under .example. It is written as plain literals so a
// reader can see exactly what must not leak.
const statusFixture = `{
  "BackendState": "Running",
  "Self": {
    "Online": true,
    "DNSName": "vantage.example.",
    "TailscaleIPs": ["192.0.2.20"]
  },
  "Peer": {
    "node-direct": {
      "Online": true,
      "CurAddr": "192.0.2.10:41641",
      "Relay": "relay.example",
      "DNSName": "tower.example.",
      "TailscaleIPs": ["192.0.2.10"]
    },
    "node-relayed": {
      "Online": false,
      "Relay": "relay.example",
      "DNSName": "box.example.",
      "TailscaleIPs": ["198.51.100.7"]
    },
    "node-plain": {
      "Online": true,
      "CurAddr": "198.51.100.9:41641",
      "DNSName": "apiary.example.",
      "TailscaleIPs": ["198.51.100.9"]
    }
  }
}`

// recorded is one request the fake daemon saw. The body is reduced to a length
// so the test can assert "no body" without keeping payload bytes around.
type recorded struct {
	method string
	path   string
	body   int
}

// fakeDaemon is a LocalAPI stand-in on a unix socket. Every test uses it; none
// touches a real daemon.
type fakeDaemon struct {
	path string

	srv *http.Server
	ln  net.Listener

	mu       sync.Mutex
	requests []recorded
	accepts  int
}

// listener wraps Accept so the "refused before a socket is opened" test can
// count connections rather than requests.
type countingListener struct {
	net.Listener
	d *fakeDaemon
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.d.mu.Lock()
		l.d.accepts++
		l.d.mu.Unlock()
	}
	return conn, err
}

// newFakeDaemon starts a server on a unix socket in a short temp directory:
// macOS caps socket paths near 104 bytes, so the default TMPDIR is too long.
func newFakeDaemon(t *testing.T, handler http.HandlerFunc) *fakeDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sx")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "d.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	d := &fakeDaemon{path: socketPath, ln: countingListener{Listener: ln, d: nil}}
	d.ln = countingListener{Listener: ln, d: d}
	d.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io_ReadAll(r)
		d.mu.Lock()
		d.requests = append(d.requests, recorded{method: r.Method, path: r.URL.Path, body: len(body)})
		d.mu.Unlock()
		handler(w, r)
	})}
	go func() { _ = d.srv.Serve(d.ln) }()
	t.Cleanup(func() { _ = d.srv.Close() })
	return d
}

// io_ReadAll keeps the body read out of the import list of the test file's
// other helpers.
func io_ReadAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for {
		n, err := r.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

func (d *fakeDaemon) snapshot() ([]recorded, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recorded(nil), d.requests...), d.accepts
}

func (d *fakeDaemon) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(d.path)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// serveBody answers every request with one body and status.
func serveBody(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestStatusSuccessAndMatches covers the decode and both lookup handles.
func TestStatusSuccessAndMatches(t *testing.T) {
	d := newFakeDaemon(t, serveBody(http.StatusOK, statusFixture))
	status, err := d.client(t).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.BackendState != "Running" {
		t.Fatalf("backend state %q, want Running", status.BackendState)
	}
	if status.Peers() != 8 {
		// Three peers plus the vantage's own node, each keyed by its name and
		// by one address.
		t.Fatalf("peers %d, want 8 keys", status.Peers())
	}

	cases := []struct {
		name    string
		address string
		want    Enrichment
		matched bool
	}{
		{
			name:    "direct peer by address",
			address: "192.0.2.10",
			want:    Enrichment{Online: true, Direct: true, Relayed: false},
			matched: true,
		},
		{
			name:    "relayed and offline peer by address",
			address: "198.51.100.7",
			want:    Enrichment{Online: false, Direct: false, Relayed: true},
			matched: true,
		},
		{
			name:    "direct peer stays direct when a relay is also set",
			address: "198.51.100.9",
			want:    Enrichment{Online: true, Direct: true, Relayed: false},
			matched: true,
		},
		{
			name:    "MagicDNS name, upper case and trailing dot",
			address: "TOWER.EXAMPLE.",
			want:    Enrichment{Online: true, Direct: true, Relayed: false},
			matched: true,
		},
		{
			name:    "MagicDNS name without the trailing dot",
			address: "box.example",
			want:    Enrichment{Online: false, Direct: false, Relayed: true},
			matched: true,
		},
		{
			name:    "the vantage's own node by address",
			address: "192.0.2.20",
			want:    Enrichment{Self: true, Online: true, Direct: false, Relayed: false},
			matched: true,
		},
		{
			name:    "the vantage's own node by name",
			address: "VANTAGE.EXAMPLE",
			want:    Enrichment{Self: true, Online: true, Direct: false, Relayed: false},
			matched: true,
		},
		{
			name:    "unmatched address",
			address: "203.0.113.9",
			matched: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := status.Match(tc.address)
			if ok != tc.matched {
				t.Fatalf("Match(%q) matched %v, want %v", tc.address, ok, tc.matched)
			}
			if ok && got != tc.want {
				t.Fatalf("Match(%q) = %+v, want %+v", tc.address, got, tc.want)
			}
		})
	}
}

// TestExactlyOneRequestPerStatus pins the access model: one GET, one path, no
// body.
func TestExactlyOneRequestPerStatus(t *testing.T) {
	d := newFakeDaemon(t, serveBody(http.StatusOK, statusFixture))
	if _, err := d.client(t).Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	requests, accepts := d.snapshot()
	if len(requests) != 1 {
		t.Fatalf("%d requests, want exactly one", len(requests))
	}
	if accepts != 1 {
		t.Fatalf("%d connections, want exactly one", accepts)
	}
	if requests[0].method != http.MethodGet || requests[0].path != statusPath {
		t.Fatalf("request was %s %s, want GET %s", requests[0].method, requests[0].path, statusPath)
	}
	if requests[0].body != 0 {
		t.Fatalf("request carried %d body bytes, want none", requests[0].body)
	}
}

// TestOtherCallsAreRefusedBeforeDial pins that only the one call exists: both a
// different method and a different path fail without opening a socket.
func TestOtherCallsAreRefusedBeforeDial(t *testing.T) {
	d := newFakeDaemon(t, serveBody(http.StatusOK, statusFixture))
	c := d.client(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, statusPath},
		{http.MethodGet, "/localapi/v0/prefs"},
		{http.MethodDelete, "/localapi/v0/logout"},
		{http.MethodPut, "/localapi/v0/status"},
	} {
		if _, err := c.get(context.Background(), tc.method, tc.path); err == nil {
			t.Fatalf("%s %s was allowed", tc.method, tc.path)
		}
	}
	requests, accepts := d.snapshot()
	if len(requests) != 0 || accepts != 0 {
		t.Fatalf("%d requests and %d connections for refused calls, want none", len(requests), accepts)
	}
}

// TestFailuresMapToSentinels covers every failure path the access model names.
func TestFailuresMapToSentinels(t *testing.T) {
	t.Run("missing socket", func(t *testing.T) {
		dir, err := os.MkdirTemp("/tmp", "sx")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		c, err := New(filepath.Join(dir, "nope.sock"))
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		_, err = c.Status(context.Background())
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v, want ErrUnavailable", err)
		}
	})

	t.Run("socket without permission", func(t *testing.T) {
		d := newFakeDaemon(t, serveBody(http.StatusOK, statusFixture))
		if err := os.Chmod(d.path, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		_, err := d.client(t).Status(context.Background())
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("got %v, want ErrDenied", err)
		}
	})

	t.Run("403", func(t *testing.T) {
		d := newFakeDaemon(t, serveBody(http.StatusForbidden, statusFixture))
		_, err := d.client(t).Status(context.Background())
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("got %v, want ErrDenied", err)
		}
	})

	t.Run("500", func(t *testing.T) {
		d := newFakeDaemon(t, serveBody(http.StatusInternalServerError, statusFixture))
		_, err := d.client(t).Status(context.Background())
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v, want ErrUnavailable", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		d := newFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := d.client(t).Status(ctx)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v, want ErrUnavailable", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("took %v, the caller deadline is 100ms", elapsed)
		}
	})
}

// TestPayloadLimits covers a body that is not JSON and one that is too big.
func TestPayloadLimits(t *testing.T) {
	t.Run("malformed json", func(t *testing.T) {
		d := newFakeDaemon(t, serveBody(http.StatusOK, `{"BackendState": "Running", "Peer": `))
		_, err := d.client(t).Status(context.Background())
		if !errors.Is(err, ErrBadPayload) {
			t.Fatalf("got %v, want ErrBadPayload", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		big := strings.Repeat("x", MaxBody+1)
		d := newFakeDaemon(t, serveBody(http.StatusOK, big))
		_, err := d.client(t).Status(context.Background())
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("got %v, want ErrTooLarge", err)
		}
	})
}

// TestNoPayloadTextInErrors is the leak test: a malformed payload full of
// documentation values must not leave a trace in the error.
func TestNoPayloadTextInErrors(t *testing.T) {
	const leaky = `{"BackendState": "192.0.2.10", "Peer": {"node": {"DNSName": "tower.example.", "TailscaleIPs": ["198.51.100.7"], "CurAddr": "192.0.2.10:41641", "Relay": "relay.example"}, "broken": `
	d := newFakeDaemon(t, serveBody(http.StatusOK, leaky))
	_, err := d.client(t).Status(context.Background())
	if err == nil {
		t.Fatal("malformed payload was accepted")
	}
	message := err.Error()
	for _, leak := range []string{
		"192.0.2.10",
		"198.51.100.7",
		"tower.example",
		"relay.example",
		"CurAddr",
		"Relay",
		"BackendState",
	} {
		if strings.Contains(message, leak) {
			t.Fatalf("error text %q contains %q", message, leak)
		}
	}
}

// TestSocketPathValidation pins the config rule: an absolute path only, never a
// URL and never a host and port.
func TestSocketPathValidation(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"/var/run/tailscale/tailscaled.sock", true},
		{"/run/tailscale/tailscaled.sock", true},
		{"", false},
		{"var/run/tailscale/tailscaled.sock", false},
		{"http://192.0.2.10:41112/localapi", false},
		{"unix:///var/run/tailscale/tailscaled.sock", false},
		{"//var/run/tailscale/tailscaled.sock", false},
		{"/var/run/tailscale//tailscaled.sock", true},
		{"/var/run/ts sock/tailscaled.sock", false},
		{"example:41112", false},
		{"[::1]:41112", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			err := ValidateSocketPath(tc.path)
			if tc.ok && err != nil {
				t.Fatalf("%q rejected: %v", tc.path, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("%q accepted", tc.path)
			}
		})
	}
}

// TestNoExecAndNoTailscaleModule is the access-model scan: this package may not
// shell out and may not depend on a Tailscale library.
func TestNoExecAndNoTailscaleModule(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		for _, banned := range []string{
			`"os/exec"`,
			`"tailscale.com/`,
			`"golang.org/x/sys/execabs"`,
		} {
			if strings.Contains(src, banned) {
				t.Fatalf("%s imports %s", name, banned)
			}
		}
	}
}
