// Override create, revoke and expiry lifecycle.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Each supported override changes enforcement at once, exactly as its type
// says, without touching the confirmed base; JWT authentication still
// applies to a granted permit. Overrides survive an unrelated permanent
// change, and revoking one removes that one only.
func TestOverrideLifecycle(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation

	// The base: bob (READER) reads /a and /b but may not write /a; alice
	// (WRITER) reads and writes /a.
	expect := func(want map[string]int, receipts [3]int) {
		t.Helper()
		e.resetReceivers()
		got := map[string]int{
			"bob POST /a/item":   e.probe("bob", http.MethodPost, "/a/item"),
			"bob GET /a/x":       e.probe("bob", http.MethodGet, "/a/x"),
			"bob GET /b/x":       e.probe("bob", http.MethodGet, "/b/x"),
			"alice GET /a/x":     e.probe("alice", http.MethodGet, "/a/x"),
			"alice POST /a/item": e.probe("alice", http.MethodPost, "/a/item"),
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("enforcement %v, want %v", got, want)
		}
		e.assertReceipts(receipts[0], receipts[1], receipts[2])
	}
	expect(map[string]int{"bob POST /a/item": 403, "bob GET /a/x": 200, "bob GET /b/x": 200, "alice GET /a/x": 200, "alice POST /a/item": 200}, [3]int{3, 1, 0})

	// An RBAC exception grants bob the write.
	before := time.Now()
	grant := e.mustOverride(harness, permitOverride(store.OverrideException, "READER", "write-a", 600), `"r1"`, "lifecycle-grant-0001")
	if grant.Override != 1 || grant.Target != "permit:READER:write-a" || grant.Revision != 1 || !strings.HasPrefix(grant.Generation, "r1+o1-") ||
		grant.Expires.Before(before.Add(600*time.Second).Add(-time.Millisecond)) || grant.Expires.After(time.Now().Add(600*time.Second)) {
		t.Fatalf("grant receipt %+v", grant)
	}
	expect(map[string]int{"bob POST /a/item": 200, "bob GET /a/x": 200, "bob GET /b/x": 200, "alice GET /a/x": 200, "alice POST /a/item": 200}, [3]int{4, 1, 0})
	for _, r := range e.a.receipts() {
		if r.Generation != grant.Generation {
			t.Fatalf("A received generation %s, want %s", r.Generation, grant.Generation)
		}
	}
	// The granted permit still needs a valid token of a known subject.
	e.resetReceivers()
	for _, header := range [][]string{nil, {"Authorization", "Bearer x.y.z"}, {"Authorization", "Bearer " + issuer.tokenFor(t, "mallory")},
		{"Authorization", "Bearer " + hmacToken(issuer.publicPEM(), claimsJSON(validClaims("bob")))}} {
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, append(header, "X-Gateway-Roles", "READER,WRITER")...)
		if code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("POST /a/item with %v: %d", header, code)
		}
	}
	// A body the schema refuses is still refused.
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `[1]`, bearer(t, "bob")...); code != http.StatusBadRequest {
		t.Fatalf("invalid body under the grant: %d", code)
	}
	e.assertReceipts(0, 0, 0)

	// An RBAC containment withdraws alice's read; a route containment closes
	// /b for everyone.
	contain := e.mustOverride(harness, permitOverride(store.OverrideContainment, "WRITER", "read-a", 600), `"r1"`, "lifecycle-contain-01")
	closed := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "lifecycle-close-0001")
	if !strings.HasPrefix(closed.Generation, "r1+o1.2.3-") {
		t.Fatalf("generation %s", closed.Generation)
	}
	expect(map[string]int{"bob POST /a/item": 200, "bob GET /a/x": 200, "bob GET /b/x": 403, "alice GET /a/x": 403, "alice POST /a/item": 200}, [3]int{3, 0, 0})

	// Inspection shows the active set and the generation serving it; the
	// confirmed base is still revision 1.
	view := e.activeOverrides(harness)
	if view.Revision != 1 || view.Confirmed != 1 || view.Generation != closed.Generation || !slices.Equal(view.GenerationOverrides, []int64{1, 2, 3}) ||
		len(view.Overrides) != 3 || view.Overrides[1].Target != "permit:WRITER:read-a" || view.Overrides[1].Status != store.OverrideActive ||
		view.Overrides[1].Actor != harnessAdmin || view.Overrides[1].Base != 1 || view.Overrides[1].Creation != contain.Transition {
		t.Fatalf("overrides view %+v", view)
	}
	if code, body, header := e.as(harness, http.MethodGet, "/admin/policy", ""); code != http.StatusOK || header.Get("ETag") != `"r1"` ||
		strings.Contains(body, `"role":"READER","operation":"write-a"`) {
		t.Fatalf("confirmed policy: %d %s", code, header.Get("ETag"))
	}
	if s := e.status(); s.Revision != 1 || s.Confirmed != 1 || s.Generation != closed.Generation {
		t.Fatalf("status %+v", s)
	}

	// An unrelated permanent change keeps every override.
	next := e.protectedPolicy()
	next.Roles = append(next.Roles, policy.Role{ID: "TRIAL"})
	next.Permits = append(next.Permits, policy.Permit{Role: "TRIAL", Operation: "read-a"})
	e.confirmPolicy(harness, next, 1, "lifecycle-permanent")
	s := e.status()
	if s.Confirmed != 2 || !strings.HasPrefix(s.Generation, "r2+o1.2.3-") {
		t.Fatalf("status after the membership change %+v", s)
	}
	expect(map[string]int{"bob POST /a/item": 200, "bob GET /a/x": 200, "bob GET /b/x": 403, "alice GET /a/x": 403, "alice POST /a/item": 200}, [3]int{3, 0, 0})
	if code := e.probe("carol", http.MethodGet, "/a/x"); code != http.StatusOK {
		t.Fatalf("new TRIAL permit applies independently of WRITER containment: %d", code)
	}

	// Revoking the grant removes it alone, over the current base (with
	// carol), never an earlier policy.
	code, revoked, body := e.revokeOverride(harness, grant.Override, "lifecycle-revoke-01")
	if code != http.StatusOK || revoked.Disposition != store.OverrideRevoked || revoked.Override != grant.Override || revoked.Revision != 2 ||
		!strings.HasPrefix(revoked.Generation, "r2+o2.3-") {
		t.Fatalf("revocation: %d %s", code, body)
	}
	expect(map[string]int{"bob POST /a/item": 403, "bob GET /a/x": 200, "bob GET /b/x": 403, "alice GET /a/x": 403, "alice POST /a/item": 200}, [3]int{2, 0, 0})
	if code := e.probe("carol", http.MethodGet, "/a/x"); code != http.StatusOK {
		t.Fatalf("carol after the revocation: %d", code)
	}
	// Retries and stale revocations.
	if code, again, body := e.revokeOverride(harness, grant.Override, "lifecycle-revoke-01"); code != http.StatusOK || again.Transition != revoked.Transition ||
		again.Disposition != store.OverrideRevoked || again.Override != grant.Override {
		t.Fatalf("retried revocation: %d %s", code, body)
	}
	if code, again, body := e.revokeOverride(harness, grant.Override, "lifecycle-revoke-02"); code != http.StatusConflict || again.Reason != "ALREADY_RESOLVED" ||
		again.Disposition != store.OverrideRevoked {
		t.Fatalf("second revocation: %d %s", code, body)
	}
	if code, _, body := e.revokeOverride(harness, 99, "lifecycle-revoke-03"); code != http.StatusNotFound {
		t.Fatalf("unknown override: %d %s", code, body)
	}
	// The identical creation request returns its recorded receipt and creates
	// nothing, although the base has moved on; changed in any way (here its
	// precondition), it is refused.
	if code, again, body := e.createOverride(harness, permitOverride(store.OverrideException, "READER", "write-a", 600), `"r1"`, "lifecycle-grant-0001"); code != http.StatusCreated ||
		again.Transition != grant.Transition || again.Override != grant.Override {
		t.Fatalf("replayed creation: %d %s", code, body)
	}
	if code, again, body := e.createOverride(harness, permitOverride(store.OverrideException, "READER", "write-a", 600), `"r2"`, "lifecycle-grant-0001"); code != http.StatusUnprocessableEntity ||
		again.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("creation key reused for another request: %d %s", code, body)
	}
	if n := len(e.activeOverrides(harness).Overrides); n != 2 {
		t.Fatalf("%d active overrides after the replay, want 2", n)
	}
	// Revoke the rest: the base is enforced again with the membership change.
	for i, id := range []int64{contain.Override, closed.Override} {
		if code, _, body := e.revokeOverride(harness, id, fmt.Sprintf("lifecycle-revoke-rest-%d", i)); code != http.StatusOK {
			t.Fatalf("revocation of %d: %d %s", id, code, body)
		}
	}
	s = e.status()
	if s.Confirmed != 2 || !strings.HasPrefix(s.Generation, "r2-") {
		t.Fatalf("status without overrides %+v", s)
	}
	expect(map[string]int{"bob POST /a/item": 403, "bob GET /a/x": 200, "bob GET /b/x": 200, "alice GET /a/x": 200, "alice POST /a/item": 200}, [3]int{3, 1, 0})
	if s.Generation == baseGeneration {
		t.Fatal("the generation of revision 2 equals that of revision 1")
	}

	// History, events and durable state.
	code, body, _ = e.as(harness, http.MethodGet, "/admin/overrides/history", "")
	history := fromJSON[struct {
		Items []store.Override `json:"items"`
	}](t, body)
	if code != http.StatusOK || len(history.Items) != 3 {
		t.Fatalf("override history: %d %s", code, body)
	}
	for _, o := range history.Items {
		if o.Status != store.OverrideRevoked || o.ResolvedBy != harnessAdmin || o.ResolvedAt == nil || o.Resolution == "" {
			t.Fatalf("history item %+v", o)
		}
	}
	if code, body, _ := e.as(harness, http.MethodGet, "/admin/overrides/2", ""); code != http.StatusOK || !strings.Contains(body, `"target":"permit:WRITER:read-a"`) {
		t.Fatalf("one override: %d %s", code, body)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.revoked") >= 3 })
	if countKind(events, "tpo.created") != 3 || countKind(events, "tpo.revoked") != 3 {
		t.Fatalf("lifecycle events: %d created, %d revoked", countKind(events, "tpo.created"), countKind(events, "tpo.revoked"))
	}
	created := findTransition(events, "tpo.created", grant.Transition)
	revokedEvent := findTransition(events, "tpo.revoked", revoked.Transition)
	if created == nil || created["override"] != float64(1) || created["target"] != "permit:READER:write-a" || created["actor"] != harnessAdmin ||
		created["generation"] != grant.Generation || created["revision"] != float64(1) || created["expires_at"] == nil ||
		revokedEvent == nil || revokedEvent["generation"] != revoked.Generation || revokedEvent["override"] != float64(1) {
		t.Fatalf("events: created %v revoked %v", created, revokedEvent)
	}
	assertAbsent(t, e, harness.cookie, harness.csrf, harnessPassword)
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 2 {
		t.Fatalf("%d revisions: overrides created revisions", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind LIKE 'tpo.%' AND outcome = 'committed' AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 6 {
		t.Fatalf("%d published and audited override transitions, want 6", n)
	}
	for _, o := range storedOverrides(t, e.database) {
		if o.Status != store.OverrideRevoked || o.Base != 1 {
			t.Fatalf("stored override %+v", o)
		}
	}
}
