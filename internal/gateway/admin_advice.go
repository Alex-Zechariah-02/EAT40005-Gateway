package gateway

import (
	"cmp"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// The fixed checks, rule set version "checks-0.2": static
// configuration warnings, unused permissions from observed traffic, and
// alerts. They advise only: nothing here blocks, changes or activates
// anything, and nothing runs on the request path. Each answer names the
// rule set version so findings can be traced to the rules that made them.
const (
	adviceRules = "checks-0.2"

	// Unused permissions are withheld until the request records read from
	// the security log span this long and number this many.
	unusedMinSpan     = 60 * time.Minute
	unusedMinRequests = 500

	// A refusal spike: at least spikeMinRefusals in the last spikeWindow and
	// a refused share at least spikeFactor times that of the spikeBaseline
	// before it, or at least spikeLowTrafficShare when that period had fewer
	// than spikeMinRefusals requests.
	spikeWindow          = time.Minute
	spikeBaseline        = 15 * time.Minute
	spikeMinRefusals     = 20
	spikeFactor          = 3.0
	spikeLowTrafficShare = 0.5

	// nearlyFull is the share of a store that raises an alert.
	nearlyFull = 0.9
)

type finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Object   string `json:"object"`
	Message  string `json:"message"`
}

var severityOrder = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

// sortFindings gives the fixed order: severity, rule, object.
func sortFindings(items []finding) {
	slices.SortFunc(items, func(a, b finding) int {
		return cmp.Or(cmp.Compare(severityOrder[a.Severity], severityOrder[b.Severity]), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Object, b.Object))
	})
}

// rules lists every compiled endpoint rule of a generation once.
func (g *Generation) rules() []*rule {
	var list []*rule
	add := func(t *target) {
		for _, r := range t.methods {
			list = append(list, r)
		}
	}
	for _, t := range g.exact {
		add(t)
	}
	for _, t := range g.prefixes {
		add(t)
	}
	slices.SortFunc(list, func(a, b *rule) int { return cmp.Compare(a.operation, b.operation) })
	return list
}

// configurationWarnings reads the compiled generation only.
func configurationWarnings(g *Generation) []finding {
	items := []finding{}
	usedRoles := map[string]bool{}
	for _, r := range g.rules() {
		for role := range r.roles {
			usedRoles[role] = true
		}
		if !r.permitted {
			continue
		}
		switch {
		case r.public && r.rate == nil:
			items = append(items, finding{"W1", "high", r.operation, "A public endpoint has no rate limit, so anyone can call it without limit."})
		case !r.public && len(r.roles) == 0:
			items = append(items, finding{"W2", "medium", r.operation, "The endpoint is switched on and needs sign-in, but no role has permission, so no one can call it."})
		case !r.public && r.rate == nil:
			items = append(items, finding{"W4", "low", r.operation, "A signed-in endpoint has no rate limit."})
		}
	}
	for role := range g.roles {
		if !usedRoles[role] {
			items = append(items, finding{"W3", "low", role, "The role has no permissions."})
		}
	}
	sortFindings(items)
	return items
}

type unusedReport struct {
	Requests      int       `json:"requests"`
	Since         *string   `json:"since"`
	Complete      bool      `json:"complete"`
	FilesSearched []string  `json:"files_searched"`
	FilesMissing  []string  `json:"files_missing"`
	Withheld      *string   `json:"withheld"`
	Items         []finding `json:"items"`
	MinMinute     int       `json:"min_minutes"`
	MinCount      int       `json:"min_requests"`
}

// unusedPermissions lists permits no allowed request in the security log
// used, once the records read cover enough time and requests. missing names
// log files moved away, which were not read.
func unusedPermissions(g *Generation, use logUse, missing []string, now time.Time) unusedReport {
	report := unusedReport{
		Requests: use.requests, Complete: use.complete, FilesSearched: use.searched, FilesMissing: missing, Items: []finding{},
		MinMinute: int(unusedMinSpan / time.Minute), MinCount: unusedMinRequests,
	}
	if report.FilesSearched == nil {
		report.FilesSearched = []string{}
	}
	if report.FilesMissing == nil {
		report.FilesMissing = []string{}
	}
	if use.requests > 0 {
		since := use.oldest.UTC().Format(time.RFC3339)
		report.Since = &since
	}
	if use.requests < unusedMinRequests || now.Sub(use.oldest) < unusedMinSpan {
		reason := "Not enough traffic has been recorded yet."
		report.Withheld = &reason
		return report
	}
	for _, r := range g.rules() {
		if r.public || !r.permitted {
			continue
		}
		for role := range r.roles {
			if !use.used[[2]string{role, r.operation}] {
				report.Items = append(report.Items, finding{"U1", "low", role + " → " + r.operation, "No allowed request in the security log used this permission. Review whether the role still needs it."})
			}
		}
	}
	sortFindings(report.Items)
	return report
}

// handleAdvice answers the configuration warnings and unused permissions for
// the generation in force, or for a stored draft compiled with the active
// overrides.
func (c *Controller) handleAdvice(w http.ResponseWriter, r *http.Request, who caller) {
	g := c.Current()
	var candidate *int64
	if text := r.URL.Query().Get("candidate"); text != "" {
		id, err := strconv.ParseInt(text, 10, 64)
		if err != nil || id < 1 {
			adminError(w, http.StatusBadRequest, "bad_request", "candidate")
			return
		}
		var ok bool
		if _, g, ok = c.candidateGeneration(w, r, id); !ok {
			return
		}
		candidate = &id
	} else if g == nil || g.expired(trustedNow()) {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	files, missing, err := securityLogFiles(c.audit.path)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	use, err := permitUse(filepath.Dir(c.audit.path), files, securityLogExportScan)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rules":          adviceRules,
		"generation":     g.ID,
		"candidate":      candidate,
		"warnings":       configurationWarnings(g),
		"unused_permits": unusedPermissions(g, use, missing, time.Now()),
	})
}

// refusalSpike applies the spike rule to the retained observations.
func refusalSpike(events []observedRequest, now time.Time) *finding {
	var recent, recentRefused, before, beforeRefused int
	for _, event := range events {
		age := now.Sub(event.Time)
		refused := event.Decision == "refused"
		switch {
		case age < 0:
		case age <= spikeWindow:
			recent++
			if refused {
				recentRefused++
			}
		case age <= spikeWindow+spikeBaseline:
			before++
			if refused {
				beforeRefused++
			}
		}
	}
	if recentRefused < spikeMinRefusals {
		return nil
	}
	share := float64(recentRefused) / float64(recent)
	spiking := share >= spikeLowTrafficShare
	if before >= spikeMinRefusals {
		spiking = share >= spikeFactor*float64(beforeRefused)/float64(before)
	}
	if !spiking {
		return nil
	}
	return &finding{"A1", "high", "refusals", strconv.Itoa(recentRefused) + " of " + strconv.Itoa(recent) + " requests were refused in the last minute, far more than before. Look up the refused requests to see who is sending them."}
}

// handleAlerts evaluates the alert rules when read; nothing is evaluated per
// request.
func (c *Controller) handleAlerts(w http.ResponseWriter, r *http.Request, who caller) {
	now := time.Now()
	items := []finding{}
	if spike := refusalSpike(c.observations.snapshot(), now); spike != nil {
		items = append(items, *spike)
	}
	if c.audit.failed.Load() {
		items = append(items, finding{"A2", "critical", "security_log", "The security log cannot be written, so the gateway refuses requests it cannot record."})
	}
	if c.audit.files.Load() >= int64(c.audit.maxFiles) && float64(c.audit.activeBytes.Load()) >= nearlyFull*float64(c.audit.maxBytes) {
		items = append(items, finding{"A3", "high", "security_log", "The security log is nearly full. Move earlier log files to an archive before it fills, or the gateway will stop accepting requests."})
	}
	if float64(c.limiter.size()) >= nearlyFull*float64(c.limiter.capacity) {
		items = append(items, finding{"A4", "medium", "rate_counters", "Rate limit counters are nearly full. When they run out, requests that need a new counter are refused."})
	}
	sortFindings(items)
	writeJSON(w, http.StatusOK, map[string]any{"rules": adviceRules, "evaluated_at": now.UTC(), "alerts": items})
}
