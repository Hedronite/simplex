// Package grafana_test holds the hermetic drift test for the checked-in
// Grafana dashboard: the dashboard cannot drift from what
// internal/metrics actually exports. Every metric name and label name a
// panel or variable query uses must exist in a rendered exposition over
// the fixture documents, and the one-hot families must be selected with
// == 1. The tests run everywhere Go runs, including macOS CI, and touch
// no network, no daemon and no root.
package grafana_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/document"
	"github.com/Hedronite/simplex/internal/metrics"
	"go.yaml.in/yaml/v4"
)

const (
	dashboardFile = "dashboards/simplex.json"
	providerFile  = "provisioning/dashboards/simplex.yaml"
	dashboardUID  = "simplex"
	dashboardTitl = "Simplex mesh health"
	promUID       = "lea-prom"
)

// TestDashboardParsesStrictly decodes the file once, rejects trailing
// content, and checks the frozen identity and the unique panel ids.
func TestDashboardParsesStrictly(t *testing.T) {
	raw := readRepoFile(t, dashboardFile)
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("dashboard is not JSON: %v", err)
	}
	if dec.More() {
		t.Fatal("trailing content after the dashboard object")
	}
	if got, _ := root["uid"].(string); got != dashboardUID {
		t.Fatalf("dashboard uid %q, want %q", got, dashboardUID)
	}
	if got, _ := root["title"].(string); got != dashboardTitl {
		t.Fatalf("dashboard title %q, want %q", got, dashboardTitl)
	}
	schema, _ := root["schemaVersion"].(json.Number)
	if schema == "" {
		t.Fatal("dashboard has no schemaVersion")
	}
	panels, _ := root["panels"].([]any)
	if len(panels) == 0 {
		t.Fatal("dashboard has no panels")
	}
	seen := map[int]bool{}
	for i, p := range panels {
		pm, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("panel %d is not an object", i)
		}
		id, err := pm["id"].(json.Number).Int64()
		if err != nil {
			t.Fatalf("panel %d has no integer id", i)
		}
		if seen[int(id)] {
			t.Fatalf("panel id %d appears twice", id)
		}
		seen[int(id)] = true
	}
}

// TestDatasourcesAreExactlyLeaProm walks every panel and every variable:
// the datasource must be exactly the shared Prometheus datasource, and no
// other uid appears anywhere in the file except the dashboard's own.
func TestDatasourcesAreExactlyLeaProm(t *testing.T) {
	raw := readRepoFile(t, dashboardFile)
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	panels, _ := root["panels"].([]any)
	for i, p := range panels {
		assertDatasource(t, "panel", i, p)
	}
	if tpl, ok := root["templating"].(map[string]any); ok {
		vars, _ := tpl["list"].([]any)
		for i, v := range vars {
			vm, _ := v.(map[string]any)
			if name, _ := vm["name"].(string); name == "" {
				continue
			}
			if qtype, _ := vm["type"].(string); qtype != "query" {
				continue
			}
			assertDatasource(t, "variable", i, v)
		}
	}
	uidPattern := regexp.MustCompile(`"uid"\s*:\s*"([^"]+)"`)
	for _, m := range uidPattern.FindAllStringSubmatch(string(raw), -1) {
		switch m[1] {
		case promUID, dashboardUID:
		default:
			t.Fatalf("uid %q in the dashboard is neither %q nor the dashboard uid", m[1], promUID)
		}
	}
}

func assertDatasource(t *testing.T, what string, i int, panel any) {
	t.Helper()
	pm, ok := panel.(map[string]any)
	if !ok {
		t.Fatalf("%s %d is not an object", what, i)
	}
	ds, ok := pm["datasource"].(map[string]any)
	if !ok {
		t.Fatalf("%s %d has no datasource object", what, i)
	}
	if got, _ := ds["type"].(string); got != "prometheus" {
		t.Fatalf("%s %d datasource type %q, want prometheus", what, i, got)
	}
	if got, _ := ds["uid"].(string); got != promUID {
		t.Fatalf("%s %d datasource uid %q, want %q", what, i, got, promUID)
	}
}

// TestNoAddressesOrHostnames scans the raw dashboard and provider text.
// The tree scrub covers the same rule for the tracked tree; this is the
// in-test guard the plan asks for.
func TestNoAddressesOrHostnames(t *testing.T) {
	for _, f := range []string{dashboardFile, providerFile} {
		raw := string(readRepoFile(t, f))
		if m := regexp.MustCompile(`([0-9]{1,3}\.){3}[0-9]{1,3}`).FindString(raw); m != "" {
			t.Fatalf("%s contains an address literal %q", f, m)
		}
		for _, pat := range []string{`\.ts\.net`, `\.local\b`, `\.lan\b`, `\.tailnet`} {
			if regexp.MustCompile(pat).MatchString(raw) {
				t.Fatalf("%s contains hostname-shaped text matching %s", f, pat)
			}
		}
	}
}

// TestQueriesAgreeWithEncoder extracts every metric name and label name
// and every fixed label value the dashboard queries use, and asserts each
// exists in what the encoder exports over the fixture documents. The
// reason set is not counted here: 3a grows it from 16 to 21 and this test
// must not care.
func TestQueriesAgreeWithEncoder(t *testing.T) {
	families := map[string]bool{}
	labels := map[string]bool{}
	labelValues := map[string]map[string]bool{}
	addSeries := func(line string) {
		seriesPattern := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})?\s+[-+0-9]`)
		m := seriesPattern.FindStringSubmatch(line)
		if m == nil {
			return
		}
		families[m[1]] = true
		if m[3] == "" {
			return
		}
		pairPattern := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
		for _, p := range pairPattern.FindAllStringSubmatch(m[3], -1) {
			labels[p[1]] = true
			if labelValues[p[1]] == nil {
				labelValues[p[1]] = map[string]bool{}
			}
			labelValues[p[1]][p[2]] = true
		}
	}
	now := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	render := func(doc *document.Document) {
		out, err := metrics.String(&metrics.Snapshot{
			Doc: doc, RunEnded: now, RunDuration: 3 * time.Second,
			RunErrors: 0, Version: "test", Commit: "test",
		}, now)
		if err != nil {
			t.Fatalf("render exposition: %v", err)
		}
		for _, line := range strings.Split(out, "\n") {
			addSeries(line)
		}
	}
	cases, err := filepath.Glob(filepath.Join("..", "testdata", "*", "document.json"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no fixture documents: %v", err)
	}
	seenVantage := map[string]bool{}
	for _, path := range cases {
		doc := loadDocument(t, path)
		seenVantage[string(doc.Vantage.Status)] = true
		render(doc)
	}
	// The vantage one-hot has three states; the fixtures may not cover
	// them all, so the unchecked state is rendered synthetically from a
	// copy of one fixture document. No count is hard-coded.
	if !seenVantage[string(contract.VantageUnchecked)] {
		doc := loadDocument(t, cases[0])
		doc.Vantage.Status = contract.VantageUnchecked
		render(doc)
	}

	dash := readRepoFile(t, dashboardFile)
	var root map[string]any
	if err := json.Unmarshal(dash, &root); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	var exprs []string
	panels, _ := root["panels"].([]any)
	for i, p := range panels {
		pm, _ := p.(map[string]any)
		targets, _ := pm["targets"].([]any)
		for j, tg := range targets {
			tm, ok := tg.(map[string]any)
			if !ok {
				t.Fatalf("panel %d target %d is not an object", i, j)
			}
			if e, _ := tm["expr"].(string); e != "" {
				exprs = append(exprs, e)
			}
		}
	}
	if tpl, ok := root["templating"].(map[string]any); ok {
		vars, _ := tpl["list"].([]any)
		for i, v := range vars {
			vm, _ := v.(map[string]any)
			if qtype, _ := vm["type"].(string); qtype != "query" {
				continue
			}
			q, ok := vm["query"].(map[string]any)
			if !ok {
				t.Fatalf("variable %d has no query object", i)
			}
			if e, _ := q["query"].(string); e != "" {
				exprs = append(exprs, e)
			}
		}
	}
	if len(exprs) == 0 {
		t.Fatal("no expressions found in the dashboard")
	}

	metricPattern := regexp.MustCompile(`simplex_[a-z0-9_]*`)
	selectorPattern := regexp.MustCompile(`\{([^{}]+)\}`)
	namePattern := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)`)
	valuesPattern := regexp.MustCompile(`label_values\(\s*[^,()]+,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)`)
	byPattern := regexp.MustCompile(`(?:by|without|on|group_left|group_right)\s*\(([^)]*)\)`)

	for _, expr := range exprs {
		for _, name := range metricPattern.FindAllString(expr, -1) {
			if !families[name] {
				t.Fatalf("query %q uses metric %q, which the encoder never exports", expr, name)
			}
		}
		var names []string
		for _, sel := range selectorPattern.FindAllStringSubmatch(expr, -1) {
			for _, p := range namePattern.FindAllStringSubmatch(sel[1], -1) {
				names = append(names, p[1])
			}
		}
		for _, m := range valuesPattern.FindAllStringSubmatch(expr, -1) {
			names = append(names, m[1])
		}
		for _, m := range byPattern.FindAllStringSubmatch(expr, -1) {
			for _, tok := range strings.Split(m[1], ",") {
				tok = strings.TrimSpace(tok)
				if regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(tok) {
					names = append(names, tok)
				}
			}
		}
		for _, name := range names {
			if !labels[name] {
				t.Fatalf("query %q uses label %q, which the encoder never exports", expr, name)
			}
		}
		// Fixed values on the enum-carrying labels must be codes or
		// states the encoder actually emits.
		pairPattern := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*=~?"((?:[^"\\]|\\.)*)"`)
		for _, sel := range selectorPattern.FindAllStringSubmatch(expr, -1) {
			for _, p := range pairPattern.FindAllStringSubmatch(sel[1], -1) {
				label, value := p[1], p[2]
				if label != "result" && label != "reason" && label != "status" {
					continue
				}
				if value == "" || strings.HasPrefix(value, "$") {
					continue
				}
				if !labelValues[label][value] {
					t.Fatalf("query %q filters %s=%q, which the encoder never emits", expr, label, value)
				}
			}
		}
		// A one-hot family is read with == 1 or not at all.
		for _, hot := range []string{"simplex_check_result", "simplex_check_reason", "simplex_vantage_status"} {
			if strings.Contains(expr, hot) && !strings.Contains(expr, "== 1") {
				t.Fatalf("query %q reads the one-hot %s without == 1", expr, hot)
			}
		}
	}
}

// TestProviderFileChecksOut pins the provider: a file provider pointing at
// the standard dashboards directory, strict decode.
func TestProviderFileChecksOut(t *testing.T) {
	raw := readRepoFile(t, providerFile)
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var provider struct {
		APIVersion int `yaml:"apiVersion"`
		Providers  []struct {
			Name                  string `yaml:"name"`
			OrgID                 int    `yaml:"orgId"`
			Folder                string `yaml:"folder"`
			FolderUID             string `yaml:"folderUid"`
			Type                  string `yaml:"type"`
			DisableDeletion       bool   `yaml:"disableDeletion"`
			UpdateIntervalSeconds int    `yaml:"updateIntervalSeconds"`
			AllowUiUpdates        bool   `yaml:"allowUiUpdates"`
			Options               struct {
				Path                      string `yaml:"path"`
				FoldersFromFilesStructure bool   `yaml:"foldersFromFilesStructure"`
			} `yaml:"options"`
		} `yaml:"providers"`
	}
	if err := dec.Decode(&provider); err != nil {
		t.Fatalf("provider file does not parse strictly: %v", err)
	}
	if provider.APIVersion != 1 {
		t.Fatalf("apiVersion %d, want 1", provider.APIVersion)
	}
	if len(provider.Providers) != 1 {
		t.Fatalf("want exactly one provider, got %d", len(provider.Providers))
	}
	p := provider.Providers[0]
	if p.Type != "file" {
		t.Fatalf("provider type %q, want file", p.Type)
	}
	if p.Options.Path != "/var/lib/grafana/dashboards" {
		t.Fatalf("provider path %q, want /var/lib/grafana/dashboards", p.Options.Path)
	}
}

func readRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return raw
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
