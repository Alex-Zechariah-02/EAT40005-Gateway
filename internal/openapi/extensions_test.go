// Import of the x-apiward-* members: permits only with the flag, every invalid member refused.

package openapi

import (
	"slices"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// described is a description with one signed-in operation carrying members.
func described(members string) []byte {
	return []byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"},
  "components": {"securitySchemes": {"apiward": {"type": "http", "scheme": "bearer"}}},
  "paths": {"/orders/{rest}": {"post": {"operationId": "create", "security": [{"apiward": []}],
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "additionalProperties": false}}}}` + members + `}}}}`)
}

func TestImportPermitsFlag(t *testing.T) {
	spec := described(`, "x-apiward-target": "/orders/*", "x-apiward-roles": ["auditor", "customer"]`)
	t.Run("without the flag", func(t *testing.T) {
		result, report, err := Import(spec, JSON, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(result.Permits, func(p policy.Permit) bool { return p.Operation == "orders.create" }) {
			t.Fatal("permits written without --with-permits")
		}
		if len(result.Roles) != 1 || !slices.Contains(report.NeedsPermissions, "orders.create") {
			t.Fatalf("roles %v, needs %v", result.Roles, report.NeedsPermissions)
		}
	})
	t.Run("with the flag", func(t *testing.T) {
		options := testOptions()
		options.WithPermits = true
		result, report, err := Import(spec, JSON, options)
		if err != nil {
			t.Fatal(err)
		}
		var granted []string
		for _, p := range result.Permits {
			if p.Operation == "orders.create" {
				granted = append(granted, p.Role)
			}
		}
		if strings.Join(granted, ",") != "auditor,customer" || len(result.Roles) != 2 || result.Roles[1].ID != "auditor" {
			t.Fatalf("permits %v, roles %v", granted, result.Roles)
		}
		if len(report.NeedsPermissions) != 0 || len(report.Widened) != 0 || report.Exit() != ExitComplete {
			t.Fatalf("report %+v", report)
		}
	})
}

func TestImportExtensions(t *testing.T) {
	permitted := testOptions()
	permitted.WithPermits = true
	cases := []struct{ name, members, field string }{
		{"unknown member", `, "x-apiward-owner": "me"`, "x-apiward-owner"},
		{"target differs", `, "x-apiward-target": "/other/*"`, "x-apiward-target"},
		{"access value", `, "x-apiward-access": "maybe"`, "x-apiward-access"},
		{"rate member", `, "x-apiward-rate": {"algorithm": "sliding-window", "count": 1, "window_seconds": 1, "key": "subject", "burst": 2}`, "x-apiward-rate"},
		{"rate case", `, "x-apiward-rate": {"Count": 1, "algorithm": "sliding-window", "window_seconds": 1, "key": "subject"}`, "x-apiward-rate"},
		{"rate value", `, "x-apiward-rate": {"algorithm": "fixed", "count": 1, "window_seconds": 1, "key": "subject"}`, "rate.algorithm"},
		{"max bytes", `, "x-apiward-max-bytes": 1048577`, "x-apiward-max-bytes"},
		{"max bytes fraction", `, "x-apiward-max-bytes": 1.5`, "x-apiward-max-bytes"},
		{"roles type", `, "x-apiward-roles": "customer"`, "x-apiward-roles"},
		{"roles repeated", `, "x-apiward-roles": ["a", "a"]`, "x-apiward-roles"},
		{"roles invalid", `, "x-apiward-roles": ["a b"]`, "x-apiward-roles"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, report, err := Import(described(c.members), JSON, permitted)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Refused) != 1 || report.Refused[0].Field != c.field {
				t.Fatalf("refused %+v, want field %s", report.Refused, c.field)
			}
		})
	}
	t.Run("max bytes needs a body", func(t *testing.T) {
		spec := []byte(`{"openapi": "3.1.0", "paths": {"/a": {"get": {"operationId": "a", "security": [], "x-apiward-max-bytes": 10}}}}`)
		_, report, err := Import(spec, JSON, permitted)
		if err != nil || len(report.Refused) != 1 || report.Refused[0].Code != CodeExtension {
			t.Fatalf("report %+v, error %v", report, err)
		}
	})
	t.Run("values applied", func(t *testing.T) {
		members := `, "x-apiward-access": "deny", "x-apiward-max-bytes": 77, "x-apiward-rate": {"algorithm": "sliding-window", "count": 5, "window_seconds": 9, "key": "subject"}`
		result, report, err := Import(described(members), JSON, permitted)
		if err != nil {
			t.Fatal(err)
		}
		op := result.Operations[len(result.Operations)-1]
		if op.Access != "deny" || op.Body.MaxBytes != 77 || *op.Rate != (policy.RateRule{Algorithm: "sliding-window", Count: 5, WindowSeconds: 9, Key: "subject"}) {
			t.Fatalf("operation %+v", op)
		}
		if len(report.Widened) != 1 || slices.ContainsFunc(report.Assumed, func(a Assumed) bool { return a.Field == "body.max_bytes" }) {
			t.Fatalf("report %+v: the path without x-apiward-target is a widening; a stated size is not assumed", report)
		}
	})
}
