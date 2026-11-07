// Admin sessions, API client tokens and secret-safe session events.

package gateway

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

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

	recorded := []string{"admin.authenticated", "admin.rejected", "admin.logout", "transition.intent", "transition.committed"}
	events := e.eventsWhen(func(events []map[string]any) bool { return hasKinds(events, recorded...) })
	for _, ev := range events {
		kind := ev["kind"].(string)
		if (strings.HasPrefix(kind, "admin.") && kind != "admin.rejected") || strings.HasPrefix(kind, "transition.") {
			if ev["actor"] != "root" && ev["actor"] != "operator" {
				t.Errorf("event %s attributed to %v", kind, ev["actor"])
			}
		}
	}
	for _, kind := range recorded {
		if countKind(events, kind) == 0 {
			t.Errorf("no %s event", kind)
		}
	}
	assertAbsent(t, e, secret, password, changed, "a-wrong-password-value", root.cookie, root.csrf)
}
