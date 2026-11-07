// Tests of migrate import: the exchange file, its ledger, the report and the command line.

package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

// settings is the number of source settings in each fixture, counted by
// hand: shop has 7 operations, 2 unchecked parameters, 1 optional body,
// 1 scope and its servers; plain has 2 operations; open has 1.
var settings = map[string]int{"shop.json": 12, "plain.yaml": 2, "open.json": 1}

// run runs a migrate command line and returns its exit status and stderr.
func run(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := Command(args, &stderr)
	return code, stderr.String()
}

// source copies a fixture into a new directory and returns the directory and the copy.
func source(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// read decodes a written file through the import's own gate.
func read(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	format, err := openapi.FormatOf(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := openapi.Decode(data, format, MaxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

// imported runs migrate import on a fixture and returns the exchange file's path.
func imported(t *testing.T, name, out string) (string, int) {
	t.Helper()
	dir, in := source(t, name)
	exchangePath := filepath.Join(dir, out)
	code, stderr := run("import", "--from", "openapi", "--in", in, "--out", exchangePath, "--report", filepath.Join(dir, "import-report.json"))
	if code == openapi.ExitFailed {
		t.Fatalf("import failed: %s", stderr)
	}
	return exchangePath, code
}

func ledgerOf(t *testing.T, path string) (map[string]any, *exchange.Ledger) {
	t.Helper()
	document, ledger, err := exchange.Split(read(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return document, ledger
}

func TestMigrateImport(t *testing.T) {
	path, code := imported(t, "shop.json", "shop.exchange.json")
	if code != openapi.ExitPartial {
		t.Fatalf("exit %d, want 2", code)
	}
	_, ledger := ledgerOf(t, path)
	raw, _ := os.ReadFile(filepath.Join("testdata", "shop.json"))
	sum := sha256.Sum256(raw)
	if ledger.Source != (exchange.Source{Format: "openapi", FormatVersion: "3.1.0", SHA256: hex.EncodeToString(sum[:])}) {
		t.Fatalf("source %+v", ledger.Source)
	}
	want := map[string][2]string{
		"openapi:operation:GET /orders":              {"exact", ""},
		"openapi:operation:POST /orders":             {"exact", ""},
		"openapi:operation:GET /files/{rest}":        {"exact", ""},
		"openapi:operation:GET /health":              {"exact", ""},
		"openapi:operation:GET /orders/{id}":         {"approximated", "looser"},
		"openapi:operation:POST /orders:body":        {"approximated", "stricter"},
		"openapi:operation:GET /orders/{id}/items":   {"not_expressible", "PATH_TEMPLATE_UNSUPPORTED"},
		"openapi:operation:TRACE /debug":             {"not_expressible", "METHOD_REFUSED"},
		"openapi:parameter:GET /orders:query.page":   {"not_expressible", "query"},
		"openapi:parameter:GET /orders/{id}:path.id": {"not_expressible", "path"},
		"openapi:scope:orders:read":                  {"not_expressible", "roles"},
		"openapi:member:servers":                     {"not_expressible", "--upstream"},
	}
	for _, item := range ledger.Items {
		w, ok := want[item.ID]
		if !ok {
			t.Errorf("unexpected item %+v", item)
			continue
		}
		delete(want, item.ID)
		if string(item.Class) != w[0] || item.Class == exchange.Approximated && string(item.Direction) != w[1] ||
			item.Class == exchange.NotExpressible && !strings.Contains(item.Note, w[1]) {
			t.Errorf("%s: %+v, want %v", item.ID, item, w)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing items %v", want)
	}
	var report Report
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "import-report.json"))
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	summary := Summary{Exact: 4, Approximated: Directions{Stricter: 1, Looser: 1}, NotExpressible: 6}
	if report.LedgerSummary != summary || report.API != "" || len(report.Refused) != 2 || report.StaleDecisions == nil {
		t.Fatalf("report %s", data)
	}
}

// TestMigrateImportComplete exits 0 when every setting is exact, and writes YAML.
func TestMigrateImportComplete(t *testing.T) {
	path, code := imported(t, "plain.yaml", "plain.exchange.yaml")
	if code != openapi.ExitComplete {
		t.Fatalf("exit %d", code)
	}
	if _, ledger := ledgerOf(t, path); ledger.Source.FormatVersion != "3.0.3" || len(ledger.Items) != 2 {
		t.Fatalf("ledger %+v", ledger)
	}
}

// TestDefaultSignIn classes an operation without stated security as stricter.
func TestDefaultSignIn(t *testing.T) {
	path, code := imported(t, "open.json", "open.exchange.json")
	_, ledger := ledgerOf(t, path)
	if item := ledger.Items[0]; code != openapi.ExitPartial || item.Class != exchange.Approximated || item.Direction != exchange.Stricter {
		t.Fatalf("exit %d, %+v", code, item)
	}
}

// TestLedgerComplete accounts for every source setting: one item per
// operation, every item's source pointer resolving, and the counted total.
func TestLedgerComplete(t *testing.T) {
	for name, count := range settings {
		path, _ := imported(t, name, "out.exchange.json")
		_, ledger := ledgerOf(t, path)
		_, in := source(t, name)
		src := read(t, in)
		if len(ledger.Items) != count {
			t.Errorf("%s: %d items, %d settings", name, len(ledger.Items), count)
		}
		operations := map[string]int{}
		for _, item := range ledger.Items {
			if _, err := resolve(src, item.Source); err != nil {
				t.Errorf("%s: %s: source %s: %v", name, item.ID, item.Source, err)
			}
			if strings.Count(item.ID, ":") == 2 && strings.HasPrefix(item.ID, "openapi:operation:") {
				operations[item.Source]++
			}
		}
		paths := src["paths"].(map[string]any)
		for path, value := range paths {
			for method := range value.(map[string]any) {
				if operations["#/paths/"+escape(path)+"/"+method] != 1 {
					t.Errorf("%s: %s %s has no single operation item", name, method, path)
				}
			}
		}
	}
}

// TestLensGetPut: the exchange file without its ledger is the source description.
func TestLensGetPut(t *testing.T) {
	for name := range settings {
		path, _ := imported(t, name, "out.exchange.yaml")
		document, _ := ledgerOf(t, path)
		_, in := source(t, name)
		if !reflect.DeepEqual(document, read(t, in)) {
			t.Errorf("%s: the exchange file changed the description", name)
		}
	}
}

func TestMigrateCommand(t *testing.T) {
	dir, in := source(t, "shop.json")
	large := filepath.Join(dir, "large.json")
	os.WriteFile(large, append([]byte(`{"openapi": "3.1.0", "x": "`), bytes.Repeat([]byte("x"), MaxInputBytes)...), 0o600)
	folder := filepath.Join(dir, "folder.json")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	out, report := filepath.Join(dir, "x.json"), filepath.Join(dir, "r.json")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", nil, "usage"},
		{"unknown subcommand", []string{"verify"}, "usage"},
		{"unknown format", []string{"import", "--from", "kong", "--in", in, "--out", out, "--report", report}, "usage"},
		{"missing flag", []string{"import", "--from", "openapi", "--in", in, "--out", out}, "--report is required"},
		{"repeated flag", []string{"import", "--from", "openapi", "--from", "openapi", "--in", in, "--out", out, "--report", report}, "more than once"},
		{"unknown flag", []string{"import", "--from", "openapi", "--in", in, "--out", out, "--report", report, "--with-trust"}, "usage"},
		{"input too large", []string{"import", "--from", "openapi", "--in", large, "--out", out, "--report", report}, "larger than"},
		{"input is a folder", []string{"import", "--from", "openapi", "--in", folder, "--out", out, "--report", report}, "in file"},
		{"output type", []string{"import", "--from", "openapi", "--in", in, "--out", filepath.Join(dir, "x.txt"), "--report", report}, "unsupported file type"},
		{"output exists", []string{"import", "--from", "openapi", "--in", in, "--out", in, "--report", report}, "already exists"},
	}
	for _, c := range cases {
		code, stderr := run(c.args...)
		if code != openapi.ExitFailed || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, stderr %q, want %q", c.name, code, stderr, c.want)
		}
		for _, path := range []string{out, report, filepath.Join(dir, "x.txt")} {
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("%s: %s was written", c.name, path)
			}
		}
	}
}

// resolve follows a JSON pointer ("#" or "#/a/b") through decoded JSON.
func resolve(root any, pointer string) (any, error) {
	node := root
	if pointer == "#" {
		return node, nil
	}
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "#/"), "/") {
		token = strings.NewReplacer("~1", "/", "~0", "~").Replace(token)
		object, ok := node.(map[string]any)
		if !ok {
			return nil, os.ErrNotExist
		}
		if node, ok = object[token]; !ok {
			return nil, os.ErrNotExist
		}
	}
	return node, nil
}
