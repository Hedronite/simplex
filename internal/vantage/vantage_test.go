package vantage

import (
	"context"
	"net"
	"testing"

	"github.com/Hedronite/simplex/internal/contract"
)

// fixedLister is the seam the replay and these tests use: the decision must not
// depend on the machine running it.
type fixedLister bool

func (f fixedLister) HasTailnetAddress() bool { return bool(f) }

func yes(context.Context) bool { return true }
func no(context.Context) bool  { return false }

func TestStatuses(t *testing.T) {
	cases := []struct {
		name      string
		onTailnet bool
		canaries  []Canary
		want      contract.VantageStatus
	}{
		{"no local address is blind even with canaries", false, []Canary{yes}, contract.VantageBlind},
		{"no local address and no canaries is blind", false, nil, contract.VantageBlind},
		{"local address and no canaries is unchecked", true, nil, contract.VantageUnchecked},
		{"local address and one canary answering is ok", true, []Canary{yes}, contract.VantageOK},
		{"one of two canaries answering is ok", true, []Canary{no, yes}, contract.VantageOK},
		{"every canary failing is blind", true, []Canary{no, no}, contract.VantageBlind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := New(Options{Lister: fixedLister(tc.onTailnet), Canaries: tc.canaries}).Status(context.Background())
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestFirstCanaryWins pins the short circuit: a canary that answers ends the
// check, so a later canary is never dialled.
func TestFirstCanaryWins(t *testing.T) {
	var called int
	second := func(context.Context) bool { called++; return false }
	got := New(Options{Lister: fixedLister(true), Canaries: []Canary{yes, second}}).Status(context.Background())
	if got != contract.VantageOK {
		t.Fatalf("got %s, want ok", got)
	}
	if called != 0 {
		t.Fatalf("the second canary was called %d times after the first answered", called)
	}
}

// TestNewFillsTheRealLister pins the wiring: an unset seam becomes the real
// reader, so the shipped binary has exactly one path. The status itself is
// machine-dependent and is not asserted here.
func TestNewFillsTheRealLister(t *testing.T) {
	if _, ok := New(Options{}).lister.(NetworkLister); !ok {
		t.Fatalf("New with no lister used %T, want NetworkLister", New(Options{}).lister)
	}
	if _, ok := Default().lister.(NetworkLister); !ok {
		t.Fatalf("Default used %T, want NetworkLister", Default().lister)
	}
}

func TestIsTailnetAddress(t *testing.T) {
	cases := []struct {
		name string
		ip   net.IP
		want bool
	}{
		// First and last address of the RFC 6598 range, then one outside on
		// each side.
		{"first address in range", net.IPv4(100, 64, 0, 1), true},
		{"last address in range", net.IPv4(100, 127, 255, 255), true},
		{"network address itself", net.IPv4(100, 64, 0, 0), true},
		{"one below the range", net.IPv4(100, 63, 255, 255), false},
		{"one above the range", net.IPv4(100, 128, 0, 0), false},
		{"documentation range", net.IPv4(192, 0, 2, 1), false},
		{"loopback", net.IPv4(127, 0, 0, 1), false},
		{"ipv6 loopback", net.ParseIP("::1"), false},
		{"unspecified", net.IPv4zero, false},
	}
	for _, tc := range cases {
		if got := IsTailnetAddress(tc.ip); got != tc.want {
			t.Errorf("IsTailnetAddress(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if IsTailnetAddress(nil) {
		t.Error("IsTailnetAddress(nil) = true, want false")
	}
}
