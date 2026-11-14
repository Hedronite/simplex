package perf

import (
	"fmt"
	"strings"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/severity"
)

// sizes are the three shapes every benchmark uses: a small mesh, a large one
// and one big enough that a quadratic term would show.
var sizes = []int{10, 100, 1000}

// hostsFor keeps ten checks per host, so host derivation and the pair rule are
// exercised at every size without making host count dominate.
func hostsFor(checks int) int {
	n := checks / 10
	if n < 1 {
		n = 1
	}
	return n
}

// slugs are host ids. They are generated, not copied from a real mesh.
func slug(i int) string { return fmt.Sprintf("host-%03d", i) }

// kinds cycles the kinds a run can carry, so the encoder's one-hots are not
// all the same shape.
var kinds = []contract.Kind{
	contract.KindTSReach,
	contract.KindPromUp,
	contract.KindKubeAPIServer,
	contract.KindNodeExporter,
	contract.KindLANReach,
	contract.KindSSH,
}

// targetFor gives each check instance a distinct target, which the contract
// requires within a host and the benchmark needs for stable sorting.
func targetFor(kind contract.Kind, i int) string {
	port := 9100 + i%1000
	switch kind {
	case contract.KindPromUp:
		return fmt.Sprintf(":%d%s", port, contract.PathReady)
	case contract.KindNodeExporter:
		return fmt.Sprintf(":%d%s", port, contract.PathMetrics)
	default:
		return fmt.Sprintf(":%d", port)
	}
}

// observation builds one synthetic probe result. The mix is deliberate: mostly
// up, some down, some unknown, with outcome and enrichment codes on a share of
// them, because the severity engine's pair rule and reason handling are where a
// quadratic term would hide.
func observation(checks, i int) severity.Observation {
	kind := kinds[i%len(kinds)]
	host := slug(i % hostsFor(checks))
	expected := contract.ExpectedLight
	if kind == contract.KindKubeAPIServer {
		expected = contract.ExpectedDark
	}
	obs := severity.Observation{
		Host:     host,
		Kind:     kind,
		Target:   targetFor(kind, i),
		Expected: expected,
		Result:   contract.ResultUp,
	}
	switch i % 7 {
	case 3:
		obs.Result = contract.ResultDown
		obs.Outcome = contract.ReasonHTTPUnexpectedStatus
	case 5:
		obs.Result = contract.ResultUnknown
		obs.Outcome = contract.ReasonTCPConnectTimeout
	}
	if i%11 == 0 {
		obs.Outcome = contract.ReasonTCPConnectOK
	}
	// Enrichment codes are only valid on ts_reach, and the engine enforces it.
	if i%13 == 0 && kind == contract.KindTSReach {
		obs.Enrichment = []contract.Reason{contract.ReasonTSPeerOnline, contract.ReasonTSPathDirect}
	}
	return obs
}

// input builds the engine's input for a mesh of the given size.
func input(checks int) *severity.Input {
	in := &severity.Input{
		Now:           time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC),
		VantageID:     "lab",
		VantageStatus: contract.VantageOK,
		Checks:        make([]severity.Observation, checks),
	}
	for i := range in.Checks {
		in.Checks[i] = observation(checks, i)
	}
	// One hold and one canary-shaped pair of ts_reach checks, so the hold path
	// and the pair rule both run at every size.
	in.Holds = []severity.Hold{{Host: slug(0), Kind: string(contract.KindTSReach), Reason: "maintenance", Until: in.Now.Add(time.Hour)}}
	return in
}

// doc builds the derived document for a mesh of the given size.
func doc(checks int) *document.Document {
	d, err := severity.Evaluate(input(checks))
	if err != nil {
		panic("perf: synthetic input failed to evaluate: " + err.Error())
	}
	return &d
}

// configYAML builds a valid config: one host per line, one check each, with the
// expected value the contract requires.
func configYAML(checks int) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: simplex.config/v0\n")
	b.WriteString("vantage:\n  id: lab\n")
	b.WriteString("hosts:\n")
	for i := 0; i < checks; i++ {
		fmt.Fprintf(&b, "  - id: %s\n    checks:\n      - {kind: ts_reach, expected: light}\n", slug(i))
	}
	return []byte(b.String())
}

// overlayYAML builds a valid overlay for the same hosts. The addresses are
// placeholder names, never real ones.
func overlayYAML(checks int) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: simplex.overlay/v0\n")
	b.WriteString("hosts:\n")
	for i := 0; i < checks; i++ {
		fmt.Fprintf(&b, "  %s: {address: %s.example}\n", slug(i), slug(i))
	}
	return []byte(b.String())
}
