// Change history order, filters and idempotency keys.

package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

type historyItem struct {
	Seq          int64  `json:"seq"`
	TransitionID string `json:"transition_id"`
	Kind         string `json:"kind"`
	Actor        string `json:"actor"`
	Outcome      string `json:"outcome"`
}

// historyPage reads one page of the history with the given query.
func (e *env) historyPage(query string) ([]historyItem, string) {
	e.t.Helper()
	code, body, _ := e.harnessCall(http.MethodGet, "/admin/history?"+query, "")
	if code != http.StatusOK {
		e.t.Fatalf("GET /admin/history?%s: %d %s", query, code, body)
	}
	page := fromJSON[struct {
		Items []historyItem `json:"items"`
		Next  string        `json:"next"`
	}](e.t, body)
	return page.Items, page.Next
}

// historyPages reads every page of a history listing, limit at a time.
func (e *env) historyPages(query string, limit int) []historyItem {
	e.t.Helper()
	var items []historyItem
	after := ""
	for range 200 {
		path := fmt.Sprintf("limit=%d%s", limit, query)
		if after != "" {
			path += "&after=" + after
		}
		page, next := e.historyPage(path)
		items = append(items, page...)
		if next == "" {
			return items
		}
		after = next
	}
	e.t.Fatalf("history listing %q did not end", query)
	return nil
}

func historySeqs(items []historyItem) []int64 {
	seqs := make([]int64, len(items))
	for i, item := range items {
		seqs[i] = item.Seq
	}
	return seqs
}

// History lists newest first on request, applies its filters before the
// page limit, pages a newest-first listing without skipping or repeating
// an entry while new ones are recorded, and refuses malformed parameters.
// The default order is unchanged.
func TestHistoryOrderAndFilters(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	control := func(method, subject, key string, want int) {
		t.Helper()
		body := ""
		if method == http.MethodPut {
			body = `{"reason":"history order test"}`
		}
		if code, reply, _ := e.harnessCall(method, "/admin/blocked-accounts/"+subject, body, "Idempotency-Key", key); code != want {
			t.Fatalf("%s %s: %d %s", method, subject, code, reply)
		}
	}
	for i := range 5 {
		control(http.MethodPut, fmt.Sprintf("cust-%03d", 100+i), fmt.Sprintf("history-block-%04d", i), http.StatusOK)
	}
	for i := range 3 {
		control(http.MethodDelete, fmt.Sprintf("cust-%03d", 100+i), fmt.Sprintf("history-unblock-%04d", i), http.StatusOK)
	}
	for i := range 2 {
		control(http.MethodDelete, "cust-999", fmt.Sprintf("history-missing-%04d", i), http.StatusNotFound)
	}

	oldest := e.historyPages("", 100)
	if len(oldest) < 10 {
		t.Fatalf("history holds %d entries, want at least the 10 made here", len(oldest))
	}
	if got := e.historyPages("&order=asc", 3); !slices.Equal(historySeqs(got), historySeqs(oldest)) {
		t.Fatalf("order=asc listed %v, want the default %v", historySeqs(got), historySeqs(oldest))
	}
	newest := e.historyPages("&order=desc", 2)
	reversed := slices.Clone(historySeqs(oldest))
	slices.Reverse(reversed)
	if !slices.Equal(historySeqs(newest), reversed) {
		t.Fatalf("order=desc listed %v, want %v", historySeqs(newest), reversed)
	}

	only := func(items []historyItem, keep func(historyItem) bool) []int64 {
		var seqs []int64
		for _, item := range items {
			if keep(item) {
				seqs = append(seqs, item.Seq)
			}
		}
		return seqs
	}
	for _, tc := range []struct {
		query string
		from  []historyItem
		keep  func(historyItem) bool
		count int
	}{
		{"&order=desc&kind=account.block", newest, func(h historyItem) bool { return h.Kind == "account.block" }, 5},
		{"&order=desc&kind=account.block,account.unblock&outcome=committed", newest, func(h historyItem) bool {
			return (h.Kind == "account.block" || h.Kind == "account.unblock") && h.Outcome == "committed"
		}, 8},
		{"&outcome=rejected&kind=account.unblock", oldest, func(h historyItem) bool { return h.Outcome == "rejected" && h.Kind == "account.unblock" }, 2},
		{"&order=desc&actor=" + harnessAdmin, newest, func(h historyItem) bool { return h.Actor == harnessAdmin }, -1},
		{"&actor=nobody", oldest, func(historyItem) bool { return false }, 0},
		{"&kind=no.such_kind", oldest, func(historyItem) bool { return false }, 0},
	} {
		want := only(tc.from, tc.keep)
		got := e.historyPages(tc.query, 2)
		if !slices.Equal(historySeqs(got), want) || (tc.count >= 0 && len(want) != tc.count) || (tc.count < 0 && len(want) < 10) {
			t.Errorf("history%s: got %v, want %v (%d expected)", tc.query, historySeqs(got), want, tc.count)
		}
	}

	// An entry recorded between pages of a newest-first listing is not in
	// that listing, and nothing in it is skipped or repeated.
	first, next := e.historyPage("limit=3&order=desc")
	control(http.MethodPut, "cust-950", "history-between-0001", http.StatusOK)
	var rest []historyItem
	for after := next; after != ""; {
		var page []historyItem
		page, after = e.historyPage("limit=3&order=desc&after=" + after)
		rest = append(rest, page...)
	}
	if seqs := historySeqs(append(first, rest...)); !slices.Equal(seqs, reversed) {
		t.Fatalf("paging across a new entry listed %v, want %v", seqs, reversed)
	}
	if fresh, _ := e.historyPage("limit=1&order=desc"); len(fresh) != 1 || fresh[0].Seq <= reversed[0] ||
		fresh[0].TransitionID != "admin:"+harnessAdmin+":history-between-0001" {
		t.Fatalf("a new listing does not start with the new entry: %+v", fresh)
	}

	tooMany := strings.TrimSuffix(strings.Repeat("a.b,", maxHistoryKinds+1), ",")
	for _, query := range []string{"order=up", "order=", "order=desc&order=asc", "kind=", "kind=Account.block", "kind=a,,b",
		"kind=" + tooMany, "kind=a&kind=b", "outcome=pending", "outcome=", "actor=", "actor=" + strings.Repeat("x", maxIDLength+1),
		"sort=seq", "order=desc&after=x"} {
		code, body, _ := e.harnessCall(http.MethodGet, "/admin/history?"+query, "")
		if code != http.StatusBadRequest || !strings.Contains(body, `"page"`) {
			t.Errorf("GET /admin/history?%s: %d %s", query, code, body)
		}
	}
}

// Every Admin API change is recorded as the transition
// admin:<administrator>:<Idempotency-Key>, which GET /admin/history/{id}
// returns with its outcome, so a change whose answer was lost can be
// resolved without sending it again. A key never sent is not found, and the
// lookup itself records nothing.
func TestHistoryEntryByIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	lookup := func(key string) (int, historyItem) {
		t.Helper()
		code, body, _ := e.as(admin, http.MethodGet, "/admin/history/admin:"+harnessAdmin+":"+key, "")
		var entry historyItem
		if code == http.StatusOK {
			entry = fromJSON[historyItem](t, body)
		}
		return code, entry
	}
	before := len(e.historyPages("", 100))
	if code, _ := lookup("never-sent-key-0001"); code != http.StatusNotFound {
		t.Fatalf("unused key: %d", code)
	}
	if after := len(e.historyPages("", 100)); after != before {
		t.Fatalf("a lookup recorded %d transitions", after-before)
	}

	revision := e.status().Revision
	confirmed := e.confirmPolicy(admin, e.cpaCandidate(), revision, "lookup-confirm-01")
	_, trial := e.provisionalFrom(admin, e.protectedPolicy(), confirmed.Revision, 600, "lookup-rollback-01")
	if code, _, body := e.resolve(admin, "rollback", trial.Provisional, policyETag(confirmed.Revision), "rollback-lookup-rollback-01"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	code, staged, body := e.stage(admin, e.protectedPolicy(), policyETag(confirmed.Revision), "stage-lookup-withdraw-01")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	if code, body, _ := e.as(admin, http.MethodDelete, fmt.Sprintf("/admin/candidates/%d", staged.Candidate.ID), "", "Idempotency-Key", "withdraw-lookup-01"); code != http.StatusOK {
		t.Fatalf("withdrawal: %d %s", code, body)
	}
	rule := e.mustOverride(admin, operationContainment(e.protectedPolicy().Operations[0].ID, 600), policyETag(confirmed.Revision), "override-lookup-01")
	if code, _, body := e.revokeOverride(admin, rule.Override, "revoke-lookup-01"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	for _, step := range []struct{ method, path, body, key string }{
		{http.MethodPut, "/admin/blocked-accounts/cust-300", `{"reason":"lookup test"}`, "block-lookup-0001"},
		{http.MethodDelete, "/admin/blocked-accounts/cust-300", "", "unblock-lookup-01"},
		{http.MethodDelete, "/admin/blocked-accounts/cust-300", "", "unblock-lookup-02"},
		{http.MethodPost, "/admin/keys/reload", "", "reload-lookup-0001"},
	} {
		e.as(admin, step.method, step.path, step.body, "Idempotency-Key", step.key)
	}

	for _, want := range []struct{ key, kind, outcome string }{
		{"stage-lookup-confirm-01", "candidate.create", "committed"},
		{"activate-lookup-confirm-01", "cpa.activate", "committed"},
		{"confirm-lookup-confirm-01", "cpa.confirm", "committed"},
		{"rollback-lookup-rollback-01", "cpa.rollback", "committed"},
		{"withdraw-lookup-01", "candidate.withdraw", "committed"},
		{"override-lookup-01", "tpo.create", "committed"},
		{"revoke-lookup-01", "tpo.revoke", "committed"},
		{"block-lookup-0001", "account.block", "committed"},
		{"unblock-lookup-01", "account.unblock", "committed"},
		{"unblock-lookup-02", "account.unblock", "rejected"},
		{"reload-lookup-0001", "keys.reload", "committed"},
	} {
		code, entry := lookup(want.key)
		if code != http.StatusOK || entry.TransitionID != "admin:"+harnessAdmin+":"+want.key || entry.Kind != want.kind ||
			entry.Outcome != want.outcome || entry.Actor != harnessAdmin {
			t.Errorf("lookup %s: %d %+v, want %s %s", want.key, code, entry, want.kind, want.outcome)
		}
	}
}

// STORE_ERROR says only that this request did not complete. When the record
// of an earlier, committed attempt cannot be read, a resend of the same
// change is answered store_error while that change stays in force, so the
// Dashboard must never describe store_error as "nothing changed". Once the
// store can be read, the resend returns the recorded receipt.
func TestStoreErrorOnResendDoesNotMeanNothingChanged(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	c, _ := e.own(e.controllerConfig())
	const reason = "store error test"
	request := struct {
		Reason string `json:"reason"`
	}{reason}
	block := adminTransition(caller{admin: administrator{Name: harnessAdmin}}, "store-error-resend-01", "account.block", "cust-500", sql.NullInt64{}, request)
	receipt, status := c.changeBlock(t.Context(), block, reason)
	if status != http.StatusOK {
		t.Fatalf("block: %d %s", status, receipt)
	}
	// A cancelled request makes the read of the earlier record fail.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if body, status := c.changeBlock(cancelled, block, reason); status != http.StatusServiceUnavailable || string(body) != `{"error":"store_error"}` {
		t.Fatalf("resend with an unreadable record: %d %s", status, body)
	}
	if _, inForce := (*c.blocked.Load())["cust-500"]; !inForce {
		t.Fatal("the committed block is no longer in force")
	}
	if body, status := c.changeBlock(t.Context(), block, reason); status != http.StatusOK || string(body) != string(receipt) {
		t.Fatalf("resend with a readable record: %d %s, want the receipt %s", status, body, receipt)
	}
}
