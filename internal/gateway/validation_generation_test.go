// Schemas follow the captured generation.

package gateway

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A request keeps the schema of the generation it captured, and under
// repeated schema publications every decision matches the schema of the
// generation it reports.
func TestSchemaFollowsCapturedGeneration(t *testing.T) {
	e := newEnv(t)
	textSchema := `{"type":"object","additionalProperties":false,"required":["v"],"properties":{"v":{"type":"string"}}}`
	numberSchema := `{"type":"object","additionalProperties":false,"required":["v"],"properties":{"v":{"type":"integer"}}}`
	withSchema := func(schema string) Policy {
		p := e.policy(e.a, false)
		for i := range p.Operations {
			if p.Operations[i].ID == "write-a" {
				p.Operations[i].Body = jsonBody(1024, schema)
			}
		}
		return p
	}
	seed(t, e.database, withSchema(textSchema), seedOptions{})
	dir := t.TempDir()
	holdFile, release := filepath.Join(dir, "captured"), filepath.Join(dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	first := e.status().Generation

	// Valid under the captured generation, invalid under the next one.
	result := make(chan int, 1)
	go func() {
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"v":"text"}`)
		result <- code
	}()
	waitForFile(t, holdFile)
	outcome, code := e.publish(Change{ID: "numbers", Expected: 1, Policy: mustJSON(withSchema(numberSchema))})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	os.WriteFile(release, nil, 0o600)
	if code := <-result; code != http.StatusOK {
		t.Fatalf("request captured under the text schema: %d, want 200", code)
	}
	if r := e.a.receipts(); len(r) != 1 || r[0].Generation != first || r[0].Body != `{"v":"text"}` {
		t.Fatalf("receipts %+v, want one under %s", r, first)
	}
	// New requests use the new schema.
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"v":"text"}`); code != http.StatusBadRequest {
		t.Fatalf("text value under the number schema: %d", code)
	}

	// Repeated publications under load: each generation's schema decides
	// every request that captured it.
	accepts := map[string]string{first: "text", outcome.Generation: "number"}
	revision := outcome.Revision
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	type decision struct{ body, requestID string }
	var rejected []decision
	for client := range 4 {
		wg.Go(func() {
			bodies := []string{`{"v":"text"}`, `{"v":7}`}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				body := bodies[(client+i)%2]
				resp, reply := e.exchange(http.MethodPost, "/a/item", "application/json", body)
				if resp.StatusCode == http.StatusBadRequest {
					_, id := requestID(resp, reply)
					mu.Lock()
					rejected = append(rejected, decision{body, id})
					mu.Unlock()
				} else if resp.StatusCode != http.StatusOK {
					t.Errorf("status %d", resp.StatusCode)
				}
			}
		})
	}
	for i := range 40 {
		schema, kind := textSchema, "text"
		if i%2 == 1 {
			schema, kind = numberSchema, "number"
		}
		outcome, code := e.publish(Change{ID: fmt.Sprintf("swap-%d", i), Expected: revision, Policy: mustJSON(withSchema(schema))})
		if code != http.StatusOK {
			t.Fatalf("swap %d: %d %+v", i, code, outcome)
		}
		revision = outcome.Revision
		mu.Lock()
		accepts[outcome.Generation] = kind
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	kindOf := func(body string) string {
		if strings.Contains(body, `"text"`) {
			return "text"
		}
		return "number"
	}
	for _, r := range e.a.receipts()[1:] {
		if accepts[r.Generation] != kindOf(r.Body) {
			t.Fatalf("generation %s (%s schema) forwarded %s", r.Generation, accepts[r.Generation], r.Body)
		}
	}
	events := e.auditEvents()
	for _, d := range rejected {
		ev := findEvent(events, "request.rejected", d.requestID)
		if ev == nil {
			ev = findEvent(e.eventsWhen(func(ev []map[string]any) bool { return findEvent(ev, "request.rejected", d.requestID) != nil }), "request.rejected", d.requestID)
		}
		generation, _ := ev["generation"].(string)
		if ev == nil || ev["stage"] != "VALIDATION" || accepts[generation] == kindOf(d.body) {
			t.Fatalf("rejection of %s: event %v, generation schema %s", d.body, ev, accepts[generation])
		}
	}
	t.Logf("%d forwarded and %d refused across 40 schema publications", len(e.a.receipts())-1, len(rejected))
	if len(rejected) == 0 || len(e.a.receipts()) < 3 {
		t.Fatal("the load did not exercise both outcomes")
	}
}
