// Tests of the Kong commands: secrets never copied, anonymous routes left out until accepted, one service per API, export and inventory.

package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

// kongFile copies a decK fixture of the kong package into a new directory.
func kongFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "convert", "kong", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// kongImport runs migrate import --from kong-deck and returns the exchange
// file, the report, the exit status and stderr.
func kongImport(t *testing.T, name string, extra ...string) (string, string, int, string) {
	t.Helper()
	dir := t.TempDir()
	out, report := filepath.Join(dir, "exchange.yaml"), filepath.Join(dir, "import-report.json")
	code, stderr := run(append([]string{"import", "--from", "kong-deck", "--in", kongFile(t, name), "--out", out, "--report", report}, extra...)...)
	return out, report, code, stderr
}

// TestSecretsNeverCopied runs every command on the fixture with canary
// credentials; no canary may reach any output, report or message.
func TestSecretsNeverCopied(t *testing.T) {
	exchangePath, importReport, code, stderr := kongImport(t, "shop.yaml")
	if code != openapi.ExitPartial {
		t.Fatalf("import exit %d: %s", code, stderr)
	}
	files := []string{exchangePath, importReport}
	messages := []string{stderr}
	code, stderr, policyOut, policyReport := migrated(t, exchangePath, base())
	if code != openapi.ExitPartial {
		t.Fatalf("policy exit %d: %s", code, stderr)
	}
	files, messages = append(files, policyOut, policyReport), append(messages, stderr)
	dir := t.TempDir()
	deck, exportReport := filepath.Join(dir, "kong.yaml"), filepath.Join(dir, "export-report.json")
	code, stderr = run("export", "--to", "kong-deck", "--exchange", exchangePath, "--out", deck, "--report", exportReport,
		"--with-upstream", "--with-trust", "--with-blocked")
	if code != openapi.ExitPartial {
		t.Fatalf("export exit %d: %s", code, stderr)
	}
	files, messages = append(files, deck, exportReport), append(messages, stderr)
	inventory := filepath.Join(dir, "inventory.json")
	code, stderr = run("inventory", "--from", "kong-deck", "--in", kongFile(t, "shop.yaml"), "--report", inventory)
	files, messages = append(files, inventory), append(messages, stderr)
	_, stderr = run("export", "--to", "kong-deck", "--exchange", exchangePath, "--out", deck, "--report", exportReport)
	messages = append(messages, stderr) // a failure: the files exist
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, string(data))
	}
	for i, text := range messages {
		if strings.Contains(text, "CANARY") || strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("output %d holds a credential: %s", i, text)
		}
	}
	if code != openapi.ExitPartial {
		t.Errorf("inventory exit %d", code)
	}
}

// TestKongPolicy imports the clean fixture: the anonymous route is left
// out until its looser item is accepted; roles need --with-permits.
func TestKongPolicy(t *testing.T) {
	exchangePath, _, code, stderr := kongImport(t, "clean.yaml")
	if code != openapi.ExitPartial {
		t.Fatalf("import exit %d: %s", code, stderr)
	}
	code, stderr, out, _ := migrated(t, exchangePath, base())
	want := []string{"inventory.list", "shop.list-1.get", "shop.list-1.post", "shop.list.get", "shop.list.post", "shop.status-1.get", "shop.status.get"}
	if got := operationIDs(t, out); code != openapi.ExitPartial || !slices.Equal(got, want) {
		t.Fatalf("exit %d, operations %v: %s", code, got, stderr)
	}
	accept := `{"target": "$['x-apiward-migration'].items[?@.id=='kong-deck:operation:catalog:GET /catalog/{rest}:jwt']", "update": {"accepted": true, "direction": "looser"}}`
	code, stderr, out, _ = migrated(t, exchangePath, base(), "--decisions", decisionsFile(t, accept), "--with-permits")
	if code != openapi.ExitPartial || !slices.Contains(operationIDs(t, out), "shop.catalog.get") {
		t.Fatalf("accepted: exit %d, %v: %s", code, operationIDs(t, out), stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"role": "CUSTOMER"`) || strings.Contains(string(data), "x-kong") {
		t.Errorf("permits missing or Kong settings in the policy: %s", data)
	}
}

// TestOneServicePerAPI refuses an exchange file from two services.
func TestOneServicePerAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "two.json")
	text := `{"_format_version": "3.0", "services": [
		{"name": "a", "url": "http://a:1", "routes": [{"name": "ra", "paths": ["/a"], "methods": ["GET"], "strip_path": false}]},
		{"name": "b", "url": "http://b:1", "routes": [{"name": "rb", "paths": ["/b"], "methods": ["GET"], "strip_path": false}]},
		{"name": "c", "url": "http://c:1", "routes": [{"name": "rc", "paths": ["/c"], "strip_path": true}]}]}`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	out, report := filepath.Join(t.TempDir(), "x.json"), filepath.Join(t.TempDir(), "r.json")
	if code, stderr := run("import", "--from", "kong-deck", "--in", path, "--out", out, "--report", report); code != openapi.ExitPartial {
		t.Fatalf("import exit %d: %s", code, stderr)
	}
	code, stderr, policyOut, _ := migrated(t, out, base())
	if code != openapi.ExitFailed || !strings.Contains(stderr, "2 services (a, b)") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(policyOut); err == nil {
		t.Error("a policy was written")
	}
}

// TestKongInventory counts without values.
func TestKongInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.json")
	if code, stderr := run("inventory", "--from", "kong-deck", "--in", kongFile(t, "shop.yaml"), "--report", path); code != openapi.ExitPartial {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Services, Consumers int
		Routes              []struct{ Route string }
		Credentials         []struct{ Type string }
		Unsupported         []string
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Services != 1 || inventory.Consumers != 2 || len(inventory.Routes) != 14 || len(inventory.Credentials) != 5 ||
		!slices.Contains(inventory.Unsupported, "route:legacy: PATH_TRANSFORM_UNSUPPORTED") {
		t.Errorf("inventory = %s", data)
	}
	if code, _ := run("inventory", "--from", "tyk-oas", "--in", path, "--report", path+".2"); code != openapi.ExitFailed {
		t.Error("an unknown format was accepted")
	}
}

// TestExportCommand checks the command line: format, flags once, and the
// GetPut round trip through files.
func TestExportCommand(t *testing.T) {
	exchangePath, _, _, _ := kongImport(t, "clean.yaml")
	dir := t.TempDir()
	deck, report := filepath.Join(dir, "kong.json"), filepath.Join(dir, "report.json")
	for _, args := range [][]string{
		{"export", "--to", "tyk-oas", "--exchange", exchangePath, "--out", deck, "--report", report},
		{"export", "--to", "kong-deck", "--exchange", exchangePath, "--out", deck, "--report", report, "--with-trust", "--with-trust"},
		{"export", "--to", "kong-deck", "--exchange", exchangePath, "--out", deck},
	} {
		if code, _ := run(args...); code != openapi.ExitFailed {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	code, stderr := run("export", "--to", "kong-deck", "--exchange", exchangePath, "--out", deck, "--report", report, "--with-upstream")
	if code != openapi.ExitPartial || !strings.Contains(stderr, "wrote 4 routes as read and 0 from operations") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

// A Kong service path and a route's strip_path become the API's forward
// rule: a route with another rule is refused, the policy carries the rule,
// and the export to Kong restores the route as read.
func TestKongForward(t *testing.T) {
	dir := t.TempDir()
	deck := filepath.Join(dir, "deck.json")
	text := `{"_format_version": "3.0", "services": [{"name": "shop", "url": "http://shop:8080/v2", "routes": [
		{"name": "api", "paths": ["/api"], "methods": ["GET"]},
		{"name": "plain", "paths": ["/plain"], "methods": ["GET"], "strip_path": false}]}]}`
	if err := os.WriteFile(deck, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	exchangePath, report := filepath.Join(dir, "x.json"), filepath.Join(dir, "r.json")
	if code, stderr := run("import", "--from", "kong-deck", "--in", deck, "--out", exchangePath, "--report", report); code != openapi.ExitPartial {
		t.Fatalf("import exit %d: %s", code, stderr)
	}
	document, ledger := ledgerOf(t, exchangePath)
	classes := map[string]string{}
	for _, item := range ledger.Items {
		classes[item.ID] = string(item.Class)
	}
	if forward, _ := json.Marshal(document["x-apiward-forward"]); string(forward) != `{"add_prefix":"/v2","strip_prefix":"/api"}` ||
		classes["kong-deck:route:api:forward"] != "equivalent" || classes["kong-deck:route:plain"] != "not_expressible" {
		t.Fatalf("forward %s, items %v", forward, classes)
	}

	code, stderr, out, _ := migrated(t, exchangePath, base())
	data, _ := os.ReadFile(out)
	if code != openapi.ExitPartial || !strings.Contains(string(data), `"strip_prefix": "/api"`) {
		t.Fatalf("policy exit %d: %s\n%s", code, stderr, data)
	}

	exported, exportReport := filepath.Join(dir, "deck-out.json"), filepath.Join(dir, "export.json")
	if code, stderr := run("export", "--to", "kong-deck", "--exchange", exchangePath, "--out", exported, "--report", exportReport); code == openapi.ExitFailed {
		t.Fatalf("export exit %d: %s", code, stderr)
	}
	var written struct {
		Restored []string `json:"restored"`
	}
	data, _ = os.ReadFile(exportReport)
	if json.Unmarshal(data, &written) != nil || !slices.Contains(written.Restored, "api") {
		t.Fatalf("export report %s", data)
	}
}
