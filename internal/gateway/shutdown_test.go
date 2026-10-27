package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests check what a graceful stop does with the audit events queued
// for the asynchronous writer. In this process the writer is held simply by
// not starting it, so events queue behind it at a known point; the gateway
// processes hold it with the fault build's GATEWAY_AUDIT_HOLD.

// heldAudit opens an audit log in a new directory without starting its
// writer; release starts it, once. When the test ends the writer is released
// and the log closed, so the file is closed before its directory is removed.
func heldAudit(t *testing.T) (*auditLog, func()) {
	t.Helper()
	a := &auditLog{
		boot:     "test",
		path:     filepath.Join(t.TempDir(), "audit.log"),
		maxBytes: defaultAuditBytes,
		maxFiles: defaultAuditFiles,
		queue:    make(chan auditEntry, auditQueueSize),
		done:     make(chan struct{}),
	}
	if _, err := a.open(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { go a.run() }) }
	t.Cleanup(func() {
		release()
		a.close(context.Background())
	})
	return a, release
}

// fileRecord is what these tests read of one audit record.
type fileRecord struct {
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	Seq       uint64 `json:"seq"`
	Lost      uint64 `json:"lost"`
}

// fileEvents reads every record of an audit file.
func fileEvents(t *testing.T, path string) []fileRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []fileRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e fileRecord
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line is not one record: %q", line)
		}
		events = append(events, e)
	}
	return events
}

// lossCounted sums the audit.loss records of events.
func lossCounted(events []fileRecord) uint64 {
	var n uint64
	for _, e := range events {
		if e.Kind == "audit.loss" {
			n += e.Lost
		}
	}
	return n
}

// lossRecords counts the audit.loss records of events.
func lossRecords(events []fileRecord) uint64 {
	var n uint64
	for _, e := range events {
		if e.Kind == "audit.loss" {
			n++
		}
	}
	return n
}

// isClosing reports whether a's close has begun.
func isClosing(a *auditLog) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.closing
}

// Every event accepted before the close begins is written, in order, however
// many wait behind the writer: one, a few, more than a batch, a full queue.
// From the moment the close begins nothing more is queued: a late Record is
// counted, and its count written before the file closes; a late Commit is
// refused at once. The close does not return before the writer has written
// everything, and then reports success.
func TestAuditCloseDrainsAcceptedEvents(t *testing.T) {
	for _, n := range []int{1, 5, auditBatchSize + 1, auditQueueSize} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			a, release := heldAudit(t)
			commits := min(2, n-1)
			acks := make(chan error, commits)
			for i := range commits {
				go func() {
					acks <- a.Commit(context.Background(), Event{Kind: "request.admitted", RequestID: fmt.Sprintf("commit-%d", i)})
				}()
			}
			for i := range n - commits {
				a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
			}
			waitFor(t, time.Now().Add(5*time.Second), "every event queued", func() bool { return a.accepted.Load() == uint64(n) })
			if a.lost.Load() != 0 {
				t.Fatalf("%d events refused before the close", a.lost.Load())
			}

			closed := make(chan error, 1)
			go func() { closed <- a.close(context.Background()) }()
			waitFor(t, time.Now().Add(5*time.Second), "the close to begin", func() bool { return isClosing(a) })
			a.Record(Event{Kind: "request.rejected", RequestID: "late"})
			if err := a.Commit(context.Background(), Event{Kind: "request.admitted", RequestID: "late-commit"}); !errors.Is(err, errAuditFailed) || !strings.Contains(err.Error(), "closed") {
				t.Fatalf("Commit after the close began: %v", err)
			}
			if a.lost.Load() != 1 || a.accepted.Load() != uint64(n) {
				t.Fatalf("after the close began: lost %d, accepted %d", a.lost.Load(), a.accepted.Load())
			}
			select {
			case err := <-closed:
				t.Fatalf("close returned %v while the writer was held", err)
			default:
			}

			release()
			if err := <-closed; err != nil {
				t.Fatalf("close: %v", err)
			}
			for range commits {
				if err := <-acks; err != nil {
					t.Fatalf("acknowledgement: %v", err)
				}
			}
			events := fileEvents(t, a.path)
			if len(events) != n+1 || lossCounted(events) != 1 {
				t.Fatalf("%d records (loss %d), want %d events and one loss count", len(events), lossCounted(events), n)
			}
			next, commitsSeen := 0, 0
			for _, e := range events {
				switch {
				case e.Kind == "audit.loss":
				case strings.HasPrefix(e.RequestID, "commit-"):
					commitsSeen++
				case e.RequestID != strconv.Itoa(next):
					t.Fatalf("record %q where %d was due", e.RequestID, next)
				default:
					next++
				}
			}
			if next != n-commits || commitsSeen != commits {
				t.Fatalf("%d recorded and %d acknowledged events, want %d and %d", next, commitsSeen, n-commits, commits)
			}
		})
	}
}

// Producers racing the close neither panic nor hang, and the boundary is
// exact: every acknowledged event is written once, every refused one is
// absent, and every recorded event is either written or counted.
func TestAuditCloseAcceptanceBoundary(t *testing.T) {
	for round := range 20 {
		a, release := heldAudit(t)
		release()
		const producers = 8
		var stopped atomic.Bool
		var recordings atomic.Int64
		acknowledged, refused := make([][]string, producers), make([][]string, producers)
		var wg sync.WaitGroup
		for p := range producers {
			wg.Go(func() {
				for i := 0; !stopped.Load(); i++ {
					if p%2 == 1 {
						recordings.Add(1)
						a.Record(Event{Kind: "request.rejected", RequestID: fmt.Sprintf("r-%d-%d", p, i)})
						continue
					}
					id := fmt.Sprintf("c-%d-%d", p, i)
					switch err := a.Commit(context.Background(), Event{Kind: "request.admitted", RequestID: id}); {
					case err == nil:
						acknowledged[p] = append(acknowledged[p], id)
					case strings.Contains(err.Error(), "closed"), strings.Contains(err.Error(), "queue full"):
						refused[p] = append(refused[p], id)
					default:
						t.Errorf("Commit %s: %v", id, err)
					}
				}
			})
		}
		waitFor(t, time.Now().Add(10*time.Second), "traffic", func() bool { return a.accepted.Load() >= 500 })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := a.close(ctx)
		cancel()
		// Every producer goes on a little after the close returned.
		after := recordings.Load()
		waitFor(t, time.Now().Add(10*time.Second), "attempts after the close", func() bool { return recordings.Load() > after+producers })
		stopped.Store(true)
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: a producer did not finish", round)
		}
		// Only events refused after the writer ended may be unwritten.
		if err != nil && !strings.Contains(err.Error(), ": 0 accepted events") {
			t.Fatalf("round %d: close: %v", round, err)
		}

		events := fileEvents(t, a.path)
		written := map[string]int{}
		recorded := int64(0)
		for _, e := range events {
			written[e.RequestID]++
			if strings.HasPrefix(e.RequestID, "r-") {
				recorded++
			}
		}
		if a.accepted.Load() != a.written.Load() || uint64(len(events))-lossRecords(events) != a.accepted.Load() {
			t.Fatalf("round %d: accepted %d, written %d, records %d", round, a.accepted.Load(), a.written.Load(), len(events))
		}
		for p := range producers {
			for _, id := range acknowledged[p] {
				if written[id] != 1 {
					t.Fatalf("round %d: acknowledged %s written %d times", round, id, written[id])
				}
			}
			for _, id := range refused[p] {
				if written[id] != 0 {
					t.Fatalf("round %d: refused %s written", round, id)
				}
			}
		}
		for id, n := range written {
			if id != "" && n != 1 {
				t.Fatalf("round %d: %s written %d times", round, id, n)
			}
		}
		if counted := recorded + int64(lossCounted(events)) + int64(a.lost.Load()); counted != recordings.Load() {
			t.Fatalf("round %d: %d written + %d counted + %d pending, want %d recorded events", round, recorded, lossCounted(events), a.lost.Load(), recordings.Load())
		}
	}
}

// Events refused because the queue was full are counted, and the count is
// written with the queued events when the writer continues: the stop
// reports no loss it did not see and hides none it did.
func TestAuditCloseRecordsLoss(t *testing.T) {
	a, release := heldAudit(t)
	for i := range auditQueueSize + 10 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	if a.lost.Load() != 10 || a.accepted.Load() != auditQueueSize {
		t.Fatalf("lost %d, accepted %d", a.lost.Load(), a.accepted.Load())
	}
	release()
	if err := a.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := fileEvents(t, a.path)
	if len(events) != auditQueueSize+1 || lossCounted(events) != 10 || lossRecords(events) != 1 {
		t.Fatalf("%d records, loss %d in %d records", len(events), lossCounted(events), lossRecords(events))
	}
	for i, e := range events[1:] {
		if e.RequestID != strconv.Itoa(i) || e.Seq != uint64(i+1) {
			t.Fatalf("record %d: request %q, seq %d", i, e.RequestID, e.Seq)
		}
	}
	// The refused events kept their sequence numbers 1025 to 1034, so the
	// gap is visible; the loss record comes after them.
	if loss := events[0]; loss.Kind != "audit.loss" || loss.Seq != auditQueueSize+11 {
		t.Fatalf("first record %+v", loss)
	}
}

// A writer that can no longer write fails the log: the stop reports the
// accepted events and the refused-event count it could not write, a waiting
// acknowledgement fails, and nothing claims success. The count is not lost
// with the failed write that carried it.
func TestAuditCloseReportsWriterFailure(t *testing.T) {
	a, release := heldAudit(t)
	for i := range auditQueueSize - 1 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	ack := make(chan error, 1)
	go func() { ack <- a.Commit(context.Background(), Event{Kind: "request.admitted"}) }()
	waitFor(t, time.Now().Add(5*time.Second), "the admission queued", func() bool { return a.accepted.Load() == auditQueueSize })
	for range 3 {
		a.Record(Event{Kind: "request.rejected"})
	}
	// The disk goes away: every write fails.
	a.file.Close()
	release()
	err := a.close(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d accepted events and the count of 3 refused events not written", auditQueueSize)) {
		t.Fatalf("close after the write failure: %v", err)
	}
	if !a.failed.Load() || !errors.Is(<-ack, errAuditFailed) {
		t.Fatal("the failure was not reported to the waiting admission")
	}
	if err := a.Commit(context.Background(), Event{Kind: "request.admitted"}); !errors.Is(err, errAuditFailed) {
		t.Fatalf("Commit after the failure: %v", err)
	}
	if events := fileEvents(t, a.path); len(events) != 0 {
		t.Fatalf("%d records written by a failed writer", len(events))
	}
}

// A rotation that fails fails the log as a failed write does: the
// refused-event count its batch carried stays pending, and the stop reports
// it.
func TestAuditCloseKeepsLossAfterFailedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	record := []byte(`{"kind":"earlier"}` + "\n")
	// The active file is full and its one rotated file is the last allowed.
	for _, name := range []string{path, path + ".1"} {
		if err := os.WriteFile(name, record, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := &auditLog{
		boot:     "test",
		path:     path,
		maxBytes: int64(len(record)),
		maxFiles: 2,
		queue:    make(chan auditEntry, auditQueueSize),
		done:     make(chan struct{}),
	}
	if _, err := a.open(); err != nil {
		t.Fatal(err)
	}
	for i := range auditQueueSize + 3 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	go a.run()
	err := a.close(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d accepted events and the count of 3 refused events not written", auditQueueSize)) {
		t.Fatalf("close after the failed rotation: %v", err)
	}
	if !a.failed.Load() {
		t.Fatal("the failed rotation did not fail the log")
	}
	if events := fileEvents(t, path); len(events) != 1 || events[0].Kind != "earlier" {
		t.Fatalf("records after the failed rotation: %+v", events)
	}
}

// An event refused after the close began is counted, and the writer, which
// ends only when the queue is closed and empty, writes that count as its last
// record: a stop that succeeds hides no refusal.
func TestAuditCloseWritesLossCountedWhileClosing(t *testing.T) {
	a, release := heldAudit(t)
	closed := make(chan error, 1)
	go func() { closed <- a.close(context.Background()) }()
	waitFor(t, time.Now().Add(5*time.Second), "the close to begin", func() bool { return isClosing(a) })
	for range 3 {
		a.Record(Event{Kind: "request.rejected"})
	}
	release()
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
	if events := fileEvents(t, a.path); len(events) != 1 || events[0].Kind != "audit.loss" || events[0].Lost != 3 {
		t.Fatalf("records %+v, want one loss record of 3", events)
	}
}

// A close whose bound ends while the writer is blocked returns at the bound
// with what is not confirmed written, and claims nothing more; it does not
// close the file under the writer. When the writer continues it writes
// everything and closes the file itself; a later close reports that.
func TestAuditCloseTimeout(t *testing.T) {
	a, release := heldAudit(t)
	for i := range 50 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := a.close(ctx)
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("close returned after %s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "not drained within the stop bound: 50 accepted events") {
		t.Fatalf("close at the bound: %v", err)
	}
	select {
	case <-a.done:
		t.Fatal("the held writer ended")
	default:
	}
	if a.file == nil || a.file.Sync() != nil {
		t.Fatal("the file was closed under the held writer")
	}

	release()
	<-a.done
	if err := a.close(context.Background()); err != nil {
		t.Fatalf("close after the writer finished: %v", err)
	}
	if events := fileEvents(t, a.path); len(events) != 50 {
		t.Fatalf("%d records after the writer continued", len(events))
	}
}

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
		{"more events than the queue holds", auditQueueSize + 10, 10},
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
	if elapsed := time.Since(started); elapsed < auditDrainWait-500*time.Millisecond || elapsed > auditDrainWait+5*time.Second {
		t.Fatalf("exited %s after the drain began, want about %s", elapsed, auditDrainWait)
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

// A stop during traffic on kept connections, with administrative changes in
// flight and controller timers armed, still records every request it took:
// each upstream receipt has its admission and outcome records, each refusal a
// client received has its record, and nothing is recorded twice. The process
// exits with status 0.
func TestNativeStopUnderLoad(t *testing.T) {
	e := newEnv(t)
	provisional := e.protectedPolicy()
	provisional.Roles = append(provisional.Roles, Role{ID: "TRIAL"})
	provisional.Permits = append(provisional.Permits, Permit{Role: "TRIAL", Operation: "read-a"})
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
	candidate.Roles = append(candidate.Roles, Role{ID: "TRIAL"})
	candidate.Permits = append(candidate.Permits, Permit{Role: "TRIAL", Operation: "read-a"})
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
