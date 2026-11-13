package probe

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// TestRetryPacing uses a fake clock, so the 250 ms gap costs no wall time and
// the assertion is exact rather than approximate.
func TestRetryPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dials atomic.Int32
		runner := New(&Options{
			RetryDelay: 250 * time.Millisecond,
			Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, timeoutError{}
			}),
		})
		start := time.Now()
		obs := runner.Run(context.Background(), []Instance{{
			Host: "tower", Kind: contract.KindTSReach, Target: ":9100", Address: "placeholder.invalid", Port: 9100,
		}})
		elapsed := time.Since(start)

		if got := dials.Load(); got != 2 {
			t.Fatalf("dialled %d times, want one attempt and one retry", got)
		}
		if elapsed != 250*time.Millisecond {
			t.Fatalf("the retry took %v, want exactly the retry delay", elapsed)
		}
		if obs[0].Result != contract.ResultUnknown || obs[0].Outcome != contract.ReasonTCPConnectTimeout {
			t.Fatalf("got (%s, %q), want (unknown, tcp_connect_timeout)", obs[0].Result, obs[0].Outcome)
		}
	})
}

// TestAttemptTimeoutIsVirtual proves the per-attempt budget is spent on the
// context, not guessed: a dial that never answers is cut off at the timeout,
// retried once, and the run costs two timeouts plus one retry gap.
func TestAttemptTimeoutIsVirtual(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dials atomic.Int32
		runner := New(&Options{
			Timeout:    time.Second,
			RetryDelay: 250 * time.Millisecond,
			Dialer: dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
				dials.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			}),
		})
		start := time.Now()
		obs := runner.Run(context.Background(), []Instance{{
			Host: "tower", Kind: contract.KindKubeAPIServer, Target: ":6443", Address: "placeholder.invalid", Port: 6443,
		}})
		elapsed := time.Since(start)

		if got := dials.Load(); got != 2 {
			t.Fatalf("dialled %d times, want two attempts", got)
		}
		if want := 2*time.Second + 250*time.Millisecond; elapsed != want {
			t.Fatalf("run took %v, want %v", elapsed, want)
		}
		if obs[0].Result != contract.ResultUnknown {
			t.Fatalf("got %s, want unknown once the budget is spent", obs[0].Result)
		}
	})
}

// TestDispatchStopsAtTheRunDeadline pins what happens when the deadline
// expires while the pool is full. The dispatch loop must stop handing out
// work, mark the instances it never started as unknown, and wait only for
// what is already in flight. Otherwise a permit released by an in-flight probe
// lets the loop start another one whose budget is already spent.
//
// The scenario runs many times because the failure it guards against is a
// select that has two ready cases at the same instant: one pass proves
// nothing, and this caught a real one-in-many flake.
func TestDispatchStopsAtTheRunDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const passes = 200
		for pass := 0; pass < passes; pass++ {
			var dials atomic.Int32
			runner := New(&Options{
				Concurrency: 1,
				Timeout:     5 * time.Second,
				Deadline:    100 * time.Millisecond,
				RetryDelay:  time.Millisecond,
				Dialer: dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
					dials.Add(1)
					<-ctx.Done()
					return nil, ctx.Err()
				}),
			})
			start := time.Now()
			obs := runner.Run(context.Background(), []Instance{
				{Host: "tower", Kind: contract.KindTSReach, Target: ":9100", Address: "placeholder.invalid", Port: 9100},
				{Host: "box", Kind: contract.KindTSReach, Target: ":9100", Address: "placeholder.invalid", Port: 9100},
				{Host: "apiary", Kind: contract.KindTSReach, Target: ":9100", Address: "placeholder.invalid", Port: 9100},
			})
			elapsed := time.Since(start)

			if elapsed != 100*time.Millisecond {
				t.Fatalf("pass %d: run took %v, want exactly the deadline", pass, elapsed)
			}
			if got := dials.Load(); got != 1 {
				t.Fatalf("pass %d: dialled %d instances, want only the one that was in flight when the deadline hit", pass, got)
			}
			for i, o := range obs {
				if o.Result != contract.ResultUnknown {
					t.Fatalf("pass %d: observation %d is %s, want unknown once the run is over", pass, i, o.Result)
				}
			}
		}
	})
}

// scriptConn serves scripted bytes, then blocks until its read deadline and
// reports it. It is the seam a real socket cannot be inside a synthetic clock:
// a real deadline would be a wall-clock instant the fake clock never reaches.
type scriptConn struct {
	mu       sync.Mutex
	data     []byte
	deadline time.Time
	writes   int
}

func newScriptConn(data string) *scriptConn { return &scriptConn{data: []byte(data)} }

func (c *scriptConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.data) > 0 {
		n := copy(p, c.data)
		c.data = c.data[n:]
		c.mu.Unlock()
		return n, nil
	}
	deadline := c.deadline
	c.mu.Unlock()
	if deadline.IsZero() {
		return 0, os.ErrDeadlineExceeded
	}
	// Virtual under synctest: the test costs no wall time.
	time.Sleep(time.Until(deadline))
	if time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return 0, os.ErrDeadlineExceeded
}

func (c *scriptConn) Write([]byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return 0, errors.New("probe must never write")
}

func (c *scriptConn) Close() error { return nil }

func (c *scriptConn) LocalAddr() net.Addr  { return scriptAddr{} }
func (c *scriptConn) RemoteAddr() net.Addr { return scriptAddr{} }

func (c *scriptConn) SetDeadline(t time.Time) error    { return c.SetReadDeadline(t) }
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }
func (c *scriptConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

type scriptAddr struct{}

func (scriptAddr) Network() string { return "script" }
func (scriptAddr) String() string  { return "script" }

// TestSSHBannerTimeoutIsRetried pins the retry rule for a banner read: a peer
// that says nothing on the first attempt and identifies itself on the second
// is up, and the run costs one attempt, the retry gap, and the second attempt.
func TestSSHBannerTimeoutIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dials atomic.Int32
		runner := New(&Options{
			Timeout:    100 * time.Millisecond,
			RetryDelay: 250 * time.Millisecond,
			Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					return newScriptConn(""), nil // silent
				}
				return newScriptConn("SSH-2.0-OpenSSH_9.6\r\n"), nil
			}),
		})
		start := time.Now()
		obs := runner.Run(context.Background(), []Instance{{
			Host: "tower", Kind: contract.KindSSH, Target: ":22", Address: "placeholder.invalid", Port: 22,
		}})
		elapsed := time.Since(start)

		if obs[0].Result != contract.ResultUp || obs[0].Outcome != contract.ReasonTCPConnectOK {
			t.Fatalf("got (%s, %q), want (up, tcp_connect_ok) after the retry", obs[0].Result, obs[0].Outcome)
		}
		if got := dials.Load(); got != 2 {
			t.Fatalf("dialled %d times, want one attempt and one retry", got)
		}
		if want := 100*time.Millisecond + 250*time.Millisecond; elapsed != want {
			t.Fatalf("run took %v, want %v", elapsed, want)
		}
	})
}

// TestSSHBannerTimeoutTwiceIsUnknown pins the far side of the same rule: two
// silent attempts leave the question open, cost exactly one retry, and carry
// no outcome code.
func TestSSHBannerTimeoutTwiceIsUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dials atomic.Int32
		runner := New(&Options{
			Timeout:    100 * time.Millisecond,
			RetryDelay: 250 * time.Millisecond,
			Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return newScriptConn(""), nil
			}),
		})
		start := time.Now()
		obs := runner.Run(context.Background(), []Instance{{
			Host: "tower", Kind: contract.KindSSH, Target: ":22", Address: "placeholder.invalid", Port: 22,
		}})
		elapsed := time.Since(start)

		if obs[0].Result != contract.ResultUnknown || obs[0].Outcome != "" {
			t.Fatalf("got (%s, %q), want unknown with no outcome code", obs[0].Result, obs[0].Outcome)
		}
		if got := dials.Load(); got != 2 {
			t.Fatalf("dialled %d times, want one attempt and one retry, never a second retry", got)
		}
		if want := 2*100*time.Millisecond + 250*time.Millisecond; elapsed != want {
			t.Fatalf("run took %v, want %v", elapsed, want)
		}
	})
}
