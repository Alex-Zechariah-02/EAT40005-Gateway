// Stored override records and the store's own path check.

package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// A temporary override changes one control of one operation of the active
// base policy until it expires or is revoked. It never rewrites a stored
// revision: the effective policy is the active base with every active
// override applied, compiled into one generation.
//
// Only these changes exist. An exception grants a role an operation it is not
// permitted, or raises an operation's rate count; a containment removes a
// role's permit, lowers a rate count, or denies an operation outright.
// Authentication, the token profile, body and header validation, the audit
// record, routes and destinations cannot be overridden, so no override can
// weaken them.
const (
	overrideException   = "exception"
	overrideContainment = "containment"

	controlPermit    = "permit"
	controlRate      = "rate"
	controlOperation = "operation"

	// An override must expire within this many seconds of its creation. The
	// upper bound limits how long a temporary change can stay in force
	// without becoming a confirmed policy change.
	minOverrideSeconds = 1
	maxOverrideSeconds = 24 * 60 * 60

	// maxActiveOverrides bounds the work of compiling an effective policy.
	maxActiveOverrides = 64
)

// Override states. Only active overrides are applied; the others are final.
// Quarantined marks an override found active in a restored historical
// database, which never regains authority.
const (
	overrideActive      = "active"
	overrideRevoked     = "revoked"
	overrideExpired     = "expired"
	overrideQuarantined = "quarantined"
)

// override is one stored temporary override and its lifecycle.
type override struct {
	ID         int64      `json:"id"`
	Type       string     `json:"type"`
	Control    string     `json:"control"`
	Operation  string     `json:"operation"`
	Role       string     `json:"role,omitempty"`
	Count      int64      `json:"count,omitempty"`
	Target     string     `json:"target"`
	Base       int64      `json:"base_revision"` // active base when it was created
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Actor      string     `json:"actor"`
	Creation   string     `json:"creation"`
	Status     string     `json:"status"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
}

// dependencies lists the control targets an override relies on: its own, the
// operation it applies to and, for a permit, the role. A candidate that
// changes any of them would change or invalidate the override.
func (o override) dependencies() []string {
	targets := []string{o.Target, controlOperation + ":" + o.Operation}
	if o.Control == controlPermit {
		targets = append(targets, "role:"+o.Role)
	}
	return targets
}

// regularPath refuses devices, directories and links at a file boundary.
// Deployment directories must be controlled by the local operator; these
// checks do not defend against an actor who can replace their ancestors.
func regularPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a link, device or directory", path)
	}
	return nil
}
