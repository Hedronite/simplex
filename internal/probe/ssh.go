package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
)

// sshPrefix is what a real SSH banner starts with. Nothing else is SSH.
var sshPrefix = []byte("SSH-")

// bannerLimit bounds how much of a banner is read. A server identifies itself
// in one short line; anything longer is not a banner we need.
const bannerLimit = 255

// probeSSH connects and reads the identification banner, without writing a
// byte and without staying a moment past the attempt budget.
//
// The classification separates three different failures that all look like
// "no service": a refusal is definitive (down), a wrong banner proves something
// else is listening (down), and silence leaves the question open (unknown).
// Silence carries no outcome code because the contract has none for a peer
// that accepted a connection and said nothing, and it is not retried: the
// retry exists for a connect that timed out, not for a host that answered.
func (o *Options) probeSSH(ctx context.Context, inst *Instance) (contract.Result, contract.Reason, bool) {
	conn, err := o.dial(ctx, inst.Address, inst.Port)
	if err != nil {
		return classifyDial(err)
	}
	defer func() { _ = conn.Close() }()

	banner, err := readBanner(ctx, conn)
	if err != nil && len(banner) == 0 {
		// Connected, then nothing said before the budget ran out.
		return contract.ResultUnknown, "", false
	}
	if bytes.HasPrefix(banner, sshPrefix) {
		return contract.ResultUp, contract.ReasonTCPConnectOK, false
	}
	if len(banner) == 0 {
		return contract.ResultUnknown, "", false
	}
	return contract.ResultDown, "", false
}

// readBanner reads at most bannerLimit bytes of the banner and returns as soon
// as the answer cannot change: the prefix does not match, a line ends, or the
// budget is gone. The connection is only ever read from.
func readBanner(ctx context.Context, conn net.Conn) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
	}
	// A cancelled run must not wait for the deadline to arrive.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()

	buf := make([]byte, 0, bannerLimit)
	chunk := make([]byte, 64)
	for len(buf) < bannerLimit {
		n, err := conn.Read(chunk[:min(len(chunk), bannerLimit-len(buf))])
		buf = append(buf, chunk[:n]...)
		// Decide as early as the bytes allow.
		if len(buf) >= len(sshPrefix) && !bytes.HasPrefix(buf, sshPrefix) {
			return buf, nil
		}
		if bytes.HasSuffix(buf, []byte("\n")) {
			return buf, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
	}
	return buf, nil
}
