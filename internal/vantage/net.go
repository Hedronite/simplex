package vantage

import (
	"net"
)

// NetworkLister reads the machine's interfaces. It is the only thing in this
// package that touches the real network stack, and main is its only caller.
type NetworkLister struct{}

// HasTailnetAddress reports whether any up interface holds an RFC 6598
// address. It reads addresses only: no packet is sent, no name is resolved.
func (NetworkLister) HasTailnetAddress() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if IsTailnetAddress(ipNet.IP) {
				return true
			}
		}
	}
	return false
}
