package perf

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/check"
	"github.com/Hedronite/simplex/internal/probe"
	"github.com/Hedronite/simplex/internal/severity"
)

// allocationCeilings are twice the measured allocations per operation on the CI
// runner (docs/performance.md records the measurement). Allocation counts are
// deterministic for this code path, so this is a gate rather than a trend: a
// change that starts allocating per host, per reason or per pair shows up here
// as a number, not as a slow benchmark nobody reads.
var allocationCeilings = []struct {
	checks       int
	engineAllocs float64
	passAllocs   float64
}{
	{checks: 10, engineAllocs: 62, passAllocs: 3688},       // measured 31, 1844
	{checks: 100, engineAllocs: 450, passAllocs: 32006},    // measured 225, 16003
	{checks: 1000, engineAllocs: 4076, passAllocs: 316184}, // measured 2038, 158092
}

// refuseTransport answers every HTTP request with a refusal. The synthetic
// configs carry only TCP kinds today, so nothing would be fetched; the seam is
// set anyway so that adding an HTTP kind here can never reach the network.
type refuseTransport struct{}

// RoundTrip refuses without a socket.
func (refuseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, refusal
}

// TestAllocationsPerCheck gates the engine and a full pass on allocations.
func TestAllocationsPerCheck(t *testing.T) {
	for _, c := range allocationCeilings {
		t.Run(fmt.Sprintf("checks=%d", c.checks), func(t *testing.T) {
			in := input(c.checks)
			engine := testing.AllocsPerRun(1, func() {
				if _, err := severity.Evaluate(in); err != nil {
					t.Fatalf("evaluate: %v", err)
				}
			})
			if engine > c.engineAllocs {
				t.Fatalf("engine allocated %.0f per run at %d checks, budget %.0f",
					engine, c.checks, c.engineAllocs)
			}

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			ovPath := filepath.Join(dir, "private.yaml")
			if err := os.WriteFile(cfgPath, configYAML(c.checks), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if err := os.WriteFile(ovPath, overlayYAML(c.checks), 0o600); err != nil {
				t.Fatalf("write overlay: %v", err)
			}
			runner := probe.New(&probe.Options{
				Dialer:      countingDialer{dials: new(atomic.Int64)},
				Transport:   refuseTransport{},
				Timeout:     time.Second,
				Concurrency: 8,
			})
			deps := &check.Deps{Runner: runner, Lister: tailnetPresent{}}
			params := &check.Params{Config: cfgPath, Overlay: ovPath, Now: time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)}

			pass := testing.AllocsPerRun(1, func() {
				if _, err := check.Run(context.Background(), params, deps); err != nil {
					t.Fatalf("run: %v", err)
				}
			})
			if pass > c.passAllocs {
				t.Fatalf("a full pass allocated %.0f at %d checks, budget %.0f",
					pass, c.checks, c.passAllocs)
			}
			t.Logf("checks=%d engine=%.0f/%0.f pass=%.0f/%.0f", c.checks, engine, c.engineAllocs, pass, c.passAllocs)
		})
	}
}
