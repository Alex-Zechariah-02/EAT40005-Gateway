// Controller configuration, state and construction.

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

const (
	// clockTolerance is how far the wall clock may sit behind the highest
	// time recorded by a committed transition before startup refuses to trust
	// persisted deadlines.
	clockTolerance    = 5 * time.Second
	defaultAuditWait  = 2 * time.Second
	minAuditBytes     = 4 << 10
	maxAuditBytes     = 1 << 30
	maxAuditFiles     = 1000
	defaultIdle       = 15 * time.Minute
	defaultLifetime   = 8 * time.Hour
	minSessionSetting = time.Second
	maxSessionSetting = 24 * time.Hour
	maxTransitionID   = 128
	// A provisional base must be confirmed within this many seconds of its
	// activation. A shorter period only brings the automatic rollback forward;
	// the upper bound limits how long an unconfirmed policy can serve.
	minConfirmSeconds = 1
	maxConfirmSeconds = 24 * 60 * 60
	// stateStopping is the state of a controller whose stop has begun, and
	// stateStopped the state after Stop.
	stateStopping = "stopping"
	stateStopped  = "stopped"
)

// Config is the controller's configuration. Zero limits take their defaults.
type Config struct {
	Database             string
	AuditLog             string
	AdminListen          string
	AdminIdleTimeout     time.Duration
	AdminSessionLifetime time.Duration
	AuditTimeout         time.Duration
	AuditMaxBytes        int64
	AuditMaxFiles        int
	JWT                  TrustConfig

	// AdminUI is an optional directory of built dashboard files, served by
	// the admin listener so the dashboard shares the Admin API's origin.
	AdminUI string
	// settingsFile is the startup operator configuration, never chosen by HTTP.
	settingsFile string
}

// Controller is the only component that changes the effective policy. It owns
// the database, compiles generations and publishes them by atomic pointer
// swap. Request handlers only read the current pointer.
//
// NewController creates it, Start serves its Admin API and Stop releases
// everything it owns. The application that calls them owns its lifecycle.
type Controller struct {
	config Config

	current atomic.Pointer[compile.Generation]

	// ui reads the dashboard files; nil when AdminUI is not set.
	ui fs.FS

	// Trust and account containment are published independently of policy.
	verifier atomic.Pointer[verifier]
	blocked  atomic.Pointer[store.BlockedAccounts]
	// addresses is the blocked address list; nil until read from the store.
	addresses atomic.Pointer[store.BlockedAddresses]

	// limiter holds the rate counters. They belong to the process, not to a
	// generation, so publication never resets them; a restart does.
	limiter      *limiter
	clock        rateClock
	observations *observations

	// Administrator authentication state, in memory only.
	sessions *sessions
	throttle *throttle
	hashing  chan struct{} // bounds concurrent password hashing

	// mu serialises every mutation, including timer-driven ones, and the
	// lifecycle changes of Start and Stop.
	mu        sync.Mutex
	state     string // a deployment mode, or the reason no authority is served
	stopping  bool   // set when the stop begins; see beginStop
	ownership *os.File
	store     *store.DB
	audit     *audit.AuditLog
	timer     *time.Timer
	admin     *http.Server
	logger    *slog.Logger

	// stopOnce runs the one stop of the controller, whose result is stopErr;
	// see Stop.
	stopOnce sync.Once
	stopErr  error

	// expiring is set while a request-triggered deadline check is pending, so
	// requests that see an expired generation start at most one.
	expiring atomic.Bool
}

// NewController checks cfg, opens the deployment's database and audit log and
// publishes the effective policy that the database holds. It opens no
// listener; Start does. When it returns an error, nothing it opened is left
// open. A database without usable authority is not an error: the controller
// is returned serving no protected request, and its health and audit log say
// why.
func NewController(cfg Config, logger *slog.Logger) (*Controller, error) {
	if logger == nil {
		return nil, errors.New("a logger is required")
	}
	if err := cfg.check(); err != nil {
		return nil, err
	}
	// Without valid trust material no token can be verified, so the gateway
	// refuses to start rather than run without authentication.
	verifier, err := loadVerifier(cfg.JWT)
	if err != nil {
		return nil, fmt.Errorf("jwt: %w", err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	c := &Controller{
		config:       cfg,
		limiter:      newLimiter(),
		clock:        rateClock{anchor: time.Now()},
		observations: newObservations(),
		sessions:     newSessions(cfg.AdminIdleTimeout, cfg.AdminSessionLifetime),
		throttle:     newThrottle(),
		hashing:      make(chan struct{}, maxHashing),
		logger:       logger,
	}
	c.verifier.Store(verifier)
	if cfg.AdminUI != "" {
		if c.ui, err = frontendFiles("admin_ui", cfg.AdminUI); err != nil {
			return nil, err
		}
	}

	// The database's exclusive lock is taken before the audit log is opened,
	// so a second gateway for the same deployment stops here and never
	// becomes a second writer of its audit log.
	ownership, err := store.LockDeployment(cfg.Database)
	if err != nil {
		return nil, err
	}
	c.ownership = ownership
	st, err := store.OpenStore(cfg.Database)
	if err != nil && !errors.Is(err, store.ErrUnavailable) {
		ownership.Close()
		return nil, err
	}
	c.audit = audit.OpenAudit(cfg.AuditLog, cfg.AuditMaxBytes, cfg.AuditMaxFiles)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.unavailable(err.Error())
		return c, nil
	}
	c.store = st
	blocked, err := st.BlockedAccounts(context.Background())
	if err != nil {
		c.unavailable("reading blocked accounts: " + err.Error())
		return c, nil
	}
	c.blocked.Store(&blocked)
	addresses, err := st.BlockedAddresses(context.Background())
	if err != nil {
		c.unavailable("reading blocked addresses: " + err.Error())
		return c, nil
	}
	c.addresses.Store(&addresses)
	c.reconcile(compile.TrustedNow())
	return c, nil
}

func (cfg Config) check() error {
	if cfg.Database == "" || cfg.AuditLog == "" || cfg.AdminListen == "" {
		return errors.New("database, audit_log and admin_listen are required")
	}
	addr, err := netip.ParseAddrPort(cfg.AdminListen)
	if err != nil || !addr.Addr().IsLoopback() || addr.Port() == 0 {
		return fmt.Errorf("admin_listen %q must be a literal loopback address with a port", cfg.AdminListen)
	}
	return nil
}

// applyDefaults sets the defaults of unset limits and checks every limit's
// bounds.
func (cfg *Config) applyDefaults() error {
	if cfg.AuditTimeout <= 0 {
		cfg.AuditTimeout = defaultAuditWait
	}
	if cfg.AuditMaxBytes == 0 {
		cfg.AuditMaxBytes = audit.DefaultAuditBytes
	}
	if cfg.AuditMaxFiles == 0 {
		cfg.AuditMaxFiles = audit.DefaultAuditFiles
	}
	if cfg.AdminIdleTimeout == 0 {
		cfg.AdminIdleTimeout = defaultIdle
	}
	if cfg.AdminSessionLifetime == 0 {
		cfg.AdminSessionLifetime = defaultLifetime
	}
	if cfg.AuditMaxBytes < minAuditBytes || cfg.AuditMaxBytes > maxAuditBytes {
		return fmt.Errorf("audit_max_bytes must be %d to %d", minAuditBytes, maxAuditBytes)
	}
	if cfg.AuditMaxFiles < 2 || cfg.AuditMaxFiles > maxAuditFiles {
		return fmt.Errorf("audit_max_files must be 2 to %d", maxAuditFiles)
	}
	for _, setting := range []time.Duration{cfg.AdminIdleTimeout, cfg.AdminSessionLifetime} {
		if setting < minSessionSetting || setting > maxSessionSetting {
			return fmt.Errorf("admin_idle_timeout and admin_session_lifetime must be %s to %s", minSessionSetting, maxSessionSetting)
		}
	}
	return nil
}

// Current returns the published generation, or nil when nothing may be served.
func (c *Controller) Current() *compile.Generation { return c.current.Load() }
