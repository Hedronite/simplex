package conformance

import (
	"fmt"
	"slices"
	"testing"

	"github.com/Hedronite/simplex/internal/contract"
)

// enrichmentAgreement checks the S3 enrichment rules on one check: only a
// ts_reach check carries enrichment codes, at most one per group, and the codes
// are informational, so they appear with whatever result and severity the other
// rules already decided. A blind check carries none.
func enrichmentAgreement(c check) error {
	var peer, path []string
	for _, code := range c.Reasons {
		r := contract.Reason(code)
		switch {
		case slices.Contains(contract.EnrichmentPeerState, r):
			peer = append(peer, code)
		case slices.Contains(contract.EnrichmentPath, r):
			path = append(path, code)
		}
	}
	if len(peer)+len(path) == 0 {
		return nil
	}
	if c.Kind != string(contract.KindTSReach) {
		return fmt.Errorf("%s %s: enrichment codes are only valid on ts_reach", c.Host, c.Kind)
	}
	if hasReason(c.Reasons, contract.ReasonVantageBlind) {
		return fmt.Errorf("%s %s: a blind check carries no enrichment codes", c.Host, c.Kind)
	}
	if len(peer) > 1 {
		return fmt.Errorf("%s %s: more than one peer-state code %v", c.Host, c.Kind, peer)
	}
	if len(path) > 1 {
		return fmt.Errorf("%s %s: more than one path code %v", c.Host, c.Kind, path)
	}
	if len(path) == 1 && len(peer) == 1 && peer[0] == string(contract.ReasonTSPeerUnmatched) {
		return fmt.Errorf("%s %s: an unmatched peer has no path to report", c.Host, c.Kind)
	}
	if len(path) == 1 && len(peer) == 0 {
		return fmt.Errorf("%s %s: a path code needs a matched peer-state code", c.Host, c.Kind)
	}
	return nil
}

func TestEnrichmentIsItsOwnClass(t *testing.T) {
	for _, r := range append(slices.Clone(contract.EnrichmentPeerState), contract.EnrichmentPath...) {
		found := false
		for _, s := range contract.ReasonSpecs {
			if s.Code == r {
				found = true
				if s.Class != contract.ClassEnrichment {
					t.Errorf("%s is class %s, want enrichment", r, s.Class)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in ReasonSpecs", r)
		}
	}
	// Enrichment codes are not outcome codes, so the outcome rules ignore them.
	for _, code := range outcomeReasons([]string{"ts_peer_online", "ts_path_direct", "tcp_connect_ok"}) {
		if code != "tcp_connect_ok" {
			t.Errorf("%s counted as an outcome code", code)
		}
	}
}

func TestEnrichmentAgreementTable(t *testing.T) {
	ck := func(kind string, reasons ...string) check {
		return check{Host: "h", Kind: kind, Result: "up", Reasons: reasons}
	}
	good := []check{
		ck("ts_reach", "tcp_connect_ok"),
		ck("ts_reach", "tcp_connect_ok", "ts_peer_online", "ts_path_direct"),
		ck("ts_reach", "tcp_connect_ok", "ts_peer_online", "ts_path_relayed"),
		ck("ts_reach", "probe_unknown", "tcp_connect_timeout", "ts_peer_offline"),
		ck("ts_reach", "ts_peer_unmatched"),
		ck("node_exporter", "tcp_connect_refused"),
	}
	for _, c := range good {
		if err := enrichmentAgreement(c); err != nil {
			t.Errorf("%v should agree: %v", c, err)
		}
	}
	bad := []check{
		ck("node_exporter", "ts_peer_online"),
		ck("prom_up", "ts_path_direct", "ts_peer_online"),
		ck("ts_reach", "ts_peer_online", "ts_peer_offline"),
		ck("ts_reach", "ts_peer_online", "ts_path_direct", "ts_path_relayed"),
		ck("ts_reach", "ts_peer_unmatched", "ts_path_direct"),
		ck("ts_reach", "ts_path_direct"),
		ck("ts_reach", "vantage_blind", "ts_peer_online"),
	}
	for _, c := range bad {
		if err := enrichmentAgreement(c); err == nil {
			t.Errorf("%v should be rejected", c)
		}
	}
}
