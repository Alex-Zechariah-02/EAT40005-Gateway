// Durable audit queue and writer.

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

type auditEntry struct {
	event Event
	done  chan error // nil for events that need no acknowledgement
}

// AuditLog appends events to a local file. Events that gate a decision wait
// until the batch containing them has been synced to disk; other events are
// queued without blocking and counted when they cannot be queued.
//
// An event is accepted when enqueue puts it in the queue. Every accepted event
// is written in order by the one writer, which ends only once close has
// closed the queue and every event in it has been written; so a close that
// returns nil means every accepted event, and the count of every refused one,
// is in the file and synced.
//
// The active file is rotated to "<path>.<n>" before it would exceed maxBytes.
// The writer never deletes records: once maxFiles files exist it fails, and
// protected admission stops until an operator archives files and restarts.
type AuditLog struct {
	boot     string // distinguishes event identities across process starts
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File // used only by the writer, which closes it as it ends
	size     int64    // bytes in the active file; used only by the writer
	rotated  int      // rotated files present; used only by the writer
	next     int      // suffix of the next rotated file; used only by the writer
	openErr  error    // why openAudit could not open the file; never changes
	queue    chan auditEntry
	failed   atomic.Bool
	lost     atomic.Uint64 // events refused and not yet counted in the file
	seq      atomic.Uint64
	accepted atomic.Uint64 // events queued
	// activeBytes and files copy size and rotated+1 for readers outside the
	// writer (the limits read); only the writer stores them.
	activeBytes atomic.Int64
	files       atomic.Int64
	written     atomic.Uint64 // queued events written and synced
	// mu orders queueing against closing: enqueue holds it shared while it
	// checks closing and queues, and close holds it exclusively while it sets
	// closing and closes the queue. Neither blocks while holding it.
	mu      sync.RWMutex
	closing bool
	done    chan struct{} // closed when the writer has ended
	fileErr error         // the result of closing the file; set by the writer before done
}

func (a *AuditLog) Fail(err error) {
	if a.failed.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "gateway: audit log failed; protected admission disabled: %v\n", err)
	}
}

// Commit records an event and waits for durable acknowledgement. A failed or
// closing log, or a full queue, refuses it at once.
func (a *AuditLog) Commit(ctx context.Context, e Event) error {
	if a.failed.Load() {
		return errAuditFailed
	}
	done := make(chan error, 1)
	if err := a.enqueue(e, done); err != nil {
		return fmt.Errorf("%w: %v", errAuditFailed, err)
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: acknowledgement timed out", errAuditFailed)
	}
}

// Record queues an event without waiting. An event that cannot be queued,
// because the log has failed, its queue is full or it is closing, is counted;
// the writer writes the count with its next batch, or as it ends.
func (a *AuditLog) Record(e Event) {
	if a.failed.Load() || a.enqueue(e, nil) != nil {
		a.lost.Add(1)
	}
}

// enqueue stamps and queues an event, unless the log is closing or its queue
// is full. Queueing never waits, so the read lock is held only briefly.
func (a *AuditLog) enqueue(e Event, done chan error) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closing {
		return errAuditClosed
	}
	// Counted before the writer can receive it, so written never exceeds
	// accepted.
	a.accepted.Add(1)
	select {
	case a.queue <- auditEntry{event: a.stamp(e), done: done}:
		return nil
	default:
		a.accepted.Add(^uint64(0))
		return errQueueFull
	}
}

func (a *AuditLog) stamp(e Event) Event {
	e.Schema = EventSchema
	e.Time = time.Now().UTC()
	e.Seq = a.seq.Add(1)
	e.ID = fmt.Sprintf("%s-%d", a.boot, e.Seq)
	if len(e.Path) > MaxLoggedPath {
		e.Path = e.Path[:MaxLoggedPath]
		e.Truncated = true
	}
	// A substring can retain the complete request target's backing storage.
	e.Path = strings.Clone(e.Path)
	return e
}

// run is the writer. It writes the queued events in batches, in queue order,
// until the queue is closed and empty, which only close does; receiving from
// a closed channel yields every value still buffered first. It then writes
// the loss count still pending and closes the file.
func (a *AuditLog) run() {
	defer close(a.done)
	batch := make([]auditEntry, 0, auditBatchSize)
	for entry := range a.queue {
		batch = append(batch[:0], entry)
	fill:
		for len(batch) < auditBatchSize {
			select {
			case entry, open := <-a.queue:
				if !open {
					break fill
				}
				batch = append(batch, entry)
			default:
				break fill
			}
		}
		err := a.write(batch)
		if err == nil {
			a.written.Add(uint64(len(batch)))
		}
		for _, entry := range batch {
			if entry.done != nil {
				entry.done <- err
			}
		}
	}
	if a.lost.Load() > 0 {
		a.write(nil)
	}
	if a.file != nil {
		a.fileErr = a.file.Close()
	}
}

func (a *AuditLog) write(batch []auditEntry) error {
	if a.failed.Load() {
		return errAuditFailed
	}
	var buffer []byte
	lost := a.lost.Swap(0)
	if lost > 0 {
		buffer = appendEvent(buffer, a.stamp(Event{Kind: "audit.loss", Lost: lost}))
	}
	for _, entry := range batch {
		buffer = appendEvent(buffer, entry.event)
	}
	if a.size > 0 && a.size+int64(len(buffer)) > a.maxBytes {
		rotated, err := a.rotate()
		if err != nil {
			// The count was not written, so it stays pending.
			a.lost.Add(lost)
			a.Fail(err)
			return errAuditFailed
		}
		marker := appendEvent(nil, a.stamp(Event{Kind: "audit.rotated", File: rotated}))
		buffer = append(marker, buffer...)
	}
	if err := fault.WriteAudit(a.file, buffer); err != nil {
		a.lost.Add(lost)
		a.Fail(err)
		return errAuditFailed
	}
	a.size += int64(len(buffer))
	a.publishUsage()
	return nil
}

// publishUsage copies the writer's size and file count for other readers.
func (a *AuditLog) publishUsage() {
	a.activeBytes.Store(a.size)
	a.files.Store(int64(a.rotated) + 1)
}

func appendEvent(buffer []byte, e Event) []byte {
	line, _ := json.Marshal(e)
	return append(append(buffer, line...), '\n')
}

// Close ends the log. From the moment it begins no event can be queued:
// Record counts it as lost and Commit refuses it. It then waits, until ctx is
// done, for the writer to write every event queued before, then the pending
// loss count, and to close the file. It returns nil only when all of that was
// written and synced; otherwise it reports what is not confirmed written. A
// writer blocked in the file system cannot be interrupted: when ctx ends
// first, the writer keeps the file and may still finish. Each later call
// reports the state it then finds.
func (a *AuditLog) Close(ctx context.Context) error {
	a.mu.Lock()
	if !a.closing {
		a.closing = true
		close(a.queue)
	}
	a.mu.Unlock()
	select {
	case <-a.done:
	case <-ctx.Done():
		select {
		case <-a.done:
		default:
			events, lost := a.Unconfirmed()
			return fmt.Errorf("audit log not drained within the stop bound: %d accepted events and the count of %d refused events not confirmed written", events, lost)
		}
	}
	if events, lost := a.Unconfirmed(); events > 0 || lost > 0 {
		return fmt.Errorf("audit log incomplete: %d accepted events and the count of %d refused events not written", events, lost)
	}
	return a.fileErr
}

// Unconfirmed returns the number of accepted events not yet written and
// synced, and the count of refused events not yet in the file.
func (a *AuditLog) Unconfirmed() (events, lost uint64) {
	return a.accepted.Load() - a.written.Load(), a.lost.Load()
}
