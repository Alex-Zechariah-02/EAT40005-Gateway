package testfixture

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// ordersVersion identifies the orders seed data and business rules.
const ordersVersion = "orders-v2"

const maxOrderQuantity = 50

type order struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Item     string `json:"item"`
	Quantity int    `json:"quantity"`
	Status   string `json:"status"`
}

// orders keeps its own customer and item references, so creating an order
// never calls another fixture.
type orders struct {
	list []order
	next int
}

var (
	orderCustomers = []string{"cust-001", "cust-002", "cust-003"}
	orderItems     = []string{"ITM-001", "ITM-002", "ITM-003"}
)

func (d *orders) reset() {
	d.list = []order{
		{ID: "ORD-1001", Customer: "cust-001", Item: "ITM-001", Quantity: 2, Status: "placed"},
		{ID: "ORD-1002", Customer: "cust-002", Item: "ITM-002", Quantity: 1, Status: "shipped"},
		{ID: "ORD-1003", Customer: "cust-001", Item: "ITM-003", Quantity: 5, Status: "placed"},
	}
	d.next = 1004
}

func (d *orders) snapshot() any {
	return map[string]any{"orders": d.list, "next": d.next}
}

func (d *orders) operations() []operation {
	return []operation{
		{id: "orders.list", method: http.MethodGet, pattern: "/orders", run: d.listOrders},
		{id: "orders.get", method: http.MethodGet, pattern: "/orders/{id}", run: d.getOrder},
		{id: "orders.create", method: http.MethodPost, pattern: "/orders", run: d.createOrder},
	}
}

func (d *orders) listOrders(_ string, _ *http.Request, _ []byte) result {
	return result{status: http.StatusOK, body: map[string]any{"orders": d.list}}
}

// Object ownership is this API's own rule, not the gateway's: the gateway
// decides only which roles may call an operation. A caller holding STAFF or
// MANAGER acts on any order; any other caller acts only on orders whose
// customer is the caller's subject. Subject and roles come from the headers
// the gateway sets after verifying the caller, which only the gateway can
// reach this listener to send.
func seesAllOrders(r *http.Request) bool {
	for role := range strings.SplitSeq(r.Header.Get("X-Gateway-Roles"), ",") {
		if role == "STAFF" || role == "MANAGER" {
			return true
		}
	}
	return false
}

func (d *orders) getOrder(id string, r *http.Request, _ []byte) result {
	for _, o := range d.list {
		// Another customer's order is reported as absent, so its existence
		// is not revealed.
		if o.ID == id && (seesAllOrders(r) || o.Customer == r.Header.Get("X-Gateway-Subject")) {
			return result{status: http.StatusOK, body: o}
		}
	}
	return result{status: http.StatusNotFound, body: errorBody("order_not_found")}
}

func (d *orders) createOrder(_ string, r *http.Request, body []byte) result {
	var request struct {
		Customer string `json:"customer"`
		Item     string `json:"item"`
		Quantity int    `json:"quantity"`
	}
	if rejected := decodeBody(r, body, &request); rejected != nil {
		return *rejected
	}

	if !seesAllOrders(r) && request.Customer != r.Header.Get("X-Gateway-Subject") {
		return result{status: http.StatusForbidden, body: errorBody("not_owner")}
	}
	if !slices.Contains(orderCustomers, request.Customer) {
		return result{status: http.StatusUnprocessableEntity, body: errorBody("unknown_customer")}
	}
	if !slices.Contains(orderItems, request.Item) {
		return result{status: http.StatusUnprocessableEntity, body: errorBody("unknown_item")}
	}
	if request.Quantity < 1 || request.Quantity > maxOrderQuantity {
		return result{status: http.StatusUnprocessableEntity, body: errorBody("invalid_quantity")}
	}

	created := order{
		ID:       fmt.Sprintf("ORD-%d", d.next),
		Customer: request.Customer,
		Item:     request.Item,
		Quantity: request.Quantity,
		Status:   "placed",
	}
	d.list = append(d.list, created)
	d.next++
	return result{status: http.StatusCreated, body: created, effect: "created " + created.ID}
}
