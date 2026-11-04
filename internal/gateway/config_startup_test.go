// Startup refuses invalid configuration.

package gateway

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyCaddyFile is e's deployment as the retired Caddy-hosted gateway was
// configured: its settings under apps.gateway and a data server under
// apps.http.
func legacyCaddyFile(e *env) []byte {
	return mustJSON(map[string]any{
		"admin": map[string]any{"disabled": true},
		"apps": map[string]any{
			"gateway": section(e.nativeSettings(), "gateway"),
			"http":    map[string]any{"servers": map[string]any{"data": map[string]any{"listen": []string{e.dataAddr}}}},
		},
	})
}

// The native process refuses a configuration it cannot trust before it opens
// anything of the deployment: it exits with status 1 and a message naming the
// problem, no listener opens, no audit file is created and the database is
// left free for another owner. The controller's own checks (the admin
// address, the trust key, the dashboard directory) are refused the same way.
func TestNativeStartupRefusesConfiguration(t *testing.T) {
	privateKey := filepath.Join(t.TempDir(), "private.pem")
	der, _ := x509.MarshalPKCS8PrivateKey(newTestIssuer(2048).key)
	if err := os.WriteFile(privateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		write func(e *env)
		cause string
	}{
		{"malformed JSON", func(e *env) { e.writeNativeFile([]byte(`{"version":1,"gateway":{`)) }, "not valid JSON"},
		{"repeated member", func(e *env) {
			document, _ := json.Marshal(e.nativeSettings())
			e.writeNativeFile([]byte(strings.Replace(string(document), `"version":1`, `"version":1,"version":1`, 1)))
		}, "is repeated"},
		{"trailing content", func(e *env) {
			document, _ := json.Marshal(e.nativeSettings())
			e.writeNativeFile(append(document, []byte(`{"version":1}`)...))
		}, "data after the JSON value"},
		{"unknown setting", func(e *env) {
			s := e.nativeSettings()
			section(s, "gateway")["audit_delete_oldest"] = true
			e.writeNative(s)
		}, `unknown field \"audit_delete_oldest\"`},
		{"wrong type", func(e *env) {
			s := e.nativeSettings()
			section(s, "data")["max_body_bytes"] = "large"
			e.writeNative(s)
		}, "data.max_body_bytes"},
		{"missing database", func(e *env) {
			s := e.nativeSettings()
			delete(section(s, "gateway"), "database")
			e.writeNative(s)
		}, "gateway.database is required"},
		{"unsupported version", func(e *env) {
			s := e.nativeSettings()
			s["version"] = 0
			e.writeNative(s)
		}, "version must be 1"},
		{"legacy Caddy configuration", func(e *env) {
			e.writeNativeFile(legacyCaddyFile(e))
		}, "Caddy configuration"},
		{"wildcard data address", func(e *env) {
			s := e.nativeSettings()
			section(s, "data")["listen"] = "0.0.0.0:" + port(e.dataAddr)
			e.writeNative(s)
		}, "data.listen"},
		{"timeout out of range", func(e *env) {
			s := e.nativeSettings()
			section(s, "upstream")["read_idle_timeout"] = "1h"
			e.writeNative(s)
		}, "upstream.read_idle_timeout"},
		{"wildcard admin address", func(e *env) {
			s := e.nativeSettings()
			section(s, "gateway")["admin_listen"] = "0.0.0.0:" + port(e.admin)
			e.writeNative(s)
		}, "literal loopback address"},
		{"private key as the trust key", func(e *env) {
			s := e.nativeSettings()
			section(s, "gateway")["jwt"] = testTrust(testIssuerName, testAudience, privateKey)
			e.writeNative(s)
		}, "exactly one PEM PUBLIC KEY block"},
		{"missing trust key", func(e *env) {
			s := e.nativeSettings()
			section(s, "gateway")["jwt"] = testTrust(testIssuerName, testAudience, filepath.Join(e.dir, "absent.pem"))
			e.writeNative(s)
		}, "jwt: opening public key"},
		{"dashboard directory without index.html", func(e *env) {
			s := e.nativeSettings()
			section(s, "gateway")["admin_ui"] = t.TempDir()
			e.writeNative(s)
		}, "admin_ui"},
		{"website directory without index.html", func(e *env) {
			s := e.nativeSettings()
			section(s, "data")["website"] = t.TempDir()
			e.writeNative(s)
		}, "data.website"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			tc.write(e)
			e.run(binaries.plain)
			if code := e.waitExit(); code != exitFailed {
				t.Fatalf("exit status %d, want %d\n%s", code, exitFailed, e.output)
			}
			if !strings.Contains(e.output.String(), tc.cause) {
				t.Fatalf("refused for another reason, want %q:\n%s", tc.cause, e.output)
			}
			if strings.Contains(e.output.String(), "PRIVATE KEY") || strings.Contains(e.output.String(), "MII") {
				t.Fatalf("the output discloses key material:\n%s", e.output)
			}
			if dialable(e.dataAddr) || dialable(e.admin) {
				t.Fatal("a listener is open")
			}
			if _, err := os.Stat(e.auditLog); !os.IsNotExist(err) {
				t.Fatalf("an audit file was created: %v", err)
			}
			st, err := openStore(e.database)
			if err != nil {
				t.Fatalf("the database is held: %v", err)
			}
			st.close()
		})
	}

	// The command line names one command and its configuration file; no
	// Caddy command or saved-configuration resume exists.
	for _, args := range [][]string{
		{}, {"run"}, {"start", "--config", "x"}, {"run", "--config"}, {"run", "--resume"},
		{"run", "--config", "x", "--resume"}, {"run", "-r", "--config", "x"}, {"run", "--config", "x", "extra"}, {"run", "--config", "x", "--config", "y"},
		{"version"}, {"list-modules"},
	} {
		e := newEnv(t)
		e.launch(binaries.plain, args)
		if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), "usage: gateway run --config <file>") {
			t.Errorf("%v: exit %d\n%s", args, code, e.output)
		}
	}
	e := newEnv(t)
	e.launch(binaries.plain, []string{"run", "--config", filepath.Join(e.dir, "absent.json")})
	if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), "configuration refused") {
		t.Errorf("missing configuration file: exit %d\n%s", code, e.output)
	}
	if dialable(e.dataAddr) || dialable(e.admin) {
		t.Error("a listener is open after a refused start")
	}
}
