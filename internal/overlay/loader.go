// Package overlay loads simplex.overlay/v0 documents — the private
// address overlay — and resolves the configured checks against it.
//
// The overlay is user data that is never tracked in git: public shape,
// private values (docs/semantics.md section 8). Load is strict the same
// way the config loader is, but its failures come back as a typed error
// that carries the same finding shape, so the validate command can print
// both files' findings through one printer.
//
// Resolve applies the per-(kind, target) overrides to the config's
// effective checks and returns, per check, the address to dial. A
// configured host with no overlay entry is a typed *MissingSlugsError
// naming every missing slug, which maps to exit 3.
package overlay

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Hedronite/simplex/internal/config"
	"github.com/Hedronite/simplex/internal/contract"
	"go.yaml.in/yaml/v4"
)

// Finding is the overlay's finding type. The overlay reports the same
// shape as the config loader, with the path rooted at the overlay file.
type Finding = config.Finding

// Level re-exports the finding levels.
type Level = config.Level

const (
	LevelWarning = config.LevelWarning
	LevelError   = config.LevelError
)

// Error is the typed failure Load returns when the overlay is invalid.
type Error struct {
	Findings []Finding
}

// Error summarises the first error-level finding.
func (e *Error) Error() string {
	if len(e.Findings) == 0 {
		return "invalid overlay"
	}
	f := e.Findings[0]
	if f.Path == "" {
		return f.Message
	}
	return f.Path + ": " + f.Message
}

// MissingSlugsError reports configured hosts that have no overlay entry.
type MissingSlugsError struct {
	Slugs []string
}

// Error names every missing slug, sorted.
func (e *MissingSlugsError) Error() string {
	return "no overlay entry for host(s): " + joinSlugs(e.Slugs)
}

func joinSlugs(slugs []string) string {
	out := ""
	for i, s := range slugs {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// Host is one host's private addressing. Address is what the TCP and HTTP
// probes dial; LANAddress is what a lan_reach check prefers when set.
type Host struct {
	Address    string
	LANAddress string
	Overrides  []Override
}

// Override replaces the port and path of the checks it matches: by kind,
// and by the check's config-effective target when one is given. Overrides
// apply in file order; the last match wins.
type Override struct {
	Kind    contract.Kind
	Target  string
	Port    int
	HasPort bool
	Path    string
	HasPath bool
}

// Overlay is a loaded simplex.overlay/v0 document, keyed by host slug.
type Overlay struct {
	Hosts map[string]Host
}

// Load reads the overlay file at path and parses it. A file that cannot
// be read is a config error: the checks that need an address cannot run.
// Findings ride on the returned *Error, so the validate command can print
// them without a second API.
func Load(path string) (*Overlay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Findings: []Finding{{
			Level:   LevelError,
			Path:    path,
			Message: "cannot read overlay: " + err.Error(),
		}}}
	}
	ov, _, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return ov, nil
}

// Parse parses overlay bytes directly: the byte-level core of Load, and
// what the fuzz targets and the other packages' tests drive.
func Parse(data []byte) (*Overlay, []Finding, error) {
	return parse(data)
}

// parse is the byte-level core the fuzz target calls directly.
func parse(data []byte) (*Overlay, []Finding, error) {
	var findings []Finding
	errAt := func(path string, line int, format string, args ...any) {
		findings = append(findings, Finding{Level: LevelError, Path: path, Line: line, Message: fmt.Sprintf(format, args...)})
	}
	fail := func() (*Overlay, []Finding, error) {
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

	ov := &Overlay{Hosts: map[string]Host{}}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i], top.Content[i+1]
		switch key.Value {
		case "apiVersion":
			// Checked after the loop, against the constant.
		case "hosts":
			parseHosts(ov, val, errAt)
		default:
			errAt(key.Value, key.Line, "unknown key %q", key.Value)
		}
	}

	// apiVersion must be present and equal to the constant. The loop
	// above skipped it; find it again for the line number.
	apiVersion := ""
	apiLine := top.Line
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "apiVersion" {
			apiVersion = parseString(top.Content[i+1], "apiVersion", errAt)
			apiLine = top.Content[i].Line
		}
	}
	if apiVersion == "" {
		errAt("apiVersion", apiLine, "required")
	} else if apiVersion != contract.OverlayAPIVersion {
		errAt("apiVersion", apiLine, "unsupported apiVersion %q (want %q)", apiVersion, contract.OverlayAPIVersion)
	}

	if hasErrors(findings) {
		return fail()
	}
	return ov, findings, nil
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

func parseHosts(ov *Overlay, n *yaml.Node, errAt errFunc) {
	n = deref(n)
	if n == nil || n.Tag != "!!map" {
		errAt("hosts", lineOf(n), "want a mapping, got %s", nodeDesc(n))
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := deref(n.Content[i]), n.Content[i+1]
		slug := key.Value
		path := fmt.Sprintf("hosts.%s", slug)
		if _, dup := ov.Hosts[slug]; dup {
			errAt(path, key.Line, "duplicate key %q", slug)
			continue
		}
		if !slugPattern.MatchString(slug) {
			errAt(path, key.Line, "not a slug (want ^[a-z][a-z0-9-]{0,31}$)")
		}
		var h Host
		item := deref(val)
		if item == nil || item.Tag != "!!map" {
			errAt(path, lineOf(item), "want a mapping, got %s", nodeDesc(item))
			ov.Hosts[slug] = h
			continue
		}
		seen := map[string]bool{}
		for j := 0; j+1 < len(item.Content); j += 2 {
			ikey, ival := item.Content[j], item.Content[j+1]
			name := ikey.Value
			fpath := path + "." + name
			if seen[name] {
				errAt(fpath, ikey.Line, "duplicate key %q", name)
				continue
			}
			seen[name] = true
			switch name {
			case "address":
				h.Address = parseString(ival, fpath, errAt)
				if h.Address == "" {
					errAt(fpath, ival.Line, "required")
				}
			case "lan_address":
				h.LANAddress = parseString(ival, fpath, errAt)
				if h.LANAddress == "" {
					errAt(fpath, ival.Line, "want a non-empty string")
				}
			case "overrides":
				h.Overrides = parseOverrides(ival, path, errAt)
			default:
				errAt(fpath, ikey.Line, "unknown key %q", name)
			}
		}
		if _, ok := seen["address"]; !ok {
			errAt(path+".address", item.Line, "required")
		}
		ov.Hosts[slug] = h
	}
}

func parseOverrides(n *yaml.Node, hostPath string, errAt errFunc) []Override {
	n = deref(n)
	path := hostPath + ".overrides"
	if n == nil || n.Tag == "!!null" {
		return nil
	}
	if n.Tag != "!!seq" {
		errAt(path, lineOf(n), "want a list, got %s", nodeDesc(n))
		return nil
	}
	var out []Override
	for i, item := range n.Content {
		item = deref(item)
		ipath := fmt.Sprintf("%s[%d]", path, i)
		if item == nil || item.Tag != "!!map" {
			errAt(ipath, lineOf(item), "want a mapping, got %s", nodeDesc(item))
			continue
		}
		var o Override
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
				o.Kind = contract.Kind(k)
				if k != "" && !isKnownKind(o.Kind) {
					errAt(fpath, val.Line, "unknown check kind %q", k)
				}
			case "target":
				o.Target = parseString(val, fpath, errAt)
				if o.Target != "" && !targetPattern.MatchString(o.Target) {
					errAt(fpath, val.Line, "want a target like :9090/-/ready, got %q", o.Target)
				}
			case "port":
				p, ok := parseInt(val, fpath, errAt)
				if ok {
					o.Port, o.HasPort = p, true
					if p < 1 || p > 65535 {
						errAt(fpath, val.Line, "port must be between 1 and 65535, got %d", p)
					} else if p == contract.DeniedPort {
						errAt(fpath, val.Line, "port %d is never dialed", contract.DeniedPort)
					}
				}
			case "path":
				p := parseString(val, fpath, errAt)
				if p != "" {
					if !isAllowedPath(p) {
						errAt(fpath, val.Line, "path %q is not on the allowlist (%s)", p, allowedList)
					}
					o.Path, o.HasPath = p, true
				}
			default:
				errAt(fpath, key.Line, "unknown key %q", name)
			}
		}
		if _, ok := seen["kind"]; !ok {
			errAt(ipath+".kind", item.Line, "required")
		}
		if o.Path != "" && o.Kind != "" && o.Kind != contract.KindPromUp {
			errAt(ipath+".path", item.Line, "path is legal only on a prom_up check, not on %q", o.Kind)
		}
		out = append(out, o)
	}
	return out
}

// Resolved is one configured check with its address to dial and the
// effective port, path and target after any overlay override.
type Resolved struct {
	Host     string
	Kind     contract.Kind
	Expected contract.Expected
	Target   string
	Address  string
	Port     int
	Path     string
}

// Resolve applies the overlay's overrides to the config's effective
// checks and returns one Resolved per check, in config file order.
//
// A configured host with no overlay entry fails closed: the returned
// error is a *MissingSlugsError naming every missing slug, sorted. A
// resolved port that equals the denied port is refused here as well as
// at load time, so a bug upstream cannot turn into a dial.
func (o *Overlay) Resolve(cfg *config.Config) ([]Resolved, error) {
	var missing []string
	for _, h := range cfg.Hosts {
		if _, ok := o.Hosts[h.ID]; !ok {
			missing = append(missing, h.ID)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &MissingSlugsError{Slugs: missing}
	}

	checks := cfg.EffectiveChecks()
	out := make([]Resolved, 0, len(checks))
	for _, c := range checks {
		entry := o.Hosts[c.Host]
		port, path := c.Port, c.Path
		cfgTarget := config.EffectiveTarget(c.Kind, port, path)
		for _, ov := range entry.Overrides {
			if ov.Kind != c.Kind {
				continue
			}
			if ov.Target != "" && ov.Target != cfgTarget {
				continue
			}
			if ov.HasPort {
				port = ov.Port
			}
			if ov.HasPath {
				path = ov.Path
			}
		}
		if port == contract.DeniedPort {
			return nil, &Error{Findings: []Finding{{
				Level:   LevelError,
				Path:    fmt.Sprintf("hosts.%s", c.Host),
				Message: fmt.Sprintf("check %s %s resolves to denied port %d", c.Kind, c.Target, contract.DeniedPort),
				Line:    c.Line,
			}}}
		}
		addr := entry.Address
		if c.Kind == contract.KindLANReach && entry.LANAddress != "" {
			addr = entry.LANAddress
		}
		out = append(out, Resolved{
			Host:     c.Host,
			Kind:     c.Kind,
			Expected: c.Expected,
			Target:   config.EffectiveTarget(c.Kind, port, path),
			Address:  addr,
			Port:     port,
			Path:     path,
		})
	}
	return out, nil
}

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var targetPattern = regexp.MustCompile(`^:[0-9]{1,5}(/[\x21-\x7e]*)?$`)

const allowedList = "/metrics, /-/ready, /-/healthy"

type errFunc func(path string, line int, format string, args ...any)

// deref follows alias nodes to their anchor.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return nil
		}
		n = n.Alias
	}
	return n
}

// documentRoot returns the document's content node, with aliases
// resolved, or nil for an empty document.
func documentRoot(root *yaml.Node) *yaml.Node {
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	return deref(root.Content[0])
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
// node is not a string. A YAML timestamp scalar counts as a string, the
// same way the config loader treats one.
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

// parseInt reads a scalar integer node, reporting a finding when the node
// is not an integer.
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

func lineOf(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line
}

// yamlErrorLine extracts a line number from a parser error, best effort.
func yamlErrorLine(err error) int {
	if e, ok := err.(interface{ Line() int }); ok {
		return e.Line()
	}
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

func isAllowedPath(p string) bool {
	for _, allowed := range contract.AllowedPaths {
		if p == allowed {
			return true
		}
	}
	return false
}

func isKnownKind(k contract.Kind) bool {
	for _, known := range contract.Kinds {
		if k == known {
			return true
		}
	}
	return false
}
