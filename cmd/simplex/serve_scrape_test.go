package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// TestServeUnderFiftyScrapers is the one real-time load run in S5: a serve
// process in this test binary, a short interval so snapshots keep turning
// over, and fifty scrapers hitting /metrics at once under the race detector.
//
// What it is looking for: a scrape that reads a half-published snapshot, a
// handler that serializes behind the run loop, and any data race between the
// loop publishing and readers loading. The body is also parsed for the two
// series that prove the snapshot is complete, so a truncated write would fail
// rather than pass as a short response.
func TestServeUnderFiftyScrapers(t *testing.T) {
	if testing.Short() {
		t.Skip("load run, skipped in -short")
	}
	cfg, ov := serveFiles(t, sentinelAddr)
	sv := startServe(t, serveDeps(sentinelAddr, contract.ReasonTCPConnectOK),
		"--config", cfg, "--overlay", ov, "--interval", "100ms", "--deadline", "80ms")
	// Before the first pass completes /metrics carries only the process series
	// by design, so the load starts once a real snapshot is being served.
	waitFor(t, "the first snapshot", func() bool {
		_, _, body := sv.get(t, http.MethodGet, "/metrics")
		return strings.Contains(body, "simplex_last_run_timestamp_seconds ")
	})

	const (
		scrapers   = 50
		perScraper = 20
	)
	var (
		ok, failed atomic.Int64
		wg         sync.WaitGroup
		firstErr   atomic.Value
	)
	start := time.Now()
	for i := 0; i < scrapers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 5 * time.Second}
			for j := 0; j < perScraper; j++ {
				resp, err := client.Get("http://" + sv.addr + "/metrics")
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					continue
				}
				body := make([]byte, 0, 64<<10)
				buf := make([]byte, 16<<10)
				for {
					n, rerr := resp.Body.Read(buf)
					body = append(body, buf[:n]...)
					if rerr != nil {
						break
					}
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, fmt.Sprintf("status %d", resp.StatusCode))
					continue
				}
				// A complete snapshot always carries these two: one from the
				// document, one from the run metadata.
				text := string(body)
				if !strings.Contains(text, "simplex_overall_severity ") ||
					!strings.Contains(text, "simplex_last_run_timestamp_seconds ") {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, "incomplete snapshot")
					continue
				}
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := int64(scrapers * perScraper)
	if got := ok.Load(); got != total {
		t.Fatalf("%d of %d scrapes succeeded during %v; first failure: %v", got, total, elapsed,
			firstErr.Load())
	}
	t.Logf("%d scrapes across %d scrapers in %v (%.0f/s), all complete", total, scrapers, elapsed,
		float64(total)/elapsed.Seconds())

	if code := sv.stop(t); code != 0 {
		t.Fatalf("serve exited %d after the load: %s", code, sv.stderr.String())
	}
}
