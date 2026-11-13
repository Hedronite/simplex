// Package contract holds the frozen vocabulary of the simplex.probe/v0
// document and the simplex.config/v0 file. docs/semantics.md is the
// prose contract; the schemas under schema/ must agree with the slices
// in this file, and tests assert that.
package contract

// APIVersion identifiers for the three file formats.
const (
	ProbeAPIVersion   = "simplex.probe/v0"
	ConfigAPIVersion  = "simplex.config/v0"
	OverlayAPIVersion = "simplex.overlay/v0"
)

// Wildcard matches any host slug or any kind in a hold.
const Wildcard = "*"

// The only paths an HTTP check may request. The config and the overlay both
// take them from this allowlist, and the probe schema pins the same three.
const (
	PathMetrics = "/metrics"
	PathReady   = "/-/ready"
	PathHealthy = "/-/healthy"
)

// AllowedPaths lists every path an HTTP check may name, in documentation order.
var AllowedPaths = []string{PathMetrics, PathReady, PathHealthy}

// MetricsListen is the default address `serve` binds: loopback only. It is
// defined once; the docs and the --help text quote it from here.
const MetricsListen = "127.0.0.1:10081"

// DeniedPort is never dialed, at config load or in the dialer.
const DeniedPort = 18080

// Kind is a check kind. The enum is open: adding a kind is a minor change.
type Kind string

const (
	KindTSReach       Kind = "ts_reach"
	KindLANReach      Kind = "lan_reach"
	KindNodeExporter  Kind = "node_exporter"
	KindPromUp        Kind = "prom_up"
	KindKubeAPIServer Kind = "kube_apiserver"
	KindSSH           Kind = "ssh"
)

// Kinds lists every check kind in documentation order.
var Kinds = []Kind{
	KindTSReach,
	KindLANReach,
	KindNodeExporter,
	KindPromUp,
	KindKubeAPIServer,
	KindSSH,
}

// Result is what a probe observed.
type Result string

const (
	ResultUp      Result = "up"
	ResultDown    Result = "down"
	ResultUnknown Result = "unknown"
)

// Results lists every result value.
var Results = []Result{ResultUp, ResultDown, ResultUnknown}

// Severity is a judgment about a check, a host, or the whole run.
type Severity string

const (
	SeverityGreen  Severity = "GREEN"
	SeverityYellow Severity = "YELLOW"
	SeverityRed    Severity = "RED"
)

// Severities lists every severity, least severe first.
var Severities = []Severity{SeverityGreen, SeverityYellow, SeverityRed}

// Expected says whether a check should answer.
type Expected string

const (
	ExpectedLight Expected = "light"
	ExpectedDark  Expected = "dark"
)

// ExpectedValues lists every expected value.
var ExpectedValues = []Expected{ExpectedLight, ExpectedDark}

// VantageStatus describes whether the vantage could be trusted.
type VantageStatus string

const (
	VantageOK        VantageStatus = "ok"
	VantageBlind     VantageStatus = "blind"
	VantageUnchecked VantageStatus = "unchecked"
)

// VantageStatuses lists every vantage status.
var VantageStatuses = []VantageStatus{VantageOK, VantageBlind, VantageUnchecked}

// Reason is a closed, snake_case code in checks[].reasons and hosts[].reason.
// Adding a code is a minor change; removing or renaming one is breaking.
type Reason string

// ReasonClass says what a reason code explains.
type ReasonClass string

const (
	// ClassOutcome codes say what the probe saw.
	ClassOutcome ReasonClass = "outcome"
	// ClassVerdict codes say why the severity is what it is.
	ClassVerdict ReasonClass = "verdict"
	// ClassBlind marks a check invalidated by a blind vantage.
	ClassBlind ReasonClass = "blind"
	// ClassEnrichment codes describe what the local tailnet daemon knows about
	// the peer of a ts_reach check. They are informational: they never change
	// a result or a severity.
	ClassEnrichment ReasonClass = "enrichment"
)

// Reason codes.
const (
	ReasonTCPConnectOK         Reason = "tcp_connect_ok"
	ReasonTCPConnectRefused    Reason = "tcp_connect_refused"
	ReasonTCPConnectTimeout    Reason = "tcp_connect_timeout"
	ReasonHTTPTimeout          Reason = "http_timeout"
	ReasonHTTPUnexpectedStatus Reason = "http_unexpected_status"
	ReasonPromNotReady         Reason = "prom_not_ready"
	ReasonPromAuth             Reason = "prom_auth"
	ReasonMetricsBodyBad       Reason = "metrics_body_unexpected"

	ReasonExpectedDark    Reason = "expected_dark"
	ReasonUnexpectedLight Reason = "unexpected_light"
	ReasonUnexpectedDark  Reason = "unexpected_dark"
	ReasonHeld            Reason = "held"
	ReasonProbeUnknown    Reason = "probe_unknown"
	ReasonPromPairDown    Reason = "prom_pair_down"
	ReasonPromPairDegrade Reason = "prom_pair_degraded"

	ReasonVantageBlind Reason = "vantage_blind"

	// Enrichment codes (S3). At most one per group on a ts_reach check: one of
	// the peer-state codes and one of the path codes.
	ReasonTSPeerOnline    Reason = "ts_peer_online"
	ReasonTSPeerOffline   Reason = "ts_peer_offline"
	ReasonTSPeerUnmatched Reason = "ts_peer_unmatched"
	ReasonTSPathDirect    Reason = "ts_path_direct"
	ReasonTSPathRelayed   Reason = "ts_path_relayed"
)

// EnrichmentPeerState and EnrichmentPath are the two groups of enrichment
// codes. A check carries at most one code from each.
var (
	EnrichmentPeerState = []Reason{ReasonTSPeerOnline, ReasonTSPeerOffline, ReasonTSPeerUnmatched}
	EnrichmentPath      = []Reason{ReasonTSPathDirect, ReasonTSPathRelayed}
)

// DefaultTailscaleSocket is the LocalAPI socket path used when the config
// enables enrichment without naming one (Linux). The config accepts absolute
// paths only.
const DefaultTailscaleSocket = "/var/run/tailscale/tailscaled.sock"

// ReasonSpec ties a reason code to its class.
type ReasonSpec struct {
	Code  Reason
	Class ReasonClass
}

// ReasonSpecs lists every reason code in documentation order.
var ReasonSpecs = []ReasonSpec{
	{ReasonTCPConnectOK, ClassOutcome},
	{ReasonTCPConnectRefused, ClassOutcome},
	{ReasonTCPConnectTimeout, ClassOutcome},
	{ReasonHTTPTimeout, ClassOutcome},
	{ReasonHTTPUnexpectedStatus, ClassOutcome},
	{ReasonPromNotReady, ClassOutcome},
	{ReasonPromAuth, ClassOutcome},
	{ReasonMetricsBodyBad, ClassOutcome},
	{ReasonExpectedDark, ClassVerdict},
	{ReasonUnexpectedLight, ClassVerdict},
	{ReasonUnexpectedDark, ClassVerdict},
	{ReasonHeld, ClassVerdict},
	{ReasonProbeUnknown, ClassVerdict},
	{ReasonPromPairDown, ClassVerdict},
	{ReasonPromPairDegrade, ClassVerdict},
	{ReasonVantageBlind, ClassBlind},
	{ReasonTSPeerOnline, ClassEnrichment},
	{ReasonTSPeerOffline, ClassEnrichment},
	{ReasonTSPeerUnmatched, ClassEnrichment},
	{ReasonTSPathDirect, ClassEnrichment},
	{ReasonTSPathRelayed, ClassEnrichment},
}

// ReasonCodes returns every reason code in documentation order.
func ReasonCodes() []Reason {
	out := make([]Reason, len(ReasonSpecs))
	for i, s := range ReasonSpecs {
		out[i] = s.Code
	}
	return out
}
