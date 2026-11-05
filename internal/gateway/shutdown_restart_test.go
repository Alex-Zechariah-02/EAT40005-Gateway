// Native stop under load, restart and kill.

package gateway

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// A stop during traffic on kept connections, with administrative changes in
// flight and controller timers armed, still records every request it took:
// each upstream receipt has its admission and outcome records, each refusal a
// client received has its record, and nothing is recorded twice. The process
// exits with status 0.
func TestNativeStopUnderLoad(t *testing.T) {
	e := newEnv(t)
	provisional := e.protectedPolicy()
	provisional.Roles = append(provisional.Roles, policy.Role{ID: "TRIAL"})
	provisional.Permits = append(provisional.Permits, policy.Permit{Role: "TRIAL", Operation: "read-a"})
	base := e.protectedPolicy()
	seed(t, e.database, base, seedOptions{provisional: &provisional, deadline: time.Now().Add(time.Hour)})
	e.interruptible = true
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	var mu sync.Mutex
	var refusals []string
	stopped := make(chan struct{})
	var wg sync.WaitGroup
	for c := range 8 {
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			token := issuer.tokenFor(t, "alice")
			for i := 0; ; i++ {
				select {
				case <-stopped:
					return
				default:
				}
				path := "/a/x"
				if (c+i)%3 == 0 {
					path = "/d"
				}
				req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+path, http.NoBody)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := client.Do(req)
				if err != nil {
					time.Sleep(time.Millisecond)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if _, id := requestID(resp, string(body)); id != "" {
					mu.Lock()
					refusals = append(refusals, id)
					mu.Unlock()
				}
			}
		})
	}
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stopped:
				return
			default:
			}
			if code, _, _ := e.adminSend(http.MethodGet, "/admin/status", "", harness.headers(e)...); code == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			// A mutation that is refused: the request names another
			// provisional revision.
			e.adminSend(http.MethodPost, "/admin/provisional/rollback", `{"revision":3}`, append(harness.headers(e),
				"If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("load-mismatch-%08d", i))...)
		}
	})
	waitFor(t, time.Now().Add(20*time.Second), "traffic", func() bool { return e.a.count() >= 200 })
	interrupt(t, e.process)
	code := e.waitExit()
	close(stopped)
	wg.Wait()
	if code != exitStopped {
		t.Fatalf("exit status %d\n%s", code, e.output)
	}

	events := e.auditEvents()
	admitted, forwarded := map[string]int{}, map[string]int{}
	for _, ev := range events {
		id, _ := ev["request_id"].(string)
		switch ev["kind"] {
		case "request.admitted":
			admitted[id]++
		case "request.forwarded":
			forwarded[id]++
		}
	}
	for _, r := range e.a.receipts() {
		if admitted[r.RequestID] != 1 || forwarded[r.RequestID] != 1 {
			t.Fatalf("receipt %s: %d admission and %d outcome records", r.RequestID, admitted[r.RequestID], forwarded[r.RequestID])
		}
	}
	recorded := rejectedIDs(events)
	for _, id := range refusals {
		if recorded[id] != 1 {
			t.Fatalf("refusal %s recorded %d times", id, recorded[id])
		}
	}
	if lossOf(events) != 0 {
		t.Fatalf("loss counted %d", lossOf(events))
	}
	t.Logf("%d forwarded, %d refusals received, %d records; exit %d", e.a.count(), len(refusals), len(events), code)
}

// A graceful stop leaves the authority coherent for the next start: a
// provisional base and an override whose times pass while the gateway is
// stopped end exactly once at the restart, and are never served again; every
// committed transition is audited; the audit files, rotated many times,
// hold complete records only.
func TestNativeRestartAfterGracefulStop(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	settings := e.nativeSettings()
	section(settings, "gateway")["audit_max_bytes"] = minAuditBytes
	section(settings, "gateway")["audit_max_files"] = maxAuditFiles
	e.writeNative(settings)
	e.interruptible = true
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	override := e.mustOverride(harness, operationContainment("read-b", 3), `"r1"`, "restart-override-create")
	candidate := e.protectedPolicy()
	candidate.Roles = append(candidate.Roles, policy.Role{ID: "TRIAL"})
	candidate.Permits = append(candidate.Permits, policy.Permit{Role: "TRIAL", Operation: "read-a"})
	_, activated := e.provisionalFrom(harness, candidate, 1, 3, "restart-cpa")
	if code, _ := e.get("/a/x", bearer(t, "carol")...); code != http.StatusOK {
		t.Fatalf("carol under the provisional base: %d", code)
	}
	e.refuse(20)

	interrupt(t, e.process)
	if code := e.waitExit(); code != exitStopped {
		t.Fatalf("exit status %d\n%s", code, e.output)
	}
	time.Sleep(time.Until(activated.Deadline.Add(500 * time.Millisecond)))
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id LIKE 'auto-%'`); n != 0 {
		t.Fatalf("%d automatic transitions by the stopped gateway", n)
	}

	e.start(binaries.plain)
	s := e.status()
	if s.Revision != 1 || s.Provisional != 0 || len(s.Overrides) != 0 || s.Admission != "ready" {
		t.Fatalf("after the restart %+v", s)
	}
	if code, _ := e.get("/a/x", bearer(t, "carol")...); code != http.StatusForbidden {
		t.Fatalf("carol after the restart: %d", code)
	}
	if code, _ := e.get("/b/x", bearer(t, "bob")...); code != http.StatusOK {
		t.Fatalf("the expired override still applies: %d", code)
	}
	interrupt(t, e.process)
	if code := e.waitExit(); code != exitStopped {
		t.Fatalf("exit status %d after the restart\n%s", code, e.output)
	}
	for _, id := range []string{fmt.Sprintf("auto-rollback:%d", activated.Provisional), fmt.Sprintf("auto-expire:%d", override.Override)} {
		if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = ? AND outcome = 'committed' AND audited_at IS NOT NULL`, id); n != 1 {
			t.Fatalf("%s: %d audited transitions, want 1", id, n)
		}
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE outcome = 'committed' AND audited_at IS NULL`); n != 0 {
		t.Fatalf("%d committed transitions without their audit record", n)
	}

	files, _ := filepath.Glob(e.auditLog + "*")
	records := 0
	for _, file := range files {
		for _, ev := range fileEvents(t, file) {
			records++
			if ev.Kind == "audit.recovered" {
				t.Fatalf("incomplete record in %s", filepath.Base(file))
			}
		}
	}
	if len(files) < 3 {
		t.Fatalf("%d audit files; the rotation was not exercised", len(files))
	}
	t.Logf("%d audit files, %d records", len(files), records)
}

// An abrupt kill is not a graceful stop: it drains nothing and claims
// nothing. Events still queued are absent after the restart, no loss count
// is invented for them, and no record is incomplete, because every write is
// whole.
func TestNativeKillMakesNoDrainClaim(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault, "GATEWAY_AUDIT_HOLD="+filepath.Join(e.dir, "audit-hold"))
	_, held := e.holdWriter()
	ids := append([]string{held}, e.refuse(20)...)
	e.stop()
	output := e.output.String()
	for _, claim := range []string{"draining", `"msg":"stopped"`, "stop incomplete"} {
		if strings.Contains(output, claim) {
			t.Fatalf("a killed process reported %q\n%s", claim, output)
		}
	}
	e.start(binaries.plain)
	events := e.auditEvents()
	recorded := rejectedIDs(events)
	for _, id := range ids {
		if recorded[id] != 0 {
			t.Fatalf("request %s recorded after the kill", id)
		}
	}
	if lossOf(events) != 0 || countKind(events, "audit.recovered") != 0 {
		t.Fatalf("after the kill: loss %d, recovered %d", lossOf(events), countKind(events, "audit.recovered"))
	}
}

// An interrupt (Ctrl+Break on Windows) stops the gateway through its graceful
// path: the process exits with status 0, both listeners close, the audit file
// ends with a complete record, and the next start serves the same generation
// without finding an incomplete record. Every admission, which waits for its
// durable record, is in the file, and so is every outcome and refusal event,
// queued without acknowledgement, because the stop drains the queue.
func TestInterruptStopsGatewayGracefully(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.interruptible = true
	e.start(binaries.plain)
	generation := e.status().Generation
	for range 3 {
		if code, _ := e.get("/a/x"); code != http.StatusOK {
			t.Fatalf("admitted request: %d", code)
		}
		if code, _ := e.get("/d"); code != http.StatusForbidden {
			t.Fatalf("denied request: %d", code)
		}
	}

	interrupt(t, e.process)
	if code := e.waitExit(); code != 0 {
		t.Fatalf("exit status %d after the interrupt\n%s", code, e.output)
	}
	if dialable(e.dataAddr) || dialable(e.admin) {
		t.Fatal("a listener still accepts connections after the gateway exited")
	}
	data, err := os.ReadFile(e.auditLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("the audit file does not end with a complete record")
	}
	events := e.auditEvents()
	for kind, want := range map[string]int{"request.admitted": 3, "request.forwarded": 3, "request.rejected": 3} {
		if n := countKind(events, kind); n != want {
			t.Fatalf("%d %s records in the audit file, want %d", n, kind, want)
		}
	}

	e.interruptible = false
	e.start(binaries.plain)
	if s := e.status(); s.Generation != generation {
		t.Fatalf("generation %s after the restart, want %s", s.Generation, generation)
	}
	events = e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "controller.reconciled") >= 2 })
	if countKind(events, "controller.reconciled") != 2 || countKind(events, "audit.recovered") != 0 {
		t.Fatalf("restart events: %d reconciled, %d recovered", countKind(events, "controller.reconciled"), countKind(events, "audit.recovered"))
	}
}
