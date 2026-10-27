// Command demo prepares and runs the local demonstration of the gateway. It
// writes the demonstration configuration and policies, acts as the external
// token issuer for the controlled test subjects, runs the three fixture APIs
// and the gateway, resets the fixtures' synthetic data and prints the
// gateway's security events. Its operator commands never sign in as an
// administrator. Only "up", on a deployment that has no policy yet, acts as
// the administrator whose password the person running it types, and then
// only through the gateway's public Admin API, as the dashboard would.
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

	"github.com/go-jose/go-jose/v4"

	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/demoports"
	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

// The demonstration addresses. The administrator dashboard and the data plane
// are on different loopback hosts because browsers scope cookies by host, not
// port: the administrator's session cookie must never accompany a data-plane
// request, which the gateway would forward to a fixture.
var (
	adminListen = "127.0.0.1:9440"
	dataListen  = "127.0.0.2:9480"
)

const (
	issuerName  = "fetch-accounts"
	audience    = "fetch-gateway"
	maxTokenTTL = time.Hour
)

// fixtureService is one fixture API and its control listener.
type fixtureService struct {
	name, listen, control string
}

var fixtures = []fixtureService{
	{"orders", "127.0.0.1:9501", "127.0.0.1:9601"},
	{"inventory", "127.0.0.1:9502", "127.0.0.1:9602"},
	{"reports", "127.0.0.1:9503", "127.0.0.1:9603"},
}

// Resolve the process's environment once. Invalid offsets stop the command
// before it opens deployment state or starts any process.
var portSetupError = applyDemoPortOffset()

func applyDemoPortOffset() error {
	addresses := []*string{&adminListen, &dataListen}
	for i := range fixtures {
		addresses = append(addresses, &fixtures[i].listen, &fixtures[i].control)
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
	secretFile     = "fixture.secret"
	auditFile      = "audit.log"
	databaseFile   = "state.db"
	baselineFile   = "baseline.json"
	denyAllFile    = "deny-all.json"
)

const usage = `usage:
  demo up     [-home H] [-fresh] [-stdin]
  demo setup  -dir D -web W -gateway G [-admin NAME]
  demo run    -dir D -gateway G -fixture F [-stdin]
  demo token  -dir D -subject ID [-ttl 15m]
  demo reset  -dir D
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
// private half stays in this directory, the fixture control secret, the
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
	fmt.Printf("demonstration directory %s is ready; start it with: demo run -dir %s -gateway %s -fixture <fixture binary>\n", dir, dir, *gateway)
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
			"database":     filepath.Join(dir, databaseFile),
			"audit_log":    filepath.Join(dir, auditFile),
			"admin_listen": adminListen,
			"admin_ui":     filepath.Join(web, "admin"),
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

// Request body schemas of the two fixture writes: the request shape each
// fixture accepts. Business rules (known items, ownership, ranges the fixture
// checks itself) stay with the fixtures.
const (
	ordersCreateSchema = `{"type":"object","additionalProperties":false,"required":["customer","item","quantity"],"properties":{` +
		`"customer":{"type":"string","minLength":1,"maxLength":64},` +
		`"item":{"type":"string","minLength":1,"maxLength":64},` +
		`"quantity":{"type":"integer"}}}`
	inventoryAdjustSchema = `{"type":"object","additionalProperties":false,"required":["delta"],"properties":{` +
		`"delta":{"type":"integer","minimum":-100,"maximum":100}}}`
)

// baselinePolicy is the demonstration's confirmed policy: the three fixtures,
// their operations, the catalogue roles and permits and the controlled
// subjects. The summary report has a tight rate limit so that a short burst
// of requests shows the limiter.
func baselinePolicy() map[string]any {
	rate := func(count int) map[string]any {
		return map[string]any{"algorithm": "sliding-window", "count": count, "window_seconds": 60, "key": "subject"}
	}
	body := func(schema string) map[string]any {
		return map[string]any{"max_bytes": 1024, "schema": json.RawMessage(schema)}
	}
	operation := func(id, api, method, target, auth string) map[string]any {
		op := map[string]any{"id": id, "api": api, "method": method, "target": target, "access": "allow", "auth": auth}
		if auth == "required" {
			op["rate"] = rate(100)
		}
		return op
	}
	ordersCreate := operation("orders.create", "orders", "POST", "/orders", "required")
	ordersCreate["body"] = body(ordersCreateSchema)
	inventoryAdjust := operation("inventory.adjust", "inventory", "PATCH", "/inventory/items/*", "required")
	inventoryAdjust["body"] = body(inventoryAdjustSchema)
	reportsSummary := operation("reports.summary", "reports", "GET", "/reports/summary", "required")
	reportsSummary["rate"] = rate(5)

	var upstreams, apis []map[string]any
	for _, f := range fixtures {
		upstreams = append(upstreams, map[string]any{"id": f.name + "-svc", "dial": f.listen})
		apis = append(apis, map[string]any{"id": f.name, "upstream": f.name + "-svc"})
	}
	permit := func(role, operation string) map[string]any {
		return map[string]any{"role": role, "operation": operation}
	}
	return map[string]any{
		"schema_version": 4,
		"profile":        "core",
		"upstreams":      upstreams,
		"apis":           apis,
		"operations": []map[string]any{
			operation("orders.list", "orders", "GET", "/orders", "required"),
			operation("orders.get", "orders", "GET", "/orders/*", "required"),
			ordersCreate,
			operation("inventory.health", "inventory", "GET", "/inventory/health", "none"),
			operation("inventory.get", "inventory", "GET", "/inventory/items/*", "required"),
			inventoryAdjust,
			operation("reports.sales", "reports", "GET", "/reports/sales", "required"),
			reportsSummary,
		},
		"roles": []map[string]any{{"id": "CUSTOMER"}, {"id": "STAFF"}, {"id": "MANAGER"}},
		"permits": []map[string]any{
			permit("STAFF", "orders.list"),
			permit("MANAGER", "orders.list"),
			permit("CUSTOMER", "orders.get"),
			permit("STAFF", "orders.get"),
			permit("CUSTOMER", "orders.create"),
			permit("STAFF", "inventory.get"),
			permit("MANAGER", "inventory.get"),
			permit("STAFF", "inventory.adjust"),
			permit("MANAGER", "reports.sales"),
			permit("STAFF", "reports.summary"),
			permit("MANAGER", "reports.summary"),
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

// run starts the three fixtures and the gateway, reports when all of them
// accept connections, and stops them all on interrupt, when the gateway
// exits, or with -stdin when standard input closes. A fixture that stops is
// reported and left stopped, so the gateway's answer for an unavailable API
// can be shown. Process output goes to log files in the demonstration
// directory.
func run(args []string) (runErr error) {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	gateway := set.String("gateway", "", "gateway binary")
	fixtureBinary := set.String("fixture", "", "fixture binary")
	stdin := set.Bool("stdin", false, "also stop when standard input closes")
	dir, err := parse(set, args)
	if err != nil {
		return err
	}
	if *gateway == "" || *fixtureBinary == "" {
		return errors.New("-gateway and -fixture are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *stdin {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			stop()
		}()
	}
	return runDemo(ctx, dir, *gateway, *fixtureBinary, nil)
}

// runDemo is run's work: it stops everything when ctx ends. ready, if not
// nil, is called once every process listens and the gateway has reported
// its admission status.
func runDemo(ctx context.Context, dir, gateway, fixtureBinary string, ready func()) (runErr error) {
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
	exited := make(chan processExit, len(fixtures)+1)
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
	for _, f := range fixtures {
		if err := start(f.name, fixtureBinary, "-service", f.name, "-listen", f.listen, "-control", f.control, "-secret-file", secret); err != nil {
			return err
		}
		addresses = append(addresses, f.listen, f.control)
	}
	if err := start("gateway", gateway, "run", "--config", filepath.Join(dir, configFile)); err != nil {
		return err
	}
	addresses = append(addresses, adminListen, dataListen)

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
		if i < len(fixtures) {
			name = fixtures[i].name
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
			fmt.Printf("fixture %s stopped (%v); the gateway now answers its requests with 502\n", result.name, result.err)
		}
	}
}

// checkPortsFree refuses a second demonstration on the same addresses.
func checkPortsFree() error {
	addresses := []string{adminListen, dataListen}
	for _, service := range fixtures {
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

// token prints one RS256 access token for a controlled subject, signed with
// the demonstration's private key. A negative -ttl gives a token that has
// already expired, for demonstrating its rejection.
func token(args []string) error {
	set := flag.NewFlagSet("token", flag.ContinueOnError)
	subject := set.String("subject", "", "token subject")
	ttl := set.Duration("ttl", 15*time.Minute, "lifetime, at most 1h; negative for an expired token")
	dir, err := parse(set, args)
	if err != nil {
		return err
	}
	if err := checkDemo(dir); err != nil {
		return err
	}
	if !validSubject(*subject) {
		return errors.New("-subject must be 1 to 128 printable ASCII characters without spaces")
	}
	if *ttl == 0 || *ttl > maxTokenTTL || *ttl < -maxTokenTTL {
		return errors.New("-ttl must be non-zero and at most 1h either way")
	}
	compact, expires, err := mintToken(dir, *subject, *ttl)
	if err != nil {
		return err
	}
	fmt.Println(compact)
	fmt.Fprintf(os.Stderr, "token for %s, expires %s\n", *subject, expires.UTC().Format(time.RFC3339))
	return nil
}

// mintToken signs one access token for subject with the demonstration's
// private key. The caller has checked the subject and lifetime.
func mintToken(dir, subject string, ttl time.Duration) (string, time.Time, error) {
	data, err := os.ReadFile(filepath.Join(dir, privateKeyFile))
	if err != nil {
		return "", time.Time{}, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return "", time.Time{}, errors.New("issuer key file holds no private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", time.Time{}, err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "fetch-2026-10-a"))
	if err != nil {
		return "", time.Time{}, err
	}

	now := time.Now()
	expires := now.Add(ttl)
	roles := []string{}
	catalogue, err := fixture.LoadCatalogue()
	if err != nil {
		return "", time.Time{}, err
	}
	for _, account := range catalogue.Subjects {
		if account.ID == subject && account.Status == "active" {
			roles = account.Roles
			break
		}
	}
	claims, err := json.Marshal(map[string]any{
		"sub":   subject,
		"iss":   issuerName,
		"aud":   audience,
		"iat":   now.Unix(),
		"exp":   expires.Unix(),
		"roles": roles,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	signed, err := signer.Sign(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		return "", time.Time{}, err
	}
	return compact, expires, nil
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

// reset restores each fixture's seed data through its control listener and
// checks the resulting state hash against the catalogue.
func reset(args []string) error {
	dir, err := parse(flag.NewFlagSet("reset", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := checkDemo(dir); err != nil {
		return err
	}
	secret, err := os.ReadFile(filepath.Join(dir, secretFile))
	if err != nil {
		return err
	}
	catalogue, err := fixture.LoadCatalogue()
	if err != nil {
		return err
	}
	seeds := map[string]string{}
	for _, f := range catalogue.Fixtures {
		seeds[f.Service] = f.SeedSHA256
	}

	client := &http.Client{Timeout: 10 * time.Second}
	failed := false
	for _, f := range fixtures {
		request, _ := http.NewRequest(http.MethodPost, "http://"+f.control+"/reset", http.NoBody)
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("resetting %s: %w", f.name, err)
		}
		var state fixture.State
		err = json.NewDecoder(response.Body).Decode(&state)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || err != nil {
			return fmt.Errorf("resetting %s: status %d", f.name, response.StatusCode)
		}
		result := "seed restored"
		if state.Service != f.name || state.SHA256 != seeds[f.name] {
			result, failed = "DOES NOT MATCH the catalogue seed", true
		}
		fmt.Printf("%-9s %s %s\n", f.name, state.SHA256, result)
	}
	if failed {
		return errors.New("a fixture did not return to its seed")
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
