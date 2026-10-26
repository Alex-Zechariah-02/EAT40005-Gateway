package gateway

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configDocument is a complete native configuration whose paths point into
// dir; a test changes it with change before encoding it.
func configDocument(t *testing.T, dir string, change func(settings map[string]any)) []byte {
	t.Helper()
	settings := map[string]any{
		"version": 1,
		"gateway": map[string]any{
			"database":     filepath.Join(dir, "state.db"),
			"audit_log":    filepath.Join(dir, "audit.log"),
			"admin_listen": "127.0.0.1:9440",
			"jwt":          map[string]any{"issuer": "issuer", "audience": "gateway", "public_key_file": filepath.Join(dir, "issuer.pem")},
		},
		"data": map[string]any{"listen": "127.0.0.2:9480"},
	}
	if change != nil {
		change(settings)
	}
	document, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func parseConfig(document []byte) (nativeConfig, error) {
	settings, err := decodeNativeFile(document)
	if err != nil {
		return nativeConfig{}, err
	}
	return settings.check()
}

// A minimal file takes the legacy values for every bound it omits, and every
// written setting is taken as written.
func TestNativeConfigSettings(t *testing.T) {
	dir := t.TempDir()
	cfg, err := parseConfig(configDocument(t, dir, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := nativeConfig{
		controller: Config{
			Database:             filepath.Join(dir, "state.db"),
			AuditLog:             filepath.Join(dir, "audit.log"),
			AdminListen:          "127.0.0.1:9440",
			AdminIdleTimeout:     15 * time.Minute,
			AdminSessionLifetime: 8 * time.Hour,
			AuditTimeout:         2 * time.Second,
			AuditMaxBytes:        64 << 20,
			AuditMaxFiles:        8,
			JWT:                  TrustConfig{Issuer: "issuer", Audience: "gateway", PublicKeyFile: filepath.Join(dir, "issuer.pem")},
		},
		listen:            "127.0.0.2:9480",
		maxBodyBytes:      16 << 10,
		headerLimit:       16 << 10,
		readHeaderTimeout: 5 * time.Second,
		readTimeout:       10 * time.Second,
		writeTimeout:      30 * time.Second,
		idleTimeout:       5 * time.Minute,
		upstream:          upstreamTimeouts{dial: 2 * time.Second, responseHeader: 5 * time.Second, readIdle: 5 * time.Second},
	}
	if cfg != want {
		t.Fatalf("defaults\n got %+v\nwant %+v", cfg, want)
	}

	site := writeDashboard(t)
	cfg, err = parseConfig(configDocument(t, dir, func(s map[string]any) {
		maps.Copy(s["gateway"].(map[string]any), map[string]any{
			"admin_idle_timeout": "10m", "admin_session_lifetime": "1h", "audit_timeout": "1s",
			"audit_max_bytes": 1 << 20, "audit_max_files": 4, "admin_ui": site,
		})
		maps.Copy(s["data"].(map[string]any), map[string]any{
			"max_body_bytes": 2048, "max_header_bytes": 32768, "read_header_timeout": "2s", "read_timeout": "3s",
			"write_timeout": "4s", "idle_timeout": "1m", "website": site,
		})
		s["upstream"] = map[string]any{"dial_timeout": "1s", "response_header_timeout": "3s", "read_idle_timeout": "2m"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.controller
	if c.AdminIdleTimeout != 10*time.Minute || c.AdminSessionLifetime != time.Hour || c.AuditTimeout != time.Second ||
		c.AuditMaxBytes != 1<<20 || c.AuditMaxFiles != 4 || c.AdminUI != site || cfg.website == nil ||
		cfg.maxBodyBytes != 2048 || cfg.headerLimit != 32768 || cfg.readHeaderTimeout != 2*time.Second ||
		cfg.readTimeout != 3*time.Second || cfg.writeTimeout != 4*time.Second || cfg.idleTimeout != time.Minute ||
		cfg.upstream != (upstreamTimeouts{dial: time.Second, responseHeader: 3 * time.Second, readIdle: 2 * time.Minute}) {
		t.Fatalf("written settings not taken: %+v", cfg)
	}
}

// Everything that is not exactly a valid native configuration is refused
// with a message that names the problem: no setting falls back to a default
// when it is written with an invalid value.
func TestNativeConfigRefusals(t *testing.T) {
	dir := t.TempDir()
	set := func(object, name string, value any) func(map[string]any) {
		return func(s map[string]any) { section(s, object)[name] = value }
	}
	remove := func(object, name string) func(map[string]any) {
		return func(s map[string]any) { delete(section(s, object), name) }
	}
	valid := string(configDocument(t, dir, nil))
	cases := []struct {
		name     string
		document string
		cause    string
	}{
		{"empty file", "", "not valid JSON"},
		{"malformed JSON", `{"version": 1,`, "not valid JSON"},
		{"not an object", `[1]`, "not a JSON object"},
		{"null", `null`, "not a JSON object"},
		{"trailing content", valid + ` {}`, "data after the JSON value"},
		{"trailing garbage", valid + `x`, "data after the JSON value"},
		{"repeated member", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), `member "version" is repeated`},
		{"repeated nested member", strings.Replace(valid, `"listen":`, `"listen":"127.0.0.3:1","listen":`, 1), `member "listen" is repeated`},
		{"member in another case", strings.Replace(valid, `"version":1`, `"Version":1`, 1), "lower-case"},
		{"no version", string(configDocument(t, dir, func(s map[string]any) { delete(s, "version") })), "version must be 1"},
		{"another version", string(configDocument(t, dir, func(s map[string]any) { s["version"] = 2 })), "version must be 1"},
		{"version as a string", string(configDocument(t, dir, func(s map[string]any) { s["version"] = "1" })), "version must be 1"},
		{"legacy Caddy file", `{"admin":{"disabled":true},"apps":{"gateway":{}}}`, "Caddy configuration"},
		{"unknown top-level member", string(configDocument(t, dir, func(s map[string]any) { s["admin"] = map[string]any{} })), `unknown field "admin"`},
		{"unknown setting", string(configDocument(t, dir, set("data", "listen_tls", true))), `unknown field "listen_tls"`},
		{"unknown trust setting", string(configDocument(t, dir, func(s map[string]any) {
			section(s, "gateway")["jwt"].(map[string]any)["algorithms"] = []string{"none"}
		})), `unknown field "algorithms"`},
		{"wrong type", string(configDocument(t, dir, set("data", "max_body_bytes", "1024"))), "data.max_body_bytes: a JSON string is not a valid value"},
		{"fractional number", string(configDocument(t, dir, set("gateway", "audit_max_files", 2.5))), "gateway.audit_max_files"},
		{"duration as a number", string(configDocument(t, dir, set("data", "read_timeout", 10))), "data.read_timeout"},
		{"no database", string(configDocument(t, dir, remove("gateway", "database"))), "gateway.database is required"},
		{"no audit log", string(configDocument(t, dir, remove("gateway", "audit_log"))), "gateway.audit_log is required"},
		{"no admin address", string(configDocument(t, dir, remove("gateway", "admin_listen"))), "gateway.admin_listen is required"},
		{"no trust key", string(configDocument(t, dir, func(s map[string]any) {
			delete(section(s, "gateway")["jwt"].(map[string]any), "public_key_file")
		})), "gateway.jwt.public_key_file is required"},
		{"no trust configuration", string(configDocument(t, dir, remove("gateway", "jwt"))), "gateway.jwt.issuer is required"},
		{"no data section", string(configDocument(t, dir, func(s map[string]any) { delete(s, "data") })), "data.listen is required"},
		{"relative database", string(configDocument(t, dir, set("gateway", "database", "state.db"))), "gateway.database must be an absolute path"},
		{"relative audit log", string(configDocument(t, dir, set("gateway", "audit_log", `.\audit.log`))), "gateway.audit_log must be an absolute path"},
		{"empty dashboard path", string(configDocument(t, dir, set("gateway", "admin_ui", ""))), "gateway.admin_ui must be an absolute path"},
		{"host name data address", string(configDocument(t, dir, set("data", "listen", "localhost:9480"))), "data.listen"},
		{"wildcard data address", string(configDocument(t, dir, set("data", "listen", "0.0.0.0:9480"))), "data.listen"},
		{"wildcard IPv6 data address", string(configDocument(t, dir, set("data", "listen", "[::]:9480"))), "data.listen"},
		{"IPv4-mapped wildcard data address", string(configDocument(t, dir, set("data", "listen", "[::ffff:0.0.0.0]:9480"))), "data.listen"},
		{"data address without a port", string(configDocument(t, dir, set("data", "listen", "127.0.0.2"))), "data.listen"},
		{"data address with port zero", string(configDocument(t, dir, set("data", "listen", "127.0.0.2:0"))), "data.listen"},
		{"multicast data address", string(configDocument(t, dir, set("data", "listen", "224.0.0.1:9480"))), "data.listen"},
		{"data and admin on one address", string(configDocument(t, dir, set("data", "listen", "127.0.0.1:9440"))), "must be different addresses"},
		{"data and admin on one IPv4-mapped address", string(configDocument(t, dir, set("data", "listen", "[::ffff:127.0.0.1]:9440"))), "must be different addresses"},
		{"invalid duration", string(configDocument(t, dir, set("upstream", "dial_timeout", "2 seconds"))), `upstream.dial_timeout: "2 seconds" is not a duration`},
		{"day duration", string(configDocument(t, dir, set("gateway", "admin_session_lifetime", "1d"))), "gateway.admin_session_lifetime"},
		{"zero timeout", string(configDocument(t, dir, set("data", "write_timeout", "0s"))), "data.write_timeout must be 1s to 10m0s"},
		{"negative timeout", string(configDocument(t, dir, set("upstream", "read_idle_timeout", "-5s"))), "upstream.read_idle_timeout must be 1s to 5m0s"},
		{"timeout too long", string(configDocument(t, dir, set("upstream", "response_header_timeout", "6m"))), "upstream.response_header_timeout must be 1s to 5m0s"},
		{"idle timeout too long", string(configDocument(t, dir, set("data", "idle_timeout", "11m"))), "data.idle_timeout must be 1s to 10m0s"},
		{"admission wait too short", string(configDocument(t, dir, set("gateway", "audit_timeout", "10ms"))), "gateway.audit_timeout must be 100ms to 30s"},
		{"session idle too short", string(configDocument(t, dir, set("gateway", "admin_idle_timeout", "500ms"))), "gateway.admin_idle_timeout must be 1s to 24h0m0s"},
		{"header before the whole request", string(configDocument(t, dir, func(s map[string]any) {
			section(s, "data")["read_header_timeout"] = "20s"
			section(s, "data")["read_timeout"] = "10s"
		})), "data.read_header_timeout must not exceed data.read_timeout"},
		{"zero body ceiling", string(configDocument(t, dir, set("data", "max_body_bytes", 0))), "data.max_body_bytes must be 1 to 1048576"},
		{"body ceiling above the compiler's", string(configDocument(t, dir, set("data", "max_body_bytes", 1<<20+1))), "data.max_body_bytes must be 1 to 1048576"},
		{"header limit too small", string(configDocument(t, dir, set("data", "max_header_bytes", 4096))), "data.max_header_bytes must be 8192 to 1048576"},
		{"zero audit file size", string(configDocument(t, dir, set("gateway", "audit_max_bytes", 0))), "gateway.audit_max_bytes must be"},
		{"one audit file", string(configDocument(t, dir, set("gateway", "audit_max_files", 1))), "gateway.audit_max_files must be 2 to 1000"},
		{"website without an index page", string(configDocument(t, dir, set("data", "website", t.TempDir()))), "data.website"},
		{"missing website directory", string(configDocument(t, dir, set("data", "website", filepath.Join(dir, "absent")))), "data.website"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.document))
			if err == nil || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("got %v, want an error with %q", err, tc.cause)
			}
		})
	}

	t.Run("file larger than the bound", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "large.json")
		padded := strings.Replace(valid, `"version":1`, `"version":1`+strings.Repeat(" ", maxConfigBytes), 1)
		if err := os.WriteFile(path, []byte(padded), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadNativeConfig(path); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("got %v", err)
		}
	})
}

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
			section(s, "gateway")["jwt"].(map[string]any)["public_key_file"] = privateKey
			e.writeNative(s)
		}, "exactly one PEM PUBLIC KEY block"},
		{"missing trust key", func(e *env) {
			s := e.nativeSettings()
			section(s, "gateway")["jwt"].(map[string]any)["public_key_file"] = filepath.Join(e.dir, "absent.pem")
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
