package probe

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// startSSHPeer accepts one connection per dial and runs handle on it. The
// listener is real loopback, so the probe exercises the dialer and the read
// path rather than a stub of either.
func startSSHPeer(t *testing.T, handle func(net.Conn)) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				handle(conn)
			}()
		}
	}()
	_, portStr, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port
}

// TestSSHBannerReads checks the whole ssh classification against real peers:
// a correct banner, a wrong one, a peer that says nothing, and a closed port.
func TestSSHBannerReads(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	_, closedPortStr, _ := net.SplitHostPort(closed.Addr().String())
	closedPort, _ := strconv.Atoi(closedPortStr)
	_ = closed.Close()

	silent := make(chan struct{})
	defer close(silent)

	cases := []struct {
		name    string
		port    func(t *testing.T) int
		result  contract.Result
		outcome contract.Reason
		dials   int32
	}{
		{
			name:    "a real banner is up",
			result:  contract.ResultUp,
			outcome: contract.ReasonTCPConnectOK,
			dials:   1,
			port: func(t *testing.T) int {
				return startSSHPeer(t, func(conn net.Conn) {
					_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
				})
			},
		},
		{
			name:    "a wrong banner is down with no outcome code",
			result:  contract.ResultDown,
			outcome: "",
			dials:   1,
			port: func(t *testing.T) int {
				return startSSHPeer(t, func(conn net.Conn) {
					_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n"))
				})
			},
		},
		{
			name:    "silence is unknown with no outcome code",
			result:  contract.ResultUnknown,
			outcome: "",
			dials:   1,
			port: func(t *testing.T) int {
				return startSSHPeer(t, func(conn net.Conn) {
					// Say nothing and hold the connection past the budget.
					<-silent
				})
			},
		},
		{
			name:    "a short banner cut off by the budget is unknown",
			result:  contract.ResultUnknown,
			outcome: "",
			dials:   1,
			port: func(t *testing.T) int {
				return startSSHPeer(t, func(conn net.Conn) {
					// Two bytes, then nothing: the prefix cannot be decided.
					_, _ = conn.Write([]byte("SS"))
					<-silent
				})
			},
		},
		{
			name:    "four differing bytes are already a negative",
			result:  contract.ResultDown,
			outcome: "",
			dials:   1,
			port: func(t *testing.T) int {
				return startSSHPeer(t, func(conn net.Conn) {
					_, _ = conn.Write([]byte("XSSH"))
					<-silent
				})
			},
		},
		{
			name:    "a short banner the peer then closes is still a negative",
			result:  contract.ResultDown,
			outcome: "",
			dials:   1,
			port: func(t *testing.T) int {
				return startSSHPeer(t, func(conn net.Conn) {
					// No more bytes are coming, so two bytes cannot be a prefix.
					_, _ = conn.Write([]byte("SS"))
				})
			},
		},
		{
			name:    "a closed port is down",
			result:  contract.ResultDown,
			outcome: contract.ReasonTCPConnectRefused,
			dials:   1,
			port:    func(*testing.T) int { return closedPort },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dials atomic.Int32
			port := tc.port(t)
			runner := New(&Options{
				Timeout:    100 * time.Millisecond,
				RetryDelay: time.Millisecond,
				Dialer: dialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					return TCPDialer{}.DialContext(ctx, network, address)
				}),
			})
			obs := runner.Run(context.Background(), []Instance{{
				Host: "tower", Kind: contract.KindSSH, Target: ":22", Address: "127.0.0.1", Port: port,
			}})
			if obs[0].Result != tc.result || obs[0].Outcome != tc.outcome {
				t.Fatalf("got (%s, %q), want (%s, %q)", obs[0].Result, obs[0].Outcome, tc.result, tc.outcome)
			}
			if got := dials.Load(); got != tc.dials {
				t.Fatalf("dialled %d times, want %d: only a connect timeout is retried", got, tc.dials)
			}
		})
	}
}

// TestSSHConnectTimeoutIsRetried is the one ssh case that earns a retry.
func TestSSHConnectTimeoutIsRetried(t *testing.T) {
	var dials atomic.Int32
	runner := New(&Options{
		Timeout:    50 * time.Millisecond,
		RetryDelay: time.Millisecond,
		Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, timeoutError{}
		}),
	})
	obs := runner.Run(context.Background(), []Instance{{
		Host: "tower", Kind: contract.KindSSH, Target: ":22", Address: "placeholder.invalid", Port: 22,
	}})
	if obs[0].Result != contract.ResultUnknown || obs[0].Outcome != contract.ReasonTCPConnectTimeout {
		t.Fatalf("got (%s, %q), want (unknown, tcp_connect_timeout)", obs[0].Result, obs[0].Outcome)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialled %d times, want one attempt and one retry", got)
	}
}

// TestSSHNeverWrites pins the read-only promise: a banner exchange must not
// put a byte on the wire.
func TestSSHNeverWrites(t *testing.T) {
	read := make(chan []byte, 1)
	port := startSSHPeer(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
		buf := make([]byte, 64)
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _ := conn.Read(buf)
		read <- buf[:n]
	})
	obs := New(&Options{Timeout: time.Second}).Run(context.Background(), []Instance{{
		Host: "tower", Kind: contract.KindSSH, Target: ":22", Address: "127.0.0.1", Port: port,
	}})
	if obs[0].Result != contract.ResultUp {
		t.Fatalf("got %s, want up", obs[0].Result)
	}
	select {
	case got := <-read:
		if len(got) != 0 {
			t.Fatalf("the peer received %q, the probe must write nothing", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the peer never observed the connection ending")
	}
}

// TestLANReachFollowsTheReachRules keeps lan_reach on the same rules as
// ts_reach: the LAN path answering at all is what it reports.
func TestLANReachFollowsTheReachRules(t *testing.T) {
	open := startSSHPeer(t, func(conn net.Conn) { _ = conn.Close() })
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	_, closedPortStr, _ := net.SplitHostPort(closed.Addr().String())
	closedPort, _ := strconv.Atoi(closedPortStr)
	_ = closed.Close()

	obs := New(&Options{RetryDelay: time.Millisecond}).Run(context.Background(), []Instance{
		{Host: "tower", Kind: contract.KindLANReach, Target: ":22", Address: "127.0.0.1", Port: open},
		{Host: "tower", Kind: contract.KindLANReach, Target: ":22", Address: "127.0.0.1", Port: closedPort},
	})
	if obs[0].Result != contract.ResultUp || obs[0].Outcome != contract.ReasonTCPConnectOK {
		t.Fatalf("open port: got (%s, %q), want (up, tcp_connect_ok)", obs[0].Result, obs[0].Outcome)
	}
	if obs[1].Result != contract.ResultUp || obs[1].Outcome != contract.ReasonTCPConnectRefused {
		t.Fatalf("closed port: got (%s, %q), want (up, tcp_connect_refused): a refusal proves the LAN path is alive", obs[1].Result, obs[1].Outcome)
	}
}
