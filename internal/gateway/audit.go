package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	auditQueueSize = 1024
	auditBatchSize = 128
	maxLoggedPath  = 256
	// maxRecordBytes bounds one record, so an incomplete last record is
	// always found within this many bytes of the end of the file.
	maxRecordBytes = 64 << 10
	// auditDrainWait bounds how long a graceful stop waits for the writer to
	// write what was queued before the log closed.
	auditDrainWait = 5 * time.Second
)

// eventSchema is the version of the event envelope below.
const eventSchema = 6

var (
	errAuditFailed = errors.New("audit log unavailable")
	errAuditClosed = errors.New("closed")
	errQueueFull   = errors.New("queue full")
)

// Event is one security record. Client-controlled strings are bounded and
// JSON-encoded, so they cannot forge additional records. Credentials, bodies
// and query strings are never recorded. Subject is set only once a token has
// been verified; Actor only once an administrator has been authenticated, and
// Session is a non-secret reference to that administrator's session.
type Event struct {
	Schema     int       `json:"schema"`
	ID         string    `json:"event_id"`
	Time       time.Time `json:"time"`
	Seq        uint64    `json:"seq"`
	Kind       string    `json:"kind"`
	RequestID  string    `json:"request_id,omitempty"`
	Stage      string    `json:"stage,omitempty"` // terminal stage of a request event
	Stages     stages    `json:"stages,omitzero"`
	Decision   string    `json:"decision,omitempty"`
	Status     int       `json:"status,omitempty"`
	Error      string    `json:"error,omitempty"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	Truncated  bool      `json:"path_truncated,omitempty"`
	Operation  string    `json:"operation,omitempty"`
	API        string    `json:"api,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	Session    string    `json:"session,omitempty"`
	Generation string    `json:"generation,omitempty"`
	Profile    string    `json:"profile,omitempty"`
	Upstream   string    `json:"upstream,omitempty"`
	Transition string    `json:"transition,omitempty"`
	Revision   int64     `json:"revision,omitempty"`
	Candidate  int64     `json:"candidate,omitempty"`
	// A provisional base's events name its revision, the confirmed revision
	// it returns to on rollback and its deadline.
	Provisional    int64     `json:"provisional,omitempty"`
	RollbackTarget int64     `json:"rollback_target,omitempty"`
	Deadline       time.Time `json:"deadline,omitzero"`
	// A temporary override's events name it, the control and target it
	// governs and its expiry time.
	Override int64     `json:"override,omitempty"`
	Target   string    `json:"target,omitempty"`
	Expires  time.Time `json:"expires_at,omitzero"`
	Reason   string    `json:"reason,omitempty"`
	Lost     uint64    `json:"lost,omitempty"`
	File     string    `json:"file,omitempty"`   // audit file named by audit.* events
	Offset   int64     `json:"offset,omitempty"` // start of an incomplete record
}

// Request stages in evaluation order. BODY covers framing, size, media type
// and JSON decoding; VALIDATION covers the header and body schemas.
const (
	stageIngress = iota
	stageCapture
	stageCanonical
	stageRoute
	stageAccess
	stageJWT
	stageRBAC
	stageRate
	stageBody
	stageValidation
	stageAudit
	stageBind
	stageForward
	stageCount
)

var stageNames = [stageCount]string{
	"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS", "JWT", "RBAC",
	"RATE", "BODY", "VALIDATION", "AUDIT", "BIND", "FORWARD",
}

// Stage results. A stage without a result was not evaluated; a stage the
// operation does not require, such as authentication of a public operation,
// is not applicable.
const (
	stagePass          = "PASS"
	stageReject        = "REJECT"
	stageFail          = "FAIL"
	stageNotRun        = "NE"
	stageNotApplicable = "NA"
)

// stages holds the result of each stage when the event was generated. It is
// an array, so every event carries its own copy.
type stages [stageCount]string

func (s stages) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, name := range stageNames {
		result := s[i]
		if result == "" {
			result = stageNotRun
		}
		if i > 0 {
			buffer.WriteByte(',')
		}
		fmt.Fprintf(&buffer, "%q:%q", name, result)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

type auditEntry struct {
	event Event
	done  chan error // nil for events that need no acknowledgement
}

// auditLog appends events to a local file. Events that gate a decision wait
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
type auditLog struct {
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
	written  atomic.Uint64 // queued events written and synced
	// mu orders queueing against closing: enqueue holds it shared while it
	// checks closing and queues, and close holds it exclusively while it sets
	// closing and closes the queue. Neither blocks while holding it.
	mu      sync.RWMutex
	closing bool
	done    chan struct{} // closed when the writer has ended
	fileErr error         // the result of closing the file; set by the writer before done
}

// openAudit never fails: an unusable file yields a log in the failed state,
// which refuses every acknowledgement and therefore every admission.
func openAudit(path string, maxBytes int64, maxFiles int) *auditLog {
	a := &auditLog{
		boot:     newRequestID()[:16],
		path:     path,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
		queue:    make(chan auditEntry, auditQueueSize),
		done:     make(chan struct{}),
	}
	incomplete, err := a.open()
	if err != nil {
		if a.file != nil {
			a.file.Close()
			a.file = nil
		}
		a.openErr = err
		a.fail(fmt.Errorf("opening audit log: %w", err))
		close(a.done)
		return a
	}
	if incomplete >= 0 {
		// A crash interrupted the last record. It stays in the file for
		// inspection; this event names where it starts.
		a.Record(Event{Kind: "audit.recovered", File: filepath.Base(path), Offset: incomplete})
	}
	go a.run()
	return a
}

// open checks and opens the active file and finds the rotated files. A
// non-empty file that does not end with a newline holds an incomplete record
// from a crash: open terminates it with a newline, so the next record starts
// on its own line, and returns its offset, or -1 when the file is complete.
func (a *auditLog) open() (int64, error) {
	info, err := os.Lstat(a.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return -1, err
	case !info.Mode().IsRegular():
		return -1, fmt.Errorf("%s is not a regular file", a.path)
	}

	incomplete, err := incompleteRecord(a.path)
	if err != nil {
		return -1, err
	}
	a.rotated, a.next, err = rotatedFiles(a.path)
	if err != nil {
		return -1, err
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return -1, err
	}
	a.file = file
	if info, err = file.Stat(); err != nil {
		return -1, err
	}
	a.size = info.Size()
	if incomplete >= 0 {
		if err := writeAudit(file, []byte("\n")); err != nil {
			return -1, err
		}
		a.size++
	}
	return incomplete, nil
}

// incompleteRecord returns the offset of an unterminated last record, or -1.
func incompleteRecord(path string) (int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return -1, err
	}
	start := max(info.Size()-maxRecordBytes, 0)
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil && err != io.EOF {
		return -1, err
	}
	if tail[len(tail)-1] == '\n' {
		return -1, nil
	}
	return start + int64(bytes.LastIndexByte(tail, '\n')+1), nil
}

// rotatedFiles counts the files named "<path>.<n>" and returns the next
// unused n.
func rotatedFiles(path string) (int, int, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return 0, 0, err
	}
	count, next := 0, 1
	prefix := filepath.Base(path) + "."
	for _, entry := range entries {
		suffix, found := strings.CutPrefix(entry.Name(), prefix)
		n, err := strconv.Atoi(suffix)
		if !found || err != nil || n < 1 {
			continue
		}
		count++
		next = max(next, n+1)
	}
	return count, next, nil
}

// rotate closes the active file, renames it to the next rotated name and
// opens a new active file. Every batch was synced when written, so closing
// loses nothing.
func (a *auditLog) rotate() (string, error) {
	if a.rotated >= a.maxFiles-1 {
		return "", fmt.Errorf("audit storage full: %d files of up to %d bytes exist; archive the rotated files and restart", a.maxFiles, a.maxBytes)
	}
	err := a.file.Close()
	a.file = nil
	if err != nil {
		return "", err
	}
	rotated := fmt.Sprintf("%s.%d", a.path, a.next)
	if err := os.Rename(a.path, rotated); err != nil {
		return "", err
	}
	crashPoint("audit-after-rename")
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	a.file, a.size = file, 0
	a.rotated++
	a.next++
	return filepath.Base(rotated), nil
}

func (a *auditLog) fail(err error) {
	if a.failed.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "gateway: audit log failed; protected admission disabled: %v\n", err)
	}
}

// Commit records an event and waits for durable acknowledgement. A failed or
// closing log, or a full queue, refuses it at once.
func (a *auditLog) Commit(ctx context.Context, e Event) error {
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
func (a *auditLog) Record(e Event) {
	if a.failed.Load() || a.enqueue(e, nil) != nil {
		a.lost.Add(1)
	}
}

// enqueue stamps and queues an event, unless the log is closing or its queue
// is full. Queueing never waits, so the read lock is held only briefly.
func (a *auditLog) enqueue(e Event, done chan error) error {
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

func (a *auditLog) stamp(e Event) Event {
	e.Schema = eventSchema
	e.Time = time.Now().UTC()
	e.Seq = a.seq.Add(1)
	e.ID = fmt.Sprintf("%s-%d", a.boot, e.Seq)
	if len(e.Path) > maxLoggedPath {
		e.Path = e.Path[:maxLoggedPath]
		e.Truncated = true
	}
	return e
}

// run is the writer. It writes the queued events in batches, in queue order,
// until the queue is closed and empty, which only close does; receiving from
// a closed channel yields every value still buffered first. It then writes
// the loss count still pending and closes the file.
func (a *auditLog) run() {
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

func (a *auditLog) write(batch []auditEntry) error {
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
			a.fail(err)
			return errAuditFailed
		}
		marker := appendEvent(nil, a.stamp(Event{Kind: "audit.rotated", File: rotated}))
		buffer = append(marker, buffer...)
	}
	if err := writeAudit(a.file, buffer); err != nil {
		a.lost.Add(lost)
		a.fail(err)
		return errAuditFailed
	}
	a.size += int64(len(buffer))
	return nil
}

func appendEvent(buffer []byte, e Event) []byte {
	line, _ := json.Marshal(e)
	return append(append(buffer, line...), '\n')
}

// close ends the log. From the moment it begins no event can be queued:
// Record counts it as lost and Commit refuses it. It then waits, until ctx is
// done, for the writer to write every event queued before, then the pending
// loss count, and to close the file. It returns nil only when all of that was
// written and synced; otherwise it reports what is not confirmed written. A
// writer blocked in the file system cannot be interrupted: when ctx ends
// first, the writer keeps the file and may still finish. Each later call
// reports the state it then finds.
func (a *auditLog) close(ctx context.Context) error {
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
			events, lost := a.unconfirmed()
			return fmt.Errorf("audit log not drained within the stop bound: %d accepted events and the count of %d refused events not confirmed written", events, lost)
		}
	}
	if events, lost := a.unconfirmed(); events > 0 || lost > 0 {
		return fmt.Errorf("audit log incomplete: %d accepted events and the count of %d refused events not written", events, lost)
	}
	return a.fileErr
}

// unconfirmed returns the number of accepted events not yet written and
// synced, and the count of refused events not yet in the file.
func (a *auditLog) unconfirmed() (events, lost uint64) {
	return a.accepted.Load() - a.written.Load(), a.lost.Load()
}
