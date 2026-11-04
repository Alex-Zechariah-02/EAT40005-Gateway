// Admin API event list and event stream handlers.

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
