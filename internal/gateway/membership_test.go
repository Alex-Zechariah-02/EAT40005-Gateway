// Blocked-account changes, refusals, concurrency and retries.

package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The six membership regression scenarios now exercise independent account
// containment. Version 4 has no membership routes or policy-owned accounts.
func (e *env) blockAccount(s adminSession, id, reason, key string) (int, blockedAccount, string) {
	e.t.Helper()
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	code, body, _ := e.as(s, "PUT", "/admin/blocked-accounts/"+id, string(payload), "Idempotency-Key", key)
	var entry blockedAccount
	json.Unmarshal([]byte(body), &entry)
	return code, entry, body
}

func (e *env) unblockAccount(s adminSession, id, key string) (int, string) {
	e.t.Helper()
	code, body, _ := e.as(s, "DELETE", "/admin/blocked-accounts/"+id, "", "Idempotency-Key", key)
	return code, body
}

func TestMembershipChangePublishes(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()
	code, entry, body := e.blockAccount(admin, "alice", "contain account", "account-publish-block")
	if code != 200 || entry.Subject != "alice" || entry.BlockedBy != harnessAdmin || entry.BlockedAt.IsZero() {
		t.Fatalf("block: %d %s", code, body)
	}
	if code, body := e.get("/a/x", bearer(t, "alice")...); code != 403 || !strings.Contains(body, "account_blocked") {
		t.Fatalf("blocked: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "bob")...); code != 200 {
		t.Fatal("unrelated account was changed")
	}
	if code, body := e.unblockAccount(admin, "alice", "account-publish-unblock"); code != 200 {
		t.Fatalf("unblock: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatal("unblock was not published")
	}
	e.assertReceipts(2, 0, 0)
	if after := e.status(); after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatal("containment changed policy authority")
	}
	for _, tc := range []struct{ key, kind string }{{"account-publish-block", "account.block"}, {"account-publish-unblock", "account.unblock"}} {
		code, body, _ := e.as(admin, "GET", "/admin/history/admin:"+harnessAdmin+":"+tc.key, "")
		history := fromJSON[historyEntry](t, body)
		if code != 200 || history.Actor != harnessAdmin || history.Kind != tc.kind || history.Target != "alice" || history.Revision != 0 || history.AuditedAt == nil {
			t.Fatalf("history: %d %s", code, body)
		}
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM revision`); n != 1 {
		t.Fatalf("%d policy revisions", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM blocked_account`); n != 0 {
		t.Fatalf("%d blocked entries", n)
	}
	events := e.auditEvents()
	for _, kind := range []string{"account.block", "account.unblock"} {
		if n := countKind(events, kind); n != 1 {
			t.Errorf("%s outcomes %d", kind, n)
		}
	}
}

func TestMembershipRefusals(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()
	for i, body := range []string{
		`{}`, `{"reason":""}`, `{"reason":7}`, `{"reason":"x","roles":["WRITER"]}`,
		`{"reason":"x","permits":[]}`, `{"reason":"x","roles_catalogue":["ADMIN"]}`,
		`{"reason":"x","id":"bob"}`, `{"reason":"x","operations":[]}`,
		`{"status":"active","roles":["READER"]}`, `{"reason":"` + strings.Repeat("x", 201) + `"}`,
	} {
		if code, data, _ := e.as(admin, "PUT", "/admin/blocked-accounts/alice", body, "Idempotency-Key", fmt.Sprintf("account-refused-%02d", i)); code != 400 {
			t.Errorf("%s: %d %s", body, code, data)
		}
	}
	if code, body := e.unblockAccount(admin, "nobody", "account-refused-absent"); code != 404 || !strings.Contains(body, "not_found") {
		t.Fatalf("absent unblock: %d %s", code, body)
	}
	for _, id := range []string{"has%20space", "tab%09x", "%C3%A9", strings.Repeat("x", 129)} {
		if code, _, _ := e.blockAccount(admin, id, "reason", "account-invalid-subject"); code != 400 && code != 404 {
			t.Errorf("id %q: %d", id, code)
		}
	}
	for _, path := range []string{"/admin/roles/WRITER", "/admin/permits", "/admin/subjects/alice", "/admin/subjects/alice/roles"} {
		if code, _, _ := e.as(admin, "PUT", path, `{}`, "Idempotency-Key", "account-removed-routes"); code != 404 {
			t.Errorf("removed route %s: %d", path, code)
		}
	}
	if after := e.status(); after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatal("refused mutations changed policy")
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatal("refused block contained alice")
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM blocked_account`); n != 0 {
		t.Fatalf("%d blocks from refused input", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM revision`); n != 1 {
		t.Fatalf("%d revisions from refused input", n)
	}
}

func TestConcurrentMembershipEdits(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	_, password := e.addAdmin(binaries.plain, "second", "read,manage_subjects")
	admins := []adminSession{e.mustLogin(harnessAdmin, harnessPassword), e.mustLogin("second", password)}
	for round := range 5 {
		entries := make([]blockedAccount, 2)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				codes[i], entries[i], _ = e.blockAccount(admins[i], "alice", fmt.Sprintf("reason-%d-%d", round, i), fmt.Sprintf("account-concurrent-%d-%d", round, i))
			})
		}
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 {
			t.Fatalf("concurrent updates: %v", codes)
		}
		code, body, _ := e.as(admins[0], "GET", "/admin/blocked-accounts", "")
		got := fromJSON[struct {
			Items []blockedAccount `json:"items"`
		}](t, body)
		if code != 200 || len(got.Items) != 1 || !slices.Contains(entries, got.Items[0]) {
			t.Fatalf("torn concurrent result: %d %s %+v", code, body, entries)
		}
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE kind='account.block' AND outcome='committed' AND audited_at IS NOT NULL`); n != 10 {
		t.Fatalf("durable updates %d", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM revision`); n != 1 {
		t.Fatalf("containment created %d policy revisions", n)
	}
}

func TestMembershipRetryIsIdempotent(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	code, _, first := e.blockAccount(admin, "alice", "first reason", "account-idempotent-key")
	if code != 200 {
		t.Fatalf("block: %d %s", code, first)
	}
	for range 3 {
		if code, _, again := e.blockAccount(admin, "alice", "first reason", "account-idempotent-key"); code != 200 || again != first {
			t.Fatalf("replay: %d %s", code, again)
		}
	}
	if code, _, body := e.blockAccount(admin, "alice", "different reason", "account-idempotent-key"); code != 422 || !strings.Contains(body, "transition_id_reused") {
		t.Fatalf("body conflict: %d %s", code, body)
	}
	if code, body := e.unblockAccount(admin, "alice", "account-idempotent-key"); code != 422 {
		t.Fatalf("kind conflict: %d %s", code, body)
	}
	if code, body := e.unblockAccount(admin, "alice", "account-idempotent-delete"); code != 200 {
		t.Fatalf("unblock: %d %s", code, body)
	}
	// Replaying the earlier block returns its original receipt without reblocking.
	if code, _, again := e.blockAccount(admin, "alice", "first reason", "account-idempotent-key"); code != 200 || again != first {
		t.Fatalf("old replay: %d %s", code, again)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatal("replay reapplied an old block")
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE kind='account.block' AND outcome='committed'`); n != 1 {
		t.Fatalf("block applied %d times", n)
	}
	if n := countKind(e.auditEvents(), "account.block"); n != 1 {
		t.Fatalf("block outcome recorded %d times", n)
	}
}
