package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// putSubject replaces one subject's membership as an administrator.
func (e *env) putSubject(s adminSession, id, body string, revision int64, key string) (int, Outcome, http.Header) {
	e.t.Helper()
	code, data, header := e.as(s, http.MethodPut, "/admin/subjects/"+id, body, "If-Match", fmt.Sprintf(`"r%d"`, revision), "Idempotency-Key", key)
	return code, fromJSON[Outcome](e.t, data), header
}

func (e *env) deleteSubject(s adminSession, id string, revision int64, key string) (int, Outcome) {
	e.t.Helper()
	code, data, _ := e.as(s, http.MethodDelete, "/admin/subjects/"+id, "", "If-Match", fmt.Sprintf(`"r%d"`, revision), "Idempotency-Key", key)
	return code, fromJSON[Outcome](e.t, data)
}

// storedPolicy reads one revision's document from a stopped gateway's
// database.
func storedPolicy(t *testing.T, path string, revision int64) Policy {
	t.Helper()
	document := queryString(t, path, `SELECT document FROM revision WHERE id = ?`, revision)
	p, err := DecodePolicy([]byte(document))
	if err != nil {
		t.Fatalf("revision %d: %v", revision, err)
	}
	return p
}

// A membership change commits a revision that differs
// from its parent only in the subjects, publishes it, and the response,
// history, audit events and the upstream's receipts all name the same
// revision and generation.
func TestMembershipChangePublishes(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	carolToken := issuer.tokenFor(t, "carol")

	if code, _ := e.get("/a/x", "Authorization", "Bearer "+carolToken); code != http.StatusForbidden {
		t.Fatalf("carol before enrolment as a subject: %d", code)
	}
	e.assertReceipts(0, 0, 0)

	code, added, header := e.putSubject(harness, "carol", `{"status":"active","roles":["READER"]}`, 1, "membership-add-carol")
	if code != http.StatusOK || added.Outcome != "committed" || added.Revision != 2 || added.Generation == "" || header.Get("ETag") != `"r2"` {
		t.Fatalf("adding carol: %d %+v %s", code, added, header.Get("ETag"))
	}
	if s := e.status(); s.Revision != 2 || s.Generation != added.Generation {
		t.Fatalf("status after the change: %+v", s)
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+carolToken); code != http.StatusOK {
		t.Fatalf("carol after enrolment as a subject: %d", code)
	}
	e.assertReceipts(1, 0, 0)
	if g := e.a.receipts()[0].Generation; g != added.Generation {
		t.Fatalf("carol's request reached A under %s, want %s", g, added.Generation)
	}
	code, body, _ := e.as(harness, http.MethodGet, "/admin/subjects/carol", "")
	if got := fromJSON[Subject](t, body); code != http.StatusOK || got.Status != "active" || !slices.Equal(got.Roles, []string{"READER"}) {
		t.Fatalf("carol as read back: %d %s", code, body)
	}

	// Disabling and removing take effect for the next request.
	code, disabled, _ := e.putSubject(harness, "alice", `{"status":"disabled","roles":["WRITER"]}`, 2, "membership-disable-alice")
	if code != http.StatusOK || disabled.Revision != 3 {
		t.Fatalf("disabling alice: %d %+v", code, disabled)
	}
	code, removed := e.deleteSubject(harness, "carol", 3, "membership-remove-carol")
	if code != http.StatusOK || removed.Revision != 4 {
		t.Fatalf("removing carol: %d %+v", code, removed)
	}
	for name, token := range map[string]string{"alice": issuer.tokenFor(t, "alice"), "carol": carolToken} {
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+token); code != http.StatusForbidden {
			t.Errorf("%s after the change: %d", name, code)
		}
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "bob")); code != http.StatusOK {
		t.Fatal("bob's unchanged membership stopped working")
	}
	e.assertReceipts(2, 0, 0)

	// History and audit name the same actor, revision and generation.
	code, body, _ = e.as(harness, http.MethodGet, "/admin/history/"+added.Transition, "")
	entry := fromJSON[historyEntry](t, body)
	if code != http.StatusOK || entry.Actor != harnessAdmin || entry.Kind != "membership.put" || entry.Target != "subject carol" ||
		entry.Expected == nil || *entry.Expected != 1 || entry.Revision != 2 || entry.PublishedBy != "publisher" || entry.AuditedAt == nil {
		t.Fatalf("history entry: %d %s", code, body)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return len(publishedEvents(events, removed.Transition)) == 1 })
	for _, receipt := range []Outcome{added, disabled, removed} {
		published := publishedEvents(events, receipt.Transition)
		if len(published) != 1 || published[0]["actor"] != harnessAdmin || published[0]["generation"] != receipt.Generation ||
			published[0]["revision"] != float64(receipt.Revision) {
			t.Errorf("published events of %s: %v", receipt.Transition, published)
		}
		intents := 0
		for _, ev := range events {
			if ev["kind"] == "transition.intent" && ev["transition"] == receipt.Transition && ev["actor"] == harnessAdmin {
				intents++
			}
		}
		if intents != 1 {
			t.Errorf("%d intent events for %s", intents, receipt.Transition)
		}
	}

	// Each revision differs from its parent only in the subjects.
	e.stop()
	original := storedPolicy(t, e.database, 1)
	for revision := int64(2); revision <= 4; revision++ {
		if origin := queryString(t, e.database, `SELECT origin FROM revision WHERE id = ?`, revision); origin != "membership" {
			t.Errorf("revision %d origin %q", revision, origin)
		}
		if parent := queryInt(t, e.database, `SELECT parent FROM revision WHERE id = ?`, revision); parent != int(revision-1) {
			t.Errorf("revision %d parent %d", revision, parent)
		}
		changed := storedPolicy(t, e.database, revision)
		changed.Subjects, original.Subjects = nil, nil
		if string(mustJSON(changed)) != string(mustJSON(original)) {
			t.Errorf("revision %d changed more than the subjects", revision)
		}
	}
}

// An invalid membership is refused as a whole, the
// request body can carry nothing but status and roles, and roles exist only
// in the policy.
func TestMembershipRefusals(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()

	cases := []struct {
		name, subject, body string
		status              int
		reason, field       string
	}{
		{"unknown role", "carol", `{"status":"active","roles":["ADMIN"]}`, 400, "UNKNOWN_REFERENCE", "subjects[2].roles[0]"},
		{"self-granted role", "alice", `{"status":"active","roles":["WRITER","manage_subjects"]}`, 400, "UNKNOWN_REFERENCE", "subjects[0].roles[1]"},
		{"duplicate role", "carol", `{"status":"active","roles":["READER","READER"]}`, 400, "DUPLICATE_ID", "subjects[2].roles[1]"},
		{"bad status", "carol", `{"status":"enabled","roles":["READER"]}`, 400, "INVALID_VALUE", "subjects[2].status"},
		{"missing status", "carol", `{"roles":["READER"]}`, 400, "INVALID_VALUE", "subjects[2].status"},
	}
	for i, tc := range cases {
		code, outcome, _ := e.putSubject(harness, tc.subject, tc.body, 1, fmt.Sprintf("membership-refused-%02d", i))
		if code != tc.status || outcome.Reason != tc.reason || outcome.Field != tc.field {
			t.Errorf("%s: %d %+v, want %d %s %s", tc.name, code, outcome, tc.status, tc.reason, tc.field)
		}
	}
	if code, outcome := e.deleteSubject(harness, "nobody", 1, "membership-delete-nobody"); code != http.StatusNotFound || outcome.Reason != "NOT_FOUND" {
		t.Errorf("deleting an unknown subject: %d %+v", code, outcome)
	}
	// The body cannot change permits, roles or operations, or name a subject.
	for i, body := range []string{
		`{"status":"active","roles":["READER"],"permits":[{"role":"READER","operation":"write-a"}]}`,
		`{"status":"active","roles":["READER"],"roles_catalogue":["ADMIN"]}`,
		`{"status":"active","roles":["READER"],"id":"alice"}`,
		`{"status":"active","roles":["READER"],"operations":[]}`,
	} {
		code, data, _ := e.as(harness, http.MethodPut, "/admin/subjects/carol", body, "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("membership-extra-%02d", i))
		if code != http.StatusBadRequest || strings.TrimSpace(data) != `{"error":"bad_request"}` {
			t.Errorf("extra field %s: %d %s", body, code, data)
		}
	}
	for _, id := range []string{"has%20space", "tab%09x", "%C3%A9", strings.Repeat("x", 129)} {
		code, _, _ := e.as(harness, http.MethodPut, "/admin/subjects/"+id, `{"status":"active","roles":[]}`, "If-Match", `"r1"`, "Idempotency-Key", "membership-bad-id-01")
		if code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Errorf("subject id %q: %d", id, code)
		}
	}
	// Role and permit routes do not exist; only a candidate can propose them.
	for _, path := range []string{"/admin/roles/WRITER", "/admin/permits", "/admin/subjects/alice/roles"} {
		if code, _, _ := e.as(harness, http.MethodPut, path, `{}`, "If-Match", `"r1"`, "Idempotency-Key", "membership-role-route"); code != http.StatusNotFound {
			t.Errorf("PUT %s: %d", path, code)
		}
	}

	if after := e.status(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("refused changes changed the status: %+v", after)
	}
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatal("alice's membership changed")
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "carol")); code != http.StatusForbidden {
		t.Fatal("a refused change admitted carol")
	}
	code, body, _ := e.as(harness, http.MethodGet, "/admin/history/admin:"+harnessAdmin+":membership-refused-00", "")
	if entry := fromJSON[historyEntry](t, body); code != http.StatusOK || entry.Outcome != "rejected" || entry.Reason != "UNKNOWN_REFERENCE" || entry.Revision != 0 {
		t.Fatalf("history of a refused change: %d %s", code, body)
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 1 {
		t.Fatalf("%d revisions after refusals, want 1", n)
	}
}

// Two administrators changing the same subject against the same
// revision: exactly one change applies and the other is told it is stale.
func TestConcurrentMembershipEdits(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	_, secondPassword := e.addAdmin(binaries.plain, "second", "manage_subjects")
	admins := []adminSession{e.mustLogin(harnessAdmin, harnessPassword), e.mustLogin("second", secondPassword)}

	revision := int64(1)
	for round := range 5 {
		roles := []string{"READER", "WRITER"}
		codes := make([]int, 2)
		outcomes := make([]Outcome, 2)
		etags := make([]string, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				body := fmt.Sprintf(`{"status":"active","roles":[%q]}`, roles[i])
				code, data, header := e.adminSend(http.MethodPut, "/admin/subjects/carol", body,
					append(admins[i].headers(e), "If-Match", fmt.Sprintf(`"r%d"`, revision), "Idempotency-Key", fmt.Sprintf("concurrent-edit-%d-%d", round, i))...)
				codes[i], etags[i] = code, header.Get("ETag")
				json.Unmarshal([]byte(data), &outcomes[i])
			})
		}
		wg.Wait()
		winner := slices.Index(codes, http.StatusOK)
		loser := 1 - winner
		if winner < 0 || codes[loser] != http.StatusPreconditionFailed || outcomes[loser].Reason != "PRECONDITION_FAILED" {
			t.Fatalf("round %d: statuses %v outcomes %+v", round, codes, outcomes)
		}
		revision++
		if outcomes[winner].Revision != revision || etags[winner] != fmt.Sprintf(`"r%d"`, revision) || etags[loser] != etags[winner] {
			t.Fatalf("round %d: winner %+v, ETags %v", round, outcomes[winner], etags)
		}
		_, body, _ := e.as(admins[0], http.MethodGet, "/admin/subjects/carol", "")
		if got := fromJSON[Subject](t, body); !slices.Equal(got.Roles, []string{roles[winner]}) {
			t.Fatalf("round %d: carol holds %v, the winner set %s", round, got.Roles, roles[winner])
		}
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 6 {
		t.Fatalf("%d revisions, want 6 (one per round)", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE reason = 'PRECONDITION_FAILED'`); n != 5 {
		t.Fatalf("%d stale refusals recorded, want 5", n)
	}
}

// A retried change returns the recorded outcome without applying
// again, and its key cannot be reused for a different change.
func TestMembershipRetryIsIdempotent(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	put := `{"status":"active","roles":["READER"]}`
	code, first, _ := e.putSubject(harness, "carol", put, 1, "membership-retry-key")
	if code != http.StatusOK || first.Revision != 2 {
		t.Fatalf("change: %d %+v", code, first)
	}
	for range 3 {
		code, again, _ := e.putSubject(harness, "carol", put, 1, "membership-retry-key")
		if code != http.StatusOK || again.Transition != first.Transition || again.Revision != first.Revision || again.Outcome != "committed" {
			t.Fatalf("retry: %d %+v", code, again)
		}
	}
	for _, change := range []struct {
		body     string
		revision int64
	}{
		{`{"status":"active","roles":["WRITER"]}`, 1}, // another body
		{put, 2}, // another precondition
	} {
		code, reused, _ := e.putSubject(harness, "carol", change.body, change.revision, "membership-retry-key")
		if code != http.StatusUnprocessableEntity || reused.Reason != "TRANSITION_ID_REUSED" {
			t.Fatalf("reused key: %d %+v", code, reused)
		}
	}
	if code, reused := e.deleteSubject(harness, "carol", 2, "membership-retry-key"); code != http.StatusUnprocessableEntity || reused.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("reused key for a removal: %d %+v", code, reused)
	}
	if s := e.status(); s.Revision != 2 {
		t.Fatalf("retries changed the revision to %d", s.Revision)
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 2 {
		t.Fatalf("%d revisions, want 2", n)
	}
	published := 0
	for _, ev := range e.auditEvents() {
		if ev["kind"] == "transition.published" && ev["transition"] == first.Transition {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("%d published events for one change", published)
	}
}

// Removing a subject while one of its requests is in flight: that
// request completes under the generation it captured, every later one is
// refused under the new generation.
func TestMembershipRemovalDuringRequest(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	first := e.status().Generation
	aliceToken := issuer.tokenFor(t, "alice")

	conn, captured := holdRequest(t, e, holdFile, aliceToken)
	defer conn.Close()
	if captured != first {
		t.Fatalf("captured %s, want %s", captured, first)
	}
	code, removed := e.deleteSubject(harness, "alice", 1, "remove-alice-in-flight")
	if code != http.StatusOK || removed.Generation == first {
		t.Fatalf("removing alice: %d %+v", code, removed)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "true}")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request: %v %v", resp, err)
	}
	e.assertReceipts(1, 0, 0)
	if held := e.a.receipts()[0]; held.Generation != first || held.Headers.Get("X-Gateway-Roles") != "WRITER" {
		t.Fatalf("in-flight request reached A under %s with roles %q", held.Generation, held.Headers.Get("X-Gateway-Roles"))
	}

	resp, body := e.exchange(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("alice after removal: %d %s", resp.StatusCode, body)
	}
	e.assertReceipts(1, 0, 0)
	_, refusedID := requestID(resp, body)
	events := e.eventsWhen(func(events []map[string]any) bool { return findEvent(events, "request.rejected", refusedID) != nil })
	if ev := findEvent(events, "request.rejected", refusedID); ev == nil || ev["generation"] != removed.Generation || ev["reason"] != "unknown_subject" {
		t.Fatalf("refusal event %v", ev)
	}
}

// Membership toggled through the Admin API under concurrent
// protected traffic and admin reads, on the production binary. Every request
// is decided under one whole generation, and every admin read shows one
// whole revision.
func TestMembershipChangesUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	writer, reader := e.mustLogin(harnessAdmin, harnessPassword), e.mustLogin(harnessAdmin, harnessPassword)
	bobToken := issuer.tokenFor(t, "bob")

	// Whether bob holds READER under each generation and each revision.
	var mu sync.Mutex
	permitted := map[string]bool{e.status().Generation: true}
	readerAt := map[string]bool{`"r1"`: true}

	stop := make(chan struct{})
	var unexpected atomic.Int64
	type read struct {
		etag  string
		roles []string
	}
	var reads []read
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", nil)
				req.Header.Set("Authorization", "Bearer "+bobToken)
				resp, err := client.Do(req)
				if err != nil {
					unexpected.Add(1)
					continue
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusForbidden {
					unexpected.Add(1)
				}
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			code, body, header := e.adminSend(http.MethodGet, "/admin/subjects/bob", "", reader.headers(e)...)
			if code != http.StatusOK {
				unexpected.Add(1)
				continue
			}
			var s Subject
			if err := json.Unmarshal([]byte(body), &s); err != nil {
				unexpected.Add(1)
				continue
			}
			mu.Lock()
			reads = append(reads, read{header.Get("ETag"), s.Roles})
			mu.Unlock()
		}
	})

	const toggles = 60
	for i := range toggles {
		body := `{"status":"active","roles":["READER"]}`
		if i%2 == 0 {
			body = `{"status":"active","roles":[]}`
		}
		code, outcome, header := e.putSubject(writer, "bob", body, int64(i+1), fmt.Sprintf("load-toggle-%04d", i))
		if code != http.StatusOK {
			t.Fatalf("toggle %d: %d %+v", i, code, outcome)
		}
		mu.Lock()
		permitted[outcome.Generation] = i%2 == 1
		readerAt[header.Get("ETag")] = i%2 == 1
		mu.Unlock()
	}
	close(stop)
	wg.Wait()
	if unexpected.Load() != 0 {
		t.Fatalf("%d requests failed or got an unexpected status", unexpected.Load())
	}

	forwarded := 0
	for _, r := range e.a.receipts() {
		if !permitted[r.Generation] || r.Headers.Get("X-Gateway-Roles") != "READER" {
			t.Fatalf("receipt under %s with roles %q, where bob holds no permitted role", r.Generation, r.Headers.Get("X-Gateway-Roles"))
		}
		forwarded++
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") >= forwarded })
	refused := 0
	for _, ev := range events {
		if ev["kind"] != "request.rejected" {
			continue
		}
		generation, _ := ev["generation"].(string)
		if ev["reason"] != "no_permit" || permitted[generation] {
			t.Fatalf("refusal %v under a generation in which bob is permitted", ev)
		}
		refused++
	}
	for _, r := range reads {
		holds, known := readerAt[r.etag]
		if !known || holds != slices.Contains(r.roles, "READER") {
			t.Fatalf("admin read under %s shows roles %v", r.etag, r.roles)
		}
	}
	t.Logf("%d toggles: %d forwarded, %d refused, %d admin reads, each consistent with one revision", toggles, forwarded, refused, len(reads))
	if forwarded == 0 || refused == 0 || len(reads) == 0 {
		t.Fatal("the load did not exercise both outcomes and the admin reads")
	}
}
