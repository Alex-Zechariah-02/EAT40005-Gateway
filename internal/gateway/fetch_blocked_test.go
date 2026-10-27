package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestFetchBlockedAccountBoundaries(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	_, password := e.addAdmin(binaries.plain, "reader", "read")
	reader := e.mustLogin("reader", password)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	if code, _, _ := e.as(reader, "GET", "/admin/blocked-accounts", ""); code != 200 {
		t.Errorf("reader list: %d", code)
	}
	if code, _, _ := e.as(reader, "PUT", "/admin/blocked-accounts/alice", `{"reason":"x"}`, "Idempotency-Key", "reader-cannot-block"); code != 403 {
		t.Errorf("reader block: %d", code)
	}
	for i, body := range []string{`{}`, `{"reason":""}`, `{"reason":1}`, `{"reason":"x","reason":"y"}`, `{"Reason":"x"}`, `{"reason":"x","roles":["WRITER"]}`, `{"reason":"` + strings.Repeat("x", 201) + `"}`} {
		if code, output, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", body, "Idempotency-Key", fmt.Sprintf("block-invalid-input-%02d", i)); code != 400 {
			t.Errorf("%s: %d %s", body, code, output)
		}
	}
	for i, body := range []string{"{\"reason\":\"" + string([]byte{0xff}) + "\"}", `{"reason":"\ud800"}`} {
		if code, output, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", body, "Idempotency-Key", fmt.Sprintf("block-invalid-unicode-%02d", i)); code != 400 {
			t.Errorf("invalid Unicode accepted: %d %s", code, output)
		}
	}
	for i, reason := range []string{"x", strings.Repeat("é", 200), "\x00"} {
		body, _ := json.Marshal(map[string]string{"reason": reason})
		if code, output, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", string(body), "Idempotency-Key", fmt.Sprintf("block-valid-input-%02d", i)); code != 200 {
			t.Errorf("valid reason %q: %d %s; gateway: %s", reason, code, output, e.output.String())
		}
	}
	e.start(binaries.plain)
	admin = e.mustLogin(harnessAdmin, harnessPassword)
	if code, body, _ := e.as(admin, "GET", "/admin/blocked-accounts", ""); code != 200 {
		t.Fatalf("persisted Unicode: %d %s", code, body)
	} else {
		page := fromJSON[struct {
			Items []blockedAccount `json:"items"`
		}](t, body)
		if len(page.Items) != 1 || page.Items[0].Reason != "\x00" {
			t.Fatalf("stored Unicode changed: %s", body)
		}
	}
	if code, _, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", `{"reason":"different"}`, "Idempotency-Key", "block-valid-input-00"); code != 422 {
		t.Errorf("conflicting replay: %d", code)
	}
	if code, _, _ := e.as(admin, "DELETE", "/admin/blocked-accounts/alice", "", "Idempotency-Key", "block-valid-input-00"); code != 422 {
		t.Errorf("cross-operation replay: %d", code)
	}
	if code, _, _ := e.as(admin, "DELETE", "/admin/blocked-accounts/alice", `{}`, "Idempotency-Key", "block-delete-with-body"); code != 400 {
		t.Errorf("delete body: %d", code)
	}
	if code, _, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", `{"reason":"x"}`); code != 400 {
		t.Errorf("missing key: %d", code)
	}
	if code, _, _ := e.adminSend("PUT", "/admin/blocked-accounts/alice", `{"reason":"x"}`, "Cookie", admin.headers(e)[1], "Idempotency-Key", "block-without-csrf"); code != 403 {
		t.Errorf("csrf: %d", code)
	}
	if code, _, _ := e.adminSend("GET", "/admin/blocked-accounts", ""); code != 401 {
		t.Errorf("unauthenticated read: %d", code)
	}
}

func TestFetchBlockedAccountSurvivesRollbackAndRestart(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	s := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()
	code, original, _ := e.as(s, http.MethodPut, "/admin/blocked-accounts/alice", `{"reason":"test containment"}`, "Idempotency-Key", "blocked-account-put-01")
	if code != 200 {
		t.Fatalf("block: %d %s", code, original)
	}
	if after := e.status(); after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatal("account blocking changed policy authority")
	}
	if code, replay, _ := e.as(s, http.MethodPut, "/admin/blocked-accounts/alice", `{"reason":"test containment"}`, "Idempotency-Key", "blocked-account-put-01"); code != 200 || replay != original {
		t.Fatalf("block replay changed result: %d %s", code, replay)
	}
	assertBlocked := func() {
		t.Helper()
		if code, body := e.get("/a/x", bearer(t, "alice")...); code != http.StatusForbidden || !strings.Contains(body, `"account_blocked"`) {
			t.Fatalf("valid blocked token: %d %s", code, body)
		}
		e.assertReceipts(0, 0, 0)
	}
	assertBlocked()
	_, provisional := e.provisionalFrom(s, e.protectedPolicy(), 1, 60, "blocked-policy-trial")
	if code, _, body := e.resolve(s, "rollback", provisional.Provisional, `"r1"`, "blocked-policy-rollback"); code != 200 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	assertBlocked()
	e.start(binaries.plain)
	s = e.mustLogin(harnessAdmin, harnessPassword)
	assertBlocked()
	code, body, _ := e.as(s, http.MethodGet, "/admin/blocked-accounts", "")
	// Decode the precise member names as well as checking the list shape.
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &raw); code != 200 || err != nil || len(raw.Items) != 1 || raw.Items[0]["subject"] != "alice" || raw.Items[0]["reason"] != "test containment" || raw.Items[0]["blocked_by"] != harnessAdmin || raw.Items[0]["blocked_at"] == nil {
		t.Fatalf("blocked list: %d %s (%v)", code, body, err)
	}
	code, body, _ = e.as(s, http.MethodDelete, "/admin/blocked-accounts/alice", "", "Idempotency-Key", "blocked-account-delete-01")
	if code != 200 || !strings.Contains(body, `"unblocked":true`) {
		t.Fatalf("unblock: %d %s", code, body)
	}
	if code, _, _ := e.as(s, http.MethodDelete, "/admin/blocked-accounts/alice", "", "Idempotency-Key", "blocked-account-delete-02"); code != 404 {
		t.Fatalf("unblock of absent account: %d", code)
	}
	if code, body := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatalf("unblocked account: %d %s", code, body)
	}
	e.assertReceipts(1, 0, 0)
	e.stop()
	for _, kind := range []string{"account.block", "account.unblock"} {
		if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE kind = ? AND outcome = 'committed' AND audited_at IS NOT NULL`, kind); n != 1 {
			t.Errorf("%s durable history: %d entries, want 1", kind, n)
		}
	}
}

// A block committed and put in force before its outcome event could be
// written is not reported as a change the gateway could not make.
func TestFetchCommittedBlockWithPendingOutcome(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-published")
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	code, body, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", `{"reason":"outcome pending"}`, "Idempotency-Key", "block-outcome-pending")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"outcome_audit_pending"`) {
		t.Fatalf("committed block with a lost outcome event: %d %s", code, body)
	}
	e.stop()
	e.start(binaries.plain)
	admin = e.mustLogin(harnessAdmin, harnessPassword)
	if code, body, _ := e.as(admin, "GET", "/admin/blocked-accounts", ""); code != 200 || !strings.Contains(body, `"subject":"alice"`) {
		t.Fatalf("committed block after restart: %d %s", code, body)
	}
}

func TestFetchAdministrators(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	s := e.mustLogin(harnessAdmin, harnessPassword)
	code, body, _ := e.as(s, http.MethodGet, "/admin/administrators", "")
	var result struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &result); code != 200 || err != nil || len(result.Items) != 1 {
		t.Fatalf("administrators: %d %s (%v)", code, body, err)
	}
	for _, entry := range result.Items {
		if len(entry) != 3 || entry["name"] == nil || entry["permissions"] == nil || entry["status"] == nil {
			t.Fatalf("unexpected administrator fields: %v", entry)
		}
	}
	if strings.Contains(body, "$argon2") || strings.Contains(body, harnessPassword) {
		t.Fatal("administrator list exposed credential material")
	}
}

func TestFetchMaximumAdministratorAndIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	name := "a" + strings.Repeat("x", 63)
	admin, password := e.addAdmin(binaries.plain, name, "read,read_history,manage_subjects")
	key := strings.Repeat("k", 64)
	code, _, first := e.blockAccount(admin, "alice", "maximum valid identity", key)
	if code != 200 {
		t.Fatalf("maximum valid actor/key refused: %d %s", code, first)
	}
	e.start(binaries.plain)
	admin = e.mustLogin(name, password)
	if code, _, again := e.blockAccount(admin, "alice", "maximum valid identity", key); code != 200 || again != first {
		t.Fatalf("maximum identity replay: %d %s", code, again)
	}
	if code, _, body := e.blockAccount(admin, "alice", "different intent", key); code != 422 {
		t.Fatalf("maximum identity conflict: %d %s", code, body)
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE actor=? AND kind='account.block' AND outcome='committed'`, name); n != 1 {
		t.Fatalf("maximum identity committed %d times", n)
	}
}

func TestFetchInvalidStoredContainmentFailsClosed(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	db := inspect(t, e.database)
	if _, err := db.Exec(`INSERT INTO blocked_account(subject,reason,blocked_by,blocked_at) VALUES (?,?,?,0)`, "alice", strings.Repeat("x", 201), harnessAdmin); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.start(binaries.plain)
	if health := e.health(); health != "unavailable" {
		t.Fatalf("invalid containment was published: %s", health)
	}
	if code, _ := e.get("/a/x", bearer(t, "bob")...); code != 503 {
		t.Fatalf("invalid authority admitted: %d", code)
	}
	e.assertReceipts(0, 0, 0)
}
