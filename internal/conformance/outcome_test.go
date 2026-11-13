package conformance

import (
	"fmt"
	"slices"
	"testing"

	"github.com/Hedronite/simplex/internal/contract"
)

// outcomeRule says which result an outcome code implies for which kinds
// (docs/semantics.md section 3). A kind missing from a rule may not carry
// that outcome code at all.
type outcomeRule struct {
	kinds  []contract.Kind
	result contract.Result
}

var reachKinds = []contract.Kind{contract.KindTSReach, contract.KindLANReach}

// outcomeRules is keyed by outcome code. tcp_connect_refused appears twice
// because a refusal means different things: the reach kinds answered, so
// the path is up; for every other kind a refusal is a definitive negative.
var outcomeRules = map[contract.Reason][]outcomeRule{
	contract.ReasonTCPConnectOK: {
		{[]contract.Kind{contract.KindTSReach, contract.KindLANReach, contract.KindKubeAPIServer, contract.KindSSH}, contract.ResultUp},
	},
	contract.ReasonTCPConnectRefused: {
		{reachKinds, contract.ResultUp},
		{[]contract.Kind{contract.KindKubeAPIServer, contract.KindNodeExporter, contract.KindPromUp, contract.KindSSH}, contract.ResultDown},
	},
	contract.ReasonTCPConnectTimeout: {
		{contract.Kinds, contract.ResultUnknown},
	},
	contract.ReasonHTTPTimeout: {
		{[]contract.Kind{contract.KindNodeExporter, contract.KindPromUp}, contract.ResultUnknown},
	},
	contract.ReasonHTTPUnexpectedStatus: {
		{[]contract.Kind{contract.KindNodeExporter, contract.KindPromUp}, contract.ResultDown},
	},
	contract.ReasonPromNotReady:   {{[]contract.Kind{contract.KindPromUp}, contract.ResultUp}},
	contract.ReasonPromAuth:       {{[]contract.Kind{contract.KindPromUp}, contract.ResultUp}},
	contract.ReasonMetricsBodyBad: {{[]contract.Kind{contract.KindPromUp}, contract.ResultUp}},
}

// outcomeAgreement reports whether a check's result agrees with its kind
// and its outcome code. Two invariants sit under it: the reach kinds never
// report down, and a blind check is always unknown.
func outcomeAgreement(c check) error {
	kind := contract.Kind(c.Kind)
	if (kind == contract.KindTSReach || kind == contract.KindLANReach) && c.Result == string(contract.ResultDown) {
		return fmt.Errorf("%s %s: %s never has result down", c.Host, c.Kind, c.Kind)
	}
	if hasReason(c.Reasons, contract.ReasonVantageBlind) {
		if c.Result != string(contract.ResultUnknown) {
			return fmt.Errorf("%s %s: blind check has result %s", c.Host, c.Kind, c.Result)
		}
		return nil
	}
	for _, code := range outcomeReasons(c.Reasons) {
		rules := outcomeRules[contract.Reason(code)]
		matched := false
		for _, r := range rules {
			if slices.Contains(r.kinds, kind) {
				matched = true
				if c.Result != string(r.result) {
					return fmt.Errorf("%s %s: outcome %s needs result %s, have %s", c.Host, c.Kind, code, r.result, c.Result)
				}
			}
		}
		if !matched {
			return fmt.Errorf("%s %s: outcome %s is not valid for this kind", c.Host, c.Kind, code)
		}
	}
	return nil
}

func TestOutcomeRulesCoverEveryOutcomeCode(t *testing.T) {
	for _, s := range contract.ReasonSpecs {
		if s.Class == contract.ClassOutcome {
			if _, ok := outcomeRules[s.Code]; !ok {
				t.Errorf("outcome code %s has no kind and result rule", s.Code)
			}
		}
	}
}

func TestOutcomeAgreementTable(t *testing.T) {
	ok := func(kind, result string, reasons ...string) check {
		return check{Host: "h", Kind: kind, Result: result, Reasons: reasons}
	}
	good := []check{
		ok("ts_reach", "up", "tcp_connect_ok"),
		ok("ts_reach", "up", "tcp_connect_refused"),
		ok("ts_reach", "unknown", "probe_unknown", "tcp_connect_timeout"),
		ok("lan_reach", "up", "tcp_connect_refused"),
		ok("kube_apiserver", "down", "expected_dark", "tcp_connect_refused"),
		ok("kube_apiserver", "up", "tcp_connect_ok", "unexpected_light"),
		ok("node_exporter", "down", "tcp_connect_refused", "unexpected_dark"),
		ok("node_exporter", "down", "http_unexpected_status", "unexpected_dark"),
		ok("node_exporter", "unknown", "http_timeout", "probe_unknown"),
		ok("node_exporter", "up"),
		ok("prom_up", "up", "prom_not_ready"),
		ok("prom_up", "up", "prom_auth"),
		ok("prom_up", "up", "metrics_body_unexpected"),
		ok("prom_up", "down", "http_unexpected_status", "prom_pair_degraded", "unexpected_dark"),
		ok("prom_up", "unknown", "vantage_blind"),
		ok("ts_reach", "unknown", "vantage_blind"),
	}
	for _, c := range good {
		if err := outcomeAgreement(c); err != nil {
			t.Errorf("%v should agree: %v", c, err)
		}
	}
	bad := []check{
		ok("ts_reach", "down", "tcp_connect_refused"),
		ok("ts_reach", "down"),
		ok("lan_reach", "down", "tcp_connect_timeout"),
		ok("ts_reach", "down", "held", "tcp_connect_refused"),
		ok("ts_reach", "unknown", "tcp_connect_ok"),
		ok("ts_reach", "up", "tcp_connect_timeout"),
		ok("kube_apiserver", "up", "tcp_connect_refused"),
		ok("node_exporter", "up", "tcp_connect_refused"),
		ok("node_exporter", "up", "http_unexpected_status"),
		ok("node_exporter", "down", "tcp_connect_ok"),
		ok("node_exporter", "up", "prom_not_ready"),
		ok("prom_up", "down", "prom_auth"),
		ok("prom_up", "up", "http_timeout"),
		ok("prom_up", "down", "vantage_blind"),
	}
	for _, c := range bad {
		if err := outcomeAgreement(c); err == nil {
			t.Errorf("%v should be rejected", c)
		}
	}
}
