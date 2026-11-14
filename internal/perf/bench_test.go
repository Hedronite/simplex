package perf

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/check"
	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/metrics"
	"github.com/Hedronite/simplex/internal/overlay"
	"github.com/Hedronite/simplex/internal/probe"
	"github.com/Hedronite/simplex/internal/severity"
	"github.com/Hedronite/simplex/internal/table"
	"github.com/Hedronite/simplex/internal/vantage"
)

var (
	cacheMu sync.Mutex
	cache   map[int]*document.Document
)

// benchDoc memoizes the derived document per size: encoding the same document
// twice would measure the encoder, not the engine.
func benchDoc(b *testing.B, checks int) *document.Document {
	b.Helper()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cache == nil {
		cache = map[int]*document.Document{}
	}
	d, ok := cache[checks]
	if !ok {
		d = doc(checks)
		cache[checks] = d
	}
	return d
}

// BenchmarkSeverityEvaluate is the pure engine: observations in, document out.
func BenchmarkSeverityEvaluate(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			in := input(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := severity.Evaluate(in); err != nil {
					b.Fatalf("evaluate: %v", err)
				}
			}
		})
	}
}

// BenchmarkDocumentEncode is the canonical byte layout, one encode per call.
func BenchmarkDocumentEncode(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			d := benchDoc(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(d.Encode()) == 0 {
					b.Fatal("empty encoding")
				}
			}
		})
	}
}

// BenchmarkMetricsWrite is what one scrape costs: 25 series per check.
func BenchmarkMetricsWrite(b *testing.B) {
	now := time.Date(2026, 11, 13, 18, 0, 12, 0, time.UTC)
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			snap := &metrics.Snapshot{
				Doc:         benchDoc(b, n),
				RunEnded:    now.Add(-10 * time.Second),
				RunDuration: 1500 * time.Millisecond,
				RunErrors:   3,
				Version:     "0.0.0-dev",
				Commit:      "unknown",
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := metrics.Write(io.Discard, snap, now); err != nil {
					b.Fatalf("write: %v", err)
				}
			}
		})
	}
}

// BenchmarkTableRender is the human table over the same document.
func BenchmarkTableRender(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			d := benchDoc(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(table.Render(d, false)) == 0 {
					b.Fatal("empty table")
				}
			}
		})
	}
}

// BenchmarkConfigParse loads the config at each size.
func BenchmarkConfigParse(b *testing.B) {
	now := func() time.Time { return time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC) }
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			data := configYAML(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cfg, findings, err := config.Parse(data, now)
				if err != nil || len(findings) != 0 || cfg == nil {
					b.Fatalf("parse: %v (%d findings)", err, len(findings))
				}
			}
		})
	}
}

// BenchmarkOverlayParse loads the overlay at each size.
func BenchmarkOverlayParse(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			data := overlayYAML(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ov, findings, err := overlay.Parse(data)
				if err != nil || len(findings) != 0 || ov == nil {
					b.Fatalf("parse: %v (%d findings)", err, len(findings))
				}
			}
		})
	}
}

// BenchmarkCheckRun is the whole pipeline behind one pass: load, resolve,
// probe against a fake mesh, derive, encode. The dialer refuses instantly, so
// this measures Simplex and not a network.
func BenchmarkCheckRun(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("checks=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			ovPath := filepath.Join(dir, "private.yaml")
			if err := os.WriteFile(cfgPath, configYAML(n), 0o600); err != nil {
				b.Fatalf("write config: %v", err)
			}
			if err := os.WriteFile(ovPath, overlayYAML(n), 0o600); err != nil {
				b.Fatalf("write overlay: %v", err)
			}
			var dials atomic.Int64
			runner := probe.New(&probe.Options{
				Dialer:    countingDialer{dials: &dials},
				Transport: noTransport{},
				// One attempt, no retry: the benchmark should not pay 250 ms per
				// timeout, and a refusal is not retried anyway.
				Timeout:     time.Second,
				Concurrency: 8,
			})
			deps := &check.Deps{Runner: runner, Lister: tailnetPresent{}}
			params := &check.Params{Config: cfgPath, Overlay: ovPath, Now: time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)}

			// One pass outside the timer proves the harness probes: exactly one
			// dial per check, a vantage that is not blind, and a check that
			// reports up from its refusal.
			dials.Store(0)
			warm, err := check.Run(context.Background(), params, deps)
			if err != nil {
				b.Fatalf("warm run: %v", err)
			}
			if got := dials.Load(); got != int64(n) {
				b.Fatalf("%d dials for %d checks; the run is not probing", got, n)
			}
			if warm.Doc.Vantage.Status == contract.VantageBlind {
				b.Fatal("the vantage is blind, so nothing was probed")
			}
			// The refusal must map to refused, not to indeterminate: a reach
			// check that is refused is up, so a result of unknown here would
			// mean the fake errno was not recognised.
			ups, others := 0, 0
			for _, c := range warm.Doc.Checks {
				if c.Result == contract.ResultUp {
					ups++
				} else {
					others++
				}
			}
			if ups != n || others != 0 {
				b.Fatalf("%d up and %d other results; the refusal is not mapping to refused", ups, others)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := check.Run(context.Background(), params, deps)
				if err != nil {
					b.Fatalf("run: %v", err)
				}
				if len(res.Body) == 0 {
					b.Fatal("empty body")
				}
			}
		})
	}
}

// refusal wraps the real errno: the probe classifies with errors.Is, so a
// look-alike error would be read as indeterminate instead of refused.
var refusal = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

// countingDialer counts dials so a run that skips probing cannot pass as a
// measurement of probing.
type countingDialer struct{ dials *atomic.Int64 }

// DialContext counts the dial and refuses it.
func (c countingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	c.dials.Add(1)
	return nil, refusal
}

// noTransport answers every HTTP request with a refusal, so a check of an HTTP
// kind can never reach the network from these benchmarks even if a generator
// later adds one. The probe's transport seam exists for exactly this.
type noTransport struct{}

// RoundTrip refuses without a socket.
func (noTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, refusal
}

// tailnetPresent is a lister that reports a tailnet address. It matters: with
// no address the vantage is blind and the pipeline skips every probe, so a
// benchmark or profile built on a false lister measures the load path and
// nothing else. It reads no real interface.
type tailnetPresent struct{}

// HasTailnetAddress reports true.
func (tailnetPresent) HasTailnetAddress() bool { return true }

var _ vantage.Lister = tailnetPresent{}
