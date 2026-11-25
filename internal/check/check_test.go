package check

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/probe"
)

var runNow = time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)

// Sentinels are reserved documentation values written as plain literals:
// TEST-NET addresses and a name under .example (RFC 2606). They are visible to
// the tree scrub, which only warns on them, and can never be real values.
const (
	secretIP   = "192.0.2.77"
	secretHost = "edge.tail.example"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodConfig = "apiVersion: simplex.config/v0\nvantage:\n  id: lab\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n"

func deps() *Deps {
	return &Deps{Runner: probe.Default(), Getenv: func(string) string { return "" }}
}

// A failed run says where it failed in field paths only. A key mistyped in a
// private file must never come back out of Fields, because serve logs it.
func TestFieldsNeverRepeatPrivateValues(t *testing.T) {
	dir := t.TempDir()
	cfg := write(t, dir, "c.yaml", goodConfig)
	for name, overlayText := range map[string]string{
		"address as a top-level key": "apiVersion: simplex.overlay/v0\n192.0.2.77: x\nhosts:\n  spoke:\n    address: placeholder.invalid\n",
		"address as a host key":      "apiVersion: simplex.overlay/v0\nhosts:\n  192.0.2.77:\n    address: placeholder.invalid\n",
		"hostname as an extra key":   "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: placeholder.invalid\n    edge.tail.example: y\n",
	} {
		t.Run(name, func(t *testing.T) {
			ov := write(t, dir, "o-"+strings.ReplaceAll(name, " ", "-")+".txt", overlayText)
			_, err := Run(context.Background(), &Params{Config: cfg, Overlay: ov, Now: runNow}, deps())
			var re *RunError
			if !errors.As(err, &re) {
				t.Fatalf("want a *RunError, got %T: %v", err, err)
			}
			if f := re.Fields(); strings.Contains(f, secretIP) || strings.Contains(f, secretHost) || strings.Contains(f, "placeholder") {
				t.Fatalf("Fields repeats a value from the private file: %q", f)
			}
			if re.Source != "overlay" {
				t.Fatalf("source %q, want overlay", re.Source)
			}
		})
	}
}

func TestFieldsKeepRealFieldPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := write(t, dir, "c.yaml", "apiVersion: simplex.config/v0\nvantage:\n  id: lab\n  canaries:\n    - {host: ghost, kind: ts_reach, target: \":9100\"}\nhosts:\n  - id: spoke\n    checks:\n      - {kind: ts_reach, expected: light}\n")
	_, err := Run(context.Background(), &Params{Config: cfg, Now: runNow}, deps())
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("want a *RunError, got %T", err)
	}
	if got := re.Fields(); got != "config vantage.canaries[0].host" {
		t.Fatalf("Fields = %q", got)
	}
	if strings.Contains(re.Fields(), "ghost") {
		t.Fatal("Fields repeats a value")
	}
}

func TestSafePath(t *testing.T) {
	keep := []string{"", "hosts", "hosts[0].checks[1].path", "vantage.canaries[0].host", "hosts.spoke.address", "hosts.spoke.overrides[0].kind", "apiVersion"}
	drop := []string{secretIP, secretHost, "hosts.Spoke", "hosts.198.51.100.9.address", "weird key", "hosts[x].id", "hosts[0"}
	for _, p := range keep {
		if safePath(p) != p {
			t.Errorf("safePath(%q) dropped a real field path", p)
		}
	}
	for _, p := range drop {
		if got := safePath(p); got != "" {
			t.Errorf("safePath(%q) = %q, want it dropped", p, got)
		}
	}
}

func TestMissingConfigIsARunErrorAndWarningsSurvive(t *testing.T) {
	res, err := Run(context.Background(), &Params{Config: filepath.Join(t.TempDir(), "absent.yaml"), Now: runNow}, deps())
	if err == nil || res == nil {
		t.Fatalf("want an error and a non-nil result, got %v, %v", res, err)
	}
	var re *RunError
	if !errors.As(err, &re) || re.Source != "config" {
		t.Fatalf("error %T %v", err, err)
	}
}

type cancelWaitDialer struct {
	entered chan struct{}
	delay   time.Duration
}

func (d cancelWaitDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	if d.entered != nil {
		select {
		case d.entered <- struct{}{}:
		default:
		}
	}
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
			c, peer := net.Pipe()
			_ = peer.Close()
			return c, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type staticLister bool

func (l staticLister) HasTailnetAddress() bool { return bool(l) }

func TestRunCancelledMidProbeReturnsError(t *testing.T) {
	dir := t.TempDir()
	cfg := write(t, dir, "c.yaml", goodConfig)
	ov := write(t, dir, "o.txt", "apiVersion: simplex.overlay/v0\nhosts:\n  spoke:\n    address: spoke.invalid\n")
	entered := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	_, err := Run(ctx,
		&Params{Config: cfg, Overlay: ov, Now: runNow},
		&Deps{Runner: probe.New(&probe.Options{Dialer: cancelWaitDialer{entered: entered}}), Lister: staticLister(true), Getenv: func(string) string { return "" }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunThreadsDeadlineIntoRunner(t *testing.T) {
	dir := t.TempDir()
	cfg := write(t, dir, "c.yaml", "apiVersion: simplex.config/v0\nvantage:\n  id: lab\nhosts:\n  - id: h1\n    checks:\n      - {kind: ts_reach, expected: light}\n  - id: h2\n    checks:\n      - {kind: ts_reach, expected: light}\n")
	ov := write(t, dir, "o.txt", "apiVersion: simplex.overlay/v0\nhosts:\n  h1:\n    address: h1.invalid\n  h2:\n    address: h2.invalid\n")
	base := probe.New(&probe.Options{
		Dialer:      cancelWaitDialer{delay: 60 * time.Millisecond},
		Concurrency: 1,
	})
	d := &Deps{Runner: base, Lister: staticLister(true), Getenv: func(string) string { return "" }}

	res, err := Run(context.Background(), &Params{Config: cfg, Overlay: ov, Now: runNow, Deadline: 250 * time.Millisecond}, d)
	if err != nil {
		t.Fatalf("250ms deadline failed: %v", err)
	}
	for i, c := range res.Doc.Checks {
		if c.Result != contract.ResultUp {
			t.Fatalf("check %d got %s, want up", i, c.Result)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, &Params{Config: cfg, Overlay: ov, Now: runNow, Deadline: 80 * time.Millisecond}, d); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}
