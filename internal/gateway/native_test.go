// Native gateway fixtures, listeners, administration and website.

package gateway

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the gateway, cmd/gateway, as its own process with its
// configuration file. Every forwarding claim is checked at the receivers or
// fixtures themselves.

// The native process serves the three fixture APIs through the one
// enforcement pipeline and the native proxy: every catalogue step reaches
// exactly its fixture or none. Its startup output tells listening apart from
// protected admission.
func TestNativeGatewayServesFixtures(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
	e.start(binaries.plain)

	c := loadCatalogue(t)
	results := runScenarios(t, e, fixtures, c)
	if len(results) != len(c.Scenarios) {
		t.Fatalf("%d of %d steps ran", len(results), len(c.Scenarios))
	}
	ran := map[string]bool{}
	for _, r := range results {
		ran[r.Operation] = true
	}
	for _, op := range c.Operations {
		if !ran[op.ID] {
			t.Errorf("catalogue operation %s was never exercised", op.ID)
		}
	}
	e.assertReceipts(0, 0, 0)

	s := e.status()
	if s.Admission != "ready" || s.State != modeNormal {
		t.Fatalf("status %+v, want normal and ready", s)
	}
	output := e.output.String()
	for _, want := range []string{
		`"msg":"listening","data":"` + e.dataAddr + `","admin":"` + e.admin + `"`,
		`"msg":"protected admission","admission":"ready","generation":"` + s.Generation + `"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("startup output lacks %s:\n%s", want, output)
		}
	}
	if strings.Contains(strings.ToLower(output), "caddy") {
		t.Errorf("the native process mentions Caddy:\n%s", output)
	}
}

// The native process listens on exactly its data, admin and metrics addresses. It has
// no Caddy administration endpoint, the data plane reaches no Admin API
// route, and the Admin API keeps its Host, session, Origin, CSRF and
// permission checks and its authority over the served policy, including the
// confirmed-activation and temporary-override lifecycles.
func TestNativeListenersAndAdministration(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain, "CADDY_ADMIN="+freeAddr(t))
	before := e.status()

	ports := listeningPorts(t, e.process.Process.Pid)
	if len(ports) != 3 || !strings.Contains(" "+strings.Join(ports, " ")+" ", " "+port(e.dataAddr)+" ") ||
		!strings.Contains(" "+strings.Join(ports, " ")+" ", " "+port(e.admin)+" ") || !strings.Contains(" "+strings.Join(ports, " ")+" ", " "+port(e.metrics)+" ") {
		t.Fatalf("listening ports %v, want exactly data %s, admin %s and metrics %s", ports, port(e.dataAddr), port(e.admin), port(e.metrics))
	}
	if dialable("127.0.0.1:2019") {
		t.Fatal("Caddy's default admin address accepts connections")
	}
	for _, path := range nativeAdminPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if code, _ := e.request(method, path, "application/json", `{}`); code != http.StatusNotFound {
				t.Errorf("%s %s on the data plane: %d", method, path, code)
			}
			if code, _ := e.adminRaw(method, path, `{}`); code != http.StatusNotFound {
				t.Errorf("%s %s on the admin listener with a session: %d", method, path, code)
			}
		}
	}

	// No Admin API route exists on the data plane, with or without the
	// administrator's session and the admin origin.
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	session := harness.headers(e)
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/admin/health"}, {http.MethodGet, "/admin/status"}, {http.MethodPost, "/admin/session"},
		{http.MethodPut, "/admin/blocked-accounts/mallory"}, {http.MethodPost, "/admin/candidates"}, {http.MethodGet, "/"},
	} {
		resp, body := e.exchange(probe.method, probe.path, "application/json", `{}`, session...)
		_, id := requestID(resp, body)
		if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusNotFound || len(events) != 1 || events[0]["stage"] != "ROUTE" {
			t.Errorf("%s %s on the data plane: %d %s, events %v", probe.method, probe.path, resp.StatusCode, body, events)
		}
	}

	// The Admin API's own boundary.
	membership := `{"reason":"test containment"}`
	put := func(header ...string) int {
		code, _, _ := e.adminSend(http.MethodPut, "/admin/blocked-accounts/alice", membership,
			append([]string{"If-Match", `"r1"`, "Idempotency-Key", "native-admin-" + newToken()[:8]}, header...)...)
		return code
	}
	cookie := []string{"Cookie", sessionCookie + "=" + harness.cookie}
	for name, tc := range map[string]struct {
		header []string
		want   int
	}{
		"no session":      {[]string{"Origin", e.adminOrigin()}, http.StatusUnauthorized},
		"no CSRF token":   {append(cookie, "Origin", e.adminOrigin()), http.StatusForbidden},
		"foreign origin":  {append(cookie, csrfHeader, harness.csrf, "Origin", "http://"+e.dataAddr), http.StatusForbidden},
		"data-plane host": {append(session, "Host", e.dataAddr), http.StatusBadRequest},
	} {
		if got := put(tc.header...); got != tc.want {
			t.Errorf("membership change with %s: %d, want %d", name, got, tc.want)
		}
	}
	if after := e.status(); after.Generation != before.Generation {
		t.Fatalf("refused requests changed the generation to %s", after.Generation)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusOK {
		t.Fatalf("alice before containment: %d", code)
	}
	if got := put(session...); got != http.StatusOK {
		t.Fatalf("authorised membership change: %d", got)
	}
	member := e.status()
	if member.Revision != 1 || member.Generation != before.Generation {
		t.Fatalf("containment changed policy: %+v", member)
	}
	e.resetReceivers()
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusForbidden {
		t.Fatalf("alice after containment: %d", code)
	}
	if code, body := e.unblockAccount(harness, "alice", "native-account-unblock"); code != 200 {
		t.Fatalf("unblock: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatalf("unblocked account: %d", code)
	}
	if r := e.a.receipts(); len(r) != 1 || r[0].Generation != member.Generation {
		t.Fatalf("receipts %+v, want one under %s", r, member.Generation)
	}

	// Confirmed activation: the candidate moves API a to C, and its
	// confirmation keeps it there.
	_, outcome := e.provisionalFrom(harness, e.cpaCandidate(), 1, 60, "native-cpa")
	if code, _, body := e.resolve(harness, "confirm", outcome.Provisional, `"r1"`, "native-cpa-confirm"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	e.resetReceivers()
	if code, body := e.get("/a/x", bearer(t, "alice")...); code != http.StatusOK || body != "C ok" {
		t.Fatalf("alice after the confirmed activation: %d %q", code, body)
	}

	// A temporary containment override blocks the operation until it is
	// revoked.
	confirmed := e.status().Confirmed
	created := e.mustOverride(harness, operationContainment("read-a", 60), policyETag(confirmed), "native-tpo-create")
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusForbidden {
		t.Fatalf("alice under the containment override: %d", code)
	}
	if code, _, body := e.revokeOverride(harness, created.Override, "native-tpo-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusOK {
		t.Fatalf("alice after the revocation: %d", code)
	}
	e.assertReceipts(0, 0, 2)
}

// The native data plane serves the Client website itself: the page
// at the approved Fetch routes and flat assets directory, with its security headers,
// matched on the exact target. Every other request, including every other
// spelling of a website path, goes to enforcement, and no request reaches a
// file outside the website or the admin dashboard. Without the setting there
// is no website.
func TestNativeWebsite(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	site := writeDashboard(t)
	settings := e.nativeSettings()
	section(settings, "data")["website"] = site
	e.writeNative(settings)
	e.start(binaries.plain)

	headers := map[string]string{
		"Content-Security-Policy": uiPolicy,
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
	}
	for _, f := range []struct{ path, contentType, body string }{
		{"/", "text/html; charset=utf-8", "<!doctype html><title>dashboard</title>"},
		{"/my-orders/ORD-1001?from=link", "text/html; charset=utf-8", "<!doctype html><title>dashboard</title>"},
		{"/assets/app.js", "text/javascript", "console.log('app')"},
		{"/assets/app.css", "text/css", "body{}"},
		{"/assets/logo.svg", "image/svg+xml", "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"},
	} {
		resp, body := e.exchange(http.MethodGet, f.path, "", "")
		if resp.StatusCode != http.StatusOK || body != f.body || resp.Header.Get("Content-Type") != f.contentType {
			t.Errorf("GET %s: %d %q %q", f.path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			continue
		}
		for name, want := range headers {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("GET %s: %s = %q, want %q", f.path, name, got, want)
			}
		}
	}
	if resp, body := e.exchange(http.MethodHead, "/", "", ""); resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /: %d %q", resp.StatusCode, body)
	}
	if code, body := e.get("/assets/missing.js"); code != http.StatusNotFound || strings.Contains(body, "request_id") {
		t.Errorf("missing asset: %d %q", code, body)
	}
	// A Windows device name is no file of the website: os.DirFS refuses it,
	// so the request is answered 404 at once instead of opening the device.
	if code, body := e.get("/assets/CON.js"); code != http.StatusNotFound || strings.Contains(body, "request_id") {
		t.Errorf("device name as an asset: %d %q", code, body)
	}

	// Everything else is the policy's: refused here by enforcement, with an
	// event, and never answered with a file.
	files := []string{"dashboard", "console.log", "body{}", "<svg", "not for the browser", "nested", "hidden", "other", "SQLite"}
	for _, probe := range []struct{ method, target string }{
		{http.MethodGet, "/demo"}, {http.MethodGet, "/demo/"}, {http.MethodGet, "/index.html"}, {http.MethodGet, "/assets/"},
		{http.MethodGet, "/assets/app.js.map"}, {http.MethodGet, "/assets/nested/x.js"}, {http.MethodGet, "/assets/readme.txt"},
		{http.MethodGet, "/assets/upper.JS.html"}, {http.MethodGet, "/secret.txt"}, {http.MethodGet, "/%61ssets/app.js"},
		{http.MethodGet, "/assets/..%2fsecret.txt"}, {http.MethodGet, "/assets/%2e%2e%2fsecret.txt"},
		{http.MethodGet, "/assets/../../secret.txt"}, {http.MethodGet, "/assets/..\\secret.txt"},
		{http.MethodGet, "/food//"}, {http.MethodGet, "//food"}, {http.MethodGet, "/FOOD"}, {http.MethodGet, "/food/%2e"},
		{http.MethodGet, "/assets/" + filepath.Base(e.database)}, {http.MethodGet, "/demo/assets/app.js"},
		{http.MethodGet, "/orders"}, {http.MethodGet, "/orders/ORD-1001"}, {http.MethodGet, "/support/tickets"},
		{http.MethodPost, "/"}, {http.MethodPut, "/assets/app.js"}, {http.MethodOptions, "/"},
	} {
		resp, body := e.rawResponse(probe.method + " " + probe.target + " HTTP/1.1\r\nHost: gw\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if resp.StatusCode == http.StatusOK || id == "" || len(events) != 1 || resp.Header.Get("Content-Security-Policy") != "" {
			t.Errorf("%s %s: %d %q, events %v", probe.method, probe.target, resp.StatusCode, body, events)
		}
		for _, content := range files {
			if strings.Contains(body, content) {
				t.Errorf("%s %s answered with file content %q", probe.method, probe.target, body)
			}
		}
	}
	if code, body, _ := e.adminSend(http.MethodGet, "/demo/", ""); code != http.StatusNotFound || strings.Contains(body, "dashboard") {
		t.Errorf("the admin listener serves the website: %d %q", code, body)
	}
	e.assertReceipts(0, 0, 0)

	e.stop()
	e.writeNative(e.nativeSettings())
	e.start(binaries.plain)
	resp, body := e.exchange(http.MethodGet, "/", "", "")
	if _, id := requestID(resp, body); resp.StatusCode != http.StatusNotFound || id == "" {
		t.Errorf("/ without a website: %d %q", resp.StatusCode, body)
	}
}
