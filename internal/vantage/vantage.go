// Package vantage decides whether this vantage can see the mesh at all.
//
// The inputs are seams: a lister that answers whether a tailnet address is
// assigned to an interface, and one function per canary that answers whether
// that canary reached its target. New fills an unset lister with the real
// interface reader and Default wires only real seams, so the shipped binary has
// one path; a caller that needs a deterministic answer (the in-process fixture
// replay) supplies its own. There is no flag, environment variable or build tag
// that swaps the lister in the shipped binary.
package vantage

import (
	"context"
	"net"

	"github.com/Hedronite/simplex/internal/contract"
)

// cgnat is the RFC 6598 shared address range every tailnet address comes from.
const cgnat = "100.64.0.0/10"

// Lister reports whether the machine running the probe has a tailnet address.
type Lister interface {
	HasTailnetAddress() bool
}

// Canary reports whether one canary check reached its target. The caller wraps
// a probe run; this package only needs the yes or no.
type Canary func(ctx context.Context) bool

// Options is what a Prober is built from.
type Options struct {
	// Lister answers the local test. Nil means NetworkLister.
	Lister Lister
	// Canaries are dialled in order until one answers. None means the status
	// cannot be anything but unchecked or blind.
	Canaries []Canary
}

// Prober decides the vantage status through one fixed set of seams.
type Prober struct {
	lister   Lister
	canaries []Canary
}

// New returns a Prober, filling an unset lister with the real interface reader.
func New(opts Options) *Prober {
	lister := opts.Lister
	if lister == nil {
		lister = NetworkLister{}
	}
	return &Prober{lister: lister, canaries: opts.Canaries}
}

// Default returns a Prober that reads the real interfaces and has no canaries,
// which reports unchecked on a machine that is on the tailnet and blind on one
// that is not.
func Default() *Prober { return New(Options{}) }

// Status decides the vantage status.
//
// A missing local address is decisive: without it nothing below can be trusted,
// so the answer is blind whether or not canaries were configured. With an
// address and no canaries the run is unchecked, which is an honest "nobody
// promised me this vantage is on the mesh". With canaries, one success is
// enough; all failures are blind.
func (p *Prober) Status(ctx context.Context) contract.VantageStatus {
	if p.lister == nil || !p.lister.HasTailnetAddress() {
		return contract.VantageBlind
	}
	if len(p.canaries) == 0 {
		return contract.VantageUnchecked
	}
	for _, canary := range p.canaries {
		if canary(ctx) {
			return contract.VantageOK
		}
	}
	return contract.VantageBlind
}

// IsTailnetAddress reports whether ip is in the RFC 6598 range.
func IsTailnetAddress(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	_, network, err := net.ParseCIDR(cgnat)
	if err != nil {
		return false
	}
	return network.Contains(ip)
}
