// Rate counters across concurrency, publication and restart.

package gateway

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Simultaneous requests near the quota never admit more than the rule
// allows, for one subject and for many at once.
func TestConcurrentRequestsThroughGateway(t *testing.T) {
	e, _ := newRateEnv(t, func(e *env) policy.Policy {
		p := e.ratePolicy(rate(10, 60), nil)
		return p
	}, base)

	count := func(subjects []string, perSubject int) map[string][2]int {
		var mu sync.Mutex
		results := map[string][2]int{}
		var wg sync.WaitGroup
		for _, subject := range subjects {
			token := issuer.token(t, changed(validClaims(subject), map[string]any{"roles": []string{"READER"}}))
			for range perSubject {
				wg.Go(func() {
					req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", http.NoBody)
					req.Header.Set("Authorization", "Bearer "+token)
					resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
					if err != nil {
						t.Error(err)
						return
					}
					resp.Body.Close()
					mu.Lock()
					defer mu.Unlock()
					r := results[subject]
					switch resp.StatusCode {
					case http.StatusOK:
						r[0]++
					case http.StatusTooManyRequests:
						r[1]++
					default:
						t.Errorf("status %d", resp.StatusCode)
					}
					results[subject] = r
				})
			}
		}
		wg.Wait()
		return results
	}

	if got := count([]string{"alice"}, 40)["alice"]; got != [2]int{10, 30} {
		t.Fatalf("alice, 40 simultaneous requests: admitted/refused %v, want [10 30]", got)
	}
	e.assertReceipts(10, 0, 0)

	var load []string
	for i := range 8 {
		load = append(load, fmt.Sprintf("load-%d", i))
	}
	for subject, got := range count(load, 15) {
		if got != [2]int{10, 5} {
			t.Errorf("%s: admitted/refused %v, want [10 5]", subject, got)
		}
	}
	e.assertReceipts(10+80, 0, 0)
}

// Publication keeps counters: an equivalent republication, a count change, a
// window change, disabling and re-enabling a rule, and representative
// rollback and temporary-override publications never grant a fresh quota.
// Each request uses the rule of the generation it captured.
func TestRateCountersAcrossPublication(t *testing.T) {
	e, clock := newRateEnv(t, func(e *env) policy.Policy { return e.ratePolicy(rate(3, 60), nil) }, base)
	revision := int64(1)
	publish := func(id string, read *policy.RateRule) {
		t.Helper()
		outcome, code := e.publish(Change{ID: id, Expected: revision, Policy: mustJSON(e.ratePolicy(read, nil))})
		if code != http.StatusOK {
			t.Fatalf("publish %s: %d %+v", id, code, outcome)
		}
		revision = outcome.Revision
	}
	expect := func(step string, n int, want ...int) {
		t.Helper()
		if got := e.sendAs("alice", http.MethodGet, "/a/x", "", n); !statusesAre(got, want...) {
			t.Fatalf("%s: %v, want %v", step, got, want)
		}
	}

	expect("first generation", 4, 200, 200, 200, 429)
	publish("same-rule", rate(3, 60))
	expect("equivalent republication", 1, 429)
	publish("count-5", rate(5, 60))
	expect("count raised to 5", 3, 200, 200, 429)
	publish("count-2", rate(2, 60))
	expect("count lowered to 2", 1, 429)
	publish("window-120", rate(5, 120))
	expect("window changed, usage 5 carried", 1, 429)
	publish("disabled", nil)
	expect("rule removed", 2, 200, 200)
	publish("enabled", rate(5, 120))
	expect("rule restored, earlier usage kept", 1, 429)
	// Representative temporary override: raise the quota, then publish the
	// previous rule back, as an expiry or rollback would.
	publish("override", rate(7, 120))
	expect("override", 3, 200, 200, 429)
	publish("override-ended", rate(5, 120))
	expect("override ended", 1, 429)
	// Representative rollback to the first document.
	publish("rollback", rate(3, 60))
	expect("rolled back to the first rule", 1, 429)
	// Real time passing is the only thing that restores quota.
	clock.set(base + 3*60000)
	expect("two windows later", 4, 200, 200, 200, 429)
	e.assertReceipts(3+2+2+2+3, 0, 0)
}

// A request paused after capture keeps the rate rule of its generation even
// when a generation with another rule is published meanwhile.
func TestInFlightRequestKeepsCapturedRateRule(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.ratePolicy(rate(1, 60), nil), seedOptions{})
	clock := newControlledClock(t, base)
	dir := t.TempDir()
	holdFile, release := filepath.Join(dir, "captured"), filepath.Join(dir, "release")
	e.start(binaries.fault, clock.env(), "GATEWAY_HOLD_PATH=/a/held", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)

	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 200) {
		t.Fatalf("first request: %v", got)
	}
	result := make(chan int, 1)
	go func() {
		code, _ := e.request(http.MethodGet, "/a/held", "", "", bearer(t, "alice")...)
		result <- code
	}()
	waitForFile(t, holdFile)
	if outcome, code := e.publish(Change{ID: "count-10", Expected: 1, Policy: mustJSON(e.ratePolicy(rate(10, 60), nil))}); code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	os.WriteFile(release, nil, 0o600)
	if code := <-result; code != http.StatusTooManyRequests {
		t.Fatalf("held request under the count-1 generation: %d, want 429", code)
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 200) {
		t.Fatalf("new request under the count-10 generation: %v", got)
	}
	e.assertReceipts(2, 0, 0)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// Counters live in the gateway process: a restart starts them empty, and no
// endpoint on either listener resets them.
func TestRateCountersAndRestart(t *testing.T) {
	e, _ := newRateEnv(t, func(e *env) policy.Policy { return e.ratePolicy(rate(2, 60), nil) }, base)
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("before reset attempts: %v", got)
	}
	for _, path := range []string{"/reset", "/rate/reset", "/counters"} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			if code, _ := e.adminRaw(method, path, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
				t.Errorf("controller %s %s: %d", method, path, code)
			}
			if code, _ := e.request(method, path, "", ""); code != http.StatusNotFound {
				t.Errorf("data plane %s %s: %d", method, path, code)
			}
		}
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("after reset attempts: %v", got)
	}

	clock := newControlledClock(t, base)
	e.start(binaries.fault, clock.env())
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("after restart: %v", got)
	}
	e.assertReceipts(4, 0, 0)
}
