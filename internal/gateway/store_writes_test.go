// Concurrent writes, lock contention and recorded rejections.

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Concurrent changes against one revision and lock contention. One
// change applies, every other is refused as stale without waiting, and an
// operator command against the running gateway's database fails at once.
func TestConcurrencyAndLockContention(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	const writers = 20
	code, staged, body := e.stage(harness, e.protectedPolicy(), `"r1"`, "concurrent-base-candidate")
	if code != http.StatusCreated {
		t.Fatalf("stage: %d %s", code, body)
	}
	codes := make([]int, writers)
	outcomes := make([]Outcome, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			codes[i], outcomes[i], _ = e.activate(harness, staged.Candidate.CandidateRecord, 60, `"r1"`, fmt.Sprintf("concurrent-activation-%04d", i))
		})
	}
	wg.Wait()
	winner := -1
	conflicts := 0
	for i, code := range codes {
		switch code {
		case http.StatusOK:
			if winner >= 0 {
				t.Fatal("two activations won")
			}
			winner = i
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("activation: %d %+v", code, outcomes[i])
		}
	}
	if winner < 0 || conflicts != writers-1 {
		t.Fatalf("winner %d conflicts %d", winner, conflicts)
	}
	if code, _, body := e.resolve(harness, "confirm", outcomes[winner].Provisional, `"r1"`, "concurrent-base-confirm"); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, body)
	}
	code, body, header := e.as(harness, "GET", "/admin/policy", "")
	revision := fromJSON[store.RevisionInfo](t, body)
	if code != http.StatusOK || header.Get("ETag") != `"r2"` || revision.ID != 2 {
		t.Fatalf("policy after activation race: %d %s", code, body)
	}

	// The running gateway is the audit log's only writer. An operator command
	// refused by the lock must not touch it, even when the log ends in a
	// record the gateway is still writing.
	e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "admin.read") >= 1 })
	partial := `{"schema":4,"kind":"request.admitted","id":"in-progress`
	audit, err := os.OpenFile(e.auditLog, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	audit.WriteString(partial)
	audit.Close()
	for _, command := range [][]string{
		{"enrol", "--admin", "late", "--permissions", "read"},
		{"backup", "--output", filepath.Join(e.dir, "busy.db")},
	} {
		started := time.Now()
		out, code := e.runOperator(binaries.plain, nil, command[0], command[1:]...)
		if code == 0 || !strings.Contains(out, "held by another gateway process") {
			t.Errorf("%s while the gateway runs: %d %s", command[0], code, out)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("%s waited %s for the lock", command[0], elapsed)
		}
	}
	data, err := os.ReadFile(e.auditLog)
	if err != nil || !strings.HasSuffix(string(data), "\n"+partial) {
		t.Errorf("a refused operator command wrote to the running gateway's audit log: %q", data[max(0, len(data)-200):])
	}
	e.stop()
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM revision`: 2,
		`SELECT COUNT(*) FROM transition WHERE kind = 'cpa.activate' AND outcome = 'rejected'`: writers - 1,
		`SELECT COUNT(*) FROM enrolment`: 0,
	} {
		if n := queryInt(t, e.database, query); n != want {
			t.Errorf("%s = %d, want %d", query, n, want)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(e.dir, "busy.db*")); len(matches) != 0 {
		t.Errorf("a refused backup left files: %v", matches)
	}
}

// A refused mutation is recorded as rejected with its reason and
// actor, and nothing it asked for is stored.
func TestRejectedMutationsAreRecorded(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	_, readerPassword := e.addAdmin(binaries.plain, "reader", "read,read_history")
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	reader := e.mustLogin("reader", readerPassword)

	// Stale and invalid: recorded in history. Unauthorised: refused before
	// the controller, recorded as an audit denial only.
	e.stage(harness, e.protectedPolicy(), `"r7"`, "rejected-stale-01")
	invalid := e.protectedPolicy()
	invalid.Permits = append(invalid.Permits, policy.Permit{Role: "ROOT", Operation: "read-a"})
	e.stage(harness, invalid, `"r1"`, "rejected-invalid-01")
	if code, _, _ := e.as(reader, http.MethodPut, "/admin/blocked-accounts/carol", `{"reason":"contain"}`,
		"If-Match", `"r1"`, "Idempotency-Key", "rejected-unauthorised"); code != http.StatusForbidden {
		t.Fatalf("reader's change: %d", code)
	}

	code, body, _ := e.as(reader, http.MethodGet, "/admin/history", "")
	history := fromJSON[struct {
		Items []store.HistoryEntry `json:"items"`
	}](t, body)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	reasons := map[string]string{}
	for _, entry := range history.Items {
		if entry.Kind == "candidate.create" {
			if entry.Outcome != "rejected" || entry.Actor != harnessAdmin || entry.Revision != 0 || entry.PublishedAt != nil {
				t.Errorf("history entry %+v", entry)
			}
			reasons[entry.ID] = entry.Reason
		}
	}
	want := map[string]string{"admin:harness:rejected-stale-01": "PRECONDITION_FAILED", "admin:harness:rejected-invalid-01": "UNKNOWN_REFERENCE"}
	if fmt.Sprint(reasons) != fmt.Sprint(want) {
		t.Fatalf("recorded refusals %v, want %v", reasons, want)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "transition.rejected") >= 2 })
	denied := false
	for _, ev := range events {
		if ev["kind"] == "transition.rejected" && ev["actor"] != harnessAdmin {
			t.Errorf("rejection event %v", ev)
		}
		denied = denied || (ev["kind"] == "admin.denied" && ev["actor"] == "reader" && ev["reason"] == "permission")
	}
	if !denied {
		t.Error("the reader's refused change was not recorded as a denial")
	}
	if s := e.status(); s.Revision != 1 {
		t.Fatalf("refusals changed the revision to %d", s.Revision)
	}
	encoded, _ := json.Marshal(history)
	if strings.Contains(string(encoded), "rejected-unauthorised") {
		t.Fatal("an unauthorised request reached the controller's history")
	}
}
