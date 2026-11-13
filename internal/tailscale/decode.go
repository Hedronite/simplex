package tailscale

import (
	"encoding/json"
	"net/netip"
	"strings"
)

// statusWire is the slice of the daemon's status payload this package reads.
// Unknown fields are ignored on purpose: the payload shape moves between
// Tailscale versions, and a field this build does not know is not an error.
type statusWire struct {
	BackendState string              `json:"BackendState"`
	Peer         map[string]peerWire `json:"Peer"`
}

// peerWire names only what answers the two questions S3 asks: is the peer
// online, and is its path direct or relayed. Everything else in the payload,
// including keys, users, control URLs and API URLs, is dropped by not naming it.
type peerWire struct {
	Online       *bool    `json:"Online"`
	CurAddr      string   `json:"CurAddr"`
	Relay        string   `json:"Relay"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
}

// decodeStatus reduces a status payload to a Status. A payload that is not a
// JSON object is an error; a payload missing fields yields a status with those
// facts absent, which callers read as "no enrichment".
func decodeStatus(body []byte) (*Status, error) {
	var wire statusWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	status := &Status{
		BackendState: wire.BackendState,
		peers:        make(map[string]Enrichment, len(wire.Peer)*3),
	}
	for _, p := range wire.Peer {
		direct := p.CurAddr != ""
		enrichment := Enrichment{
			Online: p.Online != nil && *p.Online,
			Direct: direct,
			// A relay is only interesting when the path is not direct.
			Relayed: !direct && p.Relay != "",
		}
		// Keyed by every handle a caller might hold, so a lookup by IP and a
		// lookup by MagicDNS name land on the same peer. The keys stay
		// unexported.
		if name := normalise(p.DNSName); name != "" {
			status.peers[name] = enrichment
		}
		for _, ip := range p.TailscaleIPs {
			if key := normalise(ip); key != "" {
				status.peers[key] = enrichment
			}
		}
	}
	return status, nil
}

// normalise turns an address or name into a lookup key. IPs are canonicalised
// so two spellings of one address agree; names lose the trailing dot MagicDNS
// appends and are compared case-insensitively, as DNS is.
func normalise(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		return ip.String()
	}
	return strings.TrimSuffix(strings.ToLower(s), ".")
}
