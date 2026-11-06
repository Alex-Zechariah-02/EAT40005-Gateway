package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	policydoc "github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// TestBaselineRoundTrip exports each API of the demo baseline as OpenAPI and
// imports it back onto the same baseline; the canonical policy must not change.
func TestBaselineRoundTrip(t *testing.T) {
	baseline, err := policydoc.DecodePolicy(indented(baselinePolicy()))
	if err != nil {
		t.Fatal(err)
	}
	want, err := policydoc.Encode(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range baseline.APIs {
		document, _, err := openapi.Export(baseline, openapi.ExportOptions{API: api.ID})
		if err != nil {
			t.Fatalf("%s: %v", api.ID, err)
		}
		spec, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		result, report, err := openapi.Import(spec, openapi.JSON, openapi.Options{API: api.ID, Upstream: api.Upstream,
			Base: baseline, MaxBodyBytes: openapi.DefaultBodyBytes, WithPermits: true})
		if err != nil {
			t.Fatalf("%s: %v", api.ID, err)
		}
		got, err := policydoc.Encode(result)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: round trip changed the baseline:\n%s", api.ID, got)
		}
		if report.Exit() != openapi.ExitComplete {
			t.Fatalf("%s: import report %+v", api.ID, report)
		}
	}
}
