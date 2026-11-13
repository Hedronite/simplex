// Package metrics renders one probe snapshot as a Prometheus text exposition
// (format 0.0.4).
//
// The writer is hand-rolled for three reasons: the binary must not link a
// Prometheus client, the byte layout has to be stable enough to pin with
// goldens, and every label value has to come from a known place. That last one
// is the point of the package: a label value is either a document field the
// contract already constrains (host slug, check kind, target, enum value) or it
// is not written at all. Nothing here formats an address, an overlay value or
// an error string.
package metrics

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
)

// Snapshot is one completed run as the encoder sees it. Doc is nil before the
// first run completes, and then only the process series are written.
type Snapshot struct {
	Doc         *document.Document
	RunEnded    time.Time
	RunDuration time.Duration
	RunErrors   uint64
	Version     string
	Commit      string
}

// severityValue maps a severity onto the documented scale.
func severityValue(s contract.Severity) (float64, bool) {
	switch s {
	case contract.SeverityGreen:
		return 0, true
	case contract.SeverityYellow:
		return 1, true
	case contract.SeverityRed:
		return 2, true
	}
	return 0, false
}

// Write renders the exposition for one snapshot. now is the scrape clock, used
// only for the document age, so a caller can pin it.
func Write(w io.Writer, s *Snapshot, now time.Time) error {
	if s == nil {
		s = &Snapshot{}
	}
	e := &encoder{w: w, snapshot: s}

	e.family("simplex_build_info", "gauge", "Build information for this binary.")
	e.sample("simplex_build_info", labels{{"version", s.Version}, {"commit", s.Commit}}, 1)

	if s.Doc != nil {
		e.document(now)
	}

	e.family("simplex_run_errors_total", "counter", "Runs that produced no document.")
	e.sample("simplex_run_errors_total", nil, float64(s.RunErrors))

	return e.err
}

// document writes every series derived from a completed run.
func (e *encoder) document(now time.Time) {
	doc := e.snapshot.Doc
	e.family("simplex_overall_severity", "gauge", "Worst severity of the last completed run: 0 GREEN, 1 YELLOW, 2 RED.")
	if v, ok := severityValue(doc.Overall); ok {
		e.sample("simplex_overall_severity", nil, v)
	}

	e.family("simplex_vantage_status", "gauge", "One-hot vantage status of the last completed run.")
	for _, status := range contract.VantageStatuses {
		v := 0.0
		if doc.Vantage.Status == status {
			v = 1
		}
		e.sample("simplex_vantage_status", labels{{"status", string(status)}}, v)
	}

	e.family("simplex_host_severity", "gauge", "Severity of one host: 0 GREEN, 1 YELLOW, 2 RED.")
	for _, h := range doc.Hosts {
		if v, ok := severityValue(h.Severity); ok {
			e.sample("simplex_host_severity", labels{{"host", h.ID}}, v)
		}
	}

	e.family("simplex_check_severity", "gauge", "Severity of one check instance: 0 GREEN, 1 YELLOW, 2 RED.")
	for _, c := range doc.Checks {
		if v, ok := severityValue(c.Severity); ok {
			e.sample("simplex_check_severity", checkLabels(&c), v)
		}
	}

	e.family("simplex_check_result", "gauge", "One-hot probe result of one check instance.")
	for _, c := range doc.Checks {
		for _, result := range contract.Results {
			v := 0.0
			if c.Result == result {
				v = 1
			}
			e.sample("simplex_check_result", append(checkLabels(&c), label{"result", string(result)}), v)
		}
	}

	e.family("simplex_check_reason", "gauge", "One series per frozen reason code, 1 when the check carries it.")
	for _, c := range doc.Checks {
		// Driven by the frozen enum, not by the document, so the label space
		// cannot grow past the sixteen codes even if a document carried one
		// the contract does not know. Every code gets a series, so a check
		// with no reason is a block of zeros rather than a gap: a query can
		// tell "not carried" from "not exported".
		for _, reason := range contract.ReasonCodes() {
			v := 0.0
			if hasReason(c.Reasons, reason) {
				v = 1
			}
			e.sample("simplex_check_reason", append(checkLabels(&c), label{"reason", string(reason)}), v)
		}
	}

	e.family("simplex_last_run_timestamp_seconds", "gauge", "End of the last completed run, in Unix seconds.")
	if !e.snapshot.RunEnded.IsZero() {
		e.sample("simplex_last_run_timestamp_seconds", nil, unixSeconds(e.snapshot.RunEnded))
	}

	e.family("simplex_document_age_seconds", "gauge", "Age of the served document, computed at scrape.")
	if age, ok := documentAge(doc, now); ok {
		e.sample("simplex_document_age_seconds", nil, age)
	}

	e.family("simplex_run_duration_seconds", "gauge", "Duration of the last completed run, in seconds.")
	e.sample("simplex_run_duration_seconds", nil, e.snapshot.RunDuration.Seconds())
}

// documentAge is now minus the document's generation time, clamped at zero so a
// clock that steps backwards cannot report a negative age.
func documentAge(doc *document.Document, now time.Time) (float64, bool) {
	generated, err := time.Parse(time.RFC3339, doc.GeneratedAt)
	if err != nil {
		return 0, false
	}
	age := now.Sub(generated).Seconds()
	if age < 0 {
		age = 0
	}
	return age, true
}

// unixSeconds keeps sub-second precision without losing the integer part to a
// float64 round trip.
func unixSeconds(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}

func hasReason(reasons []contract.Reason, want contract.Reason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

type label struct{ name, value string }

type labels []label

// checkLabels is the label set of a check instance. The target is the
// document form (port plus path) and never an address.
func checkLabels(c *document.Check) labels {
	return labels{
		{"host", c.Host},
		{"kind", string(c.Kind)},
		{"target", c.Target},
	}
}

// encoder accumulates the first write error so the render path stays flat.
type encoder struct {
	w        io.Writer
	snapshot *Snapshot
	err      error
}

func (e *encoder) write(s string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, s)
}

// family writes the HELP and TYPE lines for one metric family.
func (e *encoder) family(name, kind, help string) {
	e.write("# HELP " + name + " " + escapeHelp(help) + "\n")
	e.write("# TYPE " + name + " " + kind + "\n")
}

// sample writes one sample line.
func (e *encoder) sample(name string, ls labels, value float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(ls) > 0 {
		b.WriteByte('{')
		for i, l := range ls {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.name)
			b.WriteString(`="`)
			b.WriteString(escapeLabel(l.value))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	b.WriteByte('\n')
	e.write(b.String())
}

// escapeLabel escapes a label value per the exposition format: backslash,
// double quote and newline are the only special characters.
func escapeLabel(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 4)
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes a HELP line, where only backslash and newline are special.
func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// String is here so a caller can render into a string in tests without
// reaching for a buffer.
func String(s *Snapshot, now time.Time) (string, error) {
	var b strings.Builder
	if err := Write(&b, s, now); err != nil {
		return "", fmt.Errorf("render exposition: %w", err)
	}
	return b.String(), nil
}
