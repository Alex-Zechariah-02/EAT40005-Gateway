package gateway

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

func knownQuery(query url.Values, names ...string) bool {
	for name, values := range query {
		if len(values) != 1 || !slices.Contains(names, name) {
			return false
		}
	}
	return true
}

func (c *Controller) handleEvents(w http.ResponseWriter, r *http.Request, who caller) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	limit := 20
	if text := query.Get("limit"); text != "" {
		limit, err = strconv.Atoi(text)
		if strconv.Itoa(limit) != text {
			err = fmt.Errorf("invalid limit")
		}
	}
	ref, subject, operation, decision := query.Get("ref"), query.Get("subject"), query.Get("operation"), query.Get("decision")
	validDecision := decision == "" || decision == "allowed" || decision == "refused"
	if err != nil || !knownQuery(query, "ref", "subject", "operation", "decision", "limit", "after") || limit < 1 || limit > 100 || !validDecision {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	after := uint64(0)
	if text := query.Get("after"); text != "" {
		after, err = strconv.ParseUint(text, 10, 64)
		if err != nil || after == 0 || strconv.FormatUint(after, 10) != text {
			adminError(w, http.StatusBadRequest, "bad_request", "")
			return
		}
	}
	// One match beyond the page says whether a next page exists.
	matches := c.observations.recent(func(event *observedRequest) bool {
		return (after == 0 || event.sequence < after) &&
			(ref == "" || event.RequestID == ref) &&
			(subject == "" || (event.Subject != nil && *event.Subject == subject)) &&
			(operation == "" || (event.Operation != nil && *event.Operation == operation)) &&
			(decision == "" || event.Decision == decision)
	}, limit+1)
	items := []requestEvent{}
	next := ""
	for i, event := range matches {
		if i == limit {
			next = strconv.FormatUint(matches[limit-1].sequence, 10)
			break
		}
		items = append(items, event.requestEvent)
	}
	writeJSON(w, http.StatusOK, list{Items: items, Next: next})
}

func (c *Controller) handleEventStream(w http.ResponseWriter, r *http.Request, who caller) {
	if r.Header.Get("Accept") != "text/event-stream" || r.URL.RawQuery != "" {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	viewer := c.observations.subscribe()
	if viewer == nil {
		adminError(w, http.StatusServiceUnavailable, "stream_limit", "")
		return
	}
	defer c.observations.unsubscribe(viewer)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	write := func(data string) bool {
		if controller.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
			return false
		}
		if _, err := fmt.Fprint(w, data); err != nil {
			return false
		}
		if controller.Flush() != nil {
			return false
		}
		return controller.SetWriteDeadline(time.Time{}) == nil
	}
	if !write(": connected\n\n") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	expiry := time.NewTimer(time.Until(who.session.expires))
	defer expiry.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.observations.done:
			return
		case <-who.session.done:
			return
		case <-expiry.C:
			return
		case <-heartbeat.C:
			if !c.sessions.active(who.token, time.Now()) {
				return
			}
			if !write(": heartbeat\n\n") {
				return
			}
		case event := <-viewer.events:
			if !c.sessions.active(who.token, time.Now()) {
				return
			}
			if count := c.observations.takeDropped(viewer); count > 0 {
				if !write(fmt.Sprintf("event: dropped\ndata: {\"count\":%d}\n\n", count)) {
					return
				}
			}
			data, _ := json.Marshal(event.requestEvent)
			if !write("event: request\ndata: " + string(data) + "\n\n") {
				return
			}
		}
	}
}

// trafficGroup counts the requests of one API or one endpoint and keeps
// their durations for the percentiles.
type trafficGroup struct {
	API       string  `json:"api"`
	Operation string  `json:"operation,omitempty"`
	Total     int     `json:"total"`
	Allowed   int     `json:"allowed"`
	Refused   int     `json:"refused"`
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	durations []float64
}

func (g *trafficGroup) add(event requestEvent) {
	g.Total++
	g.durations = append(g.durations, event.DurationMS)
	if wasAllowed(event) {
		g.Allowed++
	} else {
		g.Refused++
	}
}

// percentiles returns the median and the 95th percentile of durations, which
// it sorts in place; both are zero when there are none.
func percentiles(durations []float64) (float64, float64) {
	n := len(durations)
	if n == 0 {
		return 0, 0
	}
	slices.Sort(durations)
	return durations[(n-1)/2], durations[(95*n+99)/100-1]
}

func (g *trafficGroup) settle() trafficGroup {
	g.P50, g.P95 = percentiles(g.durations)
	return *g
}

// trafficTop keeps the groups with the most requests, at most limit of them,
// ties broken by name so the order is stable between readings.
func trafficTop[T any](items []T, count func(T) int, name func(T) string, limit int) []T {
	slices.SortFunc(items, func(a, b T) int {
		if count(a) != count(b) {
			return count(b) - count(a)
		}
		return strings.Compare(name(a), name(b))
	})
	return items[:min(len(items), limit)]
}

const trafficTopLimit = 10

func (c *Controller) handleTraffic(w http.ResponseWriter, r *http.Request, who caller) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	window := 60
	if text := query.Get("window"); text != "" {
		window, err = strconv.Atoi(text)
		if strconv.Itoa(window) != text {
			err = fmt.Errorf("invalid window")
		}
	}
	if err != nil || !knownQuery(query, "window") || (window != 60 && window != 300 && window != 3600) {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	type reason struct {
		Code  string `json:"code"`
		Count int    `json:"count"`
	}
	type second struct {
		At      time.Time `json:"at"`
		Allowed int       `json:"allowed"`
		Refused int       `json:"refused"`
	}
	type refusedBy struct {
		Kind       string `json:"kind"`
		ID         string `json:"id"`
		Refused    int    `json:"refused"`
		LatestCode string `json:"latest_code"`
	}
	type totals struct {
		Total   int `json:"total"`
		Allowed int `json:"allowed"`
		Refused int `json:"refused"`
	}
	type interval struct {
		At        time.Time `json:"at"`
		Count     int       `json:"count"`
		P50       float64   `json:"p50_ms"`
		P95       float64   `json:"p95_ms"`
		durations []float64
	}
	now := time.Now().UTC()
	span := time.Duration(window) * time.Second
	start := now.Add(-span)
	previousStart := start.Add(-span)
	// Latency intervals match the chart's: window/60 seconds, on whole
	// multiples of their length on the clock.
	step := int64(window / 60)
	apis := map[string]*trafficGroup{}
	operations := map[string]*trafficGroup{}
	refusers := map[string]*refusedBy{}
	reasons := map[string]int{}
	seconds := map[int64]*second{}
	intervals := map[int64]*interval{}
	current, previous := totals{}, totals{}
	// The ring is newest first, so the latest refusal code of each account or
	// address is the first one seen.
	for _, entry := range c.observations.recent(func(event *observedRequest) bool {
		return !event.Time.Before(previousStart) && !event.Time.After(now)
	}, 0) {
		event := entry.requestEvent
		allowed := wasAllowed(event)
		if event.Time.Before(start) {
			previous.Total++
			if allowed {
				previous.Allowed++
			} else {
				previous.Refused++
			}
			continue
		}
		current.Total++
		api := "unknown"
		if event.API != nil {
			api = *event.API
		}
		if apis[api] == nil {
			apis[api] = &trafficGroup{API: api}
		}
		apis[api].add(event)
		if event.Operation != nil {
			if operations[*event.Operation] == nil {
				operations[*event.Operation] = &trafficGroup{API: api, Operation: *event.Operation}
			}
			operations[*event.Operation].add(event)
		}
		at := event.Time.Unix()
		if seconds[at] == nil {
			seconds[at] = &second{At: time.Unix(at, 0).UTC()}
		}
		bucket := at - at%step
		if intervals[bucket] == nil {
			intervals[bucket] = &interval{At: time.Unix(bucket, 0).UTC()}
		}
		intervals[bucket].Count++
		intervals[bucket].durations = append(intervals[bucket].durations, event.DurationMS)
		if allowed {
			current.Allowed++
			seconds[at].Allowed++
			continue
		}
		current.Refused++
		seconds[at].Refused++
		if event.Code != nil {
			reasons[*event.Code]++
		}
		kind, id := "address", event.ClientAddress
		if event.Subject != nil {
			kind, id = "account", *event.Subject
		}
		key := kind + " " + id
		if refusers[key] == nil {
			code := ""
			if event.Code != nil {
				code = *event.Code
			}
			refusers[key] = &refusedBy{Kind: kind, ID: id, LatestCode: code}
		}
		refusers[key].Refused++
	}
	byAPI := []trafficGroup{}
	for _, name := range slices.Sorted(maps.Keys(apis)) {
		byAPI = append(byAPI, apis[name].settle())
	}
	byOperation := []trafficGroup{}
	for _, group := range operations {
		byOperation = append(byOperation, group.settle())
	}
	byOperation = trafficTop(byOperation, func(g trafficGroup) int { return g.Total }, func(g trafficGroup) string { return g.Operation }, trafficTopLimit)
	topRefused := []refusedBy{}
	for _, entry := range refusers {
		topRefused = append(topRefused, *entry)
	}
	topRefused = trafficTop(topRefused, func(e refusedBy) int { return e.Refused }, func(e refusedBy) string { return e.Kind + " " + e.ID }, trafficTopLimit)
	refusalReasons := []reason{}
	for _, code := range slices.Sorted(maps.Keys(reasons)) {
		refusalReasons = append(refusalReasons, reason{code, reasons[code]})
	}
	perSecond := []second{}
	for _, at := range slices.Sorted(maps.Keys(seconds)) {
		perSecond = append(perSecond, *seconds[at])
	}
	latency := []interval{}
	for _, at := range slices.Sorted(maps.Keys(intervals)) {
		entry := intervals[at]
		entry.P50, entry.P95 = percentiles(entry.durations)
		latency = append(latency, *entry)
	}
	// A comparison needs the whole previous window: when the ring has dropped
	// requests from inside it, the count would be too low, so none is given.
	var earlier *totals
	if oldest, dropped := c.observations.retainedSince(); !dropped || !oldest.After(previousStart) {
		earlier = &previous
	}
	writeJSON(w, http.StatusOK, struct {
		Window      int            `json:"window_seconds"`
		Total       int            `json:"total"`
		Allowed     int            `json:"allowed"`
		Refused     int            `json:"refused"`
		ByAPI       []trafficGroup `json:"by_api"`
		Reasons     []reason       `json:"refusal_reasons"`
		Seconds     []second       `json:"per_second"`
		ByOperation []trafficGroup `json:"by_operation"`
		TopRefused  []refusedBy    `json:"top_refused"`
		Previous    *totals        `json:"previous"`
		Latency     []interval     `json:"latency"`
	}{window, current.Total, current.Allowed, current.Refused, byAPI, refusalReasons, perSecond, byOperation, topRefused, earlier, latency})
}

func (c *Controller) handleImpact(w http.ResponseWriter, r *http.Request, who caller) {
	if !noRequestBody(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	candidate, found, err := c.store.candidate(r.Context(), id)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	if !found {
		adminError(w, http.StatusNotFound, "not_found", "")
		return
	}
	policy, err := DecodePolicy(candidate.Document)
	if err != nil {
		adminError(w, http.StatusUnprocessableEntity, "candidate_invalid", "")
		return
	}
	// Capture authority and its active overrides together without settling a
	// deadline or publishing anything. Preview uses the same complete-policy
	// resolver as an activation, including independent temporary controls.
	c.mu.Lock()
	before := c.Current()
	if before == nil || before.expired(trustedNow()) {
		c.mu.Unlock()
		adminError(w, 503, "gateway_unavailable", "")
		return
	}
	authority, err := c.store.load(r.Context())
	c.mu.Unlock()
	if err != nil {
		adminError(w, 503, "gateway_unavailable", "")
		return
	}
	after, err := effectiveGeneration(policy, candidate.Parent, time.Time{}, authority.overrides)
	if err != nil {
		adminError(w, http.StatusUnprocessableEntity, "candidate_invalid", "")
		return
	}
	type flip struct {
		Operation string   `json:"operation"`
		Subject   *string  `json:"subject"`
		Roles     []string `json:"roles"`
		Before    string   `json:"before"`
		After     string   `json:"after"`
		AfterCode *string  `json:"after_code"`
		Count     int      `json:"count"`
	}
	events := c.observations.snapshot()
	groups := map[string]*flip{}
	unknown := 0
	for _, event := range events {
		old, _, oldOperation, oldKnown := before.previewDecision(event)
		current, code, operation, currentKnown := after.previewDecision(event)
		if !oldKnown || !currentKnown {
			unknown++
			continue
		}
		if old == current {
			continue
		}
		if operation == "" {
			operation = oldOperation
		}
		value := flip{Operation: operation, Subject: event.Subject, Roles: event.assertedRoles, Before: old, After: current, AfterCode: optionalString(code)}
		key, _ := json.Marshal(value)
		if groups[string(key)] == nil {
			groups[string(key)] = &value
		}
		groups[string(key)].Count++
	}
	flips := []flip{}
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		flips = append(flips, *groups[key])
	}
	now := time.Now().UTC()
	from := now
	if len(events) > 0 {
		from = events[len(events)-1].Time
	}
	type window struct {
		From            time.Time `json:"from"`
		To              time.Time `json:"to"`
		Requests        int       `json:"requests"`
		Previewed       int       `json:"previewed"`
		IdentityUnknown int       `json:"identity_unknown"`
	}
	notPreviewed := []string{"rate", "body"}
	if unknown > 0 {
		notPreviewed = append(notPreviewed, "unobserved_identity")
	}
	writeJSON(w, http.StatusOK, struct {
		Window       window   `json:"window"`
		Flips        []flip   `json:"flips"`
		NotPreviewed []string `json:"not_previewed"`
	}{window{from, now, len(events), len(events) - unknown, unknown}, flips, notPreviewed})
}
