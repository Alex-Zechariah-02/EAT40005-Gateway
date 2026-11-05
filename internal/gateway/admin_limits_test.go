package gateway

import (
	"encoding/json"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
)

// The limits read reports each built-in limit with its current use, taken
// from the component that owns it, and needs a signed-in administrator.
func TestAdminLimits(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	if code, _, _ := e.adminSend("GET", "/admin/limits", ""); code != 401 {
		t.Fatalf("limits without a session: %d", code)
	}
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	e.get("/a/first")
	e.get("/missing")
	code, body, _ := e.as(admin, "GET", "/admin/limits", "")
	if code != 200 {
		t.Fatalf("limits: %d %s", code, body)
	}
	var limits map[string]struct {
		Used int64 `json:"used"`
		Max  int64 `json:"max"`
	}
	if err := json.Unmarshal([]byte(body), &limits); err != nil || len(limits) != 7 {
		t.Fatalf("limits body: %s (%v)", body, err)
	}
	want := map[string]int64{
		"temporary_rules": maxActiveOverrides, "live_views": maxEventStreams, "administrator_sessions": maxSessions,
		"rate_counters": compile.MaxCounters, "recent_requests": recentEventLimit,
	}
	for name, max := range want {
		if limits[name].Max != max {
			t.Errorf("%s max = %d, want %d", name, limits[name].Max, max)
		}
	}
	if got := limits["administrator_sessions"].Used; got < 1 {
		t.Errorf("sessions used = %d, want the signed-in administrator", got)
	}
	if got := limits["recent_requests"].Used; got < 2 {
		t.Errorf("recent requests used = %d, want the two requests sent", got)
	}
	if got := limits["temporary_rules"].Used; got != 0 {
		t.Errorf("temporary rules used = %d, want 0", got)
	}
	if files, bytes := limits["audit_files"], limits["audit_active_bytes"]; files.Used < 1 || files.Used > files.Max || bytes.Used < 1 || bytes.Used > bytes.Max {
		t.Errorf("audit usage: files %+v, active bytes %+v", files, bytes)
	}
}
