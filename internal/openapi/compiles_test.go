// The output is accepted by the real policy decoder and compiler, or nothing is produced.

package openapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func TestImportCompiles(t *testing.T) {
	for _, name := range []string{"orders.json", "refusals.json"} {
		out, _, err := Import(fixture(t, name), JSON, testOptions())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		document, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := policy.DecodePolicy(document)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if _, err := compile.Compile(decoded, 1, time.Time{}); err != nil {
			t.Fatalf("%s: compile: %v", name, err)
		}
	}

	// A base that is valid alone but full: the imported operations push it
	// past the compiler's operation limit, so the whole import fails.
	options := testOptions()
	options.Base.Operations = nil
	options.Base.Permits = nil
	for i := range policy.MaxOperations - 1 {
		options.Base.Operations = append(options.Base.Operations, policy.Operation{
			ID: fmt.Sprintf("inventory.op%d", i), API: "inventory", Method: "GET",
			Target: fmt.Sprintf("/inventory/%d", i), Access: "allow", Auth: "required",
		})
	}
	_, _, err := Import(fixture(t, "orders.json"), JSON, options)
	if err == nil || !strings.Contains(err.Error(), "LIMIT_EXCEEDED at operations") {
		t.Fatalf("error %v, want the compiler's LIMIT_EXCEEDED", err)
	}
}

// TestImportSizeAsWritten checks the policy size on the indented form that
// is written, not a compact one, so the gateway can always read the file.
func TestImportSizeAsWritten(t *testing.T) {
	options := testOptions()
	options.Base.Operations, options.Base.Permits = nil, nil
	long := strings.Repeat("x", 200)
	for i := 0; ; i++ {
		compact, _ := json.Marshal(options.Base)
		if len(compact) > policy.MaxPolicyBytes-3000 {
			break
		}
		options.Base.Operations = append(options.Base.Operations, policy.Operation{
			ID: fmt.Sprintf("inventory.op%d", i), API: "inventory", Method: "GET",
			Target: fmt.Sprintf("/inventory/%s/%d", long, i), Access: "allow", Auth: "required",
		})
	}
	spec := []byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"},
		"paths": {"/orders": {"get": {"operationId": "list", "security": []}}}}`)
	_, _, err := Import(spec, JSON, options)
	if err == nil || !strings.Contains(err.Error(), "LIMIT_EXCEEDED at size") {
		t.Fatalf("error %v, want LIMIT_EXCEEDED at size", err)
	}
}
