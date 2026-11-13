package severity

import (
	"slices"
	"testing"

	"github.com/Hedronite/simplex/internal/contract"
)

func ts(enrichment ...contract.Reason) Observation {
	o := obs("h", contract.KindTSReach, contract.ExpectedLight, contract.ResultUp, contract.ReasonTCPConnectOK)
	o.Enrichment = enrichment
	return o
}

func TestEnrichmentRidesAlongAndNeverChangesAVerdict(t *testing.T) {
	plain := eval(t, contract.VantageOK, nil, ts()).Checks[0]
	with := eval(t, contract.VantageOK, nil, ts(contract.ReasonTSPeerOnline, contract.ReasonTSPathDirect)).Checks[0]
	if with.Severity != plain.Severity || with.Result != plain.Result {
		t.Fatalf("enrichment changed the verdict: %+v vs %+v", with, plain)
	}
	want := []contract.Reason{"tcp_connect_ok", "ts_path_direct", "ts_peer_online"}
	if !slices.Equal(with.Reasons, want) {
		t.Fatalf("reasons %v, want %v", with.Reasons, want)
	}
	// Held and expected-dark checks keep their verdicts too.
	down := obs("h", contract.KindTSReach, contract.ExpectedLight, contract.ResultUnknown, contract.ReasonTCPConnectTimeout)
	down.Enrichment = []contract.Reason{contract.ReasonTSPeerOffline}
	c := eval(t, contract.VantageOK, nil, down).Checks[0]
	if c.Severity != contract.SeverityYellow || !slices.Contains(c.Reasons, contract.ReasonProbeUnknown) || !slices.Contains(c.Reasons, contract.ReasonTSPeerOffline) {
		t.Fatalf("offline explains but does not judge: %+v", c)
	}
}

func TestBlindDropsEnrichment(t *testing.T) {
	c := eval(t, contract.VantageBlind, nil, ts(contract.ReasonTSPeerOnline)).Checks[0]
	if !slices.Equal(c.Reasons, []contract.Reason{contract.ReasonVantageBlind}) {
		t.Fatalf("blind reasons %v", c.Reasons)
	}
}

func TestEnrichmentRulesAreEnforced(t *testing.T) {
	other := obs("h", contract.KindNodeExporter, contract.ExpectedLight, contract.ResultUp, "")
	other.Enrichment = []contract.Reason{contract.ReasonTSPeerOnline}
	for name, o := range map[string]Observation{
		"not a ts_reach check":   other,
		"not an enrichment code": ts(contract.ReasonHeld),
		"two peer states":        ts(contract.ReasonTSPeerOnline, contract.ReasonTSPeerOffline),
		"two paths":              ts(contract.ReasonTSPeerOnline, contract.ReasonTSPathDirect, contract.ReasonTSPathRelayed),
		"path without a peer":    ts(contract.ReasonTSPathDirect),
		"path on an unmatched":   ts(contract.ReasonTSPeerUnmatched, contract.ReasonTSPathDirect),
	} {
		if _, err := Evaluate(&Input{Now: fixtureNow, VantageID: "lathe", VantageStatus: contract.VantageOK, Checks: []Observation{o}}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
