package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Alex-Zechariah-02/apiward/internal/services"
)

// The administrator enrolled by the one-command launcher.
const upAdmin = "root"

// The two frontends: their folders in this repository, and the folders of the
// web directory the gateway serves them from.
var websites = []struct{ page, folder string }{
	{"admin", "dashboard"},
	{"demo", "client"},
}

// up runs the whole local demonstration with one command. It builds the
// gateway, the services and both websites from the current sources, creates
// the demonstration the first time and then establishes its baseline policy
// through the Admin API, prints the public demo accounts and runs everything until
// interrupted. Later runs reuse the administrator and policy; services restart from seed.
func up(args []string) error {
	set := flag.NewFlagSet("up", flag.ContinueOnError)
	home := set.String("home", defaultHome(), "folder for the built programs, websites and demonstration state")
	fresh := set.Bool("fresh", false, "delete the earlier demonstration state, including its administrator and policy, and start over")
	stdin := set.Bool("stdin", false, "also stop when standard input closes")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("up takes no arguments\n" + usage)
	}
	homeDir, err := filepath.Abs(*home)
	if err != nil {
		return err
	}
	state := filepath.Join(homeDir, "state")
	bin := filepath.Join(homeDir, "bin")
	web := filepath.Join(homeDir, "web")

	// A running demonstration holds the ports and its program files.
	if err := checkPortsFree(); err != nil {
		return err
	}
	repository, err := gatewayRepository()
	if err != nil {
		return err
	}
	if *fresh {
		if err := removeState(state); err != nil {
			return err
		}
	}
	_, err = os.Stat(state)
	firstRun := errors.Is(err, os.ErrNotExist)
	if err != nil && !firstRun {
		return err
	}
	earlierSetup := false
	if !firstRun {
		if err := checkDemo(state); err != nil {
			lock, earlierErr := lockUnfinishedEarlierDemo(state)
			if earlierErr != nil {
				return errors.Join(err, earlierErr)
			}
			lock.Close()
			firstRun, earlierSetup = true, true
		}
	}

	// The password is chosen before anything is created, so stopping here
	// leaves nothing half set up.
	password := ""
	if firstRun {
		fmt.Println("First run: choose the password for the Dashboard administrator", upAdmin+".")
		if password, err = choosePassword(); err != nil {
			return err
		}
	}

	programs, err := buildPrograms(repository, bin)
	if err != nil {
		return err
	}
	if err := buildWebsites(repository, web); err != nil {
		return err
	}
	secret := ""
	if firstRun {
		if earlierSetup {
			preserved, err := preserveUnfinishedEarlierDemo(state)
			if err != nil {
				return err
			}
			fmt.Println("Preserved the unfinished earlier setup in", preserved)
		}
		var install bytes.Buffer
		if err := createDemo(state, web, programs.gateway, upAdmin, &install); err != nil {
			return err
		}
		if secret, err = enrolmentSecret(install.String()); err != nil {
			os.RemoveAll(state)
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- runDemo(running, state, programs.gateway, programs.services, programs.accounts, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-finished:
		return err
	}

	if err := prepareGateway(state, firstRun, secret, password); err != nil {
		cancel()
		return errors.Join(err, <-finished)
	}
	printSignIns()
	fmt.Printf("\nDashboard: http://%s/  (sign in as %s)\nShop:      http://%s/\nPress Ctrl+C to stop.\n", adminListen, upAdmin, dataListen)
	if *stdin {
		// Every password prompt is answered; the rest of the input only
		// signals the stop.
		go func() {
			io.Copy(io.Discard, stdinLines)
			stop()
		}()
	}
	return <-finished
}

func defaultHome() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "EAT40005-demo"
	}
	return filepath.Join(cache, "EAT40005-demo")
}

// gatewayRepository finds the root of this Go module from the working
// directory, so up builds the sources it is run from.
func gatewayRepository() (string, error) {
	output, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("finding the gateway repository with the go command: %w", err)
	}
	mod := strings.TrimSpace(string(output))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("run up from the gateway repository: go run ./cmd/demo up")
	}
	root := filepath.Dir(mod)
	if _, err := os.Stat(filepath.Join(root, "cmd", "gateway")); err != nil {
		return "", fmt.Errorf("%s is not the gateway repository", root)
	}
	return root, nil
}

// removeState deletes an earlier demonstration directory, after checking it
// is one.
func removeState(state string) error {
	if _, err := os.Stat(state); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := checkDemo(state); err != nil {
		// Earlier native state is never run or migrated. An explicit -fresh
		// may replace it only when its original directory/file binding holds.
		if oldErr := checkEarlierDemoForFresh(state); oldErr != nil {
			return fmt.Errorf("not deleting %s: %w", state, errors.Join(err, oldErr))
		}
	}
	if err := os.RemoveAll(state); err != nil {
		return err
	}
	fmt.Println("Deleted the earlier demonstration state.")
	return nil
}

var stdinLines = bufio.NewReader(os.Stdin)

func readLine() (string, error) {
	line, err := stdinLines.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// choosePassword asks for a new password twice. The gateway decides whether
// it is acceptable; the length check here only saves a failed enrolment.
func choosePassword() (string, error) {
	for {
		first, err := readSecretLine("Password (15 to 256 bytes, not shown): ")
		if err != nil {
			return "", err
		}
		second, err := readSecretLine("Repeat the password: ")
		if err != nil {
			return "", err
		}
		switch {
		case len(first) < 15 || len(first) > 256 || !utf8.ValidString(first):
			fmt.Println("Use 15 to 256 bytes. A phrase of at least 15 plain characters works.")
		case first != second:
			fmt.Println("The two passwords differ. Please enter them again.")
		default:
			return first, nil
		}
	}
}

func executable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// buildPrograms builds the gateway, services and Accounts programs into bin, every
// time, so the demonstration always runs the current sources.
type builtPrograms struct{ gateway, services, accounts string }

func buildPrograms(repository, bin string) (builtPrograms, error) {
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return builtPrograms{}, err
	}
	var built []string
	for _, name := range []string{"gateway", "services", "accounts"} {
		fmt.Println("Building the", name, "program...")
		output := filepath.Join(bin, executable(name))
		build := exec.Command("go", "build", "-trimpath", "-o", output, "./cmd/"+name)
		build.Dir = repository
		if log, err := build.CombinedOutput(); err != nil {
			return builtPrograms{}, fmt.Errorf("building %s: %w\n%s", name, err, log)
		}
		built = append(built, output)
	}
	return builtPrograms{built[0], built[1], built[2]}, nil
}

// buildWebsites builds the Dashboard and the shop in their folders of this
// repository and copies each build into web, where the gateway serves it.
func buildWebsites(repository, web string) error {
	for _, site := range websites {
		folder := filepath.Join(repository, site.folder)
		if _, err := os.Stat(filepath.Join(folder, "node_modules")); errors.Is(err, os.ErrNotExist) {
			fmt.Println("Installing the", site.folder, "packages...")
			if err := npm(folder, "ci", "--no-audit", "--no-fund"); err != nil {
				return err
			}
		}
		fmt.Println("Building the", site.folder+"...")
		if err := npm(folder, "run", "build"); err != nil {
			return err
		}

		target := filepath.Join(web, site.page)
		info, err := os.Lstat(target)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return err
		case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			// A link, such as a junction to an old build folder: removing it
			// removes only the link, never the folder it points to.
			if err := os.Remove(target); err != nil {
				return err
			}
		default:
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(web, 0o700); err != nil {
			return err
		}
		if err := os.CopyFS(target, os.DirFS(filepath.Join(folder, "dist"))); err != nil {
			return fmt.Errorf("copying the %s build: %w", site.folder, err)
		}
	}
	return nil
}

func npm(dir string, args ...string) error {
	command := exec.Command("npm", args...)
	command.Dir = dir
	if log, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("npm %s in %s: %w\n%s", strings.Join(args, " "), dir, err, log)
	}
	return nil
}

// enrolmentSecret reads the first administrator's one-time enrolment secret
// from the gateway's install output.
func enrolmentSecret(install string) (string, error) {
	for line := range strings.Lines(install) {
		if strings.HasPrefix(line, "enrolment secret for "+upAdmin+",") {
			if _, secret, found := strings.Cut(strings.TrimSpace(line), ": "); found && secret != "" {
				return secret, nil
			}
		}
	}
	return "", errors.New("the gateway install output holds no enrolment secret")
}

// prepareGateway gives a deployment without a policy its administrator and
// baseline policy, the steps an administrator would take in the Dashboard.
func prepareGateway(state string, firstRun bool, secret, password string) error {
	admin := newAdminClient()
	mode, err := admin.health()
	if err != nil {
		return err
	}
	switch mode {
	case "normal":
		return nil
	case "recovery_required":
		fmt.Println("The gateway needs recovery: sign in to the Dashboard and establish a recovery base on the Changes page.")
		return nil
	case "bootstrap_locked":
	default:
		return fmt.Errorf("the gateway reports state %q", mode)
	}

	if firstRun {
		if err := admin.enrol(secret, password); err != nil {
			return err
		}
	} else {
		fmt.Println("This demonstration has no policy yet.")
		if password, err = readSecretLine("Password of the Dashboard administrator " + upAdmin + " (not shown): "); err != nil {
			return err
		}
	}
	if err := admin.signIn(password); err != nil {
		if firstRun {
			return err
		}
		return fmt.Errorf("%w\nIf the earlier setup never finished, start over with: go run ./cmd/demo up -fresh", err)
	}
	defer admin.signOut()
	fmt.Println("Establishing the baseline policy...")
	return admin.establishBaseline(state)
}

// printSignIns prints only the deliberately public fictional demo credentials.
func printSignIns() {
	fmt.Println("\nFetch demo accounts (all fictional). Password: FetchDemo2026!")
	fmt.Printf("%-14s %-22s %-24s %-20s %-8s %s\n", "Subject", "Name", "Email", "Roles", "Merchant", "Home")
	for _, user := range services.Users() {
		merchant := ""
		if user.Merchant != nil {
			merchant = *user.Merchant
		}
		fmt.Printf("%-14s %-22s %-24s %-20s %-8s %s\n", user.ID, user.Name, user.Email, strings.Join(user.Roles, ","), merchant, user.Home)
	}
}

// adminClient speaks the Admin API's protocol as the Dashboard does: the
// admin Origin on every request and, once signed in, the session cookie and
// the session's CSRF token on every change.
type adminClient struct {
	client *http.Client
	cookie string
	csrf   string
}

// The gateway's administrator session cookie.
const adminSessionCookie = "__Host-gateway-admin"

func newAdminClient() *adminClient {
	return &adminClient{client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: refuseRedirect}}
}

// send makes one request and requires the wanted status. It never resends: a
// request without an answer may still have been applied.
func (a *adminClient) send(method, path string, body []byte, headers map[string]string, wanted int) (*http.Response, []byte, error) {
	request, err := http.NewRequest(method, "http://"+adminListen+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Origin", "http://"+adminListen)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if a.cookie != "" {
		request.Header.Set("Cookie", a.cookie)
	}
	if a.csrf != "" && method != http.MethodGet {
		request.Header.Set("X-CSRF-Token", a.csrf)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: no answer from the gateway, so it may or may not have been applied; check the Dashboard: %w", method, path, err)
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != wanted {
		return nil, nil, fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(reply)))
	}
	return response, reply, nil
}

func marshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func mutationHeaders(ifMatch string) map[string]string {
	headers := map[string]string{"Idempotency-Key": newKey()}
	if ifMatch != "" {
		headers["If-Match"] = ifMatch
	}
	return headers
}

func newKey() string {
	key := make([]byte, 16)
	rand.Read(key)
	return hex.EncodeToString(key)
}

func (a *adminClient) health() (string, error) {
	_, reply, err := a.send(http.MethodGet, "/admin/health", nil, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var health struct {
		State string `json:"state"`
	}
	// Unmarshal first: a return statement may read health.State before the call.
	err = json.Unmarshal(reply, &health)
	return health.State, err
}

func (a *adminClient) enrol(secret, password string) error {
	_, _, err := a.send(http.MethodPost, "/admin/enrol", marshal(map[string]string{"name": upAdmin, "secret": secret, "password": password}), nil, http.StatusCreated)
	return err
}

func (a *adminClient) signIn(password string) error {
	response, reply, err := a.send(http.MethodPost, "/admin/session", marshal(map[string]string{"name": upAdmin, "password": password}), nil, http.StatusCreated)
	if err != nil {
		return fmt.Errorf("signing in as %s: %w", upAdmin, err)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == adminSessionCookie {
			a.cookie = cookie.Name + "=" + cookie.Value
		}
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(reply, &session); err != nil || a.cookie == "" || session.CSRF == "" {
		return errors.New("the sign-in answer held no session")
	}
	a.csrf = session.CSRF
	return nil
}

func (a *adminClient) signOut() {
	a.send(http.MethodDelete, "/admin/session", nil, nil, http.StatusNoContent)
}

type stagedCandidate struct {
	ID       int64  `json:"id"`
	Checksum string `json:"checksum"`
}

// stage stores a complete policy document as a candidate prepared against
// the confirmed revision named by ifMatch.
func (a *adminClient) stage(document []byte, ifMatch string) (stagedCandidate, error) {
	body := append(append([]byte(`{"policy":`), document...), '}')
	_, reply, err := a.send(http.MethodPost, "/admin/candidates", body, mutationHeaders(ifMatch), http.StatusCreated)
	if err != nil {
		return stagedCandidate{}, err
	}
	var staged struct {
		Candidate stagedCandidate `json:"candidate"`
	}
	err = json.Unmarshal(reply, &staged)
	return staged.Candidate, err
}

// establishBaseline makes the demonstration's deny-all document the first
// policy, then activates the baseline document and confirms it, as the
// Dashboard's Changes page would. Both documents are the ones setup wrote
// into the demonstration directory.
func (a *adminClient) establishBaseline(state string) error {
	denyAllDocument, err := compactFile(filepath.Join(state, denyAllFile))
	if err != nil {
		return err
	}
	baselineDocument, err := compactFile(filepath.Join(state, baselineFile))
	if err != nil {
		return err
	}

	first, err := a.stage(denyAllDocument, `"r0"`)
	if err != nil {
		return err
	}
	genesis := map[string]any{"candidate": first.ID, "checksum": first.Checksum}
	if _, _, err := a.send(http.MethodPost, "/admin/genesis", marshal(genesis), nil, http.StatusOK); err != nil {
		return err
	}

	confirmed, _, err := a.send(http.MethodGet, "/admin/policy", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	revision := confirmed.Header.Get("ETag")
	baseline, err := a.stage(baselineDocument, revision)
	if err != nil {
		return err
	}
	activation := map[string]any{"candidate": baseline.ID, "checksum": baseline.Checksum, "confirm_within_seconds": 300}
	if _, _, err := a.send(http.MethodPost, "/admin/provisional", marshal(activation), mutationHeaders(revision), http.StatusOK); err != nil {
		return err
	}

	provisional, reply, err := a.send(http.MethodGet, "/admin/provisional", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var waiting struct {
		Activation struct {
			Revision int64 `json:"revision"`
		} `json:"activation"`
	}
	if err := json.Unmarshal(reply, &waiting); err != nil {
		return err
	}
	confirm := marshal(map[string]int64{"revision": waiting.Activation.Revision})
	_, _, err = a.send(http.MethodPost, "/admin/provisional/confirm", confirm, mutationHeaders(provisional.Header.Get("ETag")), http.StatusOK)
	return err
}

func compactFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return compact.Bytes(), nil
}
