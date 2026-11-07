// Classification without a base policy: per-operation results, no whole-policy limits.

package openapi

import (
	"fmt"
	"strings"
	"testing"
)

func decodeFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	value, err := Decode(fixture(t, name), JSON, MaxSpecBytes)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

// TestClassify reports what the import would do with each operation, named
// as in the description, the same as an import onto a base would.
func TestClassify(t *testing.T) {
	report, err := Classify(decodeFixture(t, "orders.json"), JSON)
	if err != nil {
		t.Fatal(err)
	}
	_, imported, err := Import(fixture(t, "orders.json"), JSON, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if report.API != "" || len(report.Mapped) != len(imported.Mapped) || len(report.Widened) != len(imported.Widened) ||
		len(report.Refused) != len(imported.Refused) || len(report.Changed) != len(imported.Changed) {
		t.Fatalf("classify %+v\nimport %+v", report, imported)
	}
	for i, m := range report.Mapped {
		if m.Operation != strings.TrimPrefix(imported.Mapped[i].Operation, "orders.") {
			t.Errorf("mapped %q, import %q", m.Operation, imported.Mapped[i].Operation)
		}
	}
	for _, c := range report.Changed {
		if strings.Contains(c.Operation, ".") {
			t.Errorf("changed names %q with a prefix", c.Operation)
		}
	}
}

// TestClassifyNoPolicyLimits classifies more operations than one policy may hold.
func TestClassifyNoPolicyLimits(t *testing.T) {
	paths := map[string]any{}
	for i := range 300 {
		paths[fmt.Sprintf("/items/%d", i)] = map[string]any{"get": map[string]any{"operationId": fmt.Sprintf("get%d", i), "security": []any{}}}
	}
	root := map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "t", "version": "1"}, "paths": paths}
	report, err := Classify(root, JSON)
	if err != nil || len(report.Mapped) != 300 {
		t.Fatalf("err %v, mapped %d", err, len(report.Mapped))
	}
}

// TestDecodeLimit refuses a file over the caller's limit, naming it.
func TestDecodeLimit(t *testing.T) {
	data := []byte(`{"openapi": "3.1.0", "pad": "` + strings.Repeat("a", 2<<20) + `"}`)
	if _, err := Decode(data, JSON, 8<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(data, JSON, 1<<20); err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Fatalf("error %v", err)
	}
}
