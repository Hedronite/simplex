package probe

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"

	"github.com/Hedronite/simplex/internal/contract"
)

// errDeniedPort is returned instead of dialling. It never carries the address.
var errDeniedPort = errors.New("probe: port is never dialled")

// dial reaches one address, refusing the port the mesh never dials.
func (o *Options) dial(ctx context.Context, address string, port int) (net.Conn, error) {
	if port == contract.DeniedPort {
		return nil, errDeniedPort
	}
	target := net.JoinHostPort(address, strconv.Itoa(port))
	return o.Dialer.DialContext(ctx, "tcp", target)
}

// probeTCP classifies a bare TCP connect.
//
// A refusal is a definitive answer, but what it means depends on the kind:
// ts_reach and lan_reach ask whether the host answers at all, so a refusal
// proves the path is alive and the result is up; kube_apiserver asks whether
// the service listens, so a refusal is down. Everything indeterminate is
// unknown, and only a timeout earns the retry.
func (o *Options) probeTCP(ctx context.Context, inst *Instance) (contract.Result, contract.Reason, bool) {
	conn, err := o.dial(ctx, inst.Address, inst.Port)
	if err != nil {
		result, outcome, timedOut := classifyDial(err)
		if result == contract.ResultDown && outcome == contract.ReasonTCPConnectRefused {
			switch inst.Kind {
			case contract.KindTSReach, contract.KindLANReach:
				return contract.ResultUp, contract.ReasonTCPConnectRefused, false
			}
		}
		return result, outcome, timedOut
	}
	// The connect is the whole probe; nothing is read from the connection.
	_ = conn.Close()
	return contract.ResultUp, contract.ReasonTCPConnectOK, false
}

// classifyDial maps a failed dial onto the contract's result and outcome. The
// third return says whether the failure was a timeout, which is the only
// condition the runner retries.
func classifyDial(err error) (contract.Result, contract.Reason, bool) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// A name that does not resolve says nothing about the host, and the
		// contract has no outcome code for it.
		return contract.ResultUnknown, "", false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return contract.ResultDown, contract.ReasonTCPConnectRefused, false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return contract.ResultUnknown, contract.ReasonTCPConnectTimeout, true
	}
	// Unreachable networks and hosts are indeterminate and carry no outcome.
	return contract.ResultUnknown, "", false
}
