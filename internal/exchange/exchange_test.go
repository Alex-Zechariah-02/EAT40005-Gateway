// Tests of the exchange file's ledger: strict reading, the class rules and the bounds on carried values.

package exchange

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// valid is a small exchange file with one item of each class.
const valid = `{
  "openapi": "3.1.0",
  "info": {"title": "shop", "version": "1"},
  "paths": {"/orders": {"get": {"operationId": "list", "security": []}}},
  "x-apiward-migration": {
    "version": 1,
    "source": {"format": "openapi", "format_version": "3.1.0", "sha256": "` + hash + `"},
    "items": [
      {"id": "openapi:operation:GET /orders", "facet": "route", "class": "exact",
       "source": "#/paths/~1orders/get", "target": "#/paths/~1orders/get", "note": "", "accepted": false},
      {"id": "openapi:operation:GET /orders/{id}", "facet": "route", "class": "approximated", "direction": "looser",
       "source": "#/paths/~1orders~1{id}/get", "target": "#/paths/~1orders~1{id}/get", "note": "deeper paths", "accepted": false},
      {"id": "openapi:member:servers", "facet": "upstream", "class": "not_expressible",
       "source": "#/servers", "target": "", "note": "", "accepted": false}
    ],
    "carried": [{"vendor": "openapi", "scope": "api", "path": "#/x-other", "value": {"Free": ["Form", 1]}}],
    "credentials_skipped": [{"type": "api_key", "count": 2}],
    "trust": {"issuers": [{"issuer": "https://login.example.com", "audience": null, "public_key_ids": ["k1"]}]}
  }
}`

const hash = "0000000000000000000000000000000000000000000000000000000000000000"

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSplitValid(t *testing.T) {
	root := decode(t, valid)
	document, ledger, err := Split(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := document[Member]; present {
		t.Fatal("the document still holds the ledger")
	}
	if _, present := root[Member]; !present {
		t.Fatal("Split changed its input")
	}
	if len(ledger.Items) != 3 || ledger.Items[1].Direction != Looser || ledger.Source.Format != "openapi" {
		t.Fatalf("ledger = %+v", ledger)
	}
	if !bytes.Contains(ledger.Carried[0].Value, []byte(`"Free"`)) {
		t.Fatalf("carried value = %s", ledger.Carried[0].Value)
	}
}

// TestExchangeStrict refuses every unknown member, case variant and value
// outside the ledger's rules.
func TestExchangeStrict(t *testing.T) {
	big := `"` + strings.Repeat("a", MaxCarriedValue) + `"`
	many := strings.Repeat(`{"vendor": "openapi", "scope": "api", "path": "#/x", "value": "`+strings.Repeat("a", 60<<10)+`"},`, 18)
	cases := map[string][2]string{
		"unknown ledger member":   {`"version": 1,`, `"version": 1, "extra": true,`},
		"case variant":            {`"version": 1,`, `"Version": 1,`},
		"unknown item member":     {`"note": "", "accepted": false},`, `"note": "", "accepted": false, "why": 1},`},
		"unknown source member":   {`"format_version"`, `"formatVersion"`},
		"unknown trust member":    {`"public_key_ids"`, `"public_keys"`},
		"version 2":               {`"version": 1,`, `"version": 2,`},
		"unknown class":           {`"class": "exact"`, `"class": "same"`},
		"unknown facet":           {`"facet": "route", "class": "exact"`, `"facet": "routing", "class": "exact"`},
		"direction on exact":      {`"class": "exact",`, `"class": "exact", "direction": "stricter",`},
		"no direction":            {`"direction": "looser",`, ``},
		"unknown direction":       {`"direction": "looser",`, `"direction": "wider",`},
		"looser without target":   {`"note": "deeper paths"`, `"note": "deeper paths", "target": ""`},
		"duplicate id":            {`"openapi:member:servers"`, `"openapi:operation:GET /orders"`},
		"id of another format":    {`"openapi:member:servers"`, `"kong-deck:member:servers"`},
		"id without a kind":       {`"openapi:member:servers"`, `"openapi:servers"`},
		"unknown format":          {`"format": "openapi"`, `"format": "unknown"`},
		"bad sha256":              {hash, "xyz"},
		"target not a pointer":    {`"target": "#/paths/~1orders/get", "note": ""`, `"target": "paths", "note": ""`},
		"target on not expressed": {`"target": "", "note": "", "accepted": false}`, `"target": "#/servers", "note": "", "accepted": false}`},
		"negative count":          {`"count": 2`, `"count": -1`},
		"empty credential type":   {`"type": "api_key"`, `"type": ""`},
		"carried value too large": {`"value": {"Free": ["Form", 1]}`, `"value": ` + big},
		"carried total too large": {`"carried": [`, `"carried": [` + many},
		"unknown root extension":  {`"x-apiward-migration": {`, `"x-apiward-other": 1, "x-apiward-migration": {`},
		"accepted when exact":     {`"note": "", "accepted": false},`, `"note": "", "accepted": true},`}, // the first item is exact
		"blocked unknown member":  {`"credentials_skipped"`, `"blocked": [{"provider": "p", "subject": "s", "x": 1}], "credentials_skipped"`},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			text := strings.Replace(valid, change[0], change[1], 1)
			if text == valid {
				t.Fatal("the case changed nothing")
			}
			root := map[string]any{}
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			if err := decoder.Decode(&root); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Split(root); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, _, err := Split(map[string]any{"openapi": "3.1.0"}); err == nil {
		t.Fatal("a file without a ledger was accepted")
	}
}

// TestAttachRoundTrip writes a ledger into a document and reads it back unchanged.
func TestAttachRoundTrip(t *testing.T) {
	document, ledger, err := Split(decode(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	attached, err := Attach(document, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := document[Member]; present {
		t.Fatal("Attach changed its input")
	}
	data, err := json.Marshal(attached)
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := Split(decode(t, string(data)))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(ledger)
	second, _ := json.Marshal(again)
	if !bytes.Equal(first, second) {
		t.Fatalf("ledger changed:\n%s\n%s", first, second)
	}
	ledger.Items[0].Class = "same"
	if _, err := Attach(document, ledger); err == nil {
		t.Fatal("Attach wrote an invalid ledger")
	}
}

// TestEmptyListsPresent writes every list as [], never null.
func TestEmptyListsPresent(t *testing.T) {
	ledger := New(Source{Format: "openapi", FormatVersion: "3.1.0", SHA256: hash})
	attached, err := Attach(map[string]any{}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(attached)
	for _, want := range []string{`"items":[]`, `"carried":[]`, `"credentials_skipped":[]`, `"issuers":[]`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("%s missing in %s", want, data)
		}
	}
	if bytes.Contains(data, []byte(`"blocked"`)) {
		t.Errorf("blocked written without blocked accounts: %s", data)
	}
}
