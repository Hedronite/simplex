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

// The reason set is frozen at exactly these 21 codes: the 16 of S0 and the five
// enrichment codes added at S3 (a minor change).
func TestReasonSetIsFrozen(t *testing.T) {
	want := []Reason{
		"expected_dark", "unexpected_light", "unexpected_dark", "held",
		"vantage_blind", "probe_unknown", "prom_not_ready", "prom_auth",
		"metrics_body_unexpected", "prom_pair_down", "prom_pair_degraded",
		"tcp_connect_ok", "tcp_connect_refused", "tcp_connect_timeout",
		"http_timeout", "http_unexpected_status",
		"ts_peer_online", "ts_peer_offline", "ts_peer_unmatched",
		"ts_path_direct", "ts_path_relayed",
	}
	got := map[Reason]bool{}
	for _, c := range ReasonCodes() {
		got[c] = true
	}
	if len(got) != len(want) {
		t.Fatalf("have %d reason codes, want %d", len(got), len(want))
	}
	for _, c := range want {
		if !got[c] {
			t.Errorf("missing reason code %q", c)
		}
	}
	for _, c := range []Reason{"watchdog_only", "probe_timeout"} {
		if got[c] {
			t.Errorf("reason code %q must not exist", c)
		}
	}
}
