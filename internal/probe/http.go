package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// buildInfoSeries is the series a Prometheus /metrics body carries once it is
// serving metrics. Its absence means something answered, but not Prometheus.
var buildInfoSeries = []byte("prometheus_build_info")

// sharedTransport is reused across runs so a serve loop keeps its sockets.
var sharedTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: MaxTimeout, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          4,
	MaxIdleConnsPerHost:   1,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   time.Second,
	ExpectContinueTimeout: 0,
	DisableCompression:    true,
}

var (
	sharedClientOnce sync.Once
	sharedClient     *http.Client
)

// client returns the client for this run. Redirects are never followed: a
// probe answers for the path it named, not for wherever it is sent. The client
// is checked out per call because the transport seam is per run; the shared
// client is only cheap when no seam was supplied.
func (o *Options) client() *http.Client {
	if o.Transport == nil {
		sharedClientOnce.Do(func() {
			sharedClient = &http.Client{Transport: sharedTransport, CheckRedirect: noRedirect}
		})
		return sharedClient
	}
	return &http.Client{Transport: o.Transport, CheckRedirect: noRedirect}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// probeHTTP classifies an HTTP check.
//
// The status map is semantics section 3 and translates to the outcome codes in
// section 4, so a Prometheus that answers 503 on its readiness path is up with
// prom_not_ready, and one that answers 401 is up with prom_auth: the service is
// alive and misconfigured, which is a different fact from being down.
func (o *Options) probeHTTP(ctx context.Context, inst *Instance) (contract.Result, contract.Reason, bool) {
	path := inst.Path
	if path == "" {
		switch inst.Kind {
		case contract.KindNodeExporter:
			path = contract.PathMetrics
		case contract.KindPromUp:
			path = contract.PathReady
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url(inst, path), nil)
	if err != nil {
		// The request never left: nothing is known about the host.
		return contract.ResultUnknown, "", false
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return classifyHTTPError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The build-info assertion applies to a Prometheus /metrics body only.
	// node_exporter serves a different series and must not be judged by it.
	checkBody := inst.Kind == contract.KindPromUp && path == contract.PathMetrics
	readyPath := inst.Kind == contract.KindPromUp && path == contract.PathReady
	hasBuildInfo, bodyErr := readBuildInfo(resp.Body, checkBody)
	if bodyErr != nil {
		return classifyHTTPError(bodyErr)
	}
	return classifyStatus(resp.StatusCode, checkBody, readyPath, hasBuildInfo)
}

// url builds the request target. The address is used only here: it never
// reaches an error, an Observation or a document.
func (o *Options) url(inst *Instance, path string) string {
	host := net.JoinHostPort(inst.Address, strconv.Itoa(inst.Port))
	return "http://" + host + path
}

// readBuildInfo drains at most MaxBody bytes, remembering whether the
// build-info series appeared in the first buildInfoWindow bytes. A body larger
// than the cap is truncated, not failed: the cap exists to bound the read, not
// to judge the peer.
func readBuildInfo(body io.Reader, wantSeries bool) (bool, error) {
	limited := io.LimitReader(body, MaxBody)
	if !wantSeries {
		_, err := io.Copy(io.Discard, limited)
		return false, err
	}
	head := make([]byte, buildInfoWindow)
	n, err := io.ReadFull(limited, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	found := bytes.Contains(head[:n], buildInfoSeries)
	// Drain the rest of the window budget so the connection can be reused.
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return false, err
	}
	return found, nil
}

// classifyStatus applies semantics section 3 to a response.
func classifyStatus(status int, checkBody, readyPath, hasBuildInfo bool) (contract.Result, contract.Reason, bool) {
	switch {
	case status == http.StatusOK:
		if checkBody && !hasBuildInfo {
			return contract.ResultUp, contract.ReasonMetricsBodyBad, false
		}
		return contract.ResultUp, "", false
	case status == http.StatusServiceUnavailable && readyPath:
		// The process answered, so it is up; it just cannot take traffic yet.
		return contract.ResultUp, contract.ReasonPromNotReady, false
	case status == http.StatusServiceUnavailable:
		return contract.ResultDown, contract.ReasonHTTPUnexpectedStatus, false
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return contract.ResultUp, contract.ReasonPromAuth, false
	default:
		return contract.ResultDown, contract.ReasonHTTPUnexpectedStatus, false
	}
}

// classifyHTTPError separates a connect-phase timeout from a response-phase
// one, because the contract has a code for each.
func classifyHTTPError(err error) (contract.Result, contract.Reason, bool) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return contract.ResultUnknown, "", false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return contract.ResultDown, contract.ReasonTCPConnectRefused, false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return contract.ResultUnknown, contract.ReasonTCPConnectTimeout, true
		}
		return contract.ResultUnknown, contract.ReasonHTTPTimeout, true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return contract.ResultUnknown, contract.ReasonHTTPTimeout, true
	}
	// Unreachable, reset, or a cancelled context: indeterminate, no outcome.
	return contract.ResultUnknown, "", false
}
