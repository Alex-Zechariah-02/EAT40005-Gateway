// Activation with fixtures and under load.

package gateway

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The lifecycle against the three fixture APIs, whose own receipts
// and state show what was executed. The candidate withdraws customers' right
// to create orders and grants staff the sales report. The test ends in the
// stable state for subsequent requests: one confirmed base, no provisional
// base, every activation resolved, fixtures back at their seeds.
func TestLifecycleWithFixtures(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	seedHash := map[string]string{}
	for _, f := range loadCatalogue(t).Fixtures {
		seedHash[f.Service] = f.SeedSHA256
	}
	candidate := fixturePolicy(fixtures)
	candidate.Permits = slices.DeleteFunc(candidate.Permits, func(p Permit) bool { return p == Permit{Role: "CUSTOMER", Operation: "orders.create"} })
	candidate.Permits = append(candidate.Permits, Permit{Role: "STAFF", Operation: "reports.sales"})

	// probe resets the fixtures, then has a customer create an order and a
	// member of staff read the sales report, and checks each fixture's
	// receipts and state.
	probe := func(underCandidate bool) {
		t.Helper()
		for _, f := range fixtures {
			f.reset()
		}
		order := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
		create, _ := e.request(http.MethodPost, "/orders", "application/json", order, bearer(t, "cust-001")...)
		sales, _ := e.get("/reports/sales", bearer(t, "staff-01")...)
		counts := receiptCounts(fixtures)
		ordersChanged := fixtures["orders"].state().SHA256 != seedHash["orders"]
		if underCandidate {
			if create != http.StatusForbidden || counts["orders"] != 0 || ordersChanged || sales != http.StatusOK || counts["reports"] != 1 {
				t.Fatalf("candidate policy: create %d, sales %d, receipts %v, orders changed %v", create, sales, counts, ordersChanged)
			}
		} else if create != http.StatusCreated || counts["orders"] != 1 || !ordersChanged || sales != http.StatusForbidden || counts["reports"] != 0 {
			t.Fatalf("confirmed base: create %d, sales %d, receipts %v, orders changed %v", create, sales, counts, ordersChanged)
		}
		if counts["inventory"] != 0 {
			t.Fatalf("inventory received %d requests", counts["inventory"])
		}
	}

	probe(false)
	_, first := e.provisionalFrom(harness, candidate, 1, 300, "fixtures-first")
	probe(true)
	if code, _, body := e.resolve(harness, "rollback", first.Provisional, `"r1"`, "fixtures-first-rollback"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	probe(false)
	_, second := e.provisionalFrom(harness, candidate, 1, 300, "fixtures-second")
	probe(true)
	if code, _, body := e.resolve(harness, "confirm", second.Provisional, `"r1"`, "fixtures-second-confirm"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	probe(true)

	s := e.status()
	if s.Confirmed != second.Provisional || s.Provisional != 0 || !s.NotAfter.IsZero() {
		t.Fatalf("stable state: %+v", s)
	}
	if code, _, _ := e.as(harness, http.MethodGet, "/admin/provisional", ""); code != http.StatusNotFound {
		t.Fatalf("provisional view in the stable state: %d", code)
	}
	code, body, _ := e.as(harness, http.MethodGet, "/admin/activations", "")
	items := fromJSON[struct {
		Items []activationRecord `json:"items"`
	}](t, body).Items
	if code != http.StatusOK || len(items) != 2 || items[0].Disposition != dispositionRolledBack || items[1].Disposition != dispositionConfirmed {
		t.Fatalf("activation history: %d %s", code, body)
	}
	for name, f := range fixtures {
		if state := f.reset(); state.SHA256 != seedHash[name] || len(f.receipts()) != 0 {
			t.Fatalf("reset of %s: %s", name, state.SHA256)
		}
	}
}

// Under continuous traffic, every request is decided and forwarded
// by one generation while activations, rollbacks and a confirmation are
// published: a receiver only ever sees generations of the policy that sends
// requests to it, and every answer is one that policy gives.
func TestLifecycleUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGenerations := map[string]bool{e.status().Generation: true}
	candidateGenerations := map[string]bool{}

	stop := make(chan struct{})
	var unexpected sync.Map
	var forwarded, refused atomic.Int64
	var wg sync.WaitGroup
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	aliceToken := "Bearer " + issuer.tokenFor(t, "alice")
	requests := []struct {
		method, path, body, token string
	}{
		{http.MethodGet, "/b/x", "", bobToken},
		{http.MethodPost, "/a/item", `{"n":1}`, bobToken},
		{http.MethodGet, "/a/x", "", aliceToken},
	}
	for i := range 6 {
		r := requests[i%len(requests)]
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(r.method, "http://"+e.dataAddr+r.path, strings.NewReader(r.body))
				req.Header.Set("Authorization", r.token)
				if r.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				resp, err := client.Do(req)
				if err != nil {
					unexpected.Store(r.method+" "+r.path+": "+err.Error(), "")
					continue
				}
				// A drained body lets the client reuse its connection.
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					forwarded.Add(1)
				case http.StatusForbidden:
					refused.Add(1)
				default:
					unexpected.Store(fmt.Sprintf("%s %s: %d", r.method, r.path, resp.StatusCode), resp.Header.Get("X-Request-Id"))
				}
			}
		})
	}

	confirmed := int64(1)
	for round := range 4 {
		time.Sleep(150 * time.Millisecond)
		_, activated := e.provisionalFrom(harness, e.cpaCandidate(), confirmed, 60, fmt.Sprintf("load-round-%02d", round))
		candidateGenerations[activated.Generation] = true
		time.Sleep(150 * time.Millisecond)
		action := "rollback"
		if round == 3 {
			action = "confirm"
		}
		code, outcome, body := e.resolve(harness, action, activated.Provisional, `"r1"`, fmt.Sprintf("load-round-%02d-%s", round, action))
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", action, code, body)
		}
		if action == "confirm" {
			candidateGenerations[outcome.Generation] = true
		} else {
			baseGenerations[outcome.Generation] = true
		}
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The audit records of an unexpected answer say which stage gave it.
	unexpected.Range(func(key, id any) bool {
		var records []map[string]any
		if id != "" {
			records = e.requestEvents(id.(string), 1)
		}
		t.Errorf("unexpected answer: %v; audit: %v", key, records)
		return true
	})
	// A and B are reached only under the confirmed base, C only under the
	// candidate; anything else would be a request decided by one generation
	// and forwarded by another.
	for _, check := range []struct {
		rc      *receiver
		allowed map[string]bool
	}{{e.a, baseGenerations}, {e.b, baseGenerations}, {e.c, candidateGenerations}} {
		for _, r := range check.rc.receipts() {
			if !check.allowed[r.Generation] {
				t.Fatalf("%s received a request of generation %s", check.rc.name, r.Generation)
			}
		}
	}
	if e.a.count() == 0 || e.b.count() == 0 || e.c.count() == 0 || refused.Load() == 0 {
		t.Fatalf("traffic did not exercise both policies: A %d B %d C %d refused %d", e.a.count(), e.b.count(), e.c.count(), refused.Load())
	}
	t.Logf("forwarded %d, refused %d; receipts A %d B %d C %d", forwarded.Load(), refused.Load(), e.a.count(), e.b.count(), e.c.count())
}
