// Package document holds the simplex.probe/v0 document and its one
// deterministic encoding. The encoding is hand-rolled so the byte layout
// of golden fixtures is fixed: no map iteration, no reflection.
package document

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Hedronite/simplex/internal/contract"
)

// Document is one probe run from one vantage.
type Document struct {
	APIVersion  string            `json:"apiVersion"`
	GeneratedAt string            `json:"generated_at"`
	Vantage     Vantage           `json:"vantage"`
	Overall     contract.Severity `json:"overall"`
	Hosts       []Host            `json:"hosts"`
	Checks      []Check           `json:"checks"`
}

// Vantage names where the run was made and whether it could be trusted.
type Vantage struct {
	ID     string                 `json:"id"`
	Status contract.VantageStatus `json:"status"`
}

// Host is the derived verdict for one host.
type Host struct {
	ID       string            `json:"id"`
	Severity contract.Severity `json:"severity"`
	Reasons  []contract.Reason `json:"reasons"`
}

// Check is one probed check instance.
type Check struct {
	Host     string            `json:"host"`
	Kind     contract.Kind     `json:"kind"`
	Target   string            `json:"target"`
	Result   contract.Result   `json:"result"`
	Severity contract.Severity `json:"severity"`
	Expected contract.Expected `json:"expected"`
	Reasons  []contract.Reason `json:"reasons"`
}

// Encode returns the document in its canonical byte layout: two-space
// indent, one line per host, three lines per check, trailing newline.
func (d Document) Encode() []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	b.WriteString("  \"apiVersion\": " + q(d.APIVersion) + ",\n")
	b.WriteString("  \"generated_at\": " + q(d.GeneratedAt) + ",\n")
	b.WriteString("  \"vantage\": {\"id\": " + q(d.Vantage.ID) + ", \"status\": " + q(string(d.Vantage.Status)) + "},\n")
	b.WriteString("  \"overall\": " + q(string(d.Overall)) + ",\n")
	b.WriteString("  \"hosts\": [\n")
	for i, h := range d.Hosts {
		b.WriteString("    {\"id\": " + q(h.ID) + ", \"severity\": " + q(string(h.Severity)) + ", \"reasons\": " + list(h.Reasons) + "}")
		b.WriteString(sep(i, len(d.Hosts)))
	}
	b.WriteString("  ],\n")
	b.WriteString("  \"checks\": [\n")
	for i, c := range d.Checks {
		b.WriteString("    {\"host\": " + q(c.Host) + ", \"kind\": " + q(string(c.Kind)) +
			", \"target\": " + q(c.Target) + ", \"result\": " + q(string(c.Result)) + ",\n")
		b.WriteString("     \"severity\": " + q(string(c.Severity)) + ", \"expected\": " + q(string(c.Expected)) + ",\n")
		b.WriteString("     \"reasons\": " + list(c.Reasons) + "}")
		b.WriteString(sep(i, len(d.Checks)))
	}
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	return b.Bytes()
}

func sep(i, n int) string {
	if i < n-1 {
		return ",\n"
	}
	return "\n"
}

// q quotes a string as JSON without HTML escaping.
func q(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// A Go string always encodes; this cannot happen.
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func list(rs []contract.Reason) string {
	if len(rs) == 0 {
		return "[]"
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = q(string(r))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
