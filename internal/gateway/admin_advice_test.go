package gateway

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func adviceGeneration(t *testing.T) *Generation {
	t.Helper()
	g, err := effectiveGeneration(Policy{
		SchemaVersion: schemaVersion, Profile: profileCore,
		Upstreams: []Upstream{{ID: "A", Dial: "127.0.0.1:9"}},
		APIs:      []API{{ID: "a", Upstream: "A"}},
		Operations: []Operation{
			{ID: "open", API: "a", Method: "GET", Target: "/open", Access: "allow", Auth: "none"},
			{ID: "limited", API: "a", Method: "GET", Target: "/limited", Access: "allow", Auth: "none", Rate: &RateRule{Algorithm: "sliding-window", Count: 5, WindowSeconds: 60, Key: "client_address"}},
			{ID: "orphan", API: "a", Method: "GET", Target: "/orphan", Access: "allow", Auth: "required", Rate: &RateRule{Algorithm: "sliding-window", Count: 5, WindowSeconds: 60, Key: "subject"}},
			{ID: "read", API: "a", Method: "GET", Target: "/read", Access: "allow", Auth: "required"},
			{ID: "off", API: "a", Method: "GET", Target: "/off", Access: "deny", Auth: "none"},
		},
		Roles:   []Role{{ID: "READER"}, {ID: "IDLE"}},
		Permits: []Permit{{Role: "READER", Operation: "read"}},
	}, 1, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func ids(items []finding) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, item.ID+":"+item.Object)
	}
	return out
}

// The configuration warnings follow the fixed rules, in the fixed order, and
// a switched-off endpoint raises none.
func TestConfigurationWarnings(t *testing.T) {
	got := ids(configurationWarnings(adviceGeneration(t)))
	want := []string{"W1:open", "W2:orphan", "W3:IDLE", "W4:read"}
	if !slices.Equal(got, want) {
		t.Errorf("warnings %v, want %v", got, want)
	}
}

func observed(at time.Time, decision, operation string, roles ...string) observedRequest {
	return observedRequest{requestEvent: requestEvent{Time: at, Decision: decision, Operation: &operation, Roles: roles}}
}

// Unused permissions are withheld until enough traffic is observed, then
// list each permit no allowed request used.
func TestUnusedPermissions(t *testing.T) {
	g := adviceGeneration(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	few := []observedRequest{observed(now.Add(-2*time.Hour), "refused", "read", "READER")}
	if report := unusedPermissions(g, few, false, now); report.Withheld == nil || len(report.Items) != 0 {
		t.Errorf("too little traffic: %+v", report)
	}
	var events []observedRequest
	for n := range unusedMinRequests {
		events = append(events, observed(now.Add(-2*time.Hour+time.Duration(n)*time.Second), "refused", "read", "READER"))
	}
	if report := unusedPermissions(g, events, false, now); report.Withheld != nil || !slices.Equal(ids(report.Items), []string{"U1:READER → read"}) {
		t.Errorf("refusals do not use a permit: %+v", report)
	}
	events = append(events, observed(now.Add(-time.Minute), "allowed", "read", "READER"))
	if report := unusedPermissions(g, events, true, now); len(report.Items) != 0 || !report.Dropped || report.Requests != unusedMinRequests+1 {
		t.Errorf("an allowed request uses it: %+v", report)
	}
}

// A refusal spike needs enough refusals and a sharp rise over the period
// before; steady refusals are not a spike.
func TestRefusalSpike(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	burst := func(count int, decision string, from, span time.Duration) []observedRequest {
		var out []observedRequest
		for n := range count {
			out = append(out, observed(now.Add(-from+span*time.Duration(n)/time.Duration(count)), decision, "read"))
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		events []observedRequest
		spike  bool
	}{
		{"quiet before, a burst now", burst(25, "refused", 50*time.Second, 40*time.Second), true},
		{"below the minimum", burst(19, "refused", 50*time.Second, 40*time.Second), false},
		{"steady refusals", append(burst(300, "refused", 15*time.Minute, 14*time.Minute), burst(25, "refused", 50*time.Second, 40*time.Second)...), false},
		{"mostly allowed now", append(burst(25, "refused", 50*time.Second, 40*time.Second), burst(100, "allowed", 50*time.Second, 40*time.Second)...), false},
		{"rise over allowed traffic", append(append(burst(300, "allowed", 15*time.Minute, 14*time.Minute), burst(5, "refused", 10*time.Minute, time.Minute)...), burst(30, "refused", 50*time.Second, 40*time.Second)...), true},
	} {
		if got := refusalSpike(tc.events, now) != nil; got != tc.spike {
			t.Errorf("%s: spike %t, want %t", tc.name, got, tc.spike)
		}
	}
}

// The advice and alerts reads answer through the Admin API.
func TestAdminAdviceAndAlerts(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	code, body, _ := e.as(admin, "GET", "/admin/advice", "")
	var advice struct {
		Rules    string    `json:"rules"`
		Warnings []finding `json:"warnings"`
		Unused   struct {
			Withheld *string `json:"withheld"`
		} `json:"unused_permits"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &advice) != nil || advice.Rules != adviceRules || advice.Unused.Withheld == nil {
		t.Fatalf("advice: %d %s", code, body)
	}
	// The harness policy has signed-in endpoints without rate limits.
	if !slices.ContainsFunc(advice.Warnings, func(f finding) bool { return f.ID == "W4" }) {
		t.Errorf("warnings: %s", body)
	}
	if code, body, _ := e.as(admin, "GET", "/admin/advice?candidate=999", ""); code != 404 {
		t.Errorf("unknown draft: %d %s", code, body)
	}
	if code, body, _ := e.as(admin, "GET", "/admin/advice?candidate=x", ""); code != 400 {
		t.Errorf("bad draft id: %d %s", code, body)
	}
	for range 25 {
		e.get("/a/x")
	}
	code, body, _ = e.as(admin, "GET", "/admin/alerts", "")
	var alerts struct {
		Alerts []finding `json:"alerts"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &alerts) != nil || !slices.ContainsFunc(alerts.Alerts, func(f finding) bool { return f.ID == "A1" }) {
		t.Errorf("25 refusals with nothing before raise a spike: %d %s", code, body)
	}
}
