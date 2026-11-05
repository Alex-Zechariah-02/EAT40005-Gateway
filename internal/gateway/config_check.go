// Configuration file checks and setting ranges.

package gateway

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/demoports"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// check resolves defaults and checks every setting the controller does not
// check itself.
func (f nativeFile) check() (nativeConfig, error) {
	g, d, u := f.Gateway, f.Data, f.Upstream
	cfg := nativeConfig{
		controller: Config{
			Database:    g.Database,
			AuditLog:    g.AuditLog,
			AdminListen: g.AdminListen,
			JWT:         g.JWT,
		},
		listen: d.Listen,
	}
	for _, required := range []struct{ name, value string }{
		{"gateway.database", g.Database},
		{"gateway.audit_log", g.AuditLog},
		{"gateway.admin_listen", g.AdminListen},
		{"gateway.jwt.issuer", g.JWT.Issuer},
		{"gateway.jwt.audience", g.JWT.Audience},
		{"data.listen", d.Listen},
	} {
		if required.value == "" {
			return nativeConfig{}, fmt.Errorf("%s is required", required.name)
		}
	}
	if len(g.JWT.Keys) == 0 || len(g.JWT.Keys) > maxVerificationKeys {
		return nativeConfig{}, fmt.Errorf("gateway.jwt.keys must contain 1 to %d keys", maxVerificationKeys)
	}
	for i, key := range g.JWT.Keys {
		if key.PublicKeyFile == "" || !filepath.IsAbs(key.PublicKeyFile) {
			return nativeConfig{}, fmt.Errorf("gateway.jwt.keys[%d].public_key_file must be an absolute path", i)
		}
	}
	// A relative path would depend on the directory the gateway is started
	// in.
	for _, file := range []struct {
		name  string
		value *string
	}{
		{"gateway.database", &g.Database},
		{"gateway.audit_log", &g.AuditLog},
		{"gateway.admin_ui", g.AdminUI},
		{"data.website", d.Website},
	} {
		if file.value != nil && !filepath.IsAbs(*file.value) {
			return nativeConfig{}, fmt.Errorf("%s must be an absolute path", file.name)
		}
	}
	if err := checkDataListen(d.Listen, g.AdminListen); err != nil {
		return nativeConfig{}, err
	}
	metrics, err := demoports.Offset("127.0.0.1:9450")
	if err != nil {
		return nativeConfig{}, err
	}
	if g.MetricsListen != nil {
		metrics = *g.MetricsListen
	}
	address, err := netip.ParseAddrPort(metrics)
	if err != nil || !address.Addr().IsLoopback() || address.Port() == 0 || metrics == d.Listen || metrics == g.AdminListen {
		return nativeConfig{}, errors.New("gateway.metrics_listen must be a separate loopback address with a nonzero port")
	}
	cfg.metricsListen = metrics

	durations := []struct {
		name      string
		value     *string
		def       time.Duration
		low, high time.Duration
		into      *time.Duration
	}{
		{"gateway.admin_idle_timeout", g.AdminIdleTimeout, defaultIdle, minSessionSetting, maxSessionSetting, &cfg.controller.AdminIdleTimeout},
		{"gateway.admin_session_lifetime", g.AdminSessionLifetime, defaultLifetime, minSessionSetting, maxSessionSetting, &cfg.controller.AdminSessionLifetime},
		{"gateway.audit_timeout", g.AuditTimeout, defaultAuditWait, minAuditTimeout, maxAuditTimeout, &cfg.controller.AuditTimeout},
		{"data.read_header_timeout", d.ReadHeaderTimeout, defaultReadHeader, minServerTimeout, maxServerTimeout, &cfg.readHeaderTimeout},
		{"data.read_timeout", d.ReadTimeout, defaultRead, minServerTimeout, maxServerTimeout, &cfg.readTimeout},
		{"data.write_timeout", d.WriteTimeout, defaultWrite, minServerTimeout, maxServerTimeout, &cfg.writeTimeout},
		{"data.idle_timeout", d.IdleTimeout, defaultIdleKeepAlive, minServerTimeout, maxServerTimeout, &cfg.idleTimeout},
		{"upstream.dial_timeout", u.DialTimeout, defaultDial, minUpstreamTimeout, maxUpstreamTimeout, &cfg.upstream.Dial},
		{"upstream.response_header_timeout", u.ResponseHeaderTimeout, defaultUpstreamWait, minUpstreamTimeout, maxUpstreamTimeout, &cfg.upstream.ResponseHeader},
		{"upstream.read_idle_timeout", u.ReadIdleTimeout, defaultUpstreamWait, minUpstreamTimeout, maxUpstreamTimeout, &cfg.upstream.ReadIdle},
	}
	for _, s := range durations {
		if *s.into, err = durationSetting(s.name, s.value, s.def, s.low, s.high); err != nil {
			return nativeConfig{}, err
		}
	}
	// The header must arrive within the time allowed for the whole request.
	if cfg.readHeaderTimeout > cfg.readTimeout {
		return nativeConfig{}, errors.New("data.read_header_timeout must not exceed data.read_timeout")
	}

	if cfg.controller.AuditMaxBytes, err = numberSetting("gateway.audit_max_bytes", g.AuditMaxBytes, defaultAuditBytes, minAuditBytes, maxAuditBytes); err != nil {
		return nativeConfig{}, err
	}
	if cfg.controller.AuditMaxFiles, err = numberSetting("gateway.audit_max_files", g.AuditMaxFiles, defaultAuditFiles, 2, maxAuditFiles); err != nil {
		return nativeConfig{}, err
	}
	if cfg.maxBodyBytes, err = numberSetting("data.max_body_bytes", d.MaxBodyBytes, defaultMaxBodyBytes, 1, policy.MaxBodyLimit); err != nil {
		return nativeConfig{}, err
	}
	if cfg.headerLimit, err = numberSetting("data.max_header_bytes", d.MaxHeaderBytes, defaultHeaderBytes, minHeaderBytes, maxHeaderBytes); err != nil {
		return nativeConfig{}, err
	}

	if g.AdminUI != nil {
		cfg.controller.AdminUI = *g.AdminUI
	}
	if d.Website != nil {
		if cfg.website, err = frontendFiles("data.website", *d.Website); err != nil {
			return nativeConfig{}, err
		}
	}
	return cfg, nil
}

// checkDataListen requires the data-plane address to be a literal IP address
// and port that names one interface, and to differ from the admin address.
// An IPv4-mapped IPv6 address is checked as the IPv4 address it binds:
// "[::ffff:0.0.0.0]" listens on every interface.
func checkDataListen(listen, adminListen string) error {
	addr, err := netip.ParseAddrPort(listen)
	ip := addr.Addr().Unmap()
	if err != nil || addr.Port() == 0 || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("data.listen %q must be a literal IP address of one interface with a port", listen)
	}
	if admin, err := netip.ParseAddrPort(adminListen); err == nil && admin.Addr().Unmap() == ip && admin.Port() == addr.Port() {
		return errors.New("data.listen and gateway.admin_listen must be different addresses")
	}
	return nil
}

// durationSetting resolves an optional duration: def when omitted, otherwise
// a Go duration string within [low, high].
func durationSetting(name string, value *string, def, low, high time.Duration) (time.Duration, error) {
	if value == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*value)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration such as \"5s\"", name, *value)
	}
	if d < low || d > high {
		return 0, fmt.Errorf("%s must be %s to %s", name, low, high)
	}
	return d, nil
}

// numberSetting resolves an optional integer: def when omitted, otherwise a
// value within [low, high].
func numberSetting[N int | int64](name string, value *N, def, low, high N) (N, error) {
	if value == nil {
		return def, nil
	}
	if *value < low || *value > high {
		return 0, fmt.Errorf("%s must be %d to %d", name, low, high)
	}
	return *value, nil
}
