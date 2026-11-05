// Audit close helpers, drain and acceptance boundary.

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
func heldAudit(t *testing.T) (*AuditLog, func()) {
	t.Helper()
	a := &AuditLog{
		boot:     "test",
		path:     filepath.Join(t.TempDir(), "audit.log"),
		maxBytes: DefaultAuditBytes,
		maxFiles: DefaultAuditFiles,
		queue:    make(chan auditEntry, AuditQueueSize),
		done:     make(chan struct{}),
	}
	if _, err := a.open(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { go a.run() }) }
	t.Cleanup(func() {
		release()
		a.Close(context.Background())
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
func isClosing(a *AuditLog) bool {
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
	for _, n := range []int{1, 5, auditBatchSize + 1, AuditQueueSize} {
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
			go func() { closed <- a.Close(context.Background()) }()
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
		err := a.Close(ctx)
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
