// Repeated generation swaps under load, with the gateway's memory reported.

package gateway

import (
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepeatedSwapsUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault)

	// Every generation maps /a/* to exactly one receiver.
	destination := map[string]string{e.status().Generation: "A"}

	stop := make(chan struct{})
	var requests, failures atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := client.Get("http://" + e.dataAddr + "/a/x")
				if err != nil {
					failures.Add(1)
					continue
				}
				resp.Body.Close()
				requests.Add(1)
				if resp.StatusCode != http.StatusOK {
					failures.Add(1)
				}
			}
		})
	}

	before := workingSet(t, e.process.Process.Pid)
	revision := int64(1)
	const swaps = 200
	for i := range swaps {
		target := e.a
		if i%2 == 0 {
			target = e.b
		}
		outcome, code := e.publish(Change{ID: fmt.Sprintf("swap-%d", i), Expected: revision, Policy: mustJSON(e.policy(target, i%3 == 0))})
		if code != http.StatusOK {
			t.Fatalf("swap %d: %d %+v", i, code, outcome)
		}
		destination[outcome.Generation] = target.name
		revision = outcome.Revision
	}
	close(stop)
	wg.Wait()
	after := workingSet(t, e.process.Process.Pid)

	checked := 0
	for _, rc := range []*receiver{e.a, e.b} {
		for _, r := range rc.receipts() {
			if destination[r.Generation] != rc.name {
				t.Fatalf("receipt at %s under generation %s, which binds /a/* to %q", rc.name, r.Generation, destination[r.Generation])
			}
			checked++
		}
	}
	if e.c.count() != 0 {
		t.Fatal("decoy receiver was reached")
	}

	// Matching, binding, forwarding and the event used one generation.
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") >= checked })
	byRequest := map[string]map[string]any{}
	for _, ev := range events {
		if ev["kind"] == "request.forwarded" {
			byRequest[ev["request_id"].(string)] = ev
		}
	}
	for _, rc := range []*receiver{e.a, e.b} {
		for _, r := range rc.receipts() {
			ev := byRequest[r.RequestID]
			if ev == nil || ev["generation"] != r.Generation || ev["upstream"] != rc.name {
				t.Fatalf("receipt at %s under %s, event %v", rc.name, r.Generation, ev)
			}
		}
	}
	t.Logf("%d swaps, %d requests (%d non-200 or transport errors), %d receipts checked; working set %s -> %s",
		swaps, requests.Load(), failures.Load(), checked, before, after)
	if failures.Load() != 0 {
		t.Fatalf("%d requests failed during swaps", failures.Load())
	}
}

// workingSet reports the process's working set as shown by tasklist.
func workingSet(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return "unavailable"
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\",\"")
	return strings.Trim(fields[len(fields)-1], "\"")
}
