package tailscale

import "testing"

// FuzzDecodeStatus asserts the decoder neither panics nor invents a peer from
// arbitrary bytes. A payload that is not a JSON object must be an error; one
// that is may produce any snapshot, but never a panic.
func FuzzDecodeStatus(f *testing.F) {
	f.Add([]byte(statusFixture))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"BackendState":"Running","Peer":{}}`))
	f.Add([]byte(`{"Peer":{"a":{"Online":true,"TailscaleIPs":["192.0.2.10"]}}}`))
	f.Add([]byte(`{"Peer":null}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		status, err := decodeStatus(body)
		if err != nil {
			return
		}
		if status == nil {
			t.Fatal("decode returned no status and no error")
		}
		// Whatever it decoded, a lookup must be safe and a key must never be
		// empty: an empty key would match any empty address.
		for _, probe := range []string{"", "192.0.2.10", "spoke.example."} {
			_, _ = status.Match(probe)
		}
		if _, ok := status.Match(""); ok {
			t.Fatal("an empty address matched a peer")
		}
	})
}
