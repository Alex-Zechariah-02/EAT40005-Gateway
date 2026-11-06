package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalOrder(t *testing.T) {
	shuffled := Policy{
		SchemaVersion: SchemaVersion, Profile: ProfileCore,
		Upstreams: []Upstream{{ID: "b", Dial: "127.0.0.1:2"}, {ID: "a", Dial: "127.0.0.1:1"}},
		APIs:      []API{{ID: "y", Upstream: "b"}, {ID: "x", Upstream: "a"}},
		Operations: []Operation{
			{ID: "y.patch", API: "y", Method: "PATCH", Target: "/y"},
			{ID: "y.get", API: "y", Method: "GET", Target: "/y"},
			{ID: "x.post", API: "x", Method: "POST", Target: "/x/*",
				Body: &BodyRule{MaxBytes: 10, Schema: json.RawMessage(`{"type":"object","required":["b","a"],"additionalProperties":false,"properties":{"b":{"type":"string"},"a":{"type":"integer","maximum":1e3}}}`)}},
			{ID: "y.delete", API: "y", Method: "DELETE", Target: "/y"},
			{ID: "x.get", API: "x", Method: "GET", Target: "/x"},
		},
		Roles:   []Role{{ID: "B"}, {ID: "A"}},
		Permits: []Permit{{Role: "B", Operation: "y.get"}, {Role: "A", Operation: "y.get"}, {Role: "A", Operation: "x.get"}},
	}
	original, _ := json.Marshal(shuffled)
	got, err := Encode(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := json.Marshal(shuffled); string(after) != string(original) {
		t.Fatal("Encode changed its input")
	}
	var decoded Policy
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, op := range decoded.Operations {
		order = append(order, op.ID)
	}
	if want := "x.get x.post y.get y.delete y.patch"; strings.Join(order, " ") != want {
		t.Fatalf("operations %v, want %s", order, want)
	}
	if decoded.Upstreams[0].ID != "a" || decoded.APIs[0].ID != "x" || decoded.Roles[0].ID != "A" {
		t.Fatalf("upstreams, APIs or roles not sorted: %s", got)
	}
	permits := decoded.Permits
	if permits[0] != (Permit{"A", "x.get"}) || permits[1] != (Permit{"A", "y.get"}) || permits[2] != (Permit{"B", "y.get"}) {
		t.Fatalf("permits %v", permits)
	}
	wantSchema := `{"additionalProperties":false,"properties":{"a":{"maximum":1e3,"type":"integer"},"b":{"type":"string"}},"required":["a","b"],"type":"object"}`
	if compact := compactJSON(t, decoded.Operations[1].Body.Schema); compact != wantSchema {
		t.Fatalf("schema %s, want %s", compact, wantSchema)
	}
	if !strings.HasSuffix(string(got), "}\n") || !strings.Contains(string(got), "\n  \"profile\": \"core\",\n") {
		t.Fatalf("not 2-space JSON with a trailing newline:\n%s", got)
	}
	again, _ := Encode(decoded)
	if string(again) != string(got) {
		t.Fatal("Encode is not stable on its own output")
	}
}

func TestCanonicalEmptyLists(t *testing.T) {
	got, err := Encode(Policy{SchemaVersion: SchemaVersion, Profile: ProfileCore})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "null") {
		t.Fatalf("empty lists encoded as null:\n%s", got)
	}
}

func TestCanonicalKeepsEnumData(t *testing.T) {
	schema := `{"type":"array","items":{"type":"object","required":["z","y"],"additionalProperties":true,"enum":[{"required":["b","a"]}]}}`
	got, err := Encode(Policy{Operations: []Operation{{ID: "a", Body: &BodyRule{MaxBytes: 1, Schema: json.RawMessage(schema)}}}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Policy
	json.Unmarshal(got, &decoded)
	want := `{"items":{"additionalProperties":true,"enum":[{"required":["b","a"]}],"required":["y","z"],"type":"object"},"type":"array"}`
	if compact := compactJSON(t, decoded.Operations[0].Body.Schema); compact != want {
		t.Fatalf("schema %s, want %s", compact, want)
	}
}

func TestCanonicalRejectsBadSchema(t *testing.T) {
	bad := Policy{Operations: []Operation{{ID: "a", Headers: json.RawMessage(`{"a":1,"a":2}`)}}}
	if _, err := Encode(bad); err == nil {
		t.Fatal("a schema with a repeated member was encoded")
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	value, reason := DecodeJSON(raw)
	if reason != "" {
		t.Fatal(reason)
	}
	out, _ := json.Marshal(value)
	return string(out)
}
