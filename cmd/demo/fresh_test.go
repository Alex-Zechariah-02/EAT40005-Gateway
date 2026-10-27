package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFreshReplacesOnlyIdentifiedEarlierNativeState(t *testing.T) {
	for _, valid := range []bool{true, false} {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, configFile), []byte(`{"version":1}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fixture.secret"), []byte("owned old demo secret"), 0600); err != nil {
			t.Fatal(err)
		}
		config, _ := demoDigest(filepath.Join(dir, configFile))
		secret, _ := demoDigest(filepath.Join(dir, "fixture.secret"))
		if !valid {
			config = "different"
		}
		identity := map[string]string{"format": "gateway-demo-1", "directory": dir, "config_sha256": config, "fixture_secret_sha256": secret}
		if err := os.WriteFile(filepath.Join(dir, identityFile), indented(identity), 0600); err != nil {
			t.Fatal(err)
		}
		err := removeState(dir)
		if valid && err != nil {
			t.Errorf("fresh refused identified earlier native state: %v", err)
		}
		_, statErr := os.Stat(dir)
		if valid && !os.IsNotExist(statErr) {
			t.Error("fresh retained its earlier state")
		}
		if !valid && (err == nil || statErr != nil) {
			t.Error("fresh removed a mismatched directory")
		}
	}
}
