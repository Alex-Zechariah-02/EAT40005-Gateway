// Native stop drains queued audit events.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
)

// draining returns the pending count of the gateway's drain line, or -1.
func (e *env) draining() int {
	m := regexp.MustCompile(`"msg":"draining the audit log","pending":(\d+)`).FindStringSubmatch(e.output.String())
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// refuse sends n denied requests and returns their request identities.
func (e *env) refuse(n int) []string {
	e.t.Helper()
	ids := make([]string, 0, n)
	for range n {
		resp, body := e.exchange(http.MethodGet, "/d", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusForbidden || id == "" {
			e.t.Fatalf("denied request: %d %s", resp.StatusCode, body)
		}
		ids = append(ids, id)
	}
	return ids
}

// holdWriter makes the gateway's writer hold before its next write and
// returns the hold file and the identity of the refusal whose event the
// writer then holds.
func (e *env) holdWriter() (string, string) {
	e.t.Helper()
	// The startup events are written first, so the held batch is the
	// refusal's.
	e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "controller.reconciled") > 0 })
	hold := filepath.Join(e.dir, "audit-hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
	held := e.refuse(1)[0]
	waitForFile(e.t, hold+".held")
	return hold, held
}

// rejectedIDs counts the request.rejected records of each request.
func rejectedIDs(events []map[string]any) map[string]int {
	ids := map[string]int{}
	for _, ev := range events {
		if ev["kind"] == "request.rejected" {
			ids[ev["request_id"].(string)]++
		}
	}
	return ids
}

func lossOf(events []map[string]any) int {
	n := 0
	for _, ev := range events {
		if ev["kind"] == "audit.loss" {
			n += int(ev["lost"].(float64))
		}
	}
	return n
}

// The native process, stopped by a real Ctrl+Break, writes every event it
// accepted before the stop, also when they wait behind a writer held until
// the drain has begun: the process exits with status 0, and the reopened audit
// file holds each of them once. Events refused because the queue was full are
// counted in the file, exactly. The next start finds a complete file.
func TestNativeStopDrainsQueuedEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued int
		lost   int
	}{
		{"events behind a held writer", 300, 0},
		{"more events than the queue holds", audit.AuditQueueSize + 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			e.interruptible = true
			e.start(binaries.fault, "GATEWAY_AUDIT_HOLD="+filepath.Join(e.dir, "audit-hold"))
			hold, held := e.holdWriter()
			ids := append([]string{held}, e.refuse(tc.queued)...)

			interrupt(t, e.process)
			e.waitOutput(`"msg":"draining the audit log"`)
			// The held event and every queued one are pending when the drain
			// begins; nothing was written since the writer was held.
			if pending := e.draining(); pending != len(ids)-tc.lost {
				t.Fatalf("pending %d at the drain, want %d", pending, len(ids)-tc.lost)
			}
			if err := os.Remove(hold); err != nil {
				t.Fatal(err)
			}
			if code := e.waitExit(); code != exitStopped {
				t.Fatalf("exit status %d\n%s", code, e.output)
			}
			if !strings.Contains(e.output.String(), `"msg":"stopped"`) || strings.Contains(e.output.String(), "stop incomplete") {
				t.Fatalf("stop not reported complete\n%s", e.output)
			}

			events := e.auditEvents()
			recorded := rejectedIDs(events)
			for i, id := range ids {
				want := 1
				if i >= len(ids)-tc.lost {
					want = 0 // refused: the queue was full
				}
				if recorded[id] != want {
					t.Fatalf("request %d (%s) recorded %d times, want %d", i, id, recorded[id], want)
				}
			}
			if loss := lossOf(events); loss != tc.lost {
				t.Fatalf("loss counted %d, want %d", loss, tc.lost)
			}

			e.interruptible = false
			e.start(binaries.plain)
			if n := countKind(e.auditEvents(), "audit.recovered"); n != 0 {
				t.Fatalf("%d incomplete records found after the graceful stop", n)
			}
		})
	}
}

// A drain that cannot finish within its bound, because the writer stays
// blocked, ends the process with status 3 and says how many accepted events
// are not confirmed written; it never reports a completed stop, and those
// events are not in the file.
func TestNativeStopReportsIncompleteDrain(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.interruptible = true
	e.start(binaries.fault, "GATEWAY_AUDIT_HOLD="+filepath.Join(e.dir, "audit-hold"))
	_, held := e.holdWriter()
	ids := append([]string{held}, e.refuse(50)...)

	interrupt(t, e.process)
	e.waitOutput(`"msg":"draining the audit log"`)
	started := time.Now()
	if code := e.waitExit(); code != exitIncomplete {
		t.Fatalf("exit status %d\n%s", code, e.output)
	}
	if elapsed := time.Since(started); elapsed < audit.AuditDrainWait-500*time.Millisecond || elapsed > audit.AuditDrainWait+5*time.Second {
		t.Fatalf("exited %s after the drain began, want about %s", elapsed, audit.AuditDrainWait)
	}
	output := e.output.String()
	if !strings.Contains(output, `"msg":"stop incomplete"`) || !strings.Contains(output, "51 accepted events") || strings.Contains(output, `"msg":"stopped"`) {
		t.Fatalf("incomplete stop not reported\n%s", output)
	}
	recorded := rejectedIDs(e.auditEvents())
	for _, id := range ids {
		if recorded[id] != 0 {
			t.Fatalf("request %s recorded although the writer never wrote it", id)
		}
	}
}

// A writer that fails, in its sync or in every write, leaves accepted events
// unwritten: the stop exits with status 3 and says so, and no credential
// reaches the output.
func TestNativeStopReportsAuditFailure(t *testing.T) {
	for _, fault := range []string{"audit-sync", "audit-full"} {
		t.Run(fault, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.interruptible = true
			e.start(binaries.fault, "GATEWAY_FAULT="+fault)
			waitFor(t, time.Now().Add(10*time.Second), "the audit failure", func() bool {
				return strings.Contains(e.output.String(), "audit log failed")
			})
			token := issuer.tokenFor(t, "alice")
			if code, _ := e.get("/a/x", "Authorization", "Bearer "+token); code != http.StatusServiceUnavailable {
				t.Fatalf("admission with the failed audit log: %d", code)
			}
			e.refuse(3)
			e.assertReceipts(0, 0, 0)
			interrupt(t, e.process)
			if code := e.waitExit(); code != exitIncomplete {
				t.Fatalf("exit status %d\n%s", code, e.output)
			}
			output := e.output.String()
			if !strings.Contains(output, `"msg":"stop incomplete"`) || !strings.Contains(output, "audit log incomplete") || strings.Contains(output, `"msg":"stopped"`) {
				t.Fatalf("failed audit log not reported\n%s", output)
			}
			if strings.Contains(output, token[:40]) || strings.Contains(output, "Bearer") {
				t.Fatalf("the output holds a credential\n%s", output)
			}
		})
	}
}
