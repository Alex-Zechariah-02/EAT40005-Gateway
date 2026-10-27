// Command demo prepares and runs the local demonstration of the gateway. It
// writes private demonstration configuration, runs Fetch Accounts, the four
// business APIs and the gateway, resets synthetic data and prints security
// events. Policy and key commands use the protected Admin API as the signed-in
// operator. Application tokens are issued only by Fetch Accounts.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/demoports"
	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services"
)

// The demonstration addresses. The administrator dashboard and the data plane
// are on different loopback hosts because browsers scope cookies by host, not
// port: the administrator's session cookie must never accompany a data-plane
// request forwarded to a business service.
var (
	adminListen   = "127.0.0.1:9440"
	dataListen    = "127.0.0.2:9480"
	metricsListen = "127.0.0.1:9450"
)

const (
	issuerName  = "fetch-accounts"
	audience    = "fetch-gateway"
	maxTokenTTL = 15 * time.Minute
)

// demoService is one Fetch API and its control listener.
type demoService struct {
	name, listen, control string
}

var fetchServices = []demoService{
	{"inventory", "127.0.0.1:9501", "127.0.0.1:9601"},
	{"orders", "127.0.0.1:9502", "127.0.0.1:9602"},
	{"reports", "127.0.0.1:9503", "127.0.0.1:9603"},
	{"support", "127.0.0.1:9504", "127.0.0.1:9604"},
	{"accounts", "127.0.0.1:9505", "127.0.0.1:9605"},
}

// Resolve the process's environment once. Invalid offsets stop the command
// before it opens deployment state or starts any process.
var portSetupError = applyDemoPortOffset()

func applyDemoPortOffset() error {
	addresses := []*string{&adminListen, &dataListen, &metricsListen}
	for i := range fetchServices {
		addresses = append(addresses, &fetchServices[i].listen, &fetchServices[i].control)
	}
	for _, address := range addresses {
		value, err := demoports.Offset(*address)
		if err != nil {
			return err
		}
		*address = value
	}
	return nil
}

// Files inside the demonstration directory.
const (
	configFile     = "gateway.json"
	privateKeyFile = "issuer-key.pem"
	publicKeyFile  = "issuer.pem"
	secretFile     = "service.secret"
	signingFile    = "accounts.json"
	auditFile      = "audit.log"
	databaseFile   = "state.db"
	baselineFile   = "baseline.json"
	denyAllFile    = "deny-all.json"
)

const usage = `usage:
  demo up     [-home H] [-fresh] [-stdin]
  demo setup  -dir D -web W -gateway G [-admin NAME]
  demo run    -dir D -gateway G -services S -accounts A [-stdin]
  demo token  -dir D -subject ID [-ttl 15m]
  demo reset  -dir D
  demo attack -dir D -scenario flood-search|flood-login|flood-orders [-from 127.0.0.50] [-count N]
  demo keys rotate -dir D
  demo keys retire -dir D -kid K
  demo policy export -dir D -out F
  demo policy stage -dir D -file F
  demo events -dir D [-request ID] [-last N]`

func main() {
	if portSetupError != nil {
		fmt.Fprintln(os.Stderr, "demo:", portSetupError)
		os.Exit(1)
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	commands := map[string]func([]string) error{
		"up":     up,
		"setup":  setup,
		"run":    run,
		"token":  token,
		"reset":  reset,
		"events": events,
		"attack": attack,
		"keys":   keys,
		"policy": policy,
	}
	command, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := command(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

// parse reads a command's flags and requires -dir.
func parse(set *flag.FlagSet, args []string) (string, error) {
	dir := set.String("dir", "", "demonstration directory")
	if err := set.Parse(args); err != nil {
		return "", err
	}
	if *dir == "" || set.NArg() != 0 {
		return "", errors.New("-dir is required and no other arguments are accepted\n" + usage)
	}
	return filepath.Abs(*dir)
}

// setup creates a new demonstration directory: a signing key pair whose
// private half stays in this directory, the service control secret, the
// gateway configuration and two policy documents for the administrator to
// stage. It then runs the gateway's install command, which prints the first
// administrator's one-time enrolment secret. An existing directory is never
// reused, so setup cannot overwrite state, and a failed setup removes the
// directory it created, private key included, so it can simply be run again.
func setup(args []string) (err error) {
	set := flag.NewFlagSet("setup", flag.ContinueOnError)
	web := set.String("web", "", "built frontend directory holding admin/ and demo/")
	gateway := set.String("gateway", "", "gateway binary")
	admin := set.String("admin", "admin", "name of the first administrator")
	dir, err := parse(set, args)
	if err != nil {
		return err
	}
	if *web == "" || *gateway == "" {
		return errors.New("-web and -gateway are required")
	}
	webDir, err := filepath.Abs(*web)
	if err != nil {
		return err
	}
	if err := createDemo(dir, webDir, *gateway, *admin, os.Stdout); err != nil {
		return err
	}
	fmt.Printf("demonstration directory %s is ready; start it with: demo run -dir %s -gateway %s -services <services binary> -accounts <accounts binary>\n", dir, dir, *gateway)
	return nil
}

// createDemo does setup's work. The gateway's install output, which holds
// the enrolment secret, goes to out.
func createDemo(dir, webDir, gateway, admin string, out io.Writer) (err error) {
	for _, page := range []string{"admin", "demo"} {
		if _, err := os.Stat(filepath.Join(webDir, page, "index.html")); err != nil {
			return fmt.Errorf("-web %s has no %s/index.html: build the frontend first", webDir, page)
		}
	}
	if err := privateDirectory(dir); err != nil {
		return fmt.Errorf("creating a new demonstration directory: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	rand.Read(secret)

	baseline := baselinePolicy()
	files := map[string][]byte{
		privateKeyFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}),
		publicKeyFile:  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}),
		secretFile:     []byte(hex.EncodeToString(secret)),
		signingFile:    indented(map[string]string{"kid": "fetch-2026-10-a", "private_key_file": filepath.Join(dir, privateKeyFile)}),
		configFile:     indented(gatewayConfig(dir, webDir)),
		baselineFile:   indented(baseline),
		denyAllFile:    indented(denyAll(baseline)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return err
		}
	}

	if err := identifyDemo(dir); err != nil {
		return err
	}

	install := exec.Command(gateway, "install", "--config", filepath.Join(dir, configFile), "--admin", admin)
	install.Stdout, install.Stderr = out, os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("gateway install: %w", err)
	}
	return nil
}

func indented(value any) []byte {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}

// gatewayConfig names the native controller, data listener and proxy settings.
// The native gateway validates this file before opening deployment state.
func gatewayConfig(dir, web string) map[string]any {
	return map[string]any{
		"version": 1,
		"gateway": map[string]any{
			"database":       filepath.Join(dir, databaseFile),
			"audit_log":      filepath.Join(dir, auditFile),
			"admin_listen":   adminListen,
			"metrics_listen": metricsListen,
			"admin_ui":       filepath.Join(web, "admin"),
			"jwt": map[string]any{
				"issuer":   issuerName,
				"audience": audience,
				"keys":     []map[string]string{{"kid": "fetch-2026-10-a", "public_key_file": filepath.Join(dir, publicKeyFile)}},
			},
		},
		"data": map[string]any{
			"listen":              dataListen,
			"max_body_bytes":      16384,
			"read_header_timeout": "5s",
			"read_timeout":        "10s",
			"write_timeout":       "30s",
			"website":             filepath.Join(web, "demo"),
		},
		"upstream": map[string]any{
			"dial_timeout":            "2s",
			"response_header_timeout": "5s",
			"read_idle_timeout":       "5s",
		},
	}
}

// denyAll is the baseline with every operation denied and no permits.
func denyAll(baseline map[string]any) map[string]any {
	policy := maps.Clone(baseline)
	var operations []map[string]any
	for _, op := range baseline["operations"].([]map[string]any) {
		denied := maps.Clone(op)
		denied["access"] = "deny"
		operations = append(operations, denied)
	}
	policy["operations"] = operations
	policy["permits"] = []any{}
	return policy
}

// run starts the five Fetch services and the gateway, reports when all of them
// accept connections, and stops them all on interrupt, when the gateway
// exits, or with -stdin when standard input closes. A service that stops is
// reported and left stopped, so the gateway's answer for an unavailable API
// can be shown. Process output goes to log files in the demonstration
// directory.
func run(args []string) (runErr error) {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	gateway := set.String("gateway", "", "gateway binary")
	serviceBinary := set.String("services", "", "Fetch services binary")
	accountsBinary := set.String("accounts", "", "Fetch Accounts binary")
	stdin := set.Bool("stdin", false, "also stop when standard input closes")
	dir, err := parse(set, args)
	if err != nil {
		return err
	}
	if *gateway == "" || *serviceBinary == "" || *accountsBinary == "" {
		return errors.New("-gateway, -services and -accounts are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *stdin {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			stop()
		}()
	}
	return runDemo(ctx, dir, *gateway, *serviceBinary, *accountsBinary, nil)
}

// runDemo is run's work: it stops everything when ctx ends. ready, if not
// nil, is called once every process listens and the gateway has reported
// its admission status.
func runDemo(ctx context.Context, dir, gateway, serviceBinary, accountsBinary string, ready func()) (runErr error) {
	if err := checkDemo(dir); err != nil {
		return err
	}
	if err := checkPortsFree(); err != nil {
		return err
	}

	// Each started process reports its exit on exited; running counts those
	// not yet received, so the cleanup waits for exactly the rest.
	type processExit struct {
		name string
		err  error
	}
	exited := make(chan processExit, len(fetchServices)+1)
	var processes []*exec.Cmd
	var gatewayLogStart int64
	running := 0
	defer func() {
		for _, process := range processes {
			// A process that already exited cannot be signalled. Its exit is
			// still received below, so every child is reaped exactly once.
			if err := interruptProcess(process); err != nil && !errors.Is(err, os.ErrProcessDone) {
				process.Process.Kill()
			}
		}
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		for running > 0 {
			select {
			case result := <-exited:
				running--
				if result.err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("%s stopped: %w; see its log in %s", result.name, result.err, dir))
				}
			case <-timer.C:
				for _, process := range processes {
					process.Process.Kill()
				}
				runErr = errors.Join(runErr, errors.New("child shutdown exceeded 12s; remaining processes killed"))
			}
		}
	}()
	start := func(name string, binary string, args ...string) error {
		logFile, err := os.OpenFile(filepath.Join(dir, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if name == "gateway" {
			info, err := logFile.Stat()
			if err != nil {
				logFile.Close()
				return err
			}
			gatewayLogStart = info.Size()
		}
		cmd := exec.Command(binary, args...)
		newProcessGroup(cmd)
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err := cmd.Start(); err != nil {
			logFile.Close()
			return fmt.Errorf("starting %s: %w", name, err)
		}
		processes = append(processes, cmd)
		running++
		go func() {
			err := cmd.Wait()
			logFile.Close()
			exited <- processExit{name: name, err: err}
		}()
		return nil
	}

	secret := filepath.Join(dir, secretFile)
	var addresses []string
	for _, f := range fetchServices {
		binary := serviceBinary
		args := []string{"-listen", f.listen, "-control", f.control, "-secret-file", secret}
		switch f.name {
		case "accounts":
			binary = accountsBinary
			args = append(args, "-signing-settings", filepath.Join(dir, signingFile))
		case "orders":
			args = append(args, "-service", f.name, "-inventory", "http://"+fetchServices[0].listen)
		case "reports":
			args = append(args, "-service", f.name, "-orders", "http://"+fetchServices[1].listen)
		default:
			args = append(args, "-service", f.name)
		}
		if err := start(f.name, binary, args...); err != nil {
			return err
		}
		addresses = append(addresses, f.listen, f.control)
	}
	if err := start("gateway", gateway, "run", "--config", filepath.Join(dir, configFile)); err != nil {
		return err
	}
	addresses = append(addresses, adminListen, dataListen, metricsListen)

	deadline := time.Now().Add(30 * time.Second)
	for _, address := range addresses {
		for !dialable(address) {
			select {
			case result := <-exited:
				running--
				return fmt.Errorf("%s exited during startup (%v); see %s", result.name, result.err, filepath.Join(dir, result.name+".log"))
			case <-ctx.Done():
				return nil
			case <-time.After(50 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not start listening; see the logs in %s", address, dir)
			}
		}
	}
	admission := ""
	for admission == "" {
		var err error
		admission, err = readAdmission(filepath.Join(dir, "gateway.log"), gatewayLogStart)
		if err != nil {
			return err
		}
		if admission != "" {
			break
		}
		select {
		case result := <-exited:
			running--
			return fmt.Errorf("%s exited before admission status (%v); see its log in %s", result.name, result.err, dir)
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("gateway did not report native admission status; see gateway.log")
		}
	}
	fmt.Printf("listening; protected admission: %s\n", admission)
	fmt.Printf("  dashboard: http://%s/\n  website:   http://%s/\n", adminListen, dataListen)
	for i, p := range processes {
		name := "gateway"
		if i < len(fetchServices) {
			name = fetchServices[i].name
		}
		fmt.Printf("  %-9s process %d\n", name, p.Process.Pid)
	}
	if ready != nil {
		ready()
	}

	for {
		select {
		case <-ctx.Done():
			fmt.Println("stopping")
			return nil
		case result := <-exited:
			running--
			if result.name == "gateway" {
				return fmt.Errorf("the gateway exited (%v); see %s", result.err, filepath.Join(dir, "gateway.log"))
			}
			fmt.Printf("service %s stopped (%v); its gateway requests can now fail\n", result.name, result.err)
		}
	}
}

// checkPortsFree refuses a second demonstration on the same addresses.
func checkPortsFree() error {
	addresses := []string{adminListen, dataListen, metricsListen}
	for _, service := range fetchServices {
		addresses = append(addresses, service.listen, service.control)
	}
	for _, address := range addresses {
		if dialable(address) {
			return fmt.Errorf("%s is already in use; refusing to start a second demonstration", address)
		}
	}
	return nil
}

func dialable(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// token obtains a demo account token from Fetch Accounts. It never reads a
// private key or changes token claims or lifetime.
func token(args []string) error {
	set := flag.NewFlagSet("token", flag.ContinueOnError)
	subject := set.String("subject", "", "seeded demo account ID")
	ttl := set.Duration("ttl", maxTokenTTL, "fixed 15-minute Accounts lifetime")
	dir, err := parse(set, args)
	if err != nil {
		return err
	}
	if err = checkDemo(dir); err != nil {
		return err
	}
	if !validSubject(*subject) || *ttl != maxTokenTTL {
		return errors.New("use a seeded subject and the fixed 15m lifetime")
	}
	compact, expires, err := accountToken(*subject)
	if err != nil {
		return err
	}
	fmt.Println(compact)
	fmt.Fprintf(os.Stderr, "token for %s, expires %s\n", *subject, expires.UTC().Format(time.RFC3339))
	return nil
}

func accountToken(subject string) (string, time.Time, error) {
	for _, user := range services.Users() {
		if user.ID == subject {
			return accountLogin("http://"+fetchServices[4].listen, user.Email)
		}
	}
	return "", time.Time{}, errors.New("unknown demo account")
}
func validSubject(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// reset restores each service's seed through its isolated protected control.
func reset(args []string) error {
	dir, err := parse(flag.NewFlagSet("reset", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err = checkDemo(dir); err != nil {
		return err
	}
	secret, err := os.ReadFile(filepath.Join(dir, secretFile))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: refuseRedirect}
	defer client.CloseIdleConnections()
	for _, service := range fetchServices {
		request, _ := http.NewRequest("POST", "http://"+service.control+"/reset", http.NoBody)
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("resetting %s: no answer; its reset outcome is unknown: %w", service.name, err)
		}
		var state services.State
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&state)
		response.Body.Close()
		hash, hashErr := hex.DecodeString(state.SHA256)
		if response.StatusCode != 200 || decodeErr != nil || state.Service != service.name || hashErr != nil || len(hash) != 32 {
			return fmt.Errorf("resetting %s: invalid reset answer, status %d", service.name, response.StatusCode)
		}
		fmt.Printf("%-9s %s seed restored\n", service.name, state.SHA256)
	}
	return nil
}

// events prints the gateway's request events from the audit file, oldest
// first: every event of one request, or the last N.
func events(args []string) error {
	set := flag.NewFlagSet("events", flag.ContinueOnError)
	requestID := set.String("request", "", "request id shown by the website")
	last := set.Int("last", 20, "number of events to print without -request")
	dir, err := parse(set, args)
	if err != nil {
		return err
	}
	if *last < 1 {
		return errors.New("-last must be at least 1")
	}
	file, err := os.Open(filepath.Join(dir, auditFile))
	if err != nil {
		return err
	}
	defer file.Close()

	var lines []string
	malformed := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			malformed++
			continue // a malformed record or an incomplete tail
		}
		kind, _ := event["kind"].(string)
		if !strings.HasPrefix(kind, "request.") {
			continue
		}
		if *requestID != "" && event["request_id"] != *requestID {
			continue
		}
		var fields []string
		for _, name := range []string{"time", "kind", "stage", "decision", "status", "error", "reason", "subject", "operation", "generation", "request_id"} {
			if value, ok := event[name]; ok && value != "" {
				fields = append(fields, fmt.Sprintf("%s=%v", name, value))
			}
		}
		lines = append(lines, strings.Join(fields, " "))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if *requestID == "" && len(lines) > *last {
		lines = lines[len(lines)-*last:]
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	fmt.Fprintln(os.Stderr, "events: searched only the active audit file; retained rotated files were not searched")
	if malformed > 0 {
		fmt.Fprintf(os.Stderr, "events: skipped %d malformed or partial records\n", malformed)
	}
	if len(lines) == 0 {
		fmt.Fprintln(os.Stderr, "events: no matching records in the active file; this does not establish non-arrival")
	}
	return nil
}
