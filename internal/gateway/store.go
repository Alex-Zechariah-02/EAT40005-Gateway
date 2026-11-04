// Store modes, errors and stored record types.

package gateway

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Deployment modes. Only normal may serve traffic; the other two allow only
// the administrator flows that establish a new confirmed base.
const (
	modeBootstrap = "bootstrap_locked"
	modeNormal    = "normal"
	modeRecovery  = "recovery_required"
)

// errUnavailable marks state that must never be served: missing, empty,
// corrupt or unrecognised authority.
var errUnavailable = errors.New("authoritative state unavailable")

// Final dispositions of a provisional base. Confirmed keeps its policy as the
// new confirmed base; the others restore the rollback target. Quarantined
// marks a provisional base found in a restored historical database.
const (
	dispositionConfirmed   = "confirmed"
	dispositionRolledBack  = "rolled_back"
	dispositionTimedOut    = "timed_out"
	dispositionQuarantined = "quarantined"
)

// store is the single writer of control-plane state. The exclusive locking
// mode keeps a second gateway process, or an operator command, from opening
// the same database while it is in use.
type store struct {
	db *sql.DB
}

// authority is the committed state from which a generation is compiled.
type authority struct {
	mode         string
	installation string
	episode      string // recovery episode, when one was declared
	highWater    time.Time
	confirmed    int64 // 0 until a base has been established
	revision     int64 // active base: provisional if present, else confirmed
	provisional  int64
	candidate    int64 // the candidate the provisional base was activated from
	deadline     time.Time
	document     []byte     // active base document; read only in normal mode
	overrides    []override // active overrides in identifier order
}

// activationRecord is the durable lifecycle record of one provisional base:
// the candidate it came from, the confirmed revision it replaced and returns
// to on rollback, its deadline and its single final disposition.
type activationRecord struct {
	Revision       int64      `json:"revision"`
	Candidate      int64      `json:"candidate"`
	RollbackTarget int64      `json:"rollback_target"`
	Deadline       time.Time  `json:"deadline"`
	ActivatedAt    time.Time  `json:"activated_at"`
	Actor          string     `json:"actor"`
	Disposition    string     `json:"disposition,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy     string     `json:"resolved_by,omitempty"`
	Resolution     string     `json:"resolution,omitempty"`
}

// transitionRecord is the durable history entry and idempotency record of one
// mutation. Its ID is the mutation identity: a repeated ID returns this record
// instead of applying the mutation again.
type transitionRecord struct {
	ID        string
	Kind      string
	Actor     string
	Target    string
	Expected  sql.NullInt64 // revision the request required, if any
	Digest    string        // canonical request digest
	Outcome   string
	Reason    string
	Revision  int64 // revision the mutation produced, if any
	Candidate int64 // candidate the mutation produced, if any
	Audited   bool  // the outcome event was durably recorded
	// Override is the override the mutation concerns, for its receipt and
	// events. It is not a column: the override row names its transitions.
	Override int64
}

// revisionChange is a new active base and the state committed with it.
type revisionChange struct {
	parent    int64 // 0: no parent
	origin    string
	document  []byte
	actor     string
	deadline  time.Time // zero: the revision becomes the confirmed base; else provisional until then
	mode      string    // deployment mode after the commit
	candidate int64     // candidate this revision establishes, if any
}

// candidateRecord is an immutable staged policy and its validation result.
type candidateRecord struct {
	ID        int64     `json:"id"`
	Parent    int64     `json:"parent,omitempty"`
	Checksum  string    `json:"checksum"`
	DenyAll   bool      `json:"deny_all"`
	Status    string    `json:"status"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	Document  []byte    `json:"-"`
}

type administrator struct {
	Name        string
	Permissions permission
	Password    string // argon2id PHC string
	Status      string
	Credential  int64 // incremented by every credential change
}

type enrolment struct {
	Name        string
	Secret      string // SHA-256 of the enrolment secret
	Permissions permission
	ExpiresAt   time.Time
}

// revisionInfo describes a stored revision; Document is set only when one
// revision is read.
type revisionInfo struct {
	ID        int64           `json:"id"`
	Parent    int64           `json:"parent,omitempty"`
	Origin    string          `json:"origin"`
	Checksum  string          `json:"checksum"`
	Actor     string          `json:"actor"`
	CreatedAt time.Time       `json:"created_at"`
	Document  json.RawMessage `json:"document,omitempty"`
}

// historyEntry is one transition as administrators see it.
type historyEntry struct {
	Seq         int64      `json:"seq"`
	ID          string     `json:"transition_id"`
	Kind        string     `json:"kind"`
	Actor       string     `json:"actor"`
	Target      string     `json:"target"`
	Expected    *int64     `json:"expected_revision,omitempty"`
	Outcome     string     `json:"outcome"`
	Reason      string     `json:"reason,omitempty"`
	Revision    int64      `json:"revision,omitempty"`
	Candidate   int64      `json:"candidate,omitempty"`
	CommittedAt time.Time  `json:"recorded_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	PublishedBy string     `json:"published_by,omitempty"`
	AuditedAt   *time.Time `json:"audited_at,omitempty"`
}
