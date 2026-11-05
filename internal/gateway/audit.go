// Audit event record and stage types.

package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	Schema        int       `json:"schema"`
	ID            string    `json:"event_id"`
	Time          time.Time `json:"time"`
	Seq           uint64    `json:"seq"`
	Kind          string    `json:"kind"`
	RequestID     string    `json:"request_id,omitempty"`
	Stage         string    `json:"stage,omitempty"` // terminal stage of a request event
	Stages        stages    `json:"stages,omitzero"`
	Decision      string    `json:"decision,omitempty"`
	Status        int       `json:"status,omitempty"`
	Error         string    `json:"error,omitempty"`
	Method        string    `json:"method,omitempty"`
	Path          string    `json:"path,omitempty"`
	Truncated     bool      `json:"path_truncated,omitempty"`
	Operation     string    `json:"operation,omitempty"`
	API           string    `json:"api,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	Roles         []string  `json:"roles,omitempty"`
	IgnoredRoles  []string  `json:"ignored_roles,omitempty"`
	ClientAddress string    `json:"client_address,omitempty"`
	DurationMS    float64   `json:"duration_ms,omitempty"`
	Actor         string    `json:"actor,omitempty"`
	Session       string    `json:"session,omitempty"`
	Generation    string    `json:"generation,omitempty"`
	Profile       string    `json:"profile,omitempty"`
	Upstream      string    `json:"upstream,omitempty"`
	Transition    string    `json:"transition,omitempty"`
	Revision      int64     `json:"revision,omitempty"`
	Candidate     int64     `json:"candidate,omitempty"`
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
	// Private replay context; neither the durable envelope nor the public Event
	// exposes this flag or retains the credential that led to it.
	identityKnown bool
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

func newRequestID() string {
	id := make([]byte, 16)
	rand.Read(id)
	return hex.EncodeToString(id)
}
