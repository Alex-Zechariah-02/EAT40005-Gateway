// Integration harness Admin API client and operator commands.

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
