package gateway

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

var durationBuckets = [...]float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type metricLabels struct{ api, operation, decision, code string }
type durationMetric struct {
	count   uint64
	sum     float64
	buckets [len(durationBuckets)]uint64
}
type requestMetrics struct {
	counts    map[metricLabels]uint64
	durations map[string]*durationMetric
}

// Called under observations.mu. Only compiler-defined identifiers and gateway
// outcome codes reach labels; a hard cap also bounds policy churn over a run.
func (m *requestMetrics) record(event requestEvent) {
	if m.counts == nil {
		m.counts = map[metricLabels]uint64{}
		m.durations = map[string]*durationMetric{}
	}
	api, operation, code := "unknown", "unknown", ""
	if event.API != nil {
		api = *event.API
	}
	if event.Operation != nil {
		operation = *event.Operation
	}
	if event.Code != nil {
		code = *event.Code
	}
	labels := metricLabels{api, operation, event.Decision, code}
	if _, exists := m.counts[labels]; !exists && len(m.counts) >= 4096 {
		labels.api, labels.operation = "unknown", "unknown"
	}
	m.counts[labels]++
	if m.durations[api] == nil && len(m.durations) >= maxAPIs {
		api = "unknown"
	}
	if m.durations[api] == nil {
		m.durations[api] = new(durationMetric)
	}
	duration := m.durations[api]
	duration.count++
	seconds := event.DurationMS / 1000
	duration.sum += seconds
	for i, upper := range durationBuckets {
		if seconds <= upper {
			duration.buckets[i]++
		}
	}
}

func (c *Controller) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.RequestURI != "/metrics" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var body strings.Builder
	fmt.Fprintln(&body, "# TYPE gateway_requests_total counter")
	o := c.observations
	o.mu.Lock()
	labels := slices.Collect(maps.Keys(o.metrics.counts))
	slices.SortFunc(labels, func(a, b metricLabels) int {
		return strings.Compare(a.api+"\x00"+a.operation+"\x00"+a.decision+"\x00"+a.code, b.api+"\x00"+b.operation+"\x00"+b.decision+"\x00"+b.code)
	})
	for _, label := range labels {
		fmt.Fprintf(&body, "gateway_requests_total{api=%q,operation=%q,decision=%q,code=%q} %d\n", label.api, label.operation, label.decision, label.code, o.metrics.counts[label])
	}
	fmt.Fprintln(&body, "# TYPE gateway_request_duration_seconds histogram")
	for _, api := range slices.Sorted(maps.Keys(o.metrics.durations)) {
		duration := o.metrics.durations[api]
		for i, upper := range durationBuckets {
			fmt.Fprintf(&body, "gateway_request_duration_seconds_bucket{api=%q,le=%q} %d\n", api, strconv.FormatFloat(upper, 'f', -1, 64), duration.buckets[i])
		}
		fmt.Fprintf(&body, "gateway_request_duration_seconds_bucket{api=%q,le=\"+Inf\"} %d\n", api, duration.count)
		fmt.Fprintf(&body, "gateway_request_duration_seconds_sum{api=%q} %g\n", api, duration.sum)
		fmt.Fprintf(&body, "gateway_request_duration_seconds_count{api=%q} %d\n", api, duration.count)
	}
	dropped := o.dropped
	o.mu.Unlock()
	revision, overrides := int64(0), 0
	if generation := c.Current(); generation != nil {
		revision, overrides = generation.Revision, len(generation.Overrides)
	}
	blocked := 0
	if accounts := c.blocked.Load(); accounts != nil {
		blocked = len(*accounts)
	}
	for _, gauge := range []struct {
		name  string
		value any
	}{{"gateway_policy_revision", revision}, {"gateway_temporary_rules_active", overrides}, {"gateway_blocked_accounts", blocked}} {
		fmt.Fprintf(&body, "# TYPE %s gauge\n%s %v\n", gauge.name, gauge.name, gauge.value)
	}
	fmt.Fprintf(&body, "# TYPE gateway_event_stream_dropped_total counter\ngateway_event_stream_dropped_total %d\n", dropped)
	fmt.Fprint(w, body.String())
}
