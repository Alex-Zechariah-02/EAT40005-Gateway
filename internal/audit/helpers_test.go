// Helpers shared by several test files of the package.

package audit

import (
	"testing"
	"time"
)

// waitFor polls cond until it holds or the limit passes.
func waitFor(t *testing.T, limit time.Time, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if time.Now().After(limit) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
