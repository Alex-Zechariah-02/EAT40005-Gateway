package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The native gateway reads all of its settings from one strict JSON file. The
// file is checked completely before anything of the deployment is opened.

const (
	// nativeConfigVersion is the only configuration file version this gateway
	// reads.
	nativeConfigVersion = 1
	maxConfigBytes      = 64 << 10
	maxConfigDepth      = 4
)

// Bounds of the native settings. Data-plane and upstream timeouts default to
// the legacy values; every timeout is at least a second, so no setting can
// make ordinary requests fail, and has an upper bound, so none leaves a phase
// practically unbounded.
const (
	minServerTimeout     = time.Second
	maxServerTimeout     = 10 * time.Minute
	minUpstreamTimeout   = time.Second
	maxUpstreamTimeout   = 5 * time.Minute
	minAuditTimeout      = 100 * time.Millisecond
	maxAuditTimeout      = 30 * time.Second
	defaultHeaderBytes   = 16 << 10
	minHeaderBytes       = 8 << 10 // a token of maxTokenBytes and ordinary headers fit
	maxHeaderBytes       = 1 << 20
	defaultReadHeader    = 5 * time.Second
	defaultRead          = 10 * time.Second
	defaultWrite         = 30 * time.Second
	defaultIdleKeepAlive = 5 * time.Minute
	defaultDial          = 2 * time.Second
	defaultUpstreamWait  = 5 * time.Second
)

// nativeFile is the configuration file as written. A setting that may be
// omitted is a pointer: omitted, it takes its default; written, it must be
// valid, so a zero or empty value is refused rather than read as "default".
type nativeFile struct {
	Version  int              `json:"version"`
	Gateway  gatewaySettings  `json:"gateway"`
	Data     dataSettings     `json:"data"`
	Upstream upstreamSettings `json:"upstream"`
}

// gatewaySettings are the controller's settings, under the names the legacy
// Caddy "gateway" app used.
type gatewaySettings struct {
	Database             string      `json:"database"`
	AuditLog             string      `json:"audit_log"`
	AdminListen          string      `json:"admin_listen"`
	AdminIdleTimeout     *string     `json:"admin_idle_timeout"`
	AdminSessionLifetime *string     `json:"admin_session_lifetime"`
	AuditTimeout         *string     `json:"audit_timeout"`
	AuditMaxBytes        *int64      `json:"audit_max_bytes"`
	AuditMaxFiles        *int        `json:"audit_max_files"`
	JWT                  TrustConfig `json:"jwt"`
	AdminUI              *string     `json:"admin_ui"`
}

// dataSettings are the data-plane server's settings.
type dataSettings struct {
	Listen            string  `json:"listen"`
	MaxBodyBytes      *int64  `json:"max_body_bytes"`
	MaxHeaderBytes    *int    `json:"max_header_bytes"`
	ReadHeaderTimeout *string `json:"read_header_timeout"`
	ReadTimeout       *string `json:"read_timeout"`
	WriteTimeout      *string `json:"write_timeout"`
	IdleTimeout       *string `json:"idle_timeout"`
	Website           *string `json:"website"`
}

// upstreamSettings bound each exchange with an upstream.
type upstreamSettings struct {
	DialTimeout           *string `json:"dial_timeout"`
	ResponseHeaderTimeout *string `json:"response_header_timeout"`
	ReadIdleTimeout       *string `json:"read_idle_timeout"`
}

// nativeConfig is a checked configuration of the native gateway.
type nativeConfig struct {
	controller        Config
	listen            string
	maxBodyBytes      int64
	headerLimit       int
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	upstream          upstreamTimeouts
	// website holds the built Client website served under /demo/; nil when
	// none is configured.
	website fs.FS
}

// loadNativeConfig reads and checks the configuration file at path. The
// controller checks the rest of its own settings, the admin address and the
// trust key, before it opens the database.
func loadNativeConfig(path string) (nativeConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nativeConfig{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nativeConfig{}, err
	}
	if len(data) > maxConfigBytes {
		return nativeConfig{}, fmt.Errorf("the file is larger than %d bytes", maxConfigBytes)
	}
	settings, err := decodeNativeFile(data)
	if err != nil {
		return nativeConfig{}, err
	}
	return settings.check()
}

// decodeNativeFile parses the configuration document. It refuses what
// encoding/json alone would accept ambiguously: repeated or non-lowercase
// member names, unknown members and anything after the document.
func decodeNativeFile(data []byte) (nativeFile, error) {
	// The document's kind and version are read first, so a file written for
	// another gateway or version is reported as such rather than as a set of
	// unknown members.
	decoder := json.NewDecoder(bytes.NewReader(data))
	var header map[string]json.RawMessage
	if err := decoder.Decode(&header); err != nil || header == nil {
		if _, wrongType := errors.AsType[*json.UnmarshalTypeError](err); wrongType || err == nil {
			return nativeFile{}, errors.New("the file is not a JSON object")
		}
		return nativeFile{}, errors.New("the file is not valid JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nativeFile{}, errors.New("the file holds data after the JSON value")
	}
	if _, legacy := header["apps"]; legacy {
		return nativeFile{}, errors.New("this is a Caddy configuration (top-level \"apps\"); write a native configuration instead, it is not converted automatically")
	}
	if err := checkMembers(data, maxConfigDepth); err != nil {
		policyErr, _ := errors.AsType[*PolicyError](err)
		switch {
		case policyErr != nil && policyErr.Code == "DUPLICATE_MEMBER":
			return nativeFile{}, fmt.Errorf("member %q is repeated in one object", policyErr.Field)
		case policyErr != nil && policyErr.Code == "UNKNOWN_FIELD":
			return nativeFile{}, errors.New("a member name is not made of lower-case letters, digits and underscores")
		case policyErr != nil && policyErr.Code == "LIMIT_EXCEEDED":
			return nativeFile{}, errors.New("the document is nested too deeply")
		}
		return nativeFile{}, errors.New("the file is not valid JSON")
	}
	var version int
	if err := json.Unmarshal(header["version"], &version); err != nil || version != nativeConfigVersion {
		return nativeFile{}, fmt.Errorf("version must be %d, the only configuration version this gateway reads", nativeConfigVersion)
	}

	var settings nativeFile
	if err := decodeStrict(data, &settings); err != nil {
		if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return nativeFile{}, fmt.Errorf("%s: a JSON %s is not a valid value", typeErr.Field, typeErr.Value)
		}
		return nativeFile{}, errors.New(strings.TrimPrefix(err.Error(), "json: "))
	}
	return settings, nil
}

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
		{"gateway.jwt.public_key_file", g.JWT.PublicKeyFile},
		{"data.listen", d.Listen},
	} {
		if required.value == "" {
			return nativeConfig{}, fmt.Errorf("%s is required", required.name)
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
		{"gateway.jwt.public_key_file", &g.JWT.PublicKeyFile},
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

	var err error
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
		{"upstream.dial_timeout", u.DialTimeout, defaultDial, minUpstreamTimeout, maxUpstreamTimeout, &cfg.upstream.dial},
		{"upstream.response_header_timeout", u.ResponseHeaderTimeout, defaultUpstreamWait, minUpstreamTimeout, maxUpstreamTimeout, &cfg.upstream.responseHeader},
		{"upstream.read_idle_timeout", u.ReadIdleTimeout, defaultUpstreamWait, minUpstreamTimeout, maxUpstreamTimeout, &cfg.upstream.readIdle},
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
	if cfg.maxBodyBytes, err = numberSetting("data.max_body_bytes", d.MaxBodyBytes, defaultMaxBodyBytes, 1, maxBodyLimit); err != nil {
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
