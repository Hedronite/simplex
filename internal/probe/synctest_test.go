package probe

import (
	"context"
	"net"
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
