// Administration absence, fault build, loopback and network bypass.

package gateway

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var nativeAdminPaths = []string{"/config/", "/load", "/stop", "/id/x", "/debug/pprof/", "/reverse_proxy/upstreams", "/adapt", "/pki/ca/local", "/metrics"}

func TestNativeAdministrationIsAbsent(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	overrideAdmin := freeAddr(t)
	e.start(binaries.plain, "CADDY_ADMIN="+overrideAdmin)
	before := e.status()

	ports := listeningPorts(t, e.process.Process.Pid)
	want := map[string]bool{port(e.dataAddr): true, port(e.admin): true, port(e.metrics): true}
	for _, p := range ports {
		if !want[p] {
			t.Fatalf("unexpected listener on port %s (listeners %v)", p, ports)
		}
	}
	if len(ports) != len(want) {
		t.Fatalf("listeners %v, want exactly the data, admin and metrics ports", ports)
	}

	for _, addr := range []string{"127.0.0.1:2019", overrideAdmin} {
		if dialable(addr) {
			t.Fatalf("native admin address %s accepts connections", addr)
		}
	}

	// The admin listener serves only the project Admin API: Caddy's native
	// paths do not exist there, with or without an administrator session.
	for _, path := range nativeAdminPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if code, _ := e.request(method, path, "application/json", `{}`); code == http.StatusOK {
				t.Fatalf("%s %s on the data plane returned 200", method, path)
			}
			if code, _, _ := e.adminSend(method, path, `{}`, "Origin", e.adminOrigin()); code != http.StatusNotFound {
				t.Fatalf("%s %s on the admin listener without a session: %d", method, path, code)
			}
			if code, _ := e.adminRaw(method, path, `{}`); code != http.StatusNotFound {
				t.Fatalf("%s %s on the admin listener with a session: %d", method, path, code)
			}
		}
	}
	e.assertReceipts(0, 0, 0)

	// A production binary has no whole-policy publication route: neither the
	// earlier bearer-secret channel nor the test publication hook exists.
	change := string(mustJSON(Change{ID: "x", Expected: 1, Policy: mustJSON(e.policy(e.a, true))}))
	for _, path := range []string{"/publish", "/test/publish", "/admin/publish", "/admin/activate", "/admin/policy"} {
		if code, _ := e.adminRaw(http.MethodPost, path, change); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s on the admin listener: %d", path, code)
		}
	}
	if after := e.status(); after.Generation != before.Generation {
		t.Fatalf("probes changed the generation to %s", after.Generation)
	}
	if code, _ := e.get("/b/x"); code != http.StatusNotFound {
		t.Fatalf("operation outside the policy after the probes: %d", code)
	}

	// The guarded blocked-accounts API still works, and blocking an account
	// changes neither the revision nor the effective generation.
	code, body, _ := e.harnessCall(http.MethodPut, "/admin/blocked-accounts/probe-01", `{"reason":"test containment"}`,
		"If-Match", `"r1"`, "Idempotency-Key", "native-admin-probe-1")
	if code != http.StatusOK {
		t.Fatalf("authorised account block: %d %s", code, body)
	}
	if after := e.status(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("account containment changed policy: %+v", after)
	}
	if code, body, _ := e.harnessCall("GET", "/admin/blocked-accounts", ""); code != 200 || !strings.Contains(body, "probe-01") {
		t.Fatalf("containment not published: %d %s", code, body)
	}
}

// The fault-injection controls are compiled only into the binaries built with
// the faultinject tag. The production binaries hold none of the environment
// variable names that select them; the fault binaries hold every one, so the
// check cannot pass merely because the names are spelt differently.
func TestFaultInjectionOnlyInFaultBuild(t *testing.T) {
	plain, err := os.ReadFile(binaries.plain)
	if err != nil {
		t.Fatal(err)
	}
	fault, err := os.ReadFile(binaries.fault)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GATEWAY_FAULT", "GATEWAY_HOLD_PATH", "GATEWAY_HOLD_FILE", "GATEWAY_HOLD_RELEASE", "GATEWAY_RATE_CLOCK", "GATEWAY_WALL_OFFSET", "GATEWAY_AUDIT_HOLD"} {
		if bytes.Contains(plain, []byte(name)) {
			t.Errorf("the production binary contains %s", name)
		}
		if !bytes.Contains(fault, []byte(name)) {
			t.Errorf("the fault-injection binary lacks %s", name)
		}
	}
}

// TestUpstreamsBindOnlyToLoopback checks the local half of bypass protection:
// upstreams accept connections only on loopback, so they are unreachable
// through any other interface of the host. It cannot show that a client on
// another machine is blocked; that requires a separate network position.
func TestUpstreamsBindOnlyToLoopback(t *testing.T) {
	e := newEnv(t)
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
		for _, rc := range []*receiver{e.a, e.b, e.c} {
			target := net.JoinHostPort(ip.String(), port(rc.addr))
			if dialable(target) {
				t.Fatalf("receiver %s reachable at %s", rc.name, target)
			}
			probed++
		}
	}
	t.Logf("probed %d non-loopback interface addresses; none reached a receiver", probed)
	if probed == 0 {
		t.Skip("no non-loopback IPv4 interface to probe")
	}
}

// The separate-network probe also isolates its port by the configured offset. Its
// optional host firewall rule must match this address and the test binary.
func wslDataAddress(t *testing.T, host string) string {
	t.Helper()
	return net.JoinHostPort(host, port(demoTestAddress(t, "127.0.0.1:18443")))
}

// TestBypassFromSeparateNetwork probes from WSL, a separate network position.
// The gateway must be reachable from there (the positive control), while the
// upstreams, the fixture APIs, their control listeners and the controller
// listener must not be, by any address. It needs
// WSL with curl and the host firewall rule, so it runs only when
// GATEWAY_TEST_WSL is set.
func TestBypassFromSeparateNetwork(t *testing.T) {
	if os.Getenv("GATEWAY_TEST_WSL") == "" {
		t.Skip("set GATEWAY_TEST_WSL=1 to probe from WSL")
	}
	host := wslHostAddress(t)
	wslGatewayBinary := filepath.Join(os.Getenv("LOCALAPPDATA"), "EAT40005", "wsl-test", "gateway.exe")
	copyFile(t, binaries.plain, wslGatewayBinary)

	e := newEnv(t)
	e.dataAddr = wslDataAddress(t, host)
	e.writeConfig(configOptions{})
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(wslGatewayBinary)

	token := issuer.tokenFor(t, "alice")
	if got := fromWSL(t, e.dataAddr, "/a/x", "Authorization: Bearer "+token); got != "200" {
		t.Fatalf("positive control: WSL to gateway returned %q, want 200", got)
	}
	e.assertReceipts(1, 0, 0)
	if got := fromWSL(t, e.dataAddr, "/a/x"); got != "401" {
		t.Fatalf("protected operation without a token from WSL returned %q, want 401", got)
	}
	if got := fromWSL(t, e.dataAddr, "/a/x", "X-Gateway-Subject: alice", "X-Gateway-Roles: WRITER"); got != "401" {
		t.Fatalf("forged identity headers from WSL returned %q, want 401", got)
	}
	if got := fromWSL(t, e.dataAddr, "/d"); got != "403" {
		t.Fatalf("denied operation from WSL returned %q, want 403", got)
	}
	e.assertReceipts(1, 0, 0)

	targets := map[string]string{"controller": e.admin, "A": e.a.addr, "B": e.b.addr, "C": e.c.addr}
	fixtures := startFixtures(t)
	for name, f := range fixtures {
		targets[name] = f.addr
		targets[name+" control"] = f.control
	}
	for name, addr := range targets {
		for _, h := range []string{host, "127.0.0.1"} {
			target := net.JoinHostPort(h, port(addr))
			if got := fromWSL(t, target, "/a/x"); got != "000" {
				t.Fatalf("%s reachable from WSL at %s (HTTP %s)", name, target, got)
			}
		}
	}
	e.assertReceipts(1, 0, 0)

	// Unreachable must mean blocked, not stopped: every listener probed above
	// still answers locally.
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+token); code != http.StatusOK {
		t.Fatalf("gateway no longer answers locally: %d", code)
	}
	for _, rc := range []*receiver{e.a, e.b, e.c} {
		if !dialable(rc.addr) {
			t.Fatalf("receiver %s stopped during the probe", rc.name)
		}
	}
	for name, f := range fixtures {
		if f.state().Service != name || !dialable(f.addr) {
			t.Fatalf("fixture %s stopped during the probe", name)
		}
	}
	if s := e.status(); s.Generation == "" {
		t.Fatal("controller listener no longer answers locally")
	}
}

// wslHostAddress returns the Windows host address as routed from WSL.
func wslHostAddress(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("wsl.exe", "-e", "ip", "route", "show", "default").Output()
	if err != nil {
		t.Fatalf("reading WSL default route: %v", err)
	}
	fields := strings.Fields(string(bytes.ReplaceAll(out, []byte{0}, nil)))
	if len(fields) < 3 || fields[0] != "default" || fields[1] != "via" {
		t.Fatalf("unexpected WSL default route %q", out)
	}
	return fields[2]
}

// fromWSL requests addr+path with curl inside WSL, sending any given header
// lines, and returns the HTTP status; "000" means no connection was made.
func fromWSL(t *testing.T, addr, path string, headers ...string) string {
	t.Helper()
	args := []string{"-e", "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5"}
	for _, h := range headers {
		args = append(args, "-H", h)
	}
	out, _ := exec.Command("wsl.exe", append(args, "http://"+addr+path)...).Output()
	return strings.TrimSpace(string(out))
}
