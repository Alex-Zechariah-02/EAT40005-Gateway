package gateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/forward"
)

// The native gateway: the controller, its enforcement handler and the native
// proxy, served by the project's own net/http data-plane server.

// Exit statuses of the native run command.
const (
	exitStopped    = 0 // stopped gracefully and completely
	exitFailed     = 1 // could not start, or the data plane failed
	exitForced     = 2 // a second signal ended the graceful stop
	exitIncomplete = 3 // the graceful stop did not complete
)

const nativeUsage = `usage: gateway run --config <file>
  gateway install --config <file> --admin <name>
  gateway enrol --config <file> --admin <name> --permissions <list>
  gateway migrate --config <file>
  gateway backup --config <file> --output <file>
  gateway restore --config <file> --backup <file>
  gateway policy import-openapi --spec <file> --api <id> --upstream <id> --base <file> --out <file> --report <file>
  gateway policy export-openapi --policy <file> --api <id> --out <file> --report <file>`

// Main runs the native gateway command line and returns the exit status.
func Main(args []string) int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, nativeUsage)
		return exitFailed
	}
	command := args[0]
	options := []string{"config"}
	switch command {
	case "run", "migrate":
	case "install":
		options = append(options, "admin")
	case "enrol":
		options = append(options, "admin", "permissions")
	case "backup":
		options = append(options, "output")
	case "restore":
		options = append(options, "backup")
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", command)
		fmt.Fprintln(os.Stderr, nativeUsage)
		return exitFailed
	}
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	parsed := map[string]*onceFlag{}
	for _, name := range options {
		value := new(onceFlag)
		parsed[name] = value
		set.Var(value, name, "required "+name)
	}
	if err := set.Parse(args[1:]); err != nil || set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, nativeUsage)
		return exitFailed
	}
	values := map[string]string{}
	for _, name := range options {
		if parsed[name].value == "" {
			fmt.Fprintln(os.Stderr, "--"+name+" is required")
			fmt.Fprintln(os.Stderr, nativeUsage)
			return exitFailed
		}
		values[name] = parsed[name].value
	}
	if command == "run" {
		return runNative(values["config"], logger)
	}
	cfg, err := loadNativeConfig(values["config"])
	if err == nil {
		err = cfg.controller.check()
	}
	if err == nil {
		_, err = loadVerifier(cfg.controller.JWT)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		return exitFailed
	}
	code, err := executeOperator(command, &operatorContext{config: cfg.controller}, values)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
	}
	return code
}

// onceFlag is a flag that may be given only once, so a command line cannot
// name two configuration files of which one would be silently ignored.
type onceFlag struct {
	value string
	set   bool
}

func (f *onceFlag) String() string { return f.value }

func (f *onceFlag) Set(value string) error {
	if f.set {
		return errors.New("given more than once")
	}
	f.value, f.set = value, true
	return nil
}

// runNative starts the gateway, serves until the first interrupt or
// termination signal, then stops it gracefully. A second signal forces the
// process to exit at once.
func runNative(configPath string, logger *slog.Logger) int {
	// Signals are taken before startup, so one that arrives while the
	// gateway is starting stops it gracefully once it has started.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	cfg, err := loadNativeConfig(configPath)
	if err == nil {
		err = operatorPaths(configPath, cfg.controller, "run", nil)
	}
	if err != nil {
		logger.Error("configuration refused", "error", err)
		return exitFailed
	}
	g, err := startNative(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return exitFailed
	}

	status := exitStopped
	select {
	case sig := <-signals:
		logger.Info("stopping", "signal", sig.String())
	case err := <-g.served:
		logger.Error("data listener failed; stopping", "error", err)
		status = exitFailed
	}
	go func() {
		<-signals
		logger.Error("forced exit: a second signal arrived before the graceful stop completed")
		os.Exit(exitForced)
	}()
	if err := g.stop(); err != nil {
		logger.Error("stop incomplete", "error", err)
		return exitIncomplete
	}
	logger.Info("stopped")
	return status
}

// nativeGateway is one running native gateway.
type nativeGateway struct {
	controller *Controller
	data       *http.Server
	metrics    *http.Server
	// served receives the data-plane and metrics servers' Serve results.
	served chan error
	stop   func() error
}

// startNative opens the deployment for cfg and serves it: the controller with
// its Admin API, and the data plane. Nothing listens before the controller
// holds the database and has published, or refused to publish, the effective
// policy, and the data-plane address is bound before the Admin API starts, so
// a failure leaves nothing served. When it returns an error, everything it
// acquired is released.
func startNative(cfg nativeConfig, logger *slog.Logger) (*nativeGateway, error) {
	proxy, err := forward.NewUpstreamProxy(cfg.upstream, logger)
	if err != nil {
		return nil, err
	}
	c, err := NewController(cfg.controller, logger)
	if err != nil {
		return nil, err
	}
	release := func(err error) (*nativeGateway, error) {
		return nil, errors.Join(err, c.Stop())
	}
	// A log that cannot be opened refuses every admission and every change
	// for as long as the process runs; a running gateway could only report it.
	if c.audit.OpenErr() != nil {
		return release(fmt.Errorf("audit log: %w", c.audit.OpenErr()))
	}
	enforcement, err := newHandler(c, cfg.maxBodyBytes, proxy.Forward)
	if err != nil {
		return release(err)
	}
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return release(fmt.Errorf("data listener: %w", err))
	}
	metricsListener, err := net.Listen("tcp", cfg.metricsListen)
	if err != nil {
		listener.Close()
		return release(fmt.Errorf("metrics listener: %w", err))
	}
	if err := c.Start(); err != nil {
		listener.Close()
		metricsListener.Close()
		return release(fmt.Errorf("admin listener: %w", err))
	}

	// HTTP/1.0 and HTTP/1.1 only, over plain TCP.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	g := &nativeGateway{
		controller: c,
		data: &http.Server{
			Handler:           dataPlane(enforcement, cfg.website),
			Protocols:         protocols,
			ReadHeaderTimeout: cfg.readHeaderTimeout,
			ReadTimeout:       cfg.readTimeout,
			WriteTimeout:      cfg.writeTimeout,
			IdleTimeout:       cfg.idleTimeout,
			MaxHeaderBytes:    cfg.headerLimit,
			// "OPTIONS *" reaches enforcement, which refuses every target
			// that is not in origin form, instead of being answered by
			// net/http.
			DisableGeneralOptionsHandler: true,
			ErrorLog:                     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		metrics: &http.Server{
			Handler:           http.HandlerFunc(c.handleMetrics),
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    defaultHeaderBytes,
		},
		served: make(chan error, 2),
	}
	g.stop = sync.OnceValue(g.shutdown)
	go func() { g.served <- g.data.Serve(listener) }()
	go func() { g.served <- g.metrics.Serve(metricsListener) }()

	// Listening does not mean protected requests are admitted: the deployment
	// may be in bootstrap or recovery, without usable authority, or without
	// its audit log.
	logger.Info("listening", "data", listener.Addr().String(), "admin", cfg.controller.AdminListen)
	attrs := []any{"admission", c.admission()}
	if current := c.Current(); current != nil {
		attrs = append(attrs, "generation", current.ID, "revision", current.Revision)
	}
	logger.Info("protected admission", attrs...)
	return g, nil
}

// shutdown stops the gateway gracefully: admission ends first, then the data
// plane stops accepting connections and its requests in progress finish,
// then the controller stops its Admin API the same way, writes the audit
// events queued until then and closes the database. Requests in progress on
// both listeners share one wait of adminShutdownWait; the audit drain has its
// own auditDrainWait. It returns nil only when every step completed.
func (g *nativeGateway) shutdown() error {
	g.controller.beginStop()
	ctx, cancel := context.WithTimeout(context.Background(), adminShutdownWait)
	defer cancel()
	var errs []error
	if err := g.metrics.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("metrics listener shutdown: %w", err), g.metrics.Close())
	}
	if err := g.data.Shutdown(ctx); err != nil {
		// Requests still running are cut off; their clients see their
		// connections close.
		errs = append(errs, fmt.Errorf("data listener shutdown: %w", err), g.data.Close())
	}
	errs = append(errs, g.controller.stopWithin(ctx))
	return errors.Join(errs...)
}

// dataPlane is the data-plane server's handler: enforcement for every request,
// except the website's exact routes when a website is configured. No router
// cleans or redirects a path before enforcement sees it.
func dataPlane(enforcement http.Handler, website fs.FS) http.Handler {
	if website == nil {
		return enforcement
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, contentType, ok := websiteFile(r)
		if !ok {
			enforcement.ServeHTTP(w, r)
			return
		}
		stripUntrustedHeaders(r.Header)
		w.Header().Set("X-Request-Id", audit.NewRequestID())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		serveFrontendFile(w, r, website, name, contentType)
	})
}

// websiteFile names the Fetch page at an approved client route, or a file
// of the flat assets directory at /assets/<name>. The
// request target is compared as received, before any decoding, and an asset
// name may hold only letters, digits, '.', '-' and '_', so no target reaches
// any other file. Every other request is not the website's.
func websiteFile(r *http.Request) (name, contentType string, ok bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", "", false
	}
	target, _, _ := strings.Cut(r.RequestURI, "?")
	if isWebsitePage(target) {
		return "index.html", "text/html; charset=utf-8", true
	}
	asset, found := strings.CutPrefix(target, "/assets/")
	switch path.Ext(asset) {
	case ".js":
		contentType = "text/javascript"
	case ".css":
		contentType = "text/css"
	case ".svg":
		contentType = "image/svg+xml"
	case ".webp":
		contentType = "image/webp"
	}
	if !found || contentType == "" || !isAssetName(asset) {
		return "", "", false
	}
	return "assets/" + asset, contentType, true
}

func isWebsitePage(target string) bool {
	switch target {
	case "/", "/food", "/mart", "/cart", "/checkout", "/my-orders", "/help", "/signin",
		"/merchant", "/rider", "/support", "/ops", "/lab", "/account":
		return true
	}
	if !compile.IsCanonical(target) {
		return false
	}
	for _, prefix := range []string{"/store/", "/item/", "/my-orders/", "/help/"} {
		if id, found := strings.CutPrefix(target, prefix); found && id != "" && !strings.Contains(id, "/") {
			return true
		}
	}
	return false
}

func isAssetName(name string) bool {
	for _, c := range name {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
