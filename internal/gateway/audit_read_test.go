// The test harness's audit file reader.

package gateway

import (
	"strconv"
	"strings"
	"testing"
)

// A record a crash cut short stays in the file; the log names its offset in
// an audit.recovered record. The reader skips exactly that line and still
// refuses any other line that is not one record.
func TestParseAuditSkipsOnlyRecoveredRecords(t *testing.T) {
	first := `{"kind":"request.admitted","seq":1}` + "\n"
	partial := `{"kind":"request.rejected","request_id":"94d3` + "\n"
	recovered := `{"kind":"audit.recovered","file":"audit.log","offset":` + strconv.Itoa(len(first)) + `}` + "\n"
	events, err := parseAudit([]byte(first+partial+recovered), "audit.log")
	if err != nil || len(events) != 2 || events[0]["kind"] != "request.admitted" || events[1]["kind"] != "audit.recovered" {
		t.Fatalf("recovered record: %v %v", events, err)
	}

	unexplained := strings.Replace(recovered, `"offset":`+strconv.Itoa(len(first)), `"offset":1`, 1)
	if _, err := parseAudit([]byte(first+partial+unexplained), "audit.log"); err == nil {
		t.Fatal("a broken line no record names was accepted")
	}
	otherFile := strings.Replace(recovered, `"file":"audit.log"`, `"file":"audit.log.1"`, 1)
	if _, err := parseAudit([]byte(first+partial+otherFile), "audit.log"); err == nil {
		t.Fatal("a recovery named for another file was applied")
	}

	// An unterminated tail is a record still being written: it is not read.
	if events, err := parseAudit([]byte(first+`{"kind":"request.rej`), "audit.log"); err != nil || len(events) != 1 {
		t.Fatalf("unterminated tail: %v %v", events, err)
	}
}
