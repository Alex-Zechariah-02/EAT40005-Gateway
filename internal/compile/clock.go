// Trusted time and generation expiry.

package compile

import (
	"sync/atomic"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

// Expired reports whether the generation's earliest deadline has passed at
// now, a reading of trustedNow.
func (g *Generation) Expired(now time.Time) bool {
	return !g.NotAfter.IsZero() && !now.Before(g.NotAfter)
}

// ProcessStart anchors this process's monotonic clock to the wall clock once,
// at startup. It is never changed.
var ProcessStart = time.Now()

// wallLead is the furthest, in nanoseconds, that the wall clock has been ahead
// of the monotonic estimate when a deadline was set in this process. It only
// grows.
var wallLead atomic.Int64

// TrustedNow is the time against which every persisted deadline is decided
// and every controller timestamp is taken; see trustedTime.
func TrustedNow() time.Time {
	return TrustedTime(time.Now().Round(0).Add(fault.WallOffset()), time.Since(ProcessStart))
}

// DeadlineNow is trustedNow for setting a deadline: a provisional base's or an
// override's. It first keeps any lead the wall clock has over the monotonic
// estimate, so a deadline set from a wall clock that is ahead is still reached
// after its own period if that clock is later set back. The cost is that, once
// the lead is kept, later timestamps stay that far ahead until a restart, and
// that restart waits for real time to pass them (the high-water check); the
// deadline's own record already carries the lead. Other readings, including
// every request, never raise it, so a brief forward step with no new deadline
// leaves no trace.
func DeadlineNow() time.Time {
	wall := time.Now().Round(0).Add(fault.WallOffset())
	elapsed := time.Since(ProcessStart)
	lead := int64(wall.Sub(ProcessStart.Round(0).Add(elapsed)))
	for {
		seen := wallLead.Load()
		if lead <= seen || wallLead.CompareAndSwap(seen, lead) {
			break
		}
	}
	return TrustedTime(wall, elapsed)
}

// TrustedTime returns the later of the wall clock reading wall and the wall
// time the monotonic clock implies after elapsed since processStart, advanced
// by the wall clock's largest lead when a deadline was set. A deadline has
// passed once either clock reaches it: a wall clock set back cannot extend it,
// and neither can a monotonic clock that stood still across a system suspend.
// A wall clock set forward ends deadlines early, which is the safe direction.
// The monotonic estimate is anchored at process start, not at each
// publication, so republishing after a wall clock change cannot move a
// deadline either.
func TrustedTime(wall time.Time, elapsed time.Duration) time.Time {
	estimate := ProcessStart.Round(0).Add(elapsed + time.Duration(wallLead.Load()))
	if wall.After(estimate) {
		return wall
	}
	return estimate
}
