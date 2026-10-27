package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// clockTolerance is how far the wall clock may sit behind the highest
	// time recorded by a committed transition before startup refuses to trust
	// persisted deadlines.
	clockTolerance    = 5 * time.Second
	defaultAuditWait  = 2 * time.Second
	defaultAuditBytes = 64 << 20
	defaultAuditFiles = 8
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

	current atomic.Pointer[Generation]

	// ui reads the dashboard files; nil when AdminUI is not set.
	ui fs.FS

	// Trust and account containment are published independently of policy.
	verifier atomic.Pointer[verifier]
	blocked  atomic.Pointer[blockedAccounts]

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
	store     *store
	audit     *auditLog
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
	ownership, err := lockDeployment(cfg.Database)
	if err != nil {
		return nil, err
	}
	c.ownership = ownership
	st, err := openStore(cfg.Database)
	if err != nil && !errors.Is(err, errUnavailable) {
		ownership.Close()
		return nil, err
	}
	c.audit = openAudit(cfg.AuditLog, cfg.AuditMaxBytes, cfg.AuditMaxFiles)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.unavailable(err.Error())
		return c, nil
	}
	c.store = st
	blocked, err := st.blockedAccounts(context.Background())
	if err != nil {
		c.unavailable("reading blocked accounts: " + err.Error())
		return c, nil
	}
	c.blocked.Store(&blocked)
	c.reconcile(trustedNow())
	return c, nil
}

// frontendFiles returns the built frontend in dir, which must hold its
// index.html; setting names the configuration field in the error.
func frontendFiles(setting, dir string) (fs.FS, error) {
	files := os.DirFS(dir)
	if info, err := fs.Stat(files, "index.html"); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %q must be a directory holding index.html", setting, dir)
	}
	return files, nil
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
		cfg.AuditMaxBytes = defaultAuditBytes
	}
	if cfg.AuditMaxFiles == 0 {
		cfg.AuditMaxFiles = defaultAuditFiles
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

// Start serves the Admin API on its loopback listener. Protected admission
// does not wait for it: it follows the generation published since
// NewController. Start succeeds at most once, and never once Stop has begun.
func (c *Controller) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopping:
		return errors.New("controller stopped")
	case c.admin != nil:
		return errors.New("controller already started")
	}
	return c.startAdmin()
}

// Stop begins the stop (see beginStop), shuts the Admin API down, waiting at
// most adminShutdownWait for admin requests in progress, then closes the
// audit log, waiting at most auditDrainWait for the events queued until then
// to be written, and the database. It returns nil only when every step
// completed. It also releases a controller that was never started. Stop may
// be called any number of times, also concurrently: the controller stops
// once, and every call returns that stop's result.
func (c *Controller) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), adminShutdownWait)
	defer cancel()
	return c.stopWithin(ctx)
}

// stopWithin is Stop with ctx bounding the wait for admin requests in
// progress; the native gateway shares that bound with its data plane.
func (c *Controller) stopWithin(ctx context.Context) error {
	c.stopOnce.Do(func() { c.stopErr = c.shutdown(ctx) })
	return c.stopErr
}

// beginStop ends protected admission and every change of authority: the
// published generation is withdrawn, so a request that has not captured one
// yet is refused; mutations and the deadline timer change nothing any more;
// Start is refused. A request that captured a generation earlier runs to its
// end with it. Stop begins with it; the native gateway calls it before it
// stops its data plane.
func (c *Controller) beginStop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return
	}
	c.stopping = true
	c.observations.close()
	c.state = stateStopping
	c.current.Store(nil)
	if c.timer != nil {
		c.timer.Stop()
	}
}

func (c *Controller) shutdown(ctx context.Context) error {
	c.beginStop()
	c.mu.Lock()
	admin := c.admin
	c.mu.Unlock()
	// Admin requests in progress may need the lock to finish, so the listener
	// is shut down without holding it.
	err := stopAdmin(ctx, admin)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = stateStopped
	// Every producer of audit events has stopped by now: the admission and
	// admin requests of the stopped listeners, mutations (the lock is held)
	// and the deadline timer (no generation is published). What they queued is
	// written before the database closes; an event arriving later anyway, from
	// a request cut off by an incomplete listener shutdown, is counted as lost.
	pending, _ := c.audit.unconfirmed()
	c.logger.Info("draining the audit log", "pending", pending)
	drain, cancel := context.WithTimeout(context.Background(), auditDrainWait)
	defer cancel()
	errs := []error{err, c.audit.close(drain)}
	if c.store != nil {
		errs = append(errs, c.store.close())
	}
	if c.ownership != nil {
		errs = append(errs, c.ownership.Close())
	}
	return errors.Join(errs...)
}

// Current returns the published generation, or nil when nothing may be served.
func (c *Controller) Current() *Generation { return c.current.Load() }

// admission reports whether protected admission can run at this moment:
// "ready" when a published generation is unexpired and the audit log accepts
// acknowledgements, otherwise why it cannot (the stop, the deployment mode,
// "unavailable", "generation_expired" or "audit_failed"). A ready gateway may
// still refuse every request its policy denies, and the next request may
// still fail; this is an observation, not a promise.
func (c *Controller) admission() string {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	switch state {
	case modeNormal:
	case modeBootstrap, modeRecovery, stateStopping, stateStopped:
		return state
	default:
		return "unavailable"
	}
	g := c.current.Load()
	switch {
	case g == nil:
		return "unavailable"
	case g.expired(trustedNow()):
		return "generation_expired"
	case c.audit.failed.Load():
		return "audit_failed"
	}
	return "ready"
}

func (c *Controller) unavailable(reason string) {
	c.current.Store(nil)
	c.state = reason
	c.logger.Error("protected admission unavailable", "reason", reason)
	c.audit.Record(Event{Kind: "controller.unavailable", Reason: reason})
}

// ready reports whether mutations can be processed: the database is open and
// its authority was read, whatever the deployment mode.
func (c *Controller) ready() bool {
	return c.store != nil && (c.state == modeNormal || c.state == modeBootstrap || c.state == modeRecovery)
}

// reconcile rebuilds the runtime generation from committed authority. It never
// falls back to an older revision: anything it cannot establish leaves the
// gateway serving no protected request.
func (c *Controller) reconcile(now time.Time) {
	ctx := context.Background()
	a, err := c.store.load(ctx)
	if err != nil {
		c.unavailable(err.Error())
		return
	}
	if err := c.reconcileAudit(ctx, now); err != nil {
		c.unavailable(fmt.Sprintf("recording missing outcome events: %v", err))
		return
	}
	if a.mode != modeNormal {
		c.unavailable(a.mode)
		return
	}
	if now.Before(a.highWater.Add(-clockTolerance)) {
		c.unavailable("clock_regression")
		return
	}
	// A provisional base or an override whose time ran out while the gateway
	// was stopped is ended before anything is served; it is never published
	// again.
	if !c.settle(ctx, now, true) {
		return
	}
	a, err = c.store.load(ctx)
	if err != nil {
		c.unavailable(err.Error())
		return
	}
	// A provisional base is served only while the base it returns to can
	// still be restored with the active overrides.
	if a.provisional != 0 {
		if _, err := c.compileEffective(ctx, a.confirmed, time.Time{}, a.overrides); err != nil {
			c.unavailable(fmt.Sprintf("rollback target revision %d: %v", a.confirmed, err))
			return
		}
	}
	// If nothing ended above, nothing is published yet.
	if c.current.Load() == nil && !c.publishAuthority(a) {
		return
	}
	ids, err := c.store.unpublished(ctx)
	if err != nil {
		c.unavailable(fmt.Sprintf("reading publication receipts: %v", err))
		return
	}
	for _, id := range ids {
		if err := c.store.markPublished(ctx, id, "reconciliation", now); err != nil {
			c.unavailable(fmt.Sprintf("recording publication receipt: %v", err))
			return
		}
	}
	if err := c.store.raiseHighWater(ctx, now); err != nil {
		c.unavailable(fmt.Sprintf("recording clock high-water mark: %v", err))
		return
	}
	g := c.current.Load()
	c.audit.Record(Event{Kind: "controller.reconciled", Generation: g.ID, Revision: g.Revision})
}

// reconcileAudit durably records the outcome event of every committed
// transition whose outcome was never acknowledged by the audit log, for
// example because the process stopped between the commit and the outcome
// event. The database holds the authoritative disposition; this restores the
// missing security record from it.
func (c *Controller) reconcileAudit(ctx context.Context, now time.Time) error {
	ids, err := c.store.unaudited(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		t, _, err := c.store.transition(ctx, id)
		if err != nil {
			return err
		}
		event := Event{Kind: outcomeKind(t), Transition: t.ID, Actor: t.Actor, Target: t.Target, Revision: t.Revision, Candidate: t.Candidate, Reason: "reconciled"}
		if strings.HasPrefix(t.Kind, "tpo.") {
			o, found, err := c.store.overrideOf(ctx, t.ID)
			if err != nil || !found {
				return fmt.Errorf("override of transition %s: %v", t.ID, err)
			}
			event.Override, event.Target, event.Expires = o.ID, o.Target, o.ExpiresAt
		}
		if err := c.commitAudit(ctx, event); err != nil {
			return err
		}
		if err := c.store.markAudited(ctx, id, now); err != nil {
			return err
		}
	}
	return nil
}

// publishAuthority compiles committed authority, the active base with its
// active overrides, and swaps it in.
func (c *Controller) publishAuthority(a authority) bool {
	g, err := authorityGeneration(a, a.overrides)
	if err != nil {
		c.unavailable(fmt.Sprintf("stored revision %d with overrides %v: %v", a.revision, overrideIDs(a.overrides), err))
		return false
	}
	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	return true
}

// authorityGeneration compiles the active base of a with the given overrides,
// keeping a provisional base's deadline.
func authorityGeneration(a authority, overrides []override) (*Generation, error) {
	p, err := DecodePolicy(a.document)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if a.provisional != 0 {
		deadline = a.deadline
	}
	return effectiveGeneration(p, a.revision, deadline, overrides)
}

func overrideIDs(overrides []override) []int64 {
	ids := make([]int64, 0, len(overrides))
	for _, o := range overrides {
		ids = append(ids, o.ID)
	}
	return ids
}

// scheduleDeadline arms the timer that wakes the controller at the earliest
// deadline the generation depends on. The timer only wakes the controller:
// every deadline is persisted and is also enforced on every request by the
// generation itself.
func (c *Controller) scheduleDeadline(g *Generation) {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if g.NotAfter.IsZero() || timerFault() {
		return
	}
	c.timer = time.AfterFunc(g.NotAfter.Sub(trustedNow()), func() { c.expire(g) })
}

// deadlinePassed is called by the request path when the generation it
// captured has expired. It starts at most one check at a time, so a late or
// missing timer cannot keep the gateway refusing every request.
func (c *Controller) deadlinePassed(g *Generation) {
	if !c.expiring.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.expiring.Store(false)
		c.expire(g)
	}()
}

// expire ends whatever made the published generation g expire. Under the
// controller lock it rechecks that g is still the published generation, so a
// timer or wake-up that arrives after another transition does nothing. A timer
// that fires before g has expired (the wall clock was set back after it was
// armed) is armed again.
func (c *Controller) expire(g *Generation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	// After beginStop no generation is published, so this is never g.
	if c.current.Load() != g {
		return
	}
	if !g.expired(now) {
		c.scheduleDeadline(g)
		return
	}
	if !c.settle(context.Background(), now, true) {
		return
	}
	// The generation expired, so a stored deadline must have passed and been
	// ended above. If none had, runtime and stored state disagree.
	if c.current.Load() == g {
		c.unavailable(fmt.Sprintf("generation %s expired but no stored deadline has passed", g.ID))
	}
}

// settle ends, one transition at a time in deadline order, every override
// whose expiry time has passed at now and, when withProvisional is set, a
// provisional base whose deadline has passed. Each ending publishes the
// effective policy that follows it. Mutation requests settle overrides only,
// so a request about a provisional base past its deadline is still answered
// as such. settle reports false when an ending could not be completed;
// protected admission has then stopped.
func (c *Controller) settle(ctx context.Context, now time.Time, withProvisional bool) bool {
	for {
		a, err := c.store.load(ctx)
		if err != nil {
			c.unavailable(err.Error())
			return false
		}
		var due *override
		for i, o := range a.overrides {
			if !now.Before(o.ExpiresAt) && (due == nil || o.ExpiresAt.Before(due.ExpiresAt)) {
				due = &a.overrides[i]
			}
		}
		provisionalDue := withProvisional && a.provisional != 0 && !now.Before(a.deadline)
		switch {
		case provisionalDue && (due == nil || !due.ExpiresAt.Before(a.deadline)):
			if !c.timeout(ctx, a, now) {
				return false
			}
		case due != nil:
			t := transitionRecord{
				ID:       fmt.Sprintf("auto-expire:%d", due.ID),
				Kind:     "tpo.expire",
				Actor:    "controller",
				Target:   fmt.Sprintf("override %d", due.ID),
				Override: due.ID,
			}
			t.Digest = t.ID
			if result, _ := c.publishOverrides(ctx, t, a, *due, overrideExpired, now); result.Outcome != "committed" {
				return false
			}
		default:
			return true
		}
	}
}

// timeout rolls back an expired provisional base under its deterministic
// identity, so a repeated attempt can never apply twice. It reports whether
// the restored base is now published; when it is not, nothing is served.
func (c *Controller) timeout(ctx context.Context, a authority, now time.Time) bool {
	t := transitionRecord{
		ID:        fmt.Sprintf("auto-rollback:%d", a.provisional),
		Kind:      "cpa.timeout",
		Actor:     "controller",
		Target:    fmt.Sprintf("revision %d", a.provisional),
		Expected:  sql.NullInt64{Int64: a.confirmed, Valid: true},
		Candidate: a.candidate,
	}
	t.Digest = t.ID
	result, _ := c.endProvisional(ctx, t, a, dispositionTimedOut, now)
	return result.Outcome == "committed"
}

// endProvisional records the final disposition of the provisional base in a
// and publishes the base that follows it, through the same steps as any
// publication: compile the target before anything is committed, record the
// audit intent, commit, swap, record the receipt and the durable outcome.
// Confirmation republishes the provisional revision without its deadline.
// Every other disposition restores the rollback target; if that cannot be
// completed, the controller stops admitting protected requests rather than
// keep serving a provisional policy that is being withdrawn.
func (c *Controller) endProvisional(ctx context.Context, t transitionRecord, a authority, disposition string, now time.Time) (Outcome, int) {
	target := a.confirmed
	if disposition == dispositionConfirmed {
		target = a.provisional
	}
	stage := strings.TrimPrefix(t.Kind, "cpa.")
	failed := func(reason string, result Outcome, status int) (Outcome, int) {
		if disposition != dispositionConfirmed {
			c.unavailable(fmt.Sprintf("%s (%s of revision %d) not completed: %s", t.ID, stage, a.provisional, reason))
		}
		return result, status
	}

	// The base that follows is compiled with the overrides active now, never
	// with an earlier effective policy: an override that ended while the
	// base was provisional stays ended.
	g, err := c.compileEffective(ctx, target, time.Time{}, a.overrides)
	if err == nil && disposition != dispositionConfirmed {
		err = rollbackFault()
	}
	if err != nil {
		c.logger.Error("compiling the base after the provisional revision", "revision", target, "error", err)
		result, status := c.reject(ctx, t, "TARGET_INVALID", "", now)
		return failed("target revision invalid", result, status)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		result, status := auditUnavailable(t.ID)
		return failed("audit unavailable", result, status)
	}
	crashPoint(stage + "-before-commit")
	t.Revision = target
	if err := c.store.commitResolution(ctx, t, a, disposition, now); err != nil {
		result, status := c.storeFailure(t.ID, err)
		return failed("commit failed", result, status)
	}
	crashPoint(stage + "-after-commit")

	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	crashPoint(stage + "-after-swap")
	if err := c.store.markPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording publication receipt", "error", err)
	}
	crashPoint(stage + "-after-receipt")

	result := Outcome{
		Transition:     t.ID,
		Outcome:        "committed",
		Revision:       target,
		Generation:     g.ID,
		Candidate:      a.candidate,
		Provisional:    a.provisional,
		RollbackTarget: a.confirmed,
		Disposition:    disposition,
	}
	event := Event{
		Kind:           outcomeKind(t),
		Transition:     t.ID,
		Actor:          t.Actor,
		Revision:       target,
		Generation:     g.ID,
		Candidate:      a.candidate,
		Provisional:    a.provisional,
		RollbackTarget: a.confirmed,
		Deadline:       a.deadline.UTC(),
	}
	return c.finish(ctx, result, event)
}

// compileEffective compiles a stored revision with the given overrides.
func (c *Controller) compileEffective(ctx context.Context, revision int64, deadline time.Time, overrides []override) (*Generation, error) {
	p, err := c.storedPolicy(ctx, revision)
	if err != nil {
		return nil, err
	}
	return effectiveGeneration(p, revision, deadline, overrides)
}

// outcomeKind names the outcome event of a committed transition.
func outcomeKind(t transitionRecord) string {
	switch t.Kind {
	case "account.block", "account.unblock", "keys.reload":
		return t.Kind
	case "cpa.activate":
		return "cpa.activated"
	case "cpa.confirm":
		return "cpa.confirmed"
	case "cpa.rollback":
		return "cpa.rolled_back"
	case "cpa.timeout":
		return "cpa.timed_out"
	case "tpo.create":
		return "tpo.created"
	case "tpo.revoke":
		return "tpo.revoked"
	case "tpo.expire":
		return "tpo.expired"
	}
	if t.Revision != 0 {
		return "transition.published"
	}
	return "transition.committed"
}

func (c *Controller) commitAudit(ctx context.Context, e Event) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.AuditTimeout)
	defer cancel()
	return c.audit.Commit(ctx, e)
}

// Outcome is the controller receipt for a mutation. For a provisional base,
// Revision is the active base after the transition, Provisional the revision
// the lifecycle concerns and RollbackTarget the confirmed revision it returns
// to.
type Outcome struct {
	Transition     string    `json:"transition_id"`
	Outcome        string    `json:"outcome"`
	Reason         string    `json:"reason,omitempty"`
	Field          string    `json:"field,omitempty"`
	Revision       int64     `json:"revision,omitempty"`
	Generation     string    `json:"generation,omitempty"`
	Candidate      int64     `json:"candidate,omitempty"`
	Provisional    int64     `json:"provisional,omitempty"`
	RollbackTarget int64     `json:"rollback_target,omitempty"`
	Deadline       time.Time `json:"deadline,omitzero"`
	Disposition    string    `json:"disposition,omitempty"`
	// For a temporary override: its identifier, the control and target it
	// governs and its expiry time. Override also names the active override a
	// refused candidate conflicts with.
	Override int64     `json:"override,omitempty"`
	Target   string    `json:"target,omitempty"`
	Expires  time.Time `json:"expires_at,omitzero"`
}

func unavailableOutcome(id string) (Outcome, int) {
	return Outcome{Transition: id, Outcome: "rejected", Reason: "UNAVAILABLE"}, http.StatusServiceUnavailable
}

// auditUnavailable refuses a mutation whose intent event could not be
// recorded; nothing was committed.
func auditUnavailable(id string) (Outcome, int) {
	return Outcome{Transition: id, Outcome: "rejected", Reason: "AUDIT_UNAVAILABLE"}, http.StatusServiceUnavailable
}

// replay returns the recorded outcome when the mutation identity was already
// processed. The same identity with a different request is refused, so a
// retried request can never apply twice and a reused identity can never
// change what was recorded.
func (c *Controller) replay(ctx context.Context, t transitionRecord) (Outcome, int, bool) {
	previous, found, err := c.store.transition(ctx, t.ID)
	if err != nil {
		result, status := c.storeFailure(t.ID, err)
		return result, status, true
	}
	if !found {
		return Outcome{}, 0, false
	}
	result := Outcome{Transition: t.ID, Outcome: "rejected", Reason: "TRANSITION_ID_REUSED"}
	if previous.Digest != t.Digest {
		return result, http.StatusUnprocessableEntity, true
	}
	result.Outcome, result.Reason = previous.Outcome, previous.Reason
	result.Revision, result.Candidate = previous.Revision, previous.Candidate
	if previous.Outcome == "rejected" {
		return result, rejectionStatus(previous.Reason), true
	}
	if strings.HasPrefix(previous.Kind, "cpa.") {
		// The activation record holds the rest of a lifecycle receipt.
		a, found, err := c.store.activation(ctx, "candidate", previous.Candidate)
		if err != nil || !found {
			result, status := c.storeFailure(t.ID, fmt.Errorf("activation of candidate %d: %v", previous.Candidate, err))
			return result, status, true
		}
		result.Provisional, result.RollbackTarget, result.Deadline = a.Revision, a.RollbackTarget, a.Deadline
		if a.Resolution == previous.ID {
			result.Disposition = a.Disposition
		}
	}
	if strings.HasPrefix(previous.Kind, "tpo.") {
		// The override record holds the rest of an override receipt.
		o, found, err := c.store.overrideOf(ctx, previous.ID)
		if err != nil || !found {
			result, status := c.storeFailure(t.ID, fmt.Errorf("override of transition %s: %v", previous.ID, err))
			return result, status, true
		}
		result.Override, result.Target, result.Expires = o.ID, o.Target, o.ExpiresAt
		if o.Resolution == previous.ID {
			result.Disposition = o.Status
		}
	}
	if !previous.Audited {
		result.Reason = "OUTCOME_AUDIT_PENDING"
		return result, http.StatusServiceUnavailable, true
	}
	return result, http.StatusOK, true
}

// reject refuses a mutation and records the refusal under its identity, so a
// retry returns the same answer. Server-chosen identities (genesis, recovery,
// enrolment) are not recorded: one refused attempt must not block the
// transition they name.
func (c *Controller) reject(ctx context.Context, t transitionRecord, reason, field string, now time.Time) (Outcome, int) {
	if !serverIdentity(t.ID) {
		t.Reason = reason
		if err := c.store.recordRejection(ctx, t, now); err != nil {
			return c.storeFailure(t.ID, err)
		}
	}
	kind := "transition.rejected"
	switch {
	case strings.HasPrefix(t.Kind, "cpa."):
		kind = "cpa.rejected"
	case strings.HasPrefix(t.Kind, "tpo."):
		kind = "tpo.rejected"
	}
	c.audit.Record(Event{Kind: kind, Transition: t.ID, Actor: t.Actor, Reason: reason, Candidate: t.Candidate, Override: t.Override})
	return Outcome{Transition: t.ID, Outcome: "rejected", Reason: reason, Field: field, Override: t.Override}, rejectionStatus(reason)
}

func serverIdentity(id string) bool {
	return strings.HasPrefix(id, "genesis:") || strings.HasPrefix(id, "recovery:") ||
		strings.HasPrefix(id, "enrol:") || strings.HasPrefix(id, "auto-rollback:") || strings.HasPrefix(id, "auto-expire:")
}

func rejectionStatus(reason string) int {
	switch reason {
	case "STALE", "PROVISIONAL_ACTIVE", "INVALID_STATE", "CANDIDATE_UNAVAILABLE", "CANDIDATE_STALE",
		"CANDIDATE_INTEGRITY", "NO_PROVISIONAL", "PROVISIONAL_MISMATCH", "ALREADY_RESOLVED", "DEADLINE_PASSED",
		"OVERRIDE_CONFLICT", "OVERRIDE_LIMIT":
		return http.StatusConflict
	case "PRECONDITION_FAILED":
		return http.StatusPreconditionFailed
	case "NOT_FOUND":
		return http.StatusNotFound
	case "TRANSITION_ID_REUSED":
		return http.StatusUnprocessableEntity
	case "KEYS_INVALID":
		return http.StatusUnprocessableEntity
	case "TARGET_INVALID":
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func policyRejection(err error) (string, string) {
	if policyErr, ok := errors.AsType[*PolicyError](err); ok {
		return policyErr.Code, policyErr.Field
	}
	return "SCHEMA_INVALID", ""
}

func (c *Controller) storeFailure(id string, err error) (Outcome, int) {
	c.logger.Error("control-plane database failure", "error", err)
	return Outcome{Transition: id, Outcome: "rejected", Reason: "STORE_ERROR"}, http.StatusServiceUnavailable
}

// publish applies a complete proposed policy: compile it with the
// active overrides, record the audit intent, commit it as the new active base,
// swap the generation in, and record the publication receipt and the durable
// outcome event. Callers have checked that the policy changes no target the
// overrides depend on.
func (c *Controller) publish(ctx context.Context, t transitionRecord, policy Policy, change revisionChange, overrides []override, now time.Time) (Outcome, int) {
	if _, err := effectiveGeneration(policy, 0, change.deadline, overrides); err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return auditUnavailable(t.ID)
	}
	crashPoint("before-commit")

	change.document, _ = json.Marshal(policy)
	revision, err := c.store.commitRevision(ctx, t, change, now)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	crashPoint("after-commit")

	g, err := effectiveGeneration(policy, revision, change.deadline, overrides)
	if err != nil {
		c.unavailable(fmt.Sprintf("compiling committed revision %d: %v", revision, err))
		return Outcome{Transition: t.ID, Outcome: "committed", Reason: "PUBLICATION_FAILED"}, http.StatusInternalServerError
	}
	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	crashPoint("after-swap")

	if err := c.store.markPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording publication receipt", "error", err)
	}
	crashPoint("after-receipt")

	t.Revision = revision
	result := Outcome{Transition: t.ID, Outcome: "committed", Revision: revision, Generation: g.ID, Candidate: change.candidate}
	event := Event{Kind: outcomeKind(t), Transition: t.ID, Actor: t.Actor, Revision: revision, Generation: g.ID, Candidate: change.candidate}
	if !change.deadline.IsZero() {
		result.Provisional, result.RollbackTarget, result.Deadline = revision, change.parent, change.deadline.UTC()
		event.Provisional, event.RollbackTarget, event.Deadline = revision, change.parent, change.deadline.UTC()
	}
	return c.finish(ctx, result, event)
}

// finish durably records a committed mutation's outcome event and
// returns the receipt. If the audit log cannot acknowledge it, the mutation
// stays committed and is reported as such, with the outcome event pending;
// the audit log is marked failed, so protected admission stops until a
// restart, whose reconciliation records the missing outcome event.
func (c *Controller) finish(ctx context.Context, result Outcome, event Event) (Outcome, int) {
	if err := c.commitAudit(ctx, event); err != nil {
		c.logger.Error("outcome event not recorded", "transition", result.Transition, "error", err)
		c.audit.fail(err)
		result.Reason = "OUTCOME_AUDIT_PENDING"
		return result, http.StatusServiceUnavailable
	}
	if err := c.store.markAudited(ctx, result.Transition, trustedNow()); err != nil {
		c.logger.Error("recording audit receipt", "error", err)
	}
	crashPoint("after-outcome")
	return result, http.StatusOK
}

// Change is one whole-policy publication, used by the test publication hook.
// The transition ID makes retries safe: a repeated ID returns the recorded
// outcome instead of applying again.
type Change struct {
	ID       string          `json:"transition_id"`
	Expected int64           `json:"expected_revision"`
	Policy   json.RawMessage `json:"policy"`
}

// Publish applies a complete policy as the new confirmed base. Production
// binaries expose no route to it: whole-policy activation belongs to the
// confirmed-activation lifecycle, and only the test publication hook calls it.
// It cannot create a provisional base.
func (c *Controller) Publish(ctx context.Context, actor string, change Change) (Outcome, int) {
	validID := change.ID != "" &&
		len(change.ID) <= maxTransitionID &&
		!strings.Contains(change.ID, ":")
	if !validID {
		return Outcome{Transition: change.ID, Outcome: "rejected", Reason: "SCHEMA_INVALID"}, http.StatusBadRequest
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if c.store == nil || c.state != modeNormal {
		return unavailableOutcome(change.ID)
	}
	t := transitionRecord{
		ID:       change.ID,
		Kind:     "publish",
		Actor:    actor,
		Target:   "policy",
		Expected: sql.NullInt64{Int64: change.Expected, Valid: true},
		Digest:   changeDigest(change),
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	if !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	if a.revision != change.Expected {
		return c.reject(ctx, t, "STALE", "", now)
	}
	if a.provisional != 0 {
		return c.reject(ctx, t, "PROVISIONAL_ACTIVE", "", now)
	}
	policy, err := DecodePolicy(change.Policy)
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}
	if result, status, conflict := c.rejectConflict(ctx, t, a, policy, now); conflict {
		return result, status
	}
	return c.publish(ctx, t, policy, revisionChange{parent: a.revision, origin: "publish", actor: actor, mode: modeNormal}, a.overrides, now)
}

// rejectConflict refuses, with the active override it conflicts with, a whole
// policy that would replace the active base of a while changing or
// invalidating an active override. It reports whether it refused.
func (c *Controller) rejectConflict(ctx context.Context, t transitionRecord, a authority, next Policy, now time.Time) (Outcome, int, bool) {
	current, err := DecodePolicy(a.document)
	if err != nil {
		result, status := c.storeFailure(t.ID, err)
		return result, status, true
	}
	o := overrideConflict(current, next, a.overrides)
	if o == nil {
		return Outcome{}, 0, false
	}
	t.Override = o.ID
	result, status := c.reject(ctx, t, "OVERRIDE_CONFLICT", "", now)
	result.Target = o.Target
	return result, status, true
}

func changeDigest(change Change) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%d|", change.Expected)
	sum.Write(change.Policy)
	return hex.EncodeToString(sum.Sum(nil))
}

// requestDigest identifies a mutation request by its kind, target, required
// revision and canonical payload.
func requestDigest(kind, target string, expected sql.NullInt64, payload any) string {
	canonical, _ := json.Marshal(payload)
	sum := sha256.New()
	fmt.Fprintf(sum, "%s|%s|%v|%d|", kind, target, expected.Valid, expected.Int64)
	sum.Write(canonical)
	return hex.EncodeToString(sum.Sum(nil))
}

// EstablishBase confirms a validated deny-all candidate as the first
// confirmed base of an installation (genesis) or as the base that ends a
// declared recovery. Each has one server-chosen identity, so a retried or
// duplicated request can never create a second base.
func (c *Controller) EstablishBase(ctx context.Context, kind, actor string, candidate int64, checksum string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(kind)
	}
	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(kind, err)
	}

	t := transitionRecord{ID: "genesis:" + a.installation, Kind: "genesis", Actor: actor, Target: fmt.Sprintf("candidate %d", candidate)}
	requiredMode := modeBootstrap
	if kind == "recovery_base" {
		t.ID, t.Kind = "recovery:"+a.episode, "recovery_base"
		requiredMode = modeRecovery
	}
	t.Digest = requestDigest(t.Kind, t.Target, sql.NullInt64{}, checksum)
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// No override may be active when a base is established: none exists
	// before genesis, and a declared restore quarantines every one.
	if a.mode != requiredMode || (kind == "recovery_base" && a.episode == "") || len(a.overrides) > 0 {
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	}

	record, found, err := c.store.candidate(ctx, candidate)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	case record.Status != "validated":
		return c.reject(ctx, t, "CANDIDATE_UNAVAILABLE", "", now)
	case record.Checksum != checksum || documentChecksum(record.Document) != checksum:
		return c.reject(ctx, t, "PRECONDITION_FAILED", "checksum", now)
	}
	policy, err := DecodePolicy(record.Document)
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}
	if !denyAll(policy) {
		return c.reject(ctx, t, "NOT_DENY_ALL", "", now)
	}
	change := revisionChange{origin: t.Kind, actor: actor, mode: modeNormal, candidate: candidate}
	return c.publish(ctx, t, policy, change, nil, now)
}

// Activate makes a validated candidate the provisional active base. The
// candidate must have been prepared against the current confirmed revision,
// which becomes its rollback target, and its checksum must match the one the
// administrator confirmed. It is decoded and compiled again in full before
// anything is committed. Its deadline is persisted with the activation, so a
// restart can neither extend nor forget it, and it is never extended.
func (c *Controller) Activate(ctx context.Context, t transitionRecord, candidate int64, checksum string, seconds int64) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := deadlineNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// Overrides that have expired by now must not block the candidate.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	t.Candidate = candidate
	switch {
	case a.mode != modeNormal:
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	case a.provisional != 0:
		return c.reject(ctx, t, "PROVISIONAL_ACTIVE", "", now)
	case a.confirmed != t.Expected.Int64:
		return c.reject(ctx, t, "PRECONDITION_FAILED", "", now)
	case seconds < minConfirmSeconds || seconds > maxConfirmSeconds:
		return c.reject(ctx, t, "INVALID_DEADLINE", "confirm_within_seconds", now)
	}

	record, found, err := c.store.candidate(ctx, candidate)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	case record.Status != "validated":
		return c.reject(ctx, t, "CANDIDATE_UNAVAILABLE", "", now)
	case record.Parent != a.confirmed:
		return c.reject(ctx, t, "CANDIDATE_STALE", "", now)
	case record.Checksum != checksum:
		return c.reject(ctx, t, "PRECONDITION_FAILED", "checksum", now)
	case documentChecksum(record.Document) != record.Checksum:
		return c.reject(ctx, t, "CANDIDATE_INTEGRITY", "", now)
	}
	policy, err := DecodePolicy(record.Document)
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}
	// A candidate may not change or invalidate an active override; one that
	// leaves every override's targets alone becomes provisional with them.
	if result, status, conflict := c.rejectConflict(ctx, t, a, policy, now); conflict {
		return result, status
	}

	// The database keeps milliseconds, so the running deadline and the one a
	// restart reads are the same instant.
	deadline := time.UnixMilli(now.Add(time.Duration(seconds) * time.Second).UnixMilli())
	change := revisionChange{parent: a.confirmed, origin: "publish", actor: t.Actor, deadline: deadline, mode: modeNormal, candidate: candidate}
	return c.publish(ctx, t, policy, change, a.overrides, now)
}

// Resolve confirms or rolls back the current provisional base on an
// administrator's explicit request. The request names the provisional
// revision it means and the confirmed revision it was prepared against; a
// request for any other state is refused. At or after the deadline neither is
// possible: the request is refused and the automatic rollback runs instead,
// however late its timer is.
func (c *Controller) Resolve(ctx context.Context, t transitionRecord, provisional int64, disposition string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// The base that follows is compiled with the overrides still active now.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	if a.mode != modeNormal {
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	}
	if a.provisional != provisional {
		earlier, found, err := c.store.activation(ctx, "revision", provisional)
		switch {
		case err != nil:
			return c.storeFailure(t.ID, err)
		case found:
			result, status := c.reject(ctx, t, "ALREADY_RESOLVED", "", now)
			result.Disposition = earlier.Disposition
			return result, status
		case a.provisional == 0:
			return c.reject(ctx, t, "NO_PROVISIONAL", "", now)
		default:
			return c.reject(ctx, t, "PROVISIONAL_MISMATCH", "", now)
		}
	}
	t.Candidate = a.candidate
	if a.confirmed != t.Expected.Int64 {
		return c.reject(ctx, t, "PRECONDITION_FAILED", "", now)
	}
	if !now.Before(a.deadline) {
		result, status := c.reject(ctx, t, "DEADLINE_PASSED", "", now)
		c.timeout(ctx, a, now)
		return result, status
	}
	return c.endProvisional(ctx, t, a, disposition, now)
}

// StageCandidate validates a complete policy with the canonical compiler and
// stores it as an immutable candidate. Nothing is published: the effective
// policy and its generation are unchanged.
func (c *Controller) StageCandidate(ctx context.Context, t transitionRecord, document []byte) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	var parent int64
	if a.mode == modeNormal {
		parent = a.confirmed
	}
	if t.Expected.Int64 != parent {
		return c.reject(ctx, t, "PRECONDITION_FAILED", "", now)
	}
	policy, err := DecodePolicy(document)
	if err == nil {
		_, err = Compile(policy, 0, time.Time{})
	}
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return auditUnavailable(t.ID)
	}
	canonical, _ := json.Marshal(policy)
	record := candidateRecord{Parent: parent, Document: canonical, Checksum: documentChecksum(canonical), DenyAll: denyAll(policy), Actor: t.Actor}
	id, err := c.store.commitCandidate(ctx, t, record, now)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	crashPoint("candidate-after-commit")
	result := Outcome{Transition: t.ID, Outcome: "committed", Candidate: id}
	return c.finish(ctx, result, Event{Kind: "transition.committed", Transition: t.ID, Actor: t.Actor})
}

// WithdrawCandidate marks a validated candidate withdrawn.
func (c *Controller) WithdrawCandidate(ctx context.Context, t transitionRecord, id int64) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}

	record, found, err := c.store.candidate(ctx, id)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	case record.Status != "validated":
		return c.reject(ctx, t, "CANDIDATE_UNAVAILABLE", "", now)
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return auditUnavailable(t.ID)
	}
	if err := c.store.commitWithdrawal(ctx, t, id, now); err != nil {
		return c.storeFailure(t.ID, err)
	}
	result := Outcome{Transition: t.ID, Outcome: "committed", Candidate: id}
	return c.finish(ctx, result, Event{Kind: "transition.committed", Transition: t.ID, Actor: t.Actor})
}

// Enrol consumes an enrolment and stores the administrator's new credential.
// The password hash is computed by the caller, outside the controller lock.
// Every existing session of the administrator ends.
func (c *Controller) Enrol(ctx context.Context, e enrolment, passwordHash string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	t := transitionRecord{ID: "enrol:" + e.Secret[:32], Kind: "admin.enrol", Actor: e.Name, Target: e.Name, Digest: e.Secret}
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: e.Name}); err != nil {
		return auditUnavailable(t.ID)
	}
	crashPoint("enrol-before-commit")
	err := c.store.commitEnrolment(ctx, t, e, passwordHash, now)
	if errors.Is(err, errNotUpdated) {
		// Another request consumed the secret first.
		c.audit.Record(Event{Kind: "transition.rejected", Transition: t.ID, Actor: e.Name, Reason: "ENROLMENT_CONSUMED"})
		return Outcome{Transition: t.ID, Outcome: "rejected", Reason: "ENROLMENT_CONSUMED"}, http.StatusUnauthorized
	}
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	c.sessions.revokeAdmin(e.Name)
	crashPoint("enrol-after-commit")
	return c.finish(ctx, Outcome{Transition: t.ID, Outcome: "committed"}, Event{Kind: "transition.committed", Transition: t.ID, Actor: e.Name})
}

// ChangePassword replaces an administrator's password, provided the
// credential is still the one the caller verified, and ends every session of
// that administrator.
func (c *Controller) ChangePassword(ctx context.Context, admin administrator, passwordHash string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	t := transitionRecord{
		ID:     fmt.Sprintf("password:%s:%d", admin.Name, admin.Credential),
		Kind:   "admin.password",
		Actor:  admin.Name,
		Target: admin.Name,
		Digest: "password",
	}
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: admin.Name}); err != nil {
		return auditUnavailable(t.ID)
	}
	err := c.store.commitPassword(ctx, t, admin.Name, admin.Credential, passwordHash, now)
	if errors.Is(err, errNotUpdated) {
		c.audit.Record(Event{Kind: "transition.rejected", Transition: t.ID, Actor: admin.Name, Reason: "CREDENTIAL_CHANGED"})
		return Outcome{Transition: t.ID, Outcome: "rejected", Reason: "CREDENTIAL_CHANGED"}, http.StatusConflict
	}
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	c.sessions.revokeAdmin(admin.Name)
	return c.finish(ctx, Outcome{Transition: t.ID, Outcome: "committed"}, Event{Kind: "transition.committed", Transition: t.ID, Actor: admin.Name})
}

// proposeOverride checks a proposed override against the current authority,
// in order: normal mode, no provisional base (none may be created while one
// is pending), the expected confirmed revision, the request itself, the
// bound on active overrides and a target no active override governs. It
// returns the override as it would be stored, or the refusal code and field;
// for a taken target it also returns the override holding it. Whether the
// effective policy compiles with it is checked by the caller.
func proposeOverride(a authority, expected int64, request OverrideRequest, actor string, now time.Time) (override, string, string, int64) {
	switch {
	case a.mode != modeNormal:
		return override{}, "INVALID_STATE", "", 0
	case a.provisional != 0:
		return override{}, "PROVISIONAL_ACTIVE", "", 0
	case a.confirmed != expected:
		return override{}, "PRECONDITION_FAILED", "", 0
	}
	o, err := request.check()
	if err != nil {
		code, field := policyRejection(err)
		return override{}, code, field, 0
	}
	if len(a.overrides) >= maxActiveOverrides {
		return override{}, "OVERRIDE_LIMIT", "", 0
	}
	for _, active := range a.overrides {
		if active.Target == o.Target {
			return override{}, "OVERRIDE_CONFLICT", "", active.ID
		}
	}
	// The database keeps milliseconds, so the running expiry and the one a
	// restart reads are the same instant.
	o.CreatedAt = time.UnixMilli(now.UnixMilli()).UTC()
	o.ExpiresAt = time.UnixMilli(now.Add(time.Duration(request.ExpiresInSeconds) * time.Second).UnixMilli()).UTC()
	o.Base, o.Actor, o.Status = a.revision, actor, overrideActive
	return o, "", "", 0
}

// ValidateOverride answers whether a proposed override would be created now,
// without creating it. It runs every check creation runs, including compiling
// the effective policy, and records the answer in the audit log only.
func (c *Controller) ValidateOverride(ctx context.Context, actor string, expected int64, request OverrideRequest) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome("")
	}
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome("")
	}
	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure("", err)
	}
	o, code, field, holder := proposeOverride(a, expected, request, actor, now)
	if code == "" {
		if _, err := authorityGeneration(a, append(slices.Clone(a.overrides), o)); err != nil {
			code, field = policyRejection(err)
		}
	}
	if code != "" {
		c.audit.Record(Event{Kind: "tpo.rejected", Actor: actor, Reason: code, Override: holder})
		return Outcome{Outcome: "rejected", Reason: code, Field: field, Override: holder}, rejectionStatus(code)
	}
	c.audit.Record(Event{Kind: "tpo.validated", Actor: actor, Target: o.Target, Revision: a.revision, Expires: o.ExpiresAt})
	return Outcome{Outcome: "valid", Revision: a.revision, Target: o.Target, Expires: o.ExpiresAt}, http.StatusOK
}

// CreateOverride creates and publishes a temporary override of the confirmed
// base. Nothing is committed unless every check passes and the complete
// effective policy with the override compiles.
func (c *Controller) CreateOverride(ctx context.Context, t transitionRecord, request OverrideRequest) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := deadlineNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// Overrides that have expired by now no longer hold their targets.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	o, code, field, holder := proposeOverride(a, t.Expected.Int64, request, t.Actor, now)
	if code != "" {
		t.Override = holder
		return c.reject(ctx, t, code, field, now)
	}
	o.ID, err = c.store.nextOverrideID(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	o.Creation = t.ID
	t.Override = o.ID
	return c.publishOverrides(ctx, t, a, o, overrideActive, now)
}

// RevokeOverride ends an active override on an administrator's request. It
// is allowed while a provisional base is pending. An override that has
// already ended, including one whose expiry time has passed, keeps its
// recorded disposition.
func (c *Controller) RevokeOverride(ctx context.Context, t transitionRecord, id int64) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// An override whose expiry time has passed expires first, so a late
	// revocation cannot replace its expiry.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	if a.mode != modeNormal {
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	}
	o, found, err := c.store.override(ctx, id)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	}
	t.Override = id
	if o.Status != overrideActive {
		result, status := c.reject(ctx, t, "ALREADY_RESOLVED", "", now)
		result.Disposition, result.Target = o.Status, o.Target
		return result, status
	}
	return c.publishOverrides(ctx, t, a, o, overrideRevoked, now)
}

// publishOverrides applies a change to the active overrides: o
// becomes active (status active) or ends with status. It compiles the active
// base with the overrides that follow before anything is committed, records
// the audit intent, commits, swaps the generation in, and records the
// publication receipt and the durable outcome. The base revision is never
// changed. A creation that cannot complete changes nothing; a revocation or
// expiry that cannot complete after its checks passed stops protected
// admission rather than keep serving an override that is being withdrawn.
func (c *Controller) publishOverrides(ctx context.Context, t transitionRecord, a authority, o override, status string, now time.Time) (Outcome, int) {
	stage := strings.ReplaceAll(t.Kind, ".", "-")
	failed := func(reason string, result Outcome, code int) (Outcome, int) {
		if status != overrideActive {
			c.unavailable(fmt.Sprintf("%s (%s of override %d) not completed: %s", t.ID, stage, o.ID, reason))
		}
		return result, code
	}

	next := slices.DeleteFunc(slices.Clone(a.overrides), func(active override) bool { return active.ID == o.ID })
	if status == overrideActive {
		next = append(next, o)
	}
	g, err := authorityGeneration(a, next)
	if err != nil {
		if status == overrideActive {
			// The override was never created, so the refusal names none.
			t.Override = 0
			code, field := policyRejection(err)
			return c.reject(ctx, t, code, field, now)
		}
		c.logger.Error("compiling the policy without an override", "override", o.ID, "error", err)
		result, code := c.reject(ctx, t, "TARGET_INVALID", "", now)
		return failed("effective policy invalid", result, code)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor, Override: o.ID}); err != nil {
		result, code := auditUnavailable(t.ID)
		return failed("audit unavailable", result, code)
	}
	crashPoint(stage + "-before-commit")
	if status == overrideActive {
		err = c.store.commitOverride(ctx, t, o, now)
	} else {
		err = c.store.commitOverrideEnd(ctx, t, o.ID, status, now)
	}
	if err != nil {
		result, code := c.storeFailure(t.ID, err)
		return failed("commit failed", result, code)
	}
	crashPoint(stage + "-after-commit")

	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	crashPoint(stage + "-after-swap")
	if err := c.store.markPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording publication receipt", "error", err)
	}
	crashPoint(stage + "-after-receipt")

	result := Outcome{
		Transition: t.ID,
		Outcome:    "committed",
		Revision:   a.revision,
		Generation: g.ID,
		Override:   o.ID,
		Target:     o.Target,
		Expires:    o.ExpiresAt,
	}
	if status != overrideActive {
		result.Disposition = status
	}
	event := Event{
		Kind:       outcomeKind(t),
		Transition: t.ID,
		Actor:      t.Actor,
		Revision:   a.revision,
		Generation: g.ID,
		Override:   o.ID,
		Target:     o.Target,
		Expires:    o.ExpiresAt,
	}
	if a.provisional != 0 {
		event.Provisional, event.RollbackTarget, event.Deadline = a.provisional, a.confirmed, a.deadline.UTC()
	}
	return c.finish(ctx, result, event)
}
