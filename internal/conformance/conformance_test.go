// Package conformance holds the S0 checks that keep the three JSON Schemas,
// the Go vocabulary in internal/contract and docs/semantics.md in agreement.
//
// The schemas are hand-written, so drift is the failure mode to design out:
// every enum is compared against the Go slice it must equal, and every
// document shape is exercised in both directions (a valid document validates,
// a doc with one forbidden thing does not).
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Hedronite/simplex/internal/contract"
)

// schemaFiles maps a short name to the checked-in schema.
var schemaFiles = map[string]string{
	"probe":   "simplex.probe.v0.schema.json",
	"config":  "simplex.config.v0.schema.json",
	"overlay": "simplex.overlay.v0.schema.json",
}

func schemaPath(t *testing.T, kind string) string {
	t.Helper()
	name, ok := schemaFiles[kind]
	if !ok {
		t.Fatalf("unknown schema kind %q", kind)
	}
	return filepath.Join("..", "..", "schema", name)
}

// compileSchema compiles one checked-in schema with the test-only dependency.
func compileSchema(t *testing.T, kind string) *jsonschema.Schema {
	t.Helper()
	path := schemaPath(t, kind)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("%s: not JSON: %v", path, err)
	}
	url := "file://" + path
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("%s: add resource: %v", path, err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("%s: does not compile as JSON Schema: %v", path, err)
	}
	return sch
}

func loadSchemaJSON(t *testing.T, kind string) any {
	t.Helper()
	raw, err := os.ReadFile(schemaPath(t, kind))
	if err != nil {
		t.Fatalf("read schema %s: %v", kind, err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", schemaFiles[kind], err)
	}
	return v
}

// enumAt returns the enum values at a JSON pointer such as
// /properties/checks/items/properties/kind.
func enumAt(t *testing.T, kind, pointer string) []string {
	t.Helper()
	node := loadSchemaJSON(t, kind)
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s%s: %q is not an object", kind, pointer, part)
		}
		node, ok = m[part]
		if !ok {
			t.Fatalf("%s%s: no such key %q", kind, pointer, part)
		}
	}
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s%s: not an object", kind, pointer)
	}
	raw, ok := m["enum"].([]any)
	if !ok {
		t.Fatalf("%s%s: no enum", kind, pointer)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s%s: non-string enum member %v", kind, pointer, v)
		}
		out = append(out, s)
	}
	return out
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestSchemaEnumsEqualGoVocabulary is the drift guard. Each schema enum must
// equal the internal/contract slice exactly, in both directions.
func TestSchemaEnumsEqualGoVocabulary(t *testing.T) {
	promPaths := contract.AllowedPaths
	cases := []struct {
		name    string
		kind    string
		pointer string
		want    []string
	}{
		{"probe overall", "probe", "/properties/overall", strs(contract.Severities)},
		{"probe vantage status", "probe", "/properties/vantage/properties/status", strs(contract.VantageStatuses)},
		{"probe host severity", "probe", "/properties/hosts/items/properties/severity", strs(contract.Severities)},
		{"probe host reasons", "probe", "/properties/hosts/items/properties/reasons/items", strs(contract.ReasonCodes())},
		{"probe check kind", "probe", "/properties/checks/items/properties/kind", strs(contract.Kinds)},
		{"probe check result", "probe", "/properties/checks/items/properties/result", strs(contract.Results)},
		{"probe check severity", "probe", "/properties/checks/items/properties/severity", strs(contract.Severities)},
		{"probe check expected", "probe", "/properties/checks/items/properties/expected", strs(contract.ExpectedValues)},
		{"probe check reasons", "probe", "/properties/checks/items/properties/reasons/items", strs(contract.ReasonCodes())},
		{"config check kind", "config", "/properties/hosts/items/properties/checks/items/properties/kind", strs(contract.Kinds)},
		{"config check expected", "config", "/properties/hosts/items/properties/checks/items/properties/expected", strs(contract.ExpectedValues)},
		{"config check path", "config", "/properties/hosts/items/properties/checks/items/properties/path", promPaths},
		{"config canary kind", "config", "/properties/vantage/properties/canaries/items/properties/kind", []string{string(contract.KindTSReach)}},
		{"overlay override kind", "overlay", "/properties/hosts/additionalProperties/properties/overrides/items/properties/kind", strs(contract.Kinds)},
		{"overlay override path", "overlay", "/properties/hosts/additionalProperties/properties/overrides/items/properties/path", promPaths},
	}
	for _, tc := range cases {
		got := enumAt(t, tc.kind, tc.pointer)
		if strings.Join(sorted(got), ",") != strings.Join(sorted(tc.want), ",") {
			t.Errorf("%s: %s enum = %v, want %v", tc.name, tc.pointer, got, tc.want)
		}
	}

	// The hold kind enum is every kind plus the wildcard.
	holdKinds := enumAt(t, "config", "/properties/holds/items/properties/kind")
	wantHold := append(strs(contract.Kinds), contract.Wildcard)
	if strings.Join(sorted(holdKinds), ",") != strings.Join(sorted(wantHold), ",") {
		t.Errorf("config hold kind enum = %v, want %v", holdKinds, wantHold)
	}
}

// TestValidDocuments validates every positive document under testdata/valid.
// The file name prefix selects the schema.
func TestValidDocuments(t *testing.T) {
	dir := filepath.Join("testdata", "valid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	seen := 0
	for _, entry := range entries {
		kind := strings.SplitN(entry.Name(), ".", 2)[0]
		if _, ok := schemaFiles[kind]; !ok {
			t.Fatalf("%s: file name must start with probe, config or overlay", entry.Name())
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			doc := readJSONFile(t, filepath.Join(dir, entry.Name()))
			if err := compileSchema(t, kind).Validate(doc); err != nil {
				t.Fatalf("valid document rejected by the %s schema: %v", kind, err)
			}
		})
	}
	if seen == 0 {
		t.Fatal("testdata/valid holds no documents")
	}
}

// TestNegativeDocuments asserts that every file under testdata/negative is
// rejected by the schema its name prefix selects. One forbidden thing per
// file, so a rejection names the rule that fired.
func TestNegativeDocuments(t *testing.T) {
	dir := filepath.Join("testdata", "negative")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	seen := 0
	for _, entry := range entries {
		parts := strings.SplitN(strings.TrimSuffix(entry.Name(), ".json"), "__", 2)
		if len(parts) != 2 {
			t.Fatalf("%s: name must be <kind>__<case>.json", entry.Name())
		}
		kind := parts[0]
		if _, ok := schemaFiles[kind]; !ok {
			t.Fatalf("%s: unknown schema prefix %q", entry.Name(), kind)
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			doc := readJSONFile(t, filepath.Join(dir, entry.Name()))
			if err := compileSchema(t, kind).Validate(doc); err == nil {
				t.Fatalf("invalid document accepted by the %s schema", kind)
			}
		})
	}
	if seen == 0 {
		t.Fatal("testdata/negative holds no documents")
	}
}

func readJSONFile(t *testing.T, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("%s: not JSON: %v", path, err)
	}
	return doc
}

// TestProbeDocumentAllowsEmptyReasonArrays pins that a healthy check and a
// fully GREEN host carry no reason, rather than a placeholder string.
func TestProbeDocumentAllowsEmptyReasonArrays(t *testing.T) {
	sch := compileSchema(t, "probe")
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(`{
  "apiVersion": "simplex.probe/v0",
  "generated_at": "2026-11-13T18:00:00Z",
  "vantage": {"id": "lathe", "status": "ok"},
  "overall": "GREEN",
  "hosts": [{"id": "tower", "severity": "GREEN", "reasons": []}],
  "checks": [{"host": "tower", "kind": "ts_reach", "target": ":9100", "result": "up",
              "severity": "GREEN", "expected": "light", "reasons": []}]
}`))
	if err != nil {
		t.Fatalf("document is not JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("GREEN document with empty reasons rejected: %v", err)
	}
}

// TestBlindDocumentShape pins the Q3 ruling: blind is RED everywhere.
func TestBlindDocumentShape(t *testing.T) {
	sch := compileSchema(t, "probe")
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(`{
  "apiVersion": "simplex.probe/v0",
  "generated_at": "2026-11-13T18:00:00Z",
  "vantage": {"id": "lathe", "status": "blind"},
  "overall": "RED",
  "hosts": [{"id": "tower", "severity": "RED", "reasons": ["vantage_blind"]}],
  "checks": [{"host": "tower", "kind": "ts_reach", "target": ":9100", "result": "unknown",
              "severity": "RED", "expected": "light", "reasons": ["vantage_blind"]}]
}`))
	if err != nil {
		t.Fatalf("document is not JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("blind document rejected: %v", err)
	}
}

// TestEverySchemaEnumValueIsSnakeOrKnown keeps a typo like "Yellow" out.
func TestEverySchemaEnumValueIsSnakeOrKnown(t *testing.T) {
	for kind := range schemaFiles {
		var walk func(node any, path string)
		walk = func(node any, path string) {
			switch x := node.(type) {
			case map[string]any:
				if raw, ok := x["enum"].([]any); ok {
					for _, v := range raw {
						s, _ := v.(string)
						if s == "" {
							t.Errorf("%s%s: empty enum member", kind, path)
						}
					}
				}
				for k, v := range x {
					walk(v, fmt.Sprintf("%s/%s", path, k))
				}
			case []any:
				for i, v := range x {
					walk(v, fmt.Sprintf("%s/%d", path, i))
				}
			}
		}
		walk(loadSchemaJSON(t, kind), "")
	}
}
