// Integration harness configuration and policy files.

package gateway

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func (e *env) writeConfig(opts configOptions) {
	if opts.headerTimeout == "" {
		opts.headerTimeout = "5s"
	}
	if opts.maxBodyBytes == 0 {
		opts.maxBodyBytes = 16384
	}
	if opts.jwt == nil {
		opts.jwt = testTrust(testIssuerName, testAudience, e.publicKey)
	}
	gateway := map[string]any{
		"database":       e.database,
		"audit_log":      e.auditLog,
		"admin_listen":   e.admin,
		"metrics_listen": e.metrics,
		"jwt":            opts.jwt,
	}
	maps.Copy(gateway, opts.audit)
	maps.Copy(gateway, opts.admin)
	settings := e.nativeSettings()
	settings["gateway"] = gateway
	section(settings, "data")["max_body_bytes"] = opts.maxBodyBytes
	section(settings, "upstream")["response_header_timeout"] = opts.headerTimeout
	e.writeNative(settings)
}

// nativeSettings is the configuration of e's deployment, as the JSON object
// writeNative writes. A test may change it first.
func (e *env) nativeSettings() map[string]any {
	return map[string]any{
		"version": nativeConfigVersion,
		"gateway": map[string]any{
			"database":       e.database,
			"audit_log":      e.auditLog,
			"admin_listen":   e.admin,
			"metrics_listen": e.metrics,
			"jwt":            testTrust(testIssuerName, testAudience, e.publicKey),
		},
		"data": map[string]any{"listen": e.dataAddr},
	}
}

// section returns the named object of native settings.
func section(settings map[string]any, name string) map[string]any {
	s, ok := settings[name].(map[string]any)
	if !ok {
		s = map[string]any{}
		settings[name] = s
	}
	return s
}

// writeNative writes settings as e's configuration file.
func (e *env) writeNative(settings map[string]any) {
	e.t.Helper()
	document, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		e.t.Fatal(err)
	}
	e.writeNativeFile(document)
}

// writeNativeFile writes content, exactly as given, as e's configuration
// file.
func (e *env) writeNativeFile(content []byte) {
	e.t.Helper()
	e.config = filepath.Join(e.dir, "gateway.json")
	if err := os.WriteFile(e.config, content, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// policy builds the standard test policy: /a/* and POST /a/item go to the
// given upstream, /d is denied, and /b/* exists only when withB is set. Every
// operation is public, so routing behaviour is tested without tokens.
func (e *env) policy(aTarget *receiver, withB bool) policy.Policy {
	p := policy.Policy{
		SchemaVersion: policy.SchemaVersion,
		Profile:       policy.ProfileCore,
		Upstreams:     []policy.Upstream{{ID: "A", Dial: e.a.addr}, {ID: "B", Dial: e.b.addr}},
		APIs:          []policy.API{{ID: "a", Upstream: aTarget.name}, {ID: "b", Upstream: "B"}, {ID: "d", Upstream: "A"}},
		Operations: []policy.Operation{
			{ID: "read-a", API: "a", Method: "GET", Target: "/a/*", Access: "allow", Auth: "none"},
			{ID: "write-a", API: "a", Method: "POST", Target: "/a/item", Access: "allow", Auth: "none", Body: jsonBody(1024, anyObject)},
			{ID: "denied", API: "d", Method: "GET", Target: "/d", Access: "deny", Auth: "none"},
		},
	}
	if withB {
		p.Operations = append(p.Operations, policy.Operation{ID: "read-b", API: "b", Method: "GET", Target: "/b/*", Access: "allow", Auth: "none"})
	}
	return p
}
