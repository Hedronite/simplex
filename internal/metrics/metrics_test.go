package metrics

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
)

// update rewrites the goldens. Goldens are the contract for byte layout, so a
// change here needs a reason in the commit message.
var update = flag.Bool("update", false, "rewrite the golden expositions")

// The snapshot every golden is rendered against. The clock is pinned so
// simplex_document_age_seconds is a fixed number.
var (
	goldenNow      = time.Date(2026, 11, 13, 18, 0, 12, 0, time.UTC)
	goldenSnapshot = Snapshot{
		RunEnded:    time.Date(2026, 11, 13, 18, 0, 2, 0, time.UTC),
		RunDuration: 1500 * time.Millisecond,
		RunErrors:   3,
		Version:     "0.0.0-dev",
		Commit:      "unknown",
	}
)

// fixtureCases lists the fixture documents that carry a probe document.
func fixtureCases(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "testdata"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	var cases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", "testdata", entry.Name(), "document.json")); err != nil {
			continue
		}
		cases = append(cases, entry.Name())
	}
	sort.Strings(cases)
	return cases
}

func loadDocument(t *testing.T, path string) *document.Document {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc document.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return &doc
}

// TestGolden pins the exposition of every fixture document byte for byte.
func TestGolden(t *testing.T) {
	cases := fixtureCases(t)
	if len(cases) == 0 {
		t.Fatal("no fixture documents under testdata/")
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := goldenSnapshot
			snapshot.Doc = loadDocument(t, filepath.Join("..", "..", "testdata", name, "document.json"))

			got, err := String(&snapshot, goldenNow)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			goldenPath := filepath.Join("testdata", name+".txt")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to write it): %v", err)
			}
			if got != string(want) {
				t.Fatalf("exposition differs from %s:\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
			}
		})
	}
}

// TestPromlint lints every golden with the upstream linter. It is a test-only
// dependency: nothing in the binary links client_golang.
func TestPromlint(t *testing.T) {
	for _, name := range fixtureCases(t) {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", name+".txt"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			problems, err := promlint.New(bytes.NewReader(body)).Lint()
			if err != nil {
				t.Fatalf("lint: %v", err)
			}
			for _, p := range problems {
				t.Errorf("%s: %s: %s", name, p.Metric, p.Text)
			}
		})
	}
}

// TestParseBack reads every golden with the reference parser and checks the
// parts a human reviewer would otherwise have to trust.
func TestParseBack(t *testing.T) {
	for _, name := range fixtureCases(t) {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", name+".txt"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			// The parser carries its own name-validation scheme; the zero value
			// panics, so it is constructed explicitly. These are ordinary
			// Prometheus names, so the legacy scheme applies.
			parser := expfmt.NewTextParser(model.LegacyValidation)
			families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			doc := loadDocument(t, filepath.Join("..", "..", "testdata", name, "document.json"))

			want := map[string]struct {
				kind    string
				samples int
			}{
				"simplex_build_info":                 {"GAUGE", 1},
				"simplex_overall_severity":           {"GAUGE", 1},
				"simplex_vantage_status":             {"GAUGE", len(contract.VantageStatuses)},
				"simplex_host_severity":              {"GAUGE", len(doc.Hosts)},
				"simplex_check_severity":             {"GAUGE", len(doc.Checks)},
				"simplex_check_result":               {"GAUGE", len(doc.Checks) * len(contract.Results)},
				"simplex_check_reason":               {"GAUGE", totalReasons(doc)},
				"simplex_last_run_timestamp_seconds": {"GAUGE", 1},
				"simplex_document_age_seconds":       {"GAUGE", 1},
				"simplex_run_duration_seconds":       {"GAUGE", 1},
				"simplex_run_errors_total":           {"COUNTER", 1},
			}
			// A family with no samples is not written at all, so it is not
			// expected here either.
			for family, spec := range want {
				if spec.samples == 0 {
					delete(want, family)
				}
			}
			if len(families) != len(want) {
				t.Errorf("got %d families, want %d: %v", len(families), len(want), want)
			}
			for family, spec := range want {
				got, ok := families[family]
				if !ok {
					t.Errorf("family %s missing", family)
					continue
				}
				if got.GetType().String() != spec.kind {
					t.Errorf("%s: type %s, want %s", family, got.GetType(), spec.kind)
				}
				if len(got.GetMetric()) != spec.samples {
					t.Errorf("%s: %d samples, want %d", family, len(got.GetMetric()), spec.samples)
				}
			}

			// One-hot invariants: exactly one of each enum group carries 1.
			assertOneHot(t, families, "simplex_vantage_status", "status", len(contract.VantageStatuses), 1)
			assertOneHot(t, families, "simplex_check_result", "result", len(contract.Results), len(doc.Checks))
		})
	}
}

// assertOneHot checks that each group of samples carries exactly one 1 and
// the rest 0, which is what makes a one-hot series usable in a query. Samples
// are grouped by every label except the enum one.
func assertOneHot(t *testing.T, families map[string]*dto.MetricFamily, family, label string, width, groups int) {
	t.Helper()
	mf, ok := families[family]
	if !ok {
		t.Fatalf("family %s missing", family)
	}
	if len(mf.GetMetric()) != width*groups {
		t.Fatalf("%s: %d samples, want %d groups of %d", family, len(mf.GetMetric()), groups, width)
	}
	seen := map[string]map[string]float64{}
	for _, m := range mf.GetMetric() {
		var key strings.Builder
		enum := ""
		for _, lp := range m.GetLabel() {
			if lp.GetName() == label {
				enum = lp.GetValue()
				continue
			}
			key.WriteString(lp.GetName())
			key.WriteByte('=')
			key.WriteString(lp.GetValue())
			key.WriteByte('\x00')
		}
		if enum == "" {
			t.Fatalf("%s: sample without the %s label", family, label)
		}
		g, ok := seen[key.String()]
		if !ok {
			g = map[string]float64{}
			seen[key.String()] = g
		}
		if _, dup := g[enum]; dup {
			t.Fatalf("%s: %s appears twice for one group", family, enum)
		}
		g[enum] = m.GetGauge().GetValue()
	}
	if len(seen) != groups {
		t.Fatalf("%s: %d groups, want %d", family, len(seen), groups)
	}
	for key, g := range seen {
		if len(g) != width {
			t.Fatalf("%s: group %q has %d members, want %d", family, key, len(g), width)
		}
		ones := 0
		for enum, v := range g {
			switch v {
			case 0:
			case 1:
				ones++
			default:
				t.Fatalf("%s: group %q member %s is %v, want 0 or 1", family, key, enum, v)
			}
		}
		if ones != 1 {
			t.Fatalf("%s: group %q has %d ones, want exactly 1", family, key, ones)
		}
	}
}

func totalReasons(doc *document.Document) int {
	n := 0
	for _, c := range doc.Checks {
		for _, r := range c.Reasons {
			for _, known := range contract.ReasonCodes() {
				if r == known {
					n++
					break
				}
			}
		}
	}
	return n
}

// TestBeforeFirstRun pins the cold-start surface: build info and the error
// counter only. There is no runtime collector, so no go_* or process_* series
// are promised or emitted.
func TestBeforeFirstRun(t *testing.T) {
	got, err := String(&Snapshot{Version: "0.0.0-dev", Commit: "unknown"}, goldenNow)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := `# HELP simplex_build_info Build information for this binary.
# TYPE simplex_build_info gauge
simplex_build_info{version="0.0.0-dev",commit="unknown"} 1
# HELP simplex_run_errors_total Runs that produced no document.
# TYPE simplex_run_errors_total counter
simplex_run_errors_total 0
`
	if got != want {
		t.Fatalf("cold-start exposition:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "go_") || strings.Contains(got, "process_") {
		t.Fatalf("cold-start exposition promises runtime series it does not have:\n%s", got)
	}
}

// labelValuePatterns are the only shapes a label value may take. Host is a
// slug, kind a frozen enum member, target the port/path document form, and the
// rest are frozen enum values.
var (
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	targetPattern = regexp.MustCompile(`^:[0-9]{1,5}(/[!-~]*)?$`)
	// Anything address- or hostname-shaped is a failure wherever it appears.
	addressPattern = regexp.MustCompile(`([0-9]{1,3}\.){3}[0-9]{1,3}|\[[0-9a-fA-F:]{2,}\]|\.(ts\.net|local|lan|internal|home\.arpa)\b|://`)
)

// TestScrubFixtureLabels walks every golden and fails on any label value that
// is not one of the shapes the contract allows, and on any address- or
// hostname-shaped text anywhere in the output.
func TestScrubFixtureLabels(t *testing.T) {
	reSample := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? `)
	reLabel := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

	for _, name := range fixtureCases(t) {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", name+".txt"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			text := string(body)
			if loc := addressPattern.FindString(text); loc != "" {
				t.Fatalf("address- or hostname-shaped text %q appears in the exposition", loc)
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "#") || line == "" {
					continue
				}
				m := reSample.FindStringSubmatch(line)
				if m == nil {
					t.Fatalf("unparsable sample line %q", line)
				}
				for _, lm := range reLabel.FindAllStringSubmatch(m[3], -1) {
					assertLabelValue(t, name, lm[1], lm[2])
				}
			}
		})
	}
}

// assertLabelValue checks one label value against its documented shape.
func assertLabelValue(t *testing.T, fixture, name, value string) {
	t.Helper()
	ok := false
	switch name {
	case "host":
		ok = slugPattern.MatchString(value)
	case "kind":
		ok = contains(contract.Kinds, contract.Kind(value))
	case "target":
		ok = targetPattern.MatchString(value)
	case "result":
		ok = contains(contract.Results, contract.Result(value))
	case "status":
		ok = contains(contract.VantageStatuses, contract.VantageStatus(value))
	case "reason":
		ok = contains(contract.ReasonCodes(), contract.Reason(value))
	case "version", "commit":
		ok = value != ""
	default:
		t.Fatalf("%s: unexpected label %q", fixture, name)
	}
	if !ok {
		t.Fatalf("%s: label %s=%q is not an allowed value", fixture, name, value)
	}
}

func contains[T comparable](set []T, v T) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// TestSentinelNeverEchoed puts an address-shaped sentinel into fields the
// encoder receives but must not export, and proves it stays out of the output.
func TestSentinelNeverEchoed(t *testing.T) {
	const sentinel = "sentinel.example.invalid:9999"
	doc := &document.Document{
		APIVersion:  contract.ProbeAPIVersion,
		GeneratedAt: goldenNow.Add(-time.Minute).Format(time.RFC3339),
		Vantage:     document.Vantage{ID: sentinel, Status: contract.VantageOK},
		Overall:     contract.SeverityYellow,
		Hosts:       []document.Host{{ID: "box", Severity: contract.SeverityYellow}},
		Checks: []document.Check{{
			Host: "box", Kind: contract.KindPromUp, Target: ":9090/-/ready",
			Result: contract.ResultDown, Severity: contract.SeverityYellow,
			Expected: contract.ExpectedLight,
			// A code the contract does not know: the encoder emits reasons from
			// the frozen enum, so this must not become a label value.
			Reasons: []contract.Reason{"sentinel_reason_code"},
		}},
	}
	got, err := String(&Snapshot{Doc: doc, Version: "v", Commit: "c"}, goldenNow)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(got, sentinel) {
		t.Fatalf("the vantage id leaked into the exposition:\n%s", got)
	}
	if strings.Contains(got, "sentinel_reason_code") {
		t.Fatalf("an unknown reason code became a label value:\n%s", got)
	}
	if strings.Contains(got, "simplex_check_reason{") {
		t.Fatalf("a check with no frozen reason still produced a reason series:\n%s", got)
	}
}

// TestCardinalityBound pins the series count of the largest fixture and the
// formula it comes from, so growth in labels shows up as a number.
func TestCardinalityBound(t *testing.T) {
	var largest string
	var largestCount int
	for _, name := range fixtureCases(t) {
		body, err := os.ReadFile(filepath.Join("testdata", name+".txt"))
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		count := 0
		for _, line := range strings.Split(string(body), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				count++
			}
		}
		if count > largestCount {
			largest, largestCount = name, count
		}
	}
	if largest == "" {
		t.Fatal("no goldens to measure")
	}

	doc := loadDocument(t, filepath.Join("..", "..", "testdata", largest, "document.json"))
	// One build info, one overall, three vantage statuses, one host severity
	// per host, one check severity and three results per check, one reason per
	// reason code present, three run-level series and one error counter.
	want := 1 + 1 + len(contract.VantageStatuses) + len(doc.Hosts) +
		len(doc.Checks)*(1+len(contract.Results)) + totalReasons(doc) + 3 + 1
	if largestCount != want {
		t.Fatalf("%s: %d samples, the formula gives %d", largest, largestCount, want)
	}
	// A per-check reason ceiling: no check may carry more series than the
	// frozen enum allows.
	perCheck := 1 + len(contract.Results) + len(contract.ReasonCodes())
	if bound := 5 + len(doc.Hosts) + len(doc.Checks)*perCheck; largestCount > bound {
		t.Fatalf("%s: %d samples exceeds the label-space bound %d", largest, largestCount, bound)
	}
	t.Logf("largest fixture %s: %d samples, per-check ceiling %d", largest, largestCount, perCheck)
}

// TestNamingAndTypes pins the naming rules the lint cannot fully express:
// counters end in _total, durations carry _seconds, and no family is both.
func TestNamingAndTypes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "green.txt"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	types := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			t.Fatalf("malformed TYPE line %q", line)
		}
		types[fields[2]] = fields[3]
	}
	if len(types) == 0 {
		t.Fatal("no TYPE lines")
	}
	for name, kind := range types {
		if strings.HasSuffix(name, "_total") && kind != "counter" {
			t.Errorf("%s ends in _total but is a %s", name, kind)
		}
		if kind == "counter" && !strings.HasSuffix(name, "_total") {
			t.Errorf("%s is a counter but does not end in _total", name)
		}
		if strings.Contains(name, "duration") && !strings.HasSuffix(name, "_seconds") {
			t.Errorf("%s measures time but does not carry _seconds", name)
		}
		if strings.Contains(name, "timestamp") && !strings.HasSuffix(name, "_seconds") {
			t.Errorf("%s is a timestamp but does not carry _seconds", name)
		}
		if !strings.HasPrefix(name, "simplex_") {
			t.Errorf("%s does not carry the simplex_ prefix", name)
		}
	}
	for _, want := range []string{
		"simplex_overall_severity", "simplex_host_severity", "simplex_check_severity",
		"simplex_check_result", "simplex_check_reason", "simplex_vantage_status",
		"simplex_last_run_timestamp_seconds", "simplex_document_age_seconds",
		"simplex_run_duration_seconds", "simplex_run_errors_total", "simplex_build_info",
	} {
		if _, ok := types[want]; !ok {
			t.Errorf("family %s is missing", want)
		}
	}
	if len(types) != 11 {
		t.Errorf("%d families, want exactly the 11 the plan lists", len(types))
	}
}
