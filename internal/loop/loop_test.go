package loop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Hedronite/simplex/internal/document"
)

func snap() *Snapshot { return &Snapshot{Doc: &document.Document{}, Body: []byte("{}\n")} }

func TestNewRejectsBadConfig(t *testing.T) {
	ok := func(context.Context, time.Time) (*Snapshot, error) { return snap(), nil }
	for name, cfg := range map[string]Config{
		"no run":            {Interval: time.Second, Deadline: time.Second},
		"zero interval":     {Deadline: time.Second, Run: ok},
		"zero deadline":     {Interval: time.Second, Run: ok},
		"deadline too long": {Interval: time.Second, Deadline: 2 * time.Second, Run: ok},
	} {
		if _, err := New(&cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := New(&Config{Interval: time.Second, Deadline: time.Second, Run: ok}); err != nil {
		t.Fatalf("deadline equal to interval is allowed: %v", err)
	}
}

// Runs start at once and then every interval, with the same start offsets.
func TestRunsAtOnceThenEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var starts []time.Duration
		origin := time.Now()
		l, err := New(&Config{
			Interval: 30 * time.Second, Deadline: 15 * time.Second,
			Run: func(_ context.Context, started time.Time) (*Snapshot, error) {
				mu.Lock()
				starts = append(starts, started.Sub(origin))
				mu.Unlock()
				return snap(), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()

		time.Sleep(100 * time.Second)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
		want := []time.Duration{0, 30 * time.Second, 60 * time.Second, 90 * time.Second}
		mu.Lock()
		defer mu.Unlock()
		if len(starts) != len(want) {
			t.Fatalf("starts %v, want %v", starts, want)
		}
		for i := range want {
			if starts[i] != want[i] {
				t.Fatalf("starts %v, want %v", starts, want)
			}
		}
	})
}

// A run that overruns the interval is not overlapped, and the slots it missed
// are skipped rather than replayed.
func TestOverrunSkipsMissedSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var starts []time.Duration
		var running int
		origin := time.Now()
		l, err := New(&Config{
			Interval: 30 * time.Second, Deadline: 30 * time.Second,
			Run: func(_ context.Context, started time.Time) (*Snapshot, error) {
				mu.Lock()
				running++
				if running > 1 {
					t.Error("two runs overlapped")
				}
				starts = append(starts, started.Sub(origin))
				mu.Unlock()
				if len(starts) == 1 {
					time.Sleep(70 * time.Second) // overruns two slots
				}
				mu.Lock()
				running--
				mu.Unlock()
				return snap(), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()
		time.Sleep(100 * time.Second)
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		want := []time.Duration{0, 90 * time.Second}
		if len(starts) != 2 || starts[0] != want[0] || starts[1] != want[1] {
			t.Fatalf("starts %v, want %v (slots at 30s and 60s skipped)", starts, want)
		}
	})
}

func TestFailedRunKeepsTheLastGoodSnapshotAndCounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		first := snap()
		l, err := New(&Config{
			Interval: 10 * time.Second, Deadline: 5 * time.Second,
			Run: func(context.Context, time.Time) (*Snapshot, error) {
				calls++
				if calls == 1 {
					return first, nil
				}
				return nil, errors.New("boom")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()
		time.Sleep(35 * time.Second) // runs at 0, 10, 20, 30
		cancel()
		<-done
		st := l.Store()
		if got := st.Load(); got != first {
			t.Fatal("a failing run replaced the last good snapshot")
		}
		if st.Errors() != 3 {
			t.Fatalf("errors %d, want 3", st.Errors())
		}
	})
}

func TestRunDeadlineIsPassedToTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got time.Duration
		var started time.Time
		l, err := New(&Config{
			Interval: 30 * time.Second, Deadline: 15 * time.Second,
			Run: func(ctx context.Context, s time.Time) (*Snapshot, error) {
				dl, ok := ctx.Deadline()
				if !ok {
					t.Error("run context has no deadline")
				}
				got, started = dl.Sub(s), s
				return snap(), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()
		time.Sleep(time.Second)
		cancel()
		<-done
		if got != 15*time.Second || started.IsZero() {
			t.Fatalf("deadline %v from %v, want 15s", got, started)
		}
	})
}

// Cancelling during a run waits for that run to finish, then stops; nothing
// new starts.
func TestCancelDrainsTheInFlightRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var finished bool
		release := make(chan struct{})
		l, err := New(&Config{
			Interval: 30 * time.Second, Deadline: 20 * time.Second,
			Run: func(context.Context, time.Time) (*Snapshot, error) {
				<-release
				finished = true
				return snap(), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Run returned before the in-flight run finished")
		default:
		}
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
		if !finished {
			t.Fatal("the in-flight run did not finish")
		}
		if l.Store().Load() == nil {
			t.Fatal("the drained run's snapshot was not published")
		}
	})
}

func TestOnRunReportsEveryRunAndNilSnapshotIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var events []Event
		calls := 0
		l, err := New(&Config{
			Interval: 10 * time.Second, Deadline: 5 * time.Second,
			Run: func(context.Context, time.Time) (*Snapshot, error) {
				calls++
				time.Sleep(2 * time.Second)
				if calls == 1 {
					return snap(), nil
				}
				return nil, nil
			},
			OnRun: func(ev Event) { events = append(events, ev) },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()
		time.Sleep(15 * time.Second)
		cancel()
		<-done
		if len(events) != 2 {
			t.Fatalf("%d events, want 2", len(events))
		}
		if events[0].Err != nil || events[0].Duration != 2*time.Second || events[0].Snapshot == nil {
			t.Fatalf("first event %+v", events[0])
		}
		if events[1].Err == nil || events[1].Snapshot != nil {
			t.Fatalf("a nil snapshot must be a failed run: %+v", events[1])
		}
		if l.Store().Errors() != 1 {
			t.Fatalf("errors %d, want 1", l.Store().Errors())
		}
	})
}
