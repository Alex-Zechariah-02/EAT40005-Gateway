// Integration harness audit file reading.

package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"time"
)

func (e *env) auditEvents() []map[string]any {
	e.t.Helper()
	data, err := os.ReadFile(e.auditLog)
	if err != nil {
		e.t.Fatal(err)
	}
	// The gateway may be appending while the file is read: every record ends
	// with a newline, so an unterminated tail is a record still being written.
	data = data[:bytes.LastIndexByte(data, '\n')+1]
	var events []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			e.t.Fatalf("audit line is not one JSON record: %q", line)
		}
		events = append(events, ev)
	}
	return events
}

// eventsWhen waits until the audit log satisfies done. Events that do not
// gate a decision are written asynchronously, so they may land shortly after
// the response.
func (e *env) eventsWhen(done func([]map[string]any) bool) []map[string]any {
	e.t.Helper()
	limit := time.Now().Add(5 * time.Second)
	for {
		events := e.auditEvents()
		if done(events) || time.Now().After(limit) {
			return events
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countKind(events []map[string]any, kind string) int {
	n := 0
	for _, ev := range events {
		if ev["kind"] == kind {
			n++
		}
	}
	return n
}
