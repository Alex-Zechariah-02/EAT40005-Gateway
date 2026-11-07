// Exports checked offline against Kong's published decK file schema (testdata, Apache-2.0, unmodified).

package kong

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// deckSchema compiles Kong's decK schema from testdata.
func deckSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "kong_json_schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	document, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("kong_json_schema.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("kong_json_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestExportSchema validates the writer's output for restored, edited and
// generated routes, with and without the flags, against Kong's schema.
func TestExportSchema(t *testing.T) {
	schema := deckSchema(t)
	document, ledger := stored(t, read(t, "clean.yaml"))
	edited, editedLedger := stored(t, read(t, "clean.yaml"))
	object(object(object(object(edited["paths"])["/orders"])["get"])["x-apiward-rate"])["count"] = int64(5)
	openapiLedger := exchange.New(exchange.Source{Format: "openapi", FormatVersion: "3.1.0", SHA256: strings.Repeat("0", 64)})
	cases := []struct {
		name     string
		document map[string]any
		ledger   *exchange.Ledger
		options  Options
	}{
		{"restored", document, ledger, Options{}},
		{"restored with flags", document, ledger, Options{WithUpstream: true, WithTrust: true}},
		{"edited", edited, editedLedger, Options{WithTrust: true}},
		{"other source", object(read(t, "shop.yaml").Document), openapiLedger, Options{}},
	}
	for _, c := range cases {
		out, _, err := Write(c.document, c.ledger, c.options)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Errorf("%s: not a valid decK file: %v", c.name, err)
		}
	}
}

// TestSchemaRejects makes sure the schema check can fail: a member decK
// does not know is refused.
func TestSchemaRejects(t *testing.T) {
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"_format_version": "3.0", "services": [{"name": "a", "colour": "red"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if deckSchema(t).Validate(instance) == nil {
		t.Fatal("an unknown service member passed the schema")
	}
}
