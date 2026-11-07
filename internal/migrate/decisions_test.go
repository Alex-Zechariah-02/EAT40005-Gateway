// Tests of --decisions on migrate import and migrate policy: accepted
// looser settings, stale actions in the report, and refusals that write nothing.

package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// acceptGetOrder accepts the looser templated path of GET /orders/{id} in shop.json.
const acceptGetOrder = `{"target": "$['x-apiward-migration'].items[?@.id=='openapi:operation:GET /orders/{id}']",
  "update": {"accepted": true, "direction": "looser"}}`

// decisionsFile writes a decisions file with the given actions and returns its path.
func decisionsFile(t *testing.T, actions string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "decisions.json")
	text := `{"overlay": "1.2.0", "info": {"title": "decisions", "version": "1"}, "actions": [` + actions + `]}`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reportOf(t *testing.T, path string) Report {
	t.Helper()
	var report Report
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestDecisionsAcceptOnPolicy(t *testing.T) {
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	code, stderr, out, reportPath := migrated(t, exchangePath, base(), "--decisions", decisionsFile(t, acceptGetOrder))
	if code != openapi.ExitPartial || !slices.Contains(operationIDs(t, out), "shop.getOrder") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if report := reportOf(t, reportPath); report.StaleDecisions == nil || len(report.StaleDecisions) != 0 {
		t.Fatalf("stale %v", report.StaleDecisions)
	}
}

// TestDecisionsOnImport records the decision in the exchange file, so
// migrate policy imports the accepted operation without the decisions file.
func TestDecisionsOnImport(t *testing.T) {
	dir, in := source(t, "shop.json")
	exchangePath := filepath.Join(dir, "shop.exchange.yaml")
	lower := `{"target": "$.paths[*][?@.operationId=='createOrder']", "update": {"x-apiward-rate": {"count": 5}}}`
	code, stderr := run("import", "--from", "openapi", "--in", in, "--out", exchangePath, "--report", filepath.Join(dir, "r.json"),
		"--decisions", decisionsFile(t, acceptGetOrder+", "+lower))
	if code != openapi.ExitPartial {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	document, ledger := ledgerOf(t, exchangePath)
	accepted := slices.IndexFunc(ledger.Items, func(i exchange.Item) bool { return i.Accepted })
	if accepted < 0 || ledger.Items[accepted].ID != "openapi:operation:GET /orders/{id}" {
		t.Fatalf("ledger %+v", ledger.Items)
	}
	rate := document["paths"].(map[string]any)["/orders"].(map[string]any)["post"].(map[string]any)["x-apiward-rate"]
	if rate.(map[string]any)["count"] != json.Number("5") {
		t.Fatalf("rate %v", rate)
	}
	code, stderr, out, _ := migrated(t, exchangePath, base())
	if code != openapi.ExitPartial || !slices.Contains(operationIDs(t, out), "shop.getOrder") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

// TestDecisionsStale reports each action that selects nothing, and a stale
// action alone makes an otherwise complete import exit 2.
func TestDecisionsStale(t *testing.T) {
	dir, in := source(t, "open.json")
	gone := `{"target": "$.paths[*][?@.operationId=='gone']", "update": {"x-apiward-access": "deny"}}`
	reportPath := filepath.Join(dir, "r.json")
	code, stderr := run("import", "--from", "openapi", "--in", in, "--out", filepath.Join(dir, "x.json"), "--report", reportPath,
		"--decisions", decisionsFile(t, gone))
	if code != openapi.ExitPartial {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	report := reportOf(t, reportPath)
	if len(report.StaleDecisions) != 1 || report.StaleDecisions[0] != "actions[0]: $.paths[*][?@.operationId=='gone']" {
		t.Fatalf("stale %q", report.StaleDecisions)
	}
	target := "$.paths[*][?@.operationId=='" + strings.Repeat("a", 400) + "']"
	long := `{"target": "` + target + `", "update": {}}`
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	_, stderr, _, reportPath = migrated(t, exchangePath, base(), "--decisions", decisionsFile(t, long))
	if stale := reportOf(t, reportPath).StaleDecisions; len(stale) != 1 || stale[0] != policy.Clip("actions[0]: "+target) {
		t.Fatalf("stale %q: %s", stale, stderr)
	}
}

// TestDecisionsRefused writes nothing when a decision is not accepted.
func TestDecisionsRefused(t *testing.T) {
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	refused := map[string]string{
		"remove":   `{"target": "$['paths']['/health']['get']", "remove": true}`,
		"widening": `{"target": "$['paths']['/health']['get']", "update": {"x-apiward-roles": ["admin"]}}`,
		"accept":   `{"target": "$['x-apiward-migration'].items[?@.id=='openapi:operation:GET /orders/{id}']", "update": {"accepted": true}}`,
	}
	for name, action := range refused {
		code, stderr, out, reportPath := migrated(t, exchangePath, base(), "--decisions", decisionsFile(t, action))
		_, outErr := os.Stat(out)
		_, reportErr := os.Stat(reportPath)
		if code != openapi.ExitFailed || !os.IsNotExist(outErr) || !os.IsNotExist(reportErr) {
			t.Errorf("%s: exit %d, files left: %v %v: %s", name, code, outErr, reportErr, stderr)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent.json")
	if code, _, _, _ := migrated(t, exchangePath, base(), "--decisions", missing); code != openapi.ExitFailed {
		t.Fatalf("missing decisions file: exit %d", code)
	}
	if code, stderr, _, _ := migrated(t, exchangePath, base(), "--decisions", decisionsFile(t, acceptGetOrder),
		"--decisions", decisionsFile(t, acceptGetOrder)); code != openapi.ExitFailed || !strings.Contains(stderr, "more than once") {
		t.Fatalf("twice: exit %d: %s", code, stderr)
	}
}
