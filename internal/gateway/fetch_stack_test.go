package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/services"
)

// This test shares the gateway integration package's serial process harness so
// its fixed offset ports cannot collide with TestNativeDemoCommands.
func TestFetchStackScenarios(t *testing.T) {
	parent := t.TempDir()
	web := filepath.Join(parent, "web")
	for _, page := range []string{"admin", "demo"} {
		if err := os.MkdirAll(filepath.Join(web, page), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(web, page, "index.html"), []byte("owned stack test page"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(parent, "state")
	install, code := demoCommand(t, "setup", "-dir", dir, "-web", web, "-gateway", binaries.plain, "-admin", "root")
	if code != 0 {
		t.Fatal("Fetch setup failed")
	}
	_, input, exited, output := startDemo(t, dir)
	e := &env{t: t, dir: dir, database: filepath.Join(dir, "state.db"), auditLog: filepath.Join(dir, "audit.log"), dataAddr: demoTestAddress(t, "127.0.0.2:9480"), admin: demoTestAddress(t, "127.0.0.1:9440"), output: output}
	password := testPassword()
	if code, body := e.enrol("root", enrolmentSecret(t, install), password); code != 201 {
		t.Fatalf("enrol: %d %s", code, body)
	}
	root := e.mustLogin("root", password)
	loadPolicy := func(name string) policy.Policy {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		p, err := policy.DecodePolicy(data)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	baseline := loadPolicy("baseline.json")
	code, first, body := e.stage(root, loadPolicy("deny-all.json"), `"r0"`, "fetch-stack-genesis")
	if code != 201 {
		t.Fatalf("stage genesis: %d %s", code, body)
	}
	if code, _ := e.establish(root, "/admin/genesis", first.Candidate.ID, first.Candidate.Checksum); code != 200 {
		t.Fatalf("genesis: %d", code)
	}
	confirmed := e.confirmPolicy(root, baseline, 1, "fetch-stack-baseline")
	revision := confirmed.Revision
	if revision == 0 {
		t.Fatal("baseline was not confirmed")
	}
	call := func(method, path, body, token string, want int) (*http.Response, string) {
		t.Helper()
		headers := []string{}
		if token != "" {
			headers = append(headers, "Authorization", "Bearer "+token)
		}
		response, reply := e.exchange(method, path, "application/json", body, headers...)
		if response.StatusCode != want || response.Header.Get("X-Request-Id") == "" {
			t.Fatalf("%s %s: status %d, want %d (response withheld for credential safety)", method, path, response.StatusCode, want)
		}
		return response, reply
	}
	login := func(email string) string {
		t.Helper()
		_, body := call("POST", "/accounts/login", string(mustJSON(map[string]string{"email": email, "password": "FetchDemo2026!"})), "", 200)
		var reply struct {
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(body), &reply) != nil || reply.Token == "" {
			t.Fatal("Accounts returned no token")
		}
		return reply.Token
	}
	customer, merchant, rider, support, ops := login("customer1@fetch.test"), login("merchant1@fetch.test"), login("rider1@fetch.test"), login("support1@fetch.test"), login("admin1@fetch.test")
	secret, err := os.ReadFile(filepath.Join(dir, "service.secret"))
	if err != nil {
		t.Fatal(err)
	}
	control := func(index int, path string) []byte {
		t.Helper()
		request, _ := http.NewRequest("GET", "http://"+demoTestAddress(t, fmt.Sprintf("127.0.0.1:%d", 9601+index))+path, nil)
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 {
			t.Fatal("service control failed")
		}
		return data
	}
	receiptCount := func(index int) int {
		t.Helper()
		var view struct {
			Receipts []services.Receipt `json:"receipts"`
		}
		if json.Unmarshal(control(index, "/receipts"), &view) != nil {
			t.Fatal("invalid receipts")
		}
		return len(view.Receipts)
	}
	// refused proves a gateway refusal by the service's own receipts: the
	// service at index recorded nothing and the answer carries no X-Service.
	refused := func(index int, method, path, body, token string, want int) {
		t.Helper()
		count := receiptCount(index)
		response, _ := call(method, path, body, token, want)
		if receiptCount(index) != count || response.Header.Get("X-Service") != "" {
			t.Fatalf("%s %s reached the service", method, path)
		}
	}
	seedHashes := make([]string, 5)
	for i := range seedHashes {
		var state services.State
		if json.Unmarshal(control(i, "/state"), &state) != nil {
			t.Fatal("invalid state")
		}
		seedHashes[i] = state.SHA256
	}
	cli := func(credential bool, args ...string) string {
		t.Helper()
		command := exec.Command(binaries.demo, args...)
		if credential {
			command.Stdin = strings.NewReader(password + "\n")
		}
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("demo %s: %v (%s)", args[0], err, data)
		}
		return string(data)
	}

	t.Log("real customer, merchant and rider flow")
	call("GET", "/inventory/categories", "", "", 200)
	call("GET", "/inventory/search?q=nasi", "", "", 200)
	call("GET", "/orders/quote?merchant=MER-001", "", customer, 200)
	checkout := `{"merchant":"MER-001","items":[{"item":"ITM-001","quantity":2},{"item":"ITM-004","quantity":1}],"address":{"line1":"12 Jalan Telawi 3","postcode":"59100","city":"Kuala Lumpur","state":"Wilayah Persekutuan Kuala Lumpur"},"payment":"cash"}`
	response, created := e.exchange("POST", "/orders", "application/json", checkout, "Authorization", "Bearer "+customer, "Idempotency-Key", "fetch-stack-checkout")
	if response.StatusCode != 201 {
		t.Fatalf("checkout: %d %s", response.StatusCode, created)
	}
	var order struct {
		Order services.Order `json:"order"`
	}
	if json.Unmarshal([]byte(created), &order) != nil || order.Order.ID != "ORD-1005" || order.Order.Total != 3329 {
		t.Fatalf("incorrect priced checkout: %s", created)
	}
	for _, action := range []struct{ path, token string }{{"accept", merchant}, {"ready", merchant}, {"pickup", rider}, {"deliver", rider}} {
		call("POST", "/orders/"+action.path+"/ORD-1005", "", action.token, 200)
	}
	call("GET", "/orders/detail/ORD-1005", "", customer, 200)
	call("GET", "/reports/summary?from=2026-08-01&to=2099-12-31", "", ops, 200)
	for _, path := range []string{"/accounts/me", "/accounts/refresh"} {
		method := "GET"
		if strings.HasSuffix(path, "refresh") {
			method = "POST"
		}
		call(method, path, "", customer, 200)
	}

	t.Log("gateway refusals never reach services")
	before := receiptCount(0)
	refusal, _ := call("PATCH", "/inventory/items/ITM-001", `{"price_sen":1400}`, support, 403)
	ref := refusal.Header.Get("X-Request-Id")
	if receiptCount(0) != before || refusal.Header.Get("X-Service") != "" {
		t.Fatal("support edit reached Inventory")
	}
	before = receiptCount(1)
	validationCustomer := login("customer3@fetch.test")
	for _, invalid := range []string{
		`{"merchant":"MER-001","items":[]}`,
		strings.Replace(checkout, `"quantity":2`, `"quantity":0`, 1),
		strings.Replace(checkout, `"quantity":2`, `"quantity":51`, 1),
		strings.Replace(checkout, `"state":"Wilayah Persekutuan Kuala Lumpur"`, `"state":"Unknown"`, 1),
		strings.Replace(checkout, `"payment":"cash"`, `"payment":"cash","unexpected":1`, 1),
	} {
		call("POST", "/orders", invalid, validationCustomer, 400)
	}
	call("POST", "/orders", strings.Repeat(" ", 4096)+checkout, validationCustomer, 413)
	if receiptCount(1) != before {
		t.Fatal("invalid checkout reached Orders")
	}
	refused(1, "GET", "/internal/orders", "", ops, 404)

	t.Log("customer stock permission is controlled by a temporary gateway grant")
	before = receiptCount(0)
	call("GET", "/inventory/stock/ITM-501", "", customer, 403)
	if receiptCount(0) != before {
		t.Fatal("unpermitted stock read reached Inventory")
	}
	e.mustOverride(root, permitOverride(overrideException, "CUSTOMER", "inventory.stock.read", 1), policyETag(revision), "fetch-stack-stock-grant")
	call("GET", "/inventory/stock/ITM-501", "", customer, 200)
	if receiptCount(0) != before+1 {
		t.Fatal("temporary stock grant did not reach Inventory exactly once")
	}
	waitFor(t, time.Now().Add(5*time.Second), "temporary stock grant expiry", func() bool {
		response, _ := e.exchange("GET", "/inventory/stock/ITM-501", "", "", "Authorization", "Bearer "+customer)
		return response.StatusCode == 403
	})

	t.Log("platform reports follow gateway grants; baseline and expiry still refuse")
	reportOperations := []struct{ id, path string }{{"reports.summary", "/reports/summary"}, {"reports.sales", "/reports/sales"}, {"reports.top-items", "/reports/top-items"}}
	before = receiptCount(2)
	for _, operation := range reportOperations {
		call("GET", operation.path, "", customer, 403)
	}
	if receiptCount(2) != before {
		t.Fatal("unpermitted report reached Reports")
	}
	for i, operation := range reportOperations {
		e.mustOverride(root, permitOverride(overrideException, "CUSTOMER", operation.id, 5), policyETag(revision), fmt.Sprintf("fetch-report-grant-%d", i))
	}
	for _, operation := range reportOperations {
		call("GET", operation.path, "", customer, 200)
	}
	if receiptCount(2) != before+3 {
		t.Fatal("gateway report grants did not forward each request exactly once")
	}
	waitFor(t, time.Now().Add(8*time.Second), "report grant expiry", func() bool { return len(e.activeOverrides(root).GenerationOverrides) == 0 })
	for _, operation := range reportOperations {
		call("GET", operation.path, "", customer, 403)
	}
	if receiptCount(2) != before+3 {
		t.Fatal("expired report grant reached Reports")
	}

	t.Log("real source-bound flood commands")
	floodCustomer := login("customer2@fetch.test")
	ordersBeforeFlood := receiptCount(1)
	for _, scenario := range []struct{ name, source, count string }{{"flood-orders", "127.0.0.51", "12"}, {"flood-search", "127.0.0.50", "65"}, {"flood-login", "127.0.0.52", "12"}} {
		result := cli(false, "attack", "-dir", dir, "-scenario", scenario.name, "-from", scenario.source, "-count", scenario.count)
		if !strings.Contains(result, "429:") || !strings.Contains(result, "from="+scenario.source) {
			t.Fatalf("flood did not demonstrate quota: %s", result)
		}
	}
	var floodReceipts struct {
		Receipts []services.Receipt `json:"receipts"`
	}
	if err := json.Unmarshal(control(1, "/receipts"), &floodReceipts); err != nil {
		t.Fatal(err)
	}
	flooded := floodReceipts.Receipts[ordersBeforeFlood:]
	if len(flooded) != 10 {
		t.Fatalf("order flood reached Orders %d times, want 10", len(flooded))
	}
	for _, receipt := range flooded {
		if receipt.Subject != "cust-002" || receipt.Status != 201 {
			t.Fatalf("order flood receipt: subject=%s status=%d", receipt.Subject, receipt.Status)
		}
	}
	refused(1, "POST", "/orders", checkout, floodCustomer, 429)
	call("POST", "/orders", checkout, customer, 201)
	if receiptCount(1) != ordersBeforeFlood+11 {
		t.Fatal("separate customer checkout did not reach Orders exactly once")
	}

	t.Log("containment applies, then expires")
	e.mustOverride(root, operationContainment("orders.create", 1), policyETag(revision), "fetch-stack-contain")
	refused(1, "POST", "/orders", checkout, customer, 403)
	refused(1, "POST", "/orders", checkout, floodCustomer, 403)
	waitFor(t, time.Now().Add(5*time.Second), "containment expiry", func() bool {
		code, _ := e.get("/orders/wallet", "Authorization", "Bearer "+customer)
		return code == 200 && len(e.activeOverrides(root).GenerationOverrides) == 0
	})
	// The old exhausted quota carries over; expiry removes only containment.
	refused(1, "POST", "/orders", checkout, floodCustomer, 429)

	t.Log("add a Support operation through complete policy activation")
	without := loadPolicy("baseline.json")
	without.Operations = slices.DeleteFunc(without.Operations, func(op policy.Operation) bool { return op.ID == "support.queue" })
	without.Permits = slices.DeleteFunc(without.Permits, func(p policy.Permit) bool { return p.Operation == "support.queue" })
	revision = e.confirmPolicy(root, without, revision, "fetch-stack-without-queue").Revision
	refused(3, "GET", "/support/queue", "", support, 404)
	revision = e.confirmPolicy(root, baseline, revision, "fetch-stack-add-queue").Revision
	call("GET", "/support/queue", "", support, 200)

	t.Log("impact predicts loss of rider access; unconfirmed activation rolls back")
	call("GET", "/orders/jobs", "", rider, 200)
	bad := loadPolicy("baseline.json")
	bad.Permits = slices.DeleteFunc(bad.Permits, func(p policy.Permit) bool { return p.Role == "RIDER" && p.Operation == "orders.jobs" })
	code, staged, body := e.stage(root, bad, policyETag(revision), "fetch-stack-bad-rider")
	if code != 201 {
		t.Fatalf("bad rider candidate: %d %s", code, body)
	}
	code, impact, _ := e.as(root, "POST", fmt.Sprintf("/admin/candidates/%d/impact", staged.Candidate.ID), "")
	if code != 200 || !strings.Contains(impact, `"operation":"orders.jobs"`) || !strings.Contains(impact, `"after":"refused"`) {
		t.Fatalf("impact missed real rider flip: %d %s", code, impact)
	}
	if code, _, body = e.activate(root, staged.Candidate.candidateRecord, 1, policyETag(revision), "fetch-stack-bad-activate"); code != 200 {
		t.Fatalf("bad activation: %d %s", code, body)
	}
	refused(1, "GET", "/orders/jobs", "", rider, 403)
	waitFor(t, time.Now().Add(5*time.Second), "automatic rollback", func() bool { code, _ := e.get("/orders/jobs", "Authorization", "Bearer "+rider); return code == 200 })

	t.Log("ticket reference resolves the exact refused decision")
	call("POST", "/support/tickets", string(mustJSON(map[string]string{"subject": "Please inspect this refusal", "message": "The request reference is attached.", "request_ref": ref})), customer, 201)
	code, event, _ := e.as(root, "GET", "/admin/events?ref="+ref, "")
	if code != 200 || !strings.Contains(event, `"subject":"support-01"`) || !strings.Contains(event, `"stage":"permit"`) || !strings.Contains(event, `"code":"forbidden"`) {
		t.Fatalf("reference lookup: %d %s", code, event)
	}

	t.Log("blocking remains an independent immediate control")
	code, body, _ = e.as(root, "PUT", "/admin/blocked-accounts/cust-001", `{"reason":"isolated stack test"}`, "Idempotency-Key", "fetch-stack-block")
	if code != 200 {
		t.Fatalf("block: %d %s", code, body)
	}
	before = receiptCount(4)
	call("GET", "/accounts/me", "", customer, 403)
	if receiptCount(4) != before {
		t.Fatal("blocked account reached Accounts")
	}
	code, body, _ = e.as(root, "DELETE", "/admin/blocked-accounts/cust-001", "", "Idempotency-Key", "fetch-stack-unblock")
	if code != 200 {
		t.Fatalf("unblock: %d %s", code, body)
	}
	call("GET", "/accounts/me", "", customer, 200)

	t.Log("operator policy export/stage and overlapping signing-key rotation")
	exported := filepath.Join(parent, "exported.json")
	cli(true, "policy", "export", "-dir", dir, "-out", exported)
	exportedData, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.DecodePolicy(exportedData); err != nil {
		t.Fatal(err)
	}
	if result := cli(true, "policy", "stage", "-dir", dir, "-file", exported); !strings.Contains(result, "not activated") {
		t.Fatal("stage silently activated")
	}
	cli(true, "keys", "rotate", "-dir", dir)
	call("GET", "/accounts/me", "", customer, 200)
	_, refreshed := call("POST", "/accounts/refresh", "", customer, 200)
	var newToken struct {
		Token string `json:"token"`
	}
	if json.Unmarshal([]byte(refreshed), &newToken) != nil || newToken.Token == "" {
		t.Fatal("refresh after rotation failed")
	}
	call("GET", "/accounts/me", "", newToken.Token, 200)
	cli(true, "keys", "retire", "-dir", dir, "-kid", "fetch-2026-10-a")
	call("GET", "/accounts/me", "", customer, 401)
	call("GET", "/accounts/me", "", newToken.Token, 200)
	cli(false, "reset", "-dir", dir)
	for i, want := range seedHashes {
		var state services.State
		if json.Unmarshal(control(i, "/state"), &state) != nil || state.SHA256 != want {
			t.Fatalf("service %d seed hash changed after reset", i)
		}
		if receiptCount(i) != 0 {
			t.Fatalf("service %d reset retained receipts", i)
		}
	}
	assertAbsent(t, e, customer, merchant, rider, support, ops, newToken.Token, "FetchDemo2026!", password)
	input.Close()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("stack shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stack did not stop")
	}
	for _, base := range []int{9440, 9450, 9480, 9501, 9502, 9503, 9504, 9505, 9601, 9602, 9603, 9604, 9605} {
		host := "127.0.0.1"
		if base == 9480 {
			host = "127.0.0.2"
		}
		if dialable(demoTestAddress(t, fmt.Sprintf("%s:%d", host, base))) {
			t.Fatalf("stack leaked port %d", base)
		}
	}
}
