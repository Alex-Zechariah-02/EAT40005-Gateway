package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFetchStreamSessionLifetime(t *testing.T) {
	for _, logout := range []bool{true, false} {
		t.Run(fmt.Sprintf("logout_%t", logout), func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, true), seedOptions{})
			settings := e.nativeSettings()
			if !logout {
				section(settings, "gateway")["admin_session_lifetime"] = "1s"
			}
			e.writeNative(settings)
			e.start(binaries.plain)
			admin := e.mustLogin(harnessAdmin, harnessPassword)
			request, _ := http.NewRequest("GET", "http://"+e.admin+"/admin/events/stream", nil)
			header := admin.headers(e)
			for i := 0; i < len(header); i += 2 {
				request.Header.Set(header[i], header[i+1])
			}
			request.Header.Set("Accept", "text/event-stream")
			client := &http.Client{Timeout: 3 * time.Second}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("stream: %d", response.StatusCode)
			}
			if logout {
				if code, _, _ := e.as(admin, "DELETE", "/admin/session", ""); code != 204 {
					t.Fatalf("logout: %d", code)
				}
				e.get("/a/after-logout")
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("stream outlived session: %v (%s)", err, body)
			}
			if strings.Contains(string(body), "event: request") {
				t.Fatalf("request disclosed after logout: %s", body)
			}
		})
	}
}

func TestFetchImpactOfNewRole(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	token := issuer.token(t, changed(validClaims("alien-account"), map[string]any{"roles": []string{"ALIEN"}}))
	if code, _ := e.get("/a/new-role", "Authorization", "Bearer "+token); code != 403 {
		t.Fatalf("unknown role: %d", code)
	}
	p.Roles = append(p.Roles, Role{ID: "ALIEN"})
	p.Permits = append(p.Permits, Permit{Role: "ALIEN", Operation: "read-a"})
	code, staged, body := e.stage(admin, p, `"r1"`, "impact-new-role-candidate")
	if code != 201 {
		t.Fatalf("stage: %d %s", code, body)
	}
	code, body, _ = e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", staged.Candidate.ID), "")
	var impact struct {
		Flips []struct {
			Before, After string
			Count         int
			Roles         []string
		} `json:"flips"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &impact) != nil || len(impact.Flips) != 1 || impact.Flips[0].Before != "refused" || impact.Flips[0].After != "allowed" || impact.Flips[0].Count != 1 || !reflect.DeepEqual(impact.Flips[0].Roles, []string{"ALIEN"}) {
		t.Fatalf("new role impact: %d %s", code, body)
	}
	e.assertReceipts(0, 0, 0)
}

func TestFetchEventsAndTraffic(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	token := issuer.tokenFor(t, "alice")
	resp, _ := e.exchange("GET", "/a/first?password=never-record-this", "", "", "Authorization", "Bearer "+token)
	ref := resp.Header.Get("X-Request-Id")
	e.get("/a/second")
	e.get("/missing")
	code, body, _ := e.as(admin, "GET", "/admin/events?ref="+ref, "")
	var page struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"next"`
	}
	if code != 200 {
		t.Fatalf("events: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("lookup: %s (%v)", body, err)
	}
	event := page.Items[0]
	wantKeys := []string{"request_id", "time", "method", "path", "operation", "api", "subject", "roles", "client_address", "decision", "status", "stage", "code", "generation", "duration_ms"}
	if len(event) != len(wantKeys) {
		t.Errorf("event fields: %v", event)
	}
	for _, key := range wantKeys {
		if _, ok := event[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if event["path"] != "/a/first" || event["subject"] != "alice" || event["decision"] != "allowed" || event["stage"] != "forward" || event["client_address"] != "127.0.0.1" || event["code"] != nil {
		t.Errorf("event: %v", event)
	}
	if strings.Contains(body, token) || strings.Contains(body, "never-record-this") {
		t.Fatal("credentials or query leaked")
	}
	for query, count := range map[string]int{"subject=alice": 1, "operation=read-a": 2, "decision=refused": 2, "limit=1": 1, "subject=nobody": 0} {
		code, body, _ = e.as(admin, "GET", "/admin/events?"+query, "")
		if code != 200 || json.Unmarshal([]byte(body), &page) != nil || len(page.Items) != count {
			t.Errorf("%s: %d %s", query, code, body)
		}
	}
	code, body, _ = e.as(admin, "GET", "/admin/events?limit=1", "")
	json.Unmarshal([]byte(body), &page)
	if page.Next == "" {
		t.Fatal("no continuation cursor")
	}
	code, body, _ = e.as(admin, "GET", "/admin/events?limit=100&after="+page.Next, "")
	json.Unmarshal([]byte(body), &page)
	if code != 200 || len(page.Items) != 2 {
		t.Errorf("continuation: %d %s", code, body)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "decision=failed", "unknown=x", "after=bad", "bad%zz=x&limit=1"} {
		if code, body, _ := e.as(admin, "GET", "/admin/events?"+query, ""); code != 400 {
			t.Errorf("%s: %d %s", query, code, body)
		}
	}
	for _, window := range []string{"60", "300", "3600"} {
		code, body, _ = e.as(admin, "GET", "/admin/traffic?window="+window, "")
		var traffic struct {
			Total, Allowed, Refused int
			ByAPI                   []map[string]any `json:"by_api"`
			Reasons                 []map[string]any `json:"refusal_reasons"`
			Seconds                 []map[string]any `json:"per_second"`
		}
		if code != 200 || json.Unmarshal([]byte(body), &traffic) != nil || traffic.Total != 3 || traffic.Allowed != 1 || traffic.Refused != 2 || len(traffic.ByAPI) != 2 || len(traffic.Reasons) != 2 || len(traffic.Seconds) == 0 {
			t.Errorf("traffic: %d %s", code, body)
		}
		for _, group := range traffic.ByAPI {
			if _, ok := group["api"].(string); !ok {
				t.Errorf("traffic api is not a string: %v", group)
			}
		}
	}
	// The insights come from the same requests: the busiest endpoints (an
	// unrouted request is not one), who was refused, the window before and the
	// response time per chart interval.
	code, body, _ = e.as(admin, "GET", "/admin/traffic?window=60", "")
	var insights struct {
		ByOperation []struct {
			Operation, API          string
			Total, Allowed, Refused int
		} `json:"by_operation"`
		TopRefused []struct {
			Kind, ID   string
			Refused    int
			LatestCode string `json:"latest_code"`
		} `json:"top_refused"`
		Previous *struct{ Total, Allowed, Refused int } `json:"previous"`
		Latency  []struct {
			Count int     `json:"count"`
			P50   float64 `json:"p50_ms"`
			P95   float64 `json:"p95_ms"`
		} `json:"latency"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &insights) != nil {
		t.Fatalf("traffic insights: %d %s", code, body)
	}
	if len(insights.ByOperation) != 1 || insights.ByOperation[0].Operation != "read-a" || insights.ByOperation[0].Total != 2 || insights.ByOperation[0].Allowed != 1 || insights.ByOperation[0].Refused != 1 {
		t.Errorf("busiest endpoints: %s", body)
	}
	if len(insights.TopRefused) != 1 || insights.TopRefused[0].Kind != "address" || insights.TopRefused[0].ID != "127.0.0.1" || insights.TopRefused[0].Refused != 2 || insights.TopRefused[0].LatestCode != "not_found" {
		t.Errorf("most refused: %s", body)
	}
	if insights.Previous == nil || insights.Previous.Total != 0 {
		t.Errorf("previous window, fully retained and empty: %s", body)
	}
	counted := 0
	for _, entry := range insights.Latency {
		counted += entry.Count
		if entry.P50 > entry.P95 {
			t.Errorf("median above the 95th percentile: %s", body)
		}
	}
	if counted != 3 {
		t.Errorf("latency intervals count %d requests, want 3: %s", counted, body)
	}
	for _, query := range []string{"window=1", "window=60&window=300", "unknown=x", "bad%zz=x&window=60"} {
		if code, _, _ := e.as(admin, "GET", "/admin/traffic?"+query, ""); code != 400 {
			t.Errorf("traffic %s: %d", query, code)
		}
	}
	if code, _, _ := e.adminSend("GET", "/admin/events", ""); code != 401 {
		t.Errorf("unauthenticated events: %d", code)
	}
}

func TestFetchEventStreams(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, true), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	client := &http.Client{Timeout: 25 * time.Second}
	var streams []*http.Response
	t.Cleanup(func() {
		for _, response := range streams {
			response.Body.Close()
		}
	})
	for i := range 9 {
		req, _ := http.NewRequest("GET", "http://"+e.admin+"/admin/events/stream", nil)
		for j := 0; j < len(admin.headers(e)); j += 2 {
			h := admin.headers(e)
			req.Header.Set(h[j], h[j+1])
		}
		req.Header.Set("Accept", "text/event-stream")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, response)
		if i == 8 {
			if response.StatusCode != 503 {
				t.Errorf("ninth stream: %d", response.StatusCode)
			}
			continue
		}
		if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream %d: %d %v", i, response.StatusCode, response.Header)
		}
	}
	e.get("/a/stream-test")
	scanner := bufio.NewScanner(streams[0].Body)
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event["path"] != "/a/stream-test" {
				t.Fatalf("stream payload: %s", line)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no request event: %v", scanner.Err())
	}
	for _, response := range streams {
		response.Body.Close()
	}
	// An open stream must not hold graceful shutdown until its timeout.
	e.stop()
}

func TestFetchImpact(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	for range 3 {
		e.get("/a/impact", "Authorization", "Bearer "+issuer.tokenFor(t, "alice"))
	}
	p.Operations[0].Access = "deny"
	stageCode, candidate, stageBody := e.stage(admin, p, `"r1"`, "fetch-impact-candidate")
	if stageCode != 201 {
		t.Fatalf("stage: %d %s", stageCode, stageBody)
	}
	before := e.status()
	code, body, _ := e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", candidate.Candidate.ID), "")
	var impact struct {
		Window struct {
			Requests        int `json:"requests"`
			Previewed       int `json:"previewed"`
			IdentityUnknown int `json:"identity_unknown"`
		} `json:"window"`
		Flips []struct {
			Before, After, AfterCode string
			Count                    int
		} `json:"flips"`
		NotPreviewed []string `json:"not_previewed"`
	}
	if code != 200 {
		t.Fatalf("impact: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &impact); err != nil || impact.Window.Requests != 3 || impact.Window.Previewed != 3 || impact.Window.IdentityUnknown != 0 || len(impact.Flips) != 1 || impact.Flips[0].Before != "allowed" || impact.Flips[0].After != "refused" || impact.Flips[0].Count != 3 || !reflect.DeepEqual(impact.NotPreviewed, []string{"rate", "body"}) {
		t.Fatalf("impact: %s (%v)", body, err)
	}
	if after := e.status(); after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatal("preview changed authority")
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/admin/candidates/99999/impact", 404}, {"/admin/candidates/no/impact", 404}} {
		if code, _, _ := e.as(admin, "POST", tc.path, ""); code != tc.want {
			t.Errorf("%s: %d", tc.path, code)
		}
	}
}
