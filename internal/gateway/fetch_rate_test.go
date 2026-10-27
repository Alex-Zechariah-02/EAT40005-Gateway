package gateway

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestFetchClientAddressQuota(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations[0].Rate = &RateRule{Algorithm: "sliding-window", Count: 2, WindowSeconds: 60, Key: "client_address"}
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	for _, tc := range []struct {
		address string
		status  int
	}{
		{"127.0.0.50", 200}, {"127.0.0.50", 200}, {"127.0.0.50", 429},
		{"127.0.0.51", 200},
	} {
		transport := &http.Transport{DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(tc.address)}}).DialContext, DisableKeepAlives: true}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		r, err := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-Forwarded-For", newRequestID())
		r.Header.Set("Forwarded", "for=192.0.2.1")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		transport.CloseIdleConnections()
		if err != nil || response.StatusCode != tc.status {
			t.Fatalf("peer %s: %d %s (%v), want %d", tc.address, response.StatusCode, body, err, tc.status)
		}
	}
	e.assertReceipts(3, 0, 0)
}

func TestFetchRateKeyCompilation(t *testing.T) {
	for _, tc := range []struct{ auth, key, code string }{
		{"none", "client_address", ""}, {"required", "client_address", ""}, {"required", "subject", ""},
		{"none", "subject", "INVALID_VALUE"}, {"required", "X-Forwarded-For", "UNSUPPORTED_FEATURE"},
		{"none", "", "UNSUPPORTED_FEATURE"},
	} {
		p := Policy{
			SchemaVersion: schemaVersion, Profile: "core",
			Upstreams: []Upstream{{ID: "u", Dial: "127.0.0.1:10501"}},
			APIs:      []API{{ID: "inventory", Upstream: "u"}},
			Operations: []Operation{{ID: "search", API: "inventory", Method: "GET", Target: "/inventory/search", Access: "allow", Auth: tc.auth,
				Rate: &RateRule{Algorithm: "sliding-window", Count: 2, WindowSeconds: 60, Key: tc.key}}},
		}
		_, err := Compile(p, 1, time.Time{})
		if code := errorCode(err); code != tc.code {
			t.Errorf("auth=%s key=%q: %v, want %q", tc.auth, tc.key, err, tc.code)
		}
	}
}
