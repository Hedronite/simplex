package perf

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/check"
	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/probe"
)

// TestProfileFullPass profiles whole passes over a fake mesh of five hundred
// hosts: one pass repeated for a second under the CPU profiler, then a heap
// profile of the settled state. It is a measurement, not an assertion about
// speed; the assertion is that a pass at this size completes and produces a
// document, and the profiles are what the PR reads.
//
// Set SIMPLEX_PROFILE_DIR to keep the profiles somewhere readable; by default
// they live in the test's temporary directory and disappear with it.
func TestProfileFullPass(t *testing.T) {
	if testing.Short() {
		t.Skip("profiling run, skipped in -short")
	}
	const hosts = 500

	dir := t.TempDir()
	if keep := os.Getenv("SIMPLEX_PROFILE_DIR"); keep != "" {
		dir = keep
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("profile dir: %v", err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	ovPath := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(cfgPath, configYAML(hosts), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(ovPath, overlayYAML(hosts), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	var dials atomic.Int64
	runner := probe.New(&probe.Options{
		Dialer:      countingDialer{dials: &dials},
		Transport:   noTransport{},
		Timeout:     time.Second,
		Concurrency: 8,
	})
	deps := &check.Deps{Runner: runner, Lister: tailnetPresent{}}
	params := &check.Params{Config: cfgPath, Overlay: ovPath, Now: time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)}

	// One warm pass, so the profile measures steady state.
	warm, err := check.Run(context.Background(), params, deps)
	if err != nil {
		t.Fatalf("warm pass: %v", err)
	}
	// A blind vantage skips every probe, so the profile would be of the load
	// path alone. Both facts are asserted, not assumed.
	if got := dials.Load(); got != hosts {
		t.Fatalf("%d dials for %d hosts; the pass is not probing", got, hosts)
	}
	if warm.Doc.Vantage.Status == contract.VantageBlind {
		t.Fatal("the vantage is blind, so nothing was probed")
	}
	// A refused reach check is up; anything else means the fake errno was not
	// recognised as a refusal, and the profile would be of the wrong path.
	for _, c := range warm.Doc.Checks {
		if c.Result != contract.ResultUp {
			t.Fatalf("%s/%s is %s, want up from a refusal", c.Host, c.Kind, c.Result)
		}
	}

	cpuPath := filepath.Join(dir, "cpu.pprof")
	cpuFile, err := os.Create(cpuPath)
	if err != nil {
		t.Fatalf("create cpu profile: %v", err)
	}
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		t.Fatalf("start cpu profile: %v", err)
	}
	passes := 0
	start := time.Now()
	deadline := start.Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := check.Run(context.Background(), params, deps); err != nil {
			pprof.StopCPUProfile()
			t.Fatalf("pass %d: %v", passes, err)
		}
		passes++
	}
	pprof.StopCPUProfile()
	elapsed := time.Since(start)
	if err := cpuFile.Close(); err != nil {
		t.Fatalf("close cpu profile: %v", err)
	}

	heapPath := filepath.Join(dir, "heap.pprof")
	heapFile, err := os.Create(heapPath)
	if err != nil {
		t.Fatalf("create heap profile: %v", err)
	}
	runtime.GC()
	if err := pprof.WriteHeapProfile(heapFile); err != nil {
		t.Fatalf("write heap profile: %v", err)
	}
	if err := heapFile.Close(); err != nil {
		t.Fatalf("close heap profile: %v", err)
	}

	for _, p := range []string{cpuPath, heapPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", p)
		}
	}
	if passes < 2 {
		t.Fatalf("only %d passes in a second; the mesh is too large for a useful profile", passes)
	}
	t.Logf("hosts=%d passes=%d per_pass=%s profiles=%s", hosts, passes, elapsed/time.Duration(passes), dir)
}
