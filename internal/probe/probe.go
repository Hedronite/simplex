// Package probe reaches one check instance and reports what it observed.
//
// It never decides severity: an Observation carries the raw result and, when
// the contract assigns one, the outcome reason. Verdicts, holds, the pair rule
// and the rollup belong to internal/severity.
//
// The dialer refuses contract.DeniedPort, and an address resolved from the
// private overlay is never written to an error string, a log line or an
// Observation, so a failure can be reported without leaking the mesh.
package probe

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// Dialer opens a TCP connection. TCPDialer is the real one; a caller that
// needs a deterministic run (the in-process fixture replay in cmd/simplex)
// supplies another. There is no flag, environment variable or build tag that
// swaps it in the shipped binary.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// TCPDialer is the default Dialer.
type TCPDialer struct{}

// DialContext dials with the probe's own ceiling on the connect attempt.
func (TCPDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: MaxTimeout}
	return d.DialContext(ctx, network, address)
}

// Transport performs one HTTP request. *http.Transport is one; a test may
// supply its own to answer without a network.
type Transport interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

// Clock is the source of time for retry pacing. SystemClock is the real one.
type Clock interface {
	// Now reports the current time.
	Now() time.Time
	// After returns a channel that receives after d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the default Clock.
type SystemClock struct{}

// Now reports the wall clock.
func (SystemClock) Now() time.Time { return time.Now() }

// After waits on the wall clock.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

const (
	// DefaultTimeout is one attempt's budget for a single check.
	DefaultTimeout = 2 * time.Second
	// MaxTimeout caps a caller-supplied per-attempt timeout.
	MaxTimeout = 10 * time.Second
	// DefaultDeadline bounds the whole run, however many checks it holds.
	DefaultDeadline = 15 * time.Second
	// DefaultRetryDelay separates the one retry a timeout earns.
	DefaultRetryDelay = 250 * time.Millisecond
	// DefaultConcurrency bounds how many probes are in flight.
	DefaultConcurrency = 8
	// MaxBody is the most of an HTTP response body that is ever read.
	MaxBody = 1 << 20
	// buildInfoWindow is how much of a /metrics body is searched for the
	// build-info series.
	buildInfoWindow = 64 << 10
)

// Instance is one check resolved for the dialer: where to reach it and how.
// Address comes from the private overlay and stays out of the document.
type Instance struct {
	Host    string
	Kind    contract.Kind
	Target  string // document form, for example ":9090/-/ready"
	Address string // resolved address or name
	Port    int
	Path    string // effective path for HTTP kinds
}

// Observation is what one instance produced. Outcome is empty when the
// contract assigns no outcome code to what was seen.
type Observation struct {
	Host    string
	Kind    contract.Kind
	Target  string
	Result  contract.Result
	Outcome contract.Reason
}

// Options tunes a run. The zero value is the contract's defaults.
type Options struct {
	// Timeout is one attempt's budget. Values above MaxTimeout are clamped.
	Timeout time.Duration
	// Deadline bounds the whole run.
	Deadline time.Duration
	// RetryDelay separates a retry from the attempt that timed out.
	RetryDelay time.Duration
	// Concurrency bounds probes in flight.
	Concurrency int
	// Dialer replaces the TCP dialer. Nil means TCPDialer.
	Dialer Dialer
	// Transport replaces the HTTP transport. Nil means the shared
	// keep-alive transport.
	Transport Transport
	// Clock paces retries. Nil means SystemClock.
	Clock Clock
}

func (o *Options) withDefaults() {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Timeout > MaxTimeout {
		o.Timeout = MaxTimeout
	}
	if o.Deadline <= 0 {
		o.Deadline = DefaultDeadline
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = DefaultRetryDelay
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Dialer == nil {
		o.Dialer = TCPDialer{}
	}
	if o.Clock == nil {
		o.Clock = SystemClock{}
	}
}

// Runner probes instances through one fixed set of seams. It is the only way
// to run a probe, so the real seams and a test's seams take the same path.
type Runner struct {
	opts *Options
}

// New returns a Runner, filling any unset seam with the real implementation
// and any unset budget with the contract default. A nil Options is the same as
// an empty one.
func New(opts *Options) *Runner {
	resolved := Options{}
	if opts != nil {
		resolved = *opts
	}
	resolved.withDefaults()
	return &Runner{opts: &resolved}
}

// Default returns a Runner that uses the real dialer, the shared HTTP
// transport and the system clock.
func Default() *Runner {
	return New(nil)
}

// Options reports the resolved budget and seams, so a caller can see what
// defaults were applied.
func (r *Runner) Options() Options { return *r.opts }

// Run probes every instance and returns one Observation per instance, in the
// order the instances arrived. Results are written into a pre-indexed slice,
// so no lock and no map ordering reaches the caller.
func (r *Runner) Run(ctx context.Context, instances []Instance) []Observation {
	opts := *r.opts

	runCtx, cancel := context.WithTimeout(ctx, opts.Deadline)
	defer cancel()

	out := make([]Observation, len(instances))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup

	for i, inst := range instances {
		if inst.Port == contract.DeniedPort {
			// Defence in depth: config load rejects this already, and the
			// dialer refuses it again. Nothing is dialled, so nothing is known.
			out[i] = Observation{Host: inst.Host, Kind: inst.Kind, Target: inst.Target, Result: contract.ResultUnknown}
			continue
		}
		// Take a permit only if the run is still live. A permit released by an
		// in-flight probe after the deadline would otherwise let the loop start
		// work whose budget is already spent.
		if !acquire(runCtx, sem) {
			out[i] = Observation{Host: inst.Host, Kind: inst.Kind, Target: inst.Target, Result: contract.ResultUnknown}
			continue
		}
		wg.Add(1)
		go func(i int, inst Instance) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = opts.probe(runCtx, &inst)
		}(i, inst)
	}
	wg.Wait()
	return out
}

// acquire takes a pool permit, or reports that the run is over. The check is
// repeated after the permit arrives: when the deadline fires, a free permit and
// a cancelled context can both be ready, and select may pick the permit, which
// would start a probe whose budget is already spent.
func acquire(ctx context.Context, sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		if ctx.Err() != nil {
			<-sem
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// probe runs one instance, retrying once after a timeout.
func (o *Options) probe(ctx context.Context, inst *Instance) Observation {
	obs, timedOut := o.attempt(ctx, inst)
	if !timedOut {
		return obs
	}
	select {
	case <-ctx.Done():
		return obs
	case <-o.Clock.After(o.RetryDelay):
	}
	obs, _ = o.attempt(ctx, inst)
	return obs
}

// attempt runs one probe and reports whether it timed out, which is the only
// condition that earns a retry. A refusal is definitive and never retried.
func (o *Options) attempt(ctx context.Context, inst *Instance) (Observation, bool) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	obs := Observation{Host: inst.Host, Kind: inst.Kind, Target: inst.Target}
	var timedOut bool
	switch inst.Kind {
	case contract.KindTSReach, contract.KindLANReach, contract.KindKubeAPIServer:
		obs.Result, obs.Outcome, timedOut = o.probeTCP(ctx, inst)
	case contract.KindNodeExporter, contract.KindPromUp:
		obs.Result, obs.Outcome, timedOut = o.probeHTTP(ctx, inst)
	case contract.KindSSH:
		obs.Result, obs.Outcome, timedOut = o.probeSSH(ctx, inst)
	default:
		// A kind this slice does not implement yet: no guess is better than a
		// wrong verdict.
		obs.Result = contract.ResultUnknown
	}
	return obs, timedOut
}
