package perf

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Hedronite/simplex/internal/loop"
)

// soakSample is one point in the soak: the settled heap after a collection,
// and how many goroutines exist at that moment.
type soakSample struct {
	pass       int
	heap       uint64
	goroutines int
}

// TestSoakLoop is the leak gate over many passes. It runs the real loop in a
// synthetic bubble for several thousand passes, publishing the same shape of
// snapshot each time, and samples the settled heap and the goroutine count.
//
// The suspects it is aimed at are the three things that can grow without a
// visible failure: the snapshot store holding old documents alive, an HTTP
// transport accumulating idle connections, and retry timers that are never
// stopped. A leak shows up as a slope, so the check compares the settled heap
// across the run rather than at one point.
func TestSoakLoop(t *testing.T) {
	const (
		passes = 3000
		warmup = 300
	)
	synctest.Test(t, func(t *testing.T) {
		d := doc(100)
		body := d.Encode()

		baselineGoroutines := runtime.NumGoroutine()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ran := 0
		var samples []soakSample
		lp, err := loop.New(&loop.Config{
			Interval: time.Second,
			Deadline: 500 * time.Millisecond,
			Run: func(context.Context, time.Time) (*loop.Snapshot, error) {
				// A fresh document per pass, as a run would produce: reusing the
				// pointer would hide the very thing the store might leak.
				fresh := *d
				return &loop.Snapshot{Doc: &fresh, Body: body}, nil
			},
			OnRun: func(loop.Event) {
				ran++
				if ran%100 != 0 {
					return
				}
				runtime.GC()
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				samples = append(samples, soakSample{pass: ran, heap: ms.HeapAlloc, goroutines: runtime.NumGoroutine()})
				if ran >= passes {
					cancel()
				}
			},
		})
		if err != nil {
			t.Fatalf("loop: %v", err)
		}
		if err := lp.Run(ctx); err == nil {
			t.Fatal("loop returned no error after cancellation")
		}
		if ran < passes {
			t.Fatalf("the loop ran %d passes, want at least %d", ran, passes)
		}

		// Goroutines must come back to where they started: nothing the loop
		// starts may outlive it. This half is deterministic and is a gate.
		runtime.GC()
		if after := runtime.NumGoroutine(); after > baselineGoroutines+2 {
			t.Fatalf("%d goroutines after %d passes, started with %d", after, ran, baselineGoroutines)
		}

		// The heap half reports a slope. The gate on it is set from the CI
		// measurement, not guessed here.
		var after []soakSample
		for _, s := range samples {
			if s.pass > warmup {
				after = append(after, s)
			}
		}
		if len(after) < 3 {
			t.Fatalf("only %d samples after warmup; the soak is too short to see a slope", len(after))
		}
		first, last := after[0], after[len(after)-1]
		peak := after[0].heap
		for _, s := range after {
			if s.heap > peak {
				peak = s.heap
			}
		}
		t.Logf("passes=%d samples=%d heap first=%s last=%s peak=%s goroutines=%d",
			ran, len(after), bytes(first.heap), bytes(last.heap), bytes(peak), last.goroutines)

		// A leak is a slope, so the gate is on growth after warmup rather than
		// on an absolute number. The tolerance is deliberately loose here and
		// is set from the CI measurement in the follow-up commit: what it
		// catches today is unbounded growth, not a few kilobytes of noise.
		tolerance := first.heap/2 + 64<<10
		if growth := peak - first.heap; growth > tolerance {
			t.Fatalf("heap grew %s over %d passes after warmup (first %s, peak %s, tolerance %s)",
				bytes(growth), len(after), bytes(first.heap), bytes(peak), bytes(tolerance))
		}
	})
}

// bytes renders a byte count the way a reader wants it in a test log.
func bytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
