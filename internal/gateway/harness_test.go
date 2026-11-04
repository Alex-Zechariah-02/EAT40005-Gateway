package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration tests run the real gateway binary as a separate process, so a
// crash can be injected by terminating it and the database is exercised
// across real restarts. The binaries include: the gateway and the fixture
// APIs, each also with fault-injection points compiled in, and the demo tool.
var binaries struct {
	plain, fault, fixture, fixtureFault, demo, services, accounts string
	race                                                          bool
}

// issuer signs the tokens of every test. Its public key is the trust anchor
// of every test gateway unless a test configures another.
var issuer = newTestIssuer(2048)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gateway-bin")
	if err != nil {
		panic(err)
	}
	binaries.plain = filepath.Join(dir, "gateway.exe")
	binaries.fault = filepath.Join(dir, "gateway-fault.exe")
	binaries.fixture = filepath.Join(dir, "fixture.exe")
	binaries.fixtureFault = filepath.Join(dir, "fixture-fault.exe")
	binaries.demo = filepath.Join(dir, "demo.exe")
	binaries.services = filepath.Join(dir, "services.exe")
	binaries.accounts = filepath.Join(dir, "accounts.exe")
	// GATEWAY_TEST_RACE builds the gateway and fixture processes with the
	// race detector, which requires cgo and a C toolchain.
	var race []string
	if os.Getenv("GATEWAY_TEST_RACE") != "" {
		race = []string{"-race"}
		binaries.race = true
	}
	builds := []struct{ out, tags, pkg string }{
		{binaries.plain, "", "../../cmd/gateway"},
		{binaries.fault, "faultinject", "../../cmd/gateway"},
		{binaries.fixture, "", "../../cmd/testfixture"},
		{binaries.fixtureFault, "faultinject", "../../cmd/testfixture"},
		{binaries.demo, "", "../../cmd/demo"},
		{binaries.services, "", "../../cmd/services"},
		{binaries.accounts, "", "../../cmd/accounts"},
	}
	for _, build := range builds {
		args := append([]string{"build", "-tags", build.tags, "-o", build.out}, race...)
		cmd := exec.Command("go", append(args, build.pkg)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n%s", build.pkg, err, out)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// receipt is what an upstream actually received.
type receipt struct {
	Method        string
	Target        string // raw request-target as received
	Path          string // decoded path
	Host          string // Host as received
	RequestID     string
	Generation    string
	Authorization bool
	Headers       http.Header
	Body          string
	Trailer       http.Header // request trailers, known once the body is read
}

// receiver is an owned upstream that records every request it receives.
// handle, when set, may take over the response; it returns true if it did.
type receiver struct {
	name   string
	addr   string
	server *http.Server
	mu     sync.Mutex
	got    []receipt
	handle func(w http.ResponseWriter, r *http.Request) bool
}

func newReceiver(t *testing.T, name string) *receiver {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{name: name, addr: listener.Addr().String()}
	rc.server = &http.Server{Handler: http.HandlerFunc(rc.serve)}
	go rc.server.Serve(listener)
	t.Cleanup(func() { rc.server.Close() })
	return rc
}

func (rc *receiver) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got = append(rc.got, receipt{
		Method:        r.Method,
		Target:        r.RequestURI,
		Path:          r.URL.Path,
		Host:          r.Host,
		RequestID:     r.Header.Get("X-Request-Id"),
		Generation:    r.Header.Get("X-Gateway-Generation"),
		Authorization: r.Header.Get("Authorization") != "",
		Headers:       r.Header.Clone(),
		Body:          string(body),
		Trailer:       r.Trailer.Clone(),
	})
	handle := rc.handle
	rc.mu.Unlock()
	if handle != nil && handle(w, r) {
		return
	}
	fmt.Fprintf(w, "%s ok", rc.name)
}

func (rc *receiver) receipts() []receipt {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]receipt(nil), rc.got...)
}

func (rc *receiver) count() int { return len(rc.receipts()) }

func (rc *receiver) reset() {
	rc.mu.Lock()
	rc.got = nil
	rc.mu.Unlock()
}

// env is one disposable gateway deployment.
type env struct {
	t         *testing.T
	dir       string
	database  string
	auditLog  string
	publicKey string // the issuer's verification key file
	dataAddr  string
	admin     string
	metrics   string
	config    string // the gateway's configuration file
	process   *exec.Cmd
	exited    chan error
	output    *logBuffer // the process's output, readable while it runs
	a, b, c   *receiver
	session   adminSession // the harness administrator's session, if any
	sessionMu sync.Mutex   // guards session: tests call the Admin API from several goroutines
	// interruptible starts the gateway in its own process group, so that
	// interrupt can reach it without reaching the test process.
	interruptible bool
}

type configOptions struct {
	headerTimeout string         // upstream response-header timeout; default 5s
	maxBodyBytes  int64          // global request body ceiling; default 16384
	jwt           any            // trust configuration; default the test issuer
	audit         map[string]any // extra gateway audit settings
	admin         map[string]any // extra gateway admin session settings
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		t:         t,
		dir:       dir,
		database:  filepath.Join(dir, "state.db"),
		auditLog:  filepath.Join(dir, "audit.log"),
		publicKey: filepath.Join(dir, "issuer.pem"),
		dataAddr:  freeAddr(t),
		admin:     freeAddr(t),
		metrics:   freeAddr(t),
		a:         newReceiver(t, "A"),
		b:         newReceiver(t, "B"),
		c:         newReceiver(t, "C"),
	}
	if err := os.WriteFile(e.publicKey, issuer.publicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	e.writeConfig(configOptions{})
	t.Cleanup(e.stop)
	return e
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func (e *env) writeConfig(opts configOptions) {
	if opts.headerTimeout == "" {
		opts.headerTimeout = "5s"
	}
	if opts.maxBodyBytes == 0 {
		opts.maxBodyBytes = 16384
	}
	if opts.jwt == nil {
		opts.jwt = testTrust(testIssuerName, testAudience, e.publicKey)
	}
	gateway := map[string]any{
		"database":       e.database,
		"audit_log":      e.auditLog,
		"admin_listen":   e.admin,
		"metrics_listen": e.metrics,
		"jwt":            opts.jwt,
	}
	maps.Copy(gateway, opts.audit)
	maps.Copy(gateway, opts.admin)
	settings := e.nativeSettings()
	settings["gateway"] = gateway
	section(settings, "data")["max_body_bytes"] = opts.maxBodyBytes
	section(settings, "upstream")["response_header_timeout"] = opts.headerTimeout
	e.writeNative(settings)
}

// start launches the gateway and waits until both listeners accept and it
// has reported its protected admission.
func (e *env) start(binary string, extraEnv ...string) {
	e.t.Helper()
	e.run(binary, extraEnv...)
	e.waitReady()
}

// nativeSettings is the configuration of e's deployment, as the JSON object
// writeNative writes. A test may change it first.
func (e *env) nativeSettings() map[string]any {
	return map[string]any{
		"version": nativeConfigVersion,
		"gateway": map[string]any{
			"database":       e.database,
			"audit_log":      e.auditLog,
			"admin_listen":   e.admin,
			"metrics_listen": e.metrics,
			"jwt":            testTrust(testIssuerName, testAudience, e.publicKey),
		},
		"data": map[string]any{"listen": e.dataAddr},
	}
}

// section returns the named object of native settings.
func section(settings map[string]any, name string) map[string]any {
	s, ok := settings[name].(map[string]any)
	if !ok {
		s = map[string]any{}
		settings[name] = s
	}
	return s
}

// writeNative writes settings as e's configuration file.
func (e *env) writeNative(settings map[string]any) {
	e.t.Helper()
	document, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		e.t.Fatal(err)
	}
	e.writeNativeFile(document)
}

// writeNativeFile writes content, exactly as given, as e's configuration
// file.
func (e *env) writeNativeFile(content []byte) {
	e.t.Helper()
	e.config = filepath.Join(e.dir, "gateway.json")
	if err := os.WriteFile(e.config, content, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// run launches the gateway's run command with e's configuration, without
// waiting for it to start.
func (e *env) run(binary string, extraEnv ...string) {
	e.t.Helper()
	e.launch(binary, []string{"run", "--config", e.config}, extraEnv...)
}

// waitOutput waits until the running gateway has written text.
func (e *env) waitOutput(text string) {
	e.t.Helper()
	waitFor(e.t, time.Now().Add(30*time.Second), fmt.Sprintf("%q in the gateway output", text), func() bool {
		return strings.Contains(e.output.String(), text)
	})
}

func (e *env) launch(binary string, args []string, extraEnv ...string) {
	e.t.Helper()
	e.stop()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(),
		"GATEWAY_FAULT=", "GATEWAY_HOLD_PATH=", "GATEWAY_HOLD_FILE=", "GATEWAY_HOLD_RELEASE=", "GATEWAY_RATE_CLOCK=", "GATEWAY_WALL_OFFSET=", "GATEWAY_AUDIT_HOLD=",
	)
	if binaries.race {
		// Stop at the first race so it cannot be lost when the test later
		// kills the process; checkRaces reports it.
		cmd.Env = append(cmd.Env, "GORACE=halt_on_error=1")
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	if e.interruptible {
		newProcessGroup(cmd)
	}
	e.output = &logBuffer{}
	cmd.Stdout, cmd.Stderr = e.output, e.output
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.process = cmd
	e.exited = make(chan error, 1)
	go func() { e.exited <- cmd.Wait() }()
}

func (e *env) waitReady() {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-e.exited:
			e.process = nil
			e.t.Fatalf("gateway exited during startup: %v\n%s", err, e.output)
		default:
		}
		// Listening is not readiness: the gateway reports its protected
		// admission once it has started.
		observed := strings.Contains(e.output.String(), `"msg":"protected admission"`)
		if observed && dialable(e.dataAddr) && dialable(e.admin) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("gateway not ready\n%s", e.output)
}

func dialable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// stop terminates the gateway abruptly. The design must tolerate this at any
// point, so tests never rely on a graceful shutdown.
func (e *env) stop() {
	e.sessionMu.Lock()
	e.session = adminSession{}
	e.sessionMu.Unlock()
	if e.process == nil {
		return
	}
	e.process.Process.Kill()
	<-e.exited
	e.process = nil
	e.checkRaces()
}

// checkRaces fails the test if the race detector reported anything in the
// gateway process. Call only after the process has exited, so the output is
// complete and no longer being written.
func (e *env) checkRaces() {
	e.t.Helper()
	if strings.Contains(e.output.String(), "WARNING: DATA RACE") {
		e.t.Errorf("race detected in gateway process:\n%s", e.output)
	}
}

// waitExit waits for a gateway that is expected to terminate itself.
func (e *env) waitExit() int {
	e.t.Helper()
	select {
	case <-e.exited:
		code := e.process.ProcessState.ExitCode()
		e.process = nil
		e.checkRaces()
		return code
	case <-time.After(30 * time.Second):
		e.t.Fatalf("gateway did not exit\n%s", e.output)
		return 0
	}
}

func (e *env) get(path string, header ...string) (int, string) {
	return e.request(http.MethodGet, path, "", "", header...)
}

func (e *env) request(method, path, contentType, body string, header ...string) (int, string) {
	e.t.Helper()
	resp, data := e.exchange(method, path, contentType, body, header...)
	return resp.StatusCode, data
}

// exchange sends one request to the data plane and returns the response,
// whose body has been read into the returned string.
func (e *env) exchange(method, path, contentType, body string, header ...string) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://"+e.dataAddr+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if body == "" {
		req.Body = http.NoBody
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

// raw sends bytes exactly as given, bypassing client-side normalisation.
func (e *env) raw(request string) (int, string) {
	e.t.Helper()
	resp, data := e.rawResponse(request)
	return resp.StatusCode, data
}

// rawResponse is raw returning the whole response, whose body has been read
// into the returned string.
func (e *env) rawResponse(request string) (*http.Response, string) {
	e.t.Helper()
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		e.t.Fatalf("reading response to %q: %v", request, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

// adminSession is an administrator's session cookie value and CSRF token.
type adminSession struct {
	cookie, csrf string
}

// headers are what a session request carries: the cookie, the CSRF token and
// the admin origin.
func (s adminSession) headers(e *env) []string {
	return []string{"Cookie", sessionCookie + "=" + s.cookie, csrfHeader, s.csrf, "Origin", e.adminOrigin()}
}

func (e *env) adminOrigin() string { return "http://" + e.admin }

// adminSend sends one request to the admin listener exactly as given: a
// non-empty body is sent as JSON, and header holds name/value pairs. A
// transport failure, such as a gateway that exited, returns status 0.
func (e *env) adminSend(method, path, body string, header ...string) (int, string, http.Header) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://"+e.admin+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if body == "" {
		req.Body = http.NoBody
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i] == "Host" {
			req.Host = header[i+1]
			continue
		}
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error(), nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

// login starts a session and returns it with the response status.
func (e *env) login(name, password string) (adminSession, int) {
	e.t.Helper()
	body := string(mustJSON(map[string]string{"name": name, "password": password}))
	code, data, header := e.adminSend(http.MethodPost, "/admin/session", body, "Origin", e.adminOrigin())
	if code != http.StatusCreated {
		return adminSession{}, code
	}
	var view struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal([]byte(data), &view)
	for _, cookie := range (&http.Response{Header: header}).Cookies() {
		if cookie.Name == sessionCookie {
			return adminSession{cookie: cookie.Value, csrf: view.CSRF}, code
		}
	}
	e.t.Fatalf("login answered 201 without a session cookie: %v", header)
	return adminSession{}, 0
}

// harnessCall sends a request as the harness administrator, logging in when
// there is no session and once more if the session has ended (a restart ends
// every session). Authentication is checked before anything else, so a
// request refused with 401 was never processed and is safe to resend.
func (e *env) harnessCall(method, path, body string, header ...string) (int, string, http.Header) {
	e.t.Helper()
	for range 2 {
		session, code := e.harnessSession()
		if code != http.StatusCreated {
			return code, "harness login failed", nil
		}
		code, data, h := e.adminSend(method, path, body, append(session.headers(e), header...)...)
		if code != http.StatusUnauthorized {
			return code, data, h
		}
		e.forgetSession(session)
	}
	return http.StatusUnauthorized, "harness session refused", nil
}

// harnessSession returns the harness administrator's session, logging in
// once when there is none; concurrent callers share that one login. The
// status is the login's, or 201 for a session already held.
func (e *env) harnessSession() (adminSession, int) {
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	if e.session.cookie == "" {
		session, code := e.login(harnessAdmin, harnessPassword)
		if code != http.StatusCreated {
			return adminSession{}, code
		}
		e.session = session
	}
	return e.session, http.StatusCreated
}

// forgetSession drops a refused session, unless another caller has already
// replaced it with a new one.
func (e *env) forgetSession(refused adminSession) {
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	if e.session.cookie == refused.cookie {
		e.session = adminSession{}
	}
}

// publish sends a whole policy through the test publication hook, which
// exists only in the fault-injection build.
func (e *env) publish(change Change) (Outcome, int) {
	e.t.Helper()
	code, body, _ := e.harnessCall(http.MethodPost, "/test/publish", string(mustJSON(change)))
	var outcome Outcome
	json.Unmarshal([]byte(body), &outcome)
	return outcome, code
}

// adminRaw sends a body exactly as given, as the harness administrator.
func (e *env) adminRaw(method, path, body string) (int, string) {
	e.t.Helper()
	code, data, _ := e.harnessCall(method, path, body)
	return code, data
}

type status struct {
	State       string    `json:"state"`
	Admission   string    `json:"admission"`
	AuditFailed bool      `json:"audit_failed"`
	Revision    int64     `json:"revision"`
	Generation  string    `json:"generation"`
	JWTKey      string    `json:"jwt_key_sha256"`
	Confirmed   int64     `json:"confirmed"`
	Provisional int64     `json:"provisional"`
	Deadline    time.Time `json:"deadline"`
	NotAfter    time.Time `json:"not_after"`
	Overrides   []int64   `json:"overrides"`
}

func (e *env) status() status {
	e.t.Helper()
	code, body, _ := e.harnessCall(http.MethodGet, "/admin/status", "")
	if code != http.StatusOK {
		e.t.Fatalf("status: %d %s", code, body)
	}
	var s status
	json.Unmarshal([]byte(body), &s)
	return s
}

// health reads the unauthenticated health state.
func (e *env) health() string {
	e.t.Helper()
	code, body, _ := e.adminSend(http.MethodGet, "/admin/health", "")
	var h struct {
		State string `json:"state"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &h) != nil {
		e.t.Fatalf("health: %d %s", code, body)
	}
	return h.State
}

// unavailableReason returns the reason of the last controller.unavailable
// event, for states in which no administrator can log in to read the status.
func (e *env) unavailableReason() string {
	e.t.Helper()
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "controller.unavailable") > 0 })
	reason := ""
	for _, ev := range events {
		if ev["kind"] == "controller.unavailable" {
			reason, _ = ev["reason"].(string)
		}
	}
	return reason
}

// runOperator runs a local operator command of the gateway binary against this
// environment's configuration. It returns the combined output, which holds
// any enrolment secret and any refusal message, and the exit code.
func (e *env) runOperator(binary string, extraEnv []string, command string, args ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command(binary, append([]string{command, "--config", e.config}, args...)...)
	cmd.Env = append(os.Environ(), "GATEWAY_FAULT=")
	cmd.Env = append(cmd.Env, extraEnv...)
	output, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatalf("running %s: %v", command, err)
	}
	e.t.Logf("%s %s --config %s %v: exit %d", filepath.Base(binary), command, e.config, args, code)
	return string(output), code
}

// enrolmentSecret extracts the secret from an install or enrol command's
// output line "enrolment secret for <name>...: <secret>".
func enrolmentSecret(t *testing.T, output string) string {
	t.Helper()
	for line := range strings.Lines(output) {
		if strings.HasPrefix(line, "enrolment secret for ") {
			index := strings.LastIndex(line, ": ")
			return strings.TrimSpace(line[index+2:])
		}
	}
	t.Fatalf("no enrolment secret in output %q", output)
	return ""
}

func (e *env) auditEvents() []map[string]any {
	e.t.Helper()
	data, err := os.ReadFile(e.auditLog)
	if err != nil {
		e.t.Fatal(err)
	}
	var events []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			e.t.Fatalf("audit line is not one JSON record: %q", line)
		}
		events = append(events, ev)
	}
	return events
}

// eventsWhen waits until the audit log satisfies done. Events that do not
// gate a decision are written asynchronously, so they may land shortly after
// the response.
func (e *env) eventsWhen(done func([]map[string]any) bool) []map[string]any {
	e.t.Helper()
	limit := time.Now().Add(5 * time.Second)
	for {
		events := e.auditEvents()
		if done(events) || time.Now().After(limit) {
			return events
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countKind(events []map[string]any, kind string) int {
	n := 0
	for _, ev := range events {
		if ev["kind"] == kind {
			n++
		}
	}
	return n
}

// assertReceipts checks every receiver, not only the intended one.
func (e *env) assertReceipts(a, b, c int) {
	e.t.Helper()
	if got := [3]int{e.a.count(), e.b.count(), e.c.count()}; got != [3]int{a, b, c} {
		e.t.Fatalf("receipts A,B,C = %v, want %v", got, [3]int{a, b, c})
	}
}

func (e *env) resetReceivers() {
	e.a.reset()
	e.b.reset()
	e.c.reset()
}

// policy builds the standard test policy: /a/* and POST /a/item go to the
// given upstream, /d is denied, and /b/* exists only when withB is set. Every
// operation is public, so routing behaviour is tested without tokens.
func (e *env) policy(aTarget *receiver, withB bool) Policy {
	p := Policy{
		SchemaVersion: schemaVersion,
		Profile:       profileCore,
		Upstreams:     []Upstream{{ID: "A", Dial: e.a.addr}, {ID: "B", Dial: e.b.addr}},
		APIs:          []API{{ID: "a", Upstream: aTarget.name}, {ID: "b", Upstream: "B"}, {ID: "d", Upstream: "A"}},
		Operations: []Operation{
			{ID: "read-a", API: "a", Method: "GET", Target: "/a/*", Access: "allow", Auth: "none"},
			{ID: "write-a", API: "a", Method: "POST", Target: "/a/item", Access: "allow", Auth: "none", Body: jsonBody(1024, anyObject)},
			{ID: "denied", API: "d", Method: "GET", Target: "/d", Access: "deny", Auth: "none"},
		},
	}
	if withB {
		p.Operations = append(p.Operations, Operation{ID: "read-b", API: "b", Method: "GET", Target: "/b/*", Access: "allow", Auth: "none"})
	}
	return p
}

// anyObject is a body schema that accepts any JSON object.
const anyObject = `{"type":"object","additionalProperties":true}`

// jsonBody declares a JSON body with the given size limit and schema.
func jsonBody(maxBytes int64, schema string) *BodyRule {
	return &BodyRule{MaxBytes: maxBytes, Schema: json.RawMessage(schema)}
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
