package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hedronite/simplex/internal/contract"
	"github.com/Hedronite/simplex/internal/probe"
)

// Leak literals are reserved documentation values written as plain literals.
// None of them may reach a document, a warning or an error.
var leakLiterals = []string{
	"192.0.2.20", "198.51.100.5", "198.51.100.99", "203.0.113.7",
	"tower.tail.example", "secret-host.example", "nodekey:leakkey0001", "user-leak-0001",
}

type fakeLocalAPI struct {
	socket   string
	requests atomic.Int32
	methods  []string
	paths    []string
}

// startLocalAPI serves canned LocalAPI answers on a unix socket in a short temp
// directory (macOS caps socket paths near 104 bytes).
func startLocalAPI(t *testing.T, status int, body string) *fakeLocalAPI {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeLocalAPI{socket: filepath.Join(dir, "ts.sock")}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.methods = append(f.methods, r.Method)
		f.paths = append(f.paths, r.URL.Path)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}), ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// statusBody builds a LocalAPI status payload from documentation values, with
// extra fields the decoder must never surface.
func statusBody(peers string) string {
	return `{"BackendState":"Running","Self":{"TailscaleIPs":["203.0.113.7"],"DNSName":"vantage.tail.example.","PublicKey":"nodekey:leakkey0001","HostName":"secret-host.example","UserID":1},` +
		`"User":{"1":{"LoginName":"user-leak-0001"}},"Peer":{` + peers + `}}`
}

func peer(ip, name string, online bool, cur, relay string) string {
	return fmt.Sprintf(`"nodekey:leakkey0001":{"TailscaleIPs":["%s"],"DNSName":"%s","Online":%t,"CurAddr":"%s","Relay":"%s","HostName":"secret-host.example","Addrs":["198.51.100.99:1"],"PublicKey":"nodekey:leakkey0001"}`,
		ip, name, online, cur, relay)
}

type acceptDialer struct{}

func (acceptDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	c, p := net.Pipe()
	_ = p.Close()
	return c, nil
}

type instantClock struct{}

func (instantClock) Now() time.Time { return time.Now() }
func (instantClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

type lister bool

func (l lister) HasTailnetAddress() bool { return bool(l) }

func enrichConfig(block string) string {
	return "apiVersion: simplex.config/v0\nvantage:\n  id: lathe\nhosts:\n  - id: tower\n    checks:\n      - {kind: ts_reach, expected: light}\n      - {kind: kube_apiserver, expected: light}\n" + block
}

func runEnriched(t *testing.T, block string, socket string, tailnet bool) (*Result, error) {
	t.Helper()
	dir := t.TempDir()
	cfg := write(t, dir, "c.yaml", enrichConfig(block))
	ov := write(t, dir, "o.txt", "apiVersion: simplex.overlay/v0\nhosts:\n  tower:\n    address: 192.0.2.20\n")
	d := &Deps{
		Runner:          probe.New(&probe.Options{Dialer: acceptDialer{}, Clock: instantClock{}}),
		Lister:          lister(tailnet),
		Getenv:          func(string) string { return "" },
		TailscaleSocket: socket,
	}
	return Run(context.Background(), &Params{Config: cfg, Overlay: ov, Now: runNow}, d)
}

func tsReasons(t *testing.T, res *Result) []contract.Reason {
	t.Helper()
	for _, c := range res.Doc.Checks {
		if c.Kind == contract.KindTSReach {
			return c.Reasons
		}
	}
	t.Fatal("no ts_reach check in the document")
	return nil
}

func noLeak(t *testing.T, where string, text string) {
	t.Helper()
	for _, lit := range leakLiterals {
		if lit == "192.0.2.20" {
			continue // the overlay address itself is checked separately below
		}
		if strings.Contains(text, lit) {
			t.Errorf("%s repeats %q from the daemon payload:\n%s", where, lit, text)
		}
	}
	if strings.Contains(text, "192.0.2.20") {
		t.Errorf("%s repeats the overlay address", where)
	}
}

const onBlock = "tailscale:\n  localapi: true\n"

func TestEnrichmentCodesFromTheDaemon(t *testing.T) {
	cases := map[string]struct {
		peers string
		want  []contract.Reason
	}{
		"online and direct": {peer("192.0.2.20", "tower.tail.example.", true, "198.51.100.5:41641", ""),
			[]contract.Reason{"tcp_connect_ok", "ts_path_direct", "ts_peer_online"}},
		"online and relayed": {peer("192.0.2.20", "tower.tail.example.", true, "", "mia"),
			[]contract.Reason{"tcp_connect_ok", "ts_path_relayed", "ts_peer_online"}},
		"offline with no path": {peer("192.0.2.20", "tower.tail.example.", false, "", ""),
			[]contract.Reason{"tcp_connect_ok", "ts_peer_offline"}},
		"offline but relayed": {peer("192.0.2.20", "tower.tail.example.", false, "", "mia"),
			[]contract.Reason{"tcp_connect_ok", "ts_path_relayed", "ts_peer_offline"}},
		"not in the list": {peer("198.51.100.5", "other.tail.example.", true, "198.51.100.5:1", ""),
			[]contract.Reason{"tcp_connect_ok", "ts_peer_unmatched"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := startLocalAPI(t, 200, statusBody(tc.peers))
			res, err := runEnriched(t, onBlock, api.socket, true)
			if err != nil {
				t.Fatal(err)
			}
			got := tsReasons(t, res)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("reasons %v, want %v", got, tc.want)
			}
			// Informational: the verdict is what a plain connect gives.
			for _, c := range res.Doc.Checks {
				if c.Severity != contract.SeverityGreen {
					t.Fatalf("enrichment changed a severity: %+v", c)
				}
				if c.Kind != contract.KindTSReach && slices.ContainsFunc(c.Reasons, func(r contract.Reason) bool { return strings.HasPrefix(string(r), "ts_") }) {
					t.Fatalf("a non-ts_reach check carries enrichment: %+v", c)
				}
			}
			if n := api.requests.Load(); n != 1 {
				t.Fatalf("%d LocalAPI requests, want exactly one", n)
			}
			if api.methods[0] != http.MethodGet || api.paths[0] != "/localapi/v0/status" {
				t.Fatalf("request was %s %s", api.methods[0], api.paths[0])
			}
			noLeak(t, "the document", string(res.Body))
		})
	}
}

func TestEnrichmentIsSilentForTheVantageItself(t *testing.T) {
	api := startLocalAPI(t, 200, statusBody(peer("198.51.100.5", "other.tail.example.", true, "x:1", "")))
	// The overlay address is the vantage's own tailnet address.
	dir := t.TempDir()
	cfg := write(t, dir, "c.yaml", enrichConfig(onBlock))
	ov := write(t, dir, "o.txt", "apiVersion: simplex.overlay/v0\nhosts:\n  tower:\n    address: 203.0.113.7\n")
	res, err := Run(context.Background(), &Params{Config: cfg, Overlay: ov, Now: runNow}, &Deps{
		Runner: probe.New(&probe.Options{Dialer: acceptDialer{}, Clock: instantClock{}}),
		Lister: lister(true), Getenv: func(string) string { return "" }, TailscaleSocket: api.socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := tsReasons(t, res); !slices.Equal(got, []contract.Reason{"tcp_connect_ok"}) {
		t.Fatalf("own node reasons %v, want only the outcome code", got)
	}
}

func TestEnrichmentOffMeansNoRequest(t *testing.T) {
	api := startLocalAPI(t, 200, statusBody(peer("192.0.2.20", "tower.tail.example.", true, "x:1", "")))
	for name, block := range map[string]string{"no block": "", "localapi false": "tailscale:\n  localapi: false\n"} {
		t.Run(name, func(t *testing.T) {
			before := api.requests.Load()
			res, err := runEnriched(t, block, api.socket, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := tsReasons(t, res); !slices.Equal(got, []contract.Reason{"tcp_connect_ok"}) {
				t.Fatalf("reasons %v", got)
			}
			if api.requests.Load() != before {
				t.Fatal("a request was made with enrichment off")
			}
		})
	}
}

func TestBlindVantageMakesNoRequestAndCarriesNoCodes(t *testing.T) {
	api := startLocalAPI(t, 200, statusBody(peer("192.0.2.20", "tower.tail.example.", true, "x:1", "")))
	res, err := runEnriched(t, onBlock, api.socket, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Doc.Vantage.Status != contract.VantageBlind {
		t.Fatalf("vantage %s, want blind", res.Doc.Vantage.Status)
	}
	if api.requests.Load() != 0 {
		t.Fatal("a blind pass made a LocalAPI request")
	}
	for _, c := range res.Doc.Checks {
		if !slices.Equal(c.Reasons, []contract.Reason{contract.ReasonVantageBlind}) {
			t.Fatalf("blind check reasons %v", c.Reasons)
		}
	}
}

// Whatever the daemon does wrong, the pass succeeds, carries no enrichment, and
// reports one fixed warning by field path that repeats nothing from the payload.
func TestEnrichmentFailsSoftWithOneWarning(t *testing.T) {
	huge := strings.Repeat("x", 5<<20)
	cases := map[string]func(t *testing.T) string{
		"forbidden":    func(t *testing.T) string { return startLocalAPI(t, 403, "nodekey:leakkey0001 user-leak-0001").socket },
		"server error": func(t *testing.T) string { return startLocalAPI(t, 500, "nodekey:leakkey0001").socket },
		"malformed": func(t *testing.T) string {
			return startLocalAPI(t, 200, `{"Peer": nodekey:leakkey0001 secret-host.example`).socket
		},
		"oversize":      func(t *testing.T) string { return startLocalAPI(t, 200, huge).socket },
		"socket absent": func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.sock") },
		"relative path": func(t *testing.T) string { return "ts.sock" },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := runEnriched(t, onBlock, mk(t), true)
			if err != nil {
				t.Fatalf("enrichment failure failed the pass: %v", err)
			}
			if got := tsReasons(t, res); !slices.Equal(got, []contract.Reason{"tcp_connect_ok"}) {
				t.Fatalf("reasons %v, want no enrichment codes", got)
			}
			var found int
			for _, w := range res.Warnings {
				if w.Path == "tailscale" {
					found++
					noLeak(t, "a warning", w.Message+" "+w.Path)
					if !strings.Contains(w.Message, "no enrichment this pass") {
						t.Errorf("unexpected warning text %q", w.Message)
					}
				}
			}
			if found != 1 {
				t.Fatalf("%d tailscale warnings, want exactly one: %+v", found, res.Warnings)
			}
			noLeak(t, "the document", string(res.Body))
		})
	}
}

func TestEnrichmentErrorsNeverCarryPayloadText(t *testing.T) {
	api := startLocalAPI(t, 200, `{"Peer": nodekey:leakkey0001 secret-host.example`)
	_, err := runEnriched(t, onBlock, api.socket, true)
	var re *RunError
	if errors.As(err, &re) {
		noLeak(t, "an error", re.Error()+re.Fields())
	}
}
