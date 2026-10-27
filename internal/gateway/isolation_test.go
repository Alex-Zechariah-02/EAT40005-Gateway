package gateway

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		t.Fatalf("listeners %v, want exactly data and controller ports", ports)
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

	// The project channel still changes the effective generation, through
	// the guarded membership API.
	code, body, _ := e.harnessCall(http.MethodPut, "/admin/blocked-accounts/probe-01", `{"reason":"test containment"}`,
		"If-Match", `"r1"`, "Idempotency-Key", "native-admin-probe-1")
	if code != http.StatusOK {
		t.Fatalf("authorised membership change: %d %s", code, body)
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

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAuditGateAndSecretSafety(t *testing.T) {
	t.Run("records outcomes without secrets", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		// An informational response before the final one must not be
		// recorded as the outcome.
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusEarlyHints)
			return false
		}
		e.start(binaries.fault)
		g := e.status().Generation
		bearer := "Bearer SECRET-TOKEN-1234567890"

		e.get("/a/x?token=SECRET-QUERY-VALUE", "Authorization", bearer)
		e.get("/d?token=SECRET-QUERY-VALUE", "Authorization", bearer)
		e.raw("GET /a/x%0A{\"kind\":\"forged\"} HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		e.assertReceipts(1, 0, 0)

		// auditEvents fails the test if any line is not exactly one JSON record.
		events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.rejected") >= 2 })
		raw, _ := os.ReadFile(e.auditLog)
		for _, secret := range []string{"SECRET-TOKEN", "SECRET-QUERY-VALUE", "Bearer"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("audit log contains %q", secret)
			}
		}
		seen := map[string]bool{}
		for _, ev := range events {
			if ev["kind"] == "forged" {
				t.Fatal("a request created a forged audit record")
			}
			if ev["kind"] == "request.forwarded" && ev["status"] != float64(http.StatusOK) {
				t.Fatalf("forwarded event %v, want final status 200", ev)
			}
			if strings.HasPrefix(ev["kind"].(string), "request.") {
				key := ev["kind"].(string) + "/" + ev["stage"].(string)
				seen[key] = true
				if ev["stage"] != "CAPTURE" && ev["generation"] != g {
					t.Fatalf("event %v lacks generation %s", ev, g)
				}
			}
		}
		for _, key := range []string{"request.admitted/AUDIT", "request.forwarded/FORWARD", "request.rejected/ACCESS", "request.rejected/CANONICAL"} {
			if !seen[key] {
				t.Fatalf("missing event %s in %v", key, events)
			}
		}
	})

	// An audit log the gateway cannot open stops its startup: nothing listens,
	// so nothing is admitted without its durable record, and the database is
	// released.
	unopenable := []struct {
		name    string
		prepare func(e *env)
	}{
		{"audit file cannot be opened", func(e *env) { os.Mkdir(e.auditLog, 0o700) }},
		{"audit file read-only", func(e *env) {
			os.WriteFile(e.auditLog, nil, 0o600)
			os.Chmod(e.auditLog, 0o400)
			e.t.Cleanup(func() { os.Chmod(e.auditLog, 0o600) })
		}},
		{"audit path is a symbolic link", func(e *env) {
			target := filepath.Join(e.dir, "elsewhere.log")
			os.WriteFile(target, nil, 0o600)
			if err := os.Symlink(target, e.auditLog); err != nil {
				e.t.Fatalf("creating the symbolic link: %v", err)
			}
		}},
	}
	for _, f := range unopenable {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			f.prepare(e)
			e.run(binaries.plain)
			if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), "audit log") {
				t.Fatalf("exit %d, want %d naming the audit log\n%s", code, exitFailed, e.output)
			}
			if dialable(e.dataAddr) || dialable(e.admin) {
				t.Fatal("a listener is open after the refused start")
			}
			e.assertReceipts(0, 0, 0)
			st, err := openStore(e.database)
			if err != nil {
				t.Fatalf("the database is held: %v", err)
			}
			st.close()
		})
	}

	// An audit log that fails while the gateway runs stops every protected
	// admission and every policy change.
	failures := []struct {
		name  string
		fault string
	}{
		{"audit sync fails", "GATEWAY_FAULT=audit-sync"},
		{"no space left", "GATEWAY_FAULT=audit-full"},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			e.start(binaries.fault, f.fault)
			before := e.status()
			for range 3 {
				if code, _ := e.get("/a/x"); code != http.StatusServiceUnavailable {
					t.Fatalf("admission with failed audit log: %d", code)
				}
			}
			// The unauthenticated health check reports the stored mode only:
			// it answers 200 and normal while every protected admission
			// fails. The authenticated status below reports the failure.
			if health := e.health(); health != modeNormal {
				t.Fatalf("health %q with a failed audit log", health)
			}
			if code, _ := e.get("/zzz"); code != http.StatusNotFound {
				t.Fatalf("rejection with failed audit log: %d", code)
			}
			e.assertReceipts(0, 0, 0)
			// A policy change needs its durable audit intent first, so the
			// membership API refuses to publish anything.
			code, body, _ := e.harnessCall(http.MethodPut, "/admin/blocked-accounts/someone", `{"reason":"test containment"}`,
				"If-Match", `"r1"`, "Idempotency-Key", "audit-failure-probe")
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "gateway_unavailable") {
				t.Fatalf("publication with failed audit log: %d %s", code, body)
			}
			if s := e.status(); !s.AuditFailed || s.Generation != before.Generation {
				t.Fatalf("status %+v", s)
			}
		})
	}
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

// listeningPorts lists the TCP ports a process listens on, using netstat.
func listeningPorts(t *testing.T, pid int) []string {
	t.Helper()
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		t.Fatalf("netstat: %v", err)
	}
	var ports []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 5 && fields[3] == "LISTENING" && fields[4] == strconv.Itoa(pid) {
			ports = append(ports, port(fields[1]))
		}
	}
	return ports
}
