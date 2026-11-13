package severity

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/exitcode"
)

var fixtureNow = time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)

func outcomeOf(t *testing.T, c document.Check) contract.Reason {
	t.Helper()
	var out contract.Reason
	for _, r := range c.Reasons {
		if isOutcome(r) {
			if out != "" {
				t.Fatalf("check %s %s carries two outcome codes", c.Host, c.Kind)
			}
			out = r
		}
	}
	return out
}

// inputFromDocument reads the observations back out of a golden document.
// Holds are rebuilt from `held` reasons; an expired hold needs no entry
// because it leaves no trace in the document (covered in TestHoldWindow).
func inputFromDocument(t *testing.T, d document.Document) Input {
	t.Helper()
	in := Input{Now: fixtureNow, VantageID: d.Vantage.ID, VantageStatus: d.Vantage.Status}
	for _, c := range d.Checks {
		in.Checks = append(in.Checks, Observation{
			Host: c.Host, Kind: c.Kind, Target: c.Target,
			Expected: c.Expected, Result: c.Result, Outcome: outcomeOf(t, c),
		})
		if slices.Contains(c.Reasons, contract.ReasonHeld) {
			in.Holds = append(in.Holds, Hold{
				Host: c.Host, Kind: string(c.Kind), Reason: "fixture",
				Until: fixtureNow.Add(time.Hour),
			})
		}
	}
	return in
}

// Every golden document is reproduced byte for byte, with its exit code.
func TestFixturesReproduceByteForByte(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	seen := 0
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "document.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		seen++
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var want document.Document
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			got, err := Evaluate(inputFromDocument(t, want))
			if err != nil {
				t.Fatal(err)
			}
			if enc := got.Encode(); !bytes.Equal(enc, raw) {
				t.Fatalf("document differs from fixture\n--- got\n%s--- want\n%s", enc, raw)
			}
			exit, err := os.ReadFile(filepath.Join(dir, "exit"))
			if err != nil {
				t.Fatal(err)
			}
			code, err := strconv.Atoi(strings.TrimSpace(string(exit)))
			if err != nil {
				t.Fatal(err)
			}
			if ExitCode(got) != code {
				t.Fatalf("exit code %d, fixture says %d", ExitCode(got), code)
			}
		})
	}
	if seen < 10 {
		t.Fatalf("only %d fixture documents found", seen)
	}
}

func obs(host string, kind contract.Kind, expected contract.Expected, result contract.Result, outcome contract.Reason) Observation {
	target := ":9100"
	switch kind {
	case contract.KindPromUp:
		target = ":9090/-/ready"
	case contract.KindNodeExporter:
		target = ":9100/metrics"
	case contract.KindKubeAPIServer:
		target = ":6443"
	}
	return Observation{Host: host, Kind: kind, Target: target, Expected: expected, Result: result, Outcome: outcome}
}

func eval(t *testing.T, status contract.VantageStatus, holds []Hold, checks ...Observation) document.Document {
	t.Helper()
	d, err := Evaluate(Input{Now: fixtureNow, VantageID: "lathe", VantageStatus: status, Holds: holds, Checks: checks})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRuleRows(t *testing.T) {
	light, dark := contract.ExpectedLight, contract.ExpectedDark
	up, down, unk := contract.ResultUp, contract.ResultDown, contract.ResultUnknown
	rows := []struct {
		name    string
		in      Observation
		sev     contract.Severity
		reasons []contract.Reason
	}{
		{"dark up", obs("h", contract.KindKubeAPIServer, dark, up, contract.ReasonTCPConnectOK), "YELLOW", []contract.Reason{"tcp_connect_ok", "unexpected_light"}},
		{"dark up 503", obs("h", contract.KindPromUp, dark, up, contract.ReasonPromNotReady), "YELLOW", []contract.Reason{"prom_not_ready", "unexpected_light"}},
		{"dark down", obs("h", contract.KindKubeAPIServer, dark, down, contract.ReasonTCPConnectRefused), "GREEN", []contract.Reason{"expected_dark", "tcp_connect_refused"}},
		{"dark unknown", obs("h", contract.KindKubeAPIServer, dark, unk, contract.ReasonTCPConnectTimeout), "GREEN", []contract.Reason{"expected_dark", "tcp_connect_timeout"}},
		{"light up", obs("h", contract.KindTSReach, light, up, contract.ReasonTCPConnectOK), "GREEN", []contract.Reason{"tcp_connect_ok"}},
		{"light up refused", obs("h", contract.KindTSReach, light, up, contract.ReasonTCPConnectRefused), "GREEN", []contract.Reason{"tcp_connect_refused"}},
		{"light up auth", obs("h", contract.KindPromUp, light, up, contract.ReasonPromAuth), "YELLOW", []contract.Reason{"prom_auth"}},
		{"light up body", obs("h", contract.KindPromUp, light, up, contract.ReasonMetricsBodyBad), "YELLOW", []contract.Reason{"metrics_body_unexpected"}},
		{"light up no outcome", obs("h", contract.KindNodeExporter, light, up, ""), "GREEN", []contract.Reason{}},
		{"light down", obs("h", contract.KindNodeExporter, light, down, contract.ReasonHTTPUnexpectedStatus), "YELLOW", []contract.Reason{"http_unexpected_status", "unexpected_dark"}},
		{"light unknown", obs("h", contract.KindTSReach, light, unk, contract.ReasonTCPConnectTimeout), "YELLOW", []contract.Reason{"probe_unknown", "tcp_connect_timeout"}},
		{"light unknown no outcome", obs("h", contract.KindTSReach, light, unk, ""), "YELLOW", []contract.Reason{"probe_unknown"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := eval(t, contract.VantageOK, nil, r.in).Checks[0]
			if c.Severity != r.sev || !slices.Equal(c.Reasons, r.reasons) {
				t.Fatalf("got %s %v, want %s %v", c.Severity, c.Reasons, r.sev, r.reasons)
			}
			if c.Result != r.in.Result {
				t.Fatalf("result changed to %s", c.Result)
			}
		})
	}
}

func TestUnknownNeverRedAlone(t *testing.T) {
	d := eval(t, contract.VantageOK, nil, obs("h", contract.KindNodeExporter, contract.ExpectedLight, contract.ResultUnknown, contract.ReasonHTTPTimeout))
	if d.Overall != contract.SeverityYellow || ExitCode(d) != exitcode.Yellow {
		t.Fatalf("overall %s exit %d", d.Overall, ExitCode(d))
	}
}

func TestBlindIsTerminal(t *testing.T) {
	held := []Hold{{Host: "*", Kind: "*", Reason: "x", Until: fixtureNow.Add(time.Hour)}}
	d := eval(t, contract.VantageBlind, held,
		obs("box", contract.KindPromUp, contract.ExpectedLight, contract.ResultUp, ""),
		obs("lathe", contract.KindPromUp, contract.ExpectedLight, contract.ResultDown, contract.ReasonHTTPUnexpectedStatus),
		obs("tower", contract.KindKubeAPIServer, contract.ExpectedDark, contract.ResultDown, contract.ReasonTCPConnectRefused))
	for _, c := range d.Checks {
		if c.Result != contract.ResultUnknown || c.Severity != contract.SeverityRed ||
			!slices.Equal(c.Reasons, []contract.Reason{contract.ReasonVantageBlind}) {
			t.Fatalf("blind check not terminal: %+v", c)
		}
	}
	for _, h := range d.Hosts {
		if h.Severity != contract.SeverityRed {
			t.Fatalf("blind host %s is %s", h.ID, h.Severity)
		}
	}
	if d.Overall != contract.SeverityRed || ExitCode(d) != exitcode.Blind {
		t.Fatalf("overall %s exit %d", d.Overall, ExitCode(d))
	}
}

func TestPairPass(t *testing.T) {
	light := contract.ExpectedLight
	p := func(host string, r contract.Result, o contract.Reason) Observation {
		return obs(host, contract.KindPromUp, light, r, o)
	}
	t.Run("both down is RED and the pair code is added", func(t *testing.T) {
		d := eval(t, contract.VantageOK, nil,
			p("box", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus),
			p("lathe", contract.ResultUnknown, contract.ReasonHTTPTimeout))
		for _, c := range d.Checks {
			if c.Severity != contract.SeverityRed || !slices.Contains(c.Reasons, contract.ReasonPromPairDown) {
				t.Fatalf("not RED pair: %+v", c)
			}
		}
		if !slices.Contains(d.Checks[0].Reasons, contract.ReasonUnexpectedDark) ||
			!slices.Contains(d.Checks[1].Reasons, contract.ReasonProbeUnknown) {
			t.Fatalf("underlying verdicts were replaced: %+v", d.Checks)
		}
		if d.Overall != contract.SeverityRed || ExitCode(d) != exitcode.Red {
			t.Fatalf("overall %s", d.Overall)
		}
	})
	t.Run("one down is YELLOW degraded", func(t *testing.T) {
		d := eval(t, contract.VantageOK, nil,
			p("box", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus),
			p("lathe", contract.ResultUp, ""))
		if d.Checks[0].Severity != contract.SeverityYellow || !slices.Contains(d.Checks[0].Reasons, contract.ReasonPromPairDegrade) ||
			d.Checks[1].Severity != contract.SeverityGreen {
			t.Fatalf("%+v", d.Checks)
		}
	})
	t.Run("a single prom check never fires the pair", func(t *testing.T) {
		d := eval(t, contract.VantageOK, nil, p("box", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus))
		if d.Checks[0].Severity != contract.SeverityYellow || slices.Contains(d.Checks[0].Reasons, contract.ReasonPromPairDown) ||
			slices.Contains(d.Checks[0].Reasons, contract.ReasonPromPairDegrade) {
			t.Fatalf("%+v", d.Checks[0])
		}
	})
	t.Run("a held member leaves the pair, so the survivor is YELLOW not RED", func(t *testing.T) {
		held := []Hold{{Host: "box", Kind: "prom_up", Reason: "x", Until: fixtureNow.Add(time.Hour)}}
		d := eval(t, contract.VantageOK, held,
			p("box", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus),
			p("lathe", contract.ResultDown, contract.ReasonHTTPUnexpectedStatus))
		if d.Checks[0].Severity != contract.SeverityGreen || d.Checks[1].Severity != contract.SeverityYellow ||
			slices.Contains(d.Checks[1].Reasons, contract.ReasonPromPairDown) {
			t.Fatalf("%+v", d.Checks)
		}
	})
	t.Run("two up with a prom_not_ready stays plain YELLOW", func(t *testing.T) {
		d := eval(t, contract.VantageOK, nil,
			p("box", contract.ResultUp, contract.ReasonPromNotReady), p("lathe", contract.ResultUp, ""))
		if d.Checks[0].Severity != contract.SeverityYellow || slices.Contains(d.Checks[0].Reasons, contract.ReasonPromPairDegrade) {
			t.Fatalf("%+v", d.Checks[0])
		}
	})
}

func TestHoldWindow(t *testing.T) {
	o := obs("tower", contract.KindTSReach, contract.ExpectedLight, contract.ResultDown, contract.ReasonTCPConnectTimeout)
	for _, tc := range []struct {
		name  string
		hold  Hold
		heldN bool
	}{
		{"future", Hold{Host: "tower", Kind: "ts_reach", Until: fixtureNow.Add(time.Second)}, true},
		{"until equals now is expired", Hold{Host: "tower", Kind: "ts_reach", Until: fixtureNow}, false},
		{"past", Hold{Host: "tower", Kind: "ts_reach", Until: fixtureNow.Add(-time.Hour)}, false},
		{"host wildcard", Hold{Host: "*", Kind: "ts_reach", Until: fixtureNow.Add(time.Hour)}, true},
		{"kind wildcard", Hold{Host: "tower", Kind: "*", Until: fixtureNow.Add(time.Hour)}, true},
		{"other host", Hold{Host: "box", Kind: "ts_reach", Until: fixtureNow.Add(time.Hour)}, false},
		{"other kind", Hold{Host: "tower", Kind: "prom_up", Until: fixtureNow.Add(time.Hour)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := eval(t, contract.VantageOK, []Hold{tc.hold}, o).Checks[0]
			if got := slices.Contains(c.Reasons, contract.ReasonHeld); got != tc.heldN {
				t.Fatalf("held=%v want %v: %+v", got, tc.heldN, c)
			}
			if tc.heldN && c.Severity != contract.SeverityGreen {
				t.Fatalf("held check is %s", c.Severity)
			}
			if !tc.heldN && c.Severity != contract.SeverityYellow {
				t.Fatalf("unheld check is %s", c.Severity)
			}
			if c.Result != contract.ResultDown {
				t.Fatalf("held changed the result to %s", c.Result)
			}
		})
	}
}

func TestInvalidInput(t *testing.T) {
	good := obs("h", contract.KindTSReach, contract.ExpectedLight, contract.ResultUp, "")
	for name, in := range map[string]Input{
		"no checks":       {Now: fixtureNow, VantageStatus: contract.VantageOK},
		"bad vantage":     {Now: fixtureNow, VantageStatus: "weird", Checks: []Observation{good}},
		"duplicate":       {Now: fixtureNow, VantageStatus: contract.VantageOK, Checks: []Observation{good, good}},
		"bad kind":        {Now: fixtureNow, VantageStatus: contract.VantageOK, Checks: []Observation{{Host: "h", Kind: "x", Target: ":1", Expected: "light", Result: "up"}}},
		"verdict outcome": {Now: fixtureNow, VantageStatus: contract.VantageOK, Checks: []Observation{{Host: "h", Kind: "ts_reach", Target: ":1", Expected: "light", Result: "up", Outcome: "held"}}},
	} {
		if _, err := Evaluate(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Invariants over random inputs: derivation, ordering, and the rule that
// held and expected-dark checks never raise a rollup.
func TestInvariantsOverRandomInputs(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	kinds := contract.Kinds
	outcomes := []contract.Reason{"", contract.ReasonTCPConnectOK, contract.ReasonTCPConnectRefused, contract.ReasonTCPConnectTimeout,
		contract.ReasonHTTPTimeout, contract.ReasonHTTPUnexpectedStatus, contract.ReasonPromNotReady, contract.ReasonPromAuth, contract.ReasonMetricsBodyBad}
	for n := 0; n < 500; n++ {
		var in Input
		in.Now = fixtureNow
		in.VantageID = "lathe"
		in.VantageStatus = []contract.VantageStatus{"ok", "ok", "ok", "unchecked", "blind"}[rng.Intn(5)]
		hosts := []string{"apiary", "box", "castle", "lathe", "tower"}
		seen := map[string]bool{}
		for i := 0; i < 1+rng.Intn(10); i++ {
			o := Observation{
				Host:     hosts[rng.Intn(len(hosts))],
				Kind:     kinds[rng.Intn(len(kinds))],
				Expected: contract.ExpectedValues[rng.Intn(2)],
				Result:   contract.Results[rng.Intn(3)],
				Outcome:  outcomes[rng.Intn(len(outcomes))],
			}
			o.Target = ":" + strconv.Itoa(9000+rng.Intn(3))
			key := o.Host + string(o.Kind) + o.Target
			if seen[key] {
				continue
			}
			seen[key] = true
			in.Checks = append(in.Checks, o)
		}
		for i := 0; i < rng.Intn(3); i++ {
			in.Holds = append(in.Holds, Hold{Host: hosts[rng.Intn(len(hosts))], Kind: string(kinds[rng.Intn(len(kinds))]),
				Until: fixtureNow.Add(time.Duration(rng.Intn(3)-1) * time.Hour)})
		}
		d, err := Evaluate(in)
		if err != nil {
			t.Fatal(err)
		}
		worst := contract.SeverityGreen
		for i, h := range d.Hosts {
			worst = worse(worst, h.Severity)
			if i > 0 && d.Hosts[i-1].ID >= h.ID {
				t.Fatalf("hosts not sorted: %+v", d.Hosts)
			}
			hw := contract.SeverityGreen
			for _, c := range d.Checks {
				if c.Host == h.ID {
					hw = worse(hw, c.Severity)
				}
			}
			if hw != h.Severity {
				t.Fatalf("host %s severity %s, worst check %s", h.ID, h.Severity, hw)
			}
		}
		if worst != d.Overall {
			t.Fatalf("overall %s, worst host %s", d.Overall, worst)
		}
		if ExitCode(d) == exitcode.Blind != (d.Vantage.Status == contract.VantageBlind) {
			t.Fatalf("exit/blind mismatch")
		}
		for i, c := range d.Checks {
			if i > 0 {
				p := d.Checks[i-1]
				if p.Host+"\x00"+string(p.Kind)+"\x00"+p.Target >= c.Host+"\x00"+string(c.Kind)+"\x00"+c.Target {
					t.Fatalf("checks not sorted")
				}
			}
			if !slices.IsSorted(c.Reasons) {
				t.Fatalf("reasons not sorted: %v", c.Reasons)
			}
			if d.Vantage.Status != contract.VantageBlind &&
				(slices.Contains(c.Reasons, contract.ReasonHeld) || slices.Contains(c.Reasons, contract.ReasonExpectedDark)) &&
				c.Severity != contract.SeverityGreen {
				t.Fatalf("held or expected-dark check raised a rollup: %+v", c)
			}
			if c.Severity == contract.SeverityRed && d.Vantage.Status != contract.VantageBlind &&
				!slices.Contains(c.Reasons, contract.ReasonPromPairDown) {
				t.Fatalf("RED without a pair verdict outside blind: %+v", c)
			}
		}
	}
}
