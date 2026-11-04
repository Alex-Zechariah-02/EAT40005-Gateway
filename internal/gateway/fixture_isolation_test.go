// Nothing reaches the fixtures unless admitted, and fixtures stay private.

package gateway

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Requests the gateway rejects leave no receipt at any
// fixture and no state change.
func TestRejectedRequestsNeverReachFixtures(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	states := map[string]string{}
	for name, f := range fixtures {
		states[name] = f.state().SHA256
	}

	// Body checks follow authentication, so those requests carry a valid
	// token for a subject the operation permits.
	requests := []struct {
		name    string
		request string
		subject string
		status  int
	}{
		{"unknown path", "GET /orders-archive HTTP/1.1\r\nHost: gw\r\n", "", 404},
		{"prefix base without segment", "GET /inventory/items/ HTTP/1.1\r\nHost: gw\r\n", "", 404},
		{"wrong method", "DELETE /orders HTTP/1.1\r\nHost: gw\r\n", "", 405},
		{"HEAD not implied by GET", "HEAD /reports/summary HTTP/1.1\r\nHost: gw\r\n", "", 405},
		{"OPTIONS not answered", "OPTIONS /orders HTTP/1.1\r\nHost: gw\r\n", "", 405},
		{"lowercase method", "get /orders HTTP/1.1\r\nHost: gw\r\n", "", 405},
		{"case changed", "GET /Orders HTTP/1.1\r\nHost: gw\r\n", "", 404},
		{"trailing slash", "GET /reports/summary/ HTTP/1.1\r\nHost: gw\r\n", "", 404},
		{"encoded slash", "GET /orders/ORD-1001%2F..%2F HTTP/1.1\r\nHost: gw\r\n", "", 400},
		{"dot segment", "GET /inventory/items/../health HTTP/1.1\r\nHost: gw\r\n", "", 400},
		{"double encoding", "GET /orders/%252e%252e HTTP/1.1\r\nHost: gw\r\n", "", 400},
		{"absolute form naming a fixture", "GET http://" + fixtures["reports"].addr + "/reports/sales HTTP/1.1\r\nHost: gw\r\n", "", 400},
		{"body on bodyless operation", "GET /orders HTTP/1.1\r\nHost: gw\r\nContent-Length: 2\r\n\r\n{}", "staff-01", 400},
		{"write without JSON media type", "PATCH /inventory/items/ITM-001 HTTP/1.1\r\nHost: gw\r\nContent-Type: text/plain\r\nContent-Length: 11\r\n\r\n{\"delta\":1}", "staff-01", 415},
		{"malformed JSON write", "POST /orders HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nContent-Length: 4\r\n\r\n{\"cu", "cust-001", 400},
	}
	for _, r := range requests {
		request := r.request
		if !strings.Contains(request, "\r\n\r\n") {
			request += "\r\n"
		}
		extra := "Connection: close\r\n"
		if r.subject != "" {
			extra += "Authorization: Bearer " + issuer.tokenFor(t, r.subject) + "\r\n"
		}
		code, _ := e.raw(strings.Replace(request, "Host: gw\r\n", "Host: gw\r\n"+extra, 1))
		if code != r.status {
			t.Errorf("%s: status %d, want %d", r.name, code, r.status)
		}
	}
	for name, f := range fixtures {
		if n := len(f.receipts()); n != 0 {
			t.Errorf("fixture %s received %d rejected requests", name, n)
		}
		if f.state().SHA256 != states[name] {
			t.Errorf("fixture %s state changed", name)
		}
	}
	e.assertReceipts(0, 0, 0)
}

// The deny-all form of the fixture policy, published through the controller,
// admits nothing: every catalogue step is refused and no fixture is reached.
func TestDenyAllPolicyForwardsNothing(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	e.start(binaries.fault) // publication below uses the test publication hook
	c := loadCatalogue(t)

	denyAll := fixturePolicy(fixtures)
	for i := range denyAll.Operations {
		denyAll.Operations[i].Access = "deny"
	}
	denyAll.Permits = nil
	if outcome, code := e.publish(Change{ID: "deny-all", Expected: 1, Policy: mustJSON(denyAll)}); code != http.StatusOK {
		t.Fatalf("publishing the deny-all policy: %d %+v", code, outcome)
	}
	for _, sc := range c.Scenarios {
		contentType := ""
		if len(sc.Body) > 0 {
			contentType = "application/json"
		}
		if code, _ := e.request(sc.Method, sc.Path, contentType, string(sc.Body)); code != http.StatusForbidden {
			t.Errorf("%s under deny-all: %d, want 403", sc.Name, code)
		}
	}
	for name, n := range receiptCounts(fixtures) {
		if n != 0 {
			t.Errorf("fixture %s received %d requests under deny-all", name, n)
		}
	}
}

// The control functions need the fixture's own secret, and the
// gateway's data plane cannot reach them.
func TestFixtureControlIsPrivileged(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	orders := fixtures["orders"]
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", `{"customer":"cust-001","item":"ITM-001","quantity":1}`, bearer(t, "cust-001")...); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	changed := orders.state().SHA256

	for _, secret := range []string{"", "wrong", fixtures["inventory"].secret} {
		if code, _ := orders.call(http.MethodPost, "/reset", secret); code != http.StatusUnauthorized {
			t.Fatalf("reset with secret %q: %d", secret, code)
		}
		if code, _ := orders.call(http.MethodGet, "/receipts", secret); code != http.StatusUnauthorized {
			t.Fatalf("receipts with secret %q: %d", secret, code)
		}
	}
	if orders.state().SHA256 != changed || len(orders.receipts()) != 1 {
		t.Fatal("an unauthorised control request changed the fixture")
	}

	// The control paths do not exist on the data plane, and the API listener
	// does not serve them either.
	for _, path := range []string{"/reset", "/receipts", "/state"} {
		if code, _ := e.request(http.MethodPost, path, "", ""); code != http.StatusNotFound {
			t.Errorf("POST %s through the gateway: %d, want 404", path, code)
		}
	}
	if orders.state().SHA256 != changed {
		t.Fatal("state changed through the data plane")
	}
}

// Fixtures listen only on loopback, refuse to
// start on any other address, and cannot be reached through the host's other
// interfaces. The separate-network half is TestBypassFromSeparateNetwork.
func TestFixturesListenOnlyOnLoopback(t *testing.T) {
	fixtures := startFixtures(t)

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	probed := 0
	for _, a := range addrs {
		ip, _, _ := net.ParseCIDR(a.String())
		if ip == nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		for _, f := range fixtures {
			for _, addr := range []string{f.addr, f.control} {
				target := net.JoinHostPort(ip.String(), port(addr))
				if dialable(target) {
					t.Fatalf("fixture %s reachable at %s", f.name, target)
				}
				probed++
			}
		}
	}
	t.Logf("probed %d non-loopback interface addresses; none reached a fixture", probed)

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	os.WriteFile(secretFile, []byte(strings.Repeat("s", 32)), 0o600)
	for _, listen := range []string{"0.0.0.0:0", ":0", "localhost:0"} {
		out, err := exec.Command(binaries.fixture, "-service", "orders", "-listen", listen, "-control", "127.0.0.1:0", "-secret-file", secretFile).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "loopback") {
			t.Errorf("fixture started on %s: %v %s", listen, err, out)
		}
	}
}
