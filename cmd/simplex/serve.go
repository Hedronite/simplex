package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hedronite/simplex/internal/check"
	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/failure"
	"github.com/Hedronite/simplex/internal/loop"
	"github.com/Hedronite/simplex/internal/metrics"
	"github.com/Hedronite/simplex/internal/outfile"
	"github.com/Hedronite/simplex/internal/probe"
)

// unsafeListenFlag is the one flag that allows a non-loopback --listen. Its
// name is frozen in docs/semantics.md sections 12 and 13.
const unsafeListenFlag = "unsafe-allow-non-loopback"

const shutdownGrace = 5 * time.Second

type serveFlags struct {
	config   string
	overlay  string
	listen   string
	interval time.Duration
	deadline time.Duration
	out      string
	unsafe   bool
}

func newServe(deps *Deps) *cobra.Command {
	var f serveFlags
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Probe on an interval and serve /metrics",
		Long: `Run a probe pass every --interval and serve the latest result as
Prometheus metrics on GET /metrics.

Each pass produces one snapshot; /metrics and --out are built from the same
one. With --out, serve is the writer of record for the document file: the
snapshot is written every interval by temp file, fsync and atomic rename.

serve binds loopback only. A non-loopback --listen is refused unless
--` + unsafeListenFlag + ` is set, and then a warning is printed on every start:
/metrics has no authentication.

A failing pass after startup is counted (simplex_run_errors_total) and logged
by field path only, and the last good snapshot stays; serve never exits on
severity. SIGINT or SIGTERM finishes the pass in flight and exits 0.

Exit codes: 0 normal stop, 3 config or usage error at startup, 4 the address
cannot be bound or the server fails.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), &f, deps, cmd.ErrOrStderr())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", defaultConfig, "config file")
	fl.StringVar(&f.overlay, "overlay", "", "overlay file (default: $"+overlayEnv+", then "+defaultOverlay+" when present)")
	fl.StringVar(&f.listen, "listen", contract.MetricsListen, "address to serve /metrics on (loopback unless --"+unsafeListenFlag+")")
	fl.DurationVar(&f.interval, "interval", 30*time.Second, "time between probe passes")
	fl.DurationVar(&f.deadline, "deadline", probe.DefaultDeadline, "time one pass may take (at most --interval)")
	fl.StringVar(&f.out, "out", "", "write the document to this file every pass (temp file, fsync, atomic rename)")
	fl.BoolVar(&f.unsafe, unsafeListenFlag, false, "allow a non-loopback --listen; /metrics is unauthenticated")
	return cmd
}

// listenAllowed reports whether addr is a loopback address. Names are not
// resolved: only "localhost" and loopback IP literals qualify, so the answer
// never depends on a lookup.
func listenAllowed(addr string) (bool, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("--listen %q: want host:port", addr)
	}
	if port == "" {
		return false, fmt.Errorf("--listen %q: missing port", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

func runServe(ctx context.Context, f *serveFlags, deps *Deps, stderr io.Writer) error {
	if f.interval <= 0 {
		return failure.Config("serve: --interval must be positive")
	}
	if f.deadline <= 0 || f.deadline > f.interval {
		return failure.Config("serve: --deadline must be positive and at most --interval (%s)", f.interval)
	}
	loopback, err := listenAllowed(f.listen)
	if err != nil {
		return failure.Config("serve: %s", err.Error())
	}
	if !loopback && !f.unsafe {
		return failure.Config("serve: --listen %s is not loopback; pass --%s to serve /metrics beyond this machine", f.listen, unsafeListenFlag)
	}

	logger := log.New(stderr, "simplex: ", 0)
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	// A bad config or overlay is a startup error: exit 3 before binding.
	params := func(now time.Time) *check.Params {
		return &check.Params{Config: f.config, Overlay: f.overlay, Now: now}
	}
	cdeps := &check.Deps{Runner: deps.Runner, Lister: deps.Lister, Getenv: deps.Getenv}
	startNow := now().UTC().Truncate(time.Second)
	res, err := check.Prepare(params(startNow), cdeps)
	logWarnings(logger, res.Warnings)
	if err != nil {
		return startupFailure(err)
	}

	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return failure.Runtime(err, "serve: cannot listen on %s", f.listen)
	}
	if !loopback {
		logger.Printf("warning: --%s is set: serving /metrics on %s without authentication", unsafeListenFlag, ln.Addr())
	}
	logger.Printf("listening on %s interval=%s deadline=%s", ln.Addr(), f.interval, f.deadline)
	if deps.Ready != nil {
		deps.Ready(ln.Addr())
	}

	runFn := func(rctx context.Context, started time.Time) (*loop.Snapshot, error) {
		r, err := check.Run(rctx, params(started.UTC().Truncate(time.Second)), cdeps)
		logWarnings(logger, r.Warnings)
		if err != nil {
			return nil, err
		}
		if f.out != "" {
			if err := outfile.Write(f.out, r.Body); err != nil {
				return nil, &outError{err: err}
			}
		}
		doc := r.Doc
		return &loop.Snapshot{Doc: &doc, Body: r.Body}, nil
	}
	lp, err := loop.New(&loop.Config{
		Interval: f.interval,
		Deadline: f.deadline,
		Run:      runFn,
		OnRun:    func(ev loop.Event) { logRun(logger, &ev) },
	})
	if err != nil {
		_ = ln.Close()
		return failure.Config("serve: %s", err.Error())
	}
	store := lp.Store()

	srv := &http.Server{
		Handler:           metricsHandler(store, now),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		// Connection errors name the client's address; the journal gets none.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = lp.Run(loopCtx)
	}()

	var fatal error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal = failure.Runtime(err, "serve: metrics server stopped")
		}
	}
	stopLoop()
	<-loopDone
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = srv.Shutdown(sctx)
	logger.Printf("stopped")
	return fatal
}

// startupFailure reports a startup error by field path only. serve runs under
// a service manager, so its stderr is a journal: the loader's own message can
// quote a value from a private file, and `simplex validate` is where an operator
// reads that. The exit code is unchanged.
func startupFailure(err error) error {
	var re *check.RunError
	if !errors.As(err, &re) {
		return failure.Config("serve: startup failed")
	}
	return failure.Config("serve: %s is invalid (run simplex validate for the details)", re.Fields())
}

// outError marks a failed --out write. Its text names only the operation; the
// operating-system message is kept off the journal.
type outError struct{ err error }

func (e *outError) Error() string { return "write --out file" }
func (e *outError) Unwrap() error { return e.err }

// logWarnings logs loader warnings by field path and line only.
func logWarnings(l *log.Logger, ws []config.Finding) {
	for i := range ws {
		path := check.SafePath(ws[i].Path)
		l.Printf("warning: config path=%q line=%d", path, ws[i].Line)
	}
}

// logRun writes one line per pass. A failure logs where it failed, as a field
// path, never the message: messages can quote a value from a private file.
func logRun(l *log.Logger, ev *loop.Event) {
	if ev.Err == nil {
		d := ev.Snapshot.Doc
		l.Printf("run ok overall=%s vantage=%s checks=%d duration=%s",
			d.Overall, d.Vantage.Status, len(d.Checks), ev.Duration.Round(time.Millisecond))
		return
	}
	var re *check.RunError
	var oe *outError
	switch {
	case errors.As(ev.Err, &re):
		l.Printf("run failed source=%s duration=%s", re.Fields(), ev.Duration.Round(time.Millisecond))
	case errors.As(ev.Err, &oe):
		l.Printf("run failed source=out duration=%s", ev.Duration.Round(time.Millisecond))
	default:
		l.Printf("run failed source=run duration=%s", ev.Duration.Round(time.Millisecond))
	}
}

// metricsHandler serves GET /metrics from the latest snapshot and nothing else.
func metricsHandler(store *loop.Store, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := metrics.Snapshot{RunErrors: store.Errors(), Version: version, Commit: commit}
		if s := store.Load(); s != nil {
			snap.Doc, snap.RunEnded, snap.RunDuration = s.Doc, s.Ended, s.Duration
		}
		var buf bytes.Buffer
		if err := metrics.Write(&buf, &snap, now()); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})
	return mux
}
