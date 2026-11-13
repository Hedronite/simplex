package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v4"
)

// The Go loader and the JSON Schema must agree over a shared corpus:
// where the schema rejects, the loader rejects; where the schema accepts,
// the loader either accepts or rejects for a reason the schema cannot
// express (cross-field rules like canary resolution and uniqueness).
// The corpus pins both directions so neither side drifts alone.
//
// Each corpus case is a directory with input.yaml and a want file:
//
//	schema-reject  the schema rejects and so must the loader
//	loader-reject  the schema accepts but the loader rejects
//	both-accept    both accept with no findings

const (
	wantSchemaReject = "schema-reject"
	wantLoaderReject = "loader-reject"
	wantBothAccept   = "both-accept"
)

func compileConfigSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "schema", "simplex.config.v0.schema.json"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("file://"+path, doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile("file://" + path)
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	return sch
}

func TestConfigLoaderAgreesWithSchema(t *testing.T) {
	sch := compileConfigSchema(t)
	root := filepath.Join("testdata", "agreement")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the agreement corpus is empty")
	}
	clock := fixtureClock(t)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			dir := filepath.Join(root, e.Name())
			raw, err := os.ReadFile(filepath.Join(dir, "input.yaml"))
			if err != nil {
				t.Fatalf("read input.yaml: %v", err)
			}
			wantRaw, err := os.ReadFile(filepath.Join(dir, "want"))
			if err != nil {
				t.Fatalf("read want: %v", err)
			}
			want := strings.TrimSpace(string(wantRaw))

			// Loader side.
			_, findings, lerr := parse(raw, clock)
			loaderRejected := lerr != nil || hasErrors(findings)

			// Schema side. The YAML is converted to its JSON form; a
			// document that cannot survive the conversion (for example
			// duplicate mapping keys) can only be judged by the loader,
			// which must reject it.
			var anyValue any
			convErr := yaml.Unmarshal(raw, &anyValue)
			if convErr != nil {
				if want != wantLoaderReject {
					t.Fatalf("want %s, but the YAML does not convert to JSON: %v", want, convErr)
				}
				if !loaderRejected {
					t.Fatal("the loader accepted a document with duplicate keys")
				}
				return
			}
			docJSON, err := json.Marshal(anyValue)
			if err != nil {
				t.Fatalf("marshal to JSON: %v", err)
			}
			var docValue any
			if err := json.Unmarshal(docJSON, &docValue); err != nil {
				t.Fatalf("unmarshal JSON: %v", err)
			}
			schemaErr := sch.Validate(docValue)

			switch want {
			case wantSchemaReject:
				if schemaErr == nil {
					t.Error("want schema-reject, the schema accepted")
				}
				if !loaderRejected {
					t.Errorf("want schema-reject, the loader accepted (findings %+v)", findings)
				}
			case wantLoaderReject:
				if schemaErr != nil {
					t.Errorf("want loader-reject, but the schema rejected: %v", schemaErr)
				}
				if !loaderRejected {
					t.Error("want loader-reject, the loader accepted")
				}
			case wantBothAccept:
				if schemaErr != nil {
					t.Errorf("want both-accept, the schema rejected: %v", schemaErr)
				}
				if loaderRejected {
					t.Errorf("want both-accept, the loader rejected (findings %+v)", findings)
				}
				if lerr == nil && len(findings) != 0 {
					t.Errorf("want both-accept with no findings, got %+v", findings)
				}
			default:
				t.Fatalf("unknown want %q", want)
			}
		})
	}
}
