// The writer's bounded queue.

package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The writer's queue is bounded: with the writer unable to keep up, an
// admission that cannot be queued is refused at once, and one that is queued
// but never written times out; neither is acknowledged.
func TestAuditQueueIsBounded(t *testing.T) {
	a := &AuditLog{boot: "test", queue: make(chan auditEntry, 1), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Commit(ctx, Event{Kind: "request.admitted"}); !errors.Is(err, errAuditFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("queued but unwritten: %v", err)
	}
	if err := a.Commit(context.Background(), Event{Kind: "request.admitted"}); !errors.Is(err, errAuditFailed) || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("queue full: %v", err)
	}
	a.Record(Event{Kind: "request.rejected"})
	if a.lost.Load() != 1 {
		t.Fatalf("lost %d, want 1", a.lost.Load())
	}
}
