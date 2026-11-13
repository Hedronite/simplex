package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// dialFunc is the test seam: the runner reaches the network through
// interfaces, so a test can answer without one and stay portable to a machine
// with no extra loopback aliases.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

// roundTripFunc answers an HTTP request without a socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// resolveStarted splits an httptest listener address into what the probe needs,
// so the HTTP tests exercise the real client against a real server.
func resolveStarted(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	return host, port
}

// TestHTTPStatusMap walks semantics section 3 through a real server.
func TestHTTPStatusMap(t *testing.T) {
	const buildInfo = "prometheus_build_info{branch=\"HEAD\"} 1\n"
	cases := []struct {
		name    string
		kind    contract.Kind
		path    string
		status  int
		body    string
		result  contract.Result
		outcome contract.Reason
	}{
		{"ready 200", contract.KindPromUp, contract.PathReady, 200, "Prometheus is Ready.\n", contract.ResultUp, ""},
		{"metrics with build info", contract.KindPromUp, contract.PathMetrics, 200, buildInfo, contract.ResultUp, ""},
		{"metrics without build info", contract.KindPromUp, contract.PathMetrics, 200, "# HELP something else\n", contract.ResultUp, contract.ReasonMetricsBodyBad},
		{"node exporter has no build info assertion", contract.KindNodeExporter, contract.PathMetrics, 200, "node_exporter_build_info 1\n", contract.ResultUp, ""},
		{"ready 503 is up but not ready", contract.KindPromUp, contract.PathReady, 503, "not ready\n", contract.ResultUp, contract.ReasonPromNotReady},
		{"healthy 503 is an unexpected status", contract.KindPromUp, contract.PathHealthy, 503, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
		{"prom 401", contract.KindPromUp, contract.PathReady, 401, "no\n", contract.ResultUp, contract.ReasonPromAuth},
		{"prom 403", contract.KindPromUp, contract.PathReady, 403, "no\n", contract.ResultUp, contract.ReasonPromAuth},
		// prom_auth belongs to prom_up alone: on any other kind these statuses
		// are simply not the answer the check expects.
		{"node exporter 401", contract.KindNodeExporter, contract.PathMetrics, 401, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
		{"node exporter 403", contract.KindNodeExporter, contract.PathMetrics, 403, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
		{"node exporter 503", contract.KindNodeExporter, contract.PathMetrics, 503, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
		{"node exporter 404", contract.KindNodeExporter, contract.PathMetrics, 404, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
		{"404", contract.KindPromUp, contract.PathReady, 404, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
		{"500", contract.KindPromUp, contract.PathReady, 500, "no\n", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			host, port := resolveStarted(t, srv)

			obs := New(&Options{Timeout: time.Second}).Run(context.Background(), []Instance{{
				Host: "box", Kind: tc.kind, Target: ":9090" + tc.path, Address: host, Port: port, Path: tc.path,
			}})

			if len(obs) != 1 {
				t.Fatalf("got %d observations", len(obs))
			}
			if obs[0].Result != tc.result || obs[0].Outcome != tc.outcome {
				t.Fatalf("got (%s, %q), want (%s, %q)", obs[0].Result, obs[0].Outcome, tc.result, tc.outcome)
			}
		})
	}
}

// TestHTTPRedirectIsNotFollowed proves the probe answers for the path it named.
func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	var followed atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/-/ready", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host, port := resolveStarted(t, srv)

	obs := New(&Options{}).Run(context.Background(), []Instance{{
		Host: "box", Kind: contract.KindPromUp, Target: ":9090/-/ready", Address: host, Port: port, Path: contract.PathReady,
	}})
	if followed.Load() != 0 {
		t.Fatalf("the redirect was followed %d times", followed.Load())
	}
	if obs[0].Result != contract.ResultDown || obs[0].Outcome != contract.ReasonHTTPUnexpectedStatus {
		t.Fatalf("got (%s, %q), want the redirect status treated as unexpected", obs[0].Result, obs[0].Outcome)
	}
}

// TestHTTPBodyCap proves the read stops at MaxBody: the server keeps writing
// and then blocks, so a probe that read everything would run to its timeout.
func TestHTTPBodyCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 32<<10)
		for written := 0; written <= MaxBody+len(chunk); written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		// Hold the response open until the client gives up. A probe that read
		// past the cap would wait here and hit its timeout instead of
		// answering, so an up result is the proof.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	host, port := resolveStarted(t, srv)

	start := time.Now()
	obs := New(&Options{Timeout: 2 * time.Second}).Run(context.Background(), []Instance{{
		Host: "box", Kind: contract.KindPromUp, Target: ":9090/metrics", Address: host, Port: port, Path: contract.PathMetrics,
	}})
	if obs[0].Result != contract.ResultUp {
		t.Fatalf("got %s, want up: the cap should end the read", obs[0].Result)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("read took %v, so it ran past the body cap", elapsed)
	}
}

// TestHTTPResponseTimeoutIsRetried uses a slow handler and a short budget.
func TestHTTPResponseTimeoutIsRetried(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := resolveStarted(t, srv)

	obs := New(&Options{Timeout: 50 * time.Millisecond, RetryDelay: 10 * time.Millisecond}).
		Run(context.Background(), []Instance{{
			Host: "box", Kind: contract.KindPromUp, Target: ":9090/-/ready", Address: host, Port: port, Path: contract.PathReady,
		}})

	if obs[0].Result != contract.ResultUnknown || obs[0].Outcome != contract.ReasonHTTPTimeout {
		t.Fatalf("got (%s, %q), want (unknown, http_timeout)", obs[0].Result, obs[0].Outcome)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("the server saw %d requests, want one attempt and one retry", got)
	}
}

// TestHTTPTransportSeam answers without a socket, which is what the in-process
// fixture replay needs, and works on a machine with no extra loopback aliases.
func TestHTTPTransportSeam(t *testing.T) {
	var paths []string
	seam := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     http.Header{},
			Request:    r,
		}, nil
	})
	obs := New(&Options{Transport: seam}).Run(context.Background(), []Instance{{
		Host: "box", Kind: contract.KindPromUp, Target: ":9090/-/ready", Address: "placeholder.invalid", Port: 9090, Path: contract.PathReady,
	}})
	if obs[0].Result != contract.ResultUp {
		t.Fatalf("got %s, want up through the transport seam", obs[0].Result)
	}
	if len(paths) != 1 || paths[0] != contract.PathReady {
		t.Fatalf("the seam saw %v, want one request for the readiness path", paths)
	}
}

// TestTCPAcceptAndRefuse uses real loopback listeners: an open port and a
// closed one. Both are on 127.0.0.1, which every CI machine has.
func TestTCPAcceptAndRefuse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()
	defer listener.Close()
	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	openPort, _ := strconv.Atoi(portStr)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	closedHost, closedPortStr, _ := net.SplitHostPort(closed.Addr().String())
	closedPort, _ := strconv.Atoi(closedPortStr)
	_ = closed.Close()

	obs := New(&Options{RetryDelay: time.Millisecond}).Run(context.Background(), []Instance{
		{Host: "tower", Kind: contract.KindTSReach, Target: ":9100", Address: host, Port: openPort},
		{Host: "tower", Kind: contract.KindTSReach, Target: ":9100", Address: host, Port: closedPort},
		{Host: "box", Kind: contract.KindKubeAPIServer, Target: ":6443", Address: closedHost, Port: closedPort},
	})

	want := []struct {
		result  contract.Result
		outcome contract.Reason
	}{
		{contract.ResultUp, contract.ReasonTCPConnectOK},
		{contract.ResultUp, contract.ReasonTCPConnectRefused},
		{contract.ResultDown, contract.ReasonTCPConnectRefused},
	}
	for i, w := range want {
		if obs[i].Result != w.result || obs[i].Outcome != w.outcome {
			t.Fatalf("observation %d: got (%s, %q), want (%s, %q)", i, obs[i].Result, obs[i].Outcome, w.result, w.outcome)
		}
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("the open port was never dialled")
	}
}

// timeoutError is a dial failure that reports itself as a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestDialOutcomes covers the indeterminate cases without a network, and pins
// that only a timeout is retried.
func TestDialOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		result  contract.Result
		outcome contract.Reason
		dials   int32
	}{
		{"timeout", timeoutError{}, contract.ResultUnknown, contract.ReasonTCPConnectTimeout, 2},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, contract.ResultDown, contract.ReasonTCPConnectRefused, 1},
		{"unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, contract.ResultUnknown, "", 1},
		{"dns", &net.OpError{Op: "dial", Err: &net.DNSError{IsNotFound: true}}, contract.ResultUnknown, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			runner := New(&Options{
				Timeout:    50 * time.Millisecond,
				RetryDelay: time.Millisecond,
				Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
					calls.Add(1)
					return nil, tc.err
				}),
			})
			obs := runner.Run(context.Background(), []Instance{{
				Host: "tower", Kind: contract.KindKubeAPIServer, Target: ":6443", Address: "placeholder.invalid", Port: 6443,
			}})
			if obs[0].Result != tc.result || obs[0].Outcome != tc.outcome {
				t.Fatalf("got (%s, %q), want (%s, %q)", obs[0].Result, obs[0].Outcome, tc.result, tc.outcome)
			}
			if got := calls.Load(); got != tc.dials {
				t.Fatalf("dialled %d times, want %d", got, tc.dials)
			}
		})
	}
}

// TestDeniedPortIsRefusedOnTheHTTPPath drives probeHTTP directly, past Run, to
// prove the refusal is in the probe and not only in the runner's loop: a
// caller that reaches the HTTP path with the denied port must not produce a
// request at all, whichever transport it supplied.
func TestDeniedPortIsRefusedOnTheHTTPPath(t *testing.T) {
	var requests atomic.Int32
	seam := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
	})
	opts := Options{Transport: seam}
	opts.withDefaults()

	result, outcome, retried := opts.probeHTTP(context.Background(), &Instance{
		Host: "box", Kind: contract.KindPromUp, Target: ":" + strconv.Itoa(contract.DeniedPort) + contract.PathReady,
		Address: "placeholder.invalid", Port: contract.DeniedPort, Path: contract.PathReady,
	})
	if requests.Load() != 0 {
		t.Fatalf("the denied port produced %d requests", requests.Load())
	}
	if result != contract.ResultUnknown || outcome != "" || retried {
		t.Fatalf("got (%s, %q, retried=%v), want unknown, no outcome, no retry", result, outcome, retried)
	}

	// The shared transport refuses it too, in case anything else drives it.
	if _, err := guardedDialContext(context.Background(), "tcp", net.JoinHostPort("placeholder.invalid", strconv.Itoa(contract.DeniedPort))); !errors.Is(err, errDeniedPort) {
		t.Fatalf("guardedDialContext returned %v, want errDeniedPort", err)
	}
}

// TestDeniedPortIsNeverDialled pins the second guard on the port the mesh never
// dials: config load rejects it, and the dialer refuses it too.
func TestDeniedPortIsNeverDialled(t *testing.T) {
	var dials atomic.Int32
	runner := New(&Options{Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	})})
	obs := runner.Run(context.Background(), []Instance{{
		Host: "box", Kind: contract.KindTSReach, Target: ":" + strconv.Itoa(contract.DeniedPort),
		Address: "placeholder.invalid", Port: contract.DeniedPort,
	}})
	if dials.Load() != 0 {
		t.Fatalf("the denied port was dialled %d times", dials.Load())
	}
	if obs[0].Result != contract.ResultUnknown || obs[0].Outcome != "" {
		t.Fatalf("got (%s, %q), want unknown with no outcome", obs[0].Result, obs[0].Outcome)
	}

	// The dialer refuses it even when a caller reaches past Run.
	real := Options{Dialer: TCPDialer{}}
	real.withDefaults()
	if _, err := real.dial(context.Background(), "placeholder.invalid", contract.DeniedPort); !errors.Is(err, errDeniedPort) {
		t.Fatalf("dial returned %v, want errDeniedPort", err)
	}
}

// TestRunKeepsInputOrderAndBoundsConcurrency checks the two properties the
// caller relies on: results line up with instances, and no more than the pool
// size is in flight.
func TestRunKeepsInputOrderAndBoundsConcurrency(t *testing.T) {
	const instances = 40
	var inFlight, peak atomic.Int32
	runner := New(&Options{
		Concurrency: 4,
		Timeout:     time.Second,
		Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
			now := inFlight.Add(1)
			for {
				old := peak.Load()
				if now <= old || peak.CompareAndSwap(old, now) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}),
	})
	batch := make([]Instance, instances)
	for i := range batch {
		batch[i] = Instance{
			Host:    fmt.Sprintf("host-%02d", i),
			Kind:    contract.KindKubeAPIServer,
			Target:  ":6443",
			Address: "placeholder.invalid",
			Port:    6443,
		}
	}
	obs := runner.Run(context.Background(), batch)

	for i := range batch {
		if obs[i].Host != batch[i].Host {
			t.Fatalf("observation %d is for %s, want %s", i, obs[i].Host, batch[i].Host)
		}
	}
	if got := peak.Load(); got > 4 {
		t.Fatalf("%d probes were in flight, the pool is 4", got)
	}
}

// TestRunDeadlineIsBounded proves a run returns inside its deadline even when
// every probe would block for longer.
func TestRunDeadlineIsBounded(t *testing.T) {
	blocked := dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := time.Now()
	obs := New(&Options{
		Timeout:    time.Second,
		Deadline:   60 * time.Millisecond,
		RetryDelay: time.Millisecond,
		Dialer:     blocked,
	}).Run(context.Background(), []Instance{
		{Host: "tower", Kind: contract.KindTSReach, Target: ":9100", Address: "placeholder.invalid", Port: 9100},
		{Host: "box", Kind: contract.KindTSReach, Target: ":9100", Address: "placeholder.invalid", Port: 9100},
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("run took %v, the deadline is 60ms", elapsed)
	}
	for i, o := range obs {
		if o.Result != contract.ResultUnknown {
			t.Fatalf("observation %d: got %s, want unknown under a blown deadline", i, o.Result)
		}
	}
}

// TestTimeoutIsClamped keeps a caller from budgeting a run away.
func TestTimeoutIsClamped(t *testing.T) {
	if got := New(&Options{Timeout: time.Hour}).Options().Timeout; got != MaxTimeout {
		t.Fatalf("timeout = %v, want the cap %v", got, MaxTimeout)
	}
	got := New(&Options{}).Options()
	if got.Timeout != DefaultTimeout || got.Deadline != DefaultDeadline ||
		got.Concurrency != DefaultConcurrency || got.RetryDelay != DefaultRetryDelay {
		t.Fatalf("zero options did not take the defaults: %+v", got)
	}
	if _, ok := got.Dialer.(TCPDialer); !ok {
		t.Fatalf("default dialer is %T, want TCPDialer", got.Dialer)
	}
	if _, ok := got.Clock.(SystemClock); !ok {
		t.Fatalf("default clock is %T, want SystemClock", got.Clock)
	}
	if Default() == nil {
		t.Fatal("Default returned nil")
	}
}
