package main

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestFetchBaselineContract(t *testing.T) { checkFetchBaseline(t) }

func checkFetchBaseline(t *testing.T) {
	t.Helper()
	var policy struct {
		Schema int `json:"schema_version"`
		Roles  []struct {
			ID string `json:"id"`
		} `json:"roles"`
		Operations []struct {
			ID, Method, Target, Auth, Access string
			Rate                             struct {
				Count     int
				Key       string
				Window    int `json:"window_seconds"`
				Algorithm string
			}
			Body *struct {
				Max    int `json:"max_bytes"`
				Schema json.RawMessage
			}
		}
		Permits []struct{ Role, Operation string }
	}
	if err := json.Unmarshal(indented(baselinePolicy()), &policy); err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"inventory.categories GET /inventory/categories none 120 - 0",
		"inventory.merchants GET /inventory/merchants none 120 - 0",
		"inventory.merchant GET /inventory/merchants/* none 120 - 0",
		"inventory.search GET /inventory/search none 60 - 0",
		"inventory.item GET /inventory/items/* none 120 - 0",
		"inventory.item.update PATCH /inventory/items/* required 30 MERCHANT 512",
		"inventory.stock.read GET /inventory/stock/* required 100 MERCHANT,OPERATIONS 0",
		"inventory.stock.adjust PATCH /inventory/stock/* required 30 MERCHANT,OPERATIONS 256",
		"orders.quote GET /orders/quote required 60 CUSTOMER 0",
		"orders.create POST /orders required 10 CUSTOMER 4096",
		"orders.mine GET /orders/mine required 60 CUSTOMER 0",
		"orders.wallet GET /orders/wallet required 60 CUSTOMER 0",
		"orders.detail GET /orders/detail/* required 120 CUSTOMER,MERCHANT,RIDER,SUPPORT,OPERATIONS 0",
		"orders.cancel POST /orders/cancel/* required 10 CUSTOMER 0",
		"orders.merchant GET /orders/merchant required 120 MERCHANT 0",
		"orders.accept POST /orders/accept/* required 60 MERCHANT 0",
		"orders.ready POST /orders/ready/* required 60 MERCHANT 0",
		"orders.jobs GET /orders/jobs required 120 RIDER 0",
		"orders.pickup POST /orders/pickup/* required 60 RIDER 0",
		"orders.deliver POST /orders/deliver/* required 60 RIDER 0",
		"orders.all GET /orders required 60 SUPPORT,OPERATIONS 0",
		"reports.merchant GET /reports/merchant required 30 MERCHANT 0",
		"reports.summary GET /reports/summary required 30 OPERATIONS 0",
		"reports.sales GET /reports/sales required 30 OPERATIONS 0",
		"reports.top-items GET /reports/top-items required 30 OPERATIONS 0",
		"support.create POST /support/tickets required 5 CUSTOMER 4096",
		"support.mine GET /support/mine required 60 CUSTOMER 0",
		"support.queue GET /support/queue required 60 SUPPORT 0",
		"support.ticket GET /support/ticket/* required 60 CUSTOMER,SUPPORT 0",
		"support.reply POST /support/reply/* required 20 CUSTOMER,SUPPORT 4096",
		"support.resolve POST /support/resolve/* required 20 SUPPORT 0",
		"accounts.login POST /accounts/login none 10 - 512",
		"accounts.refresh POST /accounts/refresh required 30 CUSTOMER,MERCHANT,RIDER,SUPPORT,OPERATIONS 0",
		"accounts.me GET /accounts/me required 60 CUSTOMER,MERCHANT,RIDER,SUPPORT,OPERATIONS 0",
	}
	if policy.Schema != 4 || len(policy.Operations) != len(wants) {
		t.Fatalf("baseline schema=%d operations=%d, want v4 and %d", policy.Schema, len(policy.Operations), len(wants))
	}
	for i, line := range wants {
		fields := strings.Fields(line)
		op := policy.Operations[i]
		if op.ID != fields[0] || op.Method != fields[1] || op.Target != fields[2] || op.Auth != fields[3] || op.Access != "allow" {
			t.Errorf("operation %d does not match %s", i, line)
		}
		if strconv.Itoa(op.Rate.Count) != fields[4] || op.Rate.Window != 60 || op.Rate.Algorithm != "sliding-window" {
			t.Errorf("%s rate does not match contract", op.ID)
		}
		key := "subject"
		if op.Auth == "none" {
			key = "client_address"
		}
		if op.Rate.Key != key {
			t.Errorf("%s key=%s", op.ID, op.Rate.Key)
		}
		var roles []string
		for _, p := range policy.Permits {
			if p.Operation == op.ID {
				roles = append(roles, p.Role)
			}
		}
		wantRoles := []string{}
		if fields[5] != "-" {
			wantRoles = strings.Split(fields[5], ",")
		}
		if !slices.Equal(roles, wantRoles) {
			t.Errorf("%s permits=%v want=%v", op.ID, roles, wantRoles)
		}
		max := 0
		if op.Body != nil {
			max = op.Body.Max
		}
		if strconv.Itoa(max) != fields[6] {
			t.Errorf("%s body limit=%d", op.ID, max)
		}
	}
	roles := []string{}
	for _, role := range policy.Roles {
		roles = append(roles, role.ID)
	}
	if !slices.Equal(roles, strings.Split("CUSTOMER,MERCHANT,RIDER,SUPPORT,OPERATIONS", ",")) {
		t.Errorf("roles=%v", roles)
	}
}

func TestFetchStackLayout(t *testing.T) {
	names := []string{}
	for _, service := range fetchServices {
		names = append(names, service.name)
	}
	if !slices.Equal(names, []string{"inventory", "orders", "reports", "support", "accounts"}) {
		t.Errorf("services=%v", names)
	}
	if secretFile != "service.secret" {
		t.Errorf("service secret name=%s", secretFile)
	}
	config := gatewayConfig(t.TempDir(), t.TempDir())
	if config["gateway"].(map[string]any)["metrics_listen"] == nil {
		t.Error("metrics listener missing")
	}
}

func TestFetchCommandArgumentRefusals(t *testing.T) {
	for _, command := range []func([]string) error{attack, keys, policy} {
		if command(nil) == nil {
			t.Error("command accepted missing required arguments")
		}
	}
}
