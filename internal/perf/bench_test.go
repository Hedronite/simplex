package perf

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/check"
	"github.com/Hedronite/simplex/internal/config"
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
			runner := probe.New(&probe.Options{
				Dialer: refusingDialer{},
				// One attempt, no retry: the benchmark should not pay 250 ms per
				// timeout, and a refusal is not retried anyway.
				Timeout:     time.Second,
				Concurrency: 8,
			})
			deps := &check.Deps{Runner: runner, Lister: noTailnet{}}
			params := &check.Params{Config: cfgPath, Overlay: ovPath, Now: time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)}

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

// refusingDialer answers every dial with a refusal: the fastest fake mesh and
// the one the severity rules treat as definitive.
type refusingDialer struct{}

// DialContext refuses without touching a socket.
func (refusingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, refusal
}

// refusal is an ECONNREFUSED-shaped error without importing syscall in a test
// helper that has to stay portable.
type refusedError struct{}

func (refusedError) Error() string   { return "connection refused" }
func (refusedError) Timeout() bool   { return false }
func (refusedError) Temporary() bool { return false }

var refusal = &net.OpError{Op: "dial", Net: "tcp", Err: refusedError{}}

// noTailnet is a lister that reports no tailnet address, which keeps the
// vantage deterministic without reading real interfaces.
type noTailnet struct{}

// HasTailnetAddress reports false.
func (noTailnet) HasTailnetAddress() bool { return false }

var _ vantage.Lister = noTailnet{}
