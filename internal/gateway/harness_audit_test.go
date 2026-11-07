// Integration harness audit file reading.

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *env) auditEvents() []map[string]any {
	e.t.Helper()
	data, err := os.ReadFile(e.auditLog)
	if err != nil {
		e.t.Fatal(err)
	}
	events, err := parseAudit(data, filepath.Base(e.auditLog))
	if err != nil {
		e.t.Fatal(err)
	}
	return events
}

// parseAudit decodes the records of the audit file named file. The gateway
// may be appending while it is read, so an unterminated tail is a record
// still being written and is skipped. A record a crash cut short stays in the
// file, ended by the next start, which names its offset in an audit.recovered
// record; only such a line may fail to decode.
func parseAudit(data []byte, file string) ([]map[string]any, error) {
	data = data[:bytes.LastIndexByte(data, '\n')+1]
	var events []map[string]any
	var broken []int
	recovered := map[int]bool{}
	for offset := 0; offset < len(data); {
		end := offset + bytes.IndexByte(data[offset:], '\n')
		line := data[offset:end]
		var ev map[string]any
		switch {
		case len(bytes.TrimSpace(line)) == 0:
		case json.Unmarshal(line, &ev) != nil:
			broken = append(broken, offset)
		default:
			if at, ok := ev["offset"].(float64); ok && ev["kind"] == "audit.recovered" && ev["file"] == file {
				recovered[int(at)] = true
			}
			events = append(events, ev)
		}
		offset = end + 1
	}
	for _, at := range broken {
		if !recovered[at] {
			end := at + bytes.IndexByte(data[at:], '\n')
			return nil, fmt.Errorf("audit line is not one JSON record: %q", data[at:end])
		}
	}
	return events, nil
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

// hasKinds reports whether events holds at least one event of every kind.
func hasKinds(events []map[string]any, kinds ...string) bool {
	for _, kind := range kinds {
		if countKind(events, kind) == 0 {
			return false
		}
	}
	return true
}

// auditLine is what the restart tests read of one audit record.
type auditLine struct {
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	Seq       uint64 `json:"seq"`
	Lost      uint64 `json:"lost"`
}

// auditLines reads every record of an audit file.
func auditLines(t *testing.T, path string) []auditLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []auditLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e auditLine
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line is not one record: %q", line)
		}
		events = append(events, e)
	}
	return events
}
