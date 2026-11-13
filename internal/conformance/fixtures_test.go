package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v4"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/exitcode"
)

// now is the clock every fixture is built against (`--now`).
var fixtureNow = time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)

// The document and config shapes the fixtures must satisfy. Decoding is
// strict in both directions, so a typo in a fixture is a test failure rather
// than a silently ignored field.
type document struct {
	APIVersion  string `json:"apiVersion"`
	GeneratedAt string `json:"generated_at"`
	Vantage     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"vantage"`
	Overall string  `json:"overall"`
	Hosts   []host  `json:"hosts"`
	Checks  []check `json:"checks"`
}

type host struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Reasons  []string `json:"reasons"`
}

type check struct {
	Host     string   `json:"host"`
	Kind     string   `json:"kind"`
	Target   string   `json:"target"`
	Result   string   `json:"result"`
	Severity string   `json:"severity"`
	Expected string   `json:"expected"`
	Reasons  []string `json:"reasons"`
}

type configFile struct {
	APIVersion string `yaml:"apiVersion"`
	Vantage    struct {
		ID       string `yaml:"id"`
		Canaries []struct {
			Host   string `yaml:"host"`
			Kind   string `yaml:"kind"`
			Target string `yaml:"target"`
		} `yaml:"canaries"`
	} `yaml:"vantage"`
	Hosts []struct {
		ID     string `yaml:"id"`
		Checks []struct {
			Kind     string `yaml:"kind"`
			Expected string `yaml:"expected"`
			Port     *int   `yaml:"port"`
			Path     string `yaml:"path"`
		} `yaml:"checks"`
	} `yaml:"hosts"`
	Holds []struct {
		Host   string `yaml:"host"`
		Kind   string `yaml:"kind"`
		Reason string `yaml:"reason"`
		Until  string `yaml:"until"`
	} `yaml:"holds"`
}

type overlayFile struct {
	APIVersion string `yaml:"apiVersion"`
	Hosts      map[string]struct {
		Address   string `yaml:"address"`
		LanAddr   string `yaml:"lan_address"`
		Overrides []struct {
			Kind   string `yaml:"kind"`
			Target string `yaml:"target"`
			Port   *int   `yaml:"port"`
			Path   string `yaml:"path"`
		} `yaml:"overrides"`
	} `yaml:"hosts"`
}

// defaultPort is the kind's port when the config does not set one.
func defaultPort(kind contract.Kind) int {
	switch kind {
	case contract.KindTSReach, contract.KindNodeExporter:
		return 9100
	case contract.KindKubeAPIServer:
		return 6443
	case contract.KindLANReach, contract.KindSSH:
		return 22
	case contract.KindPromUp:
		return 9090
	}
	return 0
}

// defaultPath is the path an HTTP kind requests when nothing overrides it.
func defaultPath(kind contract.Kind) string {
	switch kind {
	case contract.KindNodeExporter:
		return contract.PathMetrics
	case contract.KindPromUp:
		return contract.PathReady
	}
	return ""
}

// effectiveTarget is the document's target for one configured check: port,
// path, then the overlay override, which wins.
func effectiveTarget(cfgCheck configFileCheck, override *overlayOverride) string {
	kind := contract.Kind(cfgCheck.Kind)
	port := defaultPort(kind)
	if cfgCheck.Port != nil {
		port = *cfgCheck.Port
	}
	path := defaultPath(kind)
	if cfgCheck.Path != "" {
		path = cfgCheck.Path
	}
	if override != nil {
		if override.Port != nil {
			port = *override.Port
		}
		if override.Path != "" {
			path = override.Path
		}
	}
	return fmt.Sprintf(":%d%s", port, path)
}

type configFileCheck struct {
	Kind     string
	Expected string
	Port     *int
	Path     string
}

type overlayOverride struct {
	Kind   string
	Target string
	Port   *int
	Path   string
}

// defaultTarget is the instance string before an overlay override.
func defaultTarget(c configFileCheck) string {
	return effectiveTarget(c, nil)
}

// findOverride returns the overlay override for one check instance. An
// override matches on kind and, when it names a target, on the config target.
func findOverride(ov *overlayFile, slug, kind, target string) *overlayOverride {
	if ov == nil {
		return nil
	}
	entry, ok := ov.Hosts[slug]
	if !ok {
		return nil
	}
	for i := range entry.Overrides {
		o := entry.Overrides[i]
		if o.Kind != kind {
			continue
		}
		if o.Target != "" && o.Target != target {
			continue
		}
		return &overlayOverride{Kind: o.Kind, Target: o.Target, Port: o.Port, Path: o.Path}
	}
	return nil
}

func loadYAMLStrict(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	return dec.Decode(into)
}

func readExit(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "exit"))
	if err != nil {
		t.Fatalf("%s: no exit file: %v", dir, err)
	}
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &code); err != nil {
		t.Fatalf("%s: exit file %q is not an integer", dir, strings.TrimSpace(string(raw)))
	}
	return code
}

// outcomeReasons returns the codes whose contract class is outcome.
func outcomeReasons(in []string) []string {
	var out []string
	for _, code := range in {
		for _, spec := range contract.ReasonSpecs {
			if string(spec.Code) == code && spec.Class == contract.ClassOutcome {
				out = append(out, code)
			}
		}
	}
	return out
}

func hasReason(in []string, want contract.Reason) bool {
	for _, code := range in {
		if code == string(want) {
			return true
		}
	}
	return false
}

// severityRank orders the three severities, worst last.
func severityRank(s contract.Severity) int {
	for i, v := range contract.Severities {
		if v == s {
			return i
		}
	}
	return -1
}

func worst(a, b contract.Severity) contract.Severity {
	if severityRank(b) > severityRank(a) {
		return b
	}
	return a
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestFixtures is the S0 gate: every case must validate against its schema,
// recompute to its own document under the severity rules, and declare the
// exit code its document implies.
func TestFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "testdata")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	probeSchema := compileSchema(t, "probe")
	configSchema := compileSchema(t, "config")
	overlaySchema := compileSchema(t, "overlay")

	cases := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cases++
		t.Run(entry.Name(), func(t *testing.T) {
			dir := filepath.Join(root, entry.Name())
			code := readExit(t, dir)

			var cfg configFile
			if err := loadYAMLStrict(filepath.Join(dir, "config.yaml"), &cfg); err != nil {
				t.Fatalf("config.yaml: %v", err)
			}
			cfgDoc, err := jsonschema.UnmarshalJSON(strings.NewReader(readAsJSON(t, filepath.Join(dir, "config.yaml"))))
			if err != nil {
				t.Fatalf("config.yaml is not JSON-compatible YAML: %v", err)
			}
			cfgErr := configSchema.Validate(cfgDoc)

			var ov *overlayFile
			if _, statErr := os.Stat(filepath.Join(dir, "private.yaml")); statErr == nil {
				ov = &overlayFile{}
				if err := loadYAMLStrict(filepath.Join(dir, "private.yaml"), ov); err != nil {
					t.Fatalf("private.yaml: %v", err)
				}
				ovDoc, err := jsonschema.UnmarshalJSON(strings.NewReader(readAsJSON(t, filepath.Join(dir, "private.yaml"))))
				if err != nil {
					t.Fatalf("private.yaml is not JSON-compatible YAML: %v", err)
				}
				if err := overlaySchema.Validate(ovDoc); err != nil {
					t.Fatalf("private.yaml does not validate against the overlay schema: %v", err)
				}
			}

			if code == exitcode.Config {
				// A schema-invalid document would be rejected before the
				// schema gate; the fixture must prove the semantic layer.
				if cfgErr != nil {
					t.Fatalf("config-error case must be schema-valid and fail semantically, but the schema rejects it: %v", cfgErr)
				}
				if why := canaryError(cfg); why == "" {
					t.Fatal("case expects exit 3 but no semantic error is detectable")
				}
				assertNoDocument(t, dir)
				return
			}
			if cfgErr != nil {
				t.Fatalf("config.yaml does not validate against the config schema: %v", cfgErr)
			}
			if code == exitcode.Runtime {
				assertNoDocument(t, dir)
				return
			}

			if !exitcode.WritesDocument(code) {
				t.Fatalf("exit %d writes no document, but the case is not an error case", code)
			}
			docPath := filepath.Join(dir, "document.json")
			raw, err := os.ReadFile(docPath)
			if err != nil {
				t.Fatalf("case expects exit %d, which writes a document: %v", code, err)
			}
			var doc document
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("document.json: %v", err)
			}
			docAny, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatalf("document.json is not JSON: %v", err)
			}
			if err := probeSchema.Validate(docAny); err != nil {
				t.Fatalf("document.json does not validate against the probe schema: %v", err)
			}

			checkDocument(t, doc, cfg, ov)

			if doc.GeneratedAt != fixtureNow.Format(time.RFC3339) {
				t.Fatalf("generated_at = %q, want the pinned clock %q", doc.GeneratedAt, fixtureNow.Format(time.RFC3339))
			}
			want := impliedExit(doc)
			if code != want {
				t.Fatalf("exit file says %d but the document implies %d", code, want)
			}
		})
	}
	if cases == 0 {
		t.Fatal("testdata/ has no fixture cases")
	}
}

func readAsJSON(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(out)
}

func assertNoDocument(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "document.json")); err == nil {
		t.Fatal("error case must not carry a document.json")
	}
}

// canaryError reports the semantic config error a schema cannot express.
func canaryError(cfg configFile) string {
	known := map[string]bool{}
	for _, h := range cfg.Hosts {
		known[h.ID] = true
	}
	for _, c := range cfg.Vantage.Canaries {
		if !known[c.Host] {
			return fmt.Sprintf("canary names host %q, which is not configured", c.Host)
		}
	}
	return ""
}

// impliedExit maps a finished document to its exit code.
func impliedExit(doc document) int {
	if doc.Vantage.Status == string(contract.VantageBlind) {
		return exitcode.Blind
	}
	switch contract.Severity(doc.Overall) {
	case contract.SeverityGreen:
		return exitcode.Green
	case contract.SeverityYellow:
		return exitcode.Yellow
	case contract.SeverityRed:
		return exitcode.Red
	}
	return -1
}

// checkDocument recomputes section 5 and section 6 from the document itself
// plus the config and overlay, and compares every derived field.
func checkDocument(t *testing.T, doc document, cfg configFile, ov *overlayFile) {
	t.Helper()

	// Each result agrees with its kind and outcome code (semantics section 3).
	for _, c := range doc.Checks {
		if err := outcomeAgreement(c); err != nil {
			t.Fatal(err)
		}
	}

	// Ordering: checks by (host, kind, target), hosts by id.
	for i := 1; i < len(doc.Checks); i++ {
		prev, cur := doc.Checks[i-1], doc.Checks[i]
		if (prev.Host + "\x00" + prev.Kind + "\x00" + prev.Target) >= (cur.Host + "\x00" + cur.Kind + "\x00" + cur.Target) {
			t.Fatalf("checks not sorted by (host, kind, target): %q before %q", prev, cur)
		}
	}
	for i := 1; i < len(doc.Hosts); i++ {
		if doc.Hosts[i-1].ID >= doc.Hosts[i].ID {
			t.Fatalf("hosts not sorted by id: %q before %q", doc.Hosts[i-1].ID, doc.Hosts[i].ID)
		}
	}

	// Config coverage and effective targets.
	cfgChecks := map[string]configFileCheck{}
	var orderedInstances []string
	for _, h := range cfg.Hosts {
		for _, c := range h.Checks {
			cfgChecks[h.ID+"\x00"+c.Kind] = configFileCheck{Kind: c.Kind, Expected: c.Expected, Port: c.Port, Path: c.Path}
			orderedInstances = append(orderedInstances, h.ID+"\x00"+c.Kind)
		}
	}
	if len(doc.Checks) != len(orderedInstances) {
		t.Fatalf("document has %d checks, config defines %d", len(doc.Checks), len(orderedInstances))
	}

	active := func(h check) bool {
		for _, hold := range cfg.Holds {
			if hold.Host != contract.Wildcard && hold.Host != h.Host {
				continue
			}
			if hold.Kind != contract.Wildcard && hold.Kind != h.Kind {
				continue
			}
			until, err := time.Parse(time.RFC3339, hold.Until)
			if err != nil {
				t.Fatalf("hold until %q: %v", hold.Until, err)
			}
			if until.After(fixtureNow) {
				return true
			}
		}
		return false
	}

	blind := doc.Vantage.Status == string(contract.VantageBlind)

	// Pass one: per-check base severity and reasons, without the pair rule.
	baseSeverity := map[string]contract.Severity{}
	baseReasons := map[string][]string{}
	for i, c := range doc.Checks {
		key := c.Host + "\x00" + c.Kind + "\x00" + c.Target
		cfgCheck, ok := cfgChecks[c.Host+"\x00"+c.Kind]
		if !ok {
			t.Fatalf("checks[%d]: no config check for %s/%s", i, c.Host, c.Kind)
		}
		if c.Expected != cfgCheck.Expected {
			t.Fatalf("checks[%d]: expected %q, config says %q", i, c.Expected, cfgCheck.Expected)
		}
		want := effectiveTarget(cfgCheck, findOverride(ov, c.Host, c.Kind, defaultTarget(cfgCheck)))
		if c.Target != want {
			t.Fatalf("checks[%d]: target %q, effective target is %q", i, c.Target, want)
		}

		outcomes := outcomeReasons(c.Reasons)
		if len(outcomes) > 1 {
			t.Fatalf("checks[%d]: %d outcome codes %v, at most one is allowed", i, len(outcomes), outcomes)
		}

		var severity contract.Severity
		reasons := append([]string(nil), outcomes...)
		switch {
		case blind:
			if c.Result != string(contract.ResultUnknown) {
				t.Fatalf("checks[%d]: blind vantage forces result unknown, got %q", i, c.Result)
			}
			if len(c.Reasons) != 1 || c.Reasons[0] != string(contract.ReasonVantageBlind) {
				t.Fatalf("checks[%d]: blind check must carry only %q, got %v", i, contract.ReasonVantageBlind, c.Reasons)
			}
			severity = contract.SeverityRed
			reasons = []string{string(contract.ReasonVantageBlind)}
		case active(c):
			severity = contract.SeverityGreen
			reasons = append(reasons, string(contract.ReasonHeld))
		case c.Expected == string(contract.ExpectedDark):
			if c.Result == string(contract.ResultUp) {
				severity = contract.SeverityYellow
				reasons = append(reasons, string(contract.ReasonUnexpectedLight))
			} else {
				severity = contract.SeverityGreen
				reasons = append(reasons, string(contract.ReasonExpectedDark))
			}
		default:
			switch contract.Result(c.Result) {
			case contract.ResultUp:
				severity = contract.SeverityGreen
				for _, outcome := range outcomes {
					switch contract.Reason(outcome) {
					case contract.ReasonPromNotReady, contract.ReasonPromAuth, contract.ReasonMetricsBodyBad:
						severity = contract.SeverityYellow
					}
				}
			case contract.ResultDown:
				severity = contract.SeverityYellow
				reasons = append(reasons, string(contract.ReasonUnexpectedDark))
			case contract.ResultUnknown:
				severity = contract.SeverityYellow
				reasons = append(reasons, string(contract.ReasonProbeUnknown))
			default:
				t.Fatalf("checks[%d]: unknown result %q", i, c.Result)
			}
		}
		baseSeverity[key] = severity
		baseReasons[key] = reasons
	}

	// Pass two: the prometheus pair, over non-held, expected-light prom_up.
	var members []int
	for i, c := range doc.Checks {
		if c.Kind != string(contract.KindPromUp) || c.Expected != string(contract.ExpectedLight) || active(c) || blind {
			continue
		}
		members = append(members, i)
	}
	pairSeverity := map[int]contract.Severity{}
	pairCode := map[int]contract.Reason{}
	if len(members) >= 2 {
		var notUp []int
		for _, i := range members {
			if doc.Checks[i].Result != string(contract.ResultUp) {
				notUp = append(notUp, i)
			}
		}
		switch {
		case len(notUp) == len(members):
			for _, i := range notUp {
				pairSeverity[i] = contract.SeverityRed
				pairCode[i] = contract.ReasonPromPairDown
			}
		case len(notUp) > 0:
			for _, i := range notUp {
				pairSeverity[i] = contract.SeverityYellow
				pairCode[i] = contract.ReasonPromPairDegrade
			}
		}
		// Members that stayed up must not carry a pair code.
		for _, i := range members {
			if _, ok := pairCode[i]; !ok && hasReason(doc.Checks[i].Reasons, contract.ReasonPromPairDegrade) {
				t.Fatalf("checks[%d]: pair code on a member that is up", i)
			}
		}
	} else {
		for _, c := range doc.Checks {
			if hasReason(c.Reasons, contract.ReasonPromPairDown) || hasReason(c.Reasons, contract.ReasonPromPairDegrade) {
				t.Fatalf("check %s/%s carries a pair code with %d participating prom_up checks", c.Host, c.Kind, len(members))
			}
		}
	}

	// Compare each check against the recomputation.
	for i, c := range doc.Checks {
		key := c.Host + "\x00" + c.Kind + "\x00" + c.Target
		wantSeverity := baseSeverity[key]
		reasons := append([]string(nil), baseReasons[key]...)
		if s, ok := pairSeverity[i]; ok {
			wantSeverity = s
			reasons = append(reasons, string(pairCode[i]))
		}
		if contract.Severity(c.Severity) != wantSeverity {
			t.Fatalf("checks[%d] %s/%s: severity %s, rules give %s", i, c.Host, c.Kind, c.Severity, wantSeverity)
		}
		wantReasons := sortedCopy(dedupe(reasons))
		if strings.Join(c.Reasons, ",") != strings.Join(wantReasons, ",") {
			t.Fatalf("checks[%d] %s/%s: reasons %v, rules give %v", i, c.Host, c.Kind, c.Reasons, wantReasons)
		}
	}

	// Derivation: hosts, then overall.
	wantHosts := map[string]host{}
	var hostOrder []string
	for _, c := range doc.Checks {
		sev := contract.Severity(c.Severity)
		h, seen := wantHosts[c.Host]
		if !seen {
			h = host{ID: c.Host, Severity: string(sev)}
			hostOrder = append(hostOrder, c.Host)
			wantHosts[c.Host] = h
		} else if severityRank(sev) > severityRank(contract.Severity(h.Severity)) {
			h.Severity = string(sev)
			wantHosts[c.Host] = h
		}
	}
	for _, id := range hostOrder {
		h := wantHosts[id]
		var reasons []string
		for _, c := range doc.Checks {
			if c.Host != id || c.Severity != h.Severity {
				continue
			}
			reasons = append(reasons, c.Reasons...)
		}
		h.Reasons = dedupeInOrder(reasons)
		wantHosts[id] = h
	}
	if len(doc.Hosts) != len(hostOrder) {
		t.Fatalf("document has %d hosts, its checks name %d", len(doc.Hosts), len(hostOrder))
	}
	for i, got := range doc.Hosts {
		want, ok := wantHosts[got.ID]
		if !ok {
			t.Fatalf("hosts[%d]: %q has no check", i, got.ID)
		}
		if got.Severity != want.Severity {
			t.Fatalf("hosts[%d] %s: severity %s, checks give %s", i, got.ID, got.Severity, want.Severity)
		}
		if strings.Join(got.Reasons, ",") != strings.Join(want.Reasons, ",") {
			t.Fatalf("hosts[%d] %s: reasons %v, checks give %v", i, got.ID, got.Reasons, want.Reasons)
		}
	}
	overall := contract.SeverityGreen
	for _, h := range doc.Hosts {
		overall = worst(overall, contract.Severity(h.Severity))
	}
	if doc.Overall != string(overall) {
		t.Fatalf("overall %s, the worst host is %s", doc.Overall, overall)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func dedupeInOrder(in []string) []string {
	out := dedupe(in)
	if out == nil {
		return []string{}
	}
	return out
}
