// Role changes, swaps, refusals and crashes.

package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A role removal published while a request is in flight does not change that
// request: it completes under the generation it captured, with that
// generation's roles. Every later request uses the new generation. The held
// request is paused right after capture, before authentication and
// authorisation run, so both run after the new generation is published.
func TestRoleChangeAppliesToNewRequestsOnly(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	first := e.status().Generation
	aliceToken, bobToken := issuer.tokenFor(t, "alice"), issuer.tokenFor(t, "bob")

	conn, captured := holdRequest(t, e, holdFile, aliceToken)
	defer conn.Close()
	if captured != first {
		t.Fatalf("captured %s, want %s", captured, first)
	}

	// The next generation removes alice's role and grants READER the write.
	next := withoutRole(e.protectedPolicy(), "WRITER")
	next.Permits = append(next.Permits, Permit{Role: "READER", Operation: "write-a"})
	outcome, code := e.publish(Change{ID: "remove-writer", Expected: 1, Policy: mustJSON(next)})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "true}")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request: %v %v", resp, err)
	}
	e.assertReceipts(1, 0, 0)
	held := e.a.receipts()[0]
	if held.Generation != first || held.Headers.Get("X-Gateway-Roles") != "WRITER" {
		t.Fatalf("in-flight request reached A under %s with roles %q, want %s and WRITER", held.Generation, held.Headers.Get("X-Gateway-Roles"), first)
	}

	// New requests: alice is refused without reaching A; bob now may write.
	resp, body := e.exchange(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("alice after removal: %d %s", resp.StatusCode, body)
	}
	e.assertReceipts(1, 0, 0)
	_, refusedID := requestID(resp, body)
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":2}`, "Authorization", "Bearer "+bobToken); code != http.StatusOK {
		t.Fatalf("bob after the new permit: %d", code)
	}
	e.assertReceipts(2, 0, 0)
	if g := e.a.receipts()[1].Generation; g != outcome.Generation {
		t.Fatalf("bob's write under %s, want %s", g, outcome.Generation)
	}

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 2 })
	if ev := findEvent(events, "request.forwarded", held.RequestID); ev == nil || ev["generation"] != first || ev["subject"] != "alice" {
		t.Fatalf("in-flight event %v", ev)
	}
	if ev := findEvent(events, "request.rejected", refusedID); ev == nil || ev["generation"] != outcome.Generation || ev["reason"] != "no_permit" {
		t.Fatalf("refusal event %v", ev)
	}
}

// While membership changes repeatedly under concurrent load, every decision
// matches the generation it was made under: a request reaches the upstream
// only under a generation in which the caller holds a permitted role, and a
// refusal happens only under one in which it does not.
func TestRoleSwapsUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault)
	bobToken := issuer.tokenFor(t, "bob")

	// Whether bob may read /a/* under each generation.
	permitted := map[string]bool{e.status().Generation: true}

	const workers = 8
	const swaps = 100
	requests := make(chan struct{}, workers)
	completed := make(chan struct{}, workers)
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for range requests {
				req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", nil)
				req.Header.Set("Authorization", "Bearer "+bobToken)
				resp, err := client.Do(req)
				if err != nil {
					e.t.Logf("role-swap request failed: %v", err)
					unexpected.Add(1)
					completed <- struct{}{}
					continue
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if readErr != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusForbidden) {
					e.t.Logf("role-swap response: status=%d read=%v", resp.StatusCode, readErr)
					unexpected.Add(1)
				}
				completed <- struct{}{}
			}
		})
	}
	defer func() {
		close(requests)
		wg.Wait()
	}()

	for i := range swaps {
		bobRoles := []string{"READER"}
		if i%2 == 0 {
			bobRoles = nil
		}
		policy := e.protectedPolicy()
		if bobRoles == nil {
			policy = withoutRole(policy, "READER")
		}
		// Keep eight real requests racing every publication, with an exact
		// bound. An unbounded refusal loop exhausts Windows client ports
		// because the gateway deliberately closes refused connections.
		for range workers {
			requests <- struct{}{}
		}
		outcome, code := e.publish(Change{ID: fmt.Sprintf("swap-%d", i), Expected: int64(i + 1), Policy: mustJSON(policy)})
		if code != http.StatusOK {
			t.Fatalf("swap %d: %d %+v", i, code, outcome)
		}
		permitted[outcome.Generation] = bobRoles != nil
		for range workers {
			<-completed
		}
	}
	if unexpected.Load() != 0 {
		t.Fatalf("%d requests failed or got an unexpected status", unexpected.Load())
	}

	forwarded := 0
	for _, r := range e.a.receipts() {
		if !permitted[r.Generation] || r.Headers.Get("X-Gateway-Roles") != "READER" {
			t.Fatalf("receipt under %s with roles %q, where bob holds no permitted role", r.Generation, r.Headers.Get("X-Gateway-Roles"))
		}
		forwarded++
	}
	events := e.eventsWhen(func(events []map[string]any) bool {
		return countKind(events, "request.forwarded")+countKind(events, "request.rejected") >= workers*swaps
	})
	refused := 0
	for _, ev := range events {
		if ev["kind"] != "request.rejected" {
			continue
		}
		generation, _ := ev["generation"].(string)
		if ev["reason"] != "no_permit" || permitted[generation] {
			t.Fatalf("refusal %v under a generation in which bob is permitted", ev)
		}
		refused++
	}
	t.Logf("%d swaps: %d requests reached the upstream, %d refused, each under the right generation", swaps, forwarded, refused)
	if forwarded == 0 || refused == 0 {
		t.Fatal("the load did not exercise both outcomes")
	}
	if forwarded+refused != workers*swaps {
		t.Fatalf("%d receipts plus %d refusals, want %d decisions", forwarded, refused, workers*swaps)
	}
}

// A membership change that is invalid or based on a stale revision is
// refused as a whole; the previous membership keeps applying.
func TestRejectedRoleChangeKeepsMembership(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault)
	before := e.status()
	aliceToken := issuer.tokenFor(t, "alice")

	removal := withoutRole(e.protectedPolicy(), "WRITER")
	invalid := e.protectedPolicy()
	invalid.Permits = append(invalid.Permits, Permit{Role: "ADMIN", Operation: "write-a"})
	duplicate := e.protectedPolicy()
	duplicate.Roles = append(duplicate.Roles, Role{ID: "READER"})
	changes := []struct {
		change Change
		status int
		reason string
	}{
		{Change{ID: "unknown-role", Expected: 1, Policy: mustJSON(invalid)}, 400, "UNKNOWN_REFERENCE"},
		{Change{ID: "duplicate-subject", Expected: 1, Policy: mustJSON(duplicate)}, 400, "DUPLICATE_ID"},
		{Change{ID: "stale-removal", Expected: 0, Policy: mustJSON(removal)}, 409, "STALE"},
	}
	for _, c := range changes {
		outcome, code := e.publish(c.change)
		if code != c.status || outcome.Reason != c.reason {
			t.Errorf("%s: %d %+v, want %d %s", c.change.ID, code, outcome, c.status, c.reason)
		}
		if s := e.status(); s.Generation != before.Generation {
			t.Fatalf("%s changed the generation", c.change.ID)
		}
		if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken); code != http.StatusOK {
			t.Fatalf("alice after rejected %s: %d", c.change.ID, code)
		}
	}

	// The valid removal then applies.
	if _, code := e.publish(Change{ID: "disable-alice", Expected: 1, Policy: mustJSON(removal)}); code != http.StatusOK {
		t.Fatalf("valid removal: %d", code)
	}
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken); code != http.StatusForbidden {
		t.Fatalf("disabled alice: %d", code)
	}
	e.assertReceipts(len(changes), 0, 0)
}

// A crash during a membership publication leaves either the old or the new
// membership in force after restart, never a mixture, and the transition is
// applied exactly once.
func TestRoleChangeSurvivesCrash(t *testing.T) {
	for _, tc := range []struct {
		point     string
		committed bool
	}{
		{"before-commit", false},
		{"after-commit", true},
		{"after-swap", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			aliceToken, bobToken := issuer.tokenFor(t, "alice"), issuer.tokenFor(t, "bob")

			// Removes WRITER from policy and grants READER the write.
			next := withoutRole(e.protectedPolicy(), "WRITER")
			next.Permits = append(next.Permits, Permit{Role: "READER", Operation: "write-a"})
			change := Change{ID: "swap-roles", Expected: 1, Policy: mustJSON(next)}
			if _, code := e.publish(change); code != 0 {
				t.Fatalf("publication answered %d instead of crashing", code)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d, want %d\n%s", code, crashExit, e.output)
			}

			e.start(binaries.fault)
			wantAlice, wantBob, wantRevision := http.StatusOK, http.StatusForbidden, int64(1)
			if tc.committed {
				wantAlice, wantBob, wantRevision = http.StatusForbidden, http.StatusOK, 2
			}
			if s := e.status(); s.Revision != wantRevision {
				t.Fatalf("restart serves revision %d, want %d", s.Revision, wantRevision)
			}
			aliceCode, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken)
			bobCode, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":2}`, "Authorization", "Bearer "+bobToken)
			if aliceCode != wantAlice || bobCode != wantBob {
				t.Fatalf("after restart alice %d bob %d, want %d %d", aliceCode, bobCode, wantAlice, wantBob)
			}
			e.assertReceipts(1, 0, 0)

			// Retrying the same transition applies it exactly once.
			if outcome, code := e.publish(change); code != http.StatusOK || outcome.Revision != 2 {
				t.Fatalf("retry: %d %+v", code, outcome)
			}
			if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":3}`, "Authorization", "Bearer "+aliceToken); code != http.StatusForbidden {
				t.Fatalf("alice after retry: %d", code)
			}
		})
	}
}
