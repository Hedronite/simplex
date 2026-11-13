// Package loop runs a probe pass on an interval and publishes each result as
// an immutable snapshot. Runs never overlap, a run that overruns the interval
// skips the ticks it missed, and a failing run is counted and leaves the last
// good snapshot in place, so a reader sees an aging snapshot, not a gap.
package loop

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/Hedronite/simplex/internal/document"
)

// Snapshot is one completed run. It is never modified after it is published.
type Snapshot struct {
	// Doc is the derived document.
	Doc *document.Document
	// Body is Doc encoded once; /metrics and --out are built from the same run.
	Body []byte
	// Started and Ended bracket the run; Duration is Ended minus Started.
	Started, Ended time.Time
	Duration       time.Duration
}

// Store holds the latest good snapshot and the count of failed runs.
type Store struct {
	snap atomic.Pointer[Snapshot]
	errs atomic.Uint64
}

// Load returns the latest good snapshot, or nil before the first one.
func (s *Store) Load() *Snapshot { return s.snap.Load() }

// Errors returns how many runs produced no document.
func (s *Store) Errors() uint64 { return s.errs.Load() }

func (s *Store) set(sn *Snapshot) { s.snap.Store(sn) }
func (s *Store) fail()            { s.errs.Add(1) }

// RunFunc performs one pass for the given start time. The context carries the
// run deadline.
type RunFunc func(ctx context.Context, started time.Time) (*Snapshot, error)

// Event reports one finished run to the caller's logger. Err is nil on success.
// The loop adds no text of its own; what Err says is the RunFunc's business.
type Event struct {
	Started  time.Time
	Duration time.Duration
	Err      error
	Snapshot *Snapshot
}

// Config tunes a Loop.
type Config struct {
	// Interval is the time between run starts. It must be positive.
	Interval time.Duration
	// Deadline bounds one run. It must be positive and no longer than Interval.
	Deadline time.Duration
	// Run does the work.
	Run RunFunc
	// OnRun, when set, is called after every run, in the loop's goroutine.
	OnRun func(Event)
}

// Loop schedules runs.
type Loop struct {
	cfg   Config
	store *Store
}

// New validates the configuration and returns a Loop with an empty Store.
func New(cfg *Config) (*Loop, error) {
	switch {
	case cfg.Run == nil:
		return nil, errors.New("loop: Run is required")
	case cfg.Interval <= 0:
		return nil, errors.New("loop: Interval must be positive")
	case cfg.Deadline <= 0:
		return nil, errors.New("loop: Deadline must be positive")
	case cfg.Deadline > cfg.Interval:
		return nil, errors.New("loop: Deadline must not exceed Interval")
	}
	return &Loop{cfg: *cfg, store: &Store{}}, nil
}

// Store returns the store the loop publishes into.
func (l *Loop) Store() *Store { return l.store }

// Run starts a pass immediately, then one per interval, until ctx is done. It
// returns after the in-flight pass has finished; the pass sees the cancellation
// through its own context. Run returns ctx.Err().
func (l *Loop) Run(ctx context.Context) error {
	next := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.once(ctx)

		// The next start is the first scheduled slot after this run ended, so
		// a run that overran the interval skips the slots it missed.
		next = next.Add(l.cfg.Interval)
		now := time.Now()
		for !next.After(now) {
			next = next.Add(l.cfg.Interval)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Loop) once(ctx context.Context) {
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, l.cfg.Deadline)
	defer cancel()

	snap, err := l.cfg.Run(runCtx, started)
	ended := time.Now()
	if err == nil && snap == nil {
		err = errors.New("loop: run returned no snapshot")
	}
	ev := Event{Started: started, Duration: ended.Sub(started), Err: err}
	if err != nil {
		l.store.fail()
	} else {
		snap.Started, snap.Ended, snap.Duration = started, ended, ev.Duration
		l.store.set(snap)
		ev.Snapshot = snap
	}
	if l.cfg.OnRun != nil {
		l.cfg.OnRun(ev)
	}
}
