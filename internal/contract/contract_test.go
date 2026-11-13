package contract

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var snake = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestReasonCodesAreUniqueSnakeCase(t *testing.T) {
	seen := map[Reason]bool{}
	for _, c := range ReasonCodes() {
		if !snake.MatchString(string(c)) {
			t.Errorf("reason %q is not snake_case", c)
		}
		if seen[c] {
			t.Errorf("reason %q listed twice", c)
		}
		seen[c] = true
	}
}

func TestKindsAreUniqueSnakeCase(t *testing.T) {
	seen := map[Kind]bool{}
	for _, k := range Kinds {
		if !snake.MatchString(string(k)) {
			t.Errorf("kind %q is not snake_case", k)
		}
		if seen[k] {
			t.Errorf("kind %q listed twice", k)
		}
		seen[k] = true
	}
}

// The prose contract must list every code the Go package defines.
func TestSemanticsDocumentsEveryCode(t *testing.T) {
	raw, err := os.ReadFile("../../docs/semantics.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	check := func(kind, code string) {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("docs/semantics.md does not mention %s `%s`", kind, code)
		}
	}
	for _, c := range ReasonCodes() {
		check("reason", string(c))
	}
	for _, k := range Kinds {
		check("kind", string(k))
	}
}
