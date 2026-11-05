// Audit close loss records, writer failures and timeout.

package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Events refused because the queue was full are counted, and the count is
// written with the queued events when the writer continues: the stop
// reports no loss it did not see and hides none it did.
func TestAuditCloseRecordsLoss(t *testing.T) {
	a, release := heldAudit(t)
	for i := range AuditQueueSize + 10 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	if a.lost.Load() != 10 || a.accepted.Load() != AuditQueueSize {
		t.Fatalf("lost %d, accepted %d", a.lost.Load(), a.accepted.Load())
	}
	release()
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := fileEvents(t, a.path)
	if len(events) != AuditQueueSize+1 || lossCounted(events) != 10 || lossRecords(events) != 1 {
		t.Fatalf("%d records, loss %d in %d records", len(events), lossCounted(events), lossRecords(events))
	}
	for i, e := range events[1:] {
		if e.RequestID != strconv.Itoa(i) || e.Seq != uint64(i+1) {
			t.Fatalf("record %d: request %q, seq %d", i, e.RequestID, e.Seq)
		}
	}
	// The refused events kept their sequence numbers 1025 to 1034, so the
	// gap is visible; the loss record comes after them.
	if loss := events[0]; loss.Kind != "audit.loss" || loss.Seq != AuditQueueSize+11 {
		t.Fatalf("first record %+v", loss)
	}
}

// A writer that can no longer write fails the log: the stop reports the
// accepted events and the refused-event count it could not write, a waiting
// acknowledgement fails, and nothing claims success. The count is not lost
// with the failed write that carried it.
func TestAuditCloseReportsWriterFailure(t *testing.T) {
	a, release := heldAudit(t)
	for i := range AuditQueueSize - 1 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	ack := make(chan error, 1)
	go func() { ack <- a.Commit(context.Background(), Event{Kind: "request.admitted"}) }()
	waitFor(t, time.Now().Add(5*time.Second), "the admission queued", func() bool { return a.accepted.Load() == AuditQueueSize })
	for range 3 {
		a.Record(Event{Kind: "request.rejected"})
	}
	// The disk goes away: every write fails.
	a.file.Close()
	release()
	err := a.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d accepted events and the count of 3 refused events not written", AuditQueueSize)) {
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
	a := &AuditLog{
		boot:     "test",
		path:     path,
		maxBytes: int64(len(record)),
		maxFiles: 2,
		queue:    make(chan auditEntry, AuditQueueSize),
		done:     make(chan struct{}),
	}
	if _, err := a.open(); err != nil {
		t.Fatal(err)
	}
	for i := range AuditQueueSize + 3 {
		a.Record(Event{Kind: "request.rejected", RequestID: strconv.Itoa(i)})
	}
	go a.run()
	err := a.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d accepted events and the count of 3 refused events not written", AuditQueueSize)) {
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
	go func() { closed <- a.Close(context.Background()) }()
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
	err := a.Close(ctx)
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
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("close after the writer finished: %v", err)
	}
	if events := fileEvents(t, a.path); len(events) != 50 {
		t.Fatalf("%d records after the writer continued", len(events))
	}
}
