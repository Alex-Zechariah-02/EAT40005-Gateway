// Per-request stage state.

package gateway

import (
	"net/http"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
)

// request tracks one request through the stages for its security event.
type request struct {
	event        audit.Event
	w            http.ResponseWriter
	audit        *audit.AuditLog
	observations *observations
	started      time.Time
	// bodyPending is true while a declared request body has not been
	// consumed by validation.
	bodyPending bool
}

func (q *request) pass(stage int) { q.event.Stages[stage] = audit.StagePass }

func (q *request) reject(stage int, status int, code string) {
	q.event.Kind, q.event.Stage, q.event.Decision = "request.rejected", audit.StageNames[stage], "reject"
	q.event.Stages[stage] = audit.StageReject
	q.event.Status, q.event.Error = status, code
	q.event.DurationMS = float64(time.Since(q.started)) / float64(time.Millisecond)
	q.audit.Record(q.event)
	q.observations.record(q.event)
	if q.bodyPending {
		// Closing the connection stops net/http from reading the unread body
		// before it writes the rejection, which a client that declares a body
		// and never sends it could otherwise hold open.
		q.w.Header().Set("Connection", "close")
	}
	writeError(q.w, status, code, q.event.RequestID)
}

// forwarded records the outcome of a request that was handed to the
// forwarder.
func (q *request) forwarded(status int, decision, code string) {
	q.event.Kind, q.event.Stage = "request.forwarded", audit.StageNames[audit.StageForward]
	q.event.Stages[audit.StageForward] = audit.StagePass
	if decision != "forwarded" {
		q.event.Stages[audit.StageForward] = audit.StageFail
	}
	q.event.Decision, q.event.Status, q.event.Error = decision, status, code
	q.event.DurationMS = float64(time.Since(q.started)) / float64(time.Millisecond)
	q.audit.Record(q.event)
	q.observations.record(q.event)
}
