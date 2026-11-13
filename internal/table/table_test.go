package table

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
)

func docWith(checks []document.Check) *document.Document {
	return &document.Document{
		APIVersion:  contract.ProbeAPIVersion,
		GeneratedAt: "2026-11-13T18:00:00Z",
		Vantage:     document.Vantage{ID: "lathe", Status: contract.VantageOK},
		Overall:     contract.SeverityYellow,
		Checks:      checks,
	}
}

func TestRenderGolden(t *testing.T) {
	d := docWith([]document.Check{
		{Host: "box", Kind: contract.KindPromUp, Target: ":9090",
			Result: contract.ResultUp, Severity: contract.SeverityGreen, Expected: contract.ExpectedLight},
		{Host: "lathe", Kind: contract.KindKubeAPIServer, Target: ":6443",
			Result: contract.ResultDown, Severity: contract.SeverityRed, Expected: contract.ExpectedDark,
			Reasons: []contract.Reason{contract.ReasonUnexpectedDark}},
	})
	want := "" +
		"HOST  CHECK               EXPECTED  RESULT  SEVERITY  REASONS\n" +
		"box   prom_up :9090      light     up      GREEN\n" +
		"lathe  kube_apiserver :6443  dark     down    RED       unexpected_dark\n"
	if got := Render(d, false); got != want {
		t.Fatalf("golden table differs\n--- got\n%s--- want\n%s", quoteLines(got), quoteLines(want))
	}
}

func quoteLines(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		b.WriteString("\t" + l + "\n")
	}
	return b.String()
}

func TestRenderReasonsTruncateToWholeCodes(t *testing.T) {
	reasons := []contract.Reason{
		contract.ReasonTCPConnectOK, contract.ReasonTCPConnectRefused, contract.ReasonTCPConnectTimeout,
		contract.ReasonHTTPTimeout, contract.ReasonHTTPUnexpectedStatus, contract.ReasonPromNotReady,
		contract.ReasonPromAuth, contract.ReasonMetricsBodyBad,
	}
	d := docWith([]document.Check{{
		Host: "tower", Kind: contract.KindTSReach, Target: ":9100",
		Result: contract.ResultUnknown, Severity: contract.SeverityYellow, Expected: contract.ExpectedLight,
		Reasons: reasons,
	}})
	lines := strings.Split(strings.TrimSuffix(Render(d, false), "\n"), "\n")
	last := lines[len(lines)-1]
	// Two whole codes fit; the other six are counted in the marker.
	want := "tcp_connect_ok, tcp_connect_refused +6"
	if !strings.HasSuffix(last, want) {
		t.Fatalf("truncated reasons cell does not end with %q: %q", want, last)
	}
}

func TestRenderColorPaintsOnlySeverity(t *testing.T) {
	d := docWith([]document.Check{{
		Host: "box", Kind: contract.KindPromUp, Target: ":9090/-/ready",
		Result: contract.ResultDown, Severity: contract.SeverityYellow, Expected: contract.ExpectedLight,
		Reasons: []contract.Reason{contract.ReasonUnexpectedDark},
	}})
	plain := Render(d, false)
	painted := Render(d, true)
	if painted == plain {
		t.Fatal("color made no difference")
	}
	if !strings.Contains(painted, colorYellow+"YELLOW"+colorReset) {
		t.Fatalf("the severity word is not wrapped: %q", painted)
	}
	if stripEscapes(painted) != plain {
		t.Fatal("stripping color does not return the plain table")
	}
}

var escape = regexp.MustCompile("\x1b\\[[0-9]+m")

func stripEscapes(s string) string {
	return escape.ReplaceAllString(s, "")
}

func TestRenderHeldRowKeepsResultAndReason(t *testing.T) {
	d := docWith([]document.Check{{
		Host: "tower", Kind: contract.KindTSReach, Target: ":9100",
		Result: contract.ResultDown, Severity: contract.SeverityGreen, Expected: contract.ExpectedLight,
		Reasons: []contract.Reason{contract.ReasonHeld, contract.ReasonTCPConnectRefused},
	}})
	out := Render(d, false)
	if !strings.Contains(out, "down") {
		t.Fatal("a held check must keep its real result in the RESULT column")
	}
	if !strings.Contains(out, "held") {
		t.Fatal("a held check must carry held among its reasons")
	}
	if strings.Contains(out, "RED") {
		t.Fatal("a held check must not read as RED")
	}
}

func TestRenderDeterministicAndColumnAligned(t *testing.T) {
	d := docWith([]document.Check{
		{Host: "box", Kind: contract.KindPromUp, Target: ":9090/-/ready",
			Result: contract.ResultUp, Severity: contract.SeverityGreen, Expected: contract.ExpectedLight},
		{Host: "lathe", Kind: contract.KindKubeAPIServer, Target: ":6443",
			Result: contract.ResultDown, Severity: contract.SeverityRed, Expected: contract.ExpectedDark,
			Reasons: []contract.Reason{contract.ReasonUnexpectedDark}},
	})
	if Render(d, false) != Render(d, false) {
		t.Fatal("the same document rendered twice differs")
	}
	lines := strings.Split(strings.TrimSuffix(Render(d, false), "\n"), "\n")
	header := lines[0]
	for _, name := range []string{"CHECK", "EXPECTED", "RESULT", "SEVERITY", "REASONS"} {
		offset := strings.Index(header, name)
		if offset < 0 {
			t.Fatalf("header %q missing: %q", name, header)
		}
		for i, line := range lines[1:] {
			cell := strings.Repeat(" ", offset) + line
			field := cell[offset:]
			if strings.HasPrefix(field, " ") {
				t.Fatalf("row %d column %s starts with padding: %q", i+1, name, line)
			}
		}
	}
}
