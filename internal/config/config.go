// Package config loads simplex.config/v0 documents strictly and reports
// what it found as findings with config line numbers.
//
// Load never mutates anything and never dials. It is the source of the
// findings the validate command prints (docs/semantics.md sections 10 and
// 12): every problem, not just the first one, with a level, a field path,
// a message and the 1-based config line it came from. When any error-level
// finding was recorded, Load also returns a typed *Error so main can map
// the run to exit 3.
//
// The rules follow docs/semantics.md sections 2, 7 and 8: strict decoding
// with unknown keys rejected, the apiVersion constant, expected required on
// every check, the path allowlist on prom_up only, the denied port, at
// least one host and one check per host, unique host and check identities,
// and canary references that must resolve to a configured ts_reach check
// by its effective target. Expired holds become warnings; the hold itself
// stays listed with Expired set so downstream code can filter it.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"go.yaml.in/yaml/v4"
)

// Level says how serious a finding is. The validate command prints both.
type Level string

const (
	// LevelWarning is a problem the run can survive: the config loads.
	LevelWarning Level = "warning"
	// LevelError makes the config invalid: Load returns a typed *Error.
	LevelError Level = "error"
)

// Finding is one thing the loader has to say about a config document.
// Path uses the document's field shape, indexes included, for example
// hosts[0].checks[1].path. Line is the 1-based config line, 0 when no
// line applies.
type Finding struct {
	Level   Level  `json:"level"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Line    int    `json:"line"`
}

// Error is the typed failure Load returns when the config is invalid.
type Error struct {
	Findings []Finding
}

// Error summarises the first error-level finding.
func (e *Error) Error() string {
	if len(e.Findings) == 0 {
		return "invalid config"
	}
	f := e.Findings[0]
	if f.Path == "" {
		return f.Message
	}
	return f.Path + ": " + f.Message
}

// Config is a loaded simplex.config/v0 document. Hosts and checks keep
// their file order; sorting into document order happens later, in the
// engine, per docs/semantics.md section 6.
type Config struct {
	APIVersion string
	Vantage    Vantage
	Hosts      []Host
	Holds      []Hold
	Tailscale  Tailscale
}

// Tailscale is the optional LocalAPI enrichment block. The zero value means
// enrichment is off.
type Tailscale struct {
	// LocalAPI turns enrichment on.
	LocalAPI bool
	// Socket is the absolute LocalAPI socket path; empty means the default.
	Socket string
	Line   int
}

// SocketPath returns the configured socket, or the default when none is set.
func (t Tailscale) SocketPath() string {
	if t.Socket != "" {
		return t.Socket
	}
	return contract.DefaultTailscaleSocket
}

// Vantage is the run's own identity and the checks that vouch for it.
type Vantage struct {
	ID       string
	Canaries []Canary
}

// Canary names a configured ts_reach check by its effective target.
type Canary struct {
	Host   string
	Kind   contract.Kind
	Target string
	Line   int
}

// Host is one configured host and its checks.
type Host struct {
	ID     string
	Checks []Check
	Line   int
}

// Check is one configured check. Port and Path are what the document
// said: zero and empty mean unset, the kind defaults apply.
type Check struct {
	Kind     contract.Kind
	Expected contract.Expected
	Port     int
	Path     string
	Line     int
}

// Hold is one announced, temporary dark. Host and Kind may be
// contract.Wildcard. Expired is true when the hold's Until is at or
// before the run's clock (a hold is live only while Until is strictly
// after it); expired holds are ignored downstream.
type Hold struct {
	Host    string
	Kind    contract.Kind
	Reason  string
	Until   time.Time
	Expired bool
	Line    int
}

// EffectiveCheck is one configured check with the kind defaults applied.
type EffectiveCheck struct {
	Host     string
	Kind     contract.Kind
	Expected contract.Expected
	Port     int
	Path     string
	Target   string
	Line     int
}

// DefaultPort is the kind's port when the config does not set one
// (docs/semantics.md section 2).
func DefaultPort(kind contract.Kind) int {
	switch kind {
	case contract.KindTSReach, contract.KindNodeExporter:
		return 9100
	case contract.KindPromUp:
		return 9090
	case contract.KindKubeAPIServer:
		return 6443
	case contract.KindLANReach, contract.KindSSH:
		return 22
	}
	return 0
}

// DefaultPath is the path an HTTP kind requests when nothing overrides it.
// Only the two HTTP kinds have one; the config may set a path on prom_up
// only.
func DefaultPath(kind contract.Kind) string {
	switch kind {
	case contract.KindNodeExporter:
		return contract.PathMetrics
	case contract.KindPromUp:
		return contract.PathReady
	}
	return ""
}

// EffectiveTarget is the document's target string for a check of this
// kind, port and path (docs/semantics.md section 2): a port for the TCP
// kinds, port plus /metrics for node_exporter, port plus path for
// prom_up.
func EffectiveTarget(kind contract.Kind, port int, path string) string {
	switch kind {
	case contract.KindNodeExporter:
		return fmt.Sprintf(":%d%s", port, contract.PathMetrics)
	case contract.KindPromUp:
		return fmt.Sprintf(":%d%s", port, path)
	default:
		return fmt.Sprintf(":%d", port)
	}
}

// EffectivePort is the check's port with the kind default applied.
func (c *Check) EffectivePort() int {
	if c.Port != 0 {
		return c.Port
	}
	return DefaultPort(c.Kind)
}

// EffectivePath is the check's path with the kind default applied.
func (c *Check) EffectivePath() string {
	if c.Path != "" {
		return c.Path
	}
	return DefaultPath(c.Kind)
}

// EffectiveTarget is the check's target with the kind defaults applied.
func (c *Check) EffectiveTarget() string {
	return EffectiveTarget(c.Kind, c.EffectivePort(), c.EffectivePath())
}

// EffectiveChecks lists every configured check with the kind defaults
// applied, in file order.
func (cfg *Config) EffectiveChecks() []EffectiveCheck {
	out := make([]EffectiveCheck, 0, 8)
	for _, h := range cfg.Hosts {
		for _, c := range h.Checks {
			out = append(out, EffectiveCheck{
				Host:     h.ID,
				Kind:     c.Kind,
				Expected: c.Expected,
				Port:     c.EffectivePort(),
				Path:     c.EffectivePath(),
				Target:   c.EffectiveTarget(),
				Line:     c.Line,
			})
		}
	}
	return out
}

// ActiveHolds lists the unexpired holds. Expired ones stay in Holds with
// Expired set; the loader has already warned about them.
func (cfg *Config) ActiveHolds() []Hold {
	out := make([]Hold, 0, len(cfg.Holds))
	for _, h := range cfg.Holds {
		if !h.Expired {
			out = append(out, h)
		}
	}
	return out
}

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var targetPattern = regexp.MustCompile(`^:[0-9]{1,5}(/[\x21-\x7e]*)?$`)

// Load reads the config file at path and parses it. A file that cannot be
// read is a config error (exit 3), not a runtime error: the run cannot
// start without it.
func Load(path string) (*Config, []Finding, error) {
	return LoadWithClock(path, nil)
}

// LoadWithClock loads the config against the given clock. now is called
// for hold expiry; nil means time.Now.
func LoadWithClock(path string, now func() time.Time) (*Config, []Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		f := []Finding{{Level: LevelError, Path: path, Line: 0, Message: "cannot read config: " + err.Error()}}
		return nil, f, &Error{Findings: f}
	}
	return parse(data, now)
}

// Parse parses config bytes directly: the byte-level core of Load, and
// what the fuzz targets and the other packages' tests drive.
func Parse(data []byte, now func() time.Time) (*Config, []Finding, error) {
	return parse(data, now)
}

// parse is the byte-level core the fuzz targets call directly.
func parse(data []byte, now func() time.Time) (*Config, []Finding, error) {
	if now == nil {
		now = time.Now
	}
	var findings []Finding
	errAt := func(path string, line int, format string, args ...any) {
		findings = append(findings, Finding{Level: LevelError, Path: path, Line: line, Message: fmt.Sprintf(format, args...)})
	}
	warnAt := func(path string, line int, format string, args ...any) {
		findings = append(findings, Finding{Level: LevelWarning, Path: path, Line: line, Message: fmt.Sprintf(format, args...)})
	}
	fail := func() (*Config, []Finding, error) {
		return nil, findings, newError(findings)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		errAt("", yamlErrorLine(err), "not valid YAML: %v", err)
		return fail()
	}
	top := documentRoot(&root)
	if top == nil {
		errAt("", 1, "empty document")
		return fail()
	}
	if top.Tag != "!!map" {
		errAt("", top.Line, "want a top-level mapping, got %s", nodeDesc(top))
		return fail()
	}

	cfg := &Config{}

	// First pass, in file order: split the top-level pairs into known
	// and unknown. Unknown keys are errors immediately; known keys are
	// parsed in file order so findings read the way the file does.
	var vantageNode, hostsNode, holdsNode, tailscaleNode *yaml.Node
	var vantageKey, hostsKey, holdsKey, tailscaleKey int
	seen := map[string]bool{}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i], top.Content[i+1]
		name := key.Value
		if seen[name] {
			errAt(name, key.Line, "duplicate key %q", name)
			continue
		}
		seen[name] = true
		switch name {
		case "apiVersion":
			cfg.APIVersion = parseString(val, name, errAt)
		case "vantage":
			vantageNode, vantageKey = val, key.Line
		case "hosts":
			hostsNode, hostsKey = val, key.Line
		case "holds":
			holdsNode, holdsKey = val, key.Line
		case "tailscale":
			tailscaleNode, tailscaleKey = val, key.Line
		default:
			errAt(name, key.Line, "unknown key %q", name)
		}
	}

	if cfg.APIVersion == "" {
		errAt("apiVersion", top.Line, "required")
	} else if cfg.APIVersion != contract.ConfigAPIVersion {
		errAt("apiVersion", top.Line, "unsupported apiVersion %q (want %q)", cfg.APIVersion, contract.ConfigAPIVersion)
	}

	if vantageNode == nil {
		errAt("vantage", top.Line, "required")
	} else {
		cfg.Vantage = parseVantage(vantageNode, vantageKey, errAt)
	}

	if hostsNode == nil {
		errAt("hosts", top.Line, "required")
	} else {
		cfg.Hosts = parseHosts(hostsNode, hostsKey, errAt)
	}

	if holdsNode != nil {
		cfg.Holds = parseHolds(holdsNode, holdsKey, now, errAt, warnAt)
	}

	if tailscaleNode != nil {
		cfg.Tailscale = parseTailscale(tailscaleNode, tailscaleKey, errAt)
	}

	if fs := validateRefs(cfg); len(fs) > 0 {
		findings = append(findings, fs...)
	}
	if hasErrors(findings) {
		return fail()
	}
	return cfg, findings, nil
}

func newError(findings []Finding) *Error {
	var errs []Finding
	for _, f := range findings {
		if f.Level == LevelError {
			errs = append(errs, f)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &Error{Findings: errs}
}

func hasErrors(findings []Finding) bool {
	for _, f := range findings {
		if f.Level == LevelError {
			return true
		}
	}
	return false
}

// documentRoot returns the document's content node, with aliases
// resolved, or nil for an empty document.
func documentRoot(root *yaml.Node) *yaml.Node {
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	return deref(root.Content[0])
}

// deref follows alias nodes to their anchor. A chain that never lands is
// treated as a null scalar by the parser; deref stops on nil.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return nil
		}
		n = n.Alias
	}
	return n
}

func nodeDesc(n *yaml.Node) string {
	if n == nil {
		return "nothing"
	}
	switch n.Tag {
	case "!!str":
		return "a string"
	case "!!int":
		return "an integer"
	case "!!float":
		return "a float"
	case "!!bool":
		return "a boolean"
	case "!!null":
		return "an empty value"
	case "!!seq":
		return "a list"
	case "!!map":
		return "a mapping"
	}
	return "a " + n.Tag
}

// parseString reads a scalar string node, reporting a finding when the
// node is not a string. A YAML timestamp scalar counts as a string: the
// schema sees it as the JSON string it round-trips to, and only the
// RFC 3339 parses get kept.
func parseString(n *yaml.Node, path string, errAt errFunc) string {
	n = deref(n)
	if n == nil {
		errAt(path, 0, "required")
		return ""
	}
	if n.Tag != "!!str" && n.Tag != "!!timestamp" {
		errAt(path, n.Line, "want a string, got %s", nodeDesc(n))
		return ""
	}
	return n.Value
}

type errFunc func(path string, line int, format string, args ...any)

func yamlErrorLine(err error) int {
	if e, ok := err.(interface{ Line() int }); ok {
		return e.Line()
	}
	// The parser embeds the line in the message; best effort.
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, "line "); ok {
		digits := after
		if end := strings.IndexAny(after, ":, "); end >= 0 {
			digits = after[:end]
		}
		if n, e2 := strconv.Atoi(digits); e2 == nil {
			return n
		}
	}
	return 0
}

func parseVantage(n *yaml.Node, line int, errAt errFunc) Vantage {
	var v Vantage
	n = deref(n)
	if n == nil || n.Tag != "!!map" {
		errAt("vantage", line, "want a mapping, got %s", nodeDesc(n))
		return v
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		name := key.Value
		if seen[name] {
			errAt("vantage."+name, key.Line, "duplicate key %q", name)
			continue
		}
		seen[name] = true
		switch name {
		case "id":
			v.ID = parseString(val, "vantage.id", errAt)
			if v.ID != "" && !slugPattern.MatchString(v.ID) {
				errAt("vantage.id", val.Line, "not a slug (want ^[a-z][a-z0-9-]{0,31}$)")
			}
		case "canaries":
			v.Canaries = parseCanaries(val, errAt)
		default:
			errAt("vantage."+name, key.Line, "unknown key %q", name)
		}
	}
	if v.ID == "" {
		errAt("vantage.id", line, "required")
	}
	return v
}

func parseCanaries(n *yaml.Node, errAt errFunc) []Canary {
	n = deref(n)
	if n == nil || n.Tag == "!!null" {
		return nil
	}
	if n.Tag != "!!seq" {
		errAt("vantage.canaries", n.Line, "want a list, got %s", nodeDesc(n))
		return nil
	}
	var out []Canary
	for i, item := range n.Content {
		item = deref(item)
		path := fmt.Sprintf("vantage.canaries[%d]", i)
		if item == nil || item.Tag != "!!map" {
			errAt(path, lineOf(item), "want a mapping, got %s", nodeDesc(item))
			continue
		}
		var c Canary
		seen := map[string]bool{}
		for j := 0; j+1 < len(item.Content); j += 2 {
			key, val := item.Content[j], item.Content[j+1]
			name := key.Value
			fpath := path + "." + name
			if seen[name] {
				errAt(fpath, key.Line, "duplicate key %q", name)
				continue
			}
			seen[name] = true
			switch name {
			case "host":
				c.Host = parseString(val, fpath, errAt)
				if c.Host != "" && !slugPattern.MatchString(c.Host) {
					errAt(fpath, val.Line, "not a slug (want ^[a-z][a-z0-9-]{0,31}$)")
				}
			case "kind":
				k := parseString(val, fpath, errAt)
				c.Kind = contract.Kind(k)
				if k != "" && k != string(contract.KindTSReach) {
					errAt(fpath, val.Line, "a canary must be a ts_reach check, got %q", k)
				}
			case "target":
				c.Target = parseString(val, fpath, errAt)
				if c.Target != "" && !targetPattern.MatchString(c.Target) {
					errAt(fpath, val.Line, "want a target like :9100, got %q", c.Target)
				}
			default:
				errAt(fpath, key.Line, "unknown key %q", name)
			}
		}
		c.Line = item.Line
		for _, f := range []string{"host", "kind", "target"} {
			if !seen[f] {
				errAt(path+"."+f, item.Line, "required")
			}
		}
		out = append(out, c)
	}
	return out
}

func parseHosts(n *yaml.Node, line int, errAt errFunc) []Host {
	n = deref(n)
	if n == nil || n.Tag != "!!seq" {
		errAt("hosts", line, "want a list, got %s", nodeDesc(n))
		return nil
	}
	if len(n.Content) == 0 {
		errAt("hosts", line, "need at least one host")
		return nil
	}
	out := make([]Host, 0, len(n.Content))
	for i, item := range n.Content {
		item = deref(item)
		path := fmt.Sprintf("hosts[%d]", i)
		if item == nil || item.Tag != "!!map" {
			errAt(path, lineOf(item), "want a mapping, got %s", nodeDesc(item))
			continue
		}
		var h Host
		seen := map[string]bool{}
		for j := 0; j+1 < len(item.Content); j += 2 {
			key, val := item.Content[j], item.Content[j+1]
			name := key.Value
			fpath := path + "." + name
			if seen[name] {
				errAt(fpath, key.Line, "duplicate key %q", name)
				continue
			}
			seen[name] = true
			switch name {
			case "id":
				h.ID = parseString(val, fpath, errAt)
				if h.ID != "" && !slugPattern.MatchString(h.ID) {
					errAt(fpath, val.Line, "not a slug (want ^[a-z][a-z0-9-]{0,31}$)")
				}
			case "checks":
				h.Checks = parseChecks(val, path, errAt)
			default:
				errAt(fpath, key.Line, "unknown key %q", name)
			}
		}
		h.Line = item.Line
		for _, f := range []string{"id", "checks"} {
			if !seen[f] {
				errAt(path+"."+f, item.Line, "required")
			}
		}
		out = append(out, h)
	}
	return out
}

func parseChecks(n *yaml.Node, hostPath string, errAt errFunc) []Check {
	n = deref(n)
	path := hostPath + ".checks"
	if n == nil || n.Tag != "!!seq" {
		errAt(path, lineOf(n), "want a list, got %s", nodeDesc(n))
		return nil
	}
	if len(n.Content) == 0 {
		errAt(path, lineOf(n), "need at least one check on every host")
		return nil
	}
	out := make([]Check, 0, len(n.Content))
	for i, item := range n.Content {
		item = deref(item)
		ipath := fmt.Sprintf("%s[%d]", path, i)
		if item == nil || item.Tag != "!!map" {
			errAt(ipath, lineOf(item), "want a mapping, got %s", nodeDesc(item))
			continue
		}
		var c Check
		seen := map[string]bool{}
		for j := 0; j+1 < len(item.Content); j += 2 {
			key, val := item.Content[j], item.Content[j+1]
			name := key.Value
			fpath := ipath + "." + name
			if seen[name] {
				errAt(fpath, key.Line, "duplicate key %q", name)
				continue
			}
			seen[name] = true
			switch name {
			case "kind":
				k := parseString(val, fpath, errAt)
				c.Kind = contract.Kind(k)
				if k != "" && !isKnownKind(c.Kind) {
					errAt(fpath, val.Line, "unknown check kind %q", k)
				}
			case "expected":
				e := parseString(val, fpath, errAt)
				c.Expected = contract.Expected(e)
				if e != "" && e != string(contract.ExpectedLight) && e != string(contract.ExpectedDark) {
					errAt(fpath, val.Line, "want %q or %q, got %q", contract.ExpectedLight, contract.ExpectedDark, e)
				}
			case "port":
				p, ok := parseInt(val, fpath, errAt)
				c.Port = p
				if ok {
					if p < 1 || p > 65535 {
						errAt(fpath, val.Line, "port must be between 1 and 65535, got %d", p)
					} else if p == contract.DeniedPort {
						errAt(fpath, val.Line, "port %d is never dialed", contract.DeniedPort)
					}
				}
			case "path":
				c.Path = parseString(val, fpath, errAt)
				if c.Path != "" {
					if !isAllowedPath(c.Path) {
						errAt(fpath, val.Line, "path %q is not on the allowlist (%s)", c.Path, strings.Join(contract.AllowedPaths, ", "))
					}
				}
			default:
				errAt(fpath, key.Line, "unknown key %q", name)
			}
		}
		c.Line = item.Line
		for _, f := range []string{"kind", "expected"} {
			if !seen[f] {
				errAt(ipath+"."+f, item.Line, "required")
			}
		}
		if c.Path != "" && c.Kind != "" && c.Kind != contract.KindPromUp {
			errAt(ipath+".path", c.Line, "path is legal only on a prom_up check, not on %q", c.Kind)
		}
		out = append(out, c)
	}
	return out
}

func parseHolds(n *yaml.Node, line int, now func() time.Time, errAt, warnAt errFunc) []Hold {
	n = deref(n)
	if n == nil || n.Tag == "!!null" {
		return nil
	}
	if n.Tag != "!!seq" {
		errAt("holds", line, "want a list, got %s", nodeDesc(n))
		return nil
	}
	out := make([]Hold, 0, len(n.Content))
	for i, item := range n.Content {
		item = deref(item)
		path := fmt.Sprintf("holds[%d]", i)
		if item == nil || item.Tag != "!!map" {
			errAt(path, lineOf(item), "want a mapping, got %s", nodeDesc(item))
			continue
		}
		var h Hold
		seen := map[string]bool{}
		for j := 0; j+1 < len(item.Content); j += 2 {
			key, val := item.Content[j], item.Content[j+1]
			name := key.Value
			fpath := path + "." + name
			if seen[name] {
				errAt(fpath, key.Line, "duplicate key %q", name)
				continue
			}
			seen[name] = true
			switch name {
			case "host":
				h.Host = parseString(val, fpath, errAt)
				if h.Host != "" && h.Host != contract.Wildcard && !slugPattern.MatchString(h.Host) {
					errAt(fpath, val.Line, "want a slug or %q, got %q", contract.Wildcard, h.Host)
				}
			case "kind":
				k := parseString(val, fpath, errAt)
				h.Kind = contract.Kind(k)
				if k != "" && k != contract.Wildcard && !isKnownKind(h.Kind) {
					errAt(fpath, val.Line, "unknown check kind %q", k)
				}
			case "reason":
				h.Reason = parseString(val, fpath, errAt)
				if h.Reason == "" {
					errAt(fpath, val.Line, "required")
				}
			case "until":
				s := parseString(val, fpath, errAt)
				if s == "" {
					continue
				}
				t, err := time.Parse(time.RFC3339, s)
				if err != nil {
					errAt(fpath, val.Line, "not an RFC 3339 timestamp: %v", err)
					continue
				}
				h.Until = t
			default:
				errAt(fpath, key.Line, "unknown key %q", name)
			}
		}
		h.Line = item.Line
		for _, f := range []string{"host", "kind", "reason", "until"} {
			if !seen[f] {
				errAt(path+"."+f, item.Line, "required")
			}
		}
		// A hold is live only while until is strictly after the run
		// clock (docs/semantics.md section 5): until == now is expired.
		if !h.Until.IsZero() && !h.Until.After(now()) {
			h.Expired = true
			warnAt(path+".until", item.Line, "hold expired at %s; ignored", h.Until.Format(time.RFC3339))
		}
		out = append(out, h)
	}
	return out
}

// validateRefs checks the cross-field rules over a parsed config: unique
// host ids, unique check identities inside a host, and canary references
// that resolve to a configured ts_reach check by its effective target.
// It is a separate step so the fuzz targets can exercise it in isolation.
func validateRefs(cfg *Config) []Finding {
	var findings []Finding
	errAt := func(path string, line int, format string, args ...any) {
		findings = append(findings, Finding{Level: LevelError, Path: path, Line: line, Message: fmt.Sprintf(format, args...)})
	}

	hostByID := make(map[string]int, len(cfg.Hosts))
	for i, h := range cfg.Hosts {
		if h.ID == "" {
			continue
		}
		if _, dup := hostByID[h.ID]; dup {
			errAt(fmt.Sprintf("hosts[%d].id", i), h.Line, "duplicate host id %q", h.ID)
			continue
		}
		hostByID[h.ID] = i
	}
	for i, h := range cfg.Hosts {
		seen := map[string]bool{}
		for j, c := range h.Checks {
			if c.Kind == "" {
				continue
			}
			id := string(c.Kind) + " " + c.EffectiveTarget()
			if seen[id] {
				errAt(fmt.Sprintf("hosts[%d].checks[%d]", i, j), c.Line,
					"duplicate check %s %s on host %q", c.Kind, c.EffectiveTarget(), h.ID)
				continue
			}
			seen[id] = true
		}
	}
	seenCanaries := map[string]bool{}
	for i, c := range cfg.Vantage.Canaries {
		key := c.Host + " " + string(c.Kind) + " " + c.Target
		if seenCanaries[key] {
			errAt(fmt.Sprintf("vantage.canaries[%d]", i), c.Line, "duplicate canary %q", key)
			continue
		}
		seenCanaries[key] = true
		hi, ok := hostByID[c.Host]
		if !ok {
			errAt(fmt.Sprintf("vantage.canaries[%d].host", i), c.Line,
				"canary host %q is not configured", c.Host)
			continue
		}
		found := false
		for _, chk := range cfg.Hosts[hi].Checks {
			if chk.Kind == contract.KindTSReach && chk.EffectiveTarget() == c.Target {
				found = true
				break
			}
		}
		if !found {
			errAt(fmt.Sprintf("vantage.canaries[%d].target", i), c.Line,
				"no ts_reach check on host %q answers %q", c.Host, c.Target)
		}
	}
	return findings
}

func parseInt(n *yaml.Node, path string, errAt errFunc) (int, bool) {
	n = deref(n)
	if n == nil {
		errAt(path, 0, "required")
		return 0, false
	}
	if n.Tag != "!!int" {
		errAt(path, n.Line, "want an integer, got %s", nodeDesc(n))
		return 0, false
	}
	p, err := strconv.Atoi(n.Value)
	if err != nil {
		errAt(path, n.Line, "not an integer: %v", err)
		return 0, false
	}
	return p, true
}

func isKnownKind(k contract.Kind) bool {
	for _, known := range contract.Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// isAllowedPath reports whether p is one of the three HTTP paths a check
// may request (docs/semantics.md section 2).
func isAllowedPath(p string) bool {
	for _, allowed := range contract.AllowedPaths {
		if p == allowed {
			return true
		}
	}
	return false
}

func lineOf(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line
}

// parseTailscale reads the optional enrichment block. The socket must be an
// absolute path: a host:port, a URL, a relative path or "//..." is an error, and
// the error does not repeat the value.
func parseTailscale(n *yaml.Node, line int, errAt errFunc) Tailscale {
	var t Tailscale
	n = deref(n)
	if n == nil || n.Tag != "!!map" {
		errAt("tailscale", line, "want a mapping, got %s", nodeDesc(n))
		return t
	}
	t.Line = n.Line
	seen := map[string]bool{}
	defer func() {
		if !seen["localapi"] {
			errAt("tailscale.localapi", n.Line, "required when the tailscale block is present")
		}
	}()
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		name := key.Value
		fpath := "tailscale." + name
		if seen[name] {
			errAt(fpath, key.Line, "duplicate key %q", name)
			continue
		}
		seen[name] = true
		switch name {
		case "localapi":
			v := deref(val)
			if v == nil || v.Tag != "!!bool" {
				errAt(fpath, key.Line, "want true or false, got %s", nodeDesc(v))
				continue
			}
			t.LocalAPI = v.Value == "true"
		case "socket":
			t.Socket = parseString(val, fpath, errAt)
			if t.Socket != "" && !absoluteSocketPath(t.Socket) {
				errAt(fpath, key.Line, "want an absolute path to the socket (a host:port, a URL or a relative path is not allowed)")
			}
		default:
			errAt(fpath, key.Line, "unknown key %q", name)
		}
	}
	return t
}

// absoluteSocketPath reports whether p is an absolute filesystem path: it starts
// with a single slash, names something below the root, and has no whitespace.
func absoluteSocketPath(p string) bool {
	if len(p) < 2 || p[0] != '/' || p[1] == '/' {
		return false
	}
	return !strings.ContainsAny(p, " \t\r\n")
}
