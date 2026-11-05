package gateway

import (
	"net/http"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// handleLimits reports each built-in limit with its current use, so an
// administrator can see one coming before it is reached. Every figure is read
// from the component that owns it; nothing here changes a limit.
func (c *Controller) handleLimits(w http.ResponseWriter, r *http.Request, who caller) {
	active := 0
	if g := c.current.Load(); g != nil {
		active = len(g.Overrides)
	}
	limit := func(used, max int64) map[string]int64 { return map[string]int64{"used": used, "max": max} }
	writeJSON(w, http.StatusOK, map[string]any{
		"temporary_rules":        limit(int64(active), store.MaxActiveOverrides),
		"live_views":             limit(int64(c.observations.viewerCount()), maxEventStreams),
		"administrator_sessions": limit(int64(c.sessions.count()), maxSessions),
		"audit_files":            limit(c.audit.files.Load(), int64(c.audit.maxFiles)),
		"audit_active_bytes":     limit(c.audit.activeBytes.Load(), c.audit.maxBytes),
		"rate_counters":          limit(int64(c.limiter.size()), int64(c.limiter.capacity)),
		"recent_requests":        limit(int64(c.observations.retained()), recentEventLimit),
	})
}

func (o *observations) viewerCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.viewers)
}

func (o *observations) retained() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.count
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byHash)
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.counters)
}
