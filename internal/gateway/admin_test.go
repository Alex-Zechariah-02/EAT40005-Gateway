// Admin API helpers and one-time local bootstrap.

package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPassword returns a fresh administrator password for one test.
func testPassword() string { return "pw-" + newToken() }

// installEnv creates a fresh installation with the operator install command,
// for a first administrator named root, and starts the gateway on it. It
// returns the enrolment secret the command printed.
func installEnv(t *testing.T, binary string) (*env, string) {
	t.Helper()
	e := newEnv(t)
	out, code := e.runOperator(binary, nil, "install", "--admin", "root")
	if code != 0 {
		t.Fatalf("install exited %d: %s", code, out)
	}
	secret := enrolmentSecret(t, out)
	e.start(binary)
	return e, secret
}

// enrol consumes an enrolment secret over the Admin API.
func (e *env) enrol(name, secret, password string) (int, string) {
	e.t.Helper()
	body := string(mustJSON(map[string]string{"name": name, "secret": secret, "password": password}))
	code, data, _ := e.adminSend(http.MethodPost, "/admin/enrol", body, "Origin", e.adminOrigin())
	return code, data
}

func (e *env) mustLogin(name, password string) adminSession {
	e.t.Helper()
	session, code := e.login(name, password)
	if code != http.StatusCreated {
		e.t.Fatalf("login as %s: %d", name, code)
	}
	return session
}

// as sends a request with an administrator's session, origin and CSRF token.
func (e *env) as(s adminSession, method, path, body string, header ...string) (int, string, http.Header) {
	e.t.Helper()
	return e.adminSend(method, path, body, append(s.headers(e), header...)...)
}

// addAdmin enrols another administrator through the operator command (with
// the gateway stopped) and the Admin API, and logs it in.
func (e *env) addAdmin(binary, name, permissions string) (adminSession, string) {
	e.t.Helper()
	e.stop()
	out, code := e.runOperator(binary, nil, "enrol", "--admin", name, "--permissions", permissions)
	if code != 0 {
		e.t.Fatalf("enrol %s exited %d: %s", name, code, out)
	}
	e.start(binary)
	password := testPassword()
	if code, body := e.enrol(name, enrolmentSecret(e.t, out), password); code != http.StatusCreated {
		e.t.Fatalf("enrolling %s: %d %s", name, code, body)
	}
	return e.mustLogin(name, password), password
}

// assertAbsent fails if any value appears in the audit files, the gateway's
// output or the database files.
func assertAbsent(t *testing.T, e *env, values ...string) {
	t.Helper()
	files, _ := filepath.Glob(e.auditLog + "*")
	databaseFiles, _ := filepath.Glob(e.database + "*")
	files = append(files, databaseFiles...)
	sources := map[string]string{"gateway output": e.output.String()}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err == nil {
			sources[filepath.Base(file)] = string(data)
		}
	}
	for name, content := range sources {
		for _, value := range values {
			if value != "" && strings.Contains(content, value) {
				t.Errorf("%s contains a secret value", name)
			}
		}
	}
}

// denyAllPolicy is the standard test catalogue with every operation denied
// and no roles, permits or subjects: a valid first or recovery base.
func (e *env) denyAllPolicy() Policy {
	p := e.policy(e.a, true)
	for i := range p.Operations {
		p.Operations[i].Access = "deny"
	}
	return p
}

func fromJSON[T any](t *testing.T, body string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return value
}

// The first administrator can only be created with the
// one-time secret of the local install command, exactly once.
func TestBootstrapIsLocalAndOneTime(t *testing.T) {
	e := newEnv(t)
	out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root")
	if code != 0 {
		t.Fatalf("install exited %d: %s", code, out)
	}
	secret := enrolmentSecret(t, out)

	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM administrator`); n != 0 {
		t.Fatalf("%d administrators exist before enrolment", n)
	}
	if got := queryString(t, e.database, `SELECT secret FROM enrolment WHERE administrator = 'root'`); got != tokenHash(secret) {
		t.Fatal("the stored enrolment is not the hash of the printed secret")
	}
	if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != modeBootstrap {
		t.Fatalf("installed mode %q", mode)
	}

	// A second install never replaces the database.
	before := fileSHA256(t, e.database)
	if out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "other"); code == 0 {
		t.Fatalf("second install succeeded: %s", out)
	}
	if fileSHA256(t, e.database) != before {
		t.Fatal("second install changed the database")
	}

	e.start(binaries.plain)
	if health := e.health(); health != modeBootstrap {
		t.Fatalf("health %q", health)
	}
	for _, path := range []string{"/a/x", "/d", "/admin/status"} {
		if code, _ := e.get(path); code != http.StatusServiceUnavailable {
			t.Fatalf("data plane %s before genesis: %d", path, code)
		}
	}

	// Without the secret, nothing creates or authenticates an administrator.
	password := testPassword()
	attempts := []struct{ name, secret string }{
		{"root", ""}, {"root", "wrong"}, {"root", newToken()}, {"other", secret}, {"admin", secret},
	}
	for _, a := range attempts {
		if code, body := e.enrol(a.name, a.secret, password); code != http.StatusUnauthorized {
			t.Errorf("enrol %s with secret %q: %d %s", a.name, a.secret, code, body)
		}
	}
	for _, name := range []string{"root", "admin", "harness"} {
		if _, code := e.login(name, password); code != http.StatusUnauthorized {
			t.Errorf("login as %s before enrolment: %d", name, code)
		}
	}
	for _, path := range []string{"/admin/status", "/admin/policy", "/admin/history", "/admin/candidates"} {
		if code, _, _ := e.adminSend(http.MethodGet, path, ""); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: %d", path, code)
		}
	}
	// A password below the minimum is refused without consuming the secret.
	if code, _ := e.enrol("root", secret, "short-password"); code != http.StatusBadRequest {
		t.Fatalf("short password: %d", code)
	}

	// The secret establishes exactly one administrator.
	code, body := e.enrol("root", secret, password)
	if code != http.StatusCreated || !strings.Contains(body, `"manage_policy"`) || !strings.Contains(body, `"read_history"`) {
		t.Fatalf("enrolment: %d %s", code, body)
	}
	session := e.mustLogin("root", password)
	if code, _, _ := e.as(session, http.MethodGet, "/admin/status", ""); code != http.StatusOK {
		t.Fatalf("status as root: %d", code)
	}

	// The consumed secret cannot be replayed, for the same or another name.
	for _, name := range []string{"root", "other"} {
		if code, _ := e.enrol(name, secret, testPassword()); code != http.StatusUnauthorized {
			t.Errorf("replayed secret for %s: %d", name, code)
		}
	}
	if _, code := e.login("root", password); code != http.StatusCreated {
		t.Fatal("the replay attempts changed the credential")
	}

	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM administrator`); n != 1 {
		t.Fatalf("%d administrators, want 1", n)
	}
	if n := queryInt(t, e.database, `SELECT credential FROM administrator WHERE name = 'root'`); n != 1 {
		t.Fatalf("credential version %d, want 1 (never overwritten)", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM enrolment`); n != 0 {
		t.Fatalf("%d enrolments remain", n)
	}
	hash := queryString(t, e.database, `SELECT password FROM administrator WHERE name = 'root'`)
	if !strings.HasPrefix(hash, argonPrefix) {
		t.Fatalf("stored credential is not an argon2id hash with the configured parameters: %.20s", hash)
	}
	assertAbsent(t, e, secret, password)
}
