package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

// An interrupted bootstrap leaves either the pending enrolment or
// the committed administrator, never a partial one. An interrupted install
// leaves a database that neither startup nor install accepts.
func TestInterruptedBootstrap(t *testing.T) {
	for _, tc := range []struct {
		point     string
		committed bool
	}{
		{"enrol-before-commit", false},
		{"enrol-after-commit", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			out, _ := e.runOperator(binaries.plain, nil, "install", "--admin", "root")
			secret := enrolmentSecret(t, out)
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			password := testPassword()
			if code, body := e.enrol("root", secret, password); code != 0 {
				t.Fatalf("enrolment answered %d instead of crashing: %s", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}

			admins := queryInt(t, e.database, `SELECT COUNT(*) FROM administrator`)
			pending := queryInt(t, e.database, `SELECT COUNT(*) FROM enrolment`)
			if tc.committed != (admins == 1) || tc.committed == (pending == 1) || admins+pending != 1 {
				t.Fatalf("after the crash: %d administrators, %d pending enrolments", admins, pending)
			}

			e.start(binaries.plain)
			if tc.committed {
				e.mustLogin("root", password)
				if code, _ := e.enrol("root", secret, testPassword()); code != http.StatusUnauthorized {
					t.Fatalf("consumed secret accepted after restart: %d", code)
				}
				// The outcome event lost in the crash is recorded at startup.
				events := e.eventsWhen(func(events []map[string]any) bool {
					for _, ev := range events {
						if ev["kind"] == "transition.committed" && ev["reason"] == "reconciled" {
							return true
						}
					}
					return false
				})
				found := false
				for _, ev := range events {
					id, _ := ev["transition"].(string)
					found = found || (ev["kind"] == "transition.committed" && ev["reason"] == "reconciled" && strings.HasPrefix(id, "enrol:"))
				}
				if !found {
					t.Fatal("no reconciled outcome event for the enrolment")
				}
				return
			}
			if _, code := e.login("root", password); code != http.StatusUnauthorized {
				t.Fatalf("uncommitted enrolment authenticates: %d", code)
			}
			if code, body := e.enrol("root", secret, password); code != http.StatusCreated {
				t.Fatalf("retry after the interrupted enrolment: %d %s", code, body)
			}
			e.mustLogin("root", password)
		})
	}

	t.Run("install-before-commit", func(t *testing.T) {
		e := newEnv(t)
		if _, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=install-before-commit"}, "install", "--admin", "root"); code != crashExit {
			t.Fatalf("install exit code %d", code)
		}
		if n := queryInt(t, e.database, `PRAGMA user_version`); n != 0 {
			t.Fatalf("interrupted install left schema version %d", n)
		}
		e.start(binaries.plain)
		if health := e.health(); health != "unavailable" {
			t.Fatalf("health %q", health)
		}
		// Depending on what reached the main file, it is empty or holds no schema.
		if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 0") && !strings.Contains(reason, "missing or empty") {
			t.Fatalf("reason %q", reason)
		}
		e.stop()
		if out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root"); code == 0 {
			t.Fatalf("install over the interrupted file succeeded: %s", out)
		}
	})
}

// A password change and a local recovery enrolment replace the
// credential and end every session based on the old one.
func TestCredentialRotationAndRecovery(t *testing.T) {
	e, secret := installEnv(t, binaries.plain)
	password := testPassword()
	if code, body := e.enrol("root", secret, password); code != http.StatusCreated {
		t.Fatalf("enrolment: %d %s", code, body)
	}
	first, second := e.mustLogin("root", password), e.mustLogin("root", password)

	changed := testPassword()
	wrong := string(mustJSON(map[string]string{"current_password": "not-the-password", "new_password": changed}))
	if code, _, _ := e.as(first, http.MethodPut, "/admin/password", wrong); code != http.StatusUnauthorized {
		t.Fatalf("password change with a wrong current password: %d", code)
	}
	tooShort := string(mustJSON(map[string]string{"current_password": password, "new_password": "short"}))
	if code, _, _ := e.as(first, http.MethodPut, "/admin/password", tooShort); code != http.StatusBadRequest {
		t.Fatalf("short new password: %d", code)
	}
	change := string(mustJSON(map[string]string{"current_password": password, "new_password": changed}))
	if code, body, _ := e.as(first, http.MethodPut, "/admin/password", change); code != http.StatusNoContent {
		t.Fatalf("password change: %d %s", code, body)
	}
	for _, s := range []adminSession{first, second} {
		if code, _, _ := e.as(s, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
			t.Fatalf("session after password change: %d", code)
		}
	}
	if _, code := e.login("root", password); code != http.StatusUnauthorized {
		t.Fatalf("old password after change: %d", code)
	}
	current := e.mustLogin("root", changed)

	// Local recovery: the operator command refuses to run beside the gateway.
	if out, code := e.runOperator(binaries.plain, nil, "enrol", "--admin", "root", "--permissions", "read"); code == 0 ||
		!strings.Contains(out, "held by another gateway process") {
		t.Fatalf("enrol while the gateway runs: %d %s", code, out)
	}
	e.stop()
	out, code := e.runOperator(binaries.plain, nil, "enrol", "--admin", "root", "--permissions", "read")
	if code != 0 {
		t.Fatalf("enrol exited %d: %s", code, out)
	}
	recovery := enrolmentSecret(t, out)
	e.start(binaries.plain)
	current = e.mustLogin("root", changed) // valid until the enrolment is consumed

	recovered := testPassword()
	if code, body := e.enrol("root", recovery, recovered); code != http.StatusCreated || !strings.Contains(body, `["read"]`) {
		t.Fatalf("recovery enrolment: %d %s", code, body)
	}
	if code, _, _ := e.as(current, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
		t.Fatalf("session after recovery: %d", code)
	}
	if _, code := e.login("root", changed); code != http.StatusUnauthorized {
		t.Fatalf("replaced password after recovery: %d", code)
	}
	reader := e.mustLogin("root", recovered)
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/status", ""); code != http.StatusOK {
		t.Fatalf("recovered administrator reads: %d", code)
	}
	if code, _, _ := e.as(reader, http.MethodPost, "/admin/candidates", `{"policy":{}}`, "If-Match", `"r0"`, "Idempotency-Key", "recovered-reader-1"); code != http.StatusForbidden {
		t.Fatalf("recovered read-only administrator stages a candidate: %d", code)
	}

	// An expired enrolment is refused.
	e.stop()
	out, _ = e.runOperator(binaries.plain, nil, "enrol", "--admin", "late", "--permissions", "read")
	late := enrolmentSecret(t, out)
	db := inspect(t, e.database)
	if _, err := db.Exec(`UPDATE enrolment SET expires_at = ? WHERE administrator = 'late'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.start(binaries.plain)
	if code, _ := e.enrol("late", late, testPassword()); code != http.StatusUnauthorized {
		t.Fatalf("expired enrolment: %d", code)
	}
	assertAbsent(t, e, password, changed, recovered, recovery, late)
}

// Sessions are server-side, unpredictable,
// bounded in time and never adopted from the client.
func TestAdminSessions(t *testing.T) {
	t.Run("issued session", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.plain)
		login := string(mustJSON(map[string]string{"name": harnessAdmin, "password": harnessPassword}))
		code, body, header := e.adminSend(http.MethodPost, "/admin/session", login, "Origin", e.adminOrigin())
		if code != http.StatusCreated {
			t.Fatalf("login: %d %s", code, body)
		}
		setCookie := header.Values("Set-Cookie")
		if len(setCookie) != 1 {
			t.Fatalf("Set-Cookie %v", setCookie)
		}
		for _, attribute := range []string{sessionCookie + "=", "; Path=/", "; HttpOnly", "; Secure", "; SameSite=Strict"} {
			if !strings.Contains(setCookie[0], attribute) {
				t.Errorf("cookie lacks %q", attribute)
			}
		}
		if strings.Contains(strings.ToLower(setCookie[0]), "domain=") {
			t.Error("cookie names a domain")
		}
		cookie := (&http.Response{Header: header}).Cookies()[0].Value
		raw, err := base64.RawURLEncoding.DecodeString(cookie)
		if err != nil || len(raw) != 32 {
			t.Fatalf("session token is not 32 random bytes: %d %v", len(raw), err)
		}
		view := fromJSON[map[string]any](t, body)
		if view["name"] != harnessAdmin || view["csrf_token"] == cookie || len(view["csrf_token"].(string)) != 43 {
			t.Fatalf("session view %v", view)
		}
		if strings.Contains(body, "argon2") || strings.Contains(body, harnessPassword) {
			t.Fatal("session view discloses the credential")
		}
		again := e.mustLogin(harnessAdmin, harnessPassword)
		if again.cookie == cookie || again.csrf == view["csrf_token"] {
			t.Fatal("a second login reused the token")
		}
	})

	t.Run("wrong credentials", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.plain)
		var bodies []string
		for _, credentials := range []map[string]string{
			{"name": harnessAdmin, "password": "not-the-password-at-all"},
			{"name": "nobody", "password": harnessPassword},
		} {
			code, body, header := e.adminSend(http.MethodPost, "/admin/session", string(mustJSON(credentials)), "Origin", e.adminOrigin())
			if code != http.StatusUnauthorized || header.Get("Set-Cookie") != "" {
				t.Fatalf("login %v: %d", credentials["name"], code)
			}
			bodies = append(bodies, body)
		}
		if bodies[0] != bodies[1] || strings.TrimSpace(bodies[0]) != `{"error":"unauthenticated"}` {
			t.Fatalf("refusals differ or disclose detail: %q %q", bodies[0], bodies[1])
		}
		if code, _, _ := e.adminSend(http.MethodPost, "/admin/session", `{"name":"harness"}`, "Origin", e.adminOrigin()); code != http.StatusUnauthorized {
			t.Fatalf("login without password: %d", code)
		}
		if code, _, _ := e.adminSend(http.MethodPost, "/admin/session", `{"name":`, "Origin", e.adminOrigin()); code != http.StatusBadRequest {
			t.Fatalf("malformed login: %d", code)
		}
		events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "admin.rejected") >= 3 })
		for _, ev := range events {
			if ev["kind"] == "admin.rejected" && (ev["actor"] != nil || ev["reason"] != "bad_credentials") {
				t.Fatalf("refusal event %v", ev)
			}
		}
		assertAbsent(t, e, "not-the-password-at-all", harnessPassword)
	})

	t.Run("forged and tampered tokens", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.plain)
		valid := e.mustLogin(harnessAdmin, harnessPassword)
		tampered := []byte(valid.cookie)
		tampered[len(tampered)-1] ^= 1
		for _, token := range []string{"", "x", strings.Repeat("A", 43), string(tampered), valid.cookie + "A", newToken(), valid.csrf} {
			code, body, _ := e.adminSend(http.MethodGet, "/admin/status", "", "Cookie", sessionCookie+"="+token)
			if code != http.StatusUnauthorized || strings.Contains(body, "revision") {
				t.Fatalf("token %q: %d %s", token, code, body)
			}
		}
		// A token chosen by the client is never adopted at login.
		chosen := newToken()
		code, _, header := e.adminSend(http.MethodPost, "/admin/session",
			string(mustJSON(map[string]string{"name": harnessAdmin, "password": harnessPassword})),
			"Origin", e.adminOrigin(), "Cookie", sessionCookie+"="+chosen)
		if code != http.StatusCreated || strings.Contains(header.Get("Set-Cookie"), chosen) {
			t.Fatalf("login with a chosen token: %d", code)
		}
		if code, _, _ := e.adminSend(http.MethodGet, "/admin/status", "", "Cookie", sessionCookie+"="+chosen); code != http.StatusUnauthorized {
			t.Fatalf("chosen token after login: %d", code)
		}
	})

	t.Run("logout and expiry", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.writeConfig(configOptions{admin: map[string]any{"admin_idle_timeout": "1s", "admin_session_lifetime": "4s"}})
		e.start(binaries.plain)

		ended := e.mustLogin(harnessAdmin, harnessPassword)
		if code, _, _ := e.as(ended, http.MethodDelete, "/admin/session", ""); code != http.StatusNoContent {
			t.Fatalf("logout: %d", code)
		}
		if code, _, _ := e.as(ended, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
			t.Fatalf("session after logout: %d", code)
		}

		idle := e.mustLogin(harnessAdmin, harnessPassword)
		time.Sleep(1500 * time.Millisecond)
		if code, _, _ := e.as(idle, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
			t.Fatalf("session after the idle timeout: %d", code)
		}

		active := e.mustLogin(harnessAdmin, harnessPassword)
		started := time.Now()
		for {
			time.Sleep(400 * time.Millisecond)
			code, _, _ := e.as(active, http.MethodGet, "/admin/session", "")
			elapsed := time.Since(started)
			if code == http.StatusOK && elapsed < 4*time.Second {
				continue
			}
			if code != http.StatusUnauthorized || elapsed < 3800*time.Millisecond {
				t.Fatalf("after %s of activity: %d", elapsed, code)
			}
			break
		}
	})

	t.Run("restart ends sessions", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.plain)
		before := e.mustLogin(harnessAdmin, harnessPassword)
		e.start(binaries.plain)
		if code, _, _ := e.as(before, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
			t.Fatalf("session after restart: %d", code)
		}
		e.mustLogin(harnessAdmin, harnessPassword)
	})

	t.Run("listener and settings", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(e *env)
			cause string
		}{
			{"wildcard listener", func(e *env) { e.admin = "0.0.0.0:" + port(e.admin) }, "loopback"},
			{"host name listener", func(e *env) { e.admin = "localhost:" + port(e.admin) }, "loopback"},
			{"idle timeout too short", func(e *env) {}, "admin_idle_timeout"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t)
				seed(t, e.database, e.policy(e.a, false), seedOptions{})
				tc.setup(e)
				options := configOptions{}
				if tc.cause == "admin_idle_timeout" {
					options.admin = map[string]any{"admin_idle_timeout": "500ms"}
				}
				e.writeConfig(options)
				e.run(binaries.plain)
				if code := e.waitExit(); code == 0 || !strings.Contains(e.output.String(), tc.cause) {
					t.Fatalf("exit %d\n%s", code, e.output)
				}
			})
		}
	})
}

// A valid API-client token is never an administrator credential,
// even when its subject has an administrator's name.
func TestAPIClientTokenIsNotAdminCredential(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	token := issuer.tokenFor(t, harnessAdmin)
	put := `{"reason":"test containment"}`
	for _, header := range [][]string{
		{"Authorization", "Bearer " + token},
		{"Cookie", sessionCookie + "=" + token},
		{"Authorization", "Bearer " + issuer.tokenFor(t, "alice")},
	} {
		if code, _, _ := e.adminSend(http.MethodGet, "/admin/status", "", header...); code != http.StatusUnauthorized {
			t.Errorf("status with %s: %d", header[0], code)
		}
		mutation := append([]string{"Origin", e.adminOrigin(), "If-Match", `"r1"`, "Idempotency-Key", "api-token-probe-01"}, header...)
		if code, _, _ := e.adminSend(http.MethodPut, "/admin/blocked-accounts/mallory", put, mutation...); code != http.StatusUnauthorized {
			t.Errorf("membership change with %s: %d", header[0], code)
		}
	}
	if s := e.status(); s.Revision != 1 {
		t.Fatalf("revision %d after the probes", s.Revision)
	}
	// The same token is still an ordinary API credential on the data plane.
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatalf("data plane request with a valid token: %d", code)
	}
}

// Repeated failures lock one administrator name, a burst of
// failures stops every attempt, and concurrent logins stay isolated.
func TestLoginThrottling(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)
	_, secondPassword := e.addAdmin(binaries.plain, "second", "read")

	var wg sync.WaitGroup
	sessions := make(chan adminSession, 8)
	statuses := make(chan int, 8)
	for range 8 {
		wg.Go(func() {
			s, code := e.login("second", secondPassword)
			statuses <- code
			if code == http.StatusCreated {
				sessions <- s
			}
		})
	}
	wg.Wait()
	close(sessions)
	close(statuses)
	for code := range statuses {
		if code != http.StatusCreated && code != http.StatusServiceUnavailable {
			t.Fatalf("concurrent login: %d", code)
		}
	}
	seen := map[string]bool{}
	for s := range sessions {
		if seen[s.cookie] {
			t.Fatal("two concurrent logins share a session")
		}
		seen[s.cookie] = true
		if code, body, _ := e.as(s, http.MethodGet, "/admin/session", ""); code != http.StatusOK || !strings.Contains(body, s.csrf) {
			t.Fatalf("concurrent session: %d", code)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no concurrent login succeeded")
	}

	for range maxNameFailures {
		if _, code := e.login(harnessAdmin, "wrong-password-guess"); code != http.StatusUnauthorized {
			t.Fatalf("failed login: %d", code)
		}
	}
	if _, code := e.login(harnessAdmin, harnessPassword); code != http.StatusTooManyRequests {
		t.Fatalf("correct password on a locked name: %d", code)
	}
	if _, code := e.login("second", secondPassword); code != http.StatusCreated {
		t.Fatalf("another name while one is locked: %d", code)
	}

	for i := maxNameFailures; i < maxWindowFailures; i++ {
		if _, code := e.login(fmt.Sprintf("guess-%d", i), "wrong-password-guess"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	if _, code := e.login("second", secondPassword); code != http.StatusTooManyRequests {
		t.Fatalf("login after %d failures in the window: %d", maxWindowFailures, code)
	}
	events := e.eventsWhen(func(events []map[string]any) bool {
		throttled := false
		for _, ev := range events {
			throttled = throttled || ev["reason"] == "throttled"
		}
		return throttled
	})
	reasons := map[string]bool{}
	for _, ev := range events {
		if ev["kind"] == "admin.rejected" {
			reasons[ev["reason"].(string)] = true
		}
	}
	if !reasons["locked"] || !reasons["throttled"] || !reasons["bad_credentials"] {
		t.Fatalf("refusal reasons recorded: %v", reasons)
	}
	assertAbsent(t, e, "wrong-password-guess", secondPassword)
}

// State-changing requests need the admin origin and the
// session's CSRF token, every request needs the admin host, and no header
// can claim an identity or permission.
func TestBrowserRequestGuards(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	reader, _ := e.addAdmin(binaries.plain, "reader", "read")
	writer := e.mustLogin(harnessAdmin, harnessPassword)
	other := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()

	put := `{"reason":"test containment"}`
	cookie := []string{"Cookie", sessionCookie + "=" + writer.cookie}
	origin := []string{"Origin", e.adminOrigin()}
	csrf := []string{csrfHeader, writer.csrf}
	cases := []struct {
		name   string
		header []string
	}{
		{"no origin", slices.Concat(cookie, csrf)},
		{"another site", slices.Concat(cookie, csrf, []string{"Origin", "https://evil.example"})},
		{"opaque origin", slices.Concat(cookie, csrf, []string{"Origin", "null"})},
		{"same host, another port", slices.Concat(cookie, csrf, []string{"Origin", "http://127.0.0.1:1"})},
		{"same port, another host name", slices.Concat(cookie, csrf, []string{"Origin", "http://localhost:" + port(e.admin)})},
		{"no CSRF token", slices.Concat(cookie, origin)},
		{"wrong CSRF token", slices.Concat(cookie, origin, []string{csrfHeader, newToken()})},
		{"another session's CSRF token", slices.Concat(cookie, origin, []string{csrfHeader, other.csrf})},
	}
	for i, tc := range cases {
		header := append(tc.header, "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("browser-guard-%04d", i))
		if code, body, _ := e.adminSend(http.MethodPut, "/admin/blocked-accounts/mallory", put, header...); code != http.StatusForbidden {
			t.Errorf("%s: %d %s", tc.name, code, body)
		}
	}
	for _, host := range []string{"localhost:" + port(e.admin), "evil.example", "127.0.0.1"} {
		header := append(writer.headers(e), "Host", host)
		if code, _, _ := e.adminSend(http.MethodGet, "/admin/status", "", header...); code != http.StatusBadRequest {
			t.Errorf("Host %s: %d", host, code)
		}
	}
	login := string(mustJSON(map[string]string{"name": harnessAdmin, "password": harnessPassword}))
	for _, header := range [][]string{nil, {"Origin", "https://evil.example"}} {
		if code, _, _ := e.adminSend(http.MethodPost, "/admin/session", login, header...); code != http.StatusForbidden {
			t.Errorf("login with origin %v: %d", header, code)
		}
	}
	code, _, header := e.adminSend(http.MethodOptions, "/admin/status", "", "Origin", "https://evil.example", "Access-Control-Request-Method", "GET")
	if code != http.StatusMethodNotAllowed {
		t.Errorf("preflight: %d", code)
	}
	for name := range header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("CORS header %s sent", name)
		}
	}

	// Identity and permission claims in headers grant nothing.
	forged := []string{"X-Forwarded-For", "127.0.0.1", "X-Actor", harnessAdmin, "X-Admin", "true", "X-Role", "manage_subjects",
		"X-Permissions", "manage_subjects", "X-Gateway-Subject", harnessAdmin, "Forwarded", "for=127.0.0.1"}
	if code, _, _ := e.adminSend(http.MethodGet, "/admin/status", "", forged...); code != http.StatusUnauthorized {
		t.Errorf("anonymous request with forged identity headers: %d", code)
	}
	readerHeader := append(append(reader.headers(e), forged...), "If-Match", `"r1"`, "Idempotency-Key", "browser-guard-reader")
	if code, _, _ := e.adminSend(http.MethodPut, "/admin/blocked-accounts/mallory", put, readerHeader...); code != http.StatusForbidden {
		t.Errorf("reader with forged permission headers: %d", code)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "admin.denied") >= len(cases)+1 })
	denied := 0
	for _, ev := range events {
		if ev["kind"] == "admin.denied" && ev["reason"] == "permission" {
			denied++
			if ev["actor"] != "reader" {
				t.Errorf("denial attributed to %v", ev["actor"])
			}
		}
	}
	if denied != 1 {
		t.Errorf("%d permission denials recorded, want 1", denied)
	}

	if after := e.status(); after.Generation != before.Generation || after.Revision != 1 {
		t.Fatalf("refused requests changed the state: %+v", after)
	}
	if code, body, _ := e.harnessCall(http.MethodGet, "/admin/history", ""); code != http.StatusOK || strings.Contains(body, "membership") {
		t.Fatalf("history after refused requests: %d %s", code, body)
	}
}

// Every sensitive read needs a session, every operation
// its own permission, and no response carries a secret.
func TestPermissionsAreServerSide(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	code, body, _ := e.harnessCall(http.MethodPost, "/admin/candidates", string(mustJSON(map[string]any{"policy": e.protectedPolicy()})),
		"If-Match", `"r1"`, "Idempotency-Key", "permission-candidate")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	// Each addAdmin restarts the gateway, which ends earlier sessions, so all
	// administrators log in again after the last restart.
	_, readerPassword := e.addAdmin(binaries.plain, "reader", "read")
	_, subjectsPassword := e.addAdmin(binaries.plain, "subjects", "manage_subjects")
	auditor, _ := e.addAdmin(binaries.plain, "auditor", "read_history")
	reader := e.mustLogin("reader", readerPassword)
	subjects := e.mustLogin("subjects", subjectsPassword)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	sensitive := []string{"/admin/status", "/admin/policy", "/admin/revisions", "/admin/revisions/1", "/admin/blocked-accounts",
		"/admin/keys", "/admin/candidates", "/admin/candidates/1", "/admin/history", "/admin/history/x", "/admin/session", "/admin/events", "/admin/traffic", "/admin/administrators"}
	for _, path := range sensitive {
		code, body, _ := e.adminSend(http.MethodGet, path, "")
		if code != http.StatusUnauthorized || strings.TrimSpace(body) != `{"error":"unauthenticated"}` {
			t.Errorf("GET %s without a session: %d %s", path, code, body)
		}
	}

	type mutation struct{ method, path, body string }
	mutations := []mutation{
		{http.MethodPut, "/admin/blocked-accounts/mallory", `{"reason":"test containment"}`},
		{http.MethodDelete, "/admin/blocked-accounts/alice", ""},
		{http.MethodPost, "/admin/candidates", string(mustJSON(map[string]any{"policy": e.protectedPolicy()}))},
		{http.MethodDelete, "/admin/candidates/1", ""},
		{http.MethodPost, "/admin/genesis", `{"candidate":1,"checksum":"x"}`},
		{http.MethodPost, "/admin/recovery-base", `{"candidate":1,"checksum":"x"}`},
	}
	for i, m := range mutations {
		for _, who := range []adminSession{reader, auditor} {
			header := []string{"If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("permission-probe-%04d", i)}
			if code, _, _ := e.as(who, m.method, m.path, m.body, header...); code != http.StatusForbidden {
				t.Errorf("%s %s without its permission: %d", m.method, m.path, code)
			}
		}
	}
	if code, _, _ := e.as(subjects, http.MethodPost, "/admin/candidates", mutations[2].body, "If-Match", `"r1"`, "Idempotency-Key", "permission-subjects-1"); code != http.StatusForbidden {
		t.Errorf("manage_subjects stages a candidate: %d", code)
	}
	for who, paths := range map[string][]string{
		"reader":   {"/admin/history"},
		"subjects": {"/admin/status", "/admin/blocked-accounts", "/admin/history"},
		"auditor":  {"/admin/status", "/admin/policy"},
	} {
		session := map[string]adminSession{"reader": reader, "subjects": subjects, "auditor": auditor}[who]
		for _, path := range paths {
			if code, _, _ := e.as(session, http.MethodGet, path, ""); code != http.StatusForbidden {
				t.Errorf("%s GET %s: %d", who, path, code)
			}
		}
	}
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/blocked-accounts", ""); code != http.StatusOK {
		t.Errorf("reader lists subjects: %d", code)
	}
	if code, _, _ := e.as(auditor, http.MethodGet, "/admin/history", ""); code != http.StatusOK {
		t.Errorf("auditor reads history: %d", code)
	}
	if s := e.status(); s.Revision != 1 {
		t.Fatalf("refused mutations changed the revision to %d", s.Revision)
	}
	if code, body, _ := e.as(subjects, http.MethodPut, "/admin/blocked-accounts/carol", `{"reason":"test containment"}`,
		"If-Match", `"r1"`, "Idempotency-Key", "permission-subjects-2"); code != http.StatusOK {
		t.Fatalf("manage_subjects changes membership: %d %s", code, body)
	}

	// No read returns a credential, token, key or secret.
	forbidden := []string{"$argon2id", harnessPassword, harness.cookie, reader.cookie, reader.csrf, "BEGIN", "PRIVATE", "secret"}
	for _, path := range sensitive {
		_, body, _ := e.as(harness, http.MethodGet, path, "")
		for _, value := range forbidden {
			if strings.Contains(body, value) {
				t.Errorf("GET %s returns %q", path, value)
			}
		}
	}
	if s := e.status(); len(s.JWTKey) != 64 {
		t.Errorf("status key field %q is not a fingerprint", s.JWTKey)
	}
}

// Reads return the authoritative state with bounded pages.
func TestProtectedReads(t *testing.T) {
	e := newEnv(t)
	seeded := e.protectedPolicy()
	seed(t, e.database, seeded, seedOptions{})
	e.start(binaries.plain)
	before := e.status()
	if before.Revision != 1 || before.State != modeNormal {
		t.Fatalf("status %+v", before)
	}
	code, body, header := e.harnessCall(http.MethodGet, "/admin/policy", "")
	if code != http.StatusOK || header.Get("ETag") != `"r1"` {
		t.Fatalf("policy: %d %s", code, header.Get("ETag"))
	}
	revision := fromJSON[revisionInfo](t, body)
	if string(revision.Document) != string(mustJSON(seeded)) || revision.Origin != "genesis" || revision.Checksum != documentChecksum(revision.Document) {
		t.Fatalf("policy read differs from the confirmed revision: %+v", revision)
	}

	for i := 1; i <= 5; i++ {
		policy := e.protectedPolicy()
		policy.Roles = append(policy.Roles, Role{ID: fmt.Sprintf("OTHER%02d", i)})
		e.confirmPolicy(e.mustLogin(harnessAdmin, harnessPassword), policy, int64(i), fmt.Sprintf("read-revision-%04d", i))
	}
	var listed []int64
	for after, pages := "", 0; ; pages++ {
		path := "/admin/revisions?limit=3"
		if after != "" {
			path += "&after=" + after
		}
		code, body, _ := e.harnessCall(http.MethodGet, path, "")
		page := fromJSON[struct {
			Items []revisionInfo `json:"items"`
			Next  string         `json:"next"`
		}](t, body)
		if code != http.StatusOK || len(page.Items) > 3 || pages > 3 {
			t.Fatalf("page %s: %d %s", path, code, body)
		}
		for _, s := range page.Items {
			listed = append(listed, s.ID)
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if !slices.Equal(listed, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("listed %v", listed)
	}
	code, body, _ = e.harnessCall(http.MethodGet, "/admin/revisions?limit=100", "")
	revisions := fromJSON[struct {
		Items []revisionInfo `json:"items"`
	}](t, body)
	if code != http.StatusOK || len(revisions.Items) != 6 || revisions.Items[5].Origin != "publish" || revisions.Items[5].Actor != harnessAdmin {
		t.Fatalf("revisions: %d %s", code, body)
	}

	for _, path := range []string{"/admin/revisions?limit=0", "/admin/revisions?limit=101", "/admin/revisions?limit=x",
		"/admin/revisions?limit=99999999999999999999", "/admin/revisions?limit=1&limit=2", "/admin/revisions?sort=id",
		"/admin/revisions?after=-1", "/admin/revisions?after=01", "/admin/history?after=x", "/admin/candidates?limit=+5"} {
		if code, _, _ := e.harnessCall(http.MethodGet, path, ""); code != http.StatusBadRequest {
			t.Errorf("GET %s: %d", path, code)
		}
	}
	for _, path := range []string{"/admin/revisions/99", "/admin/revisions/0", "/admin/candidates/abc", "/admin/subjects/nobody", "/admin/history/none"} {
		if code, _, _ := e.harnessCall(http.MethodGet, path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, code)
		}
	}
}

// Bodies are bounded and strictly decoded, and
// refused input never reaches a reply or an event unescaped.
func TestAdminInputBounds(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	before := e.status()
	key := 0
	put := func(path, body string, header ...string) int {
		key++
		header = append([]string{"If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("input-bounds-%04d", key)}, header...)
		method := http.MethodPut
		if path == "/admin/candidates" {
			method = http.MethodPost
		}
		code, _, _ := e.harnessCall(method, path, body, header...)
		return code
	}
	cases := []struct {
		name, path, body string
		header           []string
		want             int
	}{
		{"oversized", "/admin/blocked-accounts/x", `{"reason":"` + strings.Repeat("R", 5000) + `"}`, nil, http.StatusRequestEntityTooLarge},
		{"text body", "/admin/blocked-accounts/x", `{"reason":"test containment"}`, []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType},
		{"repeated member", "/admin/blocked-accounts/x", `{"reason":"x","reason":"y"}`, nil, http.StatusBadRequest},
		{"case variant", "/admin/blocked-accounts/x", `{"Reason":"x"}`, nil, http.StatusBadRequest},
		{"unknown field", "/admin/blocked-accounts/x", `{"status":"active","roles":[],"admin":true}`, nil, http.StatusBadRequest},
		{"trailing data", "/admin/blocked-accounts/x", `{"reason":"test containment"} {}`, nil, http.StatusBadRequest},
		{"wrong type", "/admin/blocked-accounts/x", `{"reason":["WRITER"]}`, nil, http.StatusBadRequest},
		{"control character in id", "/admin/blocked-accounts/a%0Ab", `{"reason":"test containment"}`, nil, http.StatusBadRequest},
		{"overlong id", "/admin/blocked-accounts/" + strings.Repeat("s", 129), `{"reason":"test containment"}`, nil, http.StatusBadRequest},
		{"malformed If-Match", "/admin/candidates", `{"policy":{}}`, []string{"If-Match", `W/"r1"`}, http.StatusBadRequest},
		{"short idempotency key", "/admin/blocked-accounts/x", `{"reason":"test containment"}`, []string{"Idempotency-Key", "short"}, http.StatusBadRequest},
		{"invalid idempotency key", "/admin/blocked-accounts/x", `{"reason":"test containment"}`, []string{"Idempotency-Key", "has spaces in it ok?"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if code := put(tc.path, tc.body, tc.header...); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
	code, _, _ := e.harnessCall(http.MethodPost, "/admin/candidates", `{"policy":{}}`, "Idempotency-Key", "input-bounds-no-if-match")
	if code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d", code)
	}

	// Secret-looking input is neither echoed nor recorded.
	privateKey := "-----BEGIN PRIVATE KEY-----MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-----END PRIVATE KEY-----"
	token := issuer.tokenFor(t, "alice")
	if code := put("/admin/blocked-accounts/x", `{"status":"active","roles":[],"key":"`+privateKey+`"}`); code != http.StatusBadRequest {
		t.Errorf("unknown field holding a key: %d", code)
	}
	e.login(harnessAdmin, token)
	e.adminSend(http.MethodGet, "/admin/blocked-accounts/x%0A%7B%22kind%22%3A%22forged%22%7D", "")
	if after := e.status(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("refused input changed the state: %+v", after)
	}
	for _, ev := range e.auditEvents() { // fails on any line that is not one JSON record
		if ev["kind"] == "forged" {
			t.Fatal("an injected record appeared in the audit log")
		}
	}
	assertAbsent(t, e, "BEGIN PRIVATE KEY", token)
}

// Only the implemented administration operations exist. No route
// other than the confirmed-activation lifecycle activates a policy: the
// override route takes typed override requests only, and refuses a whole
// policy. No recommendation route answers as if it worked.
func TestOnlyVersionOperationsExist(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	before := e.status()
	probes := []struct{ method, path string }{
		{http.MethodPost, "/admin/candidates/1/activate"}, {http.MethodPost, "/admin/candidates/1/confirm"},
		{http.MethodPut, "/admin/candidates/1"}, {http.MethodPost, "/admin/confirm"},
		{http.MethodPost, "/admin/rollback"}, {http.MethodPost, "/admin/cpa/confirm"}, {http.MethodPost, "/admin/tpo"},
		{http.MethodPut, "/admin/overrides/1"}, {http.MethodPatch, "/admin/overrides/1"}, {http.MethodGet, "/admin/spa"},
		{http.MethodPost, "/admin/recommendations"}, {http.MethodPost, "/admin/backup"}, {http.MethodPost, "/admin/restore"},
		{http.MethodPut, "/admin/policy"}, {http.MethodPost, "/admin/policy"}, {http.MethodPost, "/admin/revisions"},
		{http.MethodPut, "/admin/roles/READER"}, {http.MethodPost, "/admin/permits"}, {http.MethodPost, "/admin/admins"},
		{http.MethodPost, "/admin/install"}, {http.MethodPost, "/publish"}, {http.MethodPost, "/test/publish"},
		{http.MethodPost, "/load"}, {http.MethodGet, "/config/"},
	}
	policy := string(mustJSON(map[string]any{"policy": e.protectedPolicy()}))
	for _, probe := range probes {
		code, _, _ := e.harnessCall(probe.method, probe.path, policy, "If-Match", `"r1"`, "Idempotency-Key", "future-route-probe-1")
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d", probe.method, probe.path, code)
		}
	}
	if code, body, _ := e.harnessCall(http.MethodPost, "/admin/overrides", policy, "If-Match", `"r1"`, "Idempotency-Key", "future-route-probe-2"); code != http.StatusBadRequest ||
		!strings.Contains(body, "bad_request") {
		t.Errorf("a whole policy sent as an override: %d %s", code, body)
	}
	if after := e.status(); after.Generation != before.Generation {
		t.Fatalf("a probe changed the generation: %+v", after)
	}
}

// The administrative events of a whole
// session carry the trusted actor and never a secret.
func TestAdminEventsAreSecretSafe(t *testing.T) {
	e, secret := installEnv(t, binaries.plain)
	password := testPassword()
	if code, _ := e.enrol("root", secret, password); code != http.StatusCreated {
		t.Fatal("enrolment failed")
	}
	root := e.mustLogin("root", password)
	e.login("root", "a-wrong-password-value")
	code, body, _ := e.as(root, http.MethodPost, "/admin/candidates", string(mustJSON(map[string]any{"policy": e.denyAllPolicy()})),
		"If-Match", `"r0"`, "Idempotency-Key", "secret-safe-candidate")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	changed := testPassword()
	e.as(root, http.MethodPut, "/admin/password", string(mustJSON(map[string]string{"current_password": password, "new_password": changed})))
	root = e.mustLogin("root", changed)
	e.as(root, http.MethodDelete, "/admin/session", "")

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "admin.logout") == 1 })
	for _, ev := range events {
		kind := ev["kind"].(string)
		if (strings.HasPrefix(kind, "admin.") && kind != "admin.rejected") || strings.HasPrefix(kind, "transition.") {
			if ev["actor"] != "root" && ev["actor"] != "operator" {
				t.Errorf("event %s attributed to %v", kind, ev["actor"])
			}
		}
	}
	for _, kind := range []string{"admin.authenticated", "admin.rejected", "admin.logout", "transition.intent", "transition.committed"} {
		if countKind(events, kind) == 0 {
			t.Errorf("no %s event", kind)
		}
	}
	assertAbsent(t, e, secret, password, changed, "a-wrong-password-value", root.cookie, root.csrf)
}
