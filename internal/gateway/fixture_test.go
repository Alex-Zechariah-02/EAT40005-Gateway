package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

var fixtureNames = []string{"orders", "inventory", "reports"}

// fixtureProcess is one fixture API running as its own process.
type fixtureProcess struct {
	t       *testing.T
	name    string
	addr    string // API listener, reached only through the gateway
	control string // receipts, state and reset; requires the secret
	secret  string
	output  *bytes.Buffer
}

// startFixtures runs the three fixture APIs on loopback, each with its own
// control secret, and stops them when the test ends.
func startFixtures(t *testing.T) map[string]*fixtureProcess {
	t.Helper()
	return startFixtureSet(t, binaries.fixture)
}

func startFixtureSet(t *testing.T, binary string, extraEnv ...string) map[string]*fixtureProcess {
	t.Helper()
	dir := t.TempDir()
	fixtures := map[string]*fixtureProcess{}
	for _, name := range fixtureNames {
		secret := make([]byte, 32)
		rand.Read(secret)
		f := &fixtureProcess{
			t:       t,
			name:    name,
			addr:    freeAddr(t),
			control: freeAddr(t),
			secret:  hex.EncodeToString(secret),
			output:  &bytes.Buffer{},
		}
		secretFile := filepath.Join(dir, name+".secret")
		if err := os.WriteFile(secretFile, []byte(f.secret), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-service", name, "-listen", f.addr, "-control", f.control, "-secret-file", secretFile)
		cmd.Env = append(os.Environ(), "FIXTURE_FAULT=")
		cmd.Env = append(cmd.Env, extraEnv...)
		if binaries.race {
			cmd.Env = append(cmd.Env, "GORACE=halt_on_error=1")
		}
		newProcessGroup(cmd)
		cmd.Stdout, cmd.Stderr = f.output, f.output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		t.Cleanup(func() {
			interrupt(t, cmd)
			select {
			case err := <-exited:
				if err != nil {
					t.Errorf("fixture %s did not stop cleanly: %v\n%s", name, err, f.output)
				}
			case <-time.After(6 * time.Second):
				cmd.Process.Kill()
				<-exited
				t.Errorf("fixture %s shutdown exceeded its bound", name)
			}
			if strings.Contains(f.output.String(), "WARNING: DATA RACE") {
				t.Errorf("race detected in fixture %s:\n%s", name, f.output)
			}
		})
		deadline := time.Now().Add(10 * time.Second)
		for !dialable(f.addr) || !dialable(f.control) {
			if time.Now().After(deadline) {
				t.Fatalf("fixture %s not ready\n%s", name, f.output)
			}
			time.Sleep(20 * time.Millisecond)
		}
		fixtures[name] = f
	}
	return fixtures
}

func (f *fixtureProcess) call(method, path, secret string) (int, []byte) {
	f.t.Helper()
	req, _ := http.NewRequest(method, "http://"+f.control+path, http.NoBody)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		f.t.Fatalf("fixture %s control %s: %v", f.name, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (f *fixtureProcess) receipts() []fixture.Receipt {
	f.t.Helper()
	code, data := f.call(http.MethodGet, "/receipts", f.secret)
	var body struct{ Receipts []fixture.Receipt }
	if code != http.StatusOK || json.Unmarshal(data, &body) != nil {
		f.t.Fatalf("fixture %s receipts: %d %s", f.name, code, data)
	}
	return body.Receipts
}

func (f *fixtureProcess) state() fixture.State {
	f.t.Helper()
	return f.decodeState(f.call(http.MethodGet, "/state", f.secret))
}

func (f *fixtureProcess) reset() fixture.State {
	f.t.Helper()
	return f.decodeState(f.call(http.MethodPost, "/reset", f.secret))
}

func (f *fixtureProcess) decodeState(code int, data []byte) fixture.State {
	f.t.Helper()
	var s fixture.State
	if code != http.StatusOK || json.Unmarshal(data, &s) != nil {
		f.t.Fatalf("fixture %s state: %d %s", f.name, code, data)
	}
	return s
}

func receiptCounts(fixtures map[string]*fixtureProcess) map[string]int {
	counts := map[string]int{}
	for name, f := range fixtures {
		counts[name] = len(f.receipts())
	}
	return counts
}

// Body schemas of the fixture writes. They state the request shape the
// fixtures accept; business rules such as known items, ownership and the
// quantity range stay with the fixtures.
const (
	ordersCreateSchema = `{"type":"object","additionalProperties":false,"required":["customer","item","quantity"],"properties":{` +
		`"customer":{"type":"string","minLength":1,"maxLength":64},` +
		`"item":{"type":"string","minLength":1,"maxLength":64},` +
		`"quantity":{"type":"integer"}}}`
	inventoryAdjustSchema = `{"type":"object","additionalProperties":false,"required":["delta"],"properties":{` +
		`"delta":{"type":"integer","minimum":-100,"maximum":100}}}`
)

// fixtureRate is the rate rule on every authenticated fixture operation. It
// is wide enough that no catalogue replay reaches it; rate tests use their
// own, tighter rules.
func fixtureRate() *RateRule {
	return &RateRule{Algorithm: "sliding-window", Count: 100, WindowSeconds: 60, Key: "subject"}
}

// fixturePolicy is the gateway policy for the three fixtures. It is written
// here by hand from the fixture contract, not generated from the catalogue,
// so a comparison between the two is meaningful.
func fixturePolicy(fixtures map[string]*fixtureProcess) Policy {
	return Policy{
		SchemaVersion: schemaVersion,
		Profile:       profileCore,
		Upstreams: []Upstream{
			{ID: "orders-svc", Dial: fixtures["orders"].addr},
			{ID: "inventory-svc", Dial: fixtures["inventory"].addr},
			{ID: "reports-svc", Dial: fixtures["reports"].addr},
		},
		APIs: []API{
			{ID: "orders", Upstream: "orders-svc"},
			{ID: "inventory", Upstream: "inventory-svc"},
			{ID: "reports", Upstream: "reports-svc"},
		},
		Operations: []Operation{
			{ID: "orders.list", API: "orders", Method: "GET", Target: "/orders", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "orders.get", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "orders.create", API: "orders", Method: "POST", Target: "/orders", Access: "allow", Auth: "required",
				Body: jsonBody(1024, ordersCreateSchema), Rate: fixtureRate()},
			{ID: "inventory.health", API: "inventory", Method: "GET", Target: "/inventory/health", Access: "allow", Auth: "none"},
			{ID: "inventory.get", API: "inventory", Method: "GET", Target: "/inventory/items/*", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "inventory.adjust", API: "inventory", Method: "PATCH", Target: "/inventory/items/*", Access: "allow", Auth: "required",
				Body: jsonBody(1024, inventoryAdjustSchema), Rate: fixtureRate()},
			{ID: "reports.sales", API: "reports", Method: "GET", Target: "/reports/sales", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "reports.summary", API: "reports", Method: "GET", Target: "/reports/summary", Access: "allow", Auth: "required", Rate: fixtureRate()},
		},
		Roles: []Role{{ID: "CUSTOMER"}, {ID: "STAFF"}, {ID: "MANAGER"}},
		Permits: []Permit{
			{Role: "STAFF", Operation: "orders.list"},
			{Role: "MANAGER", Operation: "orders.list"},
			{Role: "CUSTOMER", Operation: "orders.get"},
			{Role: "STAFF", Operation: "orders.get"},
			{Role: "CUSTOMER", Operation: "orders.create"},
			{Role: "STAFF", Operation: "inventory.get"},
			{Role: "MANAGER", Operation: "inventory.get"},
			{Role: "STAFF", Operation: "inventory.adjust"},
			{Role: "MANAGER", Operation: "reports.sales"},
			{Role: "STAFF", Operation: "reports.summary"},
			{Role: "MANAGER", Operation: "reports.summary"},
		},
	}
}

// bearer is the Authorization header pair carrying a valid token for subject.
func bearer(t *testing.T, subject string) []string {
	t.Helper()
	return []string{"Authorization", "Bearer " + issuer.tokenFor(t, subject)}
}

// catalogueRoles gives each catalogue subject's roles in the form the gateway
// forwards them: sorted and comma-separated.
func catalogueRoles(c fixture.Catalogue) map[string]string {
	roles := map[string]string{}
	for _, s := range c.Subjects {
		roles[s.ID] = strings.Join(slices.Sorted(slices.Values(s.Roles)), ",")
	}
	return roles
}

func loadCatalogue(t *testing.T) fixture.Catalogue {
	t.Helper()
	c, err := fixture.LoadCatalogue()
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	return c
}

// stepResult is what one catalogue step produced, as seen by the client, the
// fixture and the audit log.
type stepResult struct {
	Status    int
	Operation string // operation the fixture ran
	Effect    string
}

// runScenarios replays the catalogue sequence through the gateway, each step
// with a genuine token for its subject, and checks every step against the
// catalogue's expected fixture outcome, the fixture's own receipt and the
// gateway's event for the same request.
func runScenarios(t *testing.T, e *env, fixtures map[string]*fixtureProcess, c fixture.Catalogue) []stepResult {
	t.Helper()
	apiOf := map[string]string{}
	for _, op := range c.Operations {
		apiOf[op.ID] = op.API
	}
	roles := catalogueRoles(c)
	generation := e.status().Generation

	var results []stepResult
	for _, sc := range c.Scenarios {
		target := fixtures[apiOf[sc.Operation]]
		before := receiptCounts(fixtures)
		stateBefore := target.state().SHA256

		contentType := ""
		if len(sc.Body) > 0 {
			contentType = "application/json"
		}
		var header []string
		if sc.Subject != "" {
			header = bearer(t, sc.Subject)
		}
		resp, body := e.exchange(sc.Method, sc.Path, contentType, string(sc.Body), header...)

		after := receiptCounts(fixtures)
		if sc.GatewayStage != "" {
			checkGatewayRefusal(t, e, sc, resp.StatusCode, body, before, after, target.state().SHA256 != stateBefore)
			results = append(results, stepResult{Status: resp.StatusCode})
			continue
		}
		for name := range after {
			want := before[name]
			if name == target.name {
				want++
			}
			if after[name] != want {
				t.Fatalf("%s: receipts %v -> %v, want exactly one new receipt at %s", sc.Name, before, after, target.name)
			}
		}
		receipt := target.receipts()[after[target.name]-1]
		changed := target.state().SHA256 != stateBefore

		switch {
		case resp.StatusCode != sc.Status:
			t.Errorf("%s: status %d, want %d (%s)", sc.Name, resp.StatusCode, sc.Status, body)
		case resp.Header.Get("X-Service") != target.name:
			t.Errorf("%s: answered by %q, want %s", sc.Name, resp.Header.Get("X-Service"), target.name)
		case receipt.Operation != sc.Operation:
			t.Errorf("%s: fixture ran %q, want %q", sc.Name, receipt.Operation, sc.Operation)
		case receipt.Generation != generation || receipt.RequestID == "":
			t.Errorf("%s: receipt generation %q request %q, want generation %s", sc.Name, receipt.Generation, receipt.RequestID, generation)
		case receipt.Authorization:
			t.Errorf("%s: a credential reached the fixture", sc.Name)
		case receipt.Subject != sc.Subject || receipt.Roles != roles[sc.Subject]:
			t.Errorf("%s: fixture saw subject %q roles %q, want %q %q", sc.Name, receipt.Subject, receipt.Roles, sc.Subject, roles[sc.Subject])
		case changed != sc.ChangesState || receipt.Effect != sc.Effect:
			t.Errorf("%s: changed %v effect %q, want %v %q", sc.Name, changed, receipt.Effect, sc.ChangesState, sc.Effect)
		case len(sc.Response) > 0 && !sameJSON(body, sc.Response):
			t.Errorf("%s: response %s, want %s", sc.Name, body, sc.Response)
		}

		// The gateway must have selected the same operation the fixture ran.
		events := e.eventsWhen(func(events []map[string]any) bool {
			return findEvent(events, "request.forwarded", receipt.RequestID) != nil
		})
		forwarded := findEvent(events, "request.forwarded", receipt.RequestID)
		subject, _ := forwarded["subject"].(string)
		if forwarded == nil || forwarded["operation"] != receipt.Operation || forwarded["api"] != apiOf[sc.Operation] ||
			forwarded["generation"] != generation || subject != receipt.Subject {
			t.Errorf("%s: gateway event %v does not match fixture receipt %+v", sc.Name, forwarded, receipt)
		}
		results = append(results, stepResult{resp.StatusCode, receipt.Operation, receipt.Effect})
	}
	return results
}

// checkGatewayRefusal checks a step the gateway must refuse: the expected
// status, no receipt at any fixture, no state change, and a rejection event
// at the expected stage for the request the client was told about.
func checkGatewayRefusal(t *testing.T, e *env, sc fixture.Scenario, status int, body string, before, after map[string]int, changed bool) {
	t.Helper()
	if status != sc.GatewayStatus {
		t.Errorf("%s: status %d, want %d from the gateway (%s)", sc.Name, status, sc.GatewayStatus, body)
	}
	if !maps.Equal(before, after) || changed {
		t.Errorf("%s: receipts %v -> %v, state changed %v; the gateway must refuse it first", sc.Name, before, after, changed)
	}
	var reply struct {
		RequestID string `json:"request_id"`
	}
	json.Unmarshal([]byte(body), &reply)
	events := e.eventsWhen(func(events []map[string]any) bool {
		return findEvent(events, "request.rejected", reply.RequestID) != nil
	})
	rejected := findEvent(events, "request.rejected", reply.RequestID)
	if rejected == nil || rejected["stage"] != sc.GatewayStage || rejected["subject"] != sc.Subject {
		t.Errorf("%s: rejection event %v, want stage %s for subject %s", sc.Name, rejected, sc.GatewayStage, sc.Subject)
	}
}

func findEvent(events []map[string]any, kind, requestID string) map[string]any {
	for _, ev := range events {
		if ev["kind"] == kind && ev["request_id"] == requestID {
			return ev
		}
	}
	return nil
}

func sameJSON(got string, want []byte) bool {
	var a, b any
	if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal(want, &b) != nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// newFixtureEnv starts the three fixtures and a gateway serving fixturePolicy.
func newFixtureEnv(t *testing.T) (*env, map[string]*fixtureProcess) {
	t.Helper()
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
	e.start(binaries.plain)
	if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/former-01", `{"reason":"disabled test account"}`, "Idempotency-Key", "fixture-disabled-account"); code != http.StatusOK {
		t.Fatalf("seed containment: %d %s", code, body)
	}
	return e, fixtures
}

// Every catalogue operation runs
// through the real gateway and proxy on the intended fixture, with the
// expected business outcome and state change.
func TestCatalogueThroughGateway(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	c := loadCatalogue(t)

	instances := map[string]bool{}
	for _, f := range fixtures {
		instances[f.state().Instance] = true
	}
	if len(instances) != 3 {
		t.Fatalf("fixture instances %v, want three distinct processes", instances)
	}

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
}

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

// Reset restores the seed exactly, and replaying the
// catalogue from it reproduces the same outcomes and final state.
func TestResetAndReplayAreReproducible(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	c := loadCatalogue(t)
	seedHash := map[string]string{}
	for _, f := range c.Fixtures {
		seedHash[f.Service] = f.SeedSHA256
	}

	type run struct {
		results []stepResult
		final   map[string]string
	}
	var runs []run
	for range 2 {
		for name, f := range fixtures {
			s := f.reset()
			if s.SHA256 != seedHash[name] || len(f.receipts()) != 0 {
				t.Fatalf("reset of %s: state %s with %d receipts, want seed %s and none", name, s.SHA256, len(f.receipts()), seedHash[name])
			}
		}
		r := run{results: runScenarios(t, e, fixtures, c), final: map[string]string{}}
		for name, f := range fixtures {
			r.final[name] = f.state().SHA256
		}
		runs = append(runs, r)
	}

	if !slices.Equal(runs[0].results, runs[1].results) {
		t.Fatalf("replays differ:\n%v\n%v", runs[0].results, runs[1].results)
	}
	for name := range fixtures {
		if runs[0].final[name] != runs[1].final[name] {
			t.Errorf("fixture %s final state differs between replays", name)
		}
	}
	// The catalogue changes orders and inventory, and never reports.
	if runs[0].final["orders"] == seedHash["orders"] || runs[0].final["inventory"] == seedHash["inventory"] || runs[0].final["reports"] != seedHash["reports"] {
		t.Fatalf("final states %v against seeds %v", runs[0].final, seedHash)
	}
	t.Logf("final states after each replay: %v", runs[0].final)
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

// The hand-written gateway policy agrees with the independent catalogue on
// every operation, permit and subject, and the catalogue's role truth
// distinguishes profiles.
func TestPolicyAgreesWithCatalogue(t *testing.T) {
	c := loadCatalogue(t)
	p := fixturePolicy(map[string]*fixtureProcess{
		"orders": {addr: "127.0.0.1:1"}, "inventory": {addr: "127.0.0.1:2"}, "reports": {addr: "127.0.0.1:3"},
	})
	if _, err := Compile(p, 1, time.Time{}); err != nil {
		t.Fatalf("fixture policy does not compile: %v", err)
	}

	roles := map[string][]string{}
	for _, permit := range p.Permits {
		roles[permit.Operation] = append(roles[permit.Operation], permit.Role)
	}
	operations := map[string]Operation{}
	for _, op := range p.Operations {
		operations[op.ID] = op
	}
	if len(operations) != len(c.Operations) {
		t.Fatalf("policy has %d operations, catalogue %d", len(operations), len(c.Operations))
	}

	differing := 0
	for _, want := range c.Operations {
		got, ok := operations[want.ID]
		apiOK := ok && got.API == want.API && got.Method == want.Method && got.Target == want.Target
		if !apiOK || got.Auth != want.Auth || (got.Body != nil) != want.Body {
			t.Errorf("operation %s: policy %+v, catalogue %+v", want.ID, got, want)
		}
		gotRoles := slices.Sorted(slices.Values(roles[want.ID]))
		wantRoles := slices.Sorted(slices.Values(want.Roles))
		if !slices.Equal(gotRoles, wantRoles) {
			t.Errorf("operation %s: policy permits %v, catalogue truth %v", want.ID, gotRoles, wantRoles)
		}
		if len(want.Roles) > 0 && len(want.Roles) < len(c.Roles) {
			differing++
		}
	}
	if differing == 0 {
		t.Fatal("no operation distinguishes role profiles")
	}
	if !slices.Equal(slices.Sorted(slices.Values(c.Roles)), []string{"CUSTOMER", "MANAGER", "STAFF"}) {
		t.Fatalf("catalogue roles %v", c.Roles)
	}

	for _, want := range c.Subjects {
		gotRoles := testRoles(want.ID)
		wantRoles := slices.Sorted(slices.Values(want.Roles))
		if !slices.Equal(gotRoles, wantRoles) {
			t.Errorf("subject %s: issuer %v, catalogue %+v", want.ID, gotRoles, want)
		}
	}
	t.Logf("catalogue %s: %d operations, %d distinguish role profiles, %d subjects, %d scenarios",
		c.Version, len(c.Operations), differing, len(c.Subjects), len(c.Scenarios))
}
