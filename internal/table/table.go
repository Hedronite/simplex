// Package table renders the simplex.probe/v0 document as the frozen
// human table (docs/semantics.md section 12).
//
// The renderer is a pure function of the document: the document's checks
// are already sorted by (host, kind, target), so the renderer preserves
// their order and no map iteration reaches the output. Column widths come
// from the content, so the same document always renders to the same
// bytes, and diffs of table output stay reviewable.
package table

import (
	"fmt"
	"strings"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
)

// reasonWidth caps the REASONS column. Longer reason lists are cut at a
// complete code boundary and finished with "+N".
const reasonWidth = 48

// gutter separates columns. There is no trailing gutter after the last
// column.
const gutter = "  "

// Header names in the frozen column order: HOST | CHECK | EXPECTED |
// RESULT | SEVERITY | REASONS.
var headers = [...]string{"HOST", "CHECK", "EXPECTED", "RESULT", "SEVERITY", "REASONS"}

// ANSI color for the severity word only. The word is always printed, so
// a colorless reader loses nothing but emphasis.
const (
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorRed    = "\x1b[31m"
	colorReset  = "\x1b[0m"
)

func severityColor(s contract.Severity) string {
	switch s {
	case contract.SeverityGreen:
		return colorGreen
	case contract.SeverityYellow:
		return colorYellow
	case contract.SeverityRed:
		return colorRed
	}
	return ""
}

// reasonsCell joins the reasons with ", " and, when the joined list
// would overflow the column, keeps whole codes that fit and appends
// "+N" for the rest.
func reasonsCell(reasons []contract.Reason) string {
	if len(reasons) == 0 {
		return ""
	}
	out := string(reasons[0])
	kept := 1
	for _, r := range reasons[1:] {
		next := out + ", " + string(r)
		if len(next) > reasonWidth {
			return fmt.Sprintf("%s +%d", out, len(reasons)-kept)
		}
		out = next
		kept++
	}
	return out
}

// Render returns the table for the document's checks. With color, only
// the SEVERITY word of a data row is painted; the header and every other
// cell stay plain, so greyscale output loses nothing but emphasis.
func Render(d *document.Document, color bool) string {
	cells := make([][6]string, 0, len(d.Checks))
	for _, c := range d.Checks {
		// The CHECK cell is the kind and the effective target, separated
		// by one space; the document target is already the effective
		// form after overlay overrides.
		cells = append(cells, [6]string{
			c.Host,
			string(c.Kind) + " " + c.Target,
			string(c.Expected),
			string(c.Result),
			string(c.Severity),
			reasonsCell(c.Reasons),
		})
	}

	var widths [6]int
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range cells {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}

	var b strings.Builder
	writeRow := func(row [6]string, paint bool, severity contract.Severity) {
		for i, c := range row {
			if i > 0 {
				b.WriteString(gutter)
			}
			// The last column is never padded: trailing whitespace
			// would only add noise to diffs.
			last := i == len(row)-1
			if paint && i == 4 && color {
				b.WriteString(severityColor(severity) + c + colorReset)
				if last {
					break
				}
				if pad := widths[i] - len(c); pad > 0 {
					b.WriteString(strings.Repeat(" ", pad))
				}
				continue
			}
			b.WriteString(c)
			if !last {
				if pad := widths[i] - len(c); pad > 0 {
					b.WriteString(strings.Repeat(" ", pad))
				}
			}
		}
		b.WriteString("\n")
	}

	var header [6]string
	copy(header[:], headers[:])
	writeRow(header, false, "")
	for i, row := range cells {
		writeRow(row, true, d.Checks[i].Severity)
	}
	return b.String()
}
