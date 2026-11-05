package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func TestFetchStoredVersion3IsNotMigrated(t *testing.T) {
	e := newEnv(t)
	policy := e.protectedPolicy()
	seed(t, e.database, policy, seedOptions{})
	legacy := []byte(strings.Replace(string(mustJSON(policy)), `"schema_version":4`, `"schema_version":3`, 1))
	db := inspect(t, e.database)
	if _, err := db.Exec(`UPDATE revision SET document=?,checksum=? WHERE id=1`, legacy, documentChecksum(legacy)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.start(binaries.plain)
	if e.health() != "unavailable" || !strings.Contains(e.unavailableReason(), "SCHEMA_VERSION_UNSUPPORTED") {
		t.Fatal("legacy policy was served")
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 503 {
		t.Fatalf("legacy deployment admitted: %d", code)
	}
	e.assertReceipts(0, 0, 0)
	e.stop()
	if document := queryString(t, e.database, `SELECT document FROM revision WHERE id=1`); document != string(legacy) {
		t.Fatal("legacy policy was silently converted")
	}
}

// Rejecting v4 here prevents a fresh Fetch deployment from activating at all.
func TestFetchPolicyVersion4(t *testing.T) {
	const document = `{
		"schema_version":4,"profile":"core",
		"upstreams":[{"id":"inventory-svc","dial":"127.0.0.1:10501"}],
		"apis":[{"id":"inventory","upstream":"inventory-svc"}],
		"operations":[{"id":"inventory.search","api":"inventory","method":"GET",
			"target":"/inventory/search","access":"allow","auth":"none",
			"rate":{"algorithm":"sliding-window","count":60,"window_seconds":60,"key":"client_address"}}],
		"roles":[{"id":"CUSTOMER"}],"permits":[]
	}`
	policy, err := policy.DecodePolicy([]byte(document))
	if err != nil {
		t.Fatalf("Fetch v4 policy must decode: %v", err)
	}
	generation, err := compile.Compile(policy, 1, time.Time{})
	if err != nil {
		t.Fatalf("public client-address rate must compile: %v", err)
	}
	_, operation := generation.Match("/inventory/search", "GET")
	if operation == nil || !operation.Public() || !operation.Permitted() || operation.Rate() == nil {
		t.Fatalf("compiled public search lost its controls: %+v", operation)
	}
}
