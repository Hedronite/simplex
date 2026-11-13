package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// plan is what a fake seam must answer for one instance: the result and outcome
// the probe is expected to classify. The fixture test derives plans from each
// case's document, so the real probe code classifies what the fakes return.
type plan struct {
	kind    contract.Kind
	result  contract.Result
	outcome contract.Reason
	path    string
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func dialError(op string, err error) error {
	return &net.OpError{Op: op, Net: "tcp", Err: err}
}

func refused() error {
	return dialError("dial", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED})
}

func unreachable() error {
	return dialError("dial", &os.SyscallError{Syscall: "connect", Err: syscall.ENETUNREACH})
}

// fakeDialer answers TCP dials by "address:port".
type fakeDialer struct{ plans map[string]plan }

func (d fakeDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	p, ok := d.plans[address]
	if !ok {
		return nil, fmt.Errorf("fake dialer: nothing planned for %s", address)
	}
	switch p.outcome {
	case contract.ReasonTCPConnectOK:
		c, peer := net.Pipe()
		_ = peer.Close()
		return c, nil
	case contract.ReasonTCPConnectRefused:
		return nil, refused()
	case contract.ReasonTCPConnectTimeout:
		return nil, dialError("dial", timeoutError{})
	default:
		return nil, unreachable()
	}
}

// fakeTransport answers HTTP requests by "address:port" plus path.
type fakeTransport struct{ plans map[string]plan }

func (t fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.URL.Host + req.URL.Path
	p, ok := t.plans[key]
	if !ok {
		return nil, fmt.Errorf("fake transport: nothing planned for %s", key)
	}
	reply := func(status int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     strconv.Itoa(status) + " " + http.StatusText(status),
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	switch {
	case p.outcome == contract.ReasonPromNotReady:
		return reply(http.StatusServiceUnavailable, "")
	case p.outcome == contract.ReasonPromAuth:
		return reply(http.StatusUnauthorized, "")
	case p.outcome == contract.ReasonMetricsBodyBad:
		return reply(http.StatusOK, "other_series 1\n")
	case p.outcome == contract.ReasonHTTPUnexpectedStatus:
		return reply(http.StatusInternalServerError, "")
	case p.outcome == contract.ReasonTCPConnectRefused:
		return nil, refused()
	case p.outcome == contract.ReasonTCPConnectTimeout:
		return nil, dialError("dial", timeoutError{})
	case p.outcome == contract.ReasonHTTPTimeout:
		return nil, dialError("read", timeoutError{})
	case p.result == contract.ResultUp:
		body := "up 1\n"
		if p.kind == contract.KindPromUp && req.URL.Path == contract.PathMetrics {
			body = "prometheus_build_info 1\n"
		}
		return reply(http.StatusOK, body)
	default:
		return nil, unreachable()
	}
}

// instantClock makes the retry delay free.
type instantClock struct{}

func (instantClock) Now() time.Time { return time.Now() }
func (instantClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

// fakeLister answers the vantage's local-address test.
type fakeLister bool

func (l fakeLister) HasTailnetAddress() bool { return bool(l) }
