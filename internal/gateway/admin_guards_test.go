// Sign-in throttling and browser request guards.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

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
