package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Hedronite/simplex/internal/contract"
)

// allowedResponseHeaders is every header name a response from serve may carry.
// Content-Type, Content-Length and Date come from the metrics handler and the
// server; Allow is set on a refused method; X-Content-Type-Options is added by
// net/http on its own error bodies. Anything outside this set, in particular a
// cookie or a CORS header, means a response now says more than it did when this
// was reviewed.
var allowedResponseHeaders = map[string]bool{
	"Content-Type":           true,
	"Content-Length":         true,
	"Date":                   true,
	"Allow":                  true,
	"X-Content-Type-Options": true,
}

// TestServeResponseHeadersAreTheNarrowSet pins what a response carries, so a
// later change cannot start setting cookies or opening the endpoint to other
// origins without a test failing. The requests carry an Origin and a CORS
// preflight header on purpose: a handler that reflected either would show it.
func TestServeResponseHeadersAreTheNarrowSet(t *testing.T) {
	cfg, ov := serveFiles(t, sentinelAddr)
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK),
		append([]string{"--config", cfg, "--overlay", ov}, fast...)...)
	waitFor(t, "the first snapshot", func() bool {
		_, _, b := sv.get(t, http.MethodGet, "/metrics")
		return strings.Contains(b, "simplex_last_run_timestamp_seconds ")
	})

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/metrics", http.StatusOK},
		{http.MethodOptions, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
	} {
		req, err := http.NewRequest(c.method, "http://"+sv.addr+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://origin.example")
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		req.Header.Set("Cookie", "session=value")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
		for name := range resp.Header {
			if !allowedResponseHeaders[name] {
				t.Errorf("%s %s: unexpected response header %q = %q", c.method, c.path, name, resp.Header.Values(name))
			}
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("%s %s set a cookie", c.method, c.path)
		}
	}

	// The one success response is exactly the exposition type.
	_, hdr, _ := sv.get(t, http.MethodGet, "/metrics")
	if got := hdr.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if code := sv.stop(t); code != 0 {
		t.Fatalf("exit %d on stop", code)
	}
}
