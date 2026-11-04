// Per-request stage state and request IDs.

package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// request tracks one request through the stages for its security event.
type request struct {
	event        Event
	w            http.ResponseWriter
	audit        *auditLog
	observations *observations
	started      time.Time
	// bodyPending is true while a declared request body has not been
	// consumed by validation.
	bodyPending bool
}

func (q *request) pass(stage int) { q.event.Stages[stage] = stagePass }

func (q *request) reject(stage int, status int, code string) {
	q.event.Kind, q.event.Stage, q.event.Decision = "request.rejected", stageNames[stage], "reject"
	q.event.Stages[stage] = stageReject
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
	q.event.Kind, q.event.Stage = "request.forwarded", stageNames[stageForward]
	q.event.Stages[stageForward] = stagePass
	if decision != "forwarded" {
		q.event.Stages[stageForward] = stageFail
	}
	q.event.Decision, q.event.Status, q.event.Error = decision, status, code
	q.event.DurationMS = float64(time.Since(q.started)) / float64(time.Millisecond)
	q.audit.Record(q.event)
	q.observations.record(q.event)
}

func newRequestID() string {
	id := make([]byte, 16)
	rand.Read(id)
	return hex.EncodeToString(id)
}
