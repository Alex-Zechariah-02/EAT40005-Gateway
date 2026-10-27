package main

import "strings"

type fetchOperation struct {
	id, api, method, target, roles string
	count, maxBytes                int
	schema                         map[string]any
}

func stringSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "string", "minLength": minimum, "maxLength": maximum}
}
func integerSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}
func objectSchema(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func baselinePolicy() map[string]any {
	itemChange := objectSchema([]string{}, map[string]any{"price_sen": integerSchema(50, 100000), "available": map[string]any{"type": "boolean"}})
	stockChange := objectSchema([]string{"delta"}, map[string]any{"delta": integerSchema(-100, 100)})
	line := objectSchema([]string{"item", "quantity"}, map[string]any{"item": stringSchema(7, 7), "quantity": integerSchema(1, 50)})
	address := objectSchema([]string{"line1", "postcode", "city", "state"}, map[string]any{
		"line1": stringSchema(1, 100), "line2": stringSchema(0, 100), "postcode": stringSchema(5, 5), "city": stringSchema(1, 40),
		"state": map[string]any{"type": "string", "enum": []string{"Johor", "Kedah", "Kelantan", "Melaka", "Negeri Sembilan", "Pahang", "Perak", "Perlis", "Pulau Pinang", "Sabah", "Sarawak", "Selangor", "Terengganu", "Wilayah Persekutuan Kuala Lumpur", "Wilayah Persekutuan Labuan", "Wilayah Persekutuan Putrajaya"}},
	})
	checkout := objectSchema([]string{"merchant", "items", "address", "payment"}, map[string]any{
		"merchant": stringSchema(7, 7), "items": map[string]any{"type": "array", "items": line, "minItems": 1, "maxItems": 20},
		"address": address, "note": stringSchema(0, 200), "payment": map[string]any{"type": "string", "enum": []string{"cash", "wallet"}},
	})
	ticket := objectSchema([]string{"subject", "message"}, map[string]any{"subject": stringSchema(5, 120), "message": stringSchema(1, 2000), "order": stringSchema(8, 8), "request_ref": stringSchema(16, 64)})
	reply := objectSchema([]string{"message"}, map[string]any{"message": stringSchema(1, 2000)})
	login := objectSchema([]string{"email", "password"}, map[string]any{"email": stringSchema(3, 254), "password": stringSchema(1, 256)})
	allRoles := "CUSTOMER,MERCHANT,RIDER,SUPPORT,OPERATIONS"
	definitions := []fetchOperation{
		{"inventory.categories", "inventory", "GET", "/inventory/categories", "", 120, 0, nil},
		{"inventory.merchants", "inventory", "GET", "/inventory/merchants", "", 120, 0, nil},
		{"inventory.merchant", "inventory", "GET", "/inventory/merchants/*", "", 120, 0, nil},
		{"inventory.search", "inventory", "GET", "/inventory/search", "", 60, 0, nil},
		{"inventory.item", "inventory", "GET", "/inventory/items/*", "", 120, 0, nil},
		{"inventory.item.update", "inventory", "PATCH", "/inventory/items/*", "MERCHANT", 30, 512, itemChange},
		{"inventory.stock.read", "inventory", "GET", "/inventory/stock/*", "MERCHANT,OPERATIONS", 100, 0, nil},
		{"inventory.stock.adjust", "inventory", "PATCH", "/inventory/stock/*", "MERCHANT,OPERATIONS", 30, 256, stockChange},
		{"orders.quote", "orders", "GET", "/orders/quote", "CUSTOMER", 60, 0, nil},
		{"orders.create", "orders", "POST", "/orders", "CUSTOMER", 10, 4096, checkout},
		{"orders.mine", "orders", "GET", "/orders/mine", "CUSTOMER", 60, 0, nil},
		{"orders.wallet", "orders", "GET", "/orders/wallet", "CUSTOMER", 60, 0, nil},
		{"orders.detail", "orders", "GET", "/orders/detail/*", allRoles, 120, 0, nil},
		{"orders.cancel", "orders", "POST", "/orders/cancel/*", "CUSTOMER", 10, 0, nil},
		{"orders.merchant", "orders", "GET", "/orders/merchant", "MERCHANT", 120, 0, nil},
		{"orders.accept", "orders", "POST", "/orders/accept/*", "MERCHANT", 60, 0, nil},
		{"orders.ready", "orders", "POST", "/orders/ready/*", "MERCHANT", 60, 0, nil},
		{"orders.jobs", "orders", "GET", "/orders/jobs", "RIDER", 120, 0, nil},
		{"orders.pickup", "orders", "POST", "/orders/pickup/*", "RIDER", 60, 0, nil},
		{"orders.deliver", "orders", "POST", "/orders/deliver/*", "RIDER", 60, 0, nil},
		{"orders.all", "orders", "GET", "/orders", "SUPPORT,OPERATIONS", 60, 0, nil},
		{"reports.merchant", "reports", "GET", "/reports/merchant", "MERCHANT", 30, 0, nil},
		{"reports.summary", "reports", "GET", "/reports/summary", "OPERATIONS", 30, 0, nil},
		{"reports.sales", "reports", "GET", "/reports/sales", "OPERATIONS", 30, 0, nil},
		{"reports.top-items", "reports", "GET", "/reports/top-items", "OPERATIONS", 30, 0, nil},
		{"support.create", "support", "POST", "/support/tickets", "CUSTOMER", 5, 4096, ticket},
		{"support.mine", "support", "GET", "/support/mine", "CUSTOMER", 60, 0, nil},
		{"support.queue", "support", "GET", "/support/queue", "SUPPORT", 60, 0, nil},
		{"support.ticket", "support", "GET", "/support/ticket/*", "CUSTOMER,SUPPORT", 60, 0, nil},
		{"support.reply", "support", "POST", "/support/reply/*", "CUSTOMER,SUPPORT", 20, 4096, reply},
		{"support.resolve", "support", "POST", "/support/resolve/*", "SUPPORT", 20, 0, nil},
		{"accounts.login", "accounts", "POST", "/accounts/login", "", 10, 512, login},
		{"accounts.refresh", "accounts", "POST", "/accounts/refresh", allRoles, 30, 0, nil},
		{"accounts.me", "accounts", "GET", "/accounts/me", allRoles, 60, 0, nil},
	}
	upstreams, apis, operations, permits := []map[string]any{}, []map[string]any{}, []map[string]any{}, []map[string]any{}
	for _, service := range fetchServices {
		upstreams = append(upstreams, map[string]any{"id": service.name + "-svc", "dial": service.listen})
		apis = append(apis, map[string]any{"id": service.name, "upstream": service.name + "-svc"})
	}
	for _, definition := range definitions {
		auth, key := "required", "subject"
		if definition.roles == "" {
			auth, key = "none", "client_address"
		}
		op := map[string]any{"id": definition.id, "api": definition.api, "method": definition.method, "target": definition.target, "access": "allow", "auth": auth,
			"rate": map[string]any{"algorithm": "sliding-window", "count": definition.count, "window_seconds": 60, "key": key}}
		if definition.schema != nil {
			op["body"] = map[string]any{"max_bytes": definition.maxBytes, "schema": definition.schema}
		}
		operations = append(operations, op)
		if definition.roles != "" {
			for _, role := range strings.Split(definition.roles, ",") {
				permits = append(permits, map[string]any{"role": role, "operation": definition.id})
			}
		}
	}
	roles := []map[string]any{}
	for _, role := range strings.Split(allRoles, ",") {
		roles = append(roles, map[string]any{"id": role})
	}
	return map[string]any{"schema_version": 4, "profile": "core", "upstreams": upstreams, "apis": apis, "operations": operations, "roles": roles, "permits": permits}
}
