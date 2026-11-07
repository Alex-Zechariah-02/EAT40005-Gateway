// Tests of migrate policy: compiled output, looser settings only when accepted, carried data never compiled, and limits.

package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func base() policy.Policy {
	return policy.Policy{
		SchemaVersion: policy.SchemaVersion, Profile: policy.ProfileCore,
		Upstreams:  []policy.Upstream{{ID: "shop-svc", Dial: "127.0.0.1:9101"}},
		APIs:       []policy.API{{ID: "inventory", Upstream: "shop-svc"}},
		Operations: []policy.Operation{{ID: "inventory.list", API: "inventory", Method: "GET", Target: "/inventory", Access: "allow", Auth: "none"}},
		Roles:      []policy.Role{}, Permits: []policy.Permit{},
	}
}

// migrated runs migrate policy on an exchange file and returns its exit
// status, stderr and the output and report paths.
func migrated(t *testing.T, exchangePath string, p policy.Policy, extra ...string) (int, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	basePath, out, report := filepath.Join(dir, "base.json"), filepath.Join(dir, "policy.json"), filepath.Join(dir, "report.json")
	data, _ := json.Marshal(p)
	if err := os.WriteFile(basePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"policy", "--exchange", exchangePath, "--base", basePath, "--api", "shop", "--upstream", "shop-svc",
		"--out", out, "--report", report}, extra...)
	code, stderr := run(args...)
	return code, stderr, out, report
}

// rewrite changes an exchange file's decoded content and writes it as a new JSON file.
func rewrite(t *testing.T, path string, change func(document map[string]any, ledger *exchange.Ledger)) string {
	t.Helper()
	document, ledger := ledgerOf(t, path)
	change(document, ledger)
	attached, err := exchange.Attach(document, ledger)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(attached)
	out := filepath.Join(t.TempDir(), "changed.json")
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func operationIDs(t *testing.T, path string) []string {
	t.Helper()
	data, _ := os.ReadFile(path)
	p, err := policy.DecodePolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compile.Compile(p, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, op := range p.Operations {
		ids = append(ids, op.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestMigratePolicy(t *testing.T) {
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.yaml")
	code, stderr, out, reportPath := migrated(t, exchangePath, base())
	if code != openapi.ExitPartial {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := []string{"inventory.list", "shop.createOrder", "shop.getFile", "shop.health", "shop.listOrders"}
	if ids := operationIDs(t, out); !slices.Equal(ids, want) {
		t.Fatalf("operations %v, want %v", ids, want)
	}
	var report Report
	data, _ := os.ReadFile(reportPath)
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	refused := slices.IndexFunc(report.Refused, func(r openapi.Refused) bool { return r.Code == CodeLooser })
	if refused < 0 || report.Refused[refused].Path != "/orders/{id}" || report.API != "shop" || report.LedgerSummary.Exact != 4 {
		t.Fatalf("report %s", data)
	}
}

// TestLooserNeedsAccept imports a looser setting only once its ledger item is accepted.
func TestLooserNeedsAccept(t *testing.T) {
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	accepted := rewrite(t, exchangePath, func(_ map[string]any, ledger *exchange.Ledger) {
		for i := range ledger.Items {
			if ledger.Items[i].ID == "openapi:operation:GET /orders/{id}" {
				ledger.Items[i].Accepted = true
			}
		}
	})
	code, stderr, out, _ := migrated(t, accepted, base())
	if code != openapi.ExitPartial || !slices.Contains(operationIDs(t, out), "shop.getOrder") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

// TestCarriedNeverCompiled puts a canary in every place the policy must not
// read: carried values, ledger notes and vendor extensions.
func TestCarriedNeverCompiled(t *testing.T) {
	const canary = "canary-7f3a9"
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	changed := rewrite(t, exchangePath, func(document map[string]any, ledger *exchange.Ledger) {
		ledger.Carried = append(ledger.Carried, exchange.Carried{Vendor: "openapi", Scope: "api", Path: "#/x-vendor",
			Value: json.RawMessage(`{"name": "` + canary + `", "config": {"origins": ["` + canary + `"]}}`)})
		ledger.Items[0].Note = canary
		get := document["paths"].(map[string]any)["/health"].(map[string]any)["get"].(map[string]any)
		get["x-kong-plugin-cors"] = map[string]any{"config": canary}
		document["x-vendor"] = canary
	})
	code, stderr, out, reportPath := migrated(t, changed, base())
	if code != openapi.ExitPartial {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, path := range []string{out, reportPath} {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), canary) {
			t.Fatalf("%s holds a carried value", path)
		}
	}
	var report Report
	data, _ := os.ReadFile(reportPath)
	if json.Unmarshal(data, &report) != nil || report.CarriedCount != 1 {
		t.Fatalf("report %s", data)
	}
}

// TestPolicyLimitRefused names the limit a result would exceed and writes nothing.
func TestPolicyLimitRefused(t *testing.T) {
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	full := base()
	for i := range policy.MaxOperations - 2 {
		full.Operations = append(full.Operations, policy.Operation{ID: fmt.Sprintf("inventory.op%d", i), API: "inventory",
			Method: "GET", Target: fmt.Sprintf("/inventory/%d", i), Access: "allow", Auth: "none"})
	}
	large := base()
	for i := 0; len(mustJSON(t, large)) < policy.MaxPolicyBytes-500; i++ {
		large.Operations = append(large.Operations, policy.Operation{ID: fmt.Sprintf("inventory.op%d", i), API: "inventory",
			Method: "GET", Target: fmt.Sprintf("/inventory/%s/%d", strings.Repeat("x", 200), i), Access: "allow", Auth: "none"})
	}
	for name, c := range map[string]struct {
		base policy.Policy
		want string
	}{
		"operations": {full, "operations: at most 256"},
		"size":       {large, "size: at most 65536 bytes"},
	} {
		code, stderr, out, report := migrated(t, exchangePath, c.base)
		if code != openapi.ExitFailed || !strings.Contains(stderr, CodeLimit) || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, stderr %q", name, code, stderr)
		}
		for _, path := range []string{out, report} {
			if _, err := os.Stat(path); err == nil {
				t.Errorf("%s: %s was written", name, path)
			}
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestPolicyNeedsExchange refuses a description without a ledger and a ledger
// whose looser item points at no operation.
func TestPolicyNeedsExchange(t *testing.T) {
	_, plain := source(t, "shop.json")
	if code, stderr, _, _ := migrated(t, plain, base()); code != openapi.ExitFailed || !strings.Contains(stderr, "not an exchange file") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	exchangePath, _ := imported(t, "shop.json", "shop.exchange.json")
	dangling := rewrite(t, exchangePath, func(document map[string]any, _ *exchange.Ledger) {
		delete(document["paths"].(map[string]any), "/orders/{id}")
	})
	if code, stderr, _, _ := migrated(t, dangling, base()); code != openapi.ExitFailed || !strings.Contains(stderr, "no operation") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
