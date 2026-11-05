// Store modes, errors and stored record types.

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Deployment modes. Only normal may serve traffic; the other two allow only
// the administrator flows that establish a new confirmed base.
const (
	ModeBootstrap = "bootstrap_locked"
	ModeNormal    = "normal"
	ModeRecovery  = "recovery_required"
)

// ErrUnavailable marks state that must never be served: missing, empty,
// corrupt or unrecognised authority.
var ErrUnavailable = errors.New("authoritative state unavailable")

// Final dispositions of a provisional base. Confirmed keeps its policy as the
// new confirmed base; the others restore the rollback target. Quarantined
// marks a provisional base found in a restored historical database.
const (
	DispositionConfirmed   = "confirmed"
	DispositionRolledBack  = "rolled_back"
	DispositionTimedOut    = "timed_out"
	DispositionQuarantined = "quarantined"
)

// DB is the single writer of control-plane state. The exclusive locking
// mode keeps a second gateway process, or an operator command, from opening
// the same database while it is in use.
type DB struct {
	db *sql.DB
}

// Authority is the committed state from which a generation is compiled.
type Authority struct {
	Mode         string
	Installation string
	Episode      string // recovery episode, when one was declared
	HighWater    time.Time
	Confirmed    int64 // 0 until a base has been established
	Revision     int64 // active base: provisional if present, else confirmed
	Provisional  int64
	Candidate    int64 // the candidate the provisional base was activated from
	Deadline     time.Time
	Document     []byte     // active base document; read only in normal mode
	Overrides    []Override // active overrides in identifier order
}

// ActivationRecord is the durable lifecycle record of one provisional base:
// the candidate it came from, the confirmed revision it replaced and returns
// to on rollback, its deadline and its single final disposition.
type ActivationRecord struct {
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

// TransitionRecord is the durable history entry and idempotency record of one
// mutation. Its ID is the mutation identity: a repeated ID returns this record
// instead of applying the mutation again.
type TransitionRecord struct {
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

// RevisionChange is a new active base and the state committed with it.
type RevisionChange struct {
	Parent    int64 // 0: no parent
	Origin    string
	Document  []byte
	Actor     string
	Deadline  time.Time // zero: the revision becomes the confirmed base; else provisional until then
	Mode      string    // deployment mode after the commit
	Candidate int64     // candidate this revision establishes, if any
}

// CandidateRecord is an immutable staged policy and its validation result.
type CandidateRecord struct {
	ID        int64     `json:"id"`
	Parent    int64     `json:"parent,omitempty"`
	Checksum  string    `json:"checksum"`
	DenyAll   bool      `json:"deny_all"`
	Status    string    `json:"status"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	Document  []byte    `json:"-"`
}

type Administrator struct {
	Name        string
	Permissions Permission
	Password    string // argon2id PHC string
	Status      string
	Credential  int64 // incremented by every credential change
}

type Enrolment struct {
	Name        string
	Secret      string // SHA-256 of the enrolment secret
	Permissions Permission
	ExpiresAt   time.Time
}

// RevisionInfo describes a stored revision; Document is set only when one
// revision is read.
type RevisionInfo struct {
	ID        int64           `json:"id"`
	Parent    int64           `json:"parent,omitempty"`
	Origin    string          `json:"origin"`
	Checksum  string          `json:"checksum"`
	Actor     string          `json:"actor"`
	CreatedAt time.Time       `json:"created_at"`
	Document  json.RawMessage `json:"document,omitempty"`
}

// HistoryEntry is one transition as administrators see it.
type HistoryEntry struct {
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
