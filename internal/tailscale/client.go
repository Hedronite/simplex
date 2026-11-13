// Package tailscale reads peer state from the local Tailscale daemon.
//
// This is optional enrichment, and the access model is deliberately narrow: no
// root, no operator role, no exec, no tailscale CLI, no tailscale.com module.
// The client is a hand-written HTTP client over a unix socket that issues one
// request, a GET of one constant path, and decodes only the handful of fields
// that answer "is this peer online, and is its path direct". The payload is
// parsed, reduced to booleans and dropped.
//
// Four rules shape the code, and each one has a test:
//
//   - One call. The path and method are constants, and any other pair is
//     refused before a socket is opened.
//   - No payload leaves. Failures become one of four fixed sentinels with no
//     wrapped text, so no address, DNS name or key can reach an error string, a
//     log line or a document.
//   - Addresses never leave. Status keeps peer addresses and names unexported
//     so Match can resolve an overlay value; nothing else can read them.
//   - Fail soft. Every failure is a sentinel, because S3 changes no result and
//     no severity: an unavailable daemon means no enrichment.
package tailscale

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// statusPath is the one request this package makes.
	statusPath = "/localapi/v0/status"
	// statusMethod is the one method it uses.
	statusMethod = http.MethodGet
	// RequestTimeout bounds the call inside the caller's deadline.
	RequestTimeout = 2 * time.Second
	// MaxBody bounds the response. The status payload is about 18 KB on a
	// twelve-peer tailnet; 4 MiB is far above any real shape and still a bound.
	MaxBody = 4 << 20
)

// The only errors this package returns. Each is fixed: none wraps payload,
// socket or transport text.
var (
	// ErrUnavailable means the daemon could not be reached, timed out, or
	// answered with a server error.
	ErrUnavailable = errors.New("tailscale: local daemon unavailable")
	// ErrDenied means the daemon refused the call, or the socket could not be
	// opened for lack of permission.
	ErrDenied = errors.New("tailscale: local daemon denied the call")
	// ErrBadPayload means the daemon answered but the body was not a status
	// object.
	ErrBadPayload = errors.New("tailscale: local daemon answered with an unusable payload")
	// ErrTooLarge means the body exceeded MaxBody.
	ErrTooLarge = errors.New("tailscale: local daemon payload exceeded the size cap")
)

// Enrichment is what one matched peer contributes: three booleans and nothing
// else. No address, DNS name or key crosses this boundary.
type Enrichment struct {
	// Online is the control plane's view of the peer.
	Online bool
	// Direct is true when the peer has a current address, which means traffic
	// does not need a relay.
	Direct bool
	// Relayed is true when the peer is not direct and has a relay.
	Relayed bool
}

// Status is a reduced status payload. It holds peer addresses and names
// unexported, only so Match can resolve them.
type Status struct {
	// BackendState is the daemon's state, for example "Running".
	BackendState string

	peers map[string]Enrichment
}

// Match resolves an overlay value to a peer. The value may be an IP literal or
// a DNS name, with or without the trailing dot, in any case.
func (s *Status) Match(address string) (Enrichment, bool) {
	if s == nil {
		return Enrichment{}, false
	}
	enrichment, ok := s.peers[normalise(address)]
	return enrichment, ok
}

// Peers reports how many peers the snapshot holds, so a caller can warn about
// an empty map without reaching into it.
func (s *Status) Peers() int {
	if s == nil {
		return 0
	}
	return len(s.peers)
}

// Client reads the local daemon over its unix socket.
type Client struct {
	http *http.Client
}

// ValidateSocketPath accepts only an absolute filesystem path. A URL or a
// host:port is a configuration error: this slice speaks to a unix socket, and a
// remote endpoint must never be guessed from a string.
func ValidateSocketPath(path string) error {
	switch {
	case path == "":
		return errors.New("tailscale: socket path is empty")
	case !filepath.IsAbs(path):
		return errors.New("tailscale: socket path must be absolute")
	case strings.Contains(path, "://"):
		return errors.New("tailscale: socket path must not be a URL")
	}
	if host, port, err := net.SplitHostPort(path); err == nil && host != "" && port != "" {
		return errors.New("tailscale: socket path must not be a host and port")
	}
	return nil
}

// New returns a client for the daemon at path. The path is validated here as
// well as by the caller: it is the one argument that could send a request off
// the machine.
func New(path string) (*Client, error) {
	if err := ValidateSocketPath(path); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: RequestTimeout}
	transport := &http.Transport{
		// http.Transport passes the URL host here, so the socket path is used
		// directly and the URL host is a placeholder that reaches no resolver.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
		DisableCompression: true,
		MaxIdleConns:       1,
		IdleConnTimeout:    30 * time.Second,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Status makes the one call and returns the reduced snapshot.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	body, err := c.get(ctx, statusMethod, statusPath)
	if err != nil {
		return nil, err
	}
	status, err := decodeStatus(body)
	if err != nil {
		return nil, ErrBadPayload
	}
	return status, nil
}

// get is the only place a request is built, and it refuses any pair other than
// the one constant this package is allowed to use.
func (c *Client) get(ctx context.Context, method, path string) ([]byte, error) {
	if method != statusMethod || path != statusPath {
		return nil, errors.New("tailscale: only the status call is allowed")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://local"+path, http.NoBody)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusUnauthorized:
		return nil, ErrDenied
	default:
		return nil, ErrUnavailable
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(body) > MaxBody {
		return nil, ErrTooLarge
	}
	return body, nil
}

// classifyTransportError maps a transport failure onto a sentinel by the
// errno it wraps. It never quotes the error, because transport errors carry the
// socket path and, with an HTTP transport, the address.
func classifyTransportError(err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return ErrDenied
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return ErrUnavailable
	default:
		// Missing socket, refused connection, reset, or anything else.
		return ErrUnavailable
	}
}
