// Package severity is the pure severity engine: observations, holds, the
// vantage status and a clock go in; a derived document comes out. It does
// no I/O. The rules are docs/semantics.md sections 5 and 6.
package severity

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/exitcode"
)

// ErrNoChecks is returned when there is nothing to evaluate. Config
// guarantees at least one check, so this is a programming error upstream.
var ErrNoChecks = errors.New("no checks to evaluate")

// Hold is an announced, temporary dark. Host and Kind may be contract.Wildcard.
type Hold struct {
	Host, Kind, Reason string
	Until              time.Time
}

// Observation is what one probe saw. Outcome is empty when the contract
// says the check carries no outcome code.
type Observation struct {
	Host     string
	Kind     contract.Kind
	Target   string
	Expected contract.Expected
	Result   contract.Result
	Outcome  contract.Reason
	// Enrichment holds the informational codes from the local tailnet daemon
	// (docs/semantics.md section 15). They ride along on the check's reasons
	// and never change a result or a severity.
	Enrichment []contract.Reason
}

// Input is everything the engine needs. Now is the single run clock.
type Input struct {
	Now           time.Time
	VantageID     string
	VantageStatus contract.VantageStatus
	Checks        []Observation
	Holds         []Hold
}

var promOutcomes = map[contract.Reason]bool{
	contract.ReasonPromNotReady:   true,
	contract.ReasonPromAuth:       true,
	contract.ReasonMetricsBodyBad: true,
}

// Evaluate derives the document. Checks and hosts come out sorted by
// (host, kind, target) and by host id, bytewise.
func Evaluate(in *Input) (document.Document, error) {
	if len(in.Checks) == 0 {
		return document.Document{}, ErrNoChecks
	}
	if !slices.Contains(contract.VantageStatuses, in.VantageStatus) {
		return document.Document{}, fmt.Errorf("vantage status %q is not valid", in.VantageStatus)
	}
	obs := slices.Clone(in.Checks)
	sort.SliceStable(obs, func(i, j int) bool { return less(&obs[i], &obs[j]) })
	for i := range obs {
		o := &obs[i]
		if err := validate(o); err != nil {
			return document.Document{}, err
		}
		if i > 0 && !less(&obs[i-1], o) {
			return document.Document{}, fmt.Errorf("duplicate check %s %s %s", o.Host, o.Kind, o.Target)
		}
	}

	blind := in.VantageStatus == contract.VantageBlind
	checks := make([]document.Check, len(obs))
	held := make([]bool, len(obs))
	for i := range obs {
		o := &obs[i]
		c := document.Check{
			Host: o.Host, Kind: o.Kind, Target: o.Target,
			Result: o.Result, Expected: o.Expected,
		}
		switch {
		case blind:
			c.Result = contract.ResultUnknown
			c.Severity = contract.SeverityRed
			c.Reasons = []contract.Reason{contract.ReasonVantageBlind}
		default:
			if o.Outcome != "" {
				c.Reasons = append(c.Reasons, o.Outcome)
			}
			c.Reasons = append(c.Reasons, o.Enrichment...)
			switch {
			case holdMatches(in.Holds, o, in.Now):
				held[i] = true
				c.Severity = contract.SeverityGreen
				c.Reasons = append(c.Reasons, contract.ReasonHeld)
			case o.Expected == contract.ExpectedDark:
				if o.Result == contract.ResultUp {
					c.Severity = contract.SeverityYellow
					c.Reasons = append(c.Reasons, contract.ReasonUnexpectedLight)
				} else {
					c.Severity = contract.SeverityGreen
					c.Reasons = append(c.Reasons, contract.ReasonExpectedDark)
				}
			default:
				switch o.Result {
				case contract.ResultUp:
					c.Severity = contract.SeverityGreen
					if promOutcomes[o.Outcome] {
						c.Severity = contract.SeverityYellow
					}
				case contract.ResultDown:
					c.Severity = contract.SeverityYellow
					c.Reasons = append(c.Reasons, contract.ReasonUnexpectedDark)
				default:
					c.Severity = contract.SeverityYellow
					c.Reasons = append(c.Reasons, contract.ReasonProbeUnknown)
				}
			}
		}
		checks[i] = c
	}

	if !blind {
		pairPass(checks, held)
	}
	for i := range checks {
		checks[i].Reasons = sortedUnique(checks[i].Reasons)
	}

	hosts := deriveHosts(checks)
	overall := contract.SeverityGreen
	for _, h := range hosts {
		overall = worse(overall, h.Severity)
	}
	return document.Document{
		APIVersion:  contract.ProbeAPIVersion,
		GeneratedAt: in.Now.UTC().Truncate(time.Second).Format(time.RFC3339),
		Vantage:     document.Vantage{ID: in.VantageID, Status: in.VantageStatus},
		Overall:     overall,
		Hosts:       hosts,
		Checks:      checks,
	}, nil
}

// ExitCode maps a document to its process exit code (semantics section 10).
func ExitCode(d *document.Document) int {
	if d.Vantage.Status == contract.VantageBlind {
		return exitcode.Blind
	}
	switch d.Overall {
	case contract.SeverityRed:
		return exitcode.Red
	case contract.SeverityYellow:
		return exitcode.Yellow
	default:
		return exitcode.Green
	}
}

// pairPass applies section 5 rule 5 over non-held, expected-light prom_up checks.
func pairPass(checks []document.Check, held []bool) {
	var pair []int
	for i, c := range checks {
		if c.Kind == contract.KindPromUp && c.Expected == contract.ExpectedLight && !held[i] {
			pair = append(pair, i)
		}
	}
	if len(pair) < 2 {
		return
	}
	notUp := 0
	for _, i := range pair {
		if checks[i].Result != contract.ResultUp {
			notUp++
		}
	}
	switch {
	case notUp == 0:
	case notUp == len(pair):
		for _, i := range pair {
			checks[i].Severity = contract.SeverityRed
			checks[i].Reasons = append(checks[i].Reasons, contract.ReasonPromPairDown)
		}
	default:
		for _, i := range pair {
			if checks[i].Result != contract.ResultUp {
				checks[i].Severity = worse(checks[i].Severity, contract.SeverityYellow)
				checks[i].Reasons = append(checks[i].Reasons, contract.ReasonPromPairDegrade)
			}
		}
	}
}

// deriveHosts groups sorted checks by host. A host's severity is its worst
// check; its reasons are the union of the worst-severity checks' reasons in
// check order, first occurrence kept (semantics section 6).
func deriveHosts(checks []document.Check) []document.Host {
	var hosts []document.Host
	for start := 0; start < len(checks); {
		end := start
		worst := contract.SeverityGreen
		for end < len(checks) && checks[end].Host == checks[start].Host {
			worst = worse(worst, checks[end].Severity)
			end++
		}
		reasons := []contract.Reason{}
		for _, c := range checks[start:end] {
			if c.Severity != worst {
				continue
			}
			for _, r := range c.Reasons {
				if !slices.Contains(reasons, r) {
					reasons = append(reasons, r)
				}
			}
		}
		hosts = append(hosts, document.Host{ID: checks[start].Host, Severity: worst, Reasons: reasons})
		start = end
	}
	return hosts
}

func holdMatches(holds []Hold, o *Observation, now time.Time) bool {
	for _, h := range holds {
		if (h.Host == contract.Wildcard || h.Host == o.Host) &&
			(h.Kind == contract.Wildcard || h.Kind == string(o.Kind)) &&
			h.Until.After(now) {
			return true
		}
	}
	return false
}

func validate(o *Observation) error {
	switch {
	case o.Host == "" || o.Target == "":
		return fmt.Errorf("check %s %s: host and target are required", o.Host, o.Kind)
	case !slices.Contains(contract.Kinds, o.Kind):
		return fmt.Errorf("check %s: unknown kind %q", o.Host, o.Kind)
	case !slices.Contains(contract.Results, o.Result):
		return fmt.Errorf("check %s %s: unknown result %q", o.Host, o.Kind, o.Result)
	case !slices.Contains(contract.ExpectedValues, o.Expected):
		return fmt.Errorf("check %s %s: unknown expected %q", o.Host, o.Kind, o.Expected)
	case o.Outcome != "" && !isOutcome(o.Outcome):
		return fmt.Errorf("check %s %s: %q is not an outcome code", o.Host, o.Kind, o.Outcome)
	}
	return validateEnrichment(o)
}

// validateEnrichment enforces the section 15 rules on the codes a caller passed:
// only enrichment codes, only on ts_reach, at most one per group, and a path
// code only beside a matched peer-state code.
func validateEnrichment(o *Observation) error {
	if len(o.Enrichment) == 0 {
		return nil
	}
	if o.Kind != contract.KindTSReach {
		return fmt.Errorf("check %s %s: enrichment codes are only valid on ts_reach", o.Host, o.Kind)
	}
	var peer, path []contract.Reason
	for _, r := range o.Enrichment {
		switch {
		case slices.Contains(contract.EnrichmentPeerState, r):
			peer = append(peer, r)
		case slices.Contains(contract.EnrichmentPath, r):
			path = append(path, r)
		default:
			return fmt.Errorf("check %s %s: %q is not an enrichment code", o.Host, o.Kind, r)
		}
	}
	switch {
	case len(peer) > 1 || len(path) > 1:
		return fmt.Errorf("check %s %s: more than one enrichment code in a group", o.Host, o.Kind)
	case len(path) == 1 && (len(peer) == 0 || peer[0] == contract.ReasonTSPeerUnmatched):
		return fmt.Errorf("check %s %s: a path code needs a matched peer-state code", o.Host, o.Kind)
	}
	return nil
}

func isOutcome(r contract.Reason) bool {
	for _, s := range contract.ReasonSpecs {
		if s.Code == r {
			return s.Class == contract.ClassOutcome
		}
	}
	return false
}

func less(a, b *Observation) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Target < b.Target
}

func rank(s contract.Severity) int {
	for i, v := range contract.Severities {
		if v == s {
			return i
		}
	}
	return -1
}

func worse(a, b contract.Severity) contract.Severity {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func sortedUnique(rs []contract.Reason) []contract.Reason {
	out := []contract.Reason{}
	for _, r := range rs {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}
